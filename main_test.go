package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain switches the whole test process to a temporary working directory so
// any test writing state/cache/log files cannot pollute the repo directory
// (those files are written to the process cwd by design).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "workbuddy-gateway-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// chdirTemp moves the test working directory to a temp dir so tests never write
// state/cache files inside the repository.
func chdirTemp(t *testing.T) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Not using t.TempDir(): the relative order of its cleanup and "restore the
	// original working directory" is not guaranteed, and Windows refuses to delete a
	// directory that is still the current working directory; a wrong order leaves an
	// invalid cwd and every later os.Getwd/os.Chdir in the tests fails.
	dir, err := os.MkdirTemp("", "workbuddy-test-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Within one cleanup: restore the working directory first, then delete; the
		// order must not be reversed.
		_ = os.Chdir(old)
		_ = os.RemoveAll(dir)
	})
}

// Verifies reset-time parsing from a 429 message
func TestParseResetTime(t *testing.T) {
	msg := `{"code":429,"message":"upstream 429: {\"code\":6004,\"msg\":\"Your usage has exceeded the frequency limit. It will reset at 2026-09-04 07:48:15 UTC+8. You can also switch to another model to continue.\",\"requestId\":\"149e3299-ddf3-4ad1-aaa8-cc752c37630a\"}","type":"upstream_error"}`
	got, ok := parseResetTime(msg)
	if !ok {
		t.Fatal("parseResetTime should succeed")
	}
	want := time.Date(2026, 9, 4, 7, 48, 15, 0, time.FixedZone("UTC+8", 8*3600))
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	t.Logf("parsed reset time: %v", got)
}

// Verifies false is returned when unparseable
func TestParseResetTimeInvalid(t *testing.T) {
	if _, ok := parseResetTime("some random error"); ok {
		t.Fatal("should not parse random string")
	}
}

// Verifies reset-time parsing for the international site's English rate-limit
// message (generic fallback regex)
func TestParseResetTimeEnglish(t *testing.T) {
	cases := []string{
		`{"code":6004,"msg":"Your usage has exceeded the rate limit. It will reset at 2026-09-05 01:57:00 UTC+8."}`,
		`upstream 429: rate limit exceeded, reset at 2026-09-05 01:57:00`,
		`{"msg":"quota exceeded, will reset on 2026-09-05T01:57:00 UTC+8"}`,
	}
	want := time.Date(2026, 9, 5, 1, 57, 0, 0, time.FixedZone("UTC+8", 8*3600))
	for _, msg := range cases {
		got, ok := parseResetTime(msg)
		if !ok {
			t.Fatalf("parseResetTime(%q) should succeed", msg)
		}
		if !got.Equal(want) {
			t.Fatalf("parseResetTime(%q) = %v, want %v", msg, got, want)
		}
	}
}

// Verifies site profile selection: empty/unknown falls back to the domestic
// site, intl-family values match the international site
func TestProfileForEdition(t *testing.T) {
	cases := []struct {
		edition string
		wantKey string
	}{
		{"", "cn"},
		{"cn", "cn"},
		{"unknown", "cn"},
		{"intl", "intl"},
		{"INTL", "intl"},
		{" workbuddy.ai ", "intl"},
		{"international", "intl"},
	}
	for _, c := range cases {
		if got := profileForEdition(c.edition).Key; got != c.wantKey {
			t.Errorf("profileForEdition(%q).Key = %s, want %s", c.edition, got, c.wantKey)
		}
	}
	if p := profileForEdition("intl"); p.Base != "https://www.workbuddy.ai" || p.Origin != "https://www.workbuddy.ai" {
		t.Errorf("intl profile base/origin unexpected: %+v", p)
	}
	if p := profileForEdition("cn"); p.Base != "https://copilot.tencent.com" || p.Platform != "VSCode" {
		t.Errorf("cn profile base/platform unexpected: %+v", p)
	}
}

// Verifies each site's upstream URL construction matches the legacy constants
// exactly (domestic regression) plus correct international URLs
func TestUpstreamProfileURLs(t *testing.T) {
	cn := profileForEdition("cn")
	if got := cn.authStateURL(); got != "https://copilot.tencent.com/v2/plugin/auth/state?platform=VSCode" {
		t.Errorf("cn authStateURL = %s", got)
	}
	if got := cn.chatURL(); got != "https://www.workbuddy.cn/v2/chat/completions" {
		t.Errorf("cn chatURL = %s", got)
	}
	if got := cn.tokenRefreshURL(); got != "https://copilot.tencent.com/v2/plugin/auth/token/refresh" {
		t.Errorf("cn tokenRefreshURL = %s", got)
	}
	if got := cn.quotaSummaryURL(); got != "https://www.codebuddy.cn/billing/meter/get-user-resource-summary" {
		t.Errorf("cn quotaSummaryURL = %s", got)
	}
	if got := cn.dailyCheckinURL(); got != "https://www.codebuddy.cn/v2/billing/meter/daily-checkin" {
		t.Errorf("cn dailyCheckinURL = %s", got)
	}

	itl := profileForEdition("intl")
	if got := itl.authStateURL(); got != "https://www.workbuddy.ai/v2/plugin/auth/state?platform=workbuddy-ai" {
		t.Errorf("intl authStateURL = %s", got)
	}
	if got := itl.authTokenURL("abc-123"); got != "https://www.workbuddy.ai/v2/plugin/auth/token?state=abc-123" {
		t.Errorf("intl authTokenURL = %s", got)
	}
	if got := itl.loginAcctURL("abc-123"); got != "https://www.workbuddy.ai/v2/plugin/login/account?state=abc-123" {
		t.Errorf("intl loginAcctURL = %s", got)
	}
	if got := itl.chatURL(); got != "https://www.workbuddy.ai/v2/chat/completions" {
		t.Errorf("intl chatURL = %s", got)
	}
	if got := itl.quotaSummaryURL(); got != "https://www.workbuddy.ai/billing/meter/get-user-resource-summary" {
		t.Errorf("intl quotaSummaryURL = %s", got)
	}
}

// Verifies account site routing: prefer the credential's edition; when restored
// from a disabled marker (Auth nil), use the account's edition
func TestAccountProfile(t *testing.T) {
	acc := &Account{Auth: &StoredAuth{Edition: "intl"}}
	if acc.Profile().Key != "intl" {
		t.Fatalf("expected intl via Auth.Edition, got %s", acc.Profile().Key)
	}
	// Disabled-marker restoration scenario: credential file deleted
	acc2 := &Account{Edition: "intl"}
	if acc2.Profile().Key != "intl" {
		t.Fatalf("expected intl via Account.Edition, got %s", acc2.Profile().Key)
	}
	// Legacy credentials (no edition field) fall back to the domestic site
	acc3 := &Account{Auth: &StoredAuth{}}
	if acc3.Profile().Key != "cn" {
		t.Fatalf("expected cn fallback, got %s", acc3.Profile().Key)
	}
}

// Verifies rate-limit detection (429 status / code 6004 / rate-limit keywords,
// Chinese and English)
func TestIsRateLimited(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, "{}", true},
		{400, `{"code":6004,"msg":"x"}`, true},
		{400, "frequency limit", true},
		{400, "frequency limit", true},
		{400, "rate limit exceeded", true},
		{400, "Rate Limit Exceeded", true},
		{400, "ratelimit", true},
		{400, "Too Many Requests", true},
		{400, "some other error", false},
		{500, "{}", false},
	}
	for _, c := range cases {
		if got := isRateLimited(c.status, c.body); got != c.want {
			t.Errorf("isRateLimited(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// Verifies round-robin selection: two accounts alternate
func TestNextAccountRoundRobin(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a1, _ := nextAccount()
	a2, _ := nextAccount()
	a3, _ := nextAccount()
	if a1.Path != "a.json" || a2.Path != "b.json" || a3.Path != "a.json" {
		t.Fatalf("round robin failed: %s %s %s", a1.Path, a2.Path, a3.Path)
	}
	t.Logf("round-robin order: %s %s %s", a1.Path, a2.Path, a3.Path)
}

// Verifies cooldown blocking: a cooling account is skipped and the other
// compensates
func TestNextAccountSkipsCooldown(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "b.json" {
		t.Fatalf("expected b.json (only non-cooldown), got %s", a.Path)
	}
	t.Logf("cooldown skip works, selected %s", a.Path)
}

// Verifies an error when every account is cooling
func TestNextAccountAllCooldown(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
		{Path: "b.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(2 * time.Hour)},
	}
	rrIndex = 0
	accountMu.Unlock()

	_, err := nextAccount()
	if err == nil {
		t.Fatal("expected error when all accounts in cooldown")
	}
	t.Logf("all-cooldown error: %v", err)
}

// Verifies automatic recovery after the cooldown expires
func TestNextAccountCooldownExpiry(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(-time.Minute)},
		{Path: "b.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "a.json" {
		t.Fatalf("expected a.json (cooldown expired), got %s", a.Path)
	}
	t.Logf("expired cooldown recovers, selected %s", a.Path)
}

func TestNextAccountForModelPrefersKnownFreeExhausted(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "empty-free.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true, ModelStates: map[string]*modelRuntimeState{
			"free-model": {CostClass: modelCostFree},
		}},
		{Path: "available.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaRemaining: 10},
	}
	rrIndex = 0
	accountMu.Unlock()

	acc, kind, err := nextAccountForModel("free-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "empty-free.json" || kind != selectionFreeExhausted {
		t.Fatalf("expected free exhausted account, got %s kind=%s", acc.Path, kind)
	}
}

func TestNextAccountForModelAllowsOneUnknownProbe(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true},
	}
	rrIndex = 0
	accountMu.Unlock()

	acc, kind, err := nextAccountForModel("unknown-model", nil)
	if err != nil || acc.Path != "a.json" || kind != selectionProbeExhausted {
		t.Fatalf("expected one controlled probe, acc=%v kind=%s err=%v", acc, kind, err)
	}
	if _, _, err = nextAccountForModel("unknown-model", nil); err == nil || !strings.Contains(err.Error(), "probeWaiting=1") {
		t.Fatalf("expected probe cooldown error, got %v", err)
	}
}

func TestNextAccountForModelSkipsPaidAndQuotaBlocked(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "paid.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaExhausted: true, ModelStates: map[string]*modelRuntimeState{
			"paid-model": {CostClass: modelCostPaid, QuotaBlocked: true},
		}},
	}
	rrIndex = 0
	accountMu.Unlock()
	if _, _, err := nextAccountForModel("paid-model", nil); err == nil || !strings.Contains(err.Error(), "quota blocked=1") {
		t.Fatalf("expected model quota block, got %v", err)
	}
}

func TestModelCooldownOnlyBlocksTriggeringModel(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{{Path: "a.json", Auth: &StoredAuth{}, ModelStates: map[string]*modelRuntimeState{
		"limited-model": {CostClass: modelCostFree, CooldownUntil: time.Now().Add(time.Hour)},
	}}}
	rrIndex = 0
	accountMu.Unlock()
	if _, _, err := nextAccountForModel("limited-model", nil); err == nil || !strings.Contains(err.Error(), "modelCooling=1") {
		t.Fatalf("expected model cooldown, got %v", err)
	}
	acc, _, err := nextAccountForModel("other-model", nil)
	if err != nil || acc.Path != "a.json" {
		t.Fatalf("other model should remain usable, acc=%v err=%v", acc, err)
	}
}

func TestMarkModelQuotaBlockedDoesNotOverwriteAccountBalance(t *testing.T) {
	oldDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	acc := &Account{Path: "a.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaRemaining: 123}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()
	markModelQuotaBlocked(acc, "paid-model", "14018")
	accountMu.Lock()
	state := acc.ModelStates["paid-model"]
	remaining := acc.QuotaRemaining
	exhausted := acc.QuotaExhausted
	accountMu.Unlock()
	if state == nil || !state.QuotaBlocked {
		t.Fatalf("model block missing: %+v", state)
	}
	if remaining != 123 || exhausted {
		t.Fatalf("model 14018 must not overwrite account balance: remaining=%v exhausted=%v", remaining, exhausted)
	}
}

// 14018 only means the account's quota is exhausted; it says nothing about model
// cost. A known free model must keep its free classification when 14018 arrives
// and must never be rewritten to paid.
func TestQuotaBlockedMustNotRewriteFreeModelAsPaid(t *testing.T) {
	chdirTemp(t)
	acc := &Account{Path: "free.json", Auth: &StoredAuth{Edition: "intl"}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{
			"deepseek-v4.1-flash": {CostClass: modelCostFree},
		}}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	markModelQuotaBlocked(acc, "deepseek-v4.1-flash", "14018 Credits exhausted")

	accountMu.Lock()
	state := acc.ModelStates["deepseek-v4.1-flash"]
	accountMu.Unlock()
	if state.CostClass != modelCostFree {
		t.Fatalf("14018 must not overwrite a free model's cost class, got %q", state.CostClass)
	}
	if !state.QuotaBlocked {
		t.Fatal("14018 should still block this account for the model until quota recovers")
	}
	// An unknown model must also stay unknown after 14018 and must not be treated
	// as paid.
	acc2 := &Account{Path: "unknown.json", Auth: &StoredAuth{Edition: "intl"}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{"mystery": {CostClass: modelCostUnknown}}}
	markModelQuotaBlocked(acc2, "mystery", "14018")
	accountMu.Lock()
	got := acc2.ModelStates["mystery"].CostClass
	accountMu.Unlock()
	if got != modelCostUnknown {
		t.Fatalf("14018 must not turn an unknown model into paid, got %q", got)
	}
}

// Legacy snapshots may contain the dirty "14018 mistakenly written as paid"
// state; restoration must downgrade it to unknown.
func TestSanitizeRestoredModelStateFixesLegacyPaidFromQuotaBlock(t *testing.T) {
	legacy := &modelRuntimeState{
		CostClass:    modelCostPaid,
		QuotaBlocked: true,
		LastReason:   `{"error":{"data":{"code":14018,"msg":"Credits exhausted."}}}`,
	}
	sanitizeRestoredModelState(legacy)
	if legacy.CostClass != modelCostUnknown {
		t.Fatalf("legacy 14018-derived paid should be reset to unknown, got %q", legacy.CostClass)
	}
	if !legacy.QuotaBlocked {
		t.Fatal("quota block should be preserved")
	}

	// A paid model genuinely learned from credit evidence (not caused by quota
	// exhaustion) must stay paid.
	genuinePaid := &modelRuntimeState{CostClass: modelCostPaid}
	sanitizeRestoredModelState(genuinePaid)
	if genuinePaid.CostClass != modelCostPaid {
		t.Fatalf("genuine paid model must stay paid, got %q", genuinePaid.CostClass)
	}

	// Free models are unaffected.
	free := &modelRuntimeState{CostClass: modelCostFree, QuotaBlocked: true}
	sanitizeRestoredModelState(free)
	if free.CostClass != modelCostFree {
		t.Fatalf("free model must stay free, got %q", free.CostClass)
	}
}

// QuotaBlocked must short-circuit scheduling so quota-exhausted accounts are not
// repeatedly picked for probing.
func TestQuotaBlockedShortCircuitsScheduling(t *testing.T) {
	acc := &Account{Path: "exhausted.json", Auth: &StoredAuth{}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{
			"m": {CostClass: modelCostUnknown, QuotaBlocked: true},
		}}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()
	if _, _, err := nextAccountForModel("m", nil); err == nil {
		t.Fatal("quota-blocked account must not be selected for the model")
	}
}

func TestQuotaRecoveryClearsBlocksKeepsModelCooldown(t *testing.T) {
	until := time.Now().Add(time.Hour)
	acc := &Account{QuotaExhausted: true, ModelStates: map[string]*modelRuntimeState{
		"m": {CostClass: modelCostPaid, QuotaBlocked: true, CooldownUntil: until},
	}}
	accountMu.Lock()
	acc.QuotaExhausted = false
	for _, state := range acc.ModelStates {
		state.QuotaBlocked = false
		state.NextProbeAt = time.Time{}
	}
	state := acc.ModelStates["m"]
	accountMu.Unlock()
	if state.QuotaBlocked || !state.CooldownUntil.Equal(until) {
		t.Fatalf("quota recovery should clear only quota block: %+v", state)
	}
}

func TestIsModelRateLimited(t *testing.T) {
	if !isModelRateLimited(`{"code":6004,"msg":"switch to another model to continue"}`) {
		t.Fatal("6004 should be model-level rate limit")
	}
	if isModelRateLimited(`{"code":14018,"msg":"quota exhausted"}`) {
		t.Fatal("14018 should not be model rate limit")
	}
}

func TestIsQuotaExhausted(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, `{"error":{"data":{"code":14018,"msg":"quota exhausted"}}}`, true},
		{429, `{"error":{"data":{"code":14018,"msg":"Credits exhausted"}}}`, true},
		{429, `{"code":6004,"msg":"rate limit"}`, false},
		{400, `{"code":14018,"msg":"quota exhausted"}`, true},
	}
	for _, tc := range cases {
		if got := isQuotaExhausted(tc.status, tc.body); got != tc.want {
			t.Errorf("isQuotaExhausted(%d, %q)=%v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestIsAlreadyCheckedIn(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{409, `{"code":10001,"msg":"already checked in today"}`, true},
		{400, `{"code":14001,"msg":"already checked in today"}`, true},
		{409, `{"msg":"Already checked in today"}`, true},
		{500, "connection reset", false},
		{400, `{"code":10002,"msg":"insufficient credits"}`, false},
	}
	for _, tc := range cases {
		if got := isAlreadyCheckedIn(tc.status, tc.body); got != tc.want {
			t.Errorf("isAlreadyCheckedIn(%d, %q)=%v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestNextDailyCheckinUTC8(t *testing.T) {
	loc := time.FixedZone("test", 8*60*60)
	before := time.Date(2026, 9, 15, 7, 59, 0, 0, loc)
	if got := nextDailyCheckin(before); !got.Equal(time.Date(2026, 9, 15, 8, 0, 0, 0, checkinZone)) {
		t.Fatalf("before first window got %v", got)
	}
	after := time.Date(2026, 9, 15, 9, 1, 0, 0, loc)
	if got := nextDailyCheckin(after); !got.Equal(time.Date(2026, 9, 16, 8, 0, 0, 0, checkinZone)) {
		t.Fatalf("without a pending account plan got %v", got)
	}
}

func TestCheckinAccountCNAndSkipIntl(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v2/billing/meter/daily-checkin" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("X-User-Id") != "user-1" {
			t.Errorf("missing checkin auth headers")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer server.Close()

	oldOrigin := profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Origin = server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Origin = oldOrigin
		cfg.HttpClient = oldClient
	}()

	cn := &Account{Path: "cn.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "test-access"},
		Account: StoredAccount{UID: "user-1"},
	}}
	result, err := checkinAccount(context.Background(), cn)
	if err != nil || result != "ok" || calls != 1 {
		t.Fatalf("cn checkin result=%q calls=%d err=%v", result, calls, err)
	}

	intl := &Account{Path: "intl.json", Auth: &StoredAuth{
		Edition: "intl",
		Auth:    StoredTokens{AccessToken: "test-access"},
	}}
	result, err = checkinAccount(context.Background(), intl)
	if err != nil || result != "global_skipped" || calls != 1 {
		t.Fatalf("intl should be skipped: result=%q calls=%d err=%v", result, calls, err)
	}

	enterprise := &Account{Path: "enterprise.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "test-access"},
		Account: StoredAccount{UID: "user-1", EnterpriseID: "ent-1"},
	}}
	result, err = checkinAccount(context.Background(), enterprise)
	if err != nil || result != "enterprise_skipped" || calls != 1 {
		t.Fatalf("enterprise checkin must not call personal endpoint: result=%q calls=%d err=%v", result, calls, err)
	}
}

// Verifies auth-failure detection (401/403 / invalid token / expired login, etc.)
func TestIsAuthFailure(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, "{}", true},
		{403, "{}", true},
		{400, `{"msg":"invalid token"}`, true},
		{400, "unauthorized", true},
		{400, "expired login", true},
		{400, "login expired, please log in again", true},
		{400, "some other error", false},
		{429, "frequency limit", false},
		{500, "{}", false},
	}
	for _, c := range cases {
		if got := isAuthFailure(c.status, c.body); got != c.want {
			t.Errorf("isAuthFailure(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// Verifies round-robin skips disabled accounts
func TestNextAccountSkipsDisabled(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "revoked"},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "b.json" {
		t.Fatalf("expected b.json (only non-disabled), got %s", a.Path)
	}
	t.Logf("disabled skip works, selected %s", a.Path)
}

// Verifies an error with a re-login hint when every account is disabled
func TestNextAccountAllDisabled(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "expired"},
		{Path: "b.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "revoked"},
	}
	rrIndex = 0
	accountMu.Unlock()

	_, err := nextAccount()
	if err == nil {
		t.Fatal("expected error when all accounts disabled")
	}
	t.Logf("all-disabled error: %v", err)
}

// Verifies disableAccount: stops scheduling and writes the disabled marker.
// Deleting credentials requires "a read-only check that explicitly confirms the
// credential is unusable"; without an HTTP client in the test environment the
// file must be conservatively kept.
func TestDisableAccount(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"
	// Write a fake credential file
	if err := os.WriteFile(authPath, []byte(`{"auth":{"accessToken":"x"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	acc := &Account{Path: authPath, Auth: &StoredAuth{
		Auth:    StoredTokens{AccessToken: "x"},
		Account: StoredAccount{Nickname: "tester", UID: "uid-1"},
	}}

	disableAccount(acc, "token refresh failed (HTTP 401): invalid token")

	accountMu.Lock()
	disabled := acc.Disabled
	reason := acc.DisabledReason
	accountMu.Unlock()
	if !disabled {
		t.Fatal("account should be marked disabled")
	}
	if reason == "" {
		t.Fatal("disabled reason should be recorded")
	}
	// The file must be kept when a failure cannot be confirmed: deletion is
	// irreversible and a mistaken delete forces a re-login.
	if _, err := os.Stat(authPath); err != nil {
		t.Fatalf("credential file should be kept when the failure cannot be confirmed, stat err=%v", err)
	}
	// The disabled-marker file should exist
	if _, err := os.Stat(markerPath(authPath)); err != nil {
		t.Fatalf("marker file should exist: %v", err)
	}
	t.Logf("disableAccount works: disabled=%v reason=%q", disabled, reason)
}

// Verifies a disabled-marker file can be restored as a disabled account (Auth
// nil) with a re-login hint
func TestLoadDisabledMarkers(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"

	// Build a disabled-marker file
	marker := disabledMarker{
		Path:       authPath,
		Reason:     "authorization failed (token refresh failed HTTP 401)",
		DisabledAt: time.Now().Unix(),
		Nickname:   "tester",
		UID:        "uid-1",
	}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(authPath), data, 0600); err != nil {
		t.Fatal(err)
	}

	// Simulate explicitly passing the path via -auth
	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = authPath
	cfg.AuthDir = ""
	cfg.AuthExplicit = true

	accounts = nil
	loadDisabledMarkers()

	if len(accounts) != 1 {
		t.Fatalf("expected 1 disabled account, got %d", len(accounts))
	}
	acc := accounts[0]
	if !acc.Disabled || acc.Auth != nil {
		t.Fatalf("expected disabled account with nil Auth, got disabled=%v authNil=%v", acc.Disabled, acc.Auth == nil)
	}
	if acc.Nickname != "tester" || acc.UID != "uid-1" {
		t.Fatalf("marker nickname/uid not restored: %+v", acc)
	}
	t.Logf("loadDisabledMarkers restores: %s (nickname=%s)", acc.Path, acc.Nickname)
}

// Verifies the disabled marker is cleared after a successful login
func TestClearDisabledMarker(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"
	marker := disabledMarker{Path: authPath, Reason: "revoked"}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(authPath), data, 0600); err != nil {
		t.Fatal(err)
	}

	clearDisabledMarker(authPath)
	if _, err := os.Stat(markerPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("marker file should be removed, stat err=%v", err)
	}
	t.Log("clearDisabledMarker works")
}

// Verifies auto-discovery mode: without -auth/-auth-dir, all workbuddy*.json in
// the current directory are scanned
func TestCollectConfiguredAuthPathsAutoDiscover(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// Populate the directory with credential files + a non-credential file + a
	// disabled marker + the runtime status snapshot (which must be excluded)
	for _, name := range []string{"workbuddy.json", "workbuddy2.json", "workbuddy-3.json", "other.json", "workbuddy.json.disabled", "workbuddy-status.json"} {
		if err := os.WriteFile(name, []byte(`{"auth":{"accessToken":"x"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = ""
	cfg.AuthExplicit = false

	paths := collectConfiguredAuthPaths()
	if len(paths) != 3 {
		t.Fatalf("expected 3 workbuddy json files, got %d: %v", len(paths), paths)
	}
	want := []string{"workbuddy-3.json", "workbuddy.json", "workbuddy2.json"} // sort.Strings ordering result
	for i, p := range want {
		if paths[i] != p {
			t.Fatalf("paths[%d] = %s, want %s (all: %v)", i, paths[i], p, paths)
		}
	}
	t.Logf("auto-discover paths: %v", paths)
}

// Verifies auto-discovery mode falls back to the default path when the directory
// has no credential files
func TestCollectConfiguredAuthPathsAutoDiscoverFallback(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// The directory is empty

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = ""
	cfg.AuthExplicit = false

	paths := collectConfiguredAuthPaths()
	if len(paths) != 1 || paths[0] != "workbuddy.json" {
		t.Fatalf("expected fallback to default workbuddy.json, got %v", paths)
	}
	t.Logf("fallback path: %v", paths)
}

// Verifies credential hot reload: added/updated/deleted credential files all
// converge the account pool in place without a restart
func TestReloadAccounts(t *testing.T) {
	dir := t.TempDir()

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
		accountMu.Lock()
		accounts = nil
		rrIndex = 0
		accountMu.Unlock()
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = dir
	cfg.AuthExplicit = false

	accountMu.Lock()
	accounts = nil
	rrIndex = 0
	accountMu.Unlock()

	pa := filepath.Join(dir, "workbuddy-a.json")

	// 1) New credential file → joins the pool automatically
	if err := os.WriteFile(pa, []byte(`{"auth":{"accessToken":"a1"},"account":{"nickname":"A"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !reloadAccounts() {
		t.Fatal("expected changed=true after adding new credential file")
	}
	if len(accounts) != 1 || accounts[0].Auth.Auth.AccessToken != "a1" {
		t.Fatalf("expected 1 account with a1, got %+v", accounts)
	}
	t.Logf("hot-add works: %s joined the pool", accounts[0].Path)

	// 2) Content changed (including site switch cn -> intl) → credential replaced in place
	if err := os.WriteFile(pa, []byte(`{"auth":{"accessToken":"a2-longer"},"account":{"nickname":"A2"},"edition":"intl"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !reloadAccounts() {
		t.Fatal("expected changed=true after credential content change")
	}
	if len(accounts) != 1 || accounts[0].Auth.Auth.AccessToken != "a2-longer" {
		t.Fatalf("expected hot-reloaded a2 credential, got %+v", accounts)
	}
	if accounts[0].Profile().Key != "intl" {
		t.Fatalf("expected intl profile after reload, got %s", accounts[0].Profile().Key)
	}
	t.Logf("hot-update works: edition now %s", accounts[0].Profile().Key)

	// 3) Disabled phantom account: file deleted but account Disabled → kept (to
	// prompt re-login)
	accountMu.Lock()
	accounts[0].Disabled = true
	accountMu.Unlock()
	if err := os.Remove(pa); err != nil {
		t.Fatal(err)
	}
	if reloadAccounts() {
		t.Fatal("disabled phantom account should be kept, expected no change")
	}
	if len(accounts) != 1 || !accounts[0].Disabled {
		t.Fatalf("disabled phantom should be kept, got %d accounts", len(accounts))
	}
	t.Log("disabled phantom kept after file deletion")

	// 4) A non-disabled account's file deleted → leaves the pool
	accountMu.Lock()
	accounts[0].Disabled = false
	accounts[0].Auth = &StoredAuth{Auth: StoredTokens{AccessToken: "x"}}
	accountMu.Unlock()
	if !reloadAccounts() {
		t.Fatal("expected changed=true after credential file deletion")
	}
	if len(accounts) != 0 {
		t.Fatalf("expected empty pool after deletion, got %d", len(accounts))
	}
	t.Log("hot-remove works: pool emptied after file deletion")
}

// Verifies tailLines reads the last N lines of a file
func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/test.log"
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(f, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "line4" || lines[1] != "line5" {
		t.Fatalf("tailLines got %v", lines)
	}
	t.Logf("tailLines(2) = %v", lines)
}

// Verifies status snapshot writing across active / cooldown / disabled states
func TestWriteStatusSnapshot(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{
			Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: StoredAccount{Nickname: "alice", UID: "u1"},
		}, QuotaTotal: 2000, QuotaUsed: 1500, QuotaRemaining: 500, IsPaidUser: true, QuotaKnown: true},
		{Path: "b.json", Auth: &StoredAuth{
			Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: StoredAccount{Nickname: "bob", UID: "u2"},
		}, CooldownUntil: time.Now().Add(30 * time.Minute), CooldownMsg: "frequency limit"},
		{Path: "c.json", Disabled: true, DisabledReason: "revoked", Nickname: "carol", UID: "u3"},
	}
	accountMu.Unlock()

	writeStatusSnapshot()

	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 3 {
		t.Fatalf("expected 3 accounts in snapshot, got %d", len(snap.Accounts))
	}
	states := map[string]bool{}
	for _, a := range snap.Accounts {
		states[a.State] = true
	}
	if !states["active"] || !states["cooldown"] || !states["disabled"] {
		t.Fatalf("expected all three states in snapshot, got %v", states)
	}
	if snap.Accounts[0].QuotaTotal != 2000 || snap.Accounts[0].QuotaUsed != 1500 || snap.Accounts[0].QuotaRemaining != 500 || !snap.Accounts[0].IsPaidUser || !snap.Accounts[0].QuotaKnown {
		t.Fatalf("quota fields not preserved in snapshot: %+v", snap.Accounts[0])
	}
	t.Logf("snapshot states: %v", states)
}

func TestWriteStatusSnapshotMarksExpiredToken(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: "expired.json", Auth: &StoredAuth{
		Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(-time.Hour).Unix()},
		Account: StoredAccount{Nickname: "expired-user"},
	}}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	writeStatusSnapshot()
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 1 || snap.Accounts[0].State != "expired" {
		t.Fatalf("expected expired state, got %+v", snap.Accounts)
	}
}

func TestRenderAccountTableUsesFullYearAndNoEmoji(t *testing.T) {
	expires := time.Date(2027, 9, 5, 1, 36, 55, 0, time.Local)
	table := renderAccountTable([]accountSnapshot{{
		Path:           "workbuddy4.json",
		Edition:        "intl",
		Nickname:       "user@example.com",
		State:          "paid_exhausted",
		TokenExpiresAt: expires.Unix(),
		QuotaTotal:     1100,
		QuotaUsed:      1100,
		QuotaRemaining: 0,
		IsPaidUser:     false,
		QuotaKnown:     true,
		QuotaExhausted: true,
		FreeModels:     1,
		ModelCooldowns: 2,
	}})
	for _, want := range []string{"Credential file", "workbuddy4.json", "International site", "Paid exhausted", "2027-09-05 01:36:55", "Total quota", "Used", "Remaining", "Plan", "Checkin", "Model cooldown", "1100"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table missing %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "Free model") {
		t.Fatal("removed free-model column remains")
	}
	if strings.Contains(table, "Note") {
		t.Fatalf("table should not contain removed description column:\n%s", table)
	}
	if strings.ContainsAny(table, "✅🔒❌🕐📊📜⚠️") {
		t.Fatalf("table should not contain emoji:\n%s", table)
	}
}

func TestUsageCreditAndObserveModelCost(t *testing.T) {
	for _, tc := range []struct {
		usage map[string]any
		want  float64
		ok    bool
	}{
		{map[string]any{"credit": float64(0)}, 0, true},
		{map[string]any{"credit": "1.25"}, 1.25, true},
		{map[string]any{"total_tokens": 1}, 0, false},
		{nil, 0, false},
	} {
		got, ok := usageCredit(tc.usage)
		if got != tc.want || ok != tc.ok {
			t.Errorf("usageCredit(%v)=%v/%v want %v/%v", tc.usage, got, ok, tc.want, tc.ok)
		}
	}

	oldDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	acc := &Account{Path: "empty.json", Auth: &StoredAuth{}, QuotaExhausted: true}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()
	var logBuf bytes.Buffer
	oldLogWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldLogWriter)
	// Sample too small: credit=0 but total_tokens too low; must not be classified free
	observeModelCredit(acc, "tiny-model", map[string]any{"credit": float64(0), "total_tokens": float64(2)}, 10)
	accountMu.Lock()
	tiny := acc.ModelStates["tiny-model"]
	accountMu.Unlock()
	if tiny != nil && tiny.CostClass == modelCostFree {
		t.Fatalf("tiny sample must not be learned as free: %+v", tiny)
	}

	// Sufficient sample: credit=0 with total_tokens above the threshold; classified free
	observeModelCredit(acc, "free-model", map[string]any{"credit": float64(0), "total_tokens": float64(500)}, 1)
	accountMu.Lock()
	state := acc.ModelStates["free-model"]
	accountMu.Unlock()
	if state == nil || state.CostClass != modelCostFree || state.QuotaBlocked {
		t.Fatalf("free model not learned: %+v", state)
	}
	if text := logBuf.String(); !strings.Contains(text, "[FreeModel]") || !strings.Contains(text, "paid-balance-exhausted account empty.json") {
		t.Fatalf("missing explicit free model exhausted-account log: %s", text)
	}
	observeModelCredit(acc, "paid-model", map[string]any{"credit": 2.5}, 2)
	accountMu.Lock()
	paid := acc.ModelStates["paid-model"]
	accountMu.Unlock()
	if paid == nil || paid.CostClass != modelCostPaid {
		t.Fatalf("paid model not learned: %+v", paid)
	}
}

func TestKnownFreeModelUsesExhaustedAccountAndLogs(t *testing.T) {
	chdirTemp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":500}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "empty-free.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{"free-model": {CostClass: modelCostFree}}}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	text := logs.String()
	for _, want := range []string{
		"requested model free-model is a known free model, selecting paid-balance-exhausted account empty-free.json to send the request",
		"requested model free-model is free, completed using a paid-balance-exhausted account empty-free.json, usage.credit=0",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing log %q:\n%s", want, text)
		}
	}
}

func TestUnknownModelQuotaProbeStopsAfterOneExhaustedAccount(t *testing.T) {
	chdirTemp(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"data":{"code":14018,"msg":"quota exhausted"}}}`)
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	a := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaExhausted: true}
	b := &Account{Path: "b.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "b", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaExhausted: true}
	accounts, rrIndex = []*Account{a, b}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"unknown-paid","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)
	if rec.Code != http.StatusServiceUnavailable || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	accountMu.Lock()
	blockedA := a.ModelStates["unknown-paid"] != nil && a.ModelStates["unknown-paid"].QuotaBlocked
	_, touchedB := b.ModelStates["unknown-paid"]
	accountMu.Unlock()
	if !blockedA || touchedB {
		t.Fatalf("expected only first exhausted account probed: blockedA=%v touchedB=%v", blockedA, touchedB)
	}
}

func TestParseQuotaSummary(t *testing.T) {
	data := []byte(`{"Packages":[{"CycleTotalCapacity":"1500","CycleUsedCapacity":"69.98999993","CycleRemainCapacity":"1430.01000007"},{"CycleTotalCapacity":"500","CycleUsedCapacity":"500","CycleRemainCapacity":"0"}],"IsPaidUser":true}`)
	total, used, remaining, paid, plan, err := parseQuotaSummary(data)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2000 || used != 569.98999993 || remaining != 1430.01000007 || !paid {
		t.Fatalf("unexpected quota summary: total=%v used=%v remaining=%v paid=%v", total, used, remaining, paid)
	}
	if plan != planUnknown {
		t.Fatalf("summary alone cannot establish the current plan, got %q", plan)
	}
}

// Summary flags are no longer treated directly as the current valid entitlement.
func TestPlanLabelFromSummary(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"historical trial flag", `{"ProTrialStatus":1,"IsPaidUser":false}`, planUnknown},
		{"string trial flag", `{"ProTrialStatus":"1","IsPaidUser":false}`, planUnknown},
		{"trial and paid flags together", `{"ProTrialStatus":1,"IsPaidUser":true}`, planUnknown},
		{"paid flag cannot decide the tier", `{"ProTrialStatus":0,"IsPaidUser":true}`, planUnknown},
		{"empty flags still require an entitlement check", `{"ProTrialStatus":0,"IsPaidUser":false}`, planUnknown},
		{"missing fields", `{"Packages":[]}`, planUnknown},
		{"unexpected types", `{"ProTrialStatus":{"unexpected":true},"IsPaidUser":false}`, planUnknown},
		{"domestic site without trial flag", `{"IsPaidUser":false,"SubscriptionPackageCode":""}`, planUnknown},
		{"unknown subscription package", `{"IsPaidUser":false,"SubscriptionPackageCode":"pkg-code-000"}`, planUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var summary quotaSummaryData
			if err := json.Unmarshal([]byte(tc.json), &summary); err != nil {
				t.Fatal(err)
			}
			if got := planLabelFromSummary(summary); got != tc.want {
				t.Fatalf("plan=%q want=%q", got, tc.want)
			}
		})
	}
}

// The plan column must display Pro trial/pro/Free and no longer show 1 or 0.
func TestRenderAccountTableShowsPlanLabel(t *testing.T) {
	table := renderAccountTable([]accountSnapshot{
		{Path: "intl-trial.json", Edition: "intl", Nickname: "trial@example.com", State: "active", QuotaTotal: 500, QuotaRemaining: 500, QuotaKnown: true, PlanLabel: "Pro trial"},
		{Path: "cn-paid.json", Edition: "cn", Nickname: "paid@example.com", State: "active", QuotaTotal: 2200, QuotaRemaining: 400, QuotaKnown: true, PlanLabel: "pro"},
		{Path: "cn-free.json", Edition: "cn", Nickname: "free@example.com", State: "active", QuotaTotal: 2200, QuotaRemaining: 400, QuotaKnown: true, PlanLabel: "Free"},
	})
	for _, want := range []string{"Plan", "Pro trial", "pro", "Free"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table missing %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "Paid user") {
		t.Fatalf("table should no longer contain the Paid user column:\n%s", table)
	}
}

func TestLockAccountWithContextTimeout(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if lockAccountWithContext(ctx, &mu) {
		t.Fatal("lock should time out while mutex is held")
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("unexpected timeout duration: %v", elapsed)
	}
}

func TestFormatQuotaRoundsToTwoDecimals(t *testing.T) {
	cases := map[float64]string{
		4566:               "4566",
		72.01999992:        "72.02",
		4493.9800000800005: "4493.98",
		0.001:              "0",
	}
	for input, want := range cases {
		if got := formatQuota(input); got != want {
			t.Errorf("formatQuota(%v)=%q want %q", input, got, want)
		}
	}
}

func TestRenderAccountTableRowsHaveEqualDisplayWidth(t *testing.T) {
	table := renderAccountTable([]accountSnapshot{
		{Path: "workbuddy1.json", Nickname: "user-a", Edition: "cn", State: "quota_exhausted", QuotaKnown: true, QuotaTotal: 2000, QuotaUsed: 2000},
		{Path: "workbuddy2.json", Nickname: "user-b", Edition: "cn", State: "active", QuotaKnown: true, QuotaTotal: 2000, QuotaUsed: 69.98999993, QuotaRemaining: 1930.01000007},
	})
	lines := strings.Split(table, "\n")
	want := displayWidth(lines[0])
	for i, line := range lines {
		if got := displayWidth(line); got != want {
			t.Fatalf("line %d display width=%d want=%d:\n%s", i, got, want, table)
		}
	}
}

func TestRenderAccountTableUsesContentWidthsWithoutLosingColumns(t *testing.T) {
	table := renderAccountTable([]accountSnapshot{
		{Path: "a.json", Nickname: "u", Edition: "cn", State: "active", Checkin: checkinStatusSnapshot{Status: "success"},
			QuotaKnown: true, QuotaTotal: 1, QuotaUsed: 0, QuotaRemaining: 1, PlanLabel: "Free"},
	})
	lines := strings.Split(table, "\n")
	width := displayWidth(lines[0])
	if width > 150 {
		t.Fatalf("short account table still uses %d columns:\n%s", width, table)
	}
	if strings.Contains(table, "  |   |") {
		t.Fatalf("empty fixed-width gaps remain:\n%s", table)
	}
	for _, header := range []string{"Account", "Site", "Status", "Plan", "Checkin", "Model cooldown"} {
		if !strings.Contains(table, header) {
			t.Fatalf("missing column %s", header)
		}
	}
	long := renderAccountTable([]accountSnapshot{
		{Path: strings.Repeat("very-long-file-", 4) + ".json", Nickname: strings.Repeat("long-account-", 6), Edition: "cn", State: "active"},
	})
	longLines := strings.Split(long, "\n")
	want := displayWidth(longLines[0])
	for i, line := range longLines {
		if got := displayWidth(line); got != want {
			t.Fatalf("long row %d width=%d want=%d", i, got, want)
		}
	}
	if !strings.Contains(long, "...") {
		t.Fatal("long account/file should be truncated instead of expanding without bound")
	}
}

func TestRenderAccountTableCompactAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []accountSnapshot
	}{
		{"empty", nil},
		{"mixed Chinese and ASCII", []accountSnapshot{
			{Path: "workbuddy1.json", Nickname: "test account A", Edition: "cn", State: "active",
				TokenExpiresAt: 1900000000, QuotaKnown: true, QuotaTotal: 2000, QuotaUsed: 69.99,
				QuotaRemaining: 1930.01, PlanLabel: "Trial",
				Checkin: checkinStatusSnapshot{Date: checkinDay(time.Now()), Status: "success"}},
			{Path: "a.json", Nickname: "synthetic-user", Edition: "intl", State: "cooldown",
				ModelCooldowns: 12},
		}},
		{"bounded long cells", []accountSnapshot{
			{Path: strings.Repeat("very-long-", 20) + ".json", Nickname: strings.Repeat("synthetic test account ", 30),
				Edition: "cn", State: "expired", PlanLabel: strings.Repeat("Plan ", 40), QuotaKnown: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := renderAccountTable(tc.rows)
			lines := strings.Split(table, "\n")
			width := displayWidth(lines[0])
			if width > 216 {
				t.Fatalf("table exceeds the combined column caps: %d", width)
			}
			for _, line := range lines {
				if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "|") {
					if displayWidth(line) != width {
						t.Fatalf("unaligned row: %q", line)
					}
				}
				if strings.HasPrefix(line, "|") && strings.Count(line, "|") != 13 {
					t.Fatalf("expected all 12 columns, got: %q", line)
				}
			}
			if len(tc.rows) == 2 {
				for _, value := range []string{"test account A", "synthetic-user", "1930.01", "Signed", "12"} {
					if !strings.Contains(table, value) {
						t.Fatalf("compaction lost actual content %q", value)
					}
				}
			}
			t.Logf("synthetic account acceptance: scenario=%s columns=12 actual terminal width=%d\n%s", tc.name, width, table)
		})
	}
}

// Builds messages for table-driven tests
func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

// Extracts each message's role for assertions
func rolesOf(obj map[string]any) []string {
	messages, _ := obj["messages"].([]any)
	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, roleOfMessage(m))
	}
	return roles
}

// Verifies conversation structure normalization: fixes upstream 11128 (first
// message is not system prompt)
func TestEnsureLeadingSystemMessage(t *testing.T) {
	cases := []struct {
		name       string
		messages   []any
		wantRoles  []string
		wantInject bool // whether the first message should be the injected fallback system
	}{
		{
			name:      "first message already system stays unchanged",
			messages:  []any{msg("system", "You are helpful"), msg("user", "hi")},
			wantRoles: []string{"system", "user"},
		},
		{
			name:       "first message user with no system (domestic lenient / international strict; system injected uniformly)",
			messages:   []any{msg("user", "hi")},
			wantRoles:  []string{"system", "user"},
			wantInject: true,
		},
		{
			name:       "first message assistant (continuation) gets system injected",
			messages:   []any{msg("assistant", "Sure")},
			wantRoles:  []string{"system", "assistant"},
			wantInject: true,
		},
		{
			name:       "first message tool (tool results only) gets system injected",
			messages:   []any{msg("tool", "result")},
			wantRoles:  []string{"system", "tool"},
			wantInject: true,
		},
		{
			name:      "later system promoted to the front",
			messages:  []any{msg("user", "hi"), msg("system", "You are helpful"), msg("user", "bye")},
			wantRoles: []string{"system", "user", "user"},
		},
		{
			name:      "first message developer normalized to system",
			messages:  []any{msg("developer", "You are helpful"), msg("user", "hi")},
			wantRoles: []string{"system", "user"},
		},
		{
			name:       "empty messages get system injected",
			messages:   []any{},
			wantRoles:  []string{"system"},
			wantInject: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := map[string]any{"messages": c.messages}
			ensureLeadingSystemMessage(obj)
			got := rolesOf(obj)
			if len(got) != len(c.wantRoles) {
				t.Fatalf("roles = %v, want %v", got, c.wantRoles)
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Fatalf("roles = %v, want %v", got, c.wantRoles)
				}
			}
			messages, _ := obj["messages"].([]any)
			first, _ := messages[0].(map[string]any)
			if c.wantInject {
				if content, _ := first["content"].(string); content != defaultSystemPrompt {
					t.Fatalf("injected system content = %q, want %q", content, defaultSystemPrompt)
				}
			}
			t.Logf("roles -> %v", got)
		})
	}
}

// Verifies missing / invalid messages fields still inject safely (no panic)
func TestEnsureLeadingSystemMessageMissingField(t *testing.T) {
	obj := map[string]any{}
	ensureLeadingSystemMessage(obj)
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("expected 1 injected message, got %#v", obj["messages"])
	}
	if roleOfMessage(messages[0]) != "system" {
		t.Fatalf("expected system first, got %v", roleOfMessage(messages[0]))
	}
	t.Log("missing messages field handled")
}

// Verifies the developer role (GPT-5/Codex) is normalized to system, avoiding
// upstream 11128
// "Illegal API invocation from an unapproved channel"
func TestSanitizeMessagesNormalizesDeveloperRole(t *testing.T) {
	obj := map[string]any{"messages": []any{
		msg("system", "You are helpful."),
		msg("developer", "Be terse."),
		msg("user", "hi"),
	}}
	normalizeDeveloperRoles(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 1)
	roles := rolesOf(obj)
	for _, r := range roles {
		if r == "developer" {
			t.Fatalf("developer role should be normalized, got %v", roles)
		}
	}
	if roles[1] != "system" {
		t.Fatalf("roles = %v, want second = system", roles)
	}
	t.Logf("roles normalized: %v", roles)
}

// Verifies upstream tool_calls increments merge correctly by index into one
// complete tool call
func TestApplyToolCallDeltaMerge(t *testing.T) {
	merged := map[int]*mergedToolCall{}
	var order []int
	applyToolCallDelta(merged, &order, []any{
		map[string]any{"index": float64(0), "id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": ""}},
	})
	applyToolCallDelta(merged, &order, []any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `{"city":`}},
	})
	applyToolCallDelta(merged, &order, []any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `"Beijing"}`}},
	})
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("order = %v, want [0]", order)
	}
	st := merged[0]
	if st.ID != "call_1" || st.Name != "get_weather" {
		t.Fatalf("id/name = %q/%q", st.ID, st.Name)
	}
	if got := st.Args.String(); got != `{"city":"Beijing"}` {
		t.Fatalf("args = %q", got)
	}
	t.Logf("merged tool call: %s(%s) id=%s", st.Name, st.Args.String(), st.ID)
}

// Verifies aggregateCompletion merges streaming tool_calls correctly (fixes the
// old per-chunk append bug)
func TestAggregateCompletionToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"cmpl-1","model":"hy3-preview","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")

	out, err := aggregateCompletion(strings.NewReader(sse), "hy3-preview")
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	choices := chat["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("expected 1 merged tool_call, got %#v", msg["tool_calls"])
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Beijing"}` || call["id"] != "call_1" {
		t.Fatalf("bad merged call: %#v", call)
	}
	t.Logf("aggregateCompletion merged: %v", call)
}

// Verifies Responses -> Chat Completions request conversion
// (instructions/input/tools/tool_choice/parameters)
func TestResponsesToChatRequest(t *testing.T) {
	respReq := map[string]any{
		"model":             "hy3-preview",
		"instructions":      "You are helpful.",
		"max_output_tokens": float64(128),
		"temperature":       float64(0.3),
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "weather?"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"BJ"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "get_weather", "description": "Get weather",
			"parameters": map[string]any{"type": "object"},
		}},
		"tool_choice": "auto",
		"reasoning":   map[string]any{"effort": "high"},
	}

	chat, err := responsesToChatRequest(respReq, "hy3-preview")
	if err != nil {
		t.Fatal(err)
	}
	if chat["model"] != "hy3-preview" || chat["max_tokens"] != float64(128) || chat["temperature"] != float64(0.3) {
		t.Fatalf("bad top-level fields: %#v", chat)
	}
	if chat["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", chat["reasoning_effort"])
	}
	messages := chat["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages (system+user+assistant+tool), got %d: %#v", len(messages), messages)
	}
	if rolesOf(map[string]any{"messages": messages})[0] != "system" {
		t.Fatalf("first message should be system")
	}
	assistant := messages[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("3rd message role = %v", assistant["role"])
	}
	toolMsg := messages[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "sunny" {
		t.Fatalf("tool message = %#v", toolMsg)
	}
	tools := chat["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tools not flattened: %#v", tools)
	}
	if chat["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", chat["tool_choice"])
	}
	t.Log("responses request converted OK")
}

// Verifies responsesToChatRequest handling of a plain-string input and the
// empty-input error
func TestResponsesToChatRequestStringInput(t *testing.T) {
	chat, err := responsesToChatRequest(map[string]any{"model": "x", "input": "hello"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("bad messages: %#v", messages)
	}
	if _, err := responsesToChatRequest(map[string]any{"model": "x"}, "x"); err == nil {
		t.Fatal("empty input should error")
	}
}

// The international site's thinking mode requires the previous assistant message
// to carry reasoning_content, otherwise 11155.
func TestResponsesReasoningContentPassedBack(t *testing.T) {
	chat, err := responsesToChatRequest(map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"role": "user", "content": "check the weather first"},
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "previous round reasoning"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"BJ"}`},
			map[string]any{"type": "function_call", "call_id": "call_2", "name": "get_time", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
			map[string]any{"type": "reasoning", "content": []any{
				map[string]any{"type": "reasoning_text", "text": "full reasoning"},
			}, "summary": []any{
				map[string]any{"type": "summary_text", "text": "this summary must not be used"},
			}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "sunny today"},
			}},
			map[string]any{"role": "user", "content": "say it again"},
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "this must not leak downstream"},
			}},
			map[string]any{"role": "user", "content": "next question"},
			map[string]any{"type": "message", "role": "assistant", "content": "reply without reasoning"},
		},
	}, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 7 {
		t.Fatalf("reasoning and parallel calls should form one assistant message: %#v", messages)
	}
	call := messages[1].(map[string]any)
	if call["reasoning_content"] != "previous round reasoning" {
		t.Fatalf("tool call missing reasoning: %#v", call)
	}
	if calls, ok := call["tool_calls"].([]any); !ok || len(calls) != 2 {
		t.Fatalf("parallel calls were not grouped: %#v", call)
	}
	tool := messages[2].(map[string]any)
	if _, ok := tool["reasoning_content"]; ok {
		t.Fatalf("tool result must not carry reasoning: %#v", tool)
	}
	answer := messages[3].(map[string]any)
	if answer["reasoning_content"] != "full reasoning" {
		t.Fatalf("content text should win over summary, got %#v", answer)
	}
	plain := messages[len(messages)-1].(map[string]any)
	if _, ok := plain["reasoning_content"]; ok {
		t.Fatalf("reasoning before a user turn leaked forward: %#v", plain)
	}
}

func TestResponsesExplicitAssistantReasoningContentIsPreserved(t *testing.T) {
	chat, err := responsesToChatRequest(map[string]any{"input": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"type": "message", "role": "assistant", "content": "ok", "reasoning_content": "original thinking"},
	}}, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	assistant := chat["messages"].([]any)[1].(map[string]any)
	if assistant["reasoning_content"] != "original thinking" {
		t.Fatalf("explicit assistant reasoning was dropped: %#v", assistant)
	}
}

// Regression: reasoning + body + parallel tool calls come from the same
// Responses reply; the Chat upstream sees only one assistant message, with the
// reasoning on that message and tool results following without repeating the
// reasoning.
func TestResponsesReasoningTextAndCallsShareOneAssistant(t *testing.T) {
	chat, err := responsesToChatRequest(map[string]any{
		"model":     "deepseek-v4.1-flash",
		"reasoning": map[string]any{"effort": "high"},
		"input": []any{
			map[string]any{"role": "user", "content": "take a look"},
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "check the logs first"},
			}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "I checked the logs first"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_a", "name": "read", "arguments": `{}`},
			map[string]any{"type": "web_search_call", "id": "ws_1"},
			map[string]any{"type": "function_call", "call_id": "call_b", "name": "grep", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "file"},
			map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "match"},
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "confirmed"},
			}},
			map[string]any{"type": "message", "role": "assistant", "content": "done"},
		},
	}, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	report := repairToolMessageSequence(chat)
	messages := chat["messages"].([]any)
	if len(messages) != 5 || report.MovedMessages != 0 || report.MergedCallMessages != 0 {
		t.Fatalf("unexpected chat topology: report=%+v messages=%#v", report, messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "check the logs first" {
		t.Fatalf("reasoning was not attached to the original reply: %#v", assistant)
	}
	parts, ok := assistant["content"].([]any)
	if !ok || len(parts) != 1 || parts[0].(map[string]any)["text"] != "I checked the logs first" {
		t.Fatalf("text was not attached to the same reply: %#v", assistant)
	}
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("parallel tools were not attached to the same reply: %#v", assistant)
	}
	if messages[2].(map[string]any)["role"] != "tool" || messages[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("tool outputs not adjacent to their call: %#v", messages)
	}
	last := messages[4].(map[string]any)
	if last["reasoning_content"] != "confirmed" || last["content"] != "done" {
		t.Fatalf("next reply lost reasoning or text: %#v", last)
	}
	attached, chars, missing := reasoningPassthroughStats(chat)
	if attached != 2 || chars != len([]rune("check the logs firstconfirmed")) || missing != 0 {
		t.Fatalf("reasoning replay audit is incorrect: %d %d %d", attached, chars, missing)
	}
}

// Verifies the chat.completion -> Responses non-streaming response object
// conversion
func TestChatCompletionToResponses(t *testing.T) {
	chat := map[string]any{
		"id": "cmpl-123", "created": float64(1700000000), "model": "hy3-preview",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{
			"role": "assistant", "content": "hello", "reasoning_content": "thinking",
			"tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":"BJ"}`},
			}},
		}}},
		"usage": map[string]any{
			"prompt_tokens": float64(10), "completion_tokens": float64(5), "total_tokens": float64(15),
			"prompt_tokens_details":     map[string]any{"cached_tokens": float64(2)},
			"completion_tokens_details": map[string]any{"reasoning_tokens": float64(3)},
		},
	}
	raw, _ := json.Marshal(chat)
	out, err := chatCompletionToResponses(raw, "hy3-preview")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result["object"] != "response" || result["status"] != "completed" {
		t.Fatalf("bad envelope: %#v", result)
	}
	output := result["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("expected 3 output items (reasoning+message+function_call), got %d", len(output))
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output[0] = %#v", output[0])
	}
	if output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output[1] = %#v", output[1])
	}
	fc := output[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Fatalf("function_call item bad: %#v", fc)
	}
	usage := result["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) && usage["input_tokens"] != int64(10) {
		t.Fatalf("usage input = %#v", usage["input_tokens"])
	}
	if usage["total_tokens"] != float64(15) && usage["total_tokens"] != int64(15) {
		t.Fatalf("usage total = %#v", usage["total_tokens"])
	}
	t.Logf("responses object output items: %d", len(output))
}
