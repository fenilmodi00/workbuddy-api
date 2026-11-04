package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withCatalogModels swaps in a temporary model catalog and restores it after the test.
func withCatalogModels(t *testing.T, models map[string][]catalogModel) {
	t.Helper()
	modelsMu.Lock()
	oldModels, oldSource := catalogModels, dynamicSource
	catalogModels, dynamicSource = models, "test"
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		catalogModels, dynamicSource = oldModels, oldSource
		modelsMu.Unlock()
	})
}

// withFakeUpstream runs the real outbound path against a fake upstream with a single cn account; captured receives the outbound request body.
func withFakeUpstream(t *testing.T, captured *[]byte) {
	t.Helper()
	chdirTemp(t)
	resetModelStats()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			body, _ := io.ReadAll(r.Body)
			*captured = body
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{
		Path:       "max-tokens-test.json",
		Auth:       &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
		QuotaKnown: true, QuotaRemaining: 100,
	}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
}

func maxTokensCatalog() map[string][]catalogModel {
	return map[string][]catalogModel{
		"cn": {
			{ID: "budget-model", MaxInputTokens: 1000000, MaxOutputTokens: 128000},
			{ID: "small-budget-model", MaxInputTokens: 192000, MaxOutputTokens: 64000},
		},
	}
}

func decodeUpstreamBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("failed to decode outbound request: %v\n%s", err, body)
	}
	return out
}

func TestEnsureUpstreamMaxTokensInjectsCatalogLimit(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	obj := map[string]any{"model": "budget-model"}
	ensureUpstreamMaxTokens(obj, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), 7, "test-trace", "budget-model")
	if got := obj["max_tokens"]; got != 128000 {
		t.Fatalf("output limit not injected per catalog: %#v", got)
	}
}

func TestEnsureUpstreamMaxTokensUsesPerModelLimit(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	obj := map[string]any{"model": "small-budget-model"}
	ensureUpstreamMaxTokens(obj, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), 8, "test-trace", "small-budget-model")
	if got := obj["max_tokens"]; got != 64000 {
		t.Fatalf("small-window model should use its own output limit: %#v", got)
	}
}

func TestEnsureUpstreamMaxTokensKeepsClientValue(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	obj := map[string]any{"model": "budget-model", "max_tokens": 2000}
	ensureUpstreamMaxTokens(obj, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), 9, "test-trace", "budget-model")
	if got := obj["max_tokens"]; got != 2000 {
		t.Fatalf("must not override the client's explicit value: %#v", got)
	}
}

func TestEnsureUpstreamMaxTokensRespectsMaxCompletionTokens(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	obj := map[string]any{"model": "budget-model", "max_completion_tokens": 4096}
	ensureUpstreamMaxTokens(obj, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), 10, "test-trace", "budget-model")
	if _, exists := obj["max_tokens"]; exists {
		t.Fatalf("max_tokens must not be injected when the client declared a budget via max_completion_tokens: %#v", obj)
	}
}

func TestEnsureUpstreamMaxTokensTreatsNullAsAbsent(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	obj := map[string]any{"model": "budget-model", "max_tokens": nil}
	ensureUpstreamMaxTokens(obj, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), 11, "test-trace", "budget-model")
	if got := obj["max_tokens"]; got != 128000 {
		t.Fatalf("max_tokens=null should be treated as undeclared: %#v", got)
	}
}

func TestEnsureUpstreamMaxTokensSkipsUnknownModel(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	obj := map[string]any{"model": "not-in-catalog"}
	ensureUpstreamMaxTokens(obj, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), 12, "test-trace", "not-in-catalog")
	if _, exists := obj["max_tokens"]; exists {
		t.Fatalf("output limit must not be guessed for models missing from the catalog: %#v", obj)
	}
}

func TestChatEntryInjectsMaxTokensWhenClientOmitsBudget(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	var captured []byte
	withFakeUpstream(t, &captured)
	body := `{"model":"budget-model","stream":true,"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeUpstreamBody(t, captured)["max_tokens"]; got != float64(128000) {
		t.Fatalf("Chat entry did not inject the output budget: %#v\n%s", got, captured)
	}
}

func TestChatEntryKeepsClientMaxTokens(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	var captured []byte
	withFakeUpstream(t, &captured)
	body := `{"model":"budget-model","stream":true,"max_tokens":512,"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeUpstreamBody(t, captured)["max_tokens"]; got != float64(512) {
		t.Fatalf("Chat entry overrode the client's output budget: %#v", got)
	}
}

func TestResponsesEntryInjectsMaxTokensWhenClientOmitsBudget(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	var captured []byte
	withFakeUpstream(t, &captured)
	body := `{"model":"budget-model","stream":true,"input":"hi"}`
	rec := httptest.NewRecorder()
	handleResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeUpstreamBody(t, captured)["max_tokens"]; got != float64(128000) {
		t.Fatalf("Responses entry did not inject the output budget: %#v\n%s", got, captured)
	}
}

func TestResponsesEntryKeepsClientMaxOutputTokens(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	var captured []byte
	withFakeUpstream(t, &captured)
	body := `{"model":"budget-model","stream":true,"max_output_tokens":2048,"input":"hi"}`
	rec := httptest.NewRecorder()
	handleResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeUpstreamBody(t, captured)["max_tokens"]; got != float64(2048) {
		t.Fatalf("Responses entry overrode the client's output budget: %#v", got)
	}
}

func TestMessagesEntryInjectsMaxTokensWhenClientOmitsBudget(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	var captured []byte
	withFakeUpstream(t, &captured)
	body := `{"model":"budget-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	handleMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeUpstreamBody(t, captured)["max_tokens"]; got != float64(128000) {
		t.Fatalf("Anthropic entry did not inject the output budget: %#v\n%s", got, captured)
	}
}

func TestMessagesEntryKeepsClientMaxTokens(t *testing.T) {
	withCatalogModels(t, maxTokensCatalog())
	var captured []byte
	withFakeUpstream(t, &captured)
	body := `{"model":"budget-model","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	handleMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeUpstreamBody(t, captured)["max_tokens"]; got != float64(1024) {
		t.Fatalf("Anthropic entry overrode the client's output budget: %#v", got)
	}
}
