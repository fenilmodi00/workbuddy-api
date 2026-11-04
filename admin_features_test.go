package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func isolateAdminFeatures(t *testing.T) {
	t.Helper()
	isolatePolicies(t)
	oldRetention := logRetentionDays.Swap(0)
	cleanupScheduleMu.Lock()
	oldSchedule := lastLogCleanup
	lastLogCleanup.time, lastLogCleanup.days = time.Time{}, 0
	cleanupScheduleMu.Unlock()
	t.Cleanup(func() {
		logRetentionDays.Store(oldRetention)
		cleanupScheduleMu.Lock()
		lastLogCleanup = oldSchedule
		cleanupScheduleMu.Unlock()
	})
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":true,"apiKey":"feature-api-key"},"systemPrompt":{"force":"visible prompt text"},"models":{"accounts":{"all":{"blocklist":[]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
}

func TestEditableCredentialsScopeConflictAndSecretAudit(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthExplicit, cfg.AuthFile = true, "workbuddy-edit.json"
	const oldToken = "synthetic-editor-access-token-private"
	const refresh = "synthetic-editor-refresh-token-private"
	content := `{"auth":{"accessToken":"` + oldToken + `","refreshToken":"` + refresh + `","expiresAt":1900000000},"account":{"uid":"u1","nickname":"before"},"edition":"cn","extra":{"keep":true}}`
	if err := os.WriteFile(cfg.AuthFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadAccounts(); err != nil {
		t.Fatal(err)
	}
	accounts[0].QuotaKnown, accounts[0].QuotaRemaining = true, 123
	handler := newWebUIHandler()
	if w := adminRequest(handler, "GET", "/admin/api/credential-edit?file=workbuddy-edit.json", "feature-api-key", ""); w.Code != 401 {
		t.Fatal("model Key must not obtain plaintext credentials")
	}
	for _, target := range []string{"config.json", "../config.json", "/etc/passwd"} {
		w := adminRequest(handler, "GET", "/admin/api/credential-edit?file="+target, "feature-admin-key", "")
		if w.Code != 400 {
			t.Fatalf("arbitrary path not rejected: %s %d", target, w.Code)
		}
	}
	w := adminRequest(handler, "GET", "/admin/api/credential-edit?file=workbuddy-edit.json", "feature-admin-key", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), oldToken) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("authenticated plaintext editor did not load")
	}
	var view struct{ Content, Revision string }
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	edited := strings.ReplaceAll(strings.ReplaceAll(view.Content, oldToken, "synthetic-edited-new-access-token"), "before", "after")
	body, _ := json.Marshal(map[string]string{"path": cfg.AuthFile, "revision": view.Revision, "content": edited})
	w = adminRequest(handler, "POST", "/admin/api/credential-edit", "feature-admin-key", string(body))
	if w.Code != 200 {
		t.Fatalf("edit save failed: %d %s", w.Code, w.Body.String())
	}
	raw, _ := os.ReadFile(cfg.AuthFile)
	if string(raw) != edited || accounts[0].Auth.Account.Nickname != "after" || accounts[0].QuotaRemaining != 123 {
		t.Fatal("file or runtime credentials not updated, or the quota ledger was lost")
	}
	w = adminRequest(handler, "POST", "/admin/api/credential-edit", "feature-admin-key", string(body))
	if w.Code != 409 {
		t.Fatal("stale edit revision not rejected; it may overwrite background renewal")
	}
	w = adminRequest(handler, "GET", "/admin/api/credentials", "feature-admin-key", "")
	if strings.Contains(w.Body.String(), "synthetic-edited-new-access-token") || strings.Contains(w.Body.String(), refresh) {
		t.Fatal("credential list leaked the original text")
	}
	for _, file := range listLogFiles() {
		logData, _ := os.ReadFile(filepath.Join(logDir, file.Name))
		for _, secret := range []string{oldToken, refresh, "synthetic-edited-new-access-token"} {
			if strings.Contains(string(logData), secret) {
				t.Fatal("plaintext credentials were recorded into logs")
			}
		}
	}
	for _, bad := range []string{`null`, `{`, `{"auth":{"accessToken":""}}`, `{"auth":{"accessToken":"ACCESS****"}}`, `{"auth":{"accessToken":"x"},"edition":"bad"}`} {
		body, _ = json.Marshal(map[string]string{"path": cfg.AuthFile, "revision": credentialRevision(raw), "content": bad})
		if w := adminRequest(handler, "POST", "/admin/api/credential-edit", "feature-admin-key", string(body)); w.Code != 400 {
			t.Fatalf("invalid credential accepted: %s", bad)
		}
		after, _ := os.ReadFile(cfg.AuthFile)
		if string(after) != string(raw) {
			t.Fatal("invalid edit changed the file")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := saveEditedCredential(ctx, cfg.AuthFile, credentialRevision(raw), edited, "cancel"); err == nil {
		t.Fatal("cancelled request must not save credentials")
	}
}

func TestCredentialEditRejectsSymlink(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthExplicit, cfg.AuthFile = true, "workbuddy-link.json"
	if err := os.WriteFile("source.json", []byte(`{"auth":{"accessToken":"synthetic"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source.json", cfg.AuthFile); err != nil {
		t.Skip("symlinks cannot be created on this machine")
	}
	w := adminRequest(newWebUIHandler(), "GET", "/admin/api/credential-edit?file="+cfg.AuthFile, "feature-admin-key", "")
	if w.Code != 400 {
		t.Fatal("symlinked credentials must not be editable via the web")
	}
}

func TestCredentialEditorRestoresInvalidFileAndResetsNewIdentity(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthExplicit, cfg.AuthFile, cfg.ReloadInterval = true, "workbuddy-fixed.json", 0
	accounts = nil
	if err := os.WriteFile(cfg.AuthFile, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	content := `{"auth":{"accessToken":"synthetic-restored-token"},"account":{"uid":"one"},"edition":"cn"}`
	if err := saveEditedCredential(context.Background(), cfg.AuthFile, credentialRevision([]byte(`{broken`)), content, "restore"); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Auth.Account.UID != "one" {
		t.Fatal("repaired credentials were not immediately added to the running account pool")
	}
	accounts[0].QuotaKnown, accounts[0].QuotaRemaining = true, 1000
	accounts[0].ModelStates = map[string]*modelRuntimeState{"m": {CostClass: modelCostFree}}
	changed := strings.ReplaceAll(content, `"one"`, `"two"`)
	if err := saveEditedCredential(context.Background(), cfg.AuthFile, credentialRevision([]byte(content)), changed, "identity"); err != nil {
		t.Fatal(err)
	}
	if accounts[0].QuotaKnown || accounts[0].QuotaRemaining != 0 || len(accounts[0].ModelStates) != 0 {
		t.Fatal("new account identity incorrectly reused the old account's balance/free ledger")
	}
}

func TestPortSettingsSaveHotSwitchesAndValidates(t *testing.T) {
	isolateAdminFeatures(t)
	actualAPI, actualWeb := freeLoopbackPort(t), freeLoopbackPort(t)
	nextAPI, nextWeb := freeLoopbackPort(t), freeLoopbackPort(t)
	cfg.Addr, cfg.WebUI, cfg.PortExplicit = "127.0.0.1", true, false
	cfg.Port, cfg.WebPort = actualAPI, actualWeb
	servers, listeners, err := prepareGatewayServers()
	if err != nil {
		t.Fatal(err)
	}
	gatewayServers.register(servers, listeners)
	gatewayServers.start(make(chan error, 2))
	t.Cleanup(func() { gatewayServers.shutdown(context.Background()) })
	handler := newWebUIHandler()
	body := fmt.Sprintf(`{"apiPort":%d,"webPort":%d}`, nextAPI, nextWeb)
	w := adminRequest(handler, "POST", "/admin/api/settings", "feature-admin-key", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if cfg.Port != nextAPI || cfg.WebPort != nextWeb || view["configuredApiPort"] != float64(nextAPI) ||
		view["restartRequired"] != false || view["livePortSwitching"] != true {
		t.Fatal("saved ports were not hot-switched")
	}
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(nextAPI) + "/health")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("new API listener unavailable: %v", err)
	}
	response.Body.Close()
	raw, _ := os.ReadFile(runtimeConfigFile)
	var c runtimeFileConfig
	_ = json.Unmarshal(raw, &c)
	if c.Gateway.APIPort != nextAPI || c.Gateway.WebPort != nextWeb || c.SystemPrompt.Force != "visible prompt text" || currentAPIKey() != "feature-api-key" {
		t.Fatal("ports not saved or other config was changed")
	}
	for _, body := range []string{`{"apiPort":70000}`, fmt.Sprintf(`{"apiPort":%d}`, nextWeb), `{"webPort":-1}`, `{"webPort":1.5}`} {
		if w := adminRequest(handler, "POST", "/admin/api/settings", "feature-admin-key", body); w.Code != 400 {
			t.Fatalf("invalid port accepted: %s %d", body, w.Code)
		}
	}
	cfg.PortExplicit = true
	if w := adminRequest(handler, "POST", "/admin/api/settings", "feature-admin-key", fmt.Sprintf(`{"webPort":%d}`, cfg.Port)); w.Code != 400 {
		t.Fatal("port conflict caused by the -port override was not rejected")
	}
	w = adminRequest(handler, "GET", "/admin/api/config", "feature-admin-key", "")
	if !strings.Contains(w.Body.String(), "visible prompt text") || strings.Contains(w.Body.String(), "feature-api-key") {
		t.Fatal("prompt should be visible while Keys remain hidden")
	}
	html, _ := webFS.ReadFile("web/index.html")
	js, _ := webFS.ReadFile("web/app.js")
	if strings.Contains(string(html), "maskHints") || strings.Contains(string(js), "maskSystemPrompt") {
		t.Fatal("systemPrompt masking feature was not removed")
	}
}

func TestLogRetentionBoundaryAndSafeFiles(t *testing.T) {
	isolateAdminFeatures(t)
	now := time.Now()
	old := now.AddDate(0, 0, -7).Format("2006-01-02")
	boundary := now.AddDate(0, 0, -6).Format("2006-01-02")
	deleteNames := []string{"gateway-" + old + ".log", "debug-" + old + ".jsonl", "deployment-" + old + ".jsonl"}
	keepNames := []string{"gateway-" + boundary + ".log", "gateway-" + now.Format("2006-01-02") + ".log", "gateway-2099-01-01.log", "gateway-2026-02-30.log", "workbuddy-a.json", "report.md", "random-2000-01-01.log"}
	for _, name := range append(deleteNames, keepNames...) {
		if err := os.WriteFile(filepath.Join(logDir, name), []byte("fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	directory := "gateway-2000-01-01.log"
	if err := os.Mkdir(filepath.Join(logDir, directory), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := cleanupLogs(now, 0, "off")
	if err != nil || result.Deleted != 0 {
		t.Fatal("0 should keep everything")
	}
	result, err = cleanupLogs(now, 7, "boundary")
	if err != nil || result.Deleted != 3 {
		t.Fatalf("cleanup count incorrect: %+v %v", result, err)
	}
	for _, name := range deleteNames {
		if _, err := os.Stat(filepath.Join(logDir, name)); !os.IsNotExist(err) {
			t.Fatalf("expired log not deleted: %s", name)
		}
	}
	for _, name := range append(keepNames, directory) {
		if _, err := os.Stat(filepath.Join(logDir, name)); err != nil {
			t.Fatalf("cleanup deleted other files by mistake: %s", name)
		}
	}
}

func TestDailyLogRotationAndActiveProtection(t *testing.T) {
	isolateAdminFeatures(t)
	writer, err := newDailyLogWriter("debug", ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	yesterday := time.Now().AddDate(0, 0, -1)
	writer.mu.Lock()
	err = writer.rotate(yesterday)
	oldPath := writer.path
	writer.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	result, err := cleanupLogs(time.Now(), 1, "active")
	if err != nil || result.SkippedActive == 0 {
		t.Fatal("old log being written to must not be deleted")
	}
	if _, err := writer.Write([]byte("new day event\n")); err != nil {
		t.Fatal(err)
	}
	if writer.path == oldPath {
		t.Fatal("write across midnight did not switch the date")
	}
	if result, err := cleanupLogs(time.Now(), 1, "rotated"); err != nil || result.Deleted != 1 {
		t.Fatalf("old log not reclaimed after rotation: %+v %v", result, err)
	}
}

func TestLogSettingsPreserveConfigAndHotReload(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	for _, body := range []string{`{}`, `{"retentionDays":-1}`, `{"retentionDays":1.5}`, `{"retentionDays":36501}`} {
		if w := adminRequest(handler, "POST", "/admin/api/log-settings", "feature-admin-key", body); w.Code != 400 {
			t.Fatalf("invalid retention days accepted: %s", body)
		}
	}
	w := adminRequest(handler, "POST", "/admin/api/log-settings", "feature-admin-key", `{"retentionDays":7}`)
	if w.Code != 200 || logRetentionDays.Load() != 7 {
		t.Fatalf("retention setting did not take effect: %d %s", w.Code, w.Body.String())
	}
	raw, _ := os.ReadFile(runtimeConfigFile)
	var c runtimeFileConfig
	_ = json.Unmarshal(raw, &c)
	if c.Logs.RetentionDays != 7 || c.SystemPrompt.Force != "visible prompt text" || currentAPIKey() != "feature-api-key" {
		t.Fatal("retention setting overwrote other config")
	}
	oldName := "gateway-" + time.Now().AddDate(0, 0, -8).Format("2006-01-02") + ".log"
	_ = os.WriteFile(filepath.Join(logDir, oldName), []byte("expired"), 0600)
	w = adminRequest(handler, "POST", "/admin/api/log-cleanup", "feature-admin-key", `{}`)
	if w.Code != 200 {
		t.Fatalf("cleanup failed: %s", w.Body.String())
	}
	if w := adminRequest(handler, "POST", "/admin/api/log-cleanup", "feature-admin-key", `null`); w.Code != 400 {
		t.Fatal("cleanup request must explicitly submit an empty object, not null")
	}
	if _, err := os.Stat(filepath.Join(logDir, oldName)); !os.IsNotExist(err) {
		t.Fatal("manual cleanup did not delete the expired log")
	}
	if w := adminRequest(handler, "POST", "/admin/api/log-cleanup", "feature-api-key", `{}`); w.Code != 401 {
		t.Fatal("model Key must not be able to clean logs")
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"feature-admin-key"},"logs":{"retentionDays":3}}`)
	if !(new(runtimeConfigWatcher)).reload() || logRetentionDays.Load() != 3 {
		t.Fatal("log retention config from file editing was not hot-reloaded")
	}
	_ = os.WriteFile(filepath.Join(logDir, oldName), []byte("expired"), 0600)
	scheduledLogCleanup(time.Now())
	if _, err := os.Stat(filepath.Join(logDir, oldName)); !os.IsNotExist(err) {
		t.Fatal("automatic cleanup did not apply the new rule")
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"feature-admin-key"}}`)
	if !(new(runtimeConfigWatcher)).reload() || logRetentionDays.Load() != 0 {
		t.Fatal("missing field in old config should return to no automatic cleanup")
	}
	r := httptest.NewRequest(http.MethodPost, "http://localhost/admin/api/log-cleanup", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer feature-admin-key")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Workbuddy-Admin", "1")
	r.Header.Set("Origin", "https://foreign.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatal("cross-site log cleanup was not intercepted")
	}
}
