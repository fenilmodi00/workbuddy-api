package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListFilterAuditIsAuthenticatedBooleanOnlyAndDoesNotChangePolicies(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	before, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"list_filter_changed", "list_filter_restored", "list_filter_cookie_failed"} {
		for _, route := range []string{"/admin/api/status", "/admin/api/models"} {
			for _, enabled := range []bool{false, true} {
				raw, _ := json.Marshal(map[string]any{"event": event, "route": route, "status": 0, "enabled": enabled})
				w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", string(raw))
				if w.Code != http.StatusNoContent {
					t.Fatalf("valid list filter event rejected: event=%s route=%s code=%d", event, route, w.Code)
				}
			}
		}
	}
	for _, body := range []string{
		`{"event":"list_filter_changed","route":"/admin/api/status","status":0}`,
		`{"event":"list_filter_changed","route":"/admin/api/status","status":0,"enabled":"true"}`,
		`{"event":"list_filter_changed","route":"/admin/api/config-edit","status":0,"enabled":true}`,
		`{"event":"request_completed","route":"/admin/api/status","status":200,"enabled":true}`,
		`{"event":"list_filter_changed","route":"/admin/api/models","status":0,"enabled":true,"cookie":"private"}`,
	} {
		w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unsafe list filter event accepted: HTTP=%d", w.Code)
		}
	}
	body := `{"event":"list_filter_changed","route":"/admin/api/models","status":0,"enabled":true}`
	for _, key := range []string{"", "feature-api-key"} {
		if w := adminRequest(handler, "POST", "/admin/api/client-events", key, body); w.Code != http.StatusUnauthorized {
			t.Fatal("filter audit endpoint bypassed management authentication")
		}
	}
	// Cookies are not login credentials and cannot even substitute for the Authorization admin header.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	req.Header.Set("Cookie", "gateway_accounts_only_usable=1; gateway_models_only_usable=1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("display preference Cookie bypassed management authentication")
	}
	after, err := os.ReadFile(runtimeConfigFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("display preference audit changed production policies")
	}
	logData, err := os.ReadFile(filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log"))
	if err != nil || !bytes.Contains(logData, []byte("[Admin frontend filter]")) ||
		!bytes.Contains(logData, []byte("onlyUsable=true")) || !bytes.Contains(logData, []byte("onlyUsable=false")) {
		t.Fatal("display preference changes were not logged")
	}
}

func TestStatsChartHoverAuditOnlyAcceptsFixedMetadata(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	before, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"stats_chart_hover_started", "stats_chart_hover_ended"} {
		body := `{"event":"` + event + `","route":"/admin/api/stats","status":0}`
		w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", body)
		if w.Code != http.StatusNoContent {
			t.Fatalf("hover audit status=%d", w.Code)
		}
	}
	for _, body := range []string{
		`{"event":"stats_chart_hover_started","route":"/admin/api/config","status":0}`,
		`{"event":"stats_chart_hover_started","route":"/admin/api/stats","status":200}`,
		`{"event":"stats_chart_hover_ended","route":"/admin/api/stats","status":0,"enabled":true}`,
		`{"event":"stats_chart_hover_started","route":"/admin/api/stats","status":0,"coordinates":[1,2]}`,
	} {
		if w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", body); w.Code != http.StatusBadRequest {
			t.Fatalf("unsafe hover event accepted: %d", w.Code)
		}
	}
	if w := adminRequest(handler, "POST", "/admin/api/client-events", "", `{"event":"stats_chart_hover_started","route":"/admin/api/stats","status":0}`); w.Code != http.StatusUnauthorized {
		t.Fatal("hover audit bypassed authentication")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(before, after) {
		t.Fatal("hover audit changed configuration")
	}
	entries, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	var logs string
	for _, p := range entries {
		raw, _ := os.ReadFile(p)
		logs += string(raw)
	}
	if !strings.Contains(logs, "stats_chart_hover_started") || !strings.Contains(logs, "stats_chart_hover_ended") {
		t.Fatal("hover audit was not persisted")
	}
}

func TestWebUIBrandNeutralLabelsAndBothListFilters(t *testing.T) {
	html, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(html)), "workbuddy") {
		t.Fatal("web console static copy still contains the old brand")
	}
	for _, text := range []string{"<title>Local Gateway · Web Console</title>", `id="onlyUsableAccounts"`, `id="onlyUsable"`,
		"Only show usable accounts", "Only show usable models", `id="addAccountCopy"`, "Copy link"} {
		if !bytes.Contains(html, []byte(text)) {
			t.Errorf("web console missing list control or neutral copy: %s", text)
		}
	}
}

// setWebUI temporarily enables/disables the admin console and restores it after the case, avoiding pollution of other tests.
func setWebUI(t *testing.T, enabled bool, apiKey string) {
	t.Helper()
	oldCfg := cfg
	cfg.WebUI = enabled
	cfg.AdminKey = apiKey
	cfg.Port, cfg.WebPort = defaultAPIPort, defaultWebPort
	t.Cleanup(func() {
		cfg = oldCfg
	})
}

// Uses the real dedicated admin port middleware chain, including auth, origin validation, and auditing.
func newWebUIMux() http.Handler {
	return newWebUIHandler()
}

func TestWebUIPreflightError(t *testing.T) {
	cases := []struct {
		webui  bool
		apiKey string
		want   bool
	}{
		{false, "", false},
		{false, "k", false},
		{true, "", false}, // The local first-time setup page may start without an admin Key.
		{true, "k", false},
	}
	for _, c := range cases {
		setWebUI(t, c.webui, c.apiKey)
		err := webUIPreflightError()
		if (err != nil) != c.want {
			t.Fatalf("webui=%v apiKey=%q err=%v want=%v", c.webui, c.apiKey, err, c.want)
		}
	}
}

func TestWebUIDisabledRoutesAbsent(t *testing.T) {
	setWebUI(t, false, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	for _, p := range []string{"/ui/", "/ui/app.js", "/admin/api/status"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("with the admin console disabled, %s returned status %d; expected 404", p, rec.Code)
		}
	}
}

func TestWebUIEnabledAuthAndStaticShell(t *testing.T) {
	chdirTemp(t)
	setWebUI(t, true, "webui-key")
	raw, _ := json.Marshal(statusSnapshot{UpdatedAt: time.Now().Unix()})
	if err := os.WriteFile(statusSnapshotFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	handler := newWebUIMux()

	// The static shell is exempt from auth so the browser can load the page.
	for _, p := range []string{"/ui/", "/ui/app.js", "/ui/style.css"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("static asset %s returned status %d; expected 200", p, rec.Code)
		}
	}

	// Data endpoints require authentication.
	for _, p := range []string{"/admin/api/status", "/admin/api/models", "/admin/api/logs", "/admin/api/config", "/admin/api/credentials"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("accessing %s without a key returned status %d; expected 401", p, rec.Code)
		}
	}

	seen := func(p string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer webui-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("accessing %s with a key returned status %d; expected 200", p, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("accessing %s returned non-JSON: %v", p, err)
		}
		return body
	}

	status := seen("/admin/api/status")
	if _, ok := status["gateway"]; !ok {
		t.Fatal("/admin/api/status missing gateway field")
	}
	if _, ok := status["summary"]; !ok {
		t.Fatal("/admin/api/status missing summary field")
	}
	if _, ok := status["siteSummary"]; !ok {
		t.Fatal("/admin/api/status missing siteSummary field")
	}
	models := seen("/admin/api/models")
	if _, ok := models["models"]; !ok {
		t.Fatal("/admin/api/models missing models field")
	}
	seen("/admin/api/logs")
	seen("/admin/api/config")
	seen("/admin/api/credentials")
}

func TestSummarizeAccountsBySite(t *testing.T) {
	accs := []accountSnapshot{
		{Edition: "cn", State: "active"},
		{Edition: "cn", State: "cooldown"},
		{Edition: "intl", State: "disabled"},
		{Edition: "", State: "active"}, // Missing site defaults to the domestic site
	}
	got := summarizeAccountsBySite(accs)
	if got["cn"]["total"] != 3 || got["cn"]["active"] != 2 || got["cn"]["cooldown"] != 1 {
		t.Fatalf("domestic-site summary wrong: %+v", got["cn"])
	}
	if got["intl"]["total"] != 1 || got["intl"]["disabled"] != 1 || got["intl"]["active"] != 0 {
		t.Fatalf("international-site summary wrong: %+v", got["intl"])
	}
	empty := summarizeAccountsBySite(nil)
	if empty["cn"]["total"] != 0 || empty["intl"]["total"] != 0 {
		t.Fatalf("empty account pool should return all zeros: %+v", empty)
	}
}

func TestMaskToken(t *testing.T) {
	if got := maskToken(""); got != "" {
		t.Fatalf("empty token should stay empty, got %q", got)
	}
	if got := maskToken("abc"); got != "****" {
		t.Fatalf("too-short token should be fully masked, got %q", got)
	}
	if got := maskToken("ACCESS_TOKEN_SECRET_123456"); got != "ACCESS****" {
		t.Fatalf("mask should keep only the first 6 characters, got %q", got)
	}
}

func TestWebUICredentialsMaskedAndNoPlaintext(t *testing.T) {
	chdirTemp(t)
	oldExplicit, oldDir, oldFile := cfg.AuthExplicit, cfg.AuthDir, cfg.AuthFile
	cfg.AuthExplicit = false
	cfg.AuthDir = ""
	t.Cleanup(func() {
		cfg.AuthExplicit, cfg.AuthDir, cfg.AuthFile = oldExplicit, oldDir, oldFile
	})

	const accessToken = "ACCESS_TOKEN_SECRET_123456"
	const refreshToken = "REFRESH_TOKEN_SECRET_abcdef"
	payload := `{"auth":{"accessToken":"` + accessToken + `","refreshToken":"` + refreshToken +
		`","expiresAt":1893456000,"domain":"www.codebuddy.cn"},` +
		`"account":{"uid":"uid-1","nickname":"user-a"},"edition":"cn"}`
	if err := os.WriteFile("workbuddy-a.json", []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handleWebUICredentials(rec, httptest.NewRequest(http.MethodGet, "/admin/api/credentials", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; expected 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, accessToken) || strings.Contains(body, refreshToken) {
		t.Fatal("credential response leaked plaintext tokens")
	}
	if !strings.Contains(body, "ACCESS****") || !strings.Contains(body, "REFRES****") {
		t.Fatalf("credential response missing masked token prefix: %s", body)
	}
	if !strings.Contains(body, "user-a") {
		t.Fatal("credential response should keep non-sensitive fields (nickname)")
	}
}

func TestWebUILogsRejectsIllegalName(t *testing.T) {
	for _, bad := range []string{"evil.txt", "../etc/passwd", "../gateway-2026-10-06.log", "workbuddy.json"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin/api/logs?file="+bad, nil)
		handleWebUILogs(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("illegal log name %q returned status %d; expected 400", bad, rec.Code)
		}
	}
}

func TestWebUILogsListsLogDir(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}
	name := "gateway-2026-10-06.log"
	if err := os.WriteFile(filepath.Join(logDir, name), []byte("line1\nline2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handleWebUILogs(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs?lines=50", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; expected 200", rec.Code)
	}
	var resp struct {
		File  string   `json:"file"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.File != name {
		t.Fatalf("default selected file=%q; expected %q", resp.File, name)
	}
	if len(resp.Lines) != 2 {
		t.Fatalf("log line count=%d; expected 2", len(resp.Lines))
	}
}
