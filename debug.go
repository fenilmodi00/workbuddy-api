package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const runtimeConfigFile = "config.json"

// Only supplements existing task logs; does not maintain separate task state. The admin page aggregates these records from gateway logs.
func logScheduledTask(task, phase, status, trace string, started, next time.Time, detail string) {
	record := struct {
		Task      string `json:"task"`
		Phase     string `json:"phase"`
		Status    string `json:"status,omitempty"`
		Trace     string `json:"trace"`
		PID       int    `json:"pid"`
		At        int64  `json:"at"`
		StartedAt int64  `json:"startedAt,omitempty"`
		NextAt    int64  `json:"nextAt,omitempty"`
		Detail    string `json:"detail,omitempty"`
	}{Task: task, Phase: phase, Status: status, Trace: trace, PID: os.Getpid(), At: time.Now().UnixMilli(), Detail: detail}
	if !started.IsZero() {
		record.StartedAt = started.UnixMilli()
	}
	if !next.IsZero() {
		record.NextAt = next.UnixMilli()
	}
	raw, _ := json.Marshal(record)
	log.Printf("[Scheduled task] %s", raw)
}

func startScheduledTask(task string, next time.Time) (string, time.Time) {
	trace, started := newTraceID(), time.Now()
	logScheduledTask(task, "started", "running", trace, started, next, "")
	return trace, started
}

// High-frequency local checks record the most recent execution each minute; state changes / actual changes are logged immediately to avoid per-second log growth.
func logFrequentTask(task, status, detail string, started, next time.Time, last *time.Time, previous *string, changed bool) {
	now := time.Now()
	if last.IsZero() || now.Sub(*last) >= time.Minute || *previous != status || changed {
		logScheduledTask(task, "completed", status, newTraceID(), started, next, detail)
		*last = now
	}
	*previous = status
}

type runtimeFileConfig struct {
	Gateway gatewayFileConfig `json:"gateway"`
	// SystemPrompt controls the fallback text and the experimental global forced text (appended at the end of system). Empty values keep the old behavior.
	SystemPrompt struct {
		Fallback string `json:"fallback"`
		Force    string `json:"force"`
	} `json:"systemPrompt"`
	// Identity controls all configured text replacement in system/developer/assistant.
	// When omitted, the built-in WorkBuddy display name and default text rules are used.
	Identity runtimeIdentityConfig `json:"identity"`
	// Only accepts legacy fields once written by v1.14.40, preventing startup failures on unknown fields during upgrades.
	// Its rules are not parsed or executed; new text replacement is handled solely by Identity.
	DeprecatedPromptCompatibility json.RawMessage      `json:"promptCompatibility,omitempty"`
	Concurrent                    concurrentFileConfig `json:"concurrent"`
	Debug                         debugFileConfig      `json:"debug"`
	Logs                          struct {
		RetentionDays int `json:"retentionDays"` // 0 keeps everything; positive N keeps the last N calendar days including today.
	} `json:"logs"`
	// The Upstream section is optional; adjusts upstream timeouts based on network conditions (in seconds; <=0 or omitted means defaults).
	Upstream struct {
		// RequestProfile: legacy (omitted/empty) keeps the old behavior; official uses the captured domestic desktop compatibility parameters.
		RequestProfile string `json:"requestProfile"`
		// HeaderTimeoutSeconds: upper bound waiting for response headers after the request body is sent.
		HeaderTimeoutSeconds int `json:"headerTimeoutSeconds"`
		// IdleTimeoutSeconds: idle read timeout for the streaming response body (no timeout while data keeps arriving).
		IdleTimeoutSeconds int `json:"idleTimeoutSeconds"`
		// TransientRetries: extra retries for transient network errors during the request-body send phase.
		// A pointer distinguishes "not configured" (use default) from "explicitly 0" (retries disabled).
		TransientRetries *int `json:"transientRetries"`
		// StreamKeepaliveSeconds: SSE comment interval while waiting for the upstream's first byte; <=0 or omitted uses the default.
		StreamKeepaliveSeconds int `json:"streamKeepaliveSeconds"`
		// EnableFirstByteRetry: whether to allow one safe replay after transient network interruption before the first byte.
		// The pointer distinguishes "not configured" (enabled by default) from explicit false (disabled).
		EnableFirstByteRetry *bool `json:"enableFirstByteRetry"`
	} `json:"upstream"`
	// The Models section is optional; controls the model blocklist/allowlist by model name (case-insensitive).
	Models struct {
		// Blocklist: names of disabled models; any hit is rejected.
		Blocklist []string `json:"blocklist"`
		// Allowlist: when non-empty, only models in the list are allowed; everything else is rejected.
		Allowlist []string `json:"allowlist,omitempty"`
		// Accounts: all controls global scheduling; other keys restrict credential file names per model.
		Accounts map[string]modelAccountFileConfig `json:"accounts"`
	} `json:"models"`
}

// debugFileConfig tolerates missing debug, null, empty objects, empty strings, 0/1, true/false, and
// unknown fields; any non-object or non-boolean value is treated as false so that config syntax
// details never block gateway startup. Explicit false is still written to the example and first-deploy
// default config so the switch remains discoverable.
type debugFileConfig struct {
	Enabled      bool `json:"enabled"`
	FullRequests bool `json:"fullRequests"`
}

func (d *debugFileConfig) UnmarshalJSON(data []byte) error {
	*d = debugFileConfig{}
	raw := bytes.TrimSpace(data)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil // Non-object values are treated as all-false, not blocking startup; unknown fields are likewise ignored.
	}
	d.Enabled = tolerantConfigBool(object["enabled"])
	d.FullRequests = tolerantConfigBool(object["fullRequests"])
	return nil
}

func tolerantConfigBool(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte(`""`)) {
		return false
	}
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "1", "yes", "on":
			return true
		default:
			return false
		}
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number == 1
	}
	return false
}

type modelAccountFileConfig struct {
	Allowlist []string `json:"allowlist"`
	Blocklist []string `json:"blocklist"`
}

type debugRequestContextKey struct{}

type debugRequestContext struct {
	mu                  sync.RWMutex
	start               time.Time
	traceID             string
	requestID           uint64
	parentRequestID     string
	model               string
	account             string
	stream              bool
	traceHeaderReceived bool
	deadlinePresent     bool
	deadlineRemainingMS int64
}

type debugJSONSink struct {
	mu     sync.Mutex
	writer io.Writer
	closer io.Closer
}

var (
	debugSinkMu sync.RWMutex
	debugSink   *debugJSONSink
	instanceID  = hostInstanceID()
)

// modelFilter holds the effective model blocklist/allowlist (normalized to lowercase, deduplicated).
type modelFilter struct {
	blocklist map[string]bool
	allowlist map[string]bool
}

var (
	modelFilterMu sync.RWMutex
	currentFilter = modelFilter{}
)

// setModelFilter rebuilds the model blocklist/allowlist from config. Empty lists mean no restriction for that dimension.
func setModelFilter(blocklist, allowlist []string) {
	build := func(names []string) map[string]bool {
		if len(names) == 0 {
			return nil
		}
		out := make(map[string]bool, len(names))
		for _, name := range names {
			key := normalizeModelName(name)
			if key != "" {
				out[key] = true
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	modelFilterMu.Lock()
	currentFilter = modelFilter{blocklist: build(blocklist), allowlist: build(allowlist)}
	modelFilterMu.Unlock()
}

// modelDisabled reports whether a model is disabled by configuration and returns the reason.
// Rules: a blocklist hit disables; a non-empty allowlist without a hit disables; neither configured means no restriction.
func modelDisabled(model string) (bool, string) {
	key := normalizeModelName(model)
	if key == "" {
		return false, ""
	}
	modelFilterMu.RLock()
	filter := currentFilter
	modelFilterMu.RUnlock()

	if filter.blocklist != nil && filter.blocklist[key] {
		return true, fmt.Sprintf("Model %s has been disabled by the gateway (blocklist hit); contact an administrator to adjust config.json", model)
	}
	if filter.allowlist != nil && !filter.allowlist[key] {
		return true, fmt.Sprintf("Model %s has been disabled by the gateway (not in allowlist); contact an administrator to adjust config.json", model)
	}
	return false, ""
}

// modelFilterConfigured reports whether any blocklist/allowlist is currently configured (for the startup banner).
func modelFilterConfigured() bool {
	modelFilterMu.RLock()
	defer modelFilterMu.RUnlock()
	return currentFilter.blocklist != nil || currentFilter.allowlist != nil
}

// modelFilterSummary returns the effective blocklist/allowlist entry counts (for the startup banner).
func modelFilterSummary() (blocked, allowed int) {
	modelFilterMu.RLock()
	defer modelFilterMu.RUnlock()
	return len(currentFilter.blocklist), len(currentFilter.allowlist)
}

var (
	modelAccountFilterMu sync.RWMutex
	modelAccountFilters  = map[string]modelAccountFileRule{}
)

type modelAccountFileRule struct {
	allow map[string]bool
	block map[string]bool
}

// setModelAccountFilter validates and atomically replaces per-model credential file rules; credentials cannot be matched by path or wildcards.
func setModelAccountFilter(config map[string]modelAccountFileConfig) error {
	rules, err := buildModelAccountRules(config)
	if err != nil {
		return err
	}
	modelAccountFilterMu.Lock()
	modelAccountFilters = rules
	modelAccountFilterMu.Unlock()
	return nil
}

func buildModelAccountRules(config map[string]modelAccountFileConfig) (map[string]modelAccountFileRule, error) {
	rules := make(map[string]modelAccountFileRule, len(config))
	seen := make(map[string]bool, len(config))
	for model, entry := range config {
		id := normalizeModelName(model)
		if id == "" {
			return nil, fmt.Errorf("model name must not be empty")
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate configuration for model %s", id)
		}
		seen[id] = true
		rule := modelAccountFileRule{allow: map[string]bool{}, block: map[string]bool{}}
		for _, item := range []struct {
			list []string
			into map[string]bool
		}{{entry.Allowlist, rule.allow}, {entry.Blocklist, rule.block}} {
			for _, raw := range item.list {
				name := strings.TrimSpace(raw)
				if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\\*?[]`) || !strings.HasSuffix(strings.ToLower(name), ".json") {
					return nil, fmt.Errorf("credential file name %q for model %s is invalid: only exact file names ending in .json are supported; paths and wildcards are not allowed", id, raw)
				}
				item.into[name] = true
			}
		}
		if len(rule.allow) == 0 && len(rule.block) == 0 {
			continue
		}
		rules[id] = rule
	}
	return rules, nil
}

// modelAccountAllowed checks both the global all rule and model-specific account rules.
// Duplicate file names refuse the match so same-named credentials in different directories are not all let through by mistake.
func modelAccountAllowed(model string, acc *Account, pool []*Account) (bool, string) {
	modelAccountFilterMu.RLock()
	rule, configured := modelAccountFilters[normalizeModelName(model)]
	global, globalConfigured := modelAccountFilters["all"]
	modelAccountFilterMu.RUnlock()
	if acc == nil || !configured && !globalConfigured {
		return true, ""
	}
	name := filepath.Base(acc.Path)
	for _, other := range pool {
		if other != acc && filepath.Base(other.Path) == name {
			return false, "duplicate credential file name"
		}
	}
	for _, item := range []struct {
		rule  modelAccountFileRule
		label string
	}{{global, "global"}, {rule, "model"}} {
		if item.rule.block[name] {
			return false, "hit " + item.label + " account blocklist"
		}
		if len(item.rule.allow) > 0 && !item.rule.allow[name] {
			return false, "not in " + item.label + " account allowlist"
		}
	}
	return true, ""
}

// modelAccountRuleConfigured reports whether this model has a valid credential file rule configured.
func modelAccountRuleConfigured(model string) bool {
	modelAccountFilterMu.RLock()
	rule, ok := modelAccountFilters[normalizeModelName(model)]
	global, globalOK := modelAccountFilters["all"]
	modelAccountFilterMu.RUnlock()
	return ok && (len(rule.allow) > 0 || len(rule.block) > 0) || globalOK && (len(global.allow) > 0 || len(global.block) > 0)
}

func modelAccountRuleCount() int {
	modelAccountFilterMu.RLock()
	defer modelAccountFilterMu.RUnlock()
	n := len(modelAccountFilters)
	if _, ok := modelAccountFilters["all"]; ok {
		n--
	}
	return n
}

func modelAccountRuleIDs() []string {
	modelAccountFilterMu.RLock()
	ids := make([]string, 0, len(modelAccountFilters))
	for id := range modelAccountFilters {
		if id != "all" {
			ids = append(ids, id)
		}
	}
	modelAccountFilterMu.RUnlock()
	sort.Strings(ids)
	return ids
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("config file contains multiple JSON values")
}

func initDebugLogging() (func(), error) {
	if !cfg.DebugEnabled {
		return func() {}, nil
	}
	f, err := newDailyLogWriter("debug", ".jsonl")
	if err != nil {
		return nil, fmt.Errorf("open debug log: %w", err)
	}
	sink := &debugJSONSink{writer: f, closer: f}
	debugSinkMu.Lock()
	debugSink = sink
	debugSinkMu.Unlock()
	return func() {
		debugSinkMu.Lock()
		if debugSink == sink {
			debugSink = nil
		}
		debugSinkMu.Unlock()
		_ = sink.closer.Close()
	}, nil
}

func debugLoggingEnabled() bool {
	debugSinkMu.RLock()
	enabled := debugSink != nil
	debugSinkMu.RUnlock()
	return enabled
}

func hostInstanceID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "unknown"
	}
	return host
}

func ensureDebugRequestContext(r *http.Request, traceID string, start time.Time) *debugRequestContext {
	if current := debugContextFromRequest(r); current != nil {
		return current
	}
	if start.IsZero() {
		start = time.Now()
	}
	received := strings.TrimSpace(r.Header.Get("X-Trace-ID")) != ""
	if traceID == "" {
		traceID = strings.TrimSpace(r.Header.Get("X-Trace-ID"))
	}
	if traceID == "" {
		traceID = newTraceID()
	}
	parentID := strings.TrimSpace(r.Header.Get("X-Parent-Request-ID"))
	if parentID == "" {
		parentID = strings.TrimSpace(r.Header.Get("X-Request-ID"))
	}
	deadline, hasDeadline := r.Context().Deadline()
	remaining := int64(0)
	if hasDeadline {
		remaining = time.Until(deadline).Milliseconds()
	}
	state := &debugRequestContext{
		start:               start,
		traceID:             traceID,
		parentRequestID:     parentID,
		traceHeaderReceived: received,
		deadlinePresent:     hasDeadline,
		deadlineRemainingMS: remaining,
	}
	*r = *r.WithContext(context.WithValue(r.Context(), debugRequestContextKey{}, state))
	return state
}

func newTraceID() string {
	return uuid.NewString()
}

func debugContextFromRequest(r *http.Request) *debugRequestContext {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(debugRequestContextKey{}).(*debugRequestContext)
	return state
}

func requestIDFor(r *http.Request) uint64 {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.requestID == 0 {
			state.requestID = atomic.AddUint64(&reqCounter, 1)
		}
		return state.requestID
	}
	return atomic.AddUint64(&reqCounter, 1)
}

func requestStartFor(r *http.Request) time.Time {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.start
	}
	return time.Now()
}

func debugSetModelAndStream(r *http.Request, model string, stream bool) {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.Lock()
		state.model = model
		state.stream = stream
		state.mu.Unlock()
	}
}

func debugSetAccount(r *http.Request, account string) {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.Lock()
		state.account = filepath.Base(account)
		state.mu.Unlock()
	}
}

func debugTraceID(r *http.Request) string {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.traceID
	}
	return ""
}

func debugEvent(r *http.Request, level, event string, fields map[string]any) {
	debugSinkMu.RLock()
	sink := debugSink
	debugSinkMu.RUnlock()
	if sink == nil || r == nil {
		return
	}

	state := ensureDebugRequestContext(r, "", time.Time{})
	requestIDFor(r)
	state.mu.RLock()
	start := state.start
	traceID := state.traceID
	requestID := state.requestID
	parentRequestID := state.parentRequestID
	model := state.model
	account := state.account
	stream := state.stream
	traceHeaderReceived := state.traceHeaderReceived
	deadlinePresent := state.deadlinePresent
	deadlineRemainingMS := state.deadlineRemainingMS
	state.mu.RUnlock()

	record := map[string]any{
		"timestamp":             time.Now().UTC().Format(time.RFC3339Nano),
		"level":                 level,
		"event":                 event,
		"trace_id":              traceID,
		"request_id":            requestID,
		"parent_request_id":     parentRequestID,
		"service":               "workbuddy-gateway",
		"instance":              instanceID,
		"version":               version,
		"route":                 r.URL.Path,
		"method":                r.Method,
		"model":                 model,
		"acc":                   account,
		"stream":                stream,
		"elapsed_ms":            time.Since(start).Milliseconds(),
		"client_ip":             requestClientIP(r),
		"remote_addr":           r.RemoteAddr,
		"user_agent":            r.UserAgent(),
		"host":                  r.Host,
		"scheme":                requestScheme(r),
		"content_type":          r.Header.Get("Content-Type"),
		"content_length":        r.ContentLength,
		"transfer_encoding":     strings.Join(r.TransferEncoding, ","),
		"x_forwarded_for":       r.Header.Get("X-Forwarded-For"),
		"x_real_ip":             r.Header.Get("X-Real-IP"),
		"trace_header_received": traceHeaderReceived,
		"deadline_present":      deadlinePresent,
		"deadline_remaining_ms": deadlineRemainingMS,
		"protocol":              r.Proto,
		"tls":                   r.TLS != nil,
		"authorization_present": strings.TrimSpace(r.Header.Get("Authorization")) != "",
		"api_key_fingerprint":   apiKeyFingerprint(r.Header.Get("Authorization")),
	}
	for key, value := range fields {
		record[key] = value
	}

	// Only build a copy for logging; must not rewrite fields or errors the caller uses for business decisions.
	data, err := json.Marshal(redactDebugValue(record))
	if err != nil {
		return
	}
	sink.mu.Lock()
	n, writeErr := sink.writer.Write(append(data, '\n'))
	sink.mu.Unlock()
	if writeErr != nil || n != len(data)+1 {
		log.Printf("[Debug log] traceId=%s event=%s result=write failed errorType=%T businessImpact=this diagnostic record may be incomplete; model requests are not interrupted",
			traceID, event, writeErr)
	}
}

func debugBodyReadStarted(r *http.Request) time.Time {
	if !debugLoggingEnabled() {
		return time.Time{}
	}
	started := time.Now()
	debugEvent(r, "debug", "request_body_read_started", debugBodyFields(r, nil, 0, 0, false, nil))
	return started
}

func debugElapsedSince(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

func debugBodyReadCompleted(r *http.Request, body []byte, readDuration, decodeDuration time.Duration, jsonValid bool, decodeErr error) {
	if !debugLoggingEnabled() {
		return
	}
	level := "debug"
	fields := debugBodyFields(r, body, readDuration, decodeDuration, jsonValid, nil)
	if decodeErr != nil {
		level = "warn"
		fields["json_error"] = safeDebugError(decodeErr)
	}
	debugEvent(r, level, "request_body_read_completed", fields)
	debugFullIncomingBody(r, body, decodeErr == nil, false)
}

func debugBodyReadFailed(r *http.Request, body []byte, readStarted time.Time, readErr error) {
	if !debugLoggingEnabled() {
		return
	}
	debugEvent(r, "error", "request_body_read_failed", debugBodyFields(r, body, time.Since(readStarted), 0, false, readErr))
	debugFullIncomingBody(r, body, false, true)
}

func debugBodyFields(r *http.Request, body []byte, readDuration, decodeDuration time.Duration, jsonValid bool, readErr error) map[string]any {
	declared := r.ContentLength
	fields := map[string]any{
		"declared_body_bytes": declared,
		"actual_body_bytes":   len(body),
		"body_read_ms":        durationMilliseconds(readDuration),
		"body_sha256_prefix":  bodyHashPrefix(body),
		"json_valid":          jsonValid,
		"json_decode_ms":      durationMilliseconds(decodeDuration),
		"body_limit_bytes":    int64(0),
		"body_limit_exceeded": false,
		"read_error_type":     "",
		"read_error":          "",
	}
	if readErr != nil {
		fields["read_error_type"] = debugErrorType(readErr)
		fields["read_error"] = safeDebugError(readErr)
	}
	return fields
}

func durationMilliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func bodyHashPrefix(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:12]
}

func debugErrorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	default:
		t := reflect.TypeOf(err)
		if t == nil {
			return "unknown"
		}
		return t.String()
	}
}

func safeDebugError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ReplaceAll(strings.ReplaceAll(redactSensitiveText(err.Error()), "\r", " "), "\n", " ")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func requestClientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
		return forwarded
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func apiKeyFingerprint(authorization string) string {
	parts := strings.Fields(authorization)
	secret := strings.TrimSpace(authorization)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		secret = parts[1]
	}
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:12]
}
