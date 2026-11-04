package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHourListParsing(t *testing.T) {
	for _, tc := range []struct {
		input   string
		want    []int
		wantErr bool
	}{
		{"22", []int{22}, false},
		{"22,10", []int{10, 22}, false},
		{" 3 , 7 ", []int{3, 7}, false},
		{"", nil, false},
		{"0", []int{0}, false},
		{"24", nil, true},
		{"-1", nil, true},
		{"abc", nil, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			h := hourList{22}
			err := h.Set(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if len(h) != len(tc.want) {
				t.Fatalf("got=%v want=%v", []int(h), tc.want)
			}
			for i := range tc.want {
				if h[i] != tc.want[i] {
					t.Fatalf("got=%v want=%v", []int(h), tc.want)
				}
			}
		})
	}
}

func TestNextKeepaliveTimePicksEarliestFutureHour(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 10, 2, 23, 30, 0, 0, loc)
	// 23:30 is past 22:00; the next slot is 10:00 the next day.
	got := nextKeepaliveTime(now, []int{10, 22})
	want := time.Date(2026, 10, 3, 10, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got=%s want=%s", got, want)
	}
	// Before 22:00 the same day should trigger.
	now = time.Date(2026, 10, 2, 21, 0, 0, 0, loc)
	if got := nextKeepaliveTime(now, []int{22}); !got.Equal(time.Date(2026, 10, 2, 22, 0, 0, 0, loc)) {
		t.Fatalf("should trigger same day: %s", got)
	}
	// Exactly at the configured time should defer to the next day, avoiding a
	// duplicate trigger right after startup.
	now = time.Date(2026, 10, 2, 22, 0, 0, 0, loc)
	if got := nextKeepaliveTime(now, []int{22}); !got.Equal(time.Date(2026, 10, 3, 22, 0, 0, 0, loc)) {
		t.Fatalf("exact hour should defer: %s", got)
	}
}

func TestClassifyRefreshFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       refreshFailureKind
	}{
		{"upstream session dead code", `{"code":12153,"msg":"Offline user session not found"}`, 401, refreshFailureSessionDead},
		{"invalid_grant", `{"code":12153,"msg":"invalid_grant: Token is not active"}`, 401, refreshFailureSessionDead},
		{"plain 401", `{"code":10001,"msg":"unauthorized"}`, 401, refreshFailureTransient},
		{"permission denied 403", `{"code":10002,"msg":"permission denied"}`, 403, refreshFailureTransient},
		{"server 5xx", `{"code":500,"msg":"internal error"}`, 500, refreshFailureTransient},
		{"network error no body", "", 0, refreshFailureTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRefreshFailure(tc.status, tc.body); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestJWTExpiryAndRefreshTokenExpiryFallback(t *testing.T) {
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa(int(exp.Unix())) + `,"typ":"Offline"}`))
	token := "header." + payload + ".signature"
	got, ok := jwtExpiry(token)
	if !ok || !got.Equal(exp) {
		t.Fatalf("jwtExpiry got=%v ok=%v want=%v", got, ok, exp)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b", "header." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".sig"} {
		if _, ok := jwtExpiry(bad); ok {
			t.Fatalf("should fail to parse: %q", bad)
		}
	}

	// Prefer the time recorded by the renewal API.
	recorded := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	if v, source, ok := refreshTokenExpiry(StoredTokens{RefreshToken: token, RefreshExpiresAt: recorded.Unix()}); !ok || !v.Equal(recorded) || source != "renewal API response" {
		t.Fatalf("should prefer renewal API time: %v %s %v", v, source, ok)
	}
	// Without a record, fall back to unverified token claims.
	if v, source, ok := refreshTokenExpiry(StoredTokens{RefreshToken: token}); !ok || !v.Equal(exp) || source != "token claims" {
		t.Fatalf("should fall back to token claims: %v %s %v", v, source, ok)
	}
	// With neither, it is unknown.
	if _, _, ok := refreshTokenExpiry(StoredTokens{RefreshToken: "opaque"}); ok {
		t.Fatal("unparseable token should be treated as unknown")
	}
}

func TestSanitizeUpstreamTextHidesIdentifiers(t *testing.T) {
	// Synthetic identifier only; never copy identifiers from live accounts.
	raw := `upstream 401: {"msg":"userID:00000000-0000-4000-8000-000000000001","token":"Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"}`
	got := sanitizeUpstreamText(raw)
	for _, secret := range []string{"00000000-0000-4000-8000-000000000001", "eyJhbGciOiJSUzI1NiJ9"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sensitive fragment not redacted: %s", got)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("should contain the redaction marker: %s", got)
	}
}

// refreshTestServer starts a fake renewal API returning a configurable body.
func refreshTestServer(t *testing.T, handler func(call int) (int, string)) *httptest.Server {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The read-only check before overwrite hits the same site; returning
		// "available" keeps the test focused on the refresh logic itself.
		if r.URL.Path == "/billing/meter/get-user-resource-summary" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[]}}`))
			return
		}
		calls++
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("X-Refresh-Token") == "" {
			t.Error("missing X-Refresh-Token")
		}
		if got := r.Header.Get("X-Auth-Refresh-Source"); got != "workbuddy" {
			t.Errorf("X-Auth-Refresh-Source=%q", got)
		}
		status, body := handler(calls)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	oldBase, oldOrigin, oldClient := profileCN.Base, profileCN.Origin, cfg.HttpClient
	profileCN.Base, profileCN.Origin, cfg.HttpClient = server.URL, server.URL, server.Client()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin, cfg.HttpClient = oldBase, oldOrigin, oldClient
	})
	return server
}

func newTestAccount(t *testing.T, access, refresh string) *Account {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-test.json")
	sa := &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}
	if err := saveAuthTo(path, sa); err != nil {
		t.Fatal(err)
	}
	return &Account{Path: path, Auth: sa, Edition: "cn"}
}

func TestRefreshPersistsRenewalMetadata(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 200, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600,"refreshExpiresIn":7200,"domain":"example.test"}}`
	})
	acc := newTestAccount(t, "old-access", "old-refresh")
	before := time.Now()
	if err := refreshAccountToken(acc, "unit test"); err != nil {
		t.Fatal(err)
	}
	got := acc.Auth.Auth
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.Domain != "example.test" {
		t.Fatalf("token not updated: %+v", got)
	}
	if got.ExpiresAt < before.Add(3500*time.Second).Unix() {
		t.Fatalf("access token expiry not extended: %d", got.ExpiresAt)
	}
	if got.RefreshExpiresAt < before.Add(7100*time.Second).Unix() {
		t.Fatalf("refresh token expiry not recorded: %d", got.RefreshExpiresAt)
	}
	if got.LastRefreshTime < before.Unix() {
		t.Fatalf("last renewal time not recorded: %d", got.LastRefreshTime)
	}
	if acc.RefreshFailCount != 0 || acc.LastRefreshError != "" {
		t.Fatal("failure count should be cleared after a successful renewal")
	}

	// Metadata must be persisted; still visible after a restart.
	raw, err := os.ReadFile(acc.Path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk StoredAuth
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Auth.LastRefreshTime == 0 || onDisk.Auth.RefreshExpiresAt == 0 || onDisk.Auth.AccessToken != "new-access" {
		t.Fatalf("metadata not written back to disk: %+v", onDisk.Auth)
	}
}

func TestRefreshPreservesExpiryWhenUpstreamOmitsExpiresIn(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 200, `{"code":0,"data":{"accessToken":"second-access"}}`
	})
	acc := newTestAccount(t, "first-access", "refresh-token")
	originalExpires := acc.Auth.Auth.ExpiresAt
	if err := refreshAccountToken(acc, "unit test"); err != nil {
		t.Fatal(err)
	}
	if acc.Auth.Auth.ExpiresAt != originalExpires {
		t.Fatalf("should keep the old expiry when the API omits expiresIn: %d -> %d", originalExpires, acc.Auth.Auth.ExpiresAt)
	}
	if acc.Auth.Auth.AccessToken != "second-access" {
		t.Fatal("access token should still be updated")
	}
	if acc.Auth.Auth.RefreshToken != "refresh-token" {
		t.Fatal("old refresh token should be kept when the API does not send a new one")
	}
}

func TestRefreshDisablesOnlyAfterConsecutiveFailures(t *testing.T) {
	chdirTemp(t)
	// A plain 401 (without session-dead markers) is a transient failure; credentials must not be deleted on one failure.
	refreshTestServer(t, func(int) (int, string) {
		return 401, `{"code":10001,"msg":"unauthorized"}`
	})
	acc := newTestAccount(t, "access", "refresh")
	for i := 1; i < refreshDisableThreshold; i++ {
		if err := refreshAccountToken(acc, "unit test"); err == nil {
			t.Fatal("should return an error")
		}
		if acc.Disabled {
			t.Fatalf("disabling the account on failure number %d is too aggressive", i)
		}
		if acc.RefreshFailCount != i {
			t.Fatalf("failure count=%d want=%d", acc.RefreshFailCount, i)
		}
		if _, err := os.Stat(acc.Path); err != nil {
			t.Fatalf("credential file should not be deleted: %v", err)
		}
	}
	if err := refreshAccountToken(acc, "unit test"); err == nil {
		t.Fatal("should return an error")
	}
	if !acc.Disabled {
		t.Fatalf("login state should be declared dead after %d consecutive failures", refreshDisableThreshold)
	}
}

func TestSessionDeadSignalKeepsCredentialsUntilThreshold(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 401, `{"code":12153,"msg":"invalid_grant: Token is not active"}`
	})
	acc := newTestAccount(t, "access", "refresh")
	if err := refreshAccountToken(acc, "unit test"); err == nil {
		t.Fatal("should return an error")
	}
	if acc.Disabled {
		t.Fatal("the first session-dead failure only warns; credentials must not be deleted immediately")
	}
	if _, err := os.Stat(acc.Path); err != nil {
		t.Fatalf("credential file should be kept: %v", err)
	}
	if !strings.Contains(acc.LastRefreshError, "session dead") {
		t.Fatalf("should record the verdict kind: %s", acc.LastRefreshError)
	}
	for i := 1; i < refreshDisableThreshold; i++ {
		_ = refreshAccountToken(acc, "unit test")
	}
	if !acc.Disabled {
		t.Fatalf("scheduling should stop after %d consecutive session-dead failures", refreshDisableThreshold)
	}
}

func TestRefreshSuccessResetsFailureCounter(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(call int) (int, string) {
		if call == 1 {
			return 500, `{"code":500,"msg":"temporary"}`
		}
		return 200, `{"code":0,"data":{"accessToken":"ok-access","expiresIn":3600}}`
	})
	acc := newTestAccount(t, "access", "refresh")
	if err := refreshAccountToken(acc, "unit test"); err == nil {
		t.Fatal("first call should fail")
	}
	if acc.RefreshFailCount != 1 {
		t.Fatalf("failure count=%d", acc.RefreshFailCount)
	}
	if err := refreshAccountToken(acc, "unit test"); err != nil {
		t.Fatal(err)
	}
	if acc.RefreshFailCount != 0 || acc.LastRefreshError != "" {
		t.Fatalf("failure count should be cleared after success: %d %q", acc.RefreshFailCount, acc.LastRefreshError)
	}
}

func TestKeepaliveSkipsUnusableAccountsAndRefreshesValid(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 200, `{"code":0,"data":{"accessToken":"keepalive-access","expiresIn":3600}}`
	})
	valid := newTestAccount(t, "access", "refresh")
	noRefresh := &Account{Path: "no-refresh.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x"}}}
	disabled := &Account{Path: "disabled.json", Disabled: true, Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", RefreshToken: "y"}}}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{valid, noRefresh, disabled}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})

	runKeepaliveOnce("unit test", false)

	if valid.Auth.Auth.AccessToken != "keepalive-access" {
		t.Fatal("available account should be proactively renewed")
	}
	if disabled.Disabled != true || disabled.Auth.Auth.AccessToken != "x" {
		t.Fatal("dead account should not be refreshed")
	}
	if noRefresh.Auth.Auth.AccessToken != "x" {
		t.Fatal("account missing a refresh token should not be refreshed")
	}
}

func TestKeepaliveOverdueDetection(t *testing.T) {
	now := time.Now()
	accountMu.Lock()
	defer accountMu.Unlock()
	for _, tc := range []struct {
		name string
		acc  *Account
		want bool
	}{
		{"never renewed", &Account{Auth: &StoredAuth{}}, true},
		{"just renewed", &Account{Auth: &StoredAuth{Auth: StoredTokens{LastRefreshTime: now.Unix()}}}, false},
		{"over threshold", &Account{Auth: &StoredAuth{Auth: StoredTokens{LastRefreshTime: now.Add(-keepaliveOverdueAfter - time.Minute).Unix()}}}, true},
		{"no credentials", &Account{}, false},
		{"nil account", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isKeepaliveOverdue(tc.acc, now); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestRenewalStatusTextShowsExpiryAndWarnings(t *testing.T) {
	accountMu.Lock()
	defer accountMu.Unlock()
	soon := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	acc := &Account{
		Auth: &StoredAuth{Auth: StoredTokens{
			RefreshToken:     "opaque",
			RefreshExpiresAt: soon.Unix(),
			LastRefreshTime:  time.Now().Add(-time.Hour).Unix(),
		}},
	}
	text := formatRenewalStatus(acc)
	for _, want := range []string{"Refresh token", "expiring soon", "Last renewal"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	// Unknown expiry must not be shown as "valid".
	unknown := formatRenewalStatus(&Account{Auth: &StoredAuth{Auth: StoredTokens{RefreshToken: "opaque"}}})
	if !strings.Contains(unknown, "expiry unknown") || strings.Contains(unknown, "valid") {
		t.Fatalf("unknown expiry mishandled: %s", unknown)
	}
	if got := formatRenewalStatus(nil); got != "" {
		t.Fatalf("nil account should return an empty string: %q", got)
	}
}

func TestAccountTableWarnsOnExpiringRefreshToken(t *testing.T) {
	soon := time.Now().Add(48 * time.Hour).Unix()
	expired := time.Now().Add(-time.Hour).Unix()
	out := renderAccountTable([]accountSnapshot{
		{Path: "soon.json", QuotaKnown: true, PlanLabel: "Free", RefreshExpiresAt: soon},
		{Path: "dead.json", QuotaKnown: true, PlanLabel: "Free", RefreshExpiresAt: expired},
		{Path: "failing.json", QuotaKnown: true, PlanLabel: "Free", RefreshFailCount: 2},
	})
	for _, want := range []string{"soon.json Refresh token", "re-login recommended as soon as possible", "dead.json Refresh token expired", "failing.json renewal failed 2 consecutive times"} {
		if !strings.Contains(out, want) {
			t.Fatalf("monitor table missing warning %q:\n%s", want, out)
		}
	}
	// A long-valid account should not produce noise.
	clean := renderAccountTable([]accountSnapshot{{Path: "ok.json", QuotaKnown: true, PlanLabel: "Free", RefreshExpiresAt: time.Now().Add(200 * 24 * time.Hour).Unix()}})
	if strings.Contains(clean, "re-login") || strings.Contains(clean, "renewal failed") {
		t.Fatalf("long-valid account should not warn:\n%s", clean)
	}
}
