package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolatePolicies(t *testing.T) {
	t.Helper()
	isolatedGatewayConfig(t)
	resetModelsStateT(t)
	resetModelStats()
	oldHeader, oldIdle, oldRetries := upstreamSettings()
	upstreamConfigMu.RLock()
	oldKeepalive, oldFirstByteRetry := streamKeepalive, enableFirstByteRetry
	upstreamConfigMu.RUnlock()
	oldAccounts := accounts
	oldRetention := logRetentionDays.Load()
	runtimeConfigMu.Lock()
	oldHash, oldHasHash := runtimeAppliedHash, runtimeHasAppliedHash
	runtimeHasAppliedHash = false
	runtimeConfigMu.Unlock()
	t.Cleanup(func() {
		logRetentionDays.Store(oldRetention)
		runtimeConfigMu.Lock()
		runtimeAppliedHash, runtimeHasAppliedHash = oldHash, oldHasHash
		runtimeConfigMu.Unlock()
		_ = switchDebugLogging(false)
		setModelFilter(nil, nil)
		_ = setModelAccountFilter(nil)
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
		upstreamConfigMu.Lock()
		upstreamHeaderTimeout, upstreamIdleTimeout, upstreamTransientRetries = oldHeader, oldIdle, oldRetries
		streamKeepalive, enableFirstByteRetry = oldKeepalive, oldFirstByteRetry
		upstreamConfigMu.Unlock()
	})
}

func writeRuntimeFixture(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(runtimeConfigFile, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeReloadLastGoodAndDefaults(t *testing.T) {
	isolatePolicies(t)
	watcher := runtimeConfigWatcher{}
	good := `{"gateway":{"adminKey":"admin","apiKeyEnabled":true,"apiKey":"key"},"systemPrompt":{"force":"synthetic"},"upstream":{"headerTimeoutSeconds":41,"idleTimeoutSeconds":17,"transientRetries":0},"models":{"blocklist":["off"],"accounts":{"all":{"blocklist":["b.json"]}}}}`
	writeRuntimeFixture(t, good)
	if !watcher.reload() {
		t.Fatal("valid config must load")
	}
	if watcher.reload() {
		t.Fatal("identical content is not applied twice")
	}
	header, idle, retries := upstreamSettings()
	if header != 41*time.Second || idle != 17*time.Second || retries != 0 {
		t.Fatal("upstream config did not take effect")
	}
	for _, bad := range []string{
		``, `null`, `[]`, `{`, `{} {}`, `{"unknown":true}`,
		`{"gateway":{"adminKey":"changed","apiKeyEnabled":true},"models":{}}`,
		`{"gateway":{"adminKey":"changed"},"models":{"accounts":{"all":{"blocklist":["../b.json"]}}}}`,
		`{"models":{"allowlist":["one"],"blocklist":["two"]}}`,
		`{"upstream":{"headerTimeoutSeconds":9223372036854775807}}`,
	} {
		writeRuntimeFixture(t, bad)
		if watcher.reload() {
			t.Fatalf("invalid config was accepted: %q", bad)
		}
		if currentAdminKey() != "admin" || currentAPIKey() != "key" || configuredSystemPrompts().force != "synthetic" {
			t.Fatal("invalid config partially overwrote the valid config")
		}
		if disabled, _ := modelDisabled("off"); !disabled {
			t.Fatal("invalid config cleared the blocklist")
		}
	}
	if err := os.Remove(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if watcher.reload() || currentAdminKey() != "admin" {
		t.Fatal("brief removal must not clear the effective config")
	}
	writeRuntimeFixture(t, `{}`)
	if !watcher.reload() {
		t.Fatal("after fixing, config must be applied again")
	}
	if currentAPIKey() != "" || currentAdminKey() != "" || configuredSystemPrompts().force != "" {
		t.Fatal("omitting config must not leave stale values")
	}
	header, idle, retries = upstreamSettings()
	if header != upstreamHeaderTimeoutDefault || idle != upstreamIdleTimeoutDefault || retries != upstreamTransientRetriesDefault {
		t.Fatal("omitting timeouts must not leave stale values")
	}
	if ok, _ := modelAccountAllowed("new", &Account{Path: "b.json"}, nil); !ok {
		t.Fatal("omitting account rules did not restore")
	}
}

func TestRuntimeReloadDetectsRevertAfterWebWrite(t *testing.T) {
	isolatePolicies(t)
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}}}
	original := `{"gateway":{"adminKey":"admin"},"models":{"blocklist":[]}}`
	writeRuntimeFixture(t, original)
	watcher := runtimeConfigWatcher{}
	if !watcher.reload() {
		t.Fatal("initial load failed")
	}
	if err := savePolicyToggle("model", "one", false, "revert"); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixture(t, original) // revert the web UI change right away, before the scan.
	if !watcher.reload() {
		t.Fatal("reverting to the old file content was wrongly considered unchanged")
	}
	if disabled, _ := modelDisabled("one"); disabled {
		t.Fatal("stale web UI disable rule remained after old content was restored")
	}
	if err := saveAPIKeySettings(true, "api", "revert"); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixture(t, original)
	if !watcher.reload() || currentAPIKey() != "" {
		t.Fatal("auth settings revert was not detected by the scan")
	}
}

func TestOlderConfigEmptyAccountRulesRemainCompatible(t *testing.T) {
	isolatePolicies(t)
	for _, text := range []string{
		`{}`, `{"debug":{"enabled":false}}`,
		`{"models":{"blocklist":[],"allowlist":[],"accounts":{}}}`,
		`{"models":{"accounts":null}}`,
		`{"models":{"accounts":{"all":{}}}}`,
		`{"models":{"accounts":{"all":{"allowlist":null,"blocklist":null}}}}`,
	} {
		writeRuntimeFixture(t, text)
		if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
			t.Fatalf("older/empty config must not be readable: %s: %v", text, err)
		}
		if ok, _ := modelAccountAllowed("any", &Account{Path: "a.json"}, nil); !ok {
			t.Fatal("old config disabled the default account")
		}
		if disabled, _ := modelDisabled("any"); disabled {
			t.Fatal("old config disabled the default model")
		}
	}
}

func TestGlobalWhitelistToggleAndProbeSkips(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"models":{"accounts":{" ALL ":{"allowlist":["a.json"]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	accounts = []*Account{{Path: "a.json", Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic"}}}, {Path: "b.json", Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic"}}}}
	if err := savePolicyToggle("account", "a.json", false, "global-white"); err != nil {
		t.Fatal(err)
	}
	for _, acc := range accounts {
		if ok, _ := modelAccountAllowed("m", acc, accounts); ok {
			t.Fatal("disabling the last allowlisted account must not unblock other accounts")
		}
	}
	if acc := pickProbeAccountForModel("cn", "m"); acc != nil {
		t.Fatal("background probing must not pick a globally disabled account")
	}
	result := probeAccountModel(context.Background(), accounts[0], "m")
	if result.Status != "skipped" {
		t.Fatalf("explicit probe must not send an upstream request: %+v", result)
	}
	if err := savePolicyToggle("account", "b.json", true, "global-white"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := modelAccountAllowed("new-model", accounts[1], accounts); !ok {
		t.Fatal("failed to re-enable the account outside the allowlist")
	}
	data, _ := os.ReadFile(runtimeConfigFile)
	if strings.Contains(string(data), `" ALL "`) {
		t.Fatal("after saving, reserved key all was not normalized")
	}
	global := decodeObject(t, data)["models"].(map[string]any)["accounts"].(map[string]any)["all"].(map[string]any)
	if _, exists := global["allowlist"]; exists {
		t.Fatal("web UI account operations must delete the all.allowlist field")
	}
	setModelFilter([]string{"off"}, nil)
	result = probeAccountModel(context.Background(), accounts[1], "off")
	if result.Status != "skipped" {
		t.Fatal("disabled model must not be explicitly probed")
	}
}

func TestAccountToggleConvertsGlobalWhitelistPreservingOtherStates(t *testing.T) {
	for _, tc := range []struct {
		name, global, target string
		enable               bool
		blocked              []string
	}{
		{"enable account outside allowlist", `{"allowlist":["a.json"]}`, "b.json", true, []string{"c.json"}},
		{"disable last allowlisted account", `{"allowlist":["a.json"]}`, "a.json", false, []string{"a.json", "b.json", "c.json"}},
		{"keep original blocklist and unloaded accounts", `{"allowlist":["a.json","b.json"],"blocklist":["b.json","offline.json"]}`, "a.json", false, []string{"a.json", "b.json", "c.json", "offline.json"}},
		{"enable allowlisted account rejected by blocklist", `{"allowlist":["a.json","b.json"],"blocklist":["b.json"]}`, "b.json", true, []string{"c.json"}},
		{"empty allowlist does not restrict other accounts", `{"allowlist":[],"blocklist":["b.json"]}`, "a.json", false, []string{"a.json", "b.json"}},
		{"null allowlist deletes field", `{"allowlist":null,"blocklist":["b.json"]}`, "b.json", true, []string{}},
		{"null global rule", `null`, "b.json", false, []string{"b.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatePolicies(t)
			content := `{"gateway":{"adminKey":"admin"},"systemPrompt":{"force":"keep"},"models":{"allowlist":["m"],"accounts":{"m":{"allowlist":["a.json"],"blocklist":null}," ALL ":` + tc.global + `}}}`
			writeRuntimeFixture(t, content)
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatal(err)
			}
			accounts = []*Account{{Path: "a.json"}, {Path: "b.json"}, {Path: "c.json"}}
			handler := newWebUIHandler()
			body, _ := json.Marshal(map[string]any{"target": tc.target, "enabled": tc.enable})
			response := adminRequest(handler, http.MethodPost, "/admin/api/account-toggle", "admin", string(body))
			if response.Code != http.StatusOK {
				t.Fatalf("toggle save failed: %d %s", response.Code, response.Body.String())
			}
			raw, _ := os.ReadFile(runtimeConfigFile)
			doc := decodeObject(t, raw)
			models := doc["models"].(map[string]any)
			rules := models["accounts"].(map[string]any)
			global := rules["all"].(map[string]any)
			if _, exists := global["allowlist"]; exists || len(global) != 1 {
				t.Fatalf("all must write only the blocklist: %+v", global)
			}
			if _, exists := rules[" ALL "]; exists {
				t.Fatal("old ' ALL ' spelling was not deleted")
			}
			var typed runtimeFileConfig
			_ = json.Unmarshal(raw, &typed)
			gotBlocks := typed.Models.Accounts["all"].Blocklist
			got, _ := json.Marshal(gotBlocks)
			want, _ := json.Marshal(tc.blocked)
			if string(got) != string(want) {
				t.Fatalf("blocklist=%s, want=%s", got, want)
			}
			for _, acc := range accounts {
				blocked := false
				for _, name := range tc.blocked {
					blocked = blocked || acc.Path == name
				}
				if allowed, _ := modelAccountAllowed("unconfigured", acc, accounts); allowed == blocked {
					t.Fatalf("account %s has wrong global state after conversion", acc.Path)
				}
			}
			original := decodeObject(t, []byte(content))["models"].(map[string]any)["accounts"].(map[string]any)["m"]
			before, _ := json.Marshal(original)
			after, _ := json.Marshal(rules["m"])
			if string(before) != string(after) || configuredSystemPrompts().force != "keep" || currentModelPolicyMode() != "allowlist" {
				t.Fatal("converting all changed the model list, model-specific account rules, or system prompt")
			}
			if ok, _ := modelAccountAllowed("m", accounts[1], accounts); ok {
				t.Fatal("global enable bypasses the model-specific allowlist")
			}
			fresh := &Account{Path: "new.json"}
			if ok, _ := modelAccountAllowed("unconfigured", fresh, []*Account{fresh}); !ok {
				t.Fatal("after converting to blocklist, a new account should be enabled by default")
			}
			files, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
			logData, _ := os.ReadFile(files[0])
			if !strings.Contains(string(logData), "[Account toggle rules] traceId=gateway-config-test") {
				t.Fatal("conversion did not persist with the request trace ID")
			}
		})
	}
}

func TestAccountWhitelistConversionKeepsDuplicateNamesBlocked(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"models":{"accounts":{"all":{"allowlist":["a.json","same.json"]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	accounts = []*Account{{Path: "a.json"}, {Path: "first/same.json"}, {Path: "second/same.json"}}
	if err := savePolicyToggle("account", "a.json", true, "duplicates"); err != nil {
		t.Fatal(err)
	}
	for _, acc := range accounts[1:] {
		if allowed, _ := modelAccountAllowed("m", acc, accounts); allowed {
			t.Fatal("conversion unblocked a previously rejected duplicate-name account")
		}
	}
	before, _ := os.ReadFile(runtimeConfigFile)
	if err := savePolicyToggle("account", "first/same.json", true, "duplicates"); err == nil {
		t.Fatal("duplicate-name account must not be enabled individually via the web UI")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if string(before) != string(after) {
		t.Fatal("rejecting a duplicate target must not write the file")
	}
}

func TestRuntimeLoopHotKeysDebugAndPorts(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"apiPort":9317,"webPort":9316,"adminKey":"first"}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtimeConfigLoop(ctx) }()
	defer func() { cancel(); <-done }()
	writeRuntimeFixture(t, `{"gateway":{"apiPort":9417,"webPort":9416,"adminKey":"second","apiKeyEnabled":true,"apiKey":"api"},"debug":{"enabled":true}}`)
	deadline := time.Now().Add(4 * time.Second)
	for currentAdminKey() != "second" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if currentAdminKey() != "second" || currentAPIKey() != "api" || !debugLoggingEnabled() {
		t.Fatal("actual loop did not hot-reload")
	}
	if cfg.Port != 9317 || cfg.WebPort != 9316 {
		t.Fatal("hot reload must not change the actual listen port display")
	}
	r := httptest.NewRequest("GET", "/", nil)
	debugEvent(r, "debug", "hot_reload_test", nil)
	files, _ := filepath.Glob(filepath.Join(logDir, "debug-*.jsonl"))
	if len(files) != 1 {
		t.Fatal("hot-enabled debug was not persisted")
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "hot_reload_test") {
		t.Fatal("hot debug event was not written")
	}
}

func TestGlobalAccountRuleCannotBeOverridden(t *testing.T) {
	isolatePolicies(t)
	a, b := &Account{Path: "a.json", Auth: &StoredAuth{}}, &Account{Path: "b.json", Auth: &StoredAuth{}}
	accounts = []*Account{a, b}
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{
		"all": {Blocklist: []string{"a.json"}},
		"m":   {Allowlist: []string{"a.json"}},
	}); err != nil {
		t.Fatal(err)
	}
	if ok, reason := modelAccountAllowed("m", a, accounts); ok || !strings.Contains(reason, "global") {
		t.Fatal("model allowlist bypasses global disable")
	}
	if acc, _, err := nextAccountForModel("other", nil); err != nil || acc != b {
		t.Fatalf("unconfigured model must follow global rules: %v", err)
	}
	if _, _, err := nextAccountForModel("m", nil); err == nil {
		t.Fatal("must reject an undispatchable model")
	}
	if rows := buildAllModelStatSnapshots(time.Now(), accounts); len(rows) != 1 || rows[0].ID != "m" || rows[0].AvailableAccounts != 0 {
		t.Fatalf("all must not become a fake model: %+v", rows)
	}
}

func TestWebTogglePreservesConfigAndConvertsAllowlist(t *testing.T) {
	isolatePolicies(t)
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}, {ID: "two"}, {ID: "three"}}}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"systemPrompt":{"force":"synthetic preserve"},"models":{"allowlist":["one"],"accounts":{"all":{"blocklist":[]},"two":{"allowlist":["a.json"]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	a, b := &Account{Path: "a.json", Auth: &StoredAuth{}}, &Account{Path: "b.json", Auth: &StoredAuth{}}
	accounts = []*Account{a, b}
	handler := newWebUIHandler()
	w := adminRequest(handler, "POST", "/admin/api/account-toggle", "admin", `{"target":"a.json","enabled":false}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if a.Disabled || a.Auth == nil {
		t.Fatal("toggle must not modify auth-invalid state or delete credentials")
	}
	for _, model := range []string{"one", "two", "unknown"} {
		if ok, _ := modelAccountAllowed(model, a, accounts); ok {
			t.Fatalf("global disable was not applied to %s", model)
		}
	}
	rows := []accountSnapshot{{Path: "a.json", State: "active"}, {Path: "b.json", State: "active"}}
	annotateAccountPolicies(rows)
	if *rows[0].SchedulingEnabled || summarizeAccounts(rows)["paused"] != 1 {
		t.Fatal("account toggle/overview did not reflect the disable")
	}
	w = adminRequest(handler, "POST", "/admin/api/model-toggle", "admin", `{"target":"two","enabled":true}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(runtimeConfigFile)
	obj := decodeObject(t, data)
	models := obj["models"].(map[string]any)
	if _, exists := models["allowlist"]; exists {
		t.Fatal("web UI write must delete the allowlist key")
	}
	for id, want := range map[string]bool{"one": true, "two": true, "three": false} {
		disabled, _ := modelDisabled(id)
		if disabled == want {
			t.Fatalf("conversion lost the original state of model %s", id)
		}
	}
	if configuredSystemPrompts().force != "synthetic preserve" || !strings.Contains(string(data), `"a.json"`) {
		t.Fatal("write lost other config")
	}
	writeStatusSnapshot()
	w = adminRequest(handler, "GET", "/admin/api/models", "admin", "")
	var view struct {
		Models []modelStatSnapshot `json:"models"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if len(view.Models) != 3 {
		t.Fatalf("disabled model was not kept in the admin list: %s", w.Body.String())
	}
	w = adminRequest(handler, "POST", "/admin/api/model-toggle", "admin", `{"target":"three","enabled":true}`)
	if w.Code != 200 {
		t.Fatalf("failed to re-enable: %s", w.Body.String())
	}
	w = adminRequest(handler, "POST", "/admin/api/account-toggle", "admin", `{"target":"a.json","enabled":true}`)
	if w.Code != 200 {
		t.Fatalf("failed to re-enable account: %s", w.Body.String())
	}
	if ok, _ := modelAccountAllowed("two", a, accounts); !ok {
		t.Fatal("after restore, did not participate in model-specific dispatch")
	}
	if ok, _ := modelAccountAllowed("two", b, accounts); ok {
		t.Fatal("toggle must not overwrite model-specific rules")
	}
}

func TestWebPolicyWritesGuardedAndNoLostUpdates(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}, {ID: "two"}}}
	accounts = []*Account{{Path: "a.json"}, {Path: "b.json"}}
	handler := newWebUIHandler()
	for _, tc := range []struct {
		key, body string
		status    int
	}{
		{"", `{"target":"one","enabled":false}`, 401},
		{"admin", `{"target":"one"}`, 400},
		{"admin", `{"target":"nonexistent","enabled":false}`, 400},
		{"admin", `{"target":"one","enabled":"false"}`, 400},
	} {
		w := adminRequest(handler, "POST", "/admin/api/model-toggle", tc.key, tc.body)
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d %s", w.Code, tc.status, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "http://localhost/admin/api/model-toggle", strings.NewReader(`{"target":"one","enabled":false}`))
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Workbuddy-Admin", "1")
	r.Header.Set("Origin", "https://other.example")
	wrongOrigin := httptest.NewRecorder()
	handler.ServeHTTP(wrongOrigin, r)
	if wrongOrigin.Code != http.StatusForbidden {
		t.Fatal("cross-site write should be intercepted before the business logic")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, target := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); errs <- savePolicyToggle("model", id, false, "concurrent") }(target)
	}
	wg.Add(1)
	go func() { defer wg.Done(); errs <- saveAPIKeySettings(true, "api", "concurrent") }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"one", "two"} {
		if off, _ := modelDisabled(id); !off {
			t.Fatal("concurrent writes lost model state")
		}
	}
	data, _ := os.ReadFile(runtimeConfigFile)
	if !strings.Contains(string(data), `"api"`) || currentAPIKey() != "api" {
		t.Fatal("concurrent writes lost auth settings")
	}
	// After the admin key is changed externally, old sessions must not keep overwriting the file.
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"external"}}`)
	w := adminRequest(handler, "POST", "/admin/api/model-toggle", "admin", `{"target":"one","enabled":true}`)
	if w.Code != 409 {
		t.Fatalf("external config conflict should reject the overwrite: %d", w.Code)
	}
}

func TestPolicyWriteAndDebugFailuresPreserveState(t *testing.T) {
	isolatePolicies(t)
	original := `{"gateway":{"adminKey":"admin"},"models":{"blocklist":[]}}`
	writeRuntimeFixture(t, original)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}}}
	// Invalid on-disk config must not be 'conveniently fixed' or overwritten by the page.
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"models":{"accounts":{"all":{"blocklist":["bad/path.json"]}}}}`)
	if err := savePolicyToggle("model", "one", false, "invalid-disk"); err == nil {
		t.Fatal("bad config must not be overwritten")
	}
	if disabled, _ := modelDisabled("one"); disabled {
		t.Fatal("write failure changed the runtime state")
	}
	writeRuntimeFixture(t, original)
	// When an existing file occupies the logs path, a failed debug enablement must preserve the other valid config.
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	if err := os.Chdir(temp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldCwd)
	if err := os.WriteFile(logDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"changed"},"debug":{"enabled":true},"models":{"blocklist":["one"]}}`)
	watcher := runtimeConfigWatcher{}
	if watcher.reload() {
		t.Fatal("config was still loaded even though the debug log is not writable")
	}
	if currentAdminKey() != "admin" || debugLoggingEnabled() {
		t.Fatal("debug failure changed other runtime state")
	}
	if disabled, _ := modelDisabled("one"); disabled {
		t.Fatal("debug failure changed model rules")
	}
}

func TestHotTransportClonesForChangedHeaderTimeout(t *testing.T) {
	isolatePolicies(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	base := &http.Transport{ResponseHeaderTimeout: time.Second}
	transport := &hotConfigTransport{transport: base}
	defer transport.CloseIdleConnections()
	upstreamConfigMu.Lock()
	upstreamHeaderTimeout = 3 * time.Second
	upstreamConfigMu.Unlock()
	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if base.ResponseHeaderTimeout != time.Second || transport.transport == base || transport.transport.ResponseHeaderTimeout != 3*time.Second {
		t.Fatal("active Transport was modified in place")
	}
}
