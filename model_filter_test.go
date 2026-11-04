package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func resetModelFilter() {
	setModelFilter(nil, nil)
}

// Blocklist: a hit disables; a miss passes; case and whitespace insensitive.
func TestModelBlocklist(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter([]string{"  DeepSeek-V4.1-Flash ", "gpt-5.6-sol"}, nil)

	for _, model := range []string{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", "gpt-5.6-sol"} {
		disabled, reason := modelDisabled(model)
		if !disabled {
			t.Fatalf("model %s should be disabled by the blocklist", model)
		}
		if !strings.Contains(reason, "disabled by the gateway") {
			t.Fatalf("disable reason should mention the gateway, got %q", reason)
		}
	}
	if disabled, _ := modelDisabled("hy3"); disabled {
		t.Fatal("models not on the blocklist should not be disabled")
	}
}

// Allowlist: when non-empty, only models on the list pass.
func TestModelAllowlist(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter(nil, []string{"deepseek-v4.1-flash"})

	if disabled, _ := modelDisabled("deepseek-v4.1-flash"); disabled {
		t.Fatal("models on the allowlist should pass")
	}
	disabled, reason := modelDisabled("gpt-5.6-sol")
	if !disabled || !strings.Contains(reason, "not in allowlist") {
		t.Fatalf("models outside the allowlist should be disabled with a reason, disabled=%v reason=%q", disabled, reason)
	}
}

// No restriction when neither blocklist nor allowlist is configured.
func TestModelFilterDisabledByDefault(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	if modelFilterConfigured() {
		t.Fatal("no list should be enabled by default")
	}
	if disabled, _ := modelDisabled("any-model"); disabled {
		t.Fatal("no model should be disabled when no list is configured")
	}
}

// The models section of config.json should load the allowlist; the legacy sample's empty blocklist stays compatible.
func TestRuntimeConfigLoadsModelFilter(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()

	dir := t.TempDir()
	path := dir + "/config.json"
	content := `{"models":{"blocklist":[],"allowlist":["allowed-model"]}}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if !modelFilterConfigured() {
		t.Fatal("config should enable the model filter")
	}
	if disabled, _ := modelDisabled("blocked-model"); !disabled {
		t.Fatal("blocklisted model should be disabled")
	}
	if disabled, _ := modelDisabled("other-model"); !disabled {
		t.Fatal("with a non-empty allowlist, models outside it should be disabled")
	}
	if disabled, _ := modelDisabled("allowed-model"); disabled {
		t.Fatal("models on the allowlist should pass")
	}
}

// Disabled models must be hidden from the /v1/models list.
func TestMergedModelIDsHidesBlockedModels(t *testing.T) {
	resetModelsStateT(t)
	resetModelFilter()
	defer resetModelFilter()
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {
			{ID: "keep-me", FromLive: true},
			{ID: "hide-me", FromLive: true},
		},
	}
	modelsMu.Unlock()
	defer resetModelsStateT(t)

	setModelFilter([]string{"hide-me"}, nil)
	ids, _ := mergedModelIDs()
	for _, id := range ids {
		if id == "hide-me" {
			t.Fatalf("disabled model must not appear in the model list: %v", ids)
		}
	}
	found := false
	for _, id := range ids {
		if id == "keep-me" {
			found = true
		}
	}
	if !found {
		t.Fatalf("normal models should be kept: %v", ids)
	}
}

// Disabled models must be hidden from the monitor stats side table, even if requested historically.
func TestModelStatSnapshotsHideBlockedModels(t *testing.T) {
	resetModelStats()
	resetModelFilter()
	defer resetModelFilter()
	recordModelRequest("hidden-by-config")
	recordModelSuccess("hidden-by-config")

	setModelFilter([]string{"hidden-by-config"}, nil)
	rows := buildModelStatSnapshots(time.Now(), nil)
	for _, row := range rows {
		if row.ID == "hidden-by-config" {
			t.Fatalf("disabled model must not appear in the stats side table: %+v", row)
		}
	}
}

// End to end: requesting a disabled model returns 403 + error message, and upstream is never called.
func TestBlockedModelRequestReturnsChineseError(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter([]string{"banned-model"}, nil)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	oldBase := profileCN.Base
	oldClient := cfg.HttpClient
	profileCN.Base = upstream.URL
	cfg.HttpClient = upstream.Client()
	defer func() {
		profileCN.Base = oldBase
		cfg.HttpClient = oldClient
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "a.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
	}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"banned-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled model should return 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Fatal("upstream must not be called for a disabled model")
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	errObj, _ := resp["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "disabled by the gateway") {
		t.Fatalf("error message should mention the gateway, got %q", msg)
	}
	if errObj["type"] != "model_disabled" {
		t.Fatalf("error type should be model_disabled, got %v", errObj["type"])
	}
}

// End to end: the /v1/responses endpoint blocks disabled models too.
func TestBlockedModelResponsesEndpoint(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter(nil, []string{"only-this-model"})

	body := `{"model":"not-in-allowlist","stream":true,"input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleResponses)).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("models outside the allowlist should return 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disabled by the gateway") {
		t.Fatalf("should return the gateway disable message: %s", rec.Body.String())
	}
}
