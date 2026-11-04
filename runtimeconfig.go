package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed config.example.json
var embeddedDefaultRuntimeConfig []byte

// All in-process config file read-modify-write shares this lock, so web UI
// writes and hot reload do not overwrite each other.
// Lock order: runtimeConfigMu -> gatewayConfigMu; the scheduler does not hold it.
var runtimeConfigMu sync.Mutex
var upstreamConfigMu sync.RWMutex
var statusSnapshotWriteMu sync.Mutex

// Updated together with the runtime state; must not be cached only by the scan
// goroutine: reverting the old file right after a web UI write must also take effect.
var runtimeAppliedHash [32]byte
var runtimeHasAppliedHash bool
var deprecatedPromptCompatibilityPresent atomic.Bool

func parseRuntimeConfig(data []byte) (runtimeFileConfig, error) {
	var c runtimeFileConfig
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return c, errors.New("config.json must be a JSON object, not an empty file or null")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return c, err
	}
	g, err := normalizeGatewayConfig(c.Gateway)
	if err != nil {
		return c, err
	}
	apiPort := g.APIPort
	if cfg.PortExplicit {
		apiPort = cfg.Port
	}
	if apiPort < 1 || apiPort > 65535 || cfg.WebUI && apiPort == g.WebPort {
		return c, errors.New("API listen port is invalid or conflicts with the Web UI listen port")
	}
	if _, err := buildModelAccountRules(c.Models.Accounts); err != nil {
		return c, err
	}
	for _, seconds := range []int{c.Upstream.HeaderTimeoutSeconds, c.Upstream.IdleTimeoutSeconds} {
		if int64(seconds) > int64((1<<63-1)/time.Second) {
			return c, errors.New("upstream timeout seconds too large, exceeds the time.Duration representable range")
		}
	}
	// Compatible with old examples that carried two empty arrays, but a non-empty
	// blocklist and allowlist cannot both take effect.
	if hasModelNames(c.Models.Blocklist) && hasModelNames(c.Models.Allowlist) {
		return c, errors.New("models.allowlist and models.blocklist must not both be non-empty; keep only one model list")
	}
	if c.Logs.RetentionDays < 0 || c.Logs.RetentionDays > 36500 {
		return c, gatewayValidationError("logs.retentionDays must be an integer in 0–36500; 0 disables automatic cleanup")
	}
	if _, err := normalizeRequestProfile(c.Upstream.RequestProfile); err != nil {
		return c, err
	}
	return c, nil
}

func hasModelNames(names []string) bool {
	for _, name := range names {
		if normalizeModelName(name) != "" {
			return true
		}
	}
	return false
}

// ensureRuntimeConfigFile creates a complete default config for a new user and
// adds missing default sections to older configs. Existing values are preserved;
// initialization errors are warnings at startup, not a reason to stop serving.
func ensureRuntimeConfigFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return writeNewRuntimeConfig(path)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config.json is not a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil // leave it to the normal config validation to report; do not overwrite a half-written or invalid config.
	}
	changed := false
	hasKey := func(object map[string]json.RawMessage, name string) bool {
		for k := range object {
			if strings.EqualFold(k, name) {
				return true
			}
		}
		return false
	}
	if !hasKey(doc, "identity") {
		doc["identity"], err = json.Marshal(defaultIdentityConfig())
		if err != nil {
			return err
		}
		changed = true
	} else {
		updated, updateErr := ensureIdentityDefaultReplacements(doc)
		if updateErr != nil {
			return updateErr
		}
		changed = changed || updated
	}
	if !hasKey(doc, "concurrent") {
		doc["concurrent"], _ = json.Marshal(normalizeConcurrentConfig(concurrentFileConfig{}))
		changed = true
	} else {
		for key, value := range doc {
			if !strings.EqualFold(key, "concurrent") {
				continue
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(value, &fields) != nil || fields == nil {
				break
			}
			if !hasKey(fields, "MaxAcc") {
				fields["MaxAcc"] = json.RawMessage("4")
				changed = true
			}
			if !hasKey(fields, "MaxGlobal") {
				fields["MaxGlobal"] = json.RawMessage("0")
				changed = true
			}
			doc[key], _ = json.Marshal(fields)
			break
		}
	}
	if !changed {
		return nil
	}
	updated, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	if bytes.Equal(updated, raw) {
		return nil
	}
	temporary := path + ".identity-" + fmt.Sprint(os.Getpid()) + ".tmp"
	fd, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	if _, err := fd.Write(updated); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	log.Printf("[Config initialization] added missing identity/concurrent default config; existing values and other config are preserved, later changes still hot-reload")
	return nil
}

// ensureIdentityDefaultReplacements upgrades older identities without
// overwriting user-defined values. Missing replacement maps receive the full
// default set because a non-empty custom map disables runtime defaults.
func ensureIdentityDefaultReplacements(doc map[string]json.RawMessage) (bool, error) {
	identityKey := ""
	for key := range doc {
		if strings.EqualFold(key, "identity") {
			identityKey = key
			break
		}
	}
	if identityKey == "" {
		return false, nil
	}
	var identity map[string]json.RawMessage
	if json.Unmarshal(doc[identityKey], &identity) != nil {
		return false, nil
	}
	if identity == nil {
		// A legacy null section already used runtime defaults; persist the same
		// effective defaults so the upgraded configuration is editable as well.
		encoded, err := json.Marshal(defaultIdentityConfig())
		if err != nil {
			return false, err
		}
		doc[identityKey] = encoded
		return true, nil
	}
	target := "WorkBuddy"
	for key, raw := range identity {
		if strings.EqualFold(key, "target") {
			var configured string
			if json.Unmarshal(raw, &configured) == nil && strings.TrimSpace(configured) != "" {
				target = strings.TrimSpace(configured)
			}
			break
		}
	}
	replacementsKey := ""
	for key := range identity {
		if strings.EqualFold(key, "replacements") {
			replacementsKey = key
			break
		}
	}
	if replacementsKey == "" {
		encoded, err := json.Marshal(defaultIdentityReplacements(target))
		if err != nil {
			return false, err
		}
		identity["replacements"] = encoded
	} else {
		var replacements map[string]string
		if json.Unmarshal(identity[replacementsKey], &replacements) != nil {
			return false, nil // hand invalid types to full config validation; do not overwrite on my own.
		}
		// A non-empty custom map disables runtime defaults, so merge every missing
		// built-in rule while never overwriting an explicitly customized value.
		defaults := defaultIdentityReplacements(target)
		// Preserve the former configured DSH destination during migration,
		// but persist it as an ordinary editable rule rather than a runtime special case.
		if dshTarget := strings.TrimSpace(replacements["DeepSeek Harness"]); dshTarget != "" {
			defaults[defaultDSHIdentityText] = "You are " + replacements["DeepSeek Harness"]
		}
		if replacements == nil {
			replacements = make(map[string]string, len(defaults))
		}
		updated := false
		for old, newValue := range defaults {
			if _, exists := replacements[old]; !exists {
				replacements[old] = newValue
				updated = true
			}
		}
		if !updated {
			return false, nil
		}
		encoded, err := json.Marshal(replacements)
		if err != nil {
			return false, err
		}
		identity[replacementsKey] = encoded
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return false, err
	}
	doc[identityKey] = encoded
	return true, nil
}

func writeNewRuntimeConfig(path string) error {
	data := append([]byte(nil), embeddedDefaultRuntimeConfig...)
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := fd.Write(data); err != nil {
		fd.Close()
		os.Remove(path)
		return err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Close(); err != nil {
		return err
	}
	log.Printf("[Config initialization] generated default config.json")
	return nil
}

func loadRuntimeConfig(path string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte("{}")
	} else if err != nil {
		return fmt.Errorf("reading config file %s: %w", path, err)
	}
	c, err := parseRuntimeConfig(data)
	if err != nil {
		return fmt.Errorf("parsing config file %s: %w", path, err)
	}
	if err := applyRuntimeConfig(c, false, ""); err != nil {
		return err
	}
	runtimeAppliedHash, runtimeHasAppliedHash = sha256.Sum256(data), true
	return nil
}

// Fully validated before the call. Prepare resources that can fail first, then
// hot-switch; existing connections/streams are not interrupted.
func applyRuntimeConfig(c runtimeFileConfig, hot bool, trace string) error {
	profile, err := normalizeRequestProfile(c.Upstream.RequestProfile)
	if err != nil {
		return err
	}
	if hot {
		g, _ := normalizeGatewayConfig(c.Gateway)
		gatewayConfigMu.Lock()
		if err := applyGatewayListenerPortsLocked(g, trace, func() error {
			return switchDebugLogging(c.Debug.Enabled)
		}); err != nil {
			gatewayConfigMu.Unlock()
			return err
		}
		gatewaySettings = g
		setGatewayKeysLocked(g)
		gatewayConfigMu.Unlock()
	} else if err := applyGatewayConfig(c.Gateway); err != nil {
		return err
	}
	if !hot {
		cfg.DebugEnabled = c.Debug.Enabled // written to cfg only at startup; decided by the sink at runtime.
	}
	previousFull := debugFullRequests.Swap(c.Debug.FullRequests)
	if previousFull != c.Debug.FullRequests || !hot && c.Debug.FullRequests {
		log.Printf("[Full request logging] traceId=%s configEnabled=%t debugLogEnabled=%t note=model API only; inbound and actual outbound requests are fully recorded to the debug log, known credentials are redacted, prompt code and images remain sensitive data; no count or duration limits, disable after troubleshooting",
			trace, c.Debug.FullRequests, c.Debug.Enabled)
	}
	setModelFilter(c.Models.Blocklist, c.Models.Allowlist)
	_ = setModelAccountFilter(c.Models.Accounts)
	setSystemPromptConfig(c.SystemPrompt.Fallback, c.SystemPrompt.Force)
	setIdentityConfig(c.Identity)
	deprecatedPromptCompatibilityPresent.Store(len(c.DeprecatedPromptCompatibility) > 0)
	modelConcurrency.configure(c.Concurrent)
	logConcurrentConfig(trace)
	logIdentityRuntimeConfig(trace)
	previousProfile := officialRequests.Load()
	setRequestProfile(profile)
	// Without upstream.requestProfile configured, it falls back to legacy and the
	// outbound request body is therefore not compressed.
	// This fallback used to be silent: after an upgrade it could only be inferred
	// indirectly from "the request body did not shrink", so print it once
	// unconditionally at startup.
	profileSource := "config.json upstream.requestProfile"
	if c.Upstream.RequestProfile == "" {
		profileSource = "default: config.json does not provide upstream.requestProfile"
	}
	switch {
	case !hot:
		log.Printf("[Outbound compat config] traceId=%s profile=%s source=%s result=applied note=legacy does not compress the outbound request body, official enables outbound gzip and desktop fingerprints; only affects later model requests, does not modify login or credentials",
			trace, profile, profileSource)
	case previousProfile != officialRequests.Load():
		log.Printf("[Outbound compat config] traceId=%s profile=%s result=applied note=only affects later model requests, in-flight requests keep the original snapshot, login and credentials are not modified", trace, profile)
	}
	logRetentionDays.Store(int64(c.Logs.RetentionDays))
	header, idle, retries := upstreamHeaderTimeoutDefault, upstreamIdleTimeoutDefault, upstreamTransientRetriesDefault
	if n := c.Upstream.HeaderTimeoutSeconds; n > 0 {
		header = time.Duration(n) * time.Second
	}
	if n := c.Upstream.IdleTimeoutSeconds; n > 0 {
		idle = time.Duration(n) * time.Second
	}
	if n := c.Upstream.TransientRetries; n != nil && *n >= 0 {
		retries = *n
	}
	keepalive := streamKeepaliveDefault
	if n := c.Upstream.StreamKeepaliveSeconds; n > 0 {
		keepalive = time.Duration(n) * time.Second
	}
	firstByteRetry := enableFirstByteRetryDefault
	if n := c.Upstream.EnableFirstByteRetry; n != nil {
		firstByteRetry = *n
	}
	upstreamConfigMu.Lock()
	upstreamHeaderTimeout, upstreamIdleTimeout, upstreamTransientRetries = header, idle, retries
	streamKeepalive = keepalive
	enableFirstByteRetry = firstByteRetry
	upstreamConfigMu.Unlock()
	if hot {
		log.Printf("[Config hot reload] traceId=%s result=success modelBlocklist=%d modelAllowlist=%d accountRules=%d debugLog=%t logRetentionDays=%d headerTimeout=%v streamIdleTimeout=%v retries=%d streamHeartbeat=%v firstByteReplay=%t effectiveAPIPort=%d effectiveWebPort=%d note=later requests use the new config, credentials and historical ledgers are preserved; listen ports hot-switched",
			trace, len(c.Models.Blocklist), len(c.Models.Allowlist), len(c.Models.Accounts), c.Debug.Enabled, c.Logs.RetentionDays, header, idle, retries, keepalive, firstByteRetry, cfg.Port, cfg.WebPort)
	}
	return nil
}

// Logged once more after startup log initialization, so the deprecated field
// notice is persisted like the hot reload result.
// Replacement word bodies are not recorded, avoiding private text from the old
// config leaking into ordinary logs.
func logIdentityRuntimeConfig(trace string) {
	enabled, target, rules := identitySettingsSummary()
	literal, wildcard, invalid := identityRuleDetails()
	log.Printf("[Identity replacement config] traceId=%s enabled=%t target=%s ruleCount=%d literalRules=%d leadingWildcardRules=%d ignoredInvalidRules=%d note=all text replacements follow identity.replacements; only system/developer/assistant text is processed; hot reload only affects later requests",
		trace, enabled, target, rules, literal, wildcard, invalid)
	if invalid > 0 {
		log.Printf("[Replacement rule warning] traceId=%s ignoredCount=%d reason=empty key or invalid wildcard boundary note=wildcards support only one (.*), must have non-empty leading and trailing text, and must not cross lines; other valid rules still apply, rule bodies are not recorded",
			trace, invalid)
	}
	if deprecatedPromptCompatibilityPresent.Load() {
		log.Printf("[Deprecated config] traceId=%s field=promptCompatibility result=ignored note=the duplicate replacement module has been removed; configure rules you want to keep in identity.replacements, then the old field can be deleted after confirmation; old rule bodies are not recorded",
			trace)
	}
}

func upstreamSettings() (time.Duration, time.Duration, int) {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return upstreamHeaderTimeout, upstreamIdleTimeout, upstreamTransientRetries
}

func currentIdleTimeout() time.Duration {
	_, idle, _ := upstreamSettings()
	return idle
}

// currentStreamKeepalive returns the current stream heartbeat interval
// (0 means disabled).
func currentStreamKeepalive() time.Duration {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return streamKeepalive
}

// currentFirstByteRetry returns the current replay-before-first-byte switch.
func currentFirstByteRetry() bool {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return enableFirstByteRetry
}

func switchDebugLogging(enabled bool) error {
	debugSinkMu.Lock()
	defer debugSinkMu.Unlock()
	if enabled == (debugSink != nil) {
		return nil
	}
	if enabled {
		f, err := newDailyLogWriter("debug", ".jsonl")
		if err != nil {
			return err
		}
		debugSink = &debugJSONSink{writer: f, closer: f}
	} else {
		sink := debugSink
		debugSink = nil
		sink.mu.Lock()
		if sink.closer != nil {
			_ = sink.closer.Close()
		}
		sink.mu.Unlock()
	}
	return nil
}

// Detects changes by content digest, so edits with the same size/timestamp are
// not missed.
// Missing/half-written/invalid configs keep the previous valid config; the same
// bad content warns only once.
type runtimeConfigWatcher struct {
	failed    [32]byte
	hasFailed bool
	lastError error
}

func (watcher *runtimeConfigWatcher) reload() bool {
	watcher.lastError = nil
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	data, err := os.ReadFile(runtimeConfigFile)
	sum := sha256.Sum256(data)
	if err != nil {
		watcher.lastError = err
		sum = sha256.Sum256([]byte(err.Error()))
	}
	if err == nil && runtimeHasAppliedHash && runtimeAppliedHash == sum {
		watcher.hasFailed = false
		return false
	}
	trace := newTraceID()
	var c runtimeFileConfig
	if err == nil {
		c, err = parseRuntimeConfig(data)
	}
	if err == nil {
		err = applyRuntimeConfig(c, true, trace)
	}
	if err != nil {
		if !watcher.hasFailed || watcher.failed != sum {
			log.Printf("[Config hot reload] traceId=%s result=rejected reason=%s businessImpact=config not switched, previous valid config kept; fix the file and save again", trace, safeDebugError(err))
		}
		watcher.lastError = err
		watcher.failed, watcher.hasFailed = sum, true
		return false
	}
	runtimeAppliedHash, runtimeHasAppliedHash, watcher.hasFailed = sum, true, false
	return true
}

func runtimeConfigLoop(ctx context.Context) {
	watcher := runtimeConfigWatcher{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastLogged time.Time
	var previousStatus string
	logScheduledTask("config_reload", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(time.Second), "")
	if logRetentionDays.Load() == 0 {
		logScheduledTask("log_cleanup", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "retention days is 0, no automatic cleanup")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-ticker.C:
			next := tick.Add(time.Second)
			started := time.Now()
			changed := watcher.reload()
			status, detail := "success", "check complete, config unchanged"
			if changed {
				detail = "new config validated and applied"
			}
			if watcher.lastError != nil {
				status, detail = "failed", safeDebugError(watcher.lastError)
			}
			logFrequentTask("config_reload", status, detail, started, next, &lastLogged, &previousStatus, changed)
			scheduledLogCleanup(time.Now())
		}
	}
}

// A Transport cannot have its fields changed in place during active requests.
// Clone only when timeouts change; new requests use the new Transport, old
// streams keep reading, idle connections are reclaimed.
type hotConfigTransport struct {
	mu        sync.Mutex
	transport *http.Transport
}

func (t *hotConfigTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	header, _, _ := upstreamSettings()
	t.mu.Lock()
	if t.transport.ResponseHeaderTimeout != header {
		old := t.transport
		t.transport = old.Clone()
		t.transport.ResponseHeaderTimeout = header
		old.CloseIdleConnections()
	}
	current := t.transport
	t.mu.Unlock()
	return current.RoundTrip(r)
}

func (t *hotConfigTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.transport.CloseIdleConnections()
}
