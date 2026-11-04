package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func editorRequestBody(t *testing.T, revision, content string, removal bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"revision": revision, "content": content, "acknowledged": true, "confirmAdminKeyRemoval": removal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestConfigEditorRequiresAdminAndWarningAcknowledgement(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	for _, key := range []string{"", "feature-api-key"} {
		for _, method := range []string{"GET", "POST"} {
			w := adminRequest(handler, method, "/admin/api/config-edit?acknowledged=1", key, "{}")
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "feature-admin-key") {
				t.Fatalf("config edit bypassed admin auth: %s %d", method, w.Code)
			}
		}
	}
	for _, method := range []string{"GET", "POST"} {
		w := adminRequest(handler, method, "/admin/api/config-edit", "feature-admin-key", "{}")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("missing risk acknowledgement must be rejected: %s %d", method, w.Code)
		}
	}
	w := adminRequest(handler, "GET", "/admin/api/config", "feature-admin-key", "")
	if strings.Contains(w.Body.String(), "feature-admin-key") || strings.Contains(w.Body.String(), "feature-api-key") {
		t.Fatal("regular preview exposed real Keys")
	}
	w = adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "feature-admin-key", "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(w.Body.String(), "feature-admin-key") || !strings.Contains(w.Body.String(), "visible prompt text") {
		t.Fatalf("did not obtain real config after acknowledgement: %d", w.Code)
	}
	var view struct{ Content, Revision string }
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	onDisk, _ := os.ReadFile(runtimeConfigFile)
	if view.Content != string(onDisk) || view.Revision != configRevision(onDisk) {
		t.Fatal("editor must use the original text, not the redacted preview")
	}
	methodRequest := httptest.NewRequest(http.MethodPut, "/admin/api/config-edit", strings.NewReader("{}"))
	methodRequest.Header.Set("Authorization", "Bearer feature-admin-key")
	methodRequest.Header.Set("Content-Type", "application/json")
	methodRequest.Header.Set("X-Workbuddy-Admin", "1")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, methodRequest)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("config edit did not restrict methods")
	}
	crossOrigin := httptest.NewRequest(http.MethodGet, "/admin/api/config-edit?acknowledged=1", nil)
	crossOrigin.Header.Set("Authorization", "Bearer feature-admin-key")
	crossOrigin.Header.Set("Origin", "https://untrusted.invalid")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, crossOrigin)
	if w.Code != http.StatusForbidden {
		t.Fatal("plaintext config edit endpoint accepted a cross-site origin")
	}
	// The in-lock second authentication must also reject stale sessions (validates the handler alone, without outer middleware).
	req := httptest.NewRequest(http.MethodGet, "/admin/api/config-edit?acknowledged=1", nil)
	req.Header.Set("Authorization", "Bearer stale-key")
	w = httptest.NewRecorder()
	handleWebUIConfigEdit(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("session was not re-validated inside the lock")
	}
}

func TestConfigEditorSavesHotSettingsPortsAndSecretSafeAudit(t *testing.T) {
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
	old, _ := os.ReadFile(runtimeConfigFile)
	oldWriter := log.Writer()
	var audit bytes.Buffer
	log.SetOutput(&redactingLogWriter{writer: io.MultiWriter(oldWriter, &audit)})
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	const prompt = "synthetic-private-editor-prompt"
	const newAPIKey = "synthetic-new-editor-api-key"
	content := fmt.Sprintf(`{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":true,"apiKey":"%s","apiPort":%d,"webPort":%d},"systemPrompt":{"force":"%s","fallback":"fallback"},"models":{"blocklist":["off"],"accounts":{"all":{"blocklist":["paused.json"]}}},"logs":{"retentionDays":5},"upstream":{"idleTimeoutSeconds":77,"transientRetries":0},"debug":{"enabled":false}}`, newAPIKey, nextAPI, nextWeb, prompt)
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
	if w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Revision        string
		AdminKeyChanged bool
		Settings        struct{ RestartRequired bool }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(runtimeConfigFile)
	if response.Revision != configRevision(saved) || response.Settings.RestartRequired || response.AdminKeyChanged {
		t.Fatal("save status, revision, or port hot-switch state incorrect")
	}
	if cfg.Port != nextAPI || cfg.WebPort != nextWeb || currentAPIKey() != newAPIKey ||
		configuredSystemPrompts().force != prompt || logRetentionDays.Load() != 5 {
		t.Fatal("hot config not applied or listener ports not hot-switched")
	}
	if disabled, _ := modelDisabled("off"); !disabled {
		t.Fatal("model blocklist not applied")
	}
	if allowed, _ := modelAccountAllowed("any", &Account{Path: "paused.json"}, nil); allowed {
		t.Fatal("global account blocklist not applied")
	}
	_, idle, retries := upstreamSettings()
	if idle != 77*time.Second || retries != 0 {
		t.Fatal("upstream settings not applied")
	}
	watcher := runtimeConfigWatcher{}
	if watcher.reload() {
		t.Fatal("save digest not updated, causing duplicate hot reload")
	}
	for _, secret := range []string{"feature-admin-key", newAPIKey, prompt, "fallback"} {
		if strings.Contains(audit.String(), secret) || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("edit save log or response leaked config content: %q", secret)
		}
	}
	for _, marker := range []string{"[Request received]", "[Admin origin validation]", "[Admin auth]", "[Config edit]", "[Config hot reload]", "[Response returned]"} {
		if !strings.Contains(audit.String(), marker) {
			t.Errorf("missing full-chain audit entry %s", marker)
		}
	}
}

func TestConfigEditorRejectsInvalidDocumentsWithoutMutation(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	for _, content := range []string{
		"", "{", "null", "[]", `{} {}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiPort":"8317"}}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiPort":8316,"webPort":8316}}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":true}}`,
		`{"gateway":{"adminKey":"[REDACTED]"}}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiKey":"[REDACTED]"}}`,
		`{"models":{"allowlist":["one"],"blocklist":["two"]}}`,
		`{"models":{"accounts":{"all":{"blocklist":["../other.json"]}}}}`,
		`{"logs":{"retentionDays":-1}}`,
		`{"synthetic-secret-unknown-field":"secret-body"}`,
		strings.Repeat("x", configEditBudget+1),
	} {
		w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid config not rejected: bytes=%d HTTP=%d", len(content), w.Code)
		}
		after, _ := os.ReadFile(runtimeConfigFile)
		if !bytes.Equal(after, old) || currentAdminKey() != "feature-admin-key" || currentAPIKey() != "feature-api-key" {
			t.Fatal("invalid config modified disk or runtime")
		}
		if strings.Contains(w.Body.String(), "synthetic-secret-unknown-field") {
			t.Fatal("validation error leaked user content")
		}
	}
	for _, body := range []string{`{`, `{"acknowledged":true,"path":"../other.json"}`, `{"acknowledged":true,"content":false}`} {
		if w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", body); w.Code != 400 {
			t.Fatal("unknown form field or type not rejected")
		}
	}
}

func TestConfigEditorRevisionConflictAndExternalKeyChange(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	if err := saveAPIKeySettings(false, "", "other-page"); err != nil {
		t.Fatal(err)
	}
	changed, _ := os.ReadFile(runtimeConfigFile)
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), string(old), false))
	if w.Code != http.StatusConflict {
		t.Fatal("overwriting another page's update was not blocked")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(after, changed) {
		t.Fatal("revision conflict overwrote the new config")
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"changed-before-reload"}}`)
	changed, _ = os.ReadFile(runtimeConfigFile)
	for _, method := range []string{"GET", "POST"} {
		w = adminRequest(handler, method, "/admin/api/config-edit?acknowledged=1", "feature-admin-key", editorRequestBody(t, configRevision(changed), string(old), false))
		if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "changed-before-reload") {
			t.Fatal("stale session overwrote or read the not-yet-hot-reloaded new admin Key")
		}
	}
}

func TestConfigEditorAdminKeyChangeAndRemovalConfirmation(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	content := strings.ReplaceAll(string(old), "feature-admin-key", "synthetic-new-editor-admin-key")
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"adminKeyChanged":true`) {
		t.Fatal("admin Key change not reported to the client")
	}
	if adminRequest(handler, "GET", "/admin/api/config", "feature-admin-key", "").Code != 401 {
		t.Fatal("old session still valid")
	}
	old, _ = os.ReadFile(runtimeConfigFile)
	w = adminRequest(handler, "POST", "/admin/api/config-edit", "synthetic-new-editor-admin-key", editorRequestBody(t, configRevision(old), `{}`, false))
	if w.Code != 400 || currentAdminKey() == "" {
		t.Fatal("clearing admin Key allowed without extra confirmation")
	}
	w = adminRequest(handler, "POST", "/admin/api/config-edit", "synthetic-new-editor-admin-key", editorRequestBody(t, configRevision(old), `{}`, true))
	if w.Code != 200 || currentAdminKey() != "" || !strings.Contains(w.Body.String(), `"adminKeyCleared":true`) {
		t.Fatal("confirmed admin Key clearing did not take effect")
	}
	if adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "synthetic-new-editor-admin-key", "").Code != 401 {
		t.Fatal("cleared admin Key unexpectedly opened admin auth")
	}
}

func TestConfigEditorApplyFailureRestoresDiskAndRuntime(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	// When the debug file cannot be created, full config hot-apply must fail and restore the old file.
	if err := os.Mkdir(filepath.Join(logDir, "debug-"+time.Now().Format("2006-01-02")+".jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	content := `{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":false},"debug":{"enabled":true},"models":{"blocklist":["off"]}}`
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("apply failure did not return an error: %d", w.Code)
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(old, after) || currentAPIKey() != "feature-api-key" || debugLoggingEnabled() {
		t.Fatal("failure did not restore the original config")
	}
	if disabled, _ := modelDisabled("off"); disabled {
		t.Fatal("failure partially applied the model blocklist")
	}
}

func TestConfigEditorCanRepairInvalidCurrentFileAndRejectsOversizeRead(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	writeRuntimeFixture(t, `{`)
	w := adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "feature-admin-key", "")
	if w.Code != 200 {
		t.Fatal("admin could not load the corrupted file for repair")
	}
	w = adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision([]byte("{")), `{"gateway":{"adminKey":"feature-admin-key"}}`, false))
	if w.Code != 200 {
		t.Fatalf("corrupted config repair failed: %d", w.Code)
	}
	writeRuntimeFixture(t, strings.Repeat("x", configEditBudget+1))
	if adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "feature-admin-key", "").Code != 400 {
		t.Fatal("config beyond the read budget not rejected")
	}
}

func TestConfigEditorRejectsSymlink(t *testing.T) {
	isolateAdminFeatures(t)
	old, _ := os.ReadFile(runtimeConfigFile)
	if err := os.WriteFile("other.json", old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.json", "test-link.json"); err != nil {
		t.Skip("system did not allow creating symlinks")
	}
	if err := os.Remove(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename("test-link.json", runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	handler := newWebUIHandler()
	for _, method := range []string{"GET", "POST"} {
		w := adminRequest(handler, method, "/admin/api/config-edit?acknowledged=1", "feature-admin-key", editorRequestBody(t, configRevision(old), `{}`, true))
		if w.Code != 400 {
			t.Fatal("web edit incorrectly followed the config symlink")
		}
	}
	after, _ := os.ReadFile("other.json")
	if !bytes.Equal(after, old) {
		t.Fatal("web edit modified a non-config target")
	}
}
