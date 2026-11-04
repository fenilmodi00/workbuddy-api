package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WebUI is the admin console on a dedicated port: auth settings and account/model toggles allow controlled config writes.
// Toggles affect scheduling through the config allowlist/blocklist; they never delete credentials or change historical stats; credential tokens are always masked.
//
//go:embed web/index.html web/app.js web/style.css
var webFS embed.FS

const (
	webUIPrefix      = "/ui"
	webUIPrefixSlash = "/ui/"
)

// webuiEnabled reports whether the current process has the admin console enabled.
func webuiEnabled() bool { return cfg.WebUI }

// The admin Key can be initialized on first local access; it can never force-enable model API auth.
func webUIPreflightError() error {
	if cfg.WebUI && cfg.Port == cfg.WebPort {
		return errors.New("API and Web ports must not be the same")
	}
	return nil
}

// webUIIsExemptPath reports whether the path belongs to the "data-free static shell" and may be accessed without an API key.
// Only static assets under /ui and /ui/ are exempt from auth; /admin/api/* always requires a key.
func webUIIsExemptPath(p string) bool {
	if !webuiEnabled() {
		return false
	}
	return p == webUIPrefix || p == webUIPrefixSlash || strings.HasPrefix(p, webUIPrefixSlash)
}

// registerWebUIRoutes registers static assets, read-only data, and auth-protected config write routes.
func registerWebUIRoutes(mux *http.ServeMux) {
	mux.HandleFunc(webUIPrefix, handleWebUIIndex)
	mux.HandleFunc(webUIPrefixSlash, handleWebUIIndex)
	mux.HandleFunc("/admin/api/status", handleWebUIStatus)
	mux.HandleFunc("/admin/api/models", handleWebUIModels)
	mux.HandleFunc("/admin/api/logs", handleWebUILogs)
	mux.HandleFunc("/admin/api/config", handleWebUIConfig)
	mux.HandleFunc("/admin/api/config-edit", handleWebUIConfigEdit)
	mux.HandleFunc("/admin/api/credentials", handleWebUICredentials)
	mux.HandleFunc("/admin/api/setup", handleWebUISetup)
	mux.HandleFunc("/admin/api/settings", handleWebUISettings)
	mux.HandleFunc("/admin/api/client-events", handleWebUIClientEvent)
	mux.HandleFunc("/admin/api/account-toggle", handleWebUIAccountToggle)
	mux.HandleFunc("/admin/api/model-toggle", handleWebUIModelToggle)
	mux.HandleFunc("/admin/api/credential-edit", handleWebUICredentialEdit)
	mux.HandleFunc("/admin/api/log-settings", handleWebUILogSettings)
	mux.HandleFunc("/admin/api/log-cleanup", handleWebUILogCleanup)
	mux.HandleFunc("/admin/api/tasks", handleWebUITasks)
	mux.HandleFunc("/admin/api/rate-detail", handleWebUIRateDetail)
	mux.HandleFunc("/admin/api/stats", handleWebUIStats)
	mux.HandleFunc("/admin/api/account-alerts", handleWebUIAccountAlerts)
	mux.HandleFunc("/admin/api/account-add", handleWebUIAccountAdd)
	mux.HandleFunc("/admin/api/account-cancel", handleWebUIAccountCancel)
	mux.HandleFunc("/admin/api/account-delete", handleWebUIAccountDelete)
}

// Read-only log view: no extra scheduler, task status file, or in-memory task registry.
type scheduledTaskEvent struct {
	Task      string `json:"task"`
	Phase     string `json:"phase"`
	Status    string `json:"status"`
	Trace     string `json:"trace"`
	PID       int    `json:"pid"`
	At        int64  `json:"at"`
	StartedAt int64  `json:"startedAt"`
	NextAt    int64  `json:"nextAt"`
	Detail    string `json:"detail"`
	File      string `json:"-"`
}

type scheduledTaskView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Schedule        string `json:"schedule"`
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"intervalSeconds,omitempty"`
	OneShot         bool   `json:"oneShot,omitempty"`
	LastRunAt       int64  `json:"lastRunAt,omitempty"`
	LastFinishedAt  int64  `json:"lastFinishedAt,omitempty"`
	NextRunAt       int64  `json:"nextRunAt,omitempty"`
	NextEstimated   bool   `json:"nextEstimated"`
	Status          string `json:"status"`
	Success         *bool  `json:"success"`
	Detail          string `json:"detail,omitempty"`
	LogFile         string `json:"logFile,omitempty"`
	TraceID         string `json:"traceId,omitempty"`
}

var taskLogBootAt = time.Now().UnixMilli()

const taskLogReadBudget int64 = 16 << 20

func taskViewDefinitions() []scheduledTaskView {
	keepaliveEnabled := len(cfg.KeepaliveHours) > 0
	return []scheduledTaskView{
		{ID: "config_reload", Name: "Config hot reload", Schedule: "every 1s; high-frequency logs aggregated every 60s", Enabled: true, IntervalSeconds: 1},
		{ID: "credential_reload", Name: "Credential file scan", Schedule: fmt.Sprintf("every %ds; high-frequency logs aggregated every 60s", cfg.ReloadInterval), Enabled: cfg.ReloadInterval > 0, IntervalSeconds: int64(cfg.ReloadInterval)},
		{ID: "status_snapshot", Name: "Status snapshot write", Schedule: "every 3s; high-frequency logs aggregated every 60s", Enabled: true, IntervalSeconds: 3},
		{ID: "token_check", Name: "Token validity check", Schedule: "every 5 minutes; renews only when close to expiry", Enabled: true, IntervalSeconds: 300},
		{ID: "keepalive", Name: "Proactive renewal keepalive", Schedule: "daily " + formatKeepaliveHours(cfg.KeepaliveHours) + " (server local time)", Enabled: keepaliveEnabled},
		{ID: "keepalive_catchup", Name: "Startup renewal catch-up", Schedule: "90s after startup; only handles long-unrenewed accounts", Enabled: keepaliveEnabled, OneShot: true},
		{ID: "quota_scan", Name: "Quota and plan query", Schedule: "every 5 minutes; startup/events may trigger earlier", Enabled: true, IntervalSeconds: 300},
		{ID: "daily_checkin", Name: "Daily checkin", Schedule: "random time each morning", Enabled: true},
		{ID: "model_catalog", Name: "Model catalog refresh", Schedule: fmt.Sprintf("every %d minutes; startup/events may trigger earlier", cfg.ModelsRefresh), Enabled: cfg.ModelsRefresh > 0, IntervalSeconds: int64(cfg.ModelsRefresh) * 60},
		{ID: "price_probe", Name: "Model price probe", Schedule: "enabled only with -allow-price-probes; 20s after startup; then 2 or 30 minutes; 12-hour per-model limit", Enabled: cfg.automaticPriceProbesAllowed()},
		{ID: "log_cleanup", Name: "Expired log cleanup", Schedule: fmt.Sprintf("hourly; retains %d days, runs early when rules change", logRetentionDays.Load()), Enabled: logRetentionDays.Load() > 0, IntervalSeconds: 3600},
	}
}

func aggregateTaskViews(events []scheduledTaskEvent, now time.Time, pid int, bootAt int64) []scheduledTaskView {
	rows := taskViewDefinitions()
	for i := range rows {
		row := &rows[i]
		row.Status = "unknown"
		var execution, schedule scheduledTaskEvent
		for _, event := range events {
			if event.Task != row.ID || event.PID != pid || event.At < bootAt || event.At > now.Add(time.Minute).UnixMilli() {
				continue
			}
			if event.Phase == "scheduled" || event.Phase == "disabled" || event.NextAt > 0 {
				if event.At >= schedule.At {
					schedule = event
				}
			}
			if event.Phase != "started" && event.Phase != "completed" {
				continue
			}
			oldStart := execution.StartedAt
			if oldStart == 0 {
				oldStart = execution.At
			}
			newStart := event.StartedAt
			if newStart == 0 {
				newStart = event.At
			}
			// A later finish of an old round must not overwrite an already-started new round.
			if newStart > oldStart || newStart == oldStart && event.At >= execution.At {
				execution = event
			}
		}
		if execution.At > 0 {
			row.LastRunAt = execution.StartedAt / 1000
			if row.LastRunAt == 0 {
				row.LastRunAt = execution.At / 1000
			}
			row.Status, row.Detail, row.LogFile, row.TraceID = execution.Status, execution.Detail, execution.File, execution.Trace
			if execution.Phase == "completed" {
				row.LastFinishedAt = execution.At / 1000
			}
			switch row.Status {
			case "success":
				value := true
				row.Success = &value
			case "failed", "partial", "cancelled":
				value := false
				row.Success = &value
			}
		} else if !row.Enabled {
			row.Status = "disabled"
			row.Detail = "Task disabled by current config"
		}
		if row.Enabled && schedule.NextAt > 0 && schedule.Phase != "disabled" {
			next := schedule.NextAt
			if row.OneShot && execution.At > 0 {
				next = 0
			} else if next <= now.UnixMilli() && row.IntervalSeconds > 0 {
				step := row.IntervalSeconds * 1000
				next += ((now.UnixMilli()-next)/step + 1) * step
				row.NextEstimated = true
			} else if next <= now.UnixMilli() {
				next = 0
				row.Detail += "; scheduled time has passed, waiting for new schedule logs"
			}
			row.NextRunAt = next / 1000
		}
	}
	return rows
}

func readTaskEvents() ([]scheduledTaskEvent, int64, bool, error) {
	entries, err := os.ReadDir(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "gateway-") && logFilePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, 0, false, err
	}
	defer root.Close()
	var events []scheduledTaskEvent
	var scanned int64
	truncated := false
	for index, name := range names {
		budget := taskLogReadBudget - scanned
		if budget <= 0 {
			truncated = true
			break
		}
		f, err := root.Open(name)
		if err != nil {
			return nil, scanned, true, err
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			return nil, scanned, true, errors.New("task log is not a readable regular file")
		}
		size := min(st.Size(), budget)
		data := make([]byte, int(size))
		start := st.Size() - size
		count, readErr := f.ReadAt(data, start)
		f.Close()
		if readErr != nil && readErr != io.EOF {
			return nil, scanned, true, readErr
		}
		if int64(count) != size {
			return nil, scanned, true, errors.New("task log was truncated during read, please retry")
		}
		scanned += size
		if start > 0 {
			truncated = true
			if end := bytes.IndexByte(data, '\n'); end >= 0 {
				data = data[end+1:]
			} else {
				data = nil
			}
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			marker := bytes.Index(line, []byte("[Scheduled task] "))
			markerLen := len("[Scheduled task] ")
			if marker < 0 {
				// Legacy Chinese tag from logs written before the migration.
				marker = bytes.Index(line, []byte("[\u5b9a\u65f6\u4efb\u52a1] "))
				markerLen = len("[\u5b9a\u65f6\u4efb\u52a1] ")
			}
			if marker < 0 {
				continue
			}
			var event scheduledTaskEvent
			if json.Unmarshal(line[marker+markerLen:], &event) != nil || event.Task == "" || event.At <= 0 {
				continue
			}
			event.File = name
			events = append(events, event)
		}
		if scanned >= taskLogReadBudget && index < len(names)-1 {
			truncated = true
		}
	}
	return events, scanned, truncated, nil
}

// -----------------------------------------------------------------------------
// Rate sources (read-only log view)
//
// Same source as the scheduled-tasks page: no new scheduler, status file, or
// ledger; everything is parsed from the existing normal logs.
// Models with no log records show "No logs" on the page directly; sources are never guessed.
// -----------------------------------------------------------------------------

type rateLogRecord struct {
	Site      string `json:"site"`
	Model     string `json:"model"`
	Source    string `json:"source"`
	Explain   string `json:"explain"`
	UpdatedAt int64  `json:"updatedAt"`
	Value     string `json:"value"`
	Detail    string `json:"detail"`
	Unknown   bool   `json:"unknown"`
	Priority  int    `json:"priority"`
	LogFile   string `json:"logFile"`
	LogLine   string `json:"logLine"`
}

// Priority order matches modelDisplayMultiplier's evaluation order, ensuring the "exclamation mark" explains the value the console actually displays.
const (
	ratePriorityLearnedFree = 1
	ratePriorityProbeFree   = 2
	ratePriorityProbePaid   = 3
	ratePriorityPromoStale  = 4
	ratePriorityOfficial    = 6
	// Live-request "paid" verdicts do not participate in value selection (the page still shows the official multiplier); they are listed only as corroboration.
	ratePriorityLearnedPaid = 7
)

var (
	rateOfficialRe   = regexp.MustCompile(`\[Models\] official multiplier site=(\S+) model=(\S+) rawCredits="([^"]*)" effectiveMultiplier=(\S+) hasMultiplier=(\S+) promo="([^"]*)" promoEnds="([^"]*)" promoExpired=(\S+) source=(.*?) refreshedAt=(\S+)`)
	rateProbeCacheRe = regexp.MustCompile(`\[ModelPrice\] cached verdict site=(\S+) model=(\S+) verdict=(\S+) credit=(\S+) tokens=(\d+) probeTime=(\S+) note=(.*?) source=cache file`)
	rateProbeLiveRe  = regexp.MustCompile(`\[ModelPrice\] site=(\S+) account=(\S+) model=(\S+) -> verdict=(\S+) credit=(\S+) tokens=(\d+) \((.*)\)\s*$`)
	rateLearnedRe    = regexp.MustCompile(`\[ModelCost\] requestId=\d+ account (\S+) site=(\S+) model (\S+) live usage\.credit=(\S+?), learned as (free|paid) model`)
	rateProbeModeRe  = regexp.MustCompile(`\[ModelPrice\] .*stage=auto probe scheduling .*result=(disabled|enabled)`)
)

const rateLogReadBudget int64 = 16 << 20

// rateMaxOtherRecords caps how many "other records" are listed, avoiding repeated spam of the same conclusion.
const rateMaxOtherRecords = 4

func logLineTime(line string) int64 {
	if len(line) < 19 {
		return 0
	}
	if at, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local); err == nil {
		return at.Unix()
	}
	return 0
}

func probeValue(verdict string) (string, bool, int) {
	switch verdict {
	case "free":
		return "0.00x", false, ratePriorityProbeFree
	case "paid":
		return "Paid (unknown multiplier)", true, ratePriorityProbePaid
	default:
		return "-", true, ratePriorityProbePaid
	}
}

func parseRateLine(line, file string) (rateLogRecord, bool) {
	// Use three substring checks first to reject the vast majority of irrelevant lines: the
	// redaction regex and time parsing are expensive and cannot run on every line, or the
	// endpoint would exceed the caller's timeout on large logs.
	if !strings.Contains(line, "[Models] official multiplier") &&
		!strings.Contains(line, "[ModelPrice]") &&
		!strings.Contains(line, "[ModelCost]") {
		return rateLogRecord{}, false
	}
	at := logLineTime(line)
	if at == 0 {
		return rateLogRecord{}, false
	}
	base := rateLogRecord{UpdatedAt: at, LogFile: file}
	if m := rateOfficialRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[1], m[2]
		record.Source = "Official catalog (" + m[9] + ")"
		expired := m[8] == "true"
		switch {
		case m[5] != "true":
			record.Value, record.Unknown, record.Priority = "-", true, ratePriorityOfficial
			record.Detail = "Official catalog gave no valid multiplier, raw credits=" + m[3]
			record.Explain = "Official API gave no valid multiplier"
		case expired:
			record.Value, record.Unknown, record.Priority = "-", true, ratePriorityPromoStale
			record.Detail = "Official promo expired, raw credits=" + m[3] + ", promo=" + m[6] + ", ends=" + m[7] + "; value not trustworthy"
			record.Explain = "Promo expired; the value is untrustworthy"
		default:
			record.Value, record.Priority = m[4]+"x", ratePriorityOfficial
			record.Detail = "Official catalog raw credits=" + m[3]
			record.Explain = "Catalog multiplier given by the official API"
			if m[6] != "" {
				record.Detail += ", promo=" + m[6] + ", ends=" + m[7]
				record.Explain = "Derived from promo " + m[6]
			}
		}
		return record, true
	}
	if m := rateProbeCacheRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[1], m[2]
		record.Source = "Price probe (cached verdict)"
		record.Value, record.Unknown, record.Priority = probeValue(m[3])
		record.Detail = "verdict=" + m[3] + " credit=" + m[4] + " tokens=" + m[5] + " probeTime=" + m[6] + " " + m[7]
		record.Explain = "Previous probe verdict: paid (not retested)"
		if m[3] == "free" {
			record.Explain = "Previous probe verdict: free (not retested)"
		}
		return record, true
	}
	if m := rateProbeLiveRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[1], m[3]
		record.Source = "Price probe (local measurement)"
		record.Value, record.Unknown, record.Priority = probeValue(m[4])
		record.Detail = "account=" + m[2] + " verdict=" + m[4] + " credit=" + m[5] + " tokens=" + m[6] + " " + m[7]
		record.Explain = "Gateway probe: paid (no official multiplier)"
		if m[4] == "free" {
			record.Explain = "Gateway probe: free"
		}
		return record, true
	}
	if m := rateLearnedRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[2], m[3]
		record.Source = "Normal request measurement"
		record.Detail = "account=" + m[1] + " usage.credit=" + m[4] + " verdict=" + m[5]
		if m[5] == "free" {
			record.Value, record.Priority = "0.00x", ratePriorityLearnedFree
			record.Explain = "Client request measured free (credit=0)"
		} else {
			record.Value, record.Priority = "Measured paid", ratePriorityLearnedPaid
			record.Explain = "Measured paid; does not change the official rate"
		}
		return record, true
	}
	return rateLogRecord{}, false
}

type rateLogScan struct {
	Records          []rateLogRecord
	AutoProbeEnabled bool
	AutoProbeKnown   bool
	Scanned          int64
	Truncated        bool
}

// readRateEvents parses rate-source records from the tail of existing normal logs; it reads no debug logs and changes no files.
func readRateEvents() (rateLogScan, error) {
	var scan rateLogScan
	entries, err := os.ReadDir(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "gateway-") && logFilePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return scan, err
	}
	defer root.Close()
	for index, name := range names {
		budget := rateLogReadBudget - scan.Scanned
		if budget <= 0 {
			scan.Truncated = true
			break
		}
		f, err := root.Open(name)
		if err != nil {
			scan.Truncated = true
			return scan, err
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			scan.Truncated = true
			return scan, errors.New("rate log is not a readable regular file")
		}
		size := min(st.Size(), budget)
		data := make([]byte, int(size))
		start := st.Size() - size
		count, readErr := f.ReadAt(data, start)
		f.Close()
		if readErr != nil && readErr != io.EOF {
			scan.Truncated = true
			return scan, readErr
		}
		if int64(count) != size {
			scan.Truncated = true
			return scan, errors.New("rate log was truncated during read, please retry")
		}
		scan.Scanned += size
		if start > 0 {
			scan.Truncated = true
			if end := bytes.IndexByte(data, '\n'); end >= 0 {
				data = data[end+1:]
			} else {
				data = nil
			}
		}
		for _, raw := range bytes.Split(data, []byte{'\n'}) {
			if len(raw) == 0 {
				continue
			}
			line := string(raw)
			// Probe scheduling state: tells the user how to refresh when the value is
			// "Unknown"; also taken from the normal logs.
			if !scan.AutoProbeKnown && strings.Contains(line, "stage=auto probe scheduling") {
				if m := rateProbeModeRe.FindStringSubmatch(line); m != nil {
					scan.AutoProbeEnabled, scan.AutoProbeKnown = m[1] == "enabled", true
				}
			}
			if record, ok := parseRateLine(line, name); ok {
				scan.Records = append(scan.Records, record)
			}
		}
		if scan.Scanned >= rateLogReadBudget && index < len(names)-1 {
			scan.Truncated = true
		}
	}
	return scan, nil
}

func aggregateRateDetails(records []rateLogRecord) map[string]map[string]any {
	grouped := map[string][]rateLogRecord{}
	for _, record := range records {
		key := record.Site + "|" + normalizeModelName(record.Model)
		grouped[key] = append(grouped[key], record)
	}
	out := make(map[string]map[string]any, len(grouped))
	for key, list := range grouped {
		sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
		bestIndex := 0
		for i, record := range list {
			if i == 0 {
				continue
			}
			if record.Priority < list[bestIndex].Priority ||
				record.Priority == list[bestIndex].Priority && record.UpdatedAt > list[bestIndex].UpdatedAt {
				bestIndex = i
			}
		}
		best := list[bestIndex]
		// "Other records" are deduplicated by source+explanation (multiple restarts can write
		// identical records), and only the latest few are kept, so the user is not flooded
		// with the same sentence repeatedly.
		others := make([]rateLogRecord, 0, len(list)-1)
		seenOthers := map[string]bool{}
		// Register the winning record's signature as seen first, so older records with the
		// same conclusion do not reappear in "other records".
		seenOthers[best.Source+"|"+best.Explain+"|"+best.Value] = true
		for i, record := range list {
			if i != bestIndex {
				signature := record.Source + "|" + record.Explain + "|" + record.Value
				if seenOthers[signature] {
					continue
				}
				seenOthers[signature] = true
				others = append(others, record)
			}
		}
		if len(others) > rateMaxOtherRecords {
			others = others[:rateMaxOtherRecords]
		}
		out[key] = map[string]any{
			"site": best.Site, "model": best.Model, "source": best.Source,
			"updatedAt": best.UpdatedAt, "value": best.Value, "detail": best.Detail,
			"explain": best.Explain,
			"unknown": best.Unknown, "priority": best.Priority,
			"logFile": best.LogFile, "logLine": best.LogLine,
			"hasLog": true, "records": list, "others": others,
		}
	}
	return out
}

// Briefly caches scan results: fetched on demand when clicking the "exclamation mark", avoiding a full log rescan on every click.
var rateDetailCache struct {
	sync.Mutex
	at      time.Time
	payload map[string]any
}

const rateDetailCacheTTL = 20 * time.Second

func handleWebUIRateDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Read-only data; only GET is supported")
		return
	}
	rateDetailCache.Lock()
	if rateDetailCache.payload != nil && time.Since(rateDetailCache.at) < rateDetailCacheTTL {
		payload := rateDetailCache.payload
		rateDetailCache.Unlock()
		log.Printf("[Admin rate sources] traceId=%s result=cache hit note=reads existing normal logs only; does not modify config or trigger probes", debugTraceID(r))
		writeAdminData(w, r, payload)
		return
	}
	rateDetailCache.Unlock()
	scan, err := readRateEvents()
	if err != nil {
		log.Printf("[Admin rate sources] traceId=%s result=read failed reason=%s note=page shows no logs; sources are never fabricated", debugTraceID(r), safeDebugError(err))
		writeJSONError(w, http.StatusServiceUnavailable, "Logs temporarily unreadable, please retry later")
		return
	}
	now := time.Now()
	rates := aggregateRateDetails(scan.Records)
	refreshHint := rateRefreshHint(scan.AutoProbeEnabled)
	for _, entry := range rates {
		if entry["unknown"] == true {
			entry["refreshHint"] = refreshHint
		}
	}
	payload := map[string]any{
		"generatedAt": now.Unix(), "timezone": now.Format("MST -07:00"),
		"rates": rates, "recordCount": len(scan.Records),
		"readBytes": scan.Scanned, "readBudgetBytes": rateLogReadBudget, "truncated": scan.Truncated,
		"autoProbeEnabled": scan.AutoProbeEnabled, "autoProbeKnown": scan.AutoProbeKnown,
		"refreshHint": refreshHint,
		"noLogHint":   "No records for this model in the normal logs. Request this model once with an account of that site and a new rate record will be produced.",
		"source":      "gateway normal logs",
		"note":        "Rate sources and update times all come from the normal logs; models without log records show \"no logs\". Catalog multipliers are recorded on first sight or on change; probe and live-request verdicts are recorded when produced; raw logs are collapsed by default.",
	}
	rateDetailCache.Lock()
	rateDetailCache.payload, rateDetailCache.at = payload, now
	rateDetailCache.Unlock()
	log.Printf("[Admin rate sources] traceId=%s result=read success records=%d siteModels=%d scannedBytes=%d truncated=%t autoProbeEnabled=%t note=reads existing normal logs only; does not trigger probes or catalog refresh", debugTraceID(r), len(scan.Records), len(rates), scan.Scanned, scan.Truncated, scan.AutoProbeEnabled)
	writeAdminData(w, r, payload)
}

// rateRefreshHint explains how to refresh a rate when its value is "Unknown"; the probe switch state also comes from the normal logs.
func rateRefreshHint(autoProbeEnabled bool) string {
	if autoProbeEnabled {
		return "Background auto-probing is enabled and will automatically re-probe in the next round; you can also request this model once with an account of that site to re-judge immediately from usage.credit."
	}
	return "Background auto-probing is disabled. Request this model once with an account of that site and the gateway will automatically re-judge from the returned usage.credit and write it to the logs."
}

func handleWebUITasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Read-only page; only GET is supported")
		return
	}
	events, scanned, truncated, err := readTaskEvents()
	if err != nil {
		log.Printf("[Admin scheduled tasks] traceId=%s result=read failed reason=%s note=keeps the page's previous data; does not fake task success", debugTraceID(r), safeDebugError(err))
		writeJSONError(w, http.StatusServiceUnavailable, "Task logs temporarily unreadable, please retry later")
		return
	}
	now := time.Now()
	rows := aggregateTaskViews(events, now, os.Getpid(), taskLogBootAt)
	log.Printf("[Admin scheduled tasks] traceId=%s result=read success tasks=%d logRecords=%d scannedBytes=%d truncated=%t note=reads existing logs only; does not trigger tasks or modify scheduling", debugTraceID(r), len(rows), len(events), scanned, truncated)
	writeAdminData(w, r, map[string]any{"tasks": rows, "generatedAt": now.Unix(), "timezone": now.Format("MST -07:00"), "readBytes": scanned, "readBudgetBytes": taskLogReadBudget, "truncated": truncated, "source": "gateway logs; current service process only", "note": "High-frequency local tasks record their most recent execution every 60 seconds; status changes are recorded immediately. The next run time is extrapolated from the schedule in the logs; event triggers may fire earlier. No logs means success is never assumed."})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleWebUIIndex serves the admin console static shell and static assets.
// /ui redirects to /ui/ so relative asset paths resolve correctly; /ui/<file> reads the embedded asset by name.
func handleWebUIIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET/HEAD are supported")
		return
	}
	if r.URL.Path == webUIPrefix {
		http.Redirect(w, r, webUIPrefixSlash, http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, webUIPrefixSlash)
	if name == "" {
		name = "index.html"
	}
	serveWebAsset(w, name)
}

// serveWebAsset reads a single static file from the embedded assets and rejects any path traversal.
func serveWebAsset(w http.ResponseWriter, name string) {
	name = path.Clean(name)
	if name == "." || strings.HasPrefix(name, "..") || strings.Contains(name, "/") {
		http.NotFound(w, nil)
		return
	}
	data, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	_, _ = w.Write(data)
}

// webGatewayMeta is the gateway runtime metadata shown by the admin console (contains no secrets).
type webGatewayMeta struct {
	Version       string `json:"version"`
	Listen        string `json:"listen"`
	APIKeyEnabled bool   `json:"apiKeyEnabled"`
	WebUIEnabled  bool   `json:"webuiEnabled"`
	ModelSource   string `json:"modelSource,omitempty"`
	WebListen     string `json:"webListen,omitempty"`
}

// webStatusResponse is the response body of /admin/api/status.
type webStatusResponse struct {
	Gateway     webGatewayMeta            `json:"gateway"`
	UpdatedAt   int64                     `json:"updatedAt"`
	Summary     map[string]int            `json:"summary"`
	SiteSummary map[string]map[string]int `json:"siteSummary"`
	Accounts    []accountSnapshot         `json:"accounts"`
	Models      []modelStatSnapshot       `json:"models"`
	Stale       bool                      `json:"stale"`
	Health      accountHealth             `json:"health"`
}

// readStatusSnapshot reads the status snapshot written periodically by the serve cycle (same data source as the monitor command).
func readStatusSnapshot() (statusSnapshot, error) {
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		return statusSnapshot{}, err
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return statusSnapshot{}, err
	}
	if snap.UpdatedAt <= 0 {
		return statusSnapshot{}, errors.New("status snapshot has no valid update time")
	}
	return snap, nil
}

func summarizeAccounts(accs []accountSnapshot) map[string]int {
	summary := map[string]int{
		"total": len(accs), "active": 0, "cooldown": 0,
		"paidExhausted": 0, "expired": 0, "disabled": 0, "paused": 0,
	}
	for _, a := range accs {
		if a.SchedulingEnabled != nil && !*a.SchedulingEnabled {
			summary["paused"]++
			continue
		}
		switch a.State {
		case "active":
			summary["active"]++
		case "cooldown":
			summary["cooldown"]++
		case "quota_exhausted", "paid_exhausted":
			summary["paidExhausted"]++
		case "expired":
			summary["expired"]++
		case "disabled":
			summary["disabled"]++
		}
	}
	return summary
}

// summarizeAccountsBySite aggregates account counts per site (cn/intl) for the overview page's per-site display.
// Accounts missing a site marker are treated as domestic-site, consistent with the credentials' default site.
func summarizeAccountsBySite(accs []accountSnapshot) map[string]map[string]int {
	sites := map[string][]accountSnapshot{}
	for _, a := range accs {
		key := a.Edition
		if key != "intl" {
			key = "cn"
		}
		sites[key] = append(sites[key], a)
	}
	return map[string]map[string]int{
		"cn":   summarizeAccounts(sites["cn"]),
		"intl": summarizeAccounts(sites["intl"]),
	}
}

func handleWebUIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET is supported")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	apiPort, webPort := currentGatewayPorts()
	resp := webStatusResponse{
		Gateway: webGatewayMeta{
			Version:       version,
			Listen:        net.JoinHostPort(cfg.Addr, strconv.Itoa(apiPort)),
			WebListen:     net.JoinHostPort(cfg.Addr, strconv.Itoa(webPort)),
			APIKeyEnabled: currentAPIKey() != "",
			WebUIEnabled:  webuiEnabled(),
			ModelSource:   modelSourceLabel(modelSourceFromSnapshots(snap.Models)),
		},
		UpdatedAt: snap.UpdatedAt,
		Accounts:  snap.Accounts,
		Models:    snap.Models,
		Stale:     time.Now().Unix()-snap.UpdatedAt > 15,
	}
	if resp.Accounts == nil {
		resp.Accounts = []accountSnapshot{}
	}
	annotateAccountPolicies(resp.Accounts)
	if resp.Models == nil {
		resp.Models = []modelStatSnapshot{}
	}
	resp.Summary = summarizeAccounts(resp.Accounts)
	resp.SiteSummary = summarizeAccountsBySite(resp.Accounts)
	resp.Health = summarizeAccountHealth(resp.Accounts, resp.Stale)
	log.Printf("[Admin status] traceId=%s result=read success accounts=%d models=%d snapshotStale=%t note=displays the snapshot only; does not call upstream or change the ledger", debugTraceID(r), len(resp.Accounts), len(resp.Models), resp.Stale)
	writeAdminData(w, r, resp)
}

func handleWebUIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET is supported")
		return
	}
	rangeParam := strings.TrimSpace(r.URL.Query().Get("range"))
	if rangeParam != "" && rangeParam != "5h" {
		writeJSONError(w, http.StatusBadRequest, "range must be 5h")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	models := snap.ModelCatalog
	if models == nil {
		models = snap.Models // Backward compatibility with older status snapshots.
	}
	if models == nil {
		models = []modelStatSnapshot{}
	}
	for i := range models {
		disabled, reason := modelDisabled(models[i].ID)
		enabled := !disabled
		models[i].Enabled, models[i].PolicyReason = &enabled, reason
		if disabled {
			models[i].AvailableAccounts = 0
		}
	}
	if rangeParam == "5h" {
		now := time.Now()
		window := aggregateModelStatsWindow(now.Add(-5*time.Hour), now)
		for i := range models {
			// Requests / Tokens / first-token / total latency now uniformly read the last
			// 5-hour window of stats.json; the model catalog, enable/disable state,
			// multipliers, available accounts, and last status still come from the snapshot.
			models[i].Requests, models[i].Success, models[i].Failed, models[i].Tokens = 0, 0, 0, 0
			models[i].AvgTTFTMs, models[i].HasTTFT, models[i].TTFTMetric = 0, false, ""
			models[i].AvgLatencyMs, models[i].HasLatency = 0, false
			row, ok := window[models[i].ID]
			if !ok {
				continue
			}
			models[i].Requests, models[i].Success, models[i].Failed, models[i].Tokens = row.Requests, row.Success, row.Failed, row.Tokens
			if row.AvgTTFTMS != nil {
				models[i].AvgTTFTMs = int64(*row.AvgTTFTMS)
				models[i].HasTTFT = true
				models[i].TTFTMetric = ttftMetric
			}
			if row.AvgLatencyMS != nil {
				models[i].AvgLatencyMs = int64(*row.AvgLatencyMS)
				models[i].HasLatency = true
			}
		}
	}
	log.Printf("[Admin models] traceId=%s result=read success models=%d", debugTraceID(r), len(models))
	writeAdminData(w, r, map[string]any{
		"source":     modelSourceLabel(modelSourceFromSnapshots(models)),
		"models":     models,
		"stale":      time.Now().Unix()-snap.UpdatedAt > 15,
		"policyMode": currentModelPolicyMode(),
		"range":      rangeParam,
	})
}

func webSnapshotUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[Admin status] traceId=%s stage=read snapshot result=failed reason=%v statusCode=503 businessImpact=frontend keeps previous data; does not fake a zero-account state", debugTraceID(r), err)
	writeJSONError(w, http.StatusServiceUnavailable, "Status snapshot temporarily unavailable, please retry later; the existing page data is not a fresh zero-account result")
}

var logFilePattern = managedLogPattern

type logFileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

// listLogFiles lists only log files with restricted naming under logs/, newest first.
func listLogFiles() []logFileInfo {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return []logFileInfo{}
	}
	files := make([]logFileInfo, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || !logFilePattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, logFileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files
}

func handleWebUILogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET is supported")
		return
	}
	files := listLogFiles()
	name := r.URL.Query().Get("file")
	if name != "" && (baseNameSafe(name) != name || !logFilePattern.MatchString(name)) {
		log.Printf("[Admin logs] traceId=%s result=rejected reason=invalid log file name statusCode=400", debugTraceID(r))
		writeJSONError(w, http.StatusBadRequest, "Invalid log file name")
		return
	}
	if name == "" && len(files) > 0 {
		name = files[0].Name
	}

	linesN := 200
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > 2000 {
				n = 2000
			}
			linesN = n
		}
	}

	resp := map[string]any{"files": files, "file": name, "lines": []string{}, "truncated": false, "readBudgetBytes": webUILogReadBudget}
	if name != "" {
		log.Printf("[Admin logs] traceId=%s stage=read file=%s requestedLines=%d scannedByteBudget=%d", debugTraceID(r), name, linesN, webUILogReadBudget)
		var lines []string
		var truncated bool
		var err error
		// With an offset, return the window located by the alert's byte offset; otherwise keep reading the file tail.
		if focus := parseLogOffset(r.URL.Query().Get("offset")); focus >= 0 {
			var focusIndex int
			var resolved bool
			lines, focusIndex, resolved, err = readWebLogAround(name, focus, 64<<10)
			if err == nil {
				resp["focusIndex"], resp["resolved"] = focusIndex, resolved
			}
		} else {
			lines, truncated, err = readWebLog(name, linesN)
		}
		if err != nil {
			log.Printf("[Admin logs] traceId=%s result=failed file=%s reason=%v statusCode=503", debugTraceID(r), name, err)
			writeJSONError(w, http.StatusServiceUnavailable, "Logs temporarily unreadable, please refresh the file list and retry")
			return
		}
		// Redact a full copy of the lines, covering logs written by older versions; files and business errors are untouched.
		if len(lines) > 0 {
			// Build the secret-value replacer once per log batch, avoiding repeated construction line by line.
			lines = strings.Split(redactSensitiveText(strings.Join(lines, "\n")), "\n")
		}
		resp["lines"], resp["truncated"] = lines, truncated
		log.Printf("[Admin logs] traceId=%s result=success returnedLines=%d budgetTruncated=%t secretsHidden=true", debugTraceID(r), len(lines), truncated)
	}
	writeJSON(w, http.StatusOK, resp)
}

func readWebLog(name string, lines int) ([]string, bool, error) {
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	return readTailLines(f, lines, webUILogReadBudget)
}

// baseNameSafe takes only the base name, keeping any directory-traversal intent out of the following validation.
func baseNameSafe(name string) string {
	name = strings.TrimSpace(name)
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	return name
}

func handleWebUIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET is supported")
		return
	}
	runtimeConfigMu.Lock()
	data, err := os.ReadFile(runtimeConfigFile)
	runtimeConfigMu.Unlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"exists": false, "path": runtimeConfigFile, "content": ""})
		} else {
			log.Printf("[Admin config read] traceId=%s result=failed reason=%v", debugTraceID(r), err)
			writeJSONError(w, http.StatusServiceUnavailable, "Config file temporarily unreadable")
		}
		return
	}
	var parsed any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil || ensureJSONEOF(decoder) != nil {
		log.Printf("[Admin config read] traceId=%s result=failed reason=config file is not valid JSON; refusing to return unredacted raw text", debugTraceID(r))
		writeJSONError(w, http.StatusServiceUnavailable, "Config file temporarily unparseable")
		return
	}
	safe, err := json.MarshalIndent(redactDebugValue(parsed), "", "  ")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to encode config for display")
		return
	}
	log.Printf("[Admin config read] traceId=%s result=success adminKeyAndAPIKeyHidden=true note=disk config still stored in plaintext per the user's request", debugTraceID(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":  true,
		"path":    runtimeConfigFile,
		"content": string(safe),
	})
}

// credentialView is the masked credential view: only non-sensitive fields are kept; tokens show a prefix only.
type credentialView struct {
	Path               string `json:"path"`
	Exists             bool   `json:"exists"`
	Error              string `json:"error,omitempty"`
	Edition            string `json:"edition,omitempty"`
	Nickname           string `json:"nickname,omitempty"`
	UID                string `json:"uid,omitempty"`
	EnterpriseID       string `json:"enterpriseId,omitempty"`
	Domain             string `json:"domain,omitempty"`
	ExpiresAt          int64  `json:"expiresAt,omitempty"`
	RefreshExpiresAt   int64  `json:"refreshExpiresAt,omitempty"`
	LastRefreshTime    int64  `json:"lastRefreshTime,omitempty"`
	AccessTokenMasked  string `json:"accessTokenMasked,omitempty"`
	RefreshTokenMasked string `json:"refreshTokenMasked,omitempty"`
}

// maskToken keeps only the first 6 characters for identification, replacing the rest with asterisks; empty values stay empty.
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 6 {
		return "****"
	}
	return token[:6] + "****"
}

func handleWebUICredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET is supported")
		return
	}
	views := make([]credentialView, 0)
	for _, p := range collectConfiguredAuthPaths() {
		view := credentialView{Path: p, Exists: false}
		data, err := os.ReadFile(p)
		if err != nil {
			views = append(views, view)
			continue
		}
		var sa StoredAuth
		if err := json.Unmarshal(data, &sa); err != nil {
			view.Exists = true
			view.Error = "Failed to parse credential file"
			views = append(views, view)
			continue
		}
		view.Exists = true
		view.Edition = sa.Edition
		view.Nickname = sa.Account.Nickname
		view.UID = sa.Account.UID
		view.EnterpriseID = sa.Account.EnterpriseID
		view.Domain = sa.Auth.Domain
		view.ExpiresAt = sa.Auth.ExpiresAt
		view.RefreshExpiresAt = sa.Auth.RefreshExpiresAt
		view.LastRefreshTime = sa.Auth.LastRefreshTime
		view.AccessTokenMasked = maskToken(sa.Auth.AccessToken)
		view.RefreshTokenMasked = maskToken(sa.Auth.RefreshToken)
		views = append(views, view)
	}
	log.Printf("[Admin credentials] traceId=%s result=read completed files=%d note=returns masked views only; does not trigger refresh or writes", debugTraceID(r), len(views))
	writeAdminData(w, r, map[string]any{
		"hint":  "Listed tokens are masked; clicking Edit lets an admin read and save full credentials. Full tokens are never written to logs or browser persistent storage.",
		"files": views,
		// Keep the field name so the frontend can display the generation time
		"generatedAt": time.Now().Unix(),
	})
}
