package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func taskByID(t *testing.T, rows []scheduledTaskView, id string) scheduledTaskView {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing task %s", id)
	return scheduledTaskView{}
}

func TestTaskLogAggregationNoFalseSuccessOrCrossProcess(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.ReloadInterval, cfg.ModelsRefresh, cfg.KeepaliveHours = 5, 60, []int{22}
	cfg.DisablePriceProbes = true
	now := time.Now()
	at := now.UnixMilli()
	pid := os.Getpid()
	events := []scheduledTaskEvent{
		{Task: "quota_scan", Phase: "started", Status: "running", Trace: "new", PID: pid, At: at - 1000, StartedAt: at - 1000, NextAt: at + 300000},
		{Task: "quota_scan", Phase: "completed", Status: "cancelled", Trace: "old", PID: pid, At: at, StartedAt: at - 10000, NextAt: at + 300000},
		{Task: "token_check", Phase: "completed", Status: "failed", PID: pid + 1, At: at, StartedAt: at - 1000},
		{Task: "config_reload", Phase: "completed", Status: "success", PID: pid, At: at - 5000, StartedAt: at - 5010, NextAt: at - 4000},
		{Task: "keepalive_catchup", Phase: "scheduled", PID: pid, At: at - 30000, NextAt: at - 10000},
		{Task: "keepalive_catchup", Phase: "completed", Status: "skipped", PID: pid, At: at - 9000, StartedAt: at - 9500},
		{Task: "daily_checkin", Phase: "scheduled", PID: pid, At: at - 30000, NextAt: at - 1000},
	}
	rows := aggregateTaskViews(events, now, pid, at-60000)
	if len(rows) != 11 {
		t.Fatalf("Task count=%d", len(rows))
	}
	quota := taskByID(t, rows, "quota_scan")
	if quota.Status != "running" || quota.Success != nil || quota.TraceID != "new" {
		t.Fatalf("Old round completion overwrote the new round: %+v", quota)
	}
	if row := taskByID(t, rows, "token_check"); row.Status != "unknown" || row.Success != nil || row.LastRunAt != 0 {
		t.Fatal("Failure record from an old process polluted current state")
	}
	if row := taskByID(t, rows, "config_reload"); row.Success == nil || !*row.Success || row.NextRunAt <= now.Unix() || !row.NextEstimated {
		t.Fatalf("Wrong periodic estimated time: %+v", row)
	}
	if row := taskByID(t, rows, "keepalive_catchup"); row.NextRunAt != 0 || row.Status != "skipped" || row.Success != nil {
		t.Fatal("One-shot catchup should not be rescheduled after completion")
	}
	if row := taskByID(t, rows, "daily_checkin"); row.NextRunAt != 0 || row.Success != nil {
		t.Fatal("Past schedule must not fabricate a new run or success")
	}
	if row := taskByID(t, rows, "price_probe"); row.Enabled || row.Status != "disabled" || row.NextRunAt != 0 {
		t.Fatal("Disabled price probe should not show a schedule")
	}
	beforeBoot := aggregateTaskViews(events, now, pid, at+1)
	if taskByID(t, beforeBoot, "quota_scan").LastRunAt != 0 {
		t.Fatal("Pre-startup records must not count as the current run")
	}
}

func TestReadOnlyTasksEndpointAndCredentialPageRemoved(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.ReloadInterval, cfg.ModelsRefresh = 5, 60
	now := time.Now()
	logScheduledTask("quota_scan", "completed", "partial", "task-fixture", now.Add(-time.Second), now.Add(5*time.Minute), "Two account queries failed")
	logScheduledTask("config_reload", "completed", "success", "check-fixture", now, now.Add(time.Second), "Check completed")
	handler := newWebUIHandler()
	if w := adminRequest(handler, "GET", "/admin/api/tasks", "feature-api-key", ""); w.Code != 401 {
		t.Fatal("Task log endpoint must require an admin key")
	}
	before, _ := os.ReadFile(runtimeConfigFile)
	w := adminRequest(handler, "GET", "/admin/api/tasks", "feature-admin-key", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var response struct {
		Tasks  []scheduledTaskView `json:"tasks"`
		Source string              `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	row := taskByID(t, response.Tasks, "quota_scan")
	if row.Status != "partial" || row.Success == nil || *row.Success || row.LastFinishedAt == 0 || row.LogFile == "" {
		t.Fatalf("Run result did not come from the log: %+v", row)
	}
	if w := adminRequest(handler, "POST", "/admin/api/tasks", "feature-admin-key", `{}`); w.Code != 405 {
		t.Fatal("Read-only tasks endpoint should not accept writes")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if string(before) != string(after) {
		t.Fatal("Reading tasks modified the config")
	}
	html, _ := webFS.ReadFile("web/index.html")
	js, _ := webFS.ReadFile("web/app.js")
	if strings.Contains(string(html), `data-tab="credentials"`) || strings.Contains(string(html), "credentialContent") || strings.Contains(string(js), "loadCredentials") {
		t.Fatal("Web UI still contains the credentials page or editor")
	}
	if !strings.Contains(string(html), "tasksTable") {
		t.Fatal("Tasks page is not embedded")
	}
}

func TestTaskLogMissingCorruptAndBudget(t *testing.T) {
	isolateAdminFeatures(t)
	path := filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("2026/10/11 00:00:00 [Scheduled task] broken json\n")
	_ = file.Close()
	if _, _, _, err := readTaskEvents(); err != nil {
		t.Fatal("Bad lines should be skipped, not reported as success or total failure")
	}
	large := filepath.Join(logDir, "gateway-2099-01-01.log")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(taskLogReadBudget + 100); err != nil {
		t.Fatal(err)
	}
	f.Close()
	events, size, truncated, err := readTaskEvents()
	if err != nil || size > taskLogReadBudget || !truncated || len(events) != 0 {
		t.Fatalf("Wrong budget or missing-file handling: records=%d size=%d truncated=%v err=%v", len(events), size, truncated, err)
	}
}

func TestTaskLoggingWorksWithoutDebugAndIsSampled(t *testing.T) {
	isolateAdminFeatures(t)
	if debugLoggingEnabled() {
		t.Fatal("Test should have debug disabled")
	}
	var last time.Time
	var previous string
	now := time.Now()
	logFrequentTask("credential_reload", "success", "Scan succeeded", now, now.Add(5*time.Second), &last, &previous, false)
	logFrequentTask("credential_reload", "success", "Scan succeeded", now, now.Add(5*time.Second), &last, &previous, false)
	logFrequentTask("credential_reload", "failed", "Scan failed", now, now.Add(5*time.Second), &last, &previous, false)
	events, _, _, err := readTaskEvents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Task == "credential_reload" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("Frequent successes should be merged and failures logged immediately, got=%d", count)
	}
}

func TestConfigTaskRecordsFailedValidation(t *testing.T) {
	isolateAdminFeatures(t)
	writeRuntimeFixture(t, `{`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtimeConfigLoop(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		events, _, _, _ := readTaskEvents()
		for _, event := range events {
			if event.Task == "config_reload" && event.Phase == "completed" && event.Status == "failed" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Config parse failure produced no task failure event")
}

func TestCredentialScanAndSnapshotErrorsReported(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthDir = filepath.Join(t.TempDir(), "missing")
	accounts = []*Account{{Path: "keep.json"}}
	if changed, err := reloadAccountsResult(); err == nil || changed || len(accounts) != 1 {
		t.Fatal("Directory failure should keep accounts and return an error")
	}
	accounts = nil
	if err := os.Mkdir(statusSnapshotFile+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusSnapshot(); err == nil {
		t.Fatal("Snapshot write error must not count as success")
	}
}
