package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountHealthPercentage(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name    string
		rows    []accountSnapshot
		stale   bool
		state   string
		percent *float64
	}{
		{"empty", nil, false, "empty", nil},
		{"all_ok", []accountSnapshot{{State: "active"}, {State: "active"}}, false, "healthy", float64p(100)},
		{"paused_in_denominator", []accountSnapshot{{State: "active"}, {State: "active", SchedulingEnabled: &off}}, false, "degraded", float64p(50)},
		{"no_available", []accountSnapshot{{State: "expired"}, {State: "disabled"}}, false, "unavailable", float64p(0)},
		{"unknown_not_healthy", []accountSnapshot{{State: "unrecognized"}, {State: "active"}}, false, "degraded", float64p(50)},
		{"stale_not_100", []accountSnapshot{{State: "active"}}, true, "unknown", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeAccountHealth(tc.rows, tc.stale)
			if got.State != tc.state || (got.Percent == nil) != (tc.percent == nil) || (got.Percent != nil && *got.Percent != *tc.percent) {
				t.Fatalf("got=%+v want=%s/%v", got, tc.state, tc.percent)
			}
		})
	}
	rows := make([]accountSnapshot, 1000)
	for i := range rows {
		rows[i].State = "active"
	}
	rows[999].State = "cooldown"
	if got := summarizeAccountHealth(rows, false); got.Percent == nil || *got.Percent != 99.9 {
		t.Fatal("non-fully-healthy pool must not round up to 100")
	}
}

func TestAccountAlertParsesKnownLogsOnly(t *testing.T) {
	prefix := "2026/10/14 22:59:30 "
	tests := []struct{ body, state, account string }{
		{`[Account status] {"at":1791989970,"account":"workbuddy a.json","state":"expired"}`, "expired", "workbuddy a.json"},
		{"[Cooldown] Account a.json hit the rate limit, blocked automatically until 2099-01-01 (hint: x)", "cooldown", "a.json"},
		// Legacy Chinese wording from logs written before the English migration still parses (see legacyAccountLogWording).
		{"[Cooldown] 账号 a.json 触发频率限制，自动屏蔽至 2099-01-01 (提示: x)", "cooldown", "a.json"},
		{"[Quota] traceId=x account a.json quota query succeeded, total=0, used=0, remaining=0, paidUser=false, plan=Free; account scheduling frozen pending quota recovery", "paid_exhausted", "a.json"},
		{"[Quota] traceId=x account a.json enterprise quota computed=limitNum 100 - credit 100, remaining=0, unlimited=false, plan=Free; allocated quota used up, paid model scheduling paused", "paid_exhausted", "a.json"},
		{"[Auth] Account a.json login state declared dead; removed from scheduling and disabled marker written; consecutive failures=3 basis=refresh", "disabled", "a.json"},
		{`[Admin toggle] traceId=x result=success type=account target="a b.json" enabled=false note=config saved and applied immediately`, "paused", "a b.json"},
		{"[Quota] traceId=x account a.json quota query failed, HTTP=500; previous quota data kept, plan mark pending refresh", "", ""},
		{"[Auth] Account a.json token needs renewal; current expiry=2099-01-01 00:00:00, renewal threshold=15 minutes before expiry, calling upstream refresh API", "", ""},
		{"[Auth] Account a.json renewal declared login-state-dead by the upstream (1/3 consecutive): x; credentials kept for now, scheduling stops only at the threshold; re-login recommended soon", "", ""},
		{"[ModelCooldown] Account a.json model m hit a model-level rate limit", "", ""},
		{`[Admin toggle] traceId=x result=success type=model target="a.json" enabled=false note=model disabled`, "", ""},
		{`[Account status] {"at":1,"account":"a","state":"active"}`, "", ""},
		{`[Account status] {"at":0,"account":"a","state":"expired"}`, "", ""},
		{"[External API] upstream returned text: account invalid", "", ""},
	}
	for _, tc := range tests {
		event, ok := parseAccountLogEvent(prefix+tc.body, "gateway-2026-10-14.log")
		if ok != (tc.state != "") || (ok && (event.State != tc.state || event.Account != tc.account)) {
			t.Fatalf("body=%s got=%+v/%v", tc.body, event, ok)
		}
	}
}

func TestAccountAlertMarksTrackTransitionsWithoutFlooding(t *testing.T) {
	accountAlertMarks.Lock()
	oldCurrent, oldLast := accountAlertMarks.current, accountAlertMarks.last
	accountAlertMarks.current, accountAlertMarks.last = map[string]accountAlertMark{}, map[string]string{}
	accountAlertMarks.Unlock()
	t.Cleanup(func() {
		accountAlertMarks.Lock()
		accountAlertMarks.current, accountAlertMarks.last = oldCurrent, oldLast
		accountAlertMarks.Unlock()
	})
	var audit bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&audit)
	defer log.SetOutput(writer)
	off := false
	now := time.Now().Unix()
	rows := []accountSnapshot{{Path: "a.json", State: "expired", SchedulingEnabled: &off, DisabledReason: "Bearer never-emit-raw"}}
	marks := recordAccountAlertMarks(rows, now)
	recordAccountAlertMarks(rows, now+1)
	if got := strings.Count(audit.String(), "[Account status]"); got != 2 {
		t.Fatalf("must not log again when state is unchanged, wrote %d lines", got)
	}
	if strings.Contains(audit.String(), "never-emit-raw") {
		t.Fatal("must not write the raw disabled reason into the log")
	}
	seen := map[string]bool{}
	for _, mark := range marks["a.json"] {
		seen[mark.State] = true
		if mark.At != now {
			t.Fatalf("mark time should be the first-seen time: %+v", mark)
		}
	}
	// Repeated snapshots must reuse the same marker: the time is not refreshed and the locator must not be lost.
	if again := recordAccountAlertMarks(rows, now+9); len(again["a.json"]) == 0 {
		t.Fatal("repeated snapshot must not drop alert marks")
	} else {
		for _, mark := range again["a.json"] {
			if mark.At != now {
				t.Fatalf("repeated snapshot must not refresh the first-seen time: %+v", mark)
			}
		}
	}
	if !seen["expired"] || !seen["paused"] {
		t.Fatalf("snapshot marks should include every current anomaly kind: %+v", marks)
	}
	rows[0].State, rows[0].SchedulingEnabled = "active", nil
	if after := recordAccountAlertMarks(rows, now+2); len(after["a.json"]) != 0 {
		t.Fatalf("alert marks must not survive recovery: %+v", after)
	}
	if !strings.Contains(audit.String(), `"state":"active"`) {
		t.Fatal("recovery transitions should also leave an audit log")
	}
}

func TestAccountAlertReadDeduplicateAndResolve(t *testing.T) {
	chdirTemp(t)
	os.Mkdir(logDir, 0700)
	day := "gateway-2026-10-14.log"
	lines := "2026/10/14 01:00:00 [Account status] {\"at\":100,\"account\":\"a.json\",\"state\":\"disabled\"}\n" +
		"2026/10/14 02:00:00 [Account status] {\"at\":200,\"account\":\"a.json\",\"state\":\"disabled\"}\n" +
		"2026/10/14 03:00:00 [Account status] {\"at\":300,\"account\":\"b.json\",\"state\":\"expired\"}\n" +
		"2026/10/14 04:00:00 [Account status] {\"at\":400,\"account\":\"c.json\",\"state\":\"paused\"}\n" +
		"2026/10/14 04:00:00 [Account status] {\"at\":999,\"account\":\"c.json\",\"state\":\"paused\"}" // incomplete trailing line
	os.WriteFile(filepath.Join(logDir, day), []byte(lines), 0600)
	os.WriteFile(filepath.Join(logDir, "debug-2026-10-14.jsonl"), []byte(lines), 0600)
	scan, err := readAccountAlertLogs()
	if err != nil || len(scan.Events) != 3 || scan.Events["a.json\x00disabled"].At != 200 || scan.Events["c.json\x00paused"].At != 400 {
		t.Fatalf("%+v %v", scan, err)
	}
	off := false
	rows := []accountSnapshot{{Path: "a.json", State: "active"}, {Path: "b.json", State: "expired"},
		{Path: "c.json", State: "active", SchedulingEnabled: &off}, {Path: "d.json", State: "cooldown"}}
	alerts, missing := currentAccountAlerts(rows, scan)
	if len(alerts) != 2 || alerts[0].State != "paused" || alerts[1].State != "expired" || missing != 1 {
		t.Fatalf("recovery/dedup/newest first: %+v missing=%d", alerts, missing)
	}
	if alerts[0].LogFile != day {
		t.Fatal("source must reference real log")
	}
}

func TestAccountAlertNewestFirstWithoutSeverityPriority(t *testing.T) {
	off := false
	rows := []accountSnapshot{
		{Path: "old-urgent.json", State: "disabled"},
		{Path: "new-notice.json", State: "active", SchedulingEnabled: &off},
		{Path: "middle-important.json", State: "expired"},
		{Path: "same-time.json", State: "paid_exhausted", SchedulingEnabled: &off},
	}
	scan := accountAlertScan{Events: map[string]accountLogEvent{}}
	for _, event := range []accountLogEvent{
		{At: 100, Account: "old-urgent.json", State: "disabled"},
		{At: 500, Account: "new-notice.json", State: "paused"},
		{At: 300, Account: "middle-important.json", State: "expired"},
		{At: 300, Account: "same-time.json", State: "paused"},
		{At: 300, Account: "same-time.json", State: "paid_exhausted"},
	} {
		scan.Events[event.Account+"\x00"+event.State] = event
	}
	alerts, missing := currentAccountAlerts(rows, scan)
	want := []string{"new-notice.json/paused", "middle-important.json/expired",
		"same-time.json/paid_exhausted", "same-time.json/paused", "old-urgent.json/disabled"}
	if missing != 0 || len(alerts) != len(want) {
		t.Fatalf("alerts=%+v missing=%d", alerts, missing)
	}
	for i, alert := range alerts {
		if got := alert.Account + "/" + alert.State; got != want[i] {
			t.Fatalf("position %d got=%s want=%s; severity must not override timestamp", i, got, want[i])
		}
	}
	if alerts[0].Severity != "notice" || alerts[len(alerts)-1].Severity != "urgent" {
		t.Fatal("new ordering must preserve severity labels")
	}
	// Recovery removes only the resolved category. Global pause remains.
	rows[3].State = "active"
	alerts, missing = currentAccountAlerts(rows, scan)
	if missing != 0 || len(alerts) != 4 {
		t.Fatalf("recovery did not remove only the quota alert: %+v missing=%d", alerts, missing)
	}
	for _, alert := range alerts {
		if alert.Account == "same-time.json" && alert.State != "paused" {
			t.Fatal("resolved quota alert must not survive merely because the account remains paused")
		}
	}
}

func TestAccountAlertRecoveryUsesFreshSnapshotWhileLogsCached(t *testing.T) {
	isolatedGatewayConfig(t)
	accountAlertCache.Lock()
	oldAt, oldScan := accountAlertCache.at, accountAlertCache.scan
	accountAlertCache.at = time.Now()
	accountAlertCache.scan = accountAlertScan{Events: map[string]accountLogEvent{
		"a.json\x00cooldown": {At: time.Now().Unix(), Account: "a.json", State: "cooldown"},
	}}
	cachedAt := accountAlertCache.at
	accountAlertCache.Unlock()
	t.Cleanup(func() {
		accountAlertCache.Lock()
		accountAlertCache.at, accountAlertCache.scan = oldAt, oldScan
		accountAlertCache.Unlock()
	})
	for _, state := range []string{"cooldown", "active"} {
		raw, err := json.Marshal(statusSnapshot{UpdatedAt: time.Now().Unix(),
			Accounts: []accountSnapshot{{Path: "a.json", State: state}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statusSnapshotFile, raw, 0600); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		handleWebUIAccountAlerts(rec, httptest.NewRequest(http.MethodGet, "/admin/api/account-alerts", nil))
		var body struct {
			Alerts []accountAlertView `json:"alerts"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("state=%s code=%d body=%s", state, rec.Code, rec.Body)
		}
		if (state == "cooldown" && len(body.Alerts) != 1) || (state == "active" && len(body.Alerts) != 0) {
			t.Fatalf("latest snapshot must hide recovered account without rescanning logs: state=%s alerts=%+v", state, body.Alerts)
		}
	}
	accountAlertCache.Lock()
	unchanged := accountAlertCache.at.Equal(cachedAt) && len(accountAlertCache.scan.Events) == 1
	accountAlertCache.Unlock()
	if !unchanged {
		t.Fatal("fixture unexpectedly rescanned logs; recovery must be independent of the 10s log cache")
	}
}

func TestAccountAlertEndpointAuthenticatedReadOnlyAndHealth(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-health-admin"}); err != nil {
		t.Fatal(err)
	}
	accountAlertCache.Lock()
	accountAlertCache.at = time.Time{}
	accountAlertCache.Unlock()
	t.Cleanup(func() { accountAlertCache.Lock(); accountAlertCache.at = time.Time{}; accountAlertCache.Unlock() })
	doc := statusSnapshot{UpdatedAt: time.Now().Unix(), Accounts: []accountSnapshot{{Path: "a.json", State: "disabled"}}}
	raw, _ := json.Marshal(doc)
	os.WriteFile(statusSnapshotFile, raw, 0600)
	event, _ := json.Marshal(accountLogEvent{At: time.Now().Unix(), Account: "a.json", State: "disabled"})
	log.Printf("[Account status] %s", event)
	handler := newWebUIHandler()
	for _, path := range []string{"/admin/api/account-alerts", "/admin/api/status"} {
		if got := adminRequest(handler, "GET", path, "", ""); got.Code != 401 {
			t.Fatalf("no auth status=%d", got.Code)
		}
	}
	got := adminRequest(handler, "GET", "/admin/api/account-alerts", "synthetic-health-admin", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"severity":"urgent"`) {
		t.Fatalf("%d %s", got.Code, got.Body.String())
	}
	got = adminRequest(handler, "POST", "/admin/api/account-alerts", "synthetic-health-admin", "{}")
	if got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("write accepted %d", got.Code)
	}
	got = adminRequest(handler, "GET", "/admin/api/status", "synthetic-health-admin", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"state":"unavailable"`) {
		t.Fatalf("%d %s", got.Code, got.Body.String())
	}
}

func TestAccountAlertMissingAndBoundedLogs(t *testing.T) {
	chdirTemp(t)
	scan, err := readAccountAlertLogs()
	if err != nil || len(scan.Events) != 0 {
		t.Fatal("missing logs must be empty, not a fabricated alert")
	}
	os.Mkdir(logDir, 0700)
	path := filepath.Join(logDir, "gateway-2026-10-14.log")
	content := strings.Repeat("x", int(accountAlertReadBudget)+100) + "\n2026/10/14 01:00:00 [Account status] {\"at\":123,\"account\":\"a.json\",\"state\":\"expired\"}\n"
	os.WriteFile(path, []byte(content), 0600)
	scan, err = readAccountAlertLogs()
	if err != nil || !scan.Truncated || scan.Scanned != accountAlertReadBudget || len(scan.Events) != 1 {
		t.Fatalf("%+v %v", scan, err)
	}
	// The handler itself must also reject writes; an unauthenticated test run must not be mistaken for read-only.
	rec := httptest.NewRecorder()
	handleWebUIAccountAlerts(rec, httptest.NewRequest("DELETE", "/admin/api/account-alerts", nil))
	if rec.Code != 405 {
		t.Fatal("DELETE must be rejected")
	}
}

func TestAccountAlertLogReadFailureDoesNotBreakHealth(t *testing.T) {
	chdirTemp(t)
	accountAlertCache.Lock()
	accountAlertCache.at = time.Time{}
	accountAlertCache.Unlock()
	t.Cleanup(func() { accountAlertCache.Lock(); accountAlertCache.at = time.Time{}; accountAlertCache.Unlock() })
	if err := os.WriteFile(logDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(statusSnapshot{UpdatedAt: time.Now().Unix(), Accounts: []accountSnapshot{{Path: "a.json", State: "active"}}})
	if err := os.WriteFile(statusSnapshotFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handleWebUIAccountAlerts(rec, httptest.NewRequest(http.MethodGet, "/admin/api/account-alerts", nil))
	if rec.Code != 503 {
		t.Fatalf("unreadable logs must not masquerade as an empty list: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleWebUIStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("logs failure broke health endpoint: %d", rec.Code)
	}
}

func TestReadWebLogAroundFocusesOffset(t *testing.T) {
	chdirTemp(t)
	if err := os.Mkdir(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	name := "gateway-2026-10-16.log"
	var body strings.Builder
	offset := int64(0)
	for i := 1; i <= 50; i++ {
		line := fmt.Sprintf("line-%02d\n", i)
		if i == 30 {
			offset = int64(body.Len())
		}
		body.WriteString(line)
	}
	if err := os.WriteFile(filepath.Join(logDir, name), []byte(body.String()), 0600); err != nil {
		t.Fatal(err)
	}
	lines, focus, resolved, err := readWebLogAround(name, offset, 64)
	if err != nil || !resolved {
		t.Fatalf("err=%v resolved=%t", err, resolved)
	}
	if focus < 0 || focus >= len(lines) || lines[focus] != "line-30" {
		t.Fatalf("focused the wrong line focus=%d lines=%v", focus, lines)
	}
	// An offset past the end of the file (logs rotated or cleaned) must not pretend to resolve.
	if _, _, resolved, err := readWebLogAround(name, 1<<20, 64); err != nil || resolved {
		t.Fatalf("out-of-range offset should give resolved=false: resolved=%t err=%v", resolved, err)
	}
}

func TestSnapshotAlertsPreferInlineAndHideRecovered(t *testing.T) {
	mark := accountAlertMark{State: "cooldown", At: 1792056948, LogFile: "gateway-2026-10-16.log", LogOffset: 12345}
	rows := []accountSnapshot{{Path: "a.json", State: "cooldown", Alerts: []accountAlertMark{mark}}}
	alerts := currentAlertsFromSnapshot(rows)
	if len(alerts) != 1 || alerts[0].LogOffset != 12345 || alerts[0].Severity != "warning" {
		t.Fatalf("snapshot inline alerts rendered wrong: %+v", alerts)
	}
	// A recovered account must not keep showing a stale snapshot mark.
	rows[0].State = "active"
	if got := currentAlertsFromSnapshot(rows); len(got) != 0 {
		t.Fatalf("must not be shown after recovery: %+v", got)
	}
}
