package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// accountAlertMark is a compact alert marker written into the status snapshot: it records only the state,
// first-seen time and log position, without copying any log body. The page renders alerts from it, so the
// background log scan is no longer required.
type accountAlertMark struct {
	State     string `json:"state"`
	At        int64  `json:"at"`
	LogFile   string `json:"logFile,omitempty"`
	LogOffset int64  `json:"logOffset,omitempty"`
}

// alertSourceSnapshot is written into the snapshot to distinguish "new inline alerts" from "old snapshots that need a log-scan fallback".
const alertSourceSnapshot = "snapshot"

var accountAlertMarks = struct {
	sync.Mutex
	current map[string]accountAlertMark
	last    map[string]string
}{current: map[string]accountAlertMark{}, last: map[string]string{}}

// logWriteOffset returns the current length of today's log file, i.e. the byte offset of the next log line.
// Logs are append-only, so this offset points exactly at the line about to be written.
func logWriteOffset() (string, int64) {
	name := "gateway-" + time.Now().Format("2006-01-02") + ".log"
	info, err := os.Stat(filepath.Join(logDir, name))
	if err != nil {
		return name, -1
	}
	return name, info.Size()
}

// recordAccountAlertMarks writes a regular log line on state changes and records current anomalies as compact
// markers for the snapshot. Returns markers grouped by credential path for writeStatusSnapshot to persist.
func recordAccountAlertMarks(accs []accountSnapshot, now int64) map[string][]accountAlertMark {
	accountAlertMarks.Lock()
	defer accountAlertMarks.Unlock()
	next := make(map[string]string, len(accs))
	current := map[string]accountAlertMark{}
	trace := newTraceID()
	for _, acc := range accs {
		kinds := accountAlertKinds(acc)
		signature := acc.UID + "\x00" + acc.State + "\x00" + strings.Join(kinds, ",")
		next[acc.Path] = signature
		changed := accountAlertMarks.last[acc.Path] != signature
		if len(kinds) == 0 {
			// Recovery also leaves an audit log line, but no alert marker is generated.
			if changed {
				if raw, err := json.Marshal(accountLogEvent{At: now, Account: acc.Path, State: acc.State, Trace: trace}); err == nil {
					log.Printf("[Account status] %s", raw)
				}
			}
			continue
		}
		for _, kind := range kinds {
			key := acc.Path + "\x00" + kind
			mark, seen := accountAlertMarks.current[key]
			if !seen || mark.At <= 0 {
				mark = accountAlertMark{State: kind, At: now}
			}
			// Log only when the state actually changes, recording that line's file and offset.
			if changed && !seen {
				file, offset := logWriteOffset()
				if raw, err := json.Marshal(accountLogEvent{At: now, Account: acc.Path, State: kind, Trace: trace}); err == nil {
					log.Printf("[Account status] %s", raw)
				}
				mark.LogFile, mark.LogOffset = file, offset
			}
			mark.State = kind
			current[key] = mark
		}
	}
	accountAlertMarks.last = next
	// Must write back: otherwise markers are rebuilt every round, losing locators and refreshing the first-seen time.
	accountAlertMarks.current = current
	out := make(map[string][]accountAlertMark, len(accs))
	for key, mark := range current {
		path := key[:strings.Index(key, "\x00")]
		out[path] = append(out[path], mark)
	}
	return out
}

// sortAccountAlerts applies one uniform order: newest first, ties on the same second sorted stably by account and kind.
func sortAccountAlerts(out []accountAlertView) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].State < out[j].State
	})
}

// currentAlertsFromSnapshot builds alert views directly from snapshot markers without scanning log files.
// A marker inconsistent with the account's current state (e.g. just recovered) is not shown, guaranteeing
// "recovery removes the alert".
func currentAlertsFromSnapshot(accs []accountSnapshot) []accountAlertView {
	out := []accountAlertView{}
	for _, acc := range accs {
		want := map[string]bool{}
		for _, kind := range accountAlertKinds(acc) {
			want[kind] = true
		}
		for _, mark := range acc.Alerts {
			rule, known := accountAlertRules[mark.State]
			if !known || !want[mark.State] {
				continue
			}
			out = append(out, accountAlertView{Account: acc.Path, State: mark.State, Label: rule.Label,
				Severity: rule.Severity, Message: rule.Message, At: mark.At, LogFile: mark.LogFile,
				LogOffset: mark.LogOffset})
		}
	}
	sortAccountAlerts(out)
	return out
}
