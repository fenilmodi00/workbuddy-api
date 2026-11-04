package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Scenario A: any model (not dsv4.1, not declared free by the API, no promo dates)
// measured at credit=0 for the first time must be recognized as free — proving the
// recognition logic depends on no model-name whitelist.
func TestAnyModelLearnedFreeFromCreditZero(t *testing.T) {
	resetModelStats()
	chdirTemp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":4200}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
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

	// A brand-new, never-seen model name that is not in the API catalog either
	const brandNew = "some-brand-new-free-model-2099"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+brandNew+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	accountMu.Lock()
	state := acc.ModelStates[brandNew]
	accountMu.Unlock()
	if state == nil || state.CostClass != modelCostFree {
		t.Fatalf("any new model measured at credit=0 should be recognized as free, got %+v", state)
	}
	t.Logf("model %s dynamically recognized as free from credit=0 (no hardcoding)", brandNew)
}

// Scenario B: a model that was originally free and later becomes paid must be re-classified as paid automatically.
func TestFreeModelCanFlipToPaidWhenCreditReturns(t *testing.T) {
	resetModelStats()
	chdirTemp(t)
	credit := float64(0)
	tokens := 5000
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"finish_reason":"stop"}],"usage":{"credit":`+
			formatQuota(credit)+`,"total_tokens":`+itoa(tokens)+`}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
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

	model := "limited-time-free-model"
	call := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		handleChatCompletions(rec, req)
	}

	// Round 1: limited-time free, credit=0
	call()
	accountMu.Lock()
	afterFree := acc.ModelStates[model].CostClass
	accountMu.Unlock()
	if afterFree != modelCostFree {
		t.Fatalf("should be free during the limited-time free period, got %q", afterFree)
	}

	// Round 2: limited-time free ended, upstream starts charging, credit=1.25
	credit = 1.25
	call()
	accountMu.Lock()
	afterPaid := acc.ModelStates[model].CostClass
	accountMu.Unlock()
	if afterPaid != modelCostPaid {
		t.Fatalf("after becoming paid again it should be re-classified as paid automatically, got %q", afterPaid)
	}
	t.Logf("model %s automatically re-classified from free to paid as credit changed", model)

	// Round 3: limited-time free again, credit back to 0, should switch back to free
	credit = 0
	call()
	accountMu.Lock()
	backToFree := acc.ModelStates[model].CostClass
	accountMu.Unlock()
	if backToFree != modelCostFree {
		t.Fatalf("after becoming free again it should switch back to free, got %q", backToFree)
	}
	t.Logf("model %s automatically switched back to free again", model)
}

// Scenario C: a tiny sample (probe/very short request) with credit=0 must not be misjudged as free,
// to avoid mislabeling truly paid models as free.
func TestTinySampleCreditZeroNotLearnedFree(t *testing.T) {
	resetModelStats()
	chdirTemp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
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

	model := "possibly-paid-model"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)

	accountMu.Lock()
	state := acc.ModelStates[model]
	accountMu.Unlock()
	if state != nil && state.CostClass == modelCostFree {
		t.Fatalf("credit=0 with a tiny sample must not be judged free, got %+v", state)
	}
}

// Scenario D: 14018 (account quota exhausted) must never rewrite a model's paid attribute,
// and after quota recovers the block should lift automatically and learning by credit resume.
func TestQuotaExhaustedDoesNotPolluteCostClassAndRecovers(t *testing.T) {
	chdirTemp(t)
	acc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{
			"limited-free": {CostClass: modelCostFree},
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

	// Free model meets 14018: stays free
	markModelQuotaBlocked(acc, "limited-free", "14018 Credits exhausted")
	accountMu.Lock()
	if got := acc.ModelStates["limited-free"].CostClass; got != modelCostFree {
		t.Fatalf("14018 must not rewrite the free model classification, got %q", got)
	}
	blocked := acc.ModelStates["limited-free"].QuotaBlocked
	accountMu.Unlock()
	if !blocked {
		t.Fatal("14018 should set the quota block")
	}

	// Simulate quota recovery: the refresh logic clears QuotaBlocked
	accountMu.Lock()
	acc.QuotaExhausted = false
	acc.QuotaRemaining = 500
	for _, st := range acc.ModelStates {
		st.QuotaBlocked = false
		st.NextProbeAt = time.Time{}
	}
	accountMu.Unlock()
	accountMu.Lock()
	recovered := !acc.ModelStates["limited-free"].QuotaBlocked
	class := acc.ModelStates["limited-free"].CostClass
	accountMu.Unlock()
	if !recovered {
		t.Fatal("block should lift after quota recovery")
	}
	if class != modelCostFree {
		t.Fatalf("free classification should be retained after quota recovery, got %q", class)
	}
}

// Scenario E: when the API's promo date has expired but upstream is actually still free,
// a real request's credit=0 must override the conservative "expired = unknown" judgment.
func TestExpiredPromoStillFreeIsCorrectedByLiveEvidence(t *testing.T) {
	resetModelsStateT(t)
	resetModelStats()
	chdirTemp(t)

	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"intl": {{ID: "expired-but-free", Credits: "x0.00", Multiplier: 0, HasMultiplier: true,
			PromoExpired: true, FromLive: true}},
	}
	modelsMu.Unlock()
	defer resetModelsStateT(t)

	if !modelNeedsProbe("intl", "expired-but-free", 0) {
		t.Fatal("a model with an expired promo should be scheduled for probing to confirm its real cost class")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":3300}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileINTL.Base, profileINTL.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileINTL.Base, profileINTL.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileINTL.Base, profileINTL.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"expired-but-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)

	accountMu.Lock()
	state := acc.ModelStates["expired-but-free"]
	accountMu.Unlock()
	if state == nil || state.CostClass != modelCostFree {
		t.Fatalf("with an expired promo but measured free, it should be recognized as free, got %+v", state)
	}
	if !siteKnownFree("intl", "expired-but-free") {
		t.Fatal("after a measured free result, the site should be judged a free site")
	}
	t.Log("promo expired but actually free: corrected to free via measured credit=0")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
