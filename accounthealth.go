package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type accountHealth struct {
	Percent *float64 `json:"percent"`
	State   string   `json:"state"` // healthy / degraded / unavailable / empty / unknown
}

// Keep the account-table availability definition; globally disabled accounts stay in the denominator so the
// percentage cannot be inflated by excluding them. This is not the schedulability of one specific model:
// a single-model cooldown is not treated as the whole account failing.
func summarizeAccountHealth(accs []accountSnapshot, stale bool) accountHealth {
	if stale {
		return accountHealth{State: "unknown"}
	}
	if len(accs) == 0 {
		return accountHealth{State: "empty"}
	}
	healthy := 0
	for _, acc := range accs {
		if acc.State == "active" && (acc.SchedulingEnabled == nil || *acc.SchedulingEnabled) {
			healthy++
		}
	}
	// Round down to one decimal, avoiding 999/1000 rounding up to 100%.
	percent := math.Floor(float64(healthy)*1000/float64(len(accs))) / 10
	state := "degraded"
	if healthy == len(accs) {
		state = "healthy"
	} else if healthy == 0 {
		state = "unavailable"
	}
	return accountHealth{Percent: &percent, State: state}
}

type accountAlertRule struct {
	Label    string
	Severity string
	Message  string
}

var accountAlertRules = map[string]accountAlertRule{
	"disabled":       {"Account disabled", "urgent", "Login authorization has expired and the account stopped scheduling. Please log in again."},
	"expired":        {"Account expired", "important", "The access token has expired. Check the auto-renewal result or log in again."},
	"paid_exhausted": {"Paid exhausted", "important", "Account quota confirmed exhausted; paid models are unavailable until the quota recovers."},
	"cooldown":       {"Account cooldown", "warning", "The account hit a rate limit and is not scheduled during the cooldown. Wait for automatic recovery."},
	"paused":         {"Account paused", "notice", "This account was paused by a global scheduling rule; this is not an authorization failure and can be restored in the account pool."},
}

func accountAlertKinds(acc accountSnapshot) []string {
	kinds := []string{}
	if acc.SchedulingEnabled != nil && !*acc.SchedulingEnabled {
		kinds = append(kinds, "paused")
	}
	state := acc.State
	if state == "quota_exhausted" {
		state = "paid_exhausted"
	}
	if _, ok := accountAlertRules[state]; ok {
		kinds = append(kinds, state)
	}
	return kinds
}

type accountLogEvent struct {
	At      int64  `json:"at"`
	Account string `json:"account"`
	State   string `json:"state"`
	Trace   string `json:"trace,omitempty"`
	File    string `json:"-"`
}

// Only used to reduce duplicate logs: no new state file is created. Startup and state transitions write one
// regular log line, so expired/paused accounts (including direct config edits) also get a real traceable log source.

// legacyAccountLogWording rewrites the pre-migration Chinese log wording to the
// current English wording, so alert parsing keeps working over rotated logs
// written before the English migration. Phrases are ordered: specific phrases
// must come before the generic "account" token.
var legacyAccountLogWording = []struct{ old, new string }{
	{"\u8d26\u53f7\u72b6\u6001", "Account status"},                              // account status
	{"\u7ba1\u7406\u5f00\u5173", "Admin toggle"},                                // admin toggle
	{"\u8d26\u53f7\u5df2\u51bb\u7ed3\u8c03\u5ea6", "account scheduling frozen"}, // account scheduling frozen
	{"\u5206\u914d\u989d\u5ea6\u5df2\u7528\u5b8c", "allocated quota used up"},   // allocated quota used up
	{"\u5224\u5b9a\u767b\u5f55\u6001\u5931\u6548", "login state declared dead"}, // login state declared dead
	{"\u6388\u6743\u5931\u6548\uff0c\u5df2\u7981\u6b62\u8c03\u5ea6", "authorization failed; removed from scheduling"},
	{"\u5df2\u505c\u6b62\u8c03\u5ea6\uff1b", "removed from scheduling;"},    // stopped scheduling;
	{"\u89e6\u53d1\u9891\u7387\u9650\u5236", "hit the rate limit"},          // hit the rate limit
	{"\u989d\u5ea6\u67e5\u8be2\u6210\u529f", "quota query succeeded"},       // quota query succeeded
	{"\u4f01\u4e1a\u989d\u5ea6\u8ba1\u7b97=", "enterprise quota computed="}, // enterprise quota computed=
	{"\u7ed3\u679c=\u6210\u529f", "result=success"},                         // result=success
	{"\u7c7b\u578b=", "type="},                                              // type=
	{"\u76ee\u6807=", "target="},                                            // target=
	{"\u542f\u7528=", "enabled="},                                           // enabled=
	{"\u8bf4\u660e=", "note="},                                              // note=
	{"\u8d26\u53f7", "account"},                                             // account
}

func normalizeAccountLog(body string) string {
	for _, w := range legacyAccountLogWording {
		body = strings.ReplaceAll(body, w.old, w.new)
	}
	return body
}

// Compatible with existing regular logs. Only known log tags/wordings are matched;
// arbitrary lines containing "expired" wording are not alerts.
func parseAccountLogEvent(line, file string) (accountLogEvent, bool) {
	event := accountLogEvent{At: logLineTime(line), File: file}
	if event.At <= 0 || len(line) < 21 {
		return event, false
	}
	body := normalizeAccountLog(line[20:])
	if strings.HasPrefix(body, "[Account status] ") {
		if json.Unmarshal([]byte(strings.TrimPrefix(body, "[Account status] ")), &event) != nil {
			return accountLogEvent{}, false
		}
		event.File = file
		_, known := accountAlertRules[event.State]
		return event, known && event.At > 0 && event.Account != ""
	}
	low := strings.ToLower(body)
	accountBetween := func(suffix string) string {
		idx := strings.Index(low, "account ")
		if idx < 0 {
			return ""
		}
		rest := body[idx+len("account "):]
		end := strings.Index(strings.ToLower(rest), suffix)
		if end < 0 {
			return ""
		}
		return strings.TrimSpace(rest[:end])
	}
	switch {
	case strings.HasPrefix(body, "[Cooldown] "):
		event.State, event.Account = "cooldown", accountBetween(" hit the rate limit")
	case strings.HasPrefix(body, "[Quota] "):
		if strings.Contains(low, "account scheduling frozen") || strings.Contains(low, "allocated quota used up") {
			event.State = "paid_exhausted"
			event.Account = accountBetween(" quota query succeeded")
			if event.Account == "" {
				event.Account = accountBetween(" enterprise quota computed=")
			}
		}
	case strings.HasPrefix(body, "[Auth] "):
		for _, suffix := range []string{" login state declared dead", " authorization failed; removed from scheduling", " removed from scheduling;"} {
			if name := accountBetween(suffix); name != "" {
				event.Account, event.State = name, "disabled"
				break
			}
		}
	case strings.HasPrefix(body, "[Admin toggle] ") && strings.Contains(low, "result=success type=account ") && strings.Contains(low, " enabled=false "):
		_, rest, ok := strings.Cut(body, "target=")
		if ok {
			quoted, _, ok := strings.Cut(rest, " enabled=")
			if ok {
				if name, err := strconv.Unquote(quoted); err == nil {
					event.Account, event.State = name, "paused"
				}
			}
		}
	}
	_, known := accountAlertRules[event.State]
	return event, known && event.Account != ""
}

const accountAlertReadBudget int64 = 4 << 20

type accountAlertScan struct {
	Events    map[string]accountLogEvent
	Scanned   int64
	Truncated bool
}

// Scan only regular logs with a 4MiB total tail budget; read failures do not affect the health endpoint or model requests.
func readAccountAlertLogs() (accountAlertScan, error) {
	scan := accountAlertScan{Events: map[string]accountLogEvent{}}
	info, err := os.Lstat(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	if !info.IsDir() {
		return scan, errors.New("Log path is not a directory")
	}
	entries, err := os.ReadDir(logDir)
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
	for _, name := range names {
		budget := accountAlertReadBudget - scan.Scanned
		if budget <= 0 {
			scan.Truncated = true
			break
		}
		f, err := root.Open(name)
		if err != nil {
			return scan, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return scan, errors.New("Account alert logs unreadable")
		}
		size := min(info.Size(), budget)
		start := info.Size() - size
		data := make([]byte, size)
		n, readErr := f.ReadAt(data, start)
		f.Close()
		if (readErr != nil && readErr != io.EOF) || int64(n) != size {
			return scan, errors.New("Account alert logs changed while reading, please retry")
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
		// Ignore the last line that has not been fully written yet.
		lines := bytes.Split(data, []byte{'\n'})
		for _, line := range lines[:len(lines)-1] {
			if event, ok := parseAccountLogEvent(string(line), name); ok {
				key := event.Account + "\x00" + event.State
				if old, exists := scan.Events[key]; !exists || event.At > old.At {
					scan.Events[key] = event
				}
			}
		}
	}
	return scan, nil
}

var accountAlertCache struct {
	sync.Mutex
	at   time.Time
	scan accountAlertScan
}

type accountAlertView struct {
	Account   string `json:"account"`
	State     string `json:"state"`
	Label     string `json:"label"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	At        int64  `json:"at"`
	LogFile   string `json:"logFile"`
	LogOffset int64  `json:"logOffset,omitempty"`
}

func currentAccountAlerts(accs []accountSnapshot, scan accountAlertScan) ([]accountAlertView, int) {
	out := []accountAlertView{}
	missing := 0
	for _, acc := range accs {
		for _, kind := range accountAlertKinds(acc) {
			event, ok := scan.Events[acc.Path+"\x00"+kind]
			if !ok {
				missing++
				continue // do not fabricate alert "records" without logs.
			}
			rule := accountAlertRules[kind]
			out = append(out, accountAlertView{Account: acc.Path, State: kind, Label: rule.Label,
				Severity: rule.Severity, Message: rule.Message, At: event.At, LogFile: event.File})
		}
	}
	// Newest logs first; severity only decides color and label, not the sort order.
	// Ties on second-level timestamps are sorted stably by account and kind to avoid jitter between polls.
	sortAccountAlerts(out)
	return out, missing
}

func handleWebUIAccountAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Account alerts are read-only; only GET is supported")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	annotateAccountPolicies(snap.Accounts)
	// Newer snapshots carry inline alert markers: use them directly instead of reading logs; only old snapshots fall back to scanning.
	inline := snap.AlertSource == alertSourceSnapshot
	accountAlertCache.Lock()
	if !inline && (accountAlertCache.at.IsZero() || time.Since(accountAlertCache.at) >= 10*time.Second) {
		scan, readErr := readAccountAlertLogs()
		if readErr != nil {
			accountAlertCache.Unlock()
			log.Printf("[Account alerts] traceId=%s stage=read regular logs result=failed note=no fabricated empty list is shown; health and model calls unaffected", debugTraceID(r))
			writeJSONError(w, http.StatusServiceUnavailable, "Account alert logs temporarily unreadable; keeping existing results")
			return
		}
		accountAlertCache.scan, accountAlertCache.at = scan, time.Now()
	}
	scan := accountAlertCache.scan
	accountAlertCache.Unlock()
	alerts, missing := []accountAlertView{}, 0
	if inline {
		alerts = currentAlertsFromSnapshot(snap.Accounts)
	} else {
		alerts, missing = currentAccountAlerts(snap.Accounts, scan)
	}
	limited := len(alerts) > 100
	if limited {
		alerts = alerts[:100]
	}
	log.Printf("[Account alerts] traceId=%s result=success items=%d missing logs=%d scanned bytes=%d scan truncated=%t sort=newest logs first note=recovered items are dropped first, then the newest 100 are kept in reverse time order; severity is used only for color and labels and contains no tokens or upstream error bodies", debugTraceID(r), len(alerts), missing, scan.Scanned, scan.Truncated)
	writeAdminData(w, r, map[string]any{
		"alerts": alerts, "updatedAt": snap.UpdatedAt, "stale": time.Now().Unix()-snap.UpdatedAt > 15,
		"source": "gateway regular logs", "missingLogCount": missing,
		"scanTruncated": scan.Truncated, "listTruncated": limited,
	})
}
