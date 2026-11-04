package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skip2/go-qrcode"
)

const (
	version = "1.14.43"

	// Status snapshot file name: written periodically by serve in the background,
	// read live by the monitor front-end command.
	statusSnapshotFile = "workbuddy-status.json"
	logDir             = "logs"

	// defaultSystemPrompt is the fallback system prompt injected when the client's
	// first message is not a system message. It satisfies the Tencent upstream's
	// hard requirement that the first message be a system prompt (code 11128).
	defaultSystemPrompt = "You are a helpful assistant."

	// upstreamHeaderTimeout caps the wait from "request sent" to "upstream response
	// headers received".
	//
	// The value must be generous: queuing and preprocessing of 3MB+ requests at the
	// upstream can approach one minute by itself (a 2.3MB request was observed taking
	// 50.8s before responding with headers). An earlier 60s limit wrongly killed such
	// normal-but-slow requests, surfacing as 502 "timeout awaiting response headers".
	// The default of 5 minutes only covers the "upstream completely unresponsive" case;
	// long-lived streams without data are interrupted by the upstreamIdleTimeout idle
	// watchdog instead. Overridable via config.json.
	upstreamHeaderTimeoutDefault = 5 * time.Minute

	// upstreamIdleTimeoutDefault is the "idle read timeout" for streaming responses:
	// as long as data keeps arriving the stream never times out; only after this
	// duration without any new data is the upstream considered stuck and interrupted.
	upstreamIdleTimeoutDefault = 120 * time.Second

	// defaultRequestTimeout applies to short control requests such as token refresh,
	// quota queries, and model catalog fetches.
	defaultRequestTimeout = 60 * time.Second

	// upstreamTransientRetries is the number of extra retries for transient network
	// errors during the request-body send phase.
	//
	// Background: the single TCP connection between the gateway and an upstream CDN
	// edge node may be reset by the peer (connection reset by peer), closed (use of
	// closed network connection), or hit a recycled keep-alive connection. Such errors
	// are transient and independent of request body size (>5MB requests succeeded 95%
	// of the time while 0.5MB requests also failed occasionally).
	//
	// Retry only when it is confirmed the request headers have not been written; the
	// absence of a response header does not prove the upstream never processed the
	// POST. Retries force a new connection, default to at most 2 extra attempts, and
	// are overridable via config.json.
	upstreamTransientRetriesDefault = 2

	// upstreamRetryBackoff is the wait between retries, giving the upstream edge node
	// time to recover.
	upstreamRetryBackoffDefault = 300 * time.Millisecond

	// streamKeepaliveDefault is the interval for sending SSE comments downstream while
	// waiting for the first valid data from the upstream. Tencent edge nodes are
	// sensitive to connections whose headers were sent but whose first byte is long
	// delayed; the default is 15s.
	streamKeepaliveDefault = 15 * time.Second
	// enableFirstByteRetryDefault allows one safe replay when the client has not yet
	// received any valid data.
	enableFirstByteRetryDefault = true
)

var errModelAccountPolicy = errors.New("model account policy rejected all accounts")

// errStreamClosedWithoutFinish means the upstream connection was read to the end
// normally (no read error), but no non-empty finish_reason ever appeared in the
// stream. An empty string does not count: the upstream sends finish_reason:"" on
// every intermediate chunk. Such an ending must not be treated as a complete reply,
// otherwise downstream would accept truncated output as a successful result.
var errStreamClosedWithoutFinish = errors.New("upstream stream ended cleanly but without finish_reason; cannot be treated as a complete reply")

// Effective timeout values (defaulting to the Defaults above; overridable by the
// upstream section of config.json). Defined as variables so tests can lower the
// thresholds and operators can tune them to network conditions.
var (
	upstreamHeaderTimeout = upstreamHeaderTimeoutDefault
	upstreamIdleTimeout   = upstreamIdleTimeoutDefault
	// upstreamTransientRetries is the effective transient network error retry count.
	upstreamTransientRetries = upstreamTransientRetriesDefault
	// upstreamRetryBackoff is the effective retry interval (lowerable in tests).
	upstreamRetryBackoff = upstreamRetryBackoffDefault
	// streamKeepalive is the effective streaming heartbeat interval (0 disables it).
	streamKeepalive = streamKeepaliveDefault
	// enableFirstByteRetry is the effective first-byte replay switch.
	enableFirstByteRetry = enableFirstByteRetryDefault
)

// isTransientNetworkError reports whether the error is a "transient network
// failure" worth retrying.
//
// Covers: connection reset/aborted (RST), connection closed by the peer, broken
// pipe, forcibly closed connections, and the common Go keep-alive reuse error
// "server closed idle connection".
// Note: context.Canceled and DeadlineExceeded do not belong here — the former is
// a client-initiated disconnect and the latter is an explicit timeout; retrying
// either is pointless or even harmful.
//
// On Windows the same failures use different messages than POSIX: Winsock
// send/receive failures appear as "An existing connection was forcibly closed by
// the remote host" (WSAECONNRESET, often with a wsasend:/wsarecv: prefix) or
// "An established connection was aborted by the software in your host machine"
// (WSAECONNABORTED), neither containing "connection reset by peer". Both message
// sets must be matched, otherwise a Windows RST would be treated as permanent:
// doUpstreamRequest would not retry and canReplayStream would refuse a
// first-byte replay.
func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// Client cancellation or timeout: do not retry
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// A network-layer timeout (e.g. ResponseHeaderTimeout) means the upstream
		// genuinely did not respond; retrying is allowed.
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset by peer",
		"connection reset",
		"broken pipe",
		"use of closed network connection",
		"connection refused",
		"server closed idle connection",
		"unexpected eof",
		"http2: server sent goaway",
		"stream error",
		// Windows Winsock: WSAECONNRESET / WSAECONNABORTED messages differ from POSIX
		// and do not contain "connection reset by peer". msg is already lowercased;
		// these markers must stay lowercase.
		"forcibly closed",
		"an established connection was aborted",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// legacyLargeBodyWarnBytes is the threshold for the "large uncompressed request
// body" warning. Outbound bodies are not compressed under the legacy profile and
// upload time grows linearly with size, raising the chance of peer interruption
// on uplink jitter, so the config knob is mentioned once past this threshold.
const legacyLargeBodyWarnBytes = 512 * 1024

// legacyLargeBodyWarned ensures this hint is logged at most once per process,
// avoiding per-request spam.
var legacyLargeBodyWarned atomic.Bool

// doUpstreamRequest sends the upstream request with bounded retries for transient
// network errors.
//
// Retries only while the complete request headers have not been written; an EOF
// after sending a POST cannot tell whether the upstream ran the generation, so
// replaying is unsafe. Each retry rebuilds the request object and body reader.
//
// The returned cancel cancels the upstream context after streaming ends (the
// caller owns it).
func doUpstreamRequest(r *http.Request, acc *Account, prof *upstreamProfile, upstreamBytes []byte, reqID uint64, traceID string) (*http.Response, context.CancelFunc, error) {
	// A consistent credential snapshot is shared by all retries of this request.
	// Hot reload must not mix token fields across concurrent sends.
	accountMu.Lock()
	var auth *StoredAuth
	if acc != nil && acc.Auth != nil {
		value := *acc.Auth
		auth = &value
	}
	accountMu.Unlock()
	// Covers direct calls and tests; the normal entry point prepares this before
	// protocol conversion. Generate once; never rebuild inside the try loop.
	prepareConversationContext(r, nil)
	profile := requestProfileFor(r)
	wireBytes, encodeErr := encodeModelRequest(upstreamBytes, profile, prof)
	if encodeErr != nil {
		log.Printf("[Outbound compat] traceId=%s requestId=%d stage=request-encoding result=failed business_impact=not sent upstream", traceID, reqID)
		debugEvent(r, "error", "upstream_body_encode_failed", map[string]any{"status_code": 502})
		return nil, nil, encodeErr
	}
	if profile == requestProfileLegacy && len(wireBytes) >= legacyLargeBodyWarnBytes && legacyLargeBodyWarned.CompareAndSwap(false, true) {
		log.Printf("[Outbound compat] traceId=%s requestId=%d bodyBytes=%d profile=legacy uncompressed note=Outbound body is uncompressed; upload time grows linearly with size and uplink jitter makes peer interruption more likely. To enable outbound gzip set upstream.requestProfile=official in config.json",
			traceID, reqID, len(wireBytes))
		debugEvent(r, "info", "outbound_body_uncompressed_large", map[string]any{
			"wire_body_bytes": len(wireBytes), "profile": profile,
			"business_impact": "config hint only; outbound behavior unchanged",
		})
	}
	var lastErr error
	_, _, retries := upstreamSettings()
	for try := 0; try <= retries; try++ {
		upstreamCtx, upstreamCancel := context.WithCancel(r.Context())
		var headersWritten, bodyWritten atomic.Bool
		trace := &httptrace.ClientTrace{
			WroteHeaders: func() { headersWritten.Store(true) },
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					bodyWritten.Store(true)
				}
			},
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(upstreamCtx, trace), http.MethodPost, prof.chatURL(), bytes.NewReader(wireBytes))
		if err != nil {
			upstreamCancel()
			return nil, nil, err
		}
		backendHeaders(req, auth, prof)
		applyObservedDesktopHeaders(req, prof, profile)
		if usesObservedDesktopHeaders(profile, prof) {
			req.Header.Set("Content-Encoding", "gzip")
		}
		debugEvent(r, "debug", "upstream_request_profile_applied", map[string]any{
			"profile": profile, "desktop_headers": usesObservedDesktopHeaders(profile, prof),
			"decoded_body_bytes": len(upstreamBytes), "wire_body_bytes": len(wireBytes),
			"site": prof.Key, "business_impact": "model outbound only; international site reuses original headers, no auth or body logged",
		})
		if debugLoggingEnabled() {
			req.Header.Set("X-Trace-ID", debugTraceID(r))
			req.Header.Set("X-Parent-Request-ID", strconv.FormatUint(reqID, 10))
		}
		if try > 0 {
			// Do not reuse an idle connection already closed by the peer when retrying.
			req.Close = true
			cfg.HttpClient.CloseIdleConnections()
			log.Printf("[Network retry] traceId=%s requestId=%d account=%s retry %d/%d; previous attempt headers not written, connection rebuilt (error: %s)",
				traceID, reqID, acc.Path, try, retries, safeDebugError(lastErr))
		}

		debugFullUpstreamRequest(r, req, upstreamBytes, try)
		resp, err := cfg.HttpClient.Do(req)
		debugFullUpstreamResponse(r, req, resp, try, headersWritten.Load(), bodyWritten.Load(), err)
		if err == nil {
			return resp, upstreamCancel, nil
		}
		upstreamCancel()
		lastErr = err
		if !isTransientNetworkError(err) {
			return nil, nil, err
		}
		if headersWritten.Load() || bodyWritten.Load() {
			log.Printf("[Network retry skipped] traceId=%s requestId=%d account=%s headersWritten=%t bodyWritten=%t reason=upstream may have processed the POST; replay could duplicate generation error=%s",
				traceID, reqID, acc.Path, headersWritten.Load(), bodyWritten.Load(), safeDebugError(err))
			debugEvent(r, "warn", "upstream_retry_skipped", map[string]any{
				"reason": "request_may_have_reached_upstream", "headers_written": headersWritten.Load(),
				"body_written": bodyWritten.Load(), "business_impact": "avoids re-running the model request",
			})
			return nil, nil, err
		}
		if try < retries {
			debugEvent(r, "warn", "upstream_send_retry", map[string]any{
				"retry": try + 1, "reason": "request_headers_not_written",
				"business_impact": "retries the not-yet-sent model request on a new connection",
			})
			select {
			case <-r.Context().Done():
				return nil, nil, err
			case <-time.After(upstreamRetryBackoff):
			}
		}
	}
	return nil, nil, lastErr
}

// -----------------------------------------------------------------------------
// Upstream site profiles (domestic / international)
//
// The international site www.workbuddy.ai and the domestic site
// copilot.tencent.com share the same /v2/plugin/* protocol (verified: auth/state,
// auth/token, login/account, auth/token/refresh, and chat/completions use
// identical paths and response envelopes). The differences are only:
//   - upstream domain and Web Origin (the international site sits on Tencent
//     EdgeOne's international CDN)
//   - login platform parameter (workbuddy-ai instead of VSCode); login completes
//     in the browser (email / verification code / SSO)
//   - while waiting for authorization the auth/token poll returns code 11217
//     (login ing...)
// Each account credential file records its site via the edition field; the
// account pool supports mixed domestic/international round-robin.
// -----------------------------------------------------------------------------

type upstreamProfile struct {
	Key         string        // Site key stored in the credential file's edition field
	Label       string        // Display name in the console
	Base        string        // Base URL for auth, refresh, quota, and other APIs
	DefaultBase string        // Production default Base; tests/local overriding Base do not use ChatBase
	ChatBase    string        // Base URL used only for model requests
	Origin      string        // Origin/Referer spoofing source (each site's web console)
	Platform    string        // platform parameter of auth/state
	ClientUA    string        // User-Agent
	ClientID    string        // X-Client-ID
	ClientVer   string        // X-Client-Version
	Product     string        // X-Product
	LoginTTL    time.Duration // Timeout for the login command to wait for authorization
}

var (
	profileCN = upstreamProfile{
		Key: "cn", Label: "Internal site",
		Base: "https://copilot.tencent.com", DefaultBase: "https://copilot.tencent.com",
		ChatBase: "https://www.workbuddy.cn", Origin: "https://www.codebuddy.cn",
		Platform: "VSCode", ClientUA: "CLI/2.143.1 CodeBuddy/2.143.1",
		ClientID: "codebuddy-cli", ClientVer: "2.143.1", Product: "SaaS",
		LoginTTL: 5 * time.Minute,
	}
	profileINTL = upstreamProfile{
		Key: "intl", Label: "International site",
		Base: "https://www.workbuddy.ai", DefaultBase: "https://www.workbuddy.ai",
		ChatBase: "https://www.workbuddy.ai", Origin: "https://www.workbuddy.ai",
		Platform: "workbuddy-ai", ClientUA: "CLI/2.143.1 CodeBuddy/2.143.1",
		ClientID: "codebuddy-cli", ClientVer: "2.143.1", Product: "SaaS",
		LoginTTL: 15 * time.Minute, // Browser login (email/code/SSO) is slower than QR scan; allow more time
	}
	profileCodebuddy = upstreamProfile{
		Key: "codebuddy", Label: "CodeBuddy International site (codebuddy.ai)",
		Base: "https://www.codebuddy.ai", DefaultBase: "https://www.codebuddy.ai",
		ChatBase: "https://www.codebuddy.ai", Origin: "https://www.codebuddy.ai",
		Platform: "CLI", ClientUA: "CLI/2.143.1 CodeBuddy/2.143.1",
		ClientID: "codebuddy-cli", ClientVer: "2.143.1", Product: "SaaS",
		LoginTTL: 15 * time.Minute,
	}
)

// profileForEdition returns the upstream site profile for an edition identifier
// from a credential file; empty or unknown values fall back to the domestic site.
func profileForEdition(edition string) *upstreamProfile {
	switch strings.ToLower(strings.TrimSpace(edition)) {
	case "intl", "international", "global", "workbuddy.ai", "workbuddy":
		return &profileINTL
	case "codebuddy", "cb", "codebuddy.ai":
		return &profileCodebuddy
	case "internal", "ioa", "domestic", "cn", "":
		return &profileCN
	default:
		return &profileCN
	}
}

func (p *upstreamProfile) authStateURL() string {
	return p.Base + "/v2/plugin/auth/state?platform=" + url.QueryEscape(p.Platform)
}

func (p *upstreamProfile) loginAcctURL(state string) string {
	return p.Base + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
}

func (p *upstreamProfile) authTokenURL(state string) string {
	return p.Base + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
}

func (p *upstreamProfile) tokenRefreshURL() string { return p.Base + "/v2/plugin/auth/token/refresh" }

func (p *upstreamProfile) chatURL() string {
	base := p.Base
	// Tests or local runs may point Base at a fake upstream; only the production
	// default Base switches to ChatBase.
	if p.ChatBase != "" && (p.DefaultBase == "" || p.Base == p.DefaultBase) {
		base = p.ChatBase
	}
	return base + "/v2/chat/completions"
}

func (p *upstreamProfile) quotaSummaryURL() string {
	return p.Origin + "/billing/meter/get-user-resource-summary"
}

func (p *upstreamProfile) enterpriseUsageURL() string {
	// External project measurements on enterprise accounts show that
	// get-user-resource* returns an empty payload for enterpriseId; ask for the
	// allocated quota instead: credit = used this cycle, limitNum = allocated quota.
	return p.Origin + "/v2/billing/meter/get-enterprise-user-usage"
}

func (p *upstreamProfile) dailyCheckinURL() string {
	return p.Origin + "/v2/billing/meter/daily-checkin"
}

// -----------------------------------------------------------------------------
// Data structure definitions
// -----------------------------------------------------------------------------

type StoredAuth struct {
	Auth    StoredTokens  `json:"auth"`
	Account StoredAccount `json:"account"`
	Edition string        `json:"edition,omitempty"` // Site key: cn (domestic, default) | intl (www.workbuddy.ai)
}

type StoredTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	// Local renewal metadata below; the official client never reads it. Legacy
	// credential files default to 0 and parse fine.
	RefreshExpiresAt int64 `json:"refreshExpiresAt,omitempty"` // Refresh token expiry (Unix seconds); 0 = unknown
	LastRefreshTime  int64 `json:"lastRefreshTime,omitempty"`  // Last successful renewal (Unix seconds); 0 = never renewed
}

type StoredAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

type quotaPackage struct {
	CycleTotalCapacity  string `json:"CycleTotalCapacity"`
	CycleUsedCapacity   string `json:"CycleUsedCapacity"`
	CycleRemainCapacity string `json:"CycleRemainCapacity"`
}

type quotaSummaryData struct {
	Packages []quotaPackage `json:"Packages"`
	// IsPaidUser is the upstream's "formal paid subscription" flag; Pro trial users
	// also have this field set to false.
	IsPaidUser bool `json:"IsPaidUser"`
	// ProTrialStatus alone cannot prove the trial is still active; it must be
	// combined with current entitlement resources.
	ProTrialStatus any `json:"ProTrialStatus"`
	// SubscriptionPackageCode is the current subscription package code; non-empty
	// means a subscription package exists.
	SubscriptionPackageCode string `json:"SubscriptionPackageCode"`
}

// -----------------------------------------------------------------------------
// Global state and configuration
// -----------------------------------------------------------------------------

type Config struct {
	Addr               string
	Port               int
	PortExplicit       bool
	WebPort            int
	AdminKey           string
	AuthFile           string
	AuthDir            string
	AuthExplicit       bool // Whether the user explicitly passed -auth (otherwise scan the directory for all workbuddy*.json)
	LoginIntl          bool // login -intl: log into the international site (www.workbuddy.ai, login completes in browser)
	LoginCodebuddy     bool // login -codebuddy: log into the CodeBuddy international site (www.codebuddy.ai, CLI browser login)
	APIKey             string
	ProxyURL           string
	Verbose            bool
	DebugEnabled       bool   // Controlled only by debug.enabled in the working-directory config.json
	ReloadInterval     int    // Account pool hot-reload scan interval (seconds); 0 disables
	MonitorInterval    int    // monitor status refresh interval (seconds)
	LogFile            string // Extra log file path shown by monitor
	JournalService     string // Extra systemd service name followed by monitor (journalctl -u)
	LogLines           int    // Number of recent log lines shown by monitor
	ModelsRefresh      int    // Official model catalog refresh interval (minutes); 0 disables
	AllowPriceProbes   bool   // Only an explicit opt-in enables background price probes; real generation may consume credits
	DisablePriceProbes bool   // Legacy disable flag, takes precedence over AllowPriceProbes; does not affect client requests or explicit probe runs
	KeepaliveHours     []int  // Proactive renewal hours (local time); empty disables; refreshes all accounts on schedule
	ProbeModels        string // probe only: comma-separated model list
	ProbeLimit         int    // probe only: number of models to pick when none specified
	WebUI              bool   // Only -webui starts the separate admin port
	HttpClient         *http.Client
}

// Account represents one CodeBuddy account credential and its runtime state.
type Account struct {
	Path           string                        // Credential file path
	Auth           *StoredAuth                   // Credential content (may be nil when restored from a disabled marker)
	CooldownUntil  time.Time                     // Cooldown end time (blocked until then after a 429 rate limit)
	CooldownMsg    string                        // Reason/upstream hint that triggered the cooldown
	Disabled       bool                          // Authorization failed (token expired and unrenewable/revoked): excluded from scheduling
	DisabledReason string                        // Failure reason, shown in the console
	Nickname       string                        // Disabled-marker persisted field: nickname (used when Auth is nil)
	UID            string                        // Disabled-marker persisted field: UID
	Edition        string                        // Site key (cn/intl): visible even when Auth is nil after marker restoration
	QuotaTotal     float64                       // Cycle total quota from the latest quota query
	QuotaUsed      float64                       // Used quota from the latest quota query
	QuotaRemaining float64                       // Remaining quota from the latest quota query
	IsPaidUser     bool                          // Paid user flag (raw upstream IsPaidUser value)
	PlanLabel      string                        // Official plan name or known short plan name
	PlanCheckedAt  int64                         // Last successful entitlement check; 0 in old snapshots
	PlanStale      bool                          // Last query failed; display marks the result stale
	QuotaKnown     bool                          // Quota has been fetched successfully at least once
	QuotaExhausted bool                          // Confirmed zero quota; cleared automatically once a scan sees recovery
	QuotaUnlimited bool                          // Enterprise limitNum=-1: unlimited allocation, never blocked for a zero balance
	ModelStates    map[string]*modelRuntimeState // Per-model cost, rate-limit, and quota-block state
	fingerprint    string                        // Credential file change fingerprint (mtime+size, for hot reload)
	lock           sync.RWMutex                  // Write lock for credential maintenance; model sends share the read lock, concurrency governed by concurrent
	// Renewal failure counter (memory only, reset on restart): a single failure may be
	// upstream jitter; only consecutive failures mark the login state dead.
	RefreshFailCount int
	LastRefreshError string
}

const (
	modelCostUnknown = "unknown"
	modelCostFree    = "free"
	modelCostPaid    = "paid"
	modelProbeDelay  = 5 * time.Minute

	// modelFreeMinTokens is the minimum sample size to classify a "free model": the
	// upstream may record credit=0 even for tiny requests (e.g. probe requests),
	// which is not truly free and must not let zero-balance accounts hit paid models.
	modelFreeMinTokens = 100
)

type modelRuntimeState struct {
	CostClass     string
	CooldownUntil time.Time
	QuotaBlocked  bool
	NextProbeAt   time.Time
	LastReason    string
	ObservedAt    time.Time
}

type accountSelectionKind string

const (
	selectionNormal         accountSelectionKind = "normal"
	selectionFreeExhausted  accountSelectionKind = "free_exhausted"
	selectionProbeExhausted accountSelectionKind = "probe_exhausted"
)

// Profile returns the upstream site profile for this account (domestic/international).
func (acc *Account) Profile() *upstreamProfile {
	if acc.Auth != nil {
		return profileForEdition(acc.Auth.Edition)
	}
	return profileForEdition(acc.Edition)
}

// disabledMarker is the persisted marker for an account whose authorization
// failed (used to prompt re-login in the console after the credential file is gone).
type disabledMarker struct {
	Path       string `json:"path"`
	Reason     string `json:"reason"`
	DisabledAt int64  `json:"disabledAt"`
	Nickname   string `json:"nickname,omitempty"`
	UID        string `json:"uid,omitempty"`
	Edition    string `json:"edition,omitempty"` // Site key (cn/intl), for console display
}

// markerPath returns the disabled-marker file path next to the credential file.
func markerPath(authPath string) string {
	return authPath + ".disabled"
}

var (
	cfg        Config
	authLock   sync.RWMutex
	currAuth   *StoredAuth
	reqCounter uint64

	accountMu sync.Mutex // Account pool guard lock
	accounts  []*Account // Account pool (length 1 for a single account; identical to legacy behavior)
	rrIndex   int        // Round-robin cursor

	quotaScanTrigger = make(chan struct{}, 1)
)

// -----------------------------------------------------------------------------
// Main entry point and CLI handling
// -----------------------------------------------------------------------------

func registerAutomaticPriceProbeFlags(fs *flag.FlagSet, target *Config) {
	fs.BoolVar(&target.AllowPriceProbes, "allow-price-probes", false, "Allow background automatic price probes (off by default; real generation may consume credits)")
	// Keeps compatibility with old systemd/startup scripts, but no longer appears in
	// the primary usage text. Passing -disable-price-probes=false must not bypass the
	// explicit opt-in requirement.
	fs.BoolVar(&target.DisablePriceProbes, "disable-price-probes", false, "")
}

func (c Config) automaticPriceProbesAllowed() bool {
	// When both flags appear, disable wins, regardless of command-line ordering.
	return c.AllowPriceProbes && !c.DisablePriceProbes
}

func main() {
	if len(os.Args) > 1 {
		first := os.Args[1]
		if first == "help" || first == "-h" || first == "--help" {
			printHelp()
			return
		}
		if first == "version" || first == "-v" || first == "--version" {
			fmt.Printf("WorkBuddy Local Gateway v%s\n", version)
			return
		}
	}

	command := "serve"
	args := os.Args[1:]
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		command = os.Args[1]
		args = os.Args[2:]
	}

	fs := flag.NewFlagSet(command, flag.ExitOnError)
	// legacyAPIKeyFlag only swallows -api-key in old startup commands so upgrades do
	// not fail on an unknown flag. Nothing reads the value and it produces no log or
	// state change; the usage text is left empty so it stays out of --help and the
	// default flag list.
	// TODO(remove): pure compatibility placeholder, to be removed with its
	// registration below in a future version.
	var legacyAPIKeyFlag string
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1", "Gateway listen address")
	fs.IntVar(&cfg.Port, "port", defaultAPIPort, "API listen port (explicitly passed, overrides config.json gateway.apiPort)")
	fs.StringVar(&cfg.AuthFile, "auth", "workbuddy.json", "Credential storage file path (supports comma-separated files for multiple accounts)")
	fs.StringVar(&cfg.AuthDir, "auth-dir", "", "Credential directory: automatically loads every workbuddy*.json as an account pool")
	fs.StringVar(&cfg.ProxyURL, "proxy", "", "Optional: upstream request proxy (e.g. http://127.0.0.1:7890)")
	fs.StringVar(&legacyAPIKeyFlag, "api-key", "", "")
	// Custom flag error message so the default PrintDefaults does not list the
	// compatibility placeholder flag.
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Invalid arguments; run %s --help to see available flags\n", os.Args[0])
	}
	fs.BoolVar(&cfg.Verbose, "verbose", false, "Output verbose debug logs")
	fs.BoolVar(&cfg.LoginIntl, "intl", false, "login only: log into the international site (www.workbuddy.ai, browser login); default is the domestic site")
	fs.BoolVar(&cfg.LoginCodebuddy, "codebuddy", false, "login only: log into the CodeBuddy international site (www.codebuddy.ai, CLI browser login); default is the domestic site")
	fs.IntVar(&cfg.MonitorInterval, "interval", 3, "monitor status refresh interval (seconds)")
	fs.IntVar(&cfg.ReloadInterval, "reload-interval", 5, "Account pool hot-reload scan interval (seconds), 0 disables: newly added/updated/deleted credential files are picked up at runtime without restart")
	fs.StringVar(&cfg.LogFile, "logfile", "", "Extra log file followed by monitor (e.g. -logfile /var/log/workbuddy-gateway.log)")
	fs.StringVar(&cfg.JournalService, "journal", "", "Extra systemd service name followed by monitor (on Linux uses journalctl -u <service> -f)")
	fs.IntVar(&cfg.LogLines, "lines", 15, "Recent log lines shown on each monitor refresh")
	fs.IntVar(&cfg.ModelsRefresh, "models-refresh", 60, "Model catalog refresh interval (minutes), 0 disables (live API + npm merge)")
	registerAutomaticPriceProbeFlags(fs, &cfg)
	keepaliveHours := hourList{22}
	fs.Var(&keepaliveHours, "keepalive-hours", "Proactive renewal hours (local time, comma separated, default 22); empty disables")
	fs.StringVar(&cfg.ProbeModels, "models", "", "probe only: comma-separated models to probe (defaults to the first few in the catalog)")
	fs.IntVar(&cfg.ProbeLimit, "limit", 5, "probe only: model count cap when -models is not specified")
	fs.BoolVar(&cfg.WebUI, "webui", false, "Enable the standalone web console; admin key, API auth, and ports are configured in config.json")
	_ = fs.Parse(args)
	cfg.KeepaliveHours = []int(keepaliveHours)

	// Detect whether -auth was explicitly passed:
	// without -auth and without -auth-dir, scan the current directory for all
	// workbuddy*.json to form the account pool, so dropping multiple credential
	// files into the working directory yields multi-account with no extra flags.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auth" {
			cfg.AuthExplicit = true
		}
		if f.Name == "port" {
			cfg.PortExplicit = true
		}
	})
	serveCommand := command == "serve" || command == "run" || command == "start"
	if serveCommand || command == "probe" {
		if err := ensureRuntimeConfigFile(runtimeConfigFile); err != nil {
			fmt.Fprintf(os.Stderr, "Config initialization warning: %v (continuing with in-memory defaults)\n", err)
		}
		if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
			fmt.Fprintf(os.Stderr, "Startup failed: %v\n", err)
			os.Exit(1)
		}
		ignoreLegacyAPIKeyFlag(legacyAPIKeyFlag)
	}

	// Initialize the HTTP client
	initHTTPClient()
	closeLog := initFileLogging(command)
	defer closeLog()
	if serveCommand || command == "probe" {
		// Config is loaded before logging is initialized. Persist its effective
		// runtime settings now as well, rather than leaving them console-only.
		trace := newTraceID()
		logConcurrentConfig(trace)
		logIdentityRuntimeConfig(trace)
		identityEnabled, identityTarget, identityRules := identitySettingsSummary()
		log.Printf("[Config ready] traceId=%s identityEnabled=%t identityTarget=%s identityRuleCount=%d note=Startup config loaded; later changes hot-reload; keys and config body are not logged",
			trace, identityEnabled, identityTarget, identityRules)
	}
	closeDebug := func() {}
	if serveCommand {
		var err error
		closeDebug, err = initDebugLogging()
		if err != nil {
			log.Fatalf("[Debug] Failed to initialize debug logging: %v", err)
		}
	}
	defer closeDebug()

	switch command {
	case "serve", "run", "start":
		runServe()
	case "login":
		runLogin()
	case "status":
		runStatus()
	case "refresh":
		runRefresh()
	case "monitor":
		runMonitor()
	case "probe":
		runProbe()
	case "reset":
		runReset()
	case "version", "-v", "--version":
		fmt.Printf("WorkBuddy Local Gateway v%s\n", version)
	default:
		fmt.Printf("Unknown command: %s\n\n", command)
		printHelp()
		os.Exit(1)
	}
}

// initFileLogging writes runtime logs to both the console and a date-named project log file.
func initFileLogging(command string) func() {
	gatewayConfigMu.RLock()
	registerSecrets(cfg.APIKey, cfg.AdminKey, gatewaySettings.APIKey)
	gatewayConfigMu.RUnlock()
	// Even when file initialization fails, secrets stay hidden on the console.
	log.SetOutput(&redactingLogWriter{writer: os.Stderr})
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		log.Printf("[Log] Cannot create log directory %s; logging to console only: %v", logDir, err)
		return func() {}
	}
	path := filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log")
	f, err := newDailyLogWriter("gateway", ".log")
	if err != nil {
		log.Printf("[Log] Cannot open log file %s; logging to console only: %v", path, err)
		return func() {}
	}
	log.SetOutput(&redactingLogWriter{writer: io.MultiWriter(os.Stderr, f)})
	log.Printf("[Log] Audit logging enabled, file=%s, command=%s, version=%s", path, command, version)
	return func() { _ = f.Close() }
}

func printHelp() {
	fmt.Println(`WorkBuddy Local Gateway - lightweight cross-platform local AI gateway
Proxies Tencent CodeBuddy / Hunyuan behind a standard OpenAI protocol, passing any model straight through to the upstream.
Supports a multi-account pool: round-robins across accounts to balance quota; when an account hits a 429 rate limit it
is automatically cooled down and the remaining accounts compensate, resuming automatically once the cooldown expires.

Usage:
  workbuddy-gateway [command] [options]

Commands:
  serve       Start the local gateway (default action)
  login       Log in and obtain/update credentials: domestic site via WeChat QR scan; -intl logs into www.workbuddy.ai; -codebuddy logs into the CodeBuddy international site www.codebuddy.ai (in-browser)
  status      Show account pool status (site, cooldown state, and token expiry)
  refresh     Manually refresh the access token of every account right now
  monitor     Front-end live monitor: periodically refreshes account status + recent logs (Ctrl+C to exit)
  probe       Proactively probe whether accounts can use given models for free or paid (requires a running serve)
  reset       Clear all local data except login credentials (state/cache/logs/disabled markers), then re-fetch models and multipliers
  version     Show version information
  help        Show this help

Flags:
  -addr <ip>        Gateway listen address (default: 127.0.0.1)
  -port <port>      Gateway listen port (default: 8317)
  -auth <path>      Credential file path; supports comma-separated files for multiple accounts
                    (default: auto-discover all workbuddy*.json in the current directory)
  -auth-dir <dir>   Credential directory: automatically loads every workbuddy*.json as the account pool
  -intl             login only: log into the international site www.workbuddy.ai (in-browser login)
  -codebuddy        login only: log into the CodeBuddy international site www.codebuddy.ai (CLI in-browser login)
  -reload-interval <sec>
                    Account pool hot-reload scan interval (default 5s, 0 disables): newly added,
                    updated, or deleted credential files are picked up at runtime without a restart
  -models-refresh <min>
                    Model catalog refresh interval (default 60 minutes, 0 disables)
                    (catalog sources: live API + npm static package, merged and deduplicated)
  -allow-price-probes
                    Enable background automatic price probes (off by default);
                    they issue real model generation requests which may consume credits, so confirm before enabling;
                    does not affect client requests, catalog refreshes, or explicit probe runs
  -keepalive-hours <h,h,...>
                    Proactive renewal hours (local time, comma separated, default 22)
                    At those hours, refresh every account's login credentials without waiting for the
                    access token to near expiry; empty disables. Only calls the refresh API; no model
                    requests and no quota consumption.
  -webui            Enable the standalone web console (default http://<addr>:8316/ui/):
                    shows account pool/model stats/logs/config.json/credentials (tokens redacted),
                    and can toggle model API key checks; admin key and port are configured in the
                    gateway section of config.json; an empty admin key can be generated once locally on the server

probe flags:
  -auth <path>      Probe only the given credential file (file name or path); defaults to all accounts
  -models <m1,m2>   Models to probe; defaults to the first few in the model catalog
  -limit <n>        Model count when -models is not specified (default 5, cap 50)
  -addr/-port       Must match the running serve; when the model API requires a key, probe automatically sends gateway.apiKey
  -proxy <url>      Upstream forwarding proxy (e.g. http://127.0.0.1:7890 or socks5://...)
  -verbose          Output verbose debug logs (request/response bodies)

monitor flags:
  -interval <sec>   Status refresh interval in seconds (default: 3)
  -journal <svc>    Also show recent systemd service logs (Linux, e.g. -journal workbuddy-gateway)
  -logfile <path>   Also show recent content of the given log file (e.g. -logfile /var/log/wb.log)
  -lines <n>        Recent log lines shown per refresh (default: 15)

Multi-account notes:
  # Log into a second account (saved to a different file)
  workbuddy-gateway login -auth workbuddy2.json

  # Log into an international site account (www.workbuddy.ai, in-browser login)
  workbuddy-gateway login -intl -auth workbuddy-intl.json

  # Auto-discovery: drop multiple credential files into the working directory for automatic multi-account (no flags needed)
  workbuddy-gateway serve        # automatically loads ./workbuddy*.json

  # Start with multiple credential files (round-robin + automatic 429 cooldown compensation; domestic/international can mix)
  workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

  # Or use directory mode: all workbuddy*.json inside the directory form the account pool
  workbuddy-gateway serve -auth-dir ./auths

  # Credential files added/updated/deleted at runtime hot-reload automatically (default every 5 seconds); no serve restart needed

Examples:
  # First-time QR login (domestic site)
  workbuddy-gateway login

  # Log into the international site (www.workbuddy.ai)
  workbuddy-gateway login -intl

  # Log into the CodeBuddy international site (www.codebuddy.ai, same as the codebuddy CLI)
  workbuddy-gateway login -codebuddy -auth workbuddy-cb.json

  # Start the local gateway (listening on 127.0.0.1:8317)
  workbuddy-gateway serve

  # Start the gateway with a custom port and proxy
  workbuddy-gateway serve -port 9000 -proxy http://127.0.0.1:7890`)
}

func initHTTPClient() {
	jar, _ := cookiejar.New(nil)
	headerTimeout, _, _ := upstreamSettings()
	transport := &http.Transport{
		MaxIdleConns:        50,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 10,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
		// Only limits the wait up to receiving response headers; the long transfer of a
		// streaming body is protected by the upstreamIdleTimeout idle watchdog.
		ResponseHeaderTimeout: headerTimeout,
	}
	if cfg.ProxyURL != "" {
		pURL, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			log.Fatalf("Error: invalid proxy address %s: %v", cfg.ProxyURL, err)
		}
		transport.Proxy = http.ProxyURL(pURL)
	}
	// Note: http.Client.Timeout is deliberately not set here.
	// A total timeout would impose a hard cap on the whole stream (previously 180s),
	// cutting off large requests that keep producing output, losing upstream usage,
	// leaving clients with incomplete responses, and triggering CPA provider-fault 503s.
	cfg.HttpClient = &http.Client{
		Transport: &hotConfigTransport{transport: transport},
		Jar:       jar,
	}
}

// idleReadCloser provides "idle read timeout" protection for the upstream
// streaming body. As long as data keeps arriving the deadline keeps sliding;
// once no new data arrives for timeout, the upstream request context is
// canceled so the blocked Read returns an error immediately, interrupting the
// stuck stream.
type idleReadCloser struct {
	rc      io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc
	mu      sync.Mutex
	timer   *time.Timer
	last    time.Time // Time of the most recent successful read
	stopped bool
}

func newIdleReadCloser(rc io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleReadCloser {
	b := &idleReadCloser{rc: rc, timeout: timeout, cancel: cancel, last: time.Now()}
	b.mu.Lock()
	b.timer = time.AfterFunc(timeout, b.check)
	b.mu.Unlock()
	return b
}

// check decides at timer expiry whether a real idle timeout occurred; if new
// data arrived in the meantime it re-arms for the remaining time, avoiding the
// race between "data just arrived" and "timer fired" that would kill an active stream.
func (b *idleReadCloser) check() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	idle := time.Since(b.last)
	if idle >= b.timeout {
		b.stopped = true
		b.cancel()
		return
	}
	b.timer.Reset(b.timeout - idle)
}

func (b *idleReadCloser) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.last = time.Now() // Any data arriving counts as activity
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleReadCloser) Close() error {
	b.mu.Lock()
	b.stopped = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	b.cancel()
	return b.rc.Close()
}

// -----------------------------------------------------------------------------
// Credential loading, saving, and automatic renewal
// -----------------------------------------------------------------------------

func loadAuth() (*StoredAuth, error) {
	authLock.RLock()
	if currAuth != nil {
		defer authLock.RUnlock()
		return currAuth, nil
	}
	authLock.RUnlock()

	data, err := os.ReadFile(cfg.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read credential file (%s): %w", cfg.AuthFile, err)
	}
	var sa StoredAuth
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("failed to parse credential file: %w", err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("credential file is missing AccessToken")
	}
	registerCredentialSecrets(&sa)

	authLock.Lock()
	currAuth = &sa
	authLock.Unlock()
	return &sa, nil
}

func saveAuth(sa *StoredAuth) error {
	registerCredentialSecrets(sa)
	traceID := newTraceID()
	log.Printf("[Credential save] traceId=%s file=%s stage=start note=write to disk first, then switch the in-memory credential on success", traceID, cfg.AuthFile)
	dir := filepath.Dir(cfg.AuthFile)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	data, err := json.MarshalIndent(sa, "", "  ")
	if err != nil {
		log.Printf("[Credential save] traceId=%s stage=encode result=failed reason=%v business_impact=in-memory credential not switched", traceID, err)
		return err
	}
	if err := os.WriteFile(cfg.AuthFile, data, 0o600); err != nil {
		log.Printf("[Credential save] traceId=%s stage=disk-write result=failed reason=%v business_impact=in-memory credential not switched", traceID, err)
		return err
	}
	clearDisabledMarker(cfg.AuthFile)
	authLock.Lock()
	currAuth = sa
	authLock.Unlock()
	log.Printf("[Credential save] traceId=%s file=%s result=success note=disk write complete, in-memory credential updated", traceID, cfg.AuthFile)
	return nil
}

// saveAuthTo writes credentials to the given path (multi-account mode).
// On success it clears that path's disabled marker (the account re-logged in).
func saveAuthTo(path string, sa *StoredAuth) error {
	registerCredentialSecrets(sa)
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	data, err := json.MarshalIndent(sa, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	clearDisabledMarker(path)
	return nil
}

// loadAccountFile reads and parses credentials from the given path (does not
// touch the global cache).
func loadAccountFile(path string) (*StoredAuth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read credential file (%s): %w", path, err)
	}
	var sa StoredAuth
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("failed to parse credential file (%s): %w", path, err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("credential file missing AccessToken (%s)", path)
	}
	registerCredentialSecrets(&sa)
	return &sa, nil
}

// isCredentialFile reports whether a file name is a credential file in
// auto-discovery mode. Excludes runtime artifacts such as
// workbuddy-status.json (the monitor status snapshot).
func isCredentialFile(name string) bool {
	if !strings.HasPrefix(name, "workbuddy") || !strings.HasSuffix(name, ".json") {
		return false
	}
	return name != statusSnapshotFile
}

// collectConfiguredAuthPaths resolves the credential paths to load:
//  1. -auth-dir directory → all workbuddy*.json inside it
//  2. explicit -auth → comma-separated credential file list (user order preserved)
//  3. neither given (auto-discovery) → scan the current directory for all
//     workbuddy*.json, so dropping multiple credential files into the working
//     directory forms a multi-account pool; if none exist, fall back to the default cfg.AuthFile
func collectConfiguredAuthPaths() []string {
	var paths []string

	if cfg.AuthDir != "" {
		entries, err := os.ReadDir(cfg.AuthDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !isCredentialFile(e.Name()) {
					continue
				}
				paths = append(paths, filepath.Join(cfg.AuthDir, e.Name()))
			}
			sort.Strings(paths)
		}
		return paths
	}

	if cfg.AuthExplicit {
		for _, p := range strings.Split(cfg.AuthFile, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				paths = append(paths, p)
			}
		}
		return paths
	}

	// Auto-discovery mode: scan the current working directory (the systemd
	// WorkingDirectory is the credential directory)
	entries, err := os.ReadDir(".")
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !isCredentialFile(e.Name()) {
				continue
			}
			paths = append(paths, e.Name())
		}
		sort.Strings(paths)
	}
	if len(paths) == 0 {
		// Fall back to the default path when no credential files exist, so the
		// "please run login first" hint can be shown
		paths = append(paths, cfg.AuthFile)
	}
	return paths
}

// loadAccounts builds the account pool.
// -auth accepts comma-separated credential files; -auth-dir automatically loads
// every workbuddy*.json in the directory; when neither is given, all
// workbuddy*.json in the current directory are auto-discovered (multi-account
// with no flags).
func loadAccounts() error {
	accountMu.Lock()
	defer accountMu.Unlock()

	accounts = nil
	rrIndex = 0

	for _, p := range collectConfiguredAuthPaths() {
		sa, err := loadAccountFile(p)
		if err != nil {
			log.Printf("[Auth] Skipping invalid credential file %s: %v", p, err)
			continue
		}
		fp := ""
		if st, statErr := os.Stat(p); statErr == nil {
			fp = accountFingerprint(st)
		}
		accounts = append(accounts, &Account{Path: p, Auth: sa, Edition: profileForEdition(sa.Edition).Key, fingerprint: fp})
	}

	// Restore persisted disabled account markers (credential file deleted; console
	// shows a re-login hint only)
	loadDisabledMarkers()
	restoreAccountRuntimeStateLocked()
	restoreStatsAccountStatesLocked()

	if len(accounts) == 0 {
		return fmt.Errorf("no valid credentials found; run the login command to scan the QR code first")
	}
	return nil
}

// restoreAccountRuntimeStateLocked restores quota and per-model ledgers from the
// monitor snapshot; the caller holds accountMu.
func restoreAccountRuntimeStateLocked() {
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		return
	}
	var snap statusSnapshot
	if json.Unmarshal(data, &snap) != nil {
		return
	}
	byPath := make(map[string]accountSnapshot, len(snap.Accounts))
	for _, state := range snap.Accounts {
		byPath[state.Path] = state
	}
	for _, acc := range accounts {
		state, ok := byPath[acc.Path]
		if !ok || acc.Disabled {
			continue
		}
		acc.QuotaTotal = state.QuotaTotal
		acc.QuotaUsed = state.QuotaUsed
		acc.QuotaRemaining = state.QuotaRemaining
		acc.IsPaidUser = state.IsPaidUser
		acc.PlanLabel = state.PlanLabel
		acc.PlanCheckedAt = state.PlanCheckedAt
		acc.PlanStale = state.PlanStale || state.PlanCheckedAt == 0 ||
			time.Now().Unix()-state.PlanCheckedAt >= 300
		if acc.PlanCheckedAt == 0 {
			acc.PlanLabel = planUnknown // Never keep labels guessed from trial flags by old versions.
		}
		acc.QuotaKnown = state.QuotaKnown
		acc.QuotaUnlimited = state.QuotaUnlimited
		acc.QuotaExhausted = state.QuotaExhausted && !state.QuotaUnlimited
		if len(state.ModelStates) == 0 {
			continue
		}
		acc.ModelStates = make(map[string]*modelRuntimeState, len(state.ModelStates))
		for model, saved := range state.ModelStates {
			restored := &modelRuntimeState{
				CostClass: saved.CostClass, CooldownUntil: timeFromUnix(saved.CooldownUntil),
				QuotaBlocked: saved.QuotaBlocked, NextProbeAt: timeFromUnix(saved.NextProbeAt),
				LastReason: saved.LastReason, ObservedAt: timeFromUnix(saved.ObservedAt),
			}
			sanitizeRestoredModelState(restored)
			acc.ModelStates[normalizeModelName(model)] = restored
		}
	}
}

// sanitizeRestoredModelState fixes dirty data written by historical versions
// into snapshots.
//
// Old versions wrongly overwrote CostClass to paid when receiving 14018
// (account quota exhausted), but 14018 only says the account ran out of quota
// and cannot prove the model is paid. If a snapshot shows the legacy combo
// "QuotaBlocked=true + CostClass=paid + reason 14018", downgrade the cost class
// back to unknown and let later usage.credit evidence re-learn it.
// Newer versions no longer produce that combo (markModelQuotaBlocked no longer
// touches CostClass).
func sanitizeRestoredModelState(state *modelRuntimeState) {
	if state == nil || !state.QuotaBlocked || state.CostClass != modelCostPaid {
		return
	}
	reason := strings.ToLower(state.LastReason)
	// The escaped rune group is the upstream Chinese "quota used up" wording.
	if strings.Contains(reason, "14018") || strings.Contains(reason, "credits exhausted") ||
		strings.Contains(state.LastReason, "\u989d\u5ea6\u5df2\u7528\u5c3d") {
		state.CostClass = modelCostUnknown
	}
}

func timeFromUnix(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0)
}

func unixOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

// -----------------------------------------------------------------------------
// Credential hot reload: while serve runs, newly added/updated/deleted
// credential files are discovered automatically and the account pool converges
// without a restart
// -----------------------------------------------------------------------------

// accountFingerprint builds a credential file change fingerprint from mtime+size.
func accountFingerprint(st os.FileInfo) string {
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}

// reloadAccounts converges the on-disk credential files into the in-memory
// account pool in place (hot reload):
//   - newly added credential file → joins the account pool automatically;
//   - changed content (re-login/manual update) → replaces the credential in
//     place, clears cooldown/disabled state, and resumes scheduling;
//   - deleted credential file → leaves the pool (phantom accounts that already
//     have a disabled marker are kept to prompt re-login).
//
// Returns whether anything changed. The caller handles snapshot refreshes
// beyond accountMu itself.
func reloadAccounts() bool {
	changed, _ := reloadAccountsResult()
	return changed
}

func reloadAccountsResult() (bool, error) {
	accountMu.Lock()
	defer accountMu.Unlock()
	if cfg.AuthDir != "" || !cfg.AuthExplicit {
		dir := cfg.AuthDir
		if dir == "" {
			dir = "."
		}
		if _, err := os.ReadDir(dir); err != nil {
			return false, fmt.Errorf("credential directory unreadable; account pool left unchanged")
		}
	}

	changed := false
	var failures []error
	seen := make(map[string]bool)

	for _, p := range collectConfiguredAuthPaths() {
		st, err := os.Stat(p)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("credential file unreadable"))
			}
			continue
		}
		seen[p] = true
		fp := accountFingerprint(st)

		var existing *Account
		for _, acc := range accounts {
			if acc.Path == p {
				existing = acc
				break
			}
		}

		if existing == nil {
			// New credential file: load and join the pool (file may be mid-write;
			// on failure retry next round)
			sa, err := loadAccountFile(p)
			if err != nil {
				failures = append(failures, fmt.Errorf("new credential unparseable; waiting for next round"))
				log.Printf("[Reload] Credential file %s not parseable yet, retrying next round: %v", p, err)
				continue
			}
			accounts = append(accounts, &Account{
				Path:        p,
				Auth:        sa,
				Edition:     profileForEdition(sa.Edition).Key,
				fingerprint: fp,
			})
			log.Printf("[Reload] Found new account credential %s (%s), added to account pool automatically", p, profileForEdition(sa.Edition).Label)
			changed = true
			continue
		}

		if existing.fingerprint == fp {
			continue // unchanged
		}

		// Content changed: replace the credential in place (re-login or manual update)
		sa, err := loadAccountFile(p)
		if err != nil {
			failures = append(failures, fmt.Errorf("updated credential unparseable; keeping old content"))
			// Do not update the fingerprint; retry next round (the normal write
			// window is tiny, so the next round almost always succeeds)
			log.Printf("[Reload] Credential file %s changed but not parseable yet; keeping old credential: %v", p, err)
			continue
		}
		recovered := existing.Disabled
		existing.Auth = sa
		existing.Edition = profileForEdition(sa.Edition).Key
		existing.Disabled = false
		existing.DisabledReason = ""
		existing.CooldownUntil = time.Time{}
		existing.CooldownMsg = ""
		existing.QuotaKnown = false
		existing.QuotaExhausted = false
		existing.QuotaUnlimited = false
		existing.ModelStates = nil
		existing.fingerprint = fp
		clearDisabledMarker(p)
		if recovered {
			log.Printf("[Reload] Account %s re-logged in successfully; scheduling resumed automatically", p)
		} else {
			log.Printf("[Reload] Account %s credential updated (hot reload, no restart needed)", p)
		}
		changed = true
	}

	// Convergence: drop removed credentials (disabled phantom accounts are kept)
	kept := make([]*Account, 0, len(accounts))
	for _, acc := range accounts {
		if !seen[acc.Path] && !acc.Disabled {
			log.Printf("[Reload] Credential file %s deleted; removed from account pool", acc.Path)
			changed = true
			continue
		}
		kept = append(kept, acc)
	}
	accounts = kept
	if len(accounts) > 0 {
		rrIndex %= len(accounts)
	} else {
		rrIndex = 0
	}
	return changed, errors.Join(failures...)
}

// accountReloaderLoop is serve's background credential hot-reload goroutine: it
// periodically scans for credential changes and converges the pool in place.
func accountReloaderLoop() {
	interval := time.Duration(cfg.ReloadInterval) * time.Second
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastLogged time.Time
	var previousStatus string
	logScheduledTask("credential_reload", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(interval), "")
	for tick := range ticker.C {
		next := tick.Add(interval)
		started := time.Now()
		changed, err := reloadAccountsResult()
		if changed {
			writeStatusSnapshot() // refresh the monitor status file immediately
			requestQuotaScan()
			// A successful directory refresh triggers price probing automatically; this
			// second explicit trigger ensures models of a site that "had no accounts
			// before but gained one" are probed right away.
			requestModelsScan()
			requestModelsProbe()
		}
		status, detail := "success", "scan complete; credentials unchanged"
		if changed {
			detail = "account pool changes applied; related refreshes notified"
		}
		if err != nil {
			status, detail = "partial", safeDebugError(err)
			if strings.HasPrefix(err.Error(), "credential directory unreadable") {
				status = "failed"
			}
		}
		logFrequentTask("credential_reload", status, detail, started, next, &lastLogged, &previousStatus, changed)
	}
}

func normalizeModelName(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func modelStateLocked(acc *Account, model string) *modelRuntimeState {
	if acc.ModelStates == nil {
		acc.ModelStates = make(map[string]*modelRuntimeState)
	}
	model = normalizeModelName(model)
	state := acc.ModelStates[model]
	if state == nil {
		state = &modelRuntimeState{CostClass: modelCostUnknown}
		acc.ModelStates[model] = state
	}
	return state
}

func usableForModelLocked(acc *Account, model string, now time.Time) (accountSelectionKind, bool) {
	if acc.Disabled || acc.CooldownUntil.After(now) {
		return "", false
	}
	model = normalizeModelName(model)
	state := acc.ModelStates[model]
	if state == nil {
		if !acc.QuotaExhausted {
			return selectionNormal, true
		}
		return selectionProbeExhausted, true
	}
	if state.CooldownUntil.After(now) {
		return "", false
	}
	if state.QuotaBlocked {
		return "", false
	}
	if !acc.QuotaExhausted {
		return selectionNormal, true
	}
	if state.CostClass == modelCostFree && !state.QuotaBlocked {
		return selectionFreeExhausted, true
	}
	if state.CostClass == modelCostPaid || now.Before(state.NextProbeAt) {
		return "", false
	}
	return selectionProbeExhausted, true
}

// nextAccountForModel picks an account by "free-exhausted accounts first →
// accounts with balance → controlled probing of unknown models on zero-balance accounts".
func nextAccountForModel(model string, attempted map[*Account]bool) (*Account, accountSelectionKind, error) {
	// Site priority must be computed before locking: preferredFreeSites reads the
	// account ledgers and takes accountMu itself, so calling it while holding the
	// lock would self-deadlock. Semantics: when one site offers the model free and
	// another charges for it, prefer the free site until all its accounts are
	// unavailable; otherwise no priority — plain round-robin.
	preferred := preferredFreeSites(model)

	accountMu.Lock()
	defer accountMu.Unlock()

	if len(accounts) == 0 {
		return nil, "", fmt.Errorf("account pool is empty")
	}
	allowed := 0
	for _, acc := range accounts {
		if ok, _ := modelAccountAllowed(model, acc, accounts); ok {
			allowed++
		}
	}
	if allowed == 0 && modelAccountRuleConfigured(model) {
		return nil, "", fmt.Errorf("%w: no credentials matching the model account allow/block list for model %s", errModelAccountPolicy, model)
	}
	now := time.Now()

	pick := func(siteFilter func(string) bool) (*Account, accountSelectionKind, bool) {
		for _, wanted := range []accountSelectionKind{selectionFreeExhausted, selectionNormal, selectionProbeExhausted} {
			for i := 0; i < len(accounts); i++ {
				idx := (rrIndex + i) % len(accounts)
				acc := accounts[idx]
				if attempted != nil && attempted[acc] {
					continue
				}
				if ok, _ := modelAccountAllowed(model, acc, accounts); !ok {
					continue
				}
				if siteFilter != nil && !siteFilter(accSiteLocked(acc)) {
					continue
				}
				kind, ok := usableForModelLocked(acc, model, now)
				if !ok || kind != wanted {
					continue
				}
				if kind == selectionProbeExhausted {
					modelStateLocked(acc, model).NextProbeAt = now.Add(modelProbeDelay)
				}
				rrIndex = (idx + 1) % len(accounts)
				return acc, kind, true
			}
		}
		return nil, "", false
	}

	if len(preferred) > 0 {
		if acc, kind, ok := pick(func(site string) bool { return preferred[site] }); ok {
			return acc, kind, nil
		}
		log.Printf("[FreeSite] No free-site account currently usable for model %s; falling back to other sites for compensation", model)
	}
	if acc, kind, ok := pick(nil); ok {
		return acc, kind, nil
	}

	disabled, accountCooling, modelCooling, quotaBlocked, probeWaiting, policyBlocked := 0, 0, 0, 0, 0, 0
	earliest := time.Time{}
	for _, acc := range accounts {
		if ok, _ := modelAccountAllowed(model, acc, accounts); !ok {
			policyBlocked++
			continue
		}
		switch {
		case acc.Disabled:
			disabled++
		case acc.CooldownUntil.After(now):
			accountCooling++
			if earliest.IsZero() || acc.CooldownUntil.Before(earliest) {
				earliest = acc.CooldownUntil
			}
		default:
			state := acc.ModelStates[normalizeModelName(model)]
			if state == nil {
				if acc.QuotaExhausted {
					probeWaiting++
				}
				continue
			}
			if state.CooldownUntil.After(now) {
				modelCooling++
				if earliest.IsZero() || state.CooldownUntil.Before(earliest) {
					earliest = state.CooldownUntil
				}
			} else if state.QuotaBlocked || acc.QuotaExhausted && state.CostClass == modelCostPaid {
				quotaBlocked++
			} else if acc.QuotaExhausted && now.Before(state.NextProbeAt) {
				probeWaiting++
			}
		}
	}
	msg := fmt.Sprintf("no available account for model %s right now: excluded by account allowlist: %d, authFailed=%d, accountCooling=%d, modelCooling=%d, quota blocked=%d, probeWaiting=%d", model, policyBlocked, disabled, accountCooling, modelCooling, quotaBlocked, probeWaiting)
	if !earliest.IsZero() {
		msg += ", earliest recovery=" + earliest.Format("2006-01-02 15:04:05")
	}
	return nil, "", fmt.Errorf("%s", msg)
}

func nextAccount() (*Account, error) {
	acc, _, err := nextAccountForModel("", nil)
	return acc, err
}

// markCooldown blocks an account until the given time.
func markCooldown(acc *Account, until time.Time, msg string) {
	accountMu.Lock()
	acc.CooldownUntil = until
	acc.CooldownMsg = msg
	accountMu.Unlock()
	log.Printf("[Cooldown] Account %s hit the rate limit, blocked automatically until %s (hint: %s)",
		acc.Path, until.Format("2006-01-02 15:04:05"), msg)
}

func markModelCooldown(acc *Account, model string, until time.Time, msg string) {
	accountMu.Lock()
	state := modelStateLocked(acc, model)
	state.CooldownUntil = until
	state.LastReason = msg
	accountMu.Unlock()
	log.Printf("[ModelCooldown] Account %s model %s hit a model-level rate limit; only this model is blocked until %s; other models remain schedulable", acc.Path, model, until.Format("2006-01-02 15:04:05"))
	writeStatusSnapshot()
}

// markModelQuotaBlocked records one "account quota exhausted" observation.
//
// Note: 14018 means "this account's quota ran out" and says nothing about
// whether the model is paid. The upstream rejects quota-zero accounts outright
// (observed: even free models return 14018), so only QuotaBlocked is set here
// (this account temporarily cannot serve that model); CostClass is never
// overwritten — model cost can only be decided by usage.credit evidence.
func markModelQuotaBlocked(acc *Account, model, msg string) {
	accountMu.Lock()
	state := modelStateLocked(acc, model)
	oldClass := state.CostClass
	state.QuotaBlocked = true
	state.LastReason = msg
	state.ObservedAt = time.Now()
	accountMu.Unlock()
	log.Printf("[ModelQuota] Account %s model %s returned quota exhausted (14018): only this account's scheduling for this model is blocked, lifted automatically when quota recovers; model cost class stays %s and was not overwritten by this error",
		acc.Path, model, ifEmpty(oldClass, modelCostUnknown))
	writeStatusSnapshot()
}

// disableAccount marks an account disabled (authorization expired/revoked),
// stops scheduling it, deletes its credential file, and writes a persisted
// disabled marker so the console can prompt the user to log in again.
func disableAccount(acc *Account, reason string) {
	accountMu.Lock()
	acc.Disabled = true
	acc.DisabledReason = reason
	acc.CooldownUntil = time.Time{}
	path := acc.Path
	nickname := ""
	uid := ""
	edition := ""
	var authSnapshot *StoredAuth
	if acc.Auth != nil {
		nickname = acc.Auth.Account.Nickname
		uid = acc.Auth.Account.UID
		edition = profileForEdition(acc.Auth.Edition).Key
		acc.Nickname = nickname
		acc.UID = uid
		acc.Edition = edition
		copied := *acc.Auth
		authSnapshot = &copied
	}
	accountMu.Unlock()

	// Write the persisted disabled marker (so status can still prompt re-login
	// after the credential is deleted)
	marker := disabledMarker{
		Path:       path,
		Reason:     reason,
		DisabledAt: time.Now().Unix(),
		Nickname:   nickname,
		UID:        uid,
		Edition:    edition,
	}
	if data, err := json.MarshalIndent(marker, "", "  "); err == nil {
		if werr := os.WriteFile(markerPath(path), data, 0o600); werr != nil {
			log.Printf("[Auth] Failed to write disabled marker for account %s: %v", path, werr)
		}
	}

	// Do a read-only check before deleting: deletion is irreversible, destroy the
	// credential only when it is confirmed truly unusable.
	if keep, note := credentialStillUsableForDelete(authSnapshot); keep {
		log.Printf("[Auth] Account %s removed from scheduling; %s; credential file kept at %s (delete manually to remove entirely); re-login overwrites it automatically",
			path, note, path)
		writeStatusSnapshot()
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[Auth] Failed to delete credential file for account %s: %v", path, err)
	}
	log.Printf("[Auth] Account %s authorization failed; removed from scheduling and credential file deleted (read-only check confirmed unusable before deletion): %s", path, reason)
	writeStatusSnapshot()
}

// loadDisabledMarkers scans disabled-marker files and restores them as disabled
// accounts in the pool (Auth is nil).
func loadDisabledMarkers() {
	// Collect marker file paths:
	// - AuthDir mode: *.json.disabled in the directory are marker files
	// - explicit -auth mode: <credential path>.disabled is the marker file
	// - auto-discovery mode: scan the current directory for all *.json.disabled markers
	var markerPaths []string
	if cfg.AuthDir != "" {
		entries, err := os.ReadDir(cfg.AuthDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasSuffix(name, ".json.disabled") {
				markerPaths = append(markerPaths, filepath.Join(cfg.AuthDir, name))
			}
		}
	} else if cfg.AuthExplicit {
		for _, p := range strings.Split(cfg.AuthFile, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				markerPaths = append(markerPaths, markerPath(p))
			}
		}
	} else {
		// Auto-discovery mode: scan the current working directory
		entries, err := os.ReadDir(".")
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if strings.HasPrefix(name, "workbuddy") && strings.HasSuffix(name, ".json.disabled") {
					markerPaths = append(markerPaths, name)
				}
			}
			sort.Strings(markerPaths)
		}
	}

	for _, mp := range markerPaths {
		data, err := os.ReadFile(mp)
		if err != nil {
			continue
		}
		var m disabledMarker
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		// The credential file is newer than the marker: the user re-logged in or
		// renewed manually, so the old marker is stale.
		if fi, statErr := os.Stat(m.Path); statErr == nil {
			if mi, mErr := os.Stat(mp); mErr == nil && fi.ModTime().After(mi.ModTime()) {
				log.Printf("[Auth] Credential file for account %s is newer than its disabled marker; treating as re-logged-in and clearing the marker", m.Path)
				_ = os.Remove(mp)
				continue
			}
		}
		// When the credential file still exists, the marker re-disables the account
		// (the file is kept for recoverability).
		applied := false
		for _, a := range accounts {
			if a.Path != m.Path {
				continue
			}
			a.Disabled = true
			a.DisabledReason = m.Reason
			if a.Nickname == "" {
				a.Nickname = m.Nickname
			}
			if a.UID == "" {
				a.UID = m.UID
			}
			applied = true
			break
		}
		if applied {
			continue
		}
		accounts = append(accounts, &Account{
			Path:           m.Path,
			Disabled:       true,
			DisabledReason: m.Reason,
			Nickname:       m.Nickname,
			UID:            m.UID,
			Edition:        m.Edition,
		})
	}
}

// clearDisabledMarker clears the disabled marker after a successful re-login.
func clearDisabledMarker(path string) {
	mp := markerPath(path)
	if err := os.Remove(mp); err != nil && !os.IsNotExist(err) {
		log.Printf("[Auth] Failed to clear disabled marker (%s): %v", mp, err)
	}
}

// isAuthFailure reports whether the upstream response signals failed
// authorization (401/403 / invalid token / expired login, etc.).
func isAuthFailure(statusCode int, body string) bool {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return true
	}
	low := strings.ToLower(body)
	if strings.Contains(low, "invalid token") ||
		strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "expired login") ||
		strings.Contains(low, "login expired") ||
		strings.Contains(low, "token expired") ||
		strings.Contains(low, "authentication required") ||
		// Upstream Chinese wording, kept as escaped runes to keep this file ASCII:
		// "login expired", "login invalid", "token expired".
		strings.Contains(body, "\u767b\u5f55\u5df2\u8fc7\u671f") ||
		strings.Contains(body, "\u767b\u5f55\u5931\u6548") ||
		strings.Contains(body, "token \u5df2\u5931\u6548") {
		return true
	}
	return false
}

// resetTimeRe extracts "will be reset at <date> <time> (UTC+n)" from a
// rate-limit notice; the leading \x{...} pair is the upstream Chinese "will be"
// prefix, kept as escaped runes so the source stays ASCII.
var resetTimeRe = regexp.MustCompile(`\x{5C06}\x{5728}\s*(\d{4}-\d{2}-\d{2})\s+(\d{2}:\d{2}:\d{2})\s*(UTC[+-]\d+(?::\d{2})?)?`)

// resetTimeGenericRe is the fallback matching of any "date + time (optional UTC
// offset)" fragment, used for English rate-limit messages from the international
// site (e.g. "will reset at 2026-09-05 01:57:00 UTC+8").
var resetTimeGenericRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})(?:\s*(UTC[+-]\d+(?::\d{2})?))?`)

// parseResetTime parses the rate-limit reset time from an upstream 429 message.
// Domestic example: a Chinese rate-limit message containing "2026-09-04 07:48:15 UTC+8" right after the "will reset" wording
// International example: "Your usage has exceeded the rate limit. It will reset at 2026-09-05 01:57:00 UTC+8."
func parseResetTime(s string) (time.Time, bool) {
	m := resetTimeRe.FindStringSubmatch(s)
	if m == nil {
		m = resetTimeGenericRe.FindStringSubmatch(s)
	}
	if m == nil {
		return time.Time{}, false
	}
	return parseDateTimeTZ(m[1], m[2], m[3])
}

// parseDateTimeTZ parses "date time (optional UTC offset)"; without a zone it
// defaults to UTC+8.
func parseDateTimeTZ(date, clock, zone string) (time.Time, bool) {
	offset := 8 * 3600 // parse as UTC+8 by default
	if zone != "" {
		z := zone
		z = strings.TrimPrefix(z, "UTC")
		z = strings.TrimPrefix(z, "utc")
		var h, mi int
		if _, err := fmt.Sscanf(z, "%d:%d", &h, &mi); err == nil {
			offset = h*3600 + mi*60
		} else if _, err := fmt.Sscanf(z, "%d", &h); err == nil {
			offset = h * 3600
		}
	}
	loc := time.FixedZone("UTC", offset)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// isRateLimited reports whether the upstream response is rate-limited
// (429 or code 6004 / rate-limit hint, Chinese and English).
func isRateLimited(statusCode int, body string) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	// The escaped rune group is the upstream Chinese "rate limited" hint.
	if strings.Contains(body, `"code":6004`) || strings.Contains(body, "\u9891\u7387\u9650\u5236") || strings.Contains(body, "frequency limit") {
		return true
	}
	low := strings.ToLower(body)
	if strings.Contains(low, "rate limit") || strings.Contains(low, "ratelimit") || strings.Contains(low, "too many requests") {
		return true
	}
	return false
}

func isModelRateLimited(body string) bool {
	low := strings.ToLower(body)
	// Escaped rune group: upstream Chinese "switch to another model".
	return strings.Contains(body, `"code":6004`) || strings.Contains(body, "\u5207\u6362\u5176\u4ed6\u6a21\u578b") || strings.Contains(low, "switch to another model")
}

func isQuotaExhausted(statusCode int, body string) bool {
	if statusCode != http.StatusTooManyRequests && !strings.Contains(body, `"code":14018`) {
		return false
	}
	low := strings.ToLower(body)
	// Escaped rune group: upstream Chinese "quota used up".
	return strings.Contains(body, `"code":14018`) || strings.Contains(body, "\u989d\u5ea6\u5df2\u7528\u5c3d") || strings.Contains(low, "credits exhausted")
}

// ensureValidTokenFor checks and renews the token for the given account
// (multi-account version). Disabled accounts (Disabled or Auth nil) are skipped
// and never scheduled.
func ensureValidTokenFor(acc *Account) error {
	if acc == nil {
		return nil
	}
	accountMu.Lock()
	disabled := acc.Disabled
	hasAuth := acc.Auth != nil
	expiresAt := int64(0)
	if hasAuth {
		expiresAt = acc.Auth.Auth.ExpiresAt
	}
	accountMu.Unlock()
	if disabled || !hasAuth {
		return nil
	}
	now := time.Now()
	if now.Unix() > expiresAt-900 {
		log.Printf("[Auth] Account %s token needs renewal; current expiry=%s, renewal threshold=15 minutes before expiry, calling upstream refresh API", acc.Path, time.Unix(expiresAt, 0).Format("2006-01-02 15:04:05"))
		if err := doRefreshTokenFor(acc); err != nil {
			accountMu.Lock()
			isDisabled := acc.Disabled
			accountMu.Unlock()
			if !isDisabled {
				markCooldown(acc, now.Add(time.Minute), "Automatic token renewal failed: "+err.Error())
			}
			return err
		}
	}
	return nil
}

func ensureValidToken() error {
	sa, err := loadAuth()
	if err != nil {
		return err
	}
	// Refresh automatically when the token expires within 15 minutes
	if time.Now().Unix() > sa.Auth.ExpiresAt-900 {
		log.Printf("[Auth] Access token is expiring or expired (ExpiresAt=%s), refreshing automatically...", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
		return doRefreshToken(sa)
	}
	return nil
}

func doRefreshToken(sa *StoredAuth) error {
	if sa == nil || sa.Auth.RefreshToken == "" {
		return fmt.Errorf("cannot refresh: missing RefreshToken")
	}
	if _, err := refreshTokenPayload(sa); err != nil {
		return err
	}
	if err := saveAuth(sa); err != nil {
		return fmt.Errorf("failed to write credentials back: %w", err)
	}
	log.Printf("[Auth] Token refreshed successfully! New expiry: %s", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	return nil
}

// doRefreshTokenFor refreshes the given account's token and saves it back to its
// credential file (multi-account version).
func doRefreshTokenFor(acc *Account) error {
	return refreshAccountToken(acc, "on-demand renewal")
}

// refreshAccountToken refreshes the given account's login credentials and writes
// them back atomically.
//
// Failures are handled in two tiers: the account is disabled only when the
// upstream's "login state dead" flag hits (e.g. 12153 / invalid_grant) or when
// consecutive failures reach refreshDisableThreshold; plain network errors, 5xx,
// and unmatched 401/403 just increment the counter and keep the original
// credentials, so one blip cannot delete the user's credentials.
func refreshAccountToken(acc *Account, reason string) error {
	if acc == nil {
		return fmt.Errorf("cannot refresh: account is missing RefreshToken or is disabled")
	}

	// Serialize with this account's upstream requests so a refresh never swaps the
	// token mid-request.
	acc.lock.Lock()
	defer acc.lock.Unlock()

	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.RefreshToken == "" {
		accountMu.Unlock()
		return fmt.Errorf("cannot refresh: account is missing RefreshToken or is disabled")
	}
	if reason == "on-demand renewal" && time.Now().Unix() <= acc.Auth.Auth.ExpiresAt-900 {
		accountMu.Unlock()
		log.Printf("[Auth] Account %s on-demand renewal skipped: another request refreshed it while we waited; continuing with valid credentials", acc.Path)
		return nil
	}
	refreshed := *acc.Auth
	oldTokens := refreshed.Auth // Pre-overwrite validation compares old credentials; keep a copy.
	path := acc.Path
	oldExpiresAt := refreshed.Auth.ExpiresAt
	accountMu.Unlock()

	log.Printf("[Auth] Account %s starting token refresh, trigger=%s, site=%s, access token expiry before refresh=%s",
		path, reason, profileForEdition(refreshed.Edition).Label, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"))
	status, err := refreshTokenPayload(&refreshed)
	if err != nil {
		kind := classifyRefreshFailure(status, err.Error())
		summary := refreshFailureSummary(kind, status, err)
		count := recordRefreshFailure(acc, summary)
		if count >= refreshDisableThreshold {
			log.Printf("[Auth] Account %s login state declared dead; removed from scheduling and disabled marker written; consecutive failures=%d basis=%s", path, count, summary)
			disableAccount(acc, fmt.Sprintf("token refresh failed %d consecutive times: %s", count, summary))
		} else if kind == refreshFailureSessionDead {
			// The upstream explicitly says the login state is gone, but keep the
			// credentials for now: keep retrying with them and only stop scheduling
			// once the threshold is reached.
			log.Printf("[Auth] Account %s renewal declared login-state-dead by the upstream (%d/%d consecutive): %s; credentials kept for now, scheduling stops only at the threshold; re-login recommended soon",
				path, count, refreshDisableThreshold, summary)
		} else {
			log.Printf("[Auth] Account %s token refresh failed, old credentials kept; consecutive failures=%d/%d verdict=%s business_impact=not renewed this time, account remains usable until the access token expires, retrying later automatically",
				path, count, refreshDisableThreshold, summary)
		}
		return err
	}
	// Pre-overwrite validation: the new credentials must still belong to the same
	// account and be genuinely usable; otherwise keep the old ones.
	if err := validateRefreshedCredential(path, &refreshed, oldTokens); err != nil {
		accountMu.Lock()
		acc.LastRefreshError = "new credentials failed pre-overwrite validation: " + err.Error()
		accountMu.Unlock()
		log.Printf("[Auth] Account %s new credentials failed pre-overwrite validation; overwrite abandoned, original credentials kept; reason=%v business_impact=account continues with original credentials, nothing destroyed or downgraded",
			path, err)
		return err
	}
	if err := saveAuthTo(path, &refreshed); err != nil {
		log.Printf("[Auth] Account %s token refreshed from upstream but writing credentials back failed; old credentials kept in memory and on disk: %v", path, err)
		return fmt.Errorf("failed to write credentials back: %w", err)
	}
	fingerprint := ""
	if st, statErr := os.Stat(path); statErr == nil {
		fingerprint = accountFingerprint(st)
	}
	accountMu.Lock()
	if !acc.Disabled {
		acc.Auth = &refreshed
		acc.Edition = profileForEdition(refreshed.Edition).Key
		acc.fingerprint = fingerprint
		acc.RefreshFailCount = 0
		acc.LastRefreshError = ""
	}
	accountMu.Unlock()
	newExpiresAt := refreshed.Auth.ExpiresAt
	if oldExpiresAt > 0 && newExpiresAt <= oldExpiresAt {
		// Refresh succeeded but the access token lifetime did not extend: the account
		// may be near the upstream login-state cap.
		log.Printf("[Auth] Account %s refresh succeeded but the access token lifetime did not extend (%s -> %s); the account may be near the login-state cap, watch whether re-login is needed",
			path, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"), time.Unix(newExpiresAt, 0).Format("2006-01-02 15:04:05"))
	}
	log.Printf("[Auth] Account %s token refreshed successfully; access token expiry updated from %s to %s, trigger=%s, credentials written back safely",
		path, time.Unix(oldExpiresAt, 0).Format("2006-01-02 15:04:05"), time.Unix(newExpiresAt, 0).Format("2006-01-02 15:04:05"), reason)
	writeStatusSnapshot()
	return nil
}

// recordRefreshFailure increments the consecutive refresh failure count and
// returns the current count.
func recordRefreshFailure(acc *Account, summary string) int {
	accountMu.Lock()
	defer accountMu.Unlock()
	acc.RefreshFailCount++
	acc.LastRefreshError = summary
	return acc.RefreshFailCount
}

// refreshTokenPayload calls the upstream refresh API and updates the in-memory
// token fields (not persisted). Routes to the matching site's (domestic/
// international) refresh endpoint based on the credential file's edition.
// Returns the upstream HTTP status code (the actual status on success or
// failure; 0 on network error).
func refreshTokenPayload(sa *StoredAuth) (int, error) {
	registerCredentialSecrets(sa)
	prof := profileForEdition(sa.Edition)
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	}

	data, status, err := doJSON(cfg.HttpClient, http.MethodPost, prof.tokenRefreshURL(), headers, nil)
	if err != nil {
		return status, fmt.Errorf("upstream refresh rejected (HTTP %d): %w", status, err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return status, fmt.Errorf("failed to parse new token: %w", err)
	}
	registerSecrets(tok.AccessToken, tok.RefreshToken)

	now := time.Now()
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	// Keep the old expiry when the API returns no expiresIn (preserveExpiry) to
	// avoid a refresh storm.
	if tok.ExpiresIn > 0 {
		sa.Auth.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	// Refresh token expiry: prefer the API-returned refreshExpiresIn; when absent,
	// read the unverified claims of the current refresh token so the UI does not
	// show "unknown".
	if tok.RefreshExpiresIn > 0 {
		sa.Auth.RefreshExpiresAt = now.Add(time.Duration(tok.RefreshExpiresIn) * time.Second).Unix()
	} else if exp, ok := jwtExpiry(sa.Auth.RefreshToken); ok {
		sa.Auth.RefreshExpiresAt = exp.Unix()
	}
	sa.Auth.LastRefreshTime = now.Unix()
	return status, nil
}

func parseQuotaCapacity(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	return strconv.ParseFloat(value, 64)
}

// summary carries only quota figures and historical flags; it cannot decide a
// valid plan on its own. A full entitlement query replaces "pending" on success;
// guessing a Pro trial from trial flags is forbidden.
func planLabelFromSummary(summary quotaSummaryData) string {
	return planUnknown
}

func parseQuotaSummary(data []byte) (total, used, remaining float64, paid bool, plan string, err error) {
	var summary quotaSummaryData
	if err = json.Unmarshal(data, &summary); err != nil {
		return 0, 0, 0, false, "", fmt.Errorf("failed to parse quota response: %w", err)
	}
	for _, pkg := range summary.Packages {
		pkgTotal, parseErr := parseQuotaCapacity(pkg.CycleTotalCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, "", fmt.Errorf("failed to parse total quota %q: %w", pkg.CycleTotalCapacity, parseErr)
		}
		pkgUsed, parseErr := parseQuotaCapacity(pkg.CycleUsedCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, "", fmt.Errorf("failed to parse used quota %q: %w", pkg.CycleUsedCapacity, parseErr)
		}
		pkgRemaining, parseErr := parseQuotaCapacity(pkg.CycleRemainCapacity)
		if parseErr != nil {
			return 0, 0, 0, false, "", fmt.Errorf("failed to parse remaining quota %q: %w", pkg.CycleRemainCapacity, parseErr)
		}
		total += pkgTotal
		used += pkgUsed
		remaining += pkgRemaining
	}
	return total, used, remaining, summary.IsPaidUser, planLabelFromSummary(summary), nil
}

// parseEnterpriseUsage converts enterprise allocated quota into the gateway's
// total/used/remaining. credit (or used_num) is used this cycle, opposite of the
// personal "remaining" convention; limitNum (or limit_num) is the quota
// allocated to this member. limitNum=-1 means unlimited.
func parseEnterpriseUsage(data []byte) (total, used, remaining float64, unlimited bool, err error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, 0, 0, false, fmt.Errorf("enterprise quota response is empty")
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(trimmed, &raw); err != nil {
		return 0, 0, 0, false, fmt.Errorf("failed to parse enterprise quota response: %w", err)
	}
	limit, hasLimit, err := firstEnterpriseNumber(raw, "limitNum", "limit_num")
	if err != nil {
		return 0, 0, 0, false, err
	}
	if !hasLimit {
		return 0, 0, 0, false, fmt.Errorf("enterprise quota response missing limitNum")
	}
	used, hasUsed, err := firstEnterpriseNumber(raw, "credit", "used_num", "usedNum")
	if err != nil {
		return 0, 0, 0, false, err
	}
	if !hasUsed {
		return 0, 0, 0, false, fmt.Errorf("enterprise quota response missing credit/used_num")
	}
	if used < 0 || (limit < 0 && limit != -1) {
		return 0, 0, 0, false, fmt.Errorf("enterprise quota response contains an invalid negative number")
	}
	if limit == -1 {
		// Unlimited has no "remaining<=0" concept. -1 is the sentinel for total/
		// remaining and the display layer shows "unlimited".
		return -1, used, -1, true, nil
	}
	remaining = limit - used
	if remaining < 0 {
		remaining = 0
	}
	return limit, used, remaining, false, nil
}

func firstEnterpriseNumber(raw map[string]json.RawMessage, keys ...string) (float64, bool, error) {
	var value float64
	var present bool
	for _, key := range keys {
		val, ok := raw[key]
		if !ok || string(bytes.TrimSpace(val)) == "null" {
			continue
		}
		num, err := parseEnterpriseJSONNumber(val)
		if err != nil {
			return 0, true, fmt.Errorf("enterprise quota field %s could not be parsed", key)
		}
		if present && value != num {
			return 0, true, fmt.Errorf("enterprise quota synonym fields conflict numerically")
		}
		value, present = num, true
	}
	return value, present, nil
}

func parseEnterpriseJSONNumber(raw json.RawMessage) (float64, error) {
	raw = bytes.TrimSpace(raw)
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil && !math.IsNaN(num) && !math.IsInf(num, 0) {
		return num, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		num, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err == nil && !math.IsNaN(num) && !math.IsInf(num, 0) {
			return num, nil
		}
	}
	return 0, fmt.Errorf("not a number")
}

// applyQuotaObservation records quota and lifts model quota blocks when the
// balance recovers or is unlimited. The caller may hold the account lock; only
// the account pool lock is taken here.
func applyQuotaObservation(acc *Account, total, used, remaining float64, unlimited, paid bool, plan string, planOK bool, now time.Time) (wasExhausted, exhausted bool, display string) {
	accountMu.Lock()
	defer accountMu.Unlock()
	wasExhausted = acc.QuotaExhausted
	acc.QuotaTotal = total
	acc.QuotaUsed = used
	acc.QuotaRemaining = remaining
	acc.QuotaUnlimited = unlimited
	acc.IsPaidUser = paid
	if planOK {
		acc.PlanLabel = plan
		acc.PlanCheckedAt = now.Unix()
		acc.PlanStale = false
	} else {
		if acc.PlanCheckedAt == 0 {
			acc.PlanLabel = planUnknown
		}
		acc.PlanStale = true
	}
	display = planDisplayLabel(acc.PlanLabel, acc.PlanStale)
	acc.QuotaKnown = true
	exhausted = !unlimited && remaining <= 0
	acc.QuotaExhausted = exhausted
	if unlimited || remaining > 0 {
		for _, state := range acc.ModelStates {
			if state == nil {
				continue
			}
			state.QuotaBlocked = false
			state.NextProbeAt = time.Time{}
		}
	}
	return wasExhausted, exhausted, display
}

func formatQuota(value float64) string {
	if value > -0.005 && value < 0.005 {
		value = 0
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
}

func formatQuotaAmount(unlimited bool, kind string, value float64) string {
	if unlimited && (kind == "total" || kind == "remaining") {
		return "Unlimited"
	}
	return formatQuota(value)
}

func usageCredit(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	value, exists := usage["credit"]
	if !exists {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func usageFromCompletion(data []byte) map[string]any {
	var completion map[string]any
	if json.Unmarshal(data, &completion) != nil {
		return nil
	}
	usage, _ := completion["usage"].(map[string]any)
	return usage
}

func usageNumber(usage map[string]any, key string) (int64, bool) {
	if usage == nil {
		return 0, false
	}
	switch v := usage[key].(type) {
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n, true
		}
		f, err := v.Float64()
		return int64(f), err == nil
	case string:
		s := strings.TrimSpace(v)
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return n, true
		}
		f, err := strconv.ParseFloat(s, 64)
		return int64(f), err == nil
	default:
		return 0, false
	}
}

func usageTotalTokens(usage map[string]any) (int64, bool) {
	if n, ok := usageNumber(usage, "total_tokens"); ok {
		return n, true
	}
	prompt, hasPrompt := usageNumber(usage, "prompt_tokens")
	completion, hasCompletion := usageNumber(usage, "completion_tokens")
	if hasPrompt || hasCompletion {
		return prompt + completion, true
	}
	return 0, false
}

func observeModelCredit(acc *Account, model string, usage map[string]any, reqID uint64) {
	credit, ok := usageCredit(usage)
	if !ok || acc == nil {
		return
	}
	// credit=0 is only classified free when the sample is large enough: the upstream
	// may record 0 even for tiny requests (e.g. probes), which does not mean the
	// model is truly free and must not send zero-balance accounts at paid models.
	if credit <= 0 {
		total, hasTotal := usageTotalTokens(usage)
		if !hasTotal || total < modelFreeMinTokens {
			log.Printf("[ModelCost] requestId=%d account %s model %s credit=0 but sample too small (total_tokens=%d < %d); ignoring this free classification",
				reqID, acc.Path, model, total, modelFreeMinTokens)
			return
		}
	}
	accountMu.Lock()
	state := modelStateLocked(acc, model)
	oldClass := state.CostClass
	if credit <= 0 {
		state.CostClass = modelCostFree
		state.QuotaBlocked = false
	} else {
		state.CostClass = modelCostPaid
	}
	state.ObservedAt = time.Now()
	quotaExhausted := acc.QuotaExhausted
	path := acc.Path
	site := acc.Profile().Key
	accountMu.Unlock()
	recordModelCostClass(model, site, credit <= 0)

	if credit <= 0 {
		if oldClass != modelCostFree {
			log.Printf("[ModelCost] requestId=%d account %s site=%s model %s live usage.credit=0, learned as free model", reqID, path, site, model)
		}
		if quotaExhausted {
			log.Printf("[FreeModel] requestId=%d site=%s requested model %s is free, completed using a paid-balance-exhausted account %s, usage.credit=0", reqID, site, model, path)
		}
	} else if oldClass != modelCostPaid {
		log.Printf("[ModelCost] requestId=%d account %s site=%s model %s live usage.credit=%s, learned as paid model", reqID, path, site, model, formatQuota(credit))
	}
	writeStatusSnapshot()
}

// refreshAccountQuota refreshes an account's quota via the read-only user-center
// billing API; tokens are never logged or sent back.
func refreshAccountQuota(ctx context.Context, acc *Account) error {
	if acc == nil {
		return nil
	}
	if !lockAccountWithContext(ctx, &acc.lock) {
		return ctx.Err()
	}
	defer acc.lock.Unlock()

	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
		accountMu.Unlock()
		return nil
	}
	auth := *acc.Auth
	path := acc.Path
	prof := acc.Profile()
	accountMu.Unlock()

	traceID := uuid.NewString()
	enterpriseID := strings.TrimSpace(auth.Account.EnterpriseID)
	if enterpriseID != "" {
		return refreshEnterpriseAccountQuota(ctx, acc, auth, path, prof, traceID, enterpriseID)
	}
	log.Printf("[Quota] traceId=%s account %s starting quota query, site=%s, endpoint=%s", traceID, path, prof.Label, prof.quotaSummaryURL())
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		r.Header.Set("X-Trace-ID", traceID)
	}
	data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, prof.quotaSummaryURL(), headers, strings.NewReader("{}"))
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s account %s quota query failed, HTTP=%d; previous quota data kept, plan mark pending refresh", traceID, path, status)
		return err
	}
	total, used, remaining, paid, plan, err := parseQuotaSummary(data)
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s account %s quota response unparseable; previous quota data kept, plan mark pending refresh", traceID, path)
		return err
	}
	var summary quotaSummaryData
	_ = json.Unmarshal(data, &summary) // parseQuotaSummary already validated it.
	now := time.Now()
	resources, planErr := fetchPlanResources(ctx, prof, headers, traceID, path, now)
	if planErr == nil {
		plan, planErr = identifyPlan(prof.Key, summary, resources, now)
	}
	wasExhausted, exhausted, plan := applyQuotaObservation(acc, total, used, remaining, false, paid, plan, planErr == nil, now)
	if planErr != nil {
		log.Printf("[Plan verdict] traceId=%s account=%s result=pending reason=%v display=%s business_impact=quota still updated; API failure is not reported as free or trial", traceID, path, planErr, plan)
	} else {
		log.Printf("[Plan verdict] traceId=%s account=%s validityCheckedAt=%s resourceCount=%d display=%s result=full entitlement validation passed", traceID, path, now.Format(time.RFC3339), len(resources), plan)
	}
	switch {
	case !wasExhausted && exhausted:
		log.Printf("[Quota] traceId=%s account %s quota query succeeded, total=%s, used=%s, remaining=%s, paidUser=%t, plan=%s; account scheduling frozen pending quota recovery", traceID, path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid, plan)
	case wasExhausted && !exhausted:
		log.Printf("[Quota] traceId=%s account %s quota recovered, total=%s, used=%s, remaining=%s, paidUser=%t, plan=%s; account freeze lifted automatically and scheduling resumed", traceID, path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid, plan)
	default:
		log.Printf("[Quota] traceId=%s account %s quota query succeeded, total=%s, used=%s, remaining=%s, paidUser=%t, plan=%s", traceID, path, formatQuota(total), formatQuota(used), formatQuota(remaining), paid, plan)
	}
	return nil
}

// refreshEnterpriseAccountQuota only queries the quota allocated to an
// enterprise member. External project measurements show personal
// get-user-resource* is empty for enterprise accounts, so this must not mark the
// account as having 0 remaining. The caller already holds acc.lock.
func refreshEnterpriseAccountQuota(ctx context.Context, acc *Account, auth StoredAuth, path string, prof *upstreamProfile, traceID, enterpriseID string) error {
	endpoint := prof.enterpriseUsageURL()
	log.Printf("[Quota] traceId=%s account %s starting enterprise allocated quota query, site=%s, endpoint=%s, enterprise identity=recognized, note=personal resource package get-user-resource is not called, avoiding an empty payload being read as 0 remaining", traceID, path, prof.Label, endpoint)
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		r.Header.Set("X-Trace-ID", traceID)
		r.Header.Set("X-Enterprise-Id", enterpriseID)
		r.Header.Set("X-Tenant-Id", enterpriseID)
		if auth.Account.UID != "" {
			r.Header.Set("X-User-Id", strings.TrimSpace(auth.Account.UID))
		}
		if auth.Auth.Domain != "" {
			r.Header.Set("X-Domain", auth.Auth.Domain)
		}
	}
	data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, endpoint, headers, strings.NewReader("{}"))
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s account %s enterprise quota query failed, HTTP=%d; previous quota kept, business_impact=the failure is not read as 0 remaining, no block added or lifted this round", traceID, path, status)
		return err
	}
	total, used, remaining, unlimited, err := parseEnterpriseUsage(data)
	if err != nil {
		markAccountPlanStale(acc)
		log.Printf("[Quota] traceId=%s account %s enterprise quota response unparseable; previous quota kept, reason=%v, business_impact=bad data is not read as 0 remaining", traceID, path, err)
		return err
	}
	now := time.Now()
	wasExhausted, exhausted, plan := applyQuotaObservation(acc, total, used, remaining, unlimited, false, planEnterprise, true, now)
	log.Printf("[Plan verdict] traceId=%s account=%s result=%s note=enterprise accounts have no personal plan resource package, get-user-resource not called, display=%s", traceID, path, planEnterprise, plan)
	totalText := formatQuotaAmount(unlimited, "total", total)
	remainText := formatQuotaAmount(unlimited, "remaining", remaining)
	switch {
	case !wasExhausted && exhausted:
		log.Printf("[Quota] traceId=%s account %s enterprise quota computed=limitNum %s - credit %s, remaining=%s, unlimited=%t, plan=%s; allocated quota used up, paid model scheduling paused", traceID, path, totalText, formatQuota(used), remainText, unlimited, plan)
	case wasExhausted && !exhausted:
		log.Printf("[Quota] traceId=%s account %s enterprise quota recovered, computed=limitNum %s - credit %s, remaining=%s, unlimited=%t, plan=%s; quota blocks caused earlier by personal empty payloads or 14018 have been lifted", traceID, path, totalText, formatQuota(used), remainText, unlimited, plan)
	default:
		log.Printf("[Quota] traceId=%s account %s enterprise quota query succeeded, computed=limitNum %s - credit %s, remaining=%s, unlimited=%t, plan=%s", traceID, path, totalText, formatQuota(used), remainText, unlimited, plan)
	}
	return nil
}

func lockAccountWithContext(ctx context.Context, mu interface{ TryLock() bool }) bool {
	return tryAccountLockWithContext(ctx, mu.TryLock)
}

func tryAccountLockWithContext(ctx context.Context, tryLock func() bool) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return false
		}
		if tryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func refreshAllAccountQuotas(ctx context.Context, done chan<- struct{}, schedule ...time.Time) {
	defer close(done)
	next := time.Time{}
	if len(schedule) > 0 {
		next = schedule[0]
	}
	trace, started := startScheduledTask("quota_scan", next)
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()
	log.Printf("[Quota] Starting batch quota update, accountCount=%d", len(accs))
	processed, failed := 0, 0
	for _, acc := range accs {
		if err := ctx.Err(); err != nil {
			log.Printf("[Quota] This quota update round was canceled; accounts not yet scanned will wait for the next round: %v", err)
			break
		}
		accountCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := refreshAccountQuota(accountCtx, acc)
		processed++
		cancel()
		if err != nil {
			failed++
			log.Printf("[Quota] Account %s quota update did not finish this round: %v", acc.Path, err)
		} else {
			accountMu.Lock()
			if acc != nil && !acc.Disabled && acc.PlanStale {
				failed++
			}
			accountMu.Unlock()
		}
	}
	snapshotErr := writeStatusSnapshot()
	log.Printf("[Quota] Quota update round complete, status snapshot written")
	status := "success"
	if ctx.Err() != nil {
		status = "cancelled"
	} else if len(accs) == 0 {
		status = "skipped"
	} else if failed == len(accs) {
		status = "failed"
	} else if failed > 0 || snapshotErr != nil {
		status = "partial"
	}
	logScheduledTask("quota_scan", "completed", status, trace, started, next, fmt.Sprintf("accounts=%d, checked=%d, quota/plan anomalies=%d, snapshotFailed=%t", len(accs), processed, failed, snapshotErr != nil))
}

func backgroundQuotaRefresher() {
	const interval = 300 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	next := time.Now().Add(interval)
	var cancel context.CancelFunc
	var done chan struct{}
	start := func() {
		if cancel != nil && done != nil {
			select {
			case <-done:
			default:
				log.Printf("[Quota] New quota scan round arrived; force-canceling the previous round's unfinished requests")
				cancel()
			}
		}
		ctx, nextCancel := context.WithCancel(context.Background())
		cancel = nextCancel
		done = make(chan struct{})
		go refreshAllAccountQuotas(ctx, done, next)
	}
	start()
	for {
		select {
		case tick := <-ticker.C:
			next = tick.Add(interval)
			start()
		case <-quotaScanTrigger:
			log.Printf("[Quota] Credential pool changed; triggering an immediate quota scan round")
			start()
		}
	}
}

func requestQuotaScan() {
	select {
	case quotaScanTrigger <- struct{}{}:
	default:
	}
}

func isAlreadyCheckedIn(status int, message string) bool {
	if status == 0 && message == "" {
		return false
	}
	low := strings.ToLower(message)
	// Escaped rune group: upstream Chinese "already checked in".
	return strings.Contains(message, `"code":10001`) || strings.Contains(message, `"code":14001`) ||
		strings.Contains(message, "\u5df2\u7b7e\u5230") ||
		strings.Contains(low, "already checked in")
}

// checkinAccount is the single-attempt transport. Only the persisted scheduler
// calls it automatically; credential events never invoke it directly.
func checkinAccount(ctx context.Context, acc *Account) (string, error) {
	return checkinAccountWithTrace(ctx, acc, newTraceID())
}

// -----------------------------------------------------------------------------
// CLI subcommand implementations: login, status, refresh
// -----------------------------------------------------------------------------

// formatAccountStatus renders the full status text for one account (shared by
// the status command and the serve startup banner). idx is the 1-based account
// number; now is the current time (for cooldown/expiry decisions).
func formatAccountStatus(acc *Account, idx int, now time.Time) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n--- Account #%d ---\n", idx))
	sb.WriteString(fmt.Sprintf("Credential file: %s\n", acc.Path))
	prof := acc.Profile()
	sb.WriteString(fmt.Sprintf("Site:         %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://")))
	sb.WriteString(fmt.Sprintf("Checkin:      %s (Beijing time %s)\n", checkinDisplay(dailyCheckins.view(checkinTargetLocked(acc), now), prof.Key, now), checkinDay(now)))

	if acc.Disabled {
		// Disabled account (Auth may be nil: the credential file was deleted, info
		// comes from the disabled marker)
		nickname := acc.Nickname
		uid := acc.UID
		if acc.Auth != nil {
			nickname = acc.Auth.Account.Nickname
			uid = acc.Auth.Account.UID
		}
		sb.WriteString(fmt.Sprintf("Nickname:     %s\n", ifEmpty(nickname, "(unknown)")))
		sb.WriteString(fmt.Sprintf("User UID:     %s\n", ifEmpty(uid, "(unknown)")))
		sb.WriteString("Account status: authorization failed (scheduling disabled)\n")
		sb.WriteString(fmt.Sprintf("Failure reason: %s\n", truncate(acc.DisabledReason, 200)))
		sb.WriteString(fmt.Sprintf("Suggested action: credential file deleted, please run again: workbuddy-gateway login -auth %s\n", acc.Path))
		return sb.String()
	}

	if acc.Auth == nil {
		return sb.String()
	}

	expTime := time.Unix(acc.Auth.Auth.ExpiresAt, 0)
	remaining := time.Until(expTime)
	statusStr := "valid"
	if remaining <= 0 {
		statusStr = "expired"
	}

	sb.WriteString(fmt.Sprintf("Nickname:     %s\n", acc.Auth.Account.Nickname))
	sb.WriteString(fmt.Sprintf("User UID:     %s\n", acc.Auth.Account.UID))
	sb.WriteString(fmt.Sprintf("Enterprise ID: %s\n", ifEmpty(acc.Auth.Account.EnterpriseID, "(personal account)")))
	sb.WriteString(fmt.Sprintf("Auth domain:  %s\n", ifEmpty(acc.Auth.Auth.Domain, "www.codebuddy.cn")))
	sb.WriteString(formatRenewalStatus(acc))
	sb.WriteString(fmt.Sprintf("Plan:         %s\n", planDisplayLabel(acc.PlanLabel, acc.PlanStale)))
	if acc.PlanStale {
		sb.WriteString("              * Entitlement query failed, pending refresh; the name may be the last successful result.\n")
	}
	if acc.QuotaKnown {
		sb.WriteString(fmt.Sprintf("Quota:        total %s / used %s / remaining %s\n",
			formatQuotaAmount(acc.QuotaUnlimited, "total", acc.QuotaTotal),
			formatQuotaAmount(acc.QuotaUnlimited, "used", acc.QuotaUsed),
			formatQuotaAmount(acc.QuotaUnlimited, "remaining", acc.QuotaRemaining)))
	}
	if acc.CooldownUntil.After(now) {
		sb.WriteString(fmt.Sprintf("Cooldown:     cooling down (unblocks: %s, remaining %v)\n",
			acc.CooldownUntil.Format("2006-01-02 15:04:05"),
			time.Until(acc.CooldownUntil).Round(time.Minute)))
		sb.WriteString(fmt.Sprintf("Cooldown reason: %s\n", truncate(acc.CooldownMsg, 120)))
	} else {
		sb.WriteString("Cooldown:     available\n")
	}
	sb.WriteString(fmt.Sprintf("Token status: %s\n", statusStr))
	sb.WriteString(fmt.Sprintf("Expiry time:  %s (remaining %v)\n", expTime.Format("2006-01-02 15:04:05"), remaining.Round(time.Minute)))
	return sb.String()
}

func runStatus() {
	if err := loadAccounts(); err != nil {
		fmt.Printf("No valid credentials found: %v\nPlease run first: workbuddy-gateway login to scan the QR code.\n", err)
		return
	}

	accountMu.Lock()
	defer accountMu.Unlock()

	fmt.Println("================== WorkBuddy Account Pool Status ==================")
	fmt.Printf("Total accounts: %d\n", len(accounts))
	now := time.Now()
	for i, acc := range accounts {
		fmt.Print(formatAccountStatus(acc, i+1, now))
	}
	fmt.Println("\n=======================================================")
}

func runRefresh() {
	if err := loadAccounts(); err != nil {
		fmt.Printf("Failed to read credentials: %v\n", err)
		return
	}
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()
	if len(accs) == 0 {
		fmt.Println("Account pool is empty")
		return
	}
	ok := 0
	fail := 0
	skipped := 0
	for i, acc := range accs {
		fmt.Printf("Refreshing account #%d (%s)... ", i+1, acc.Path)
		accountMu.Lock()
		disabled := acc.Disabled || acc.Auth == nil
		accountMu.Unlock()
		if disabled {
			fmt.Printf("Skipped (authorization failed, please log in again)\n")
			skipped++
			continue
		}
		if err := doRefreshTokenFor(acc); err != nil {
			fmt.Printf("Failed: %v\n", err)
			fail++
		} else {
			fmt.Println("Success")
			ok++
		}
	}
	fmt.Printf("\nRefresh complete: %d succeeded, %d failed, %d skipped (authorization failed)\n", ok, fail, skipped)
}

// -----------------------------------------------------------------------------
// Status snapshot and front-end live monitoring (monitor)
// -----------------------------------------------------------------------------

// accountSnapshot is one account's state written to the status snapshot file.
type accountSnapshot struct {
	Alerts            []accountAlertMark `json:"alerts,omitempty"`
	Path              string             `json:"path"`
	SchedulingEnabled *bool              `json:"schedulingEnabled,omitempty"`
	SchedulingReason  string             `json:"schedulingReason,omitempty"`
	Edition           string             `json:"edition,omitempty"` // Site key (cn/intl)
	Nickname          string             `json:"nickname"`
	UID               string             `json:"uid"`
	State             string             `json:"state"` // active | cooldown | paid_exhausted | expired | disabled
	CooldownUntil     int64              `json:"cooldownUntil,omitempty"`
	CooldownMsg       string             `json:"cooldownMsg,omitempty"`
	DisabledReason    string             `json:"disabledReason,omitempty"`
	TokenExpiresAt    int64              `json:"tokenExpiresAt,omitempty"`
	QuotaTotal        float64            `json:"quotaTotal,omitempty"`
	QuotaUsed         float64            `json:"quotaUsed,omitempty"`
	QuotaRemaining    float64            `json:"quotaRemaining"`
	IsPaidUser        bool               `json:"isPaidUser"`
	PlanLabel         string             `json:"planLabel,omitempty"` // Official plan name or known short name
	PlanCheckedAt     int64              `json:"planCheckedAt,omitempty"`
	PlanStale         bool               `json:"planStale,omitempty"`
	// Login renewal state: refresh token expiry and last successful renewal time,
	// to surface accounts that will need re-login early.
	RefreshExpiresAt int64                         `json:"refreshExpiresAt,omitempty"`
	LastRefreshTime  int64                         `json:"lastRefreshTime,omitempty"`
	RefreshFailCount int                           `json:"refreshFailCount,omitempty"`
	QuotaKnown       bool                          `json:"quotaKnown,omitempty"`
	QuotaExhausted   bool                          `json:"quotaExhausted,omitempty"`
	QuotaUnlimited   bool                          `json:"quotaUnlimited,omitempty"`
	ModelStates      map[string]modelStateSnapshot `json:"modelStates,omitempty"`
	FreeModels       int                           `json:"freeModels,omitempty"` // Legacy snapshot compat field; no longer shown in the account table and not used in scheduling
	ModelCooldowns   int                           `json:"modelCooldowns,omitempty"`
	Checkin          checkinStatusSnapshot         `json:"checkin"`
}

type modelStateSnapshot struct {
	CostClass     string `json:"costClass,omitempty"`
	CooldownUntil int64  `json:"cooldownUntil,omitempty"`
	QuotaBlocked  bool   `json:"quotaBlocked,omitempty"`
	NextProbeAt   int64  `json:"nextProbeAt,omitempty"`
	LastReason    string `json:"lastReason,omitempty"`
	ObservedAt    int64  `json:"observedAt,omitempty"`
}

// statusSnapshot is the complete snapshot written to workbuddy-status.json.
type statusSnapshot struct {
	AlertSource  string              `json:"alertSource,omitempty"`
	UpdatedAt    int64               `json:"updatedAt"`
	Accounts     []accountSnapshot   `json:"accounts"`
	Models       []modelStatSnapshot `json:"models,omitempty"`
	ModelCatalog []modelStatSnapshot `json:"modelCatalog,omitempty"` // Full model catalog for the admin page, including disabled entries.
}

// writeStatusSnapshot atomically writes the account pool's live state (including
// cooldowns/disabled accounts) to the status snapshot file.
// Called periodically by serve in the background; read and displayed by monitor.
func writeStatusSnapshot() error {
	statusSnapshotWriteMu.Lock()
	defer statusSnapshotWriteMu.Unlock()
	accountMu.Lock()
	snap := statusSnapshot{UpdatedAt: time.Now().Unix()}
	now := time.Now()
	for _, acc := range accounts {
		as := accountSnapshot{Path: acc.Path, Edition: acc.Profile().Key}
		as.Checkin = dailyCheckins.view(checkinTargetLocked(acc), now)
		as.QuotaTotal = acc.QuotaTotal
		as.QuotaUsed = acc.QuotaUsed
		as.QuotaRemaining = acc.QuotaRemaining
		as.IsPaidUser = acc.IsPaidUser
		as.PlanLabel = acc.PlanLabel
		as.PlanCheckedAt = acc.PlanCheckedAt
		as.PlanStale = acc.PlanStale
		as.RefreshFailCount = acc.RefreshFailCount
		if acc.Auth != nil {
			as.RefreshExpiresAt = acc.Auth.Auth.RefreshExpiresAt
			as.LastRefreshTime = acc.Auth.Auth.LastRefreshTime
			if as.RefreshExpiresAt == 0 {
				// Old or manually imported credentials lack this field; fill in a display
				// value from the refresh token's unverified claims.
				if exp, ok := jwtExpiry(acc.Auth.Auth.RefreshToken); ok {
					as.RefreshExpiresAt = exp.Unix()
				}
			}
		}
		as.QuotaKnown = acc.QuotaKnown
		as.QuotaExhausted = acc.QuotaExhausted
		as.QuotaUnlimited = acc.QuotaUnlimited
		if len(acc.ModelStates) > 0 {
			as.ModelStates = make(map[string]modelStateSnapshot, len(acc.ModelStates))
			for model, state := range acc.ModelStates {
				if state.CooldownUntil.After(now) {
					as.ModelCooldowns++
				}
				as.ModelStates[model] = modelStateSnapshot{
					CostClass: state.CostClass, CooldownUntil: unixOrZero(state.CooldownUntil),
					QuotaBlocked: state.QuotaBlocked, NextProbeAt: unixOrZero(state.NextProbeAt),
					LastReason: state.LastReason, ObservedAt: unixOrZero(state.ObservedAt),
				}
			}
		}
		switch {
		case acc.Disabled:
			as.State = "disabled"
			as.DisabledReason = acc.DisabledReason
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
			} else {
				as.Nickname = acc.Nickname
				as.UID = acc.UID
			}
		case acc.CooldownUntil.After(now):
			as.State = "cooldown"
			as.CooldownUntil = acc.CooldownUntil.Unix()
			as.CooldownMsg = acc.CooldownMsg
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
				as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
			}
		case acc.Auth != nil && acc.Auth.Auth.ExpiresAt > 0 && acc.Auth.Auth.ExpiresAt <= now.Unix():
			as.State = "expired"
			as.Nickname = acc.Auth.Account.Nickname
			as.UID = acc.Auth.Account.UID
			as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
		case acc.QuotaExhausted:
			as.State = "paid_exhausted"
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
				as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
			}
		default:
			as.State = "active"
			if acc.Auth != nil {
				as.Nickname = acc.Auth.Account.Nickname
				as.UID = acc.Auth.Account.UID
				as.TokenExpiresAt = acc.Auth.Auth.ExpiresAt
			}
		}
		snap.Accounts = append(snap.Accounts, as)
	}
	annotateAccountPolicies(snap.Accounts)
	snap.ModelCatalog = buildAllModelStatSnapshots(now, accounts)
	captureAccountStatsLocked()
	for _, row := range snap.ModelCatalog {
		if disabled, _ := modelDisabled(row.ID); !disabled {
			snap.Models = append(snap.Models, row)
		}
	}
	cnFree, intlFree := freeModelDisplayCounts(snap.Models)
	for i := range snap.Accounts {
		if profileForEdition(snap.Accounts[i].Edition).Key == "intl" {
			snap.Accounts[i].FreeModels = intlFree
		} else {
			snap.Accounts[i].FreeModels = cnFree
		}
	}
	if !lastFreeModelDisplay.initialized || lastFreeModelDisplay.cn != cnFree || lastFreeModelDisplay.intl != intlFree {
		lastFreeModelDisplay.initialized = true
		lastFreeModelDisplay.cn, lastFreeModelDisplay.intl = cnFree, intlFree
		log.Printf("[Compat stats] traceId=%s source=model-stats site multipliers domestic=%d international=%d modelRows=%d rule=count only 0.00x business_impact=legacy snapshot compat field only, not shown in the account table, does not change measured ledgers, scheduling, or billing decisions",
			newTraceID(), cnFree, intlFree, len(snap.Models))
	}
	accountMu.Unlock()

	// Alert marks are written in the same pass as the snapshot, avoiding the window
	// where "the snapshot is already bad but the log scan hasn't caught it".
	marks := recordAccountAlertMarks(snap.Accounts, snap.UpdatedAt)
	for i := range snap.Accounts {
		snap.Accounts[i].Alerts = marks[snap.Accounts[i].Path]
	}
	snap.AlertSource = alertSourceSnapshot

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: write a temp file then rename, so monitor never reads a partial file
	tmp := statusSnapshotFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, statusSnapshotFile); err != nil {
		return err
	}
	return nil
}

func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
			(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe6f) ||
			(r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6)) {
			width += 2
		} else {
			width++
		}
	}
	return width
}

func fitCell(s string, width int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if displayWidth(s) <= width {
		return s + strings.Repeat(" ", width-displayWidth(s))
	}
	limit := width - 3
	var b strings.Builder
	used := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		rw := displayWidth(string(r))
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
		s = s[size:]
	}
	return b.String() + "..." + strings.Repeat(" ", width-used-3)
}

func renderAccountTable(accs []accountSnapshot) string {
	now := time.Now()
	headers := []string{"#", "Credential file", "Account", "Site", "Status", "Token validity", "Total quota", "Used", "Remaining", "Plan", "Checkin", "Model cooldown"}
	// Column widths tighten to current data, keeping only necessary header and
	// content width. Long account/file names still have caps so one outlier cannot
	// blow up the whole table.
	caps := []int{4, 28, 36, 18, 14, 19, 12, 12, 12, 24, 10, 15}
	rows := make([][]string, 0, len(accs))
	for i, a := range accs {
		status := "Available"
		expires := "-"
		total, used, remaining, plan := "-", "-", "-", "-"
		if a.QuotaKnown {
			total = formatQuotaAmount(a.QuotaUnlimited, "total", a.QuotaTotal)
			used = formatQuotaAmount(a.QuotaUnlimited, "used", a.QuotaUsed)
			remaining = formatQuotaAmount(a.QuotaUnlimited, "remaining", a.QuotaRemaining)
			plan = planDisplayLabel(a.PlanLabel, a.PlanStale)
		}
		if a.TokenExpiresAt > 0 {
			expires = time.Unix(a.TokenExpiresAt, 0).Format("2006-01-02 15:04:05")
		}
		switch a.State {
		case "cooldown":
			status = "Cooling"
		case "expired":
			status = "Expired"
		case "quota_exhausted", "paid_exhausted":
			status = "Paid exhausted"
		case "disabled":
			status = "Disabled"
		}
		rows = append(rows, []string{
			strconv.Itoa(i + 1), filepath.Base(a.Path), ifEmpty(a.Nickname, "-"),
			profileForEdition(a.Edition).Label, status, expires, total, used, remaining, plan,
			checkinDisplay(a.Checkin, a.Edition, now), strconv.Itoa(a.ModelCooldowns),
		})
	}
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = displayWidth(header)
	}
	for _, cells := range rows {
		for i, cell := range cells {
			widths[i] = max(widths[i], min(caps[i], displayWidth(cell)))
		}
	}
	border := func() string {
		var b strings.Builder
		b.WriteByte('+')
		for _, width := range widths {
			b.WriteString(strings.Repeat("-", width+2))
			b.WriteByte('+')
		}
		return b.String()
	}
	row := func(cells []string) string {
		var b strings.Builder
		b.WriteByte('|')
		for i, cell := range cells {
			b.WriteByte(' ')
			b.WriteString(fitCell(cell, widths[i]))
			b.WriteString(" |")
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString(border() + "\n")
	b.WriteString(row(headers) + "\n")
	b.WriteString(border() + "\n")
	for _, cells := range rows {
		b.WriteString(row(cells) + "\n")
	}
	b.WriteString(border())
	for _, a := range accs {
		if a.PlanStale {
			b.WriteString("\n* Plan entitlement query failed, pending refresh; the name may be the last successful result.")
			break
		}
	}
	for _, a := range accs {
		if a.RefreshExpiresAt <= 0 {
			continue
		}
		exp := time.Unix(a.RefreshExpiresAt, 0)
		switch {
		case !exp.After(now):
			b.WriteString(fmt.Sprintf("\n! %s Refresh token expired.", filepath.Base(a.Path)))
		case exp.Sub(now) < 7*24*time.Hour:
			b.WriteString(fmt.Sprintf("\n! %s Refresh token expires %s (remaining %s); re-login recommended as soon as possible.",
				filepath.Base(a.Path), exp.Format("2006-01-02 15:04:05"), humanDuration(time.Until(exp))))
		}
	}
	for _, a := range accs {
		if a.RefreshFailCount > 0 {
			b.WriteString(fmt.Sprintf("\n! %s renewal failed %d consecutive times; re-login may be required.", filepath.Base(a.Path), a.RefreshFailCount))
		}
	}
	return b.String()
}

// statusSnapshotLoop refreshes the status snapshot every 3 seconds in serve's background.
func statusSnapshotLoop() {
	var lastLogged time.Time
	var previousStatus string
	write := func(next time.Time) {
		started := time.Now()
		err := writeStatusSnapshot()
		status, detail := "success", "account and model snapshots written"
		if err != nil {
			status, detail = "failed", safeDebugError(err)
		}
		logFrequentTask("status_snapshot", status, detail, started, next, &lastLogged, &previousStatus, err != nil)
	}
	write(time.Now().Add(3 * time.Second))
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for tick := range ticker.C {
		write(tick.Add(3 * time.Second))
	}
}

// tailLines reads the last n lines of a file (monitor shows recent logs).
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lines, _, err := readTailLines(f, n, 0)
	return lines, err
}

// journalLines fetches the systemd service's last n log lines (Linux journalctl).
func journalLines(service string, n int) ([]string, error) {
	cmd := exec.Command("journalctl", "-u", service, "-n", strconv.Itoa(n), "--no-pager", "-o", "short")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// runMonitor is the front-end live monitor: periodically refreshes account pool
// status + recent logs (Ctrl+C to exit).
func runMonitor() {
	interval := time.Duration(cfg.MonitorInterval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	logN := cfg.LogLines
	if logN <= 0 {
		logN = 15
	}

	// Detect whether stdout is a terminal (ANSI clear+redraw if so, otherwise
	// scrolling output)
	isTTY := false
	if fi, err := os.Stdout.Stat(); err == nil {
		isTTY = fi.Mode()&os.ModeCharDevice != 0
	}

	fmt.Println("================ WorkBuddy Live Monitor ================")
	fmt.Println("Press Ctrl+C to exit | Status file: " + statusSnapshotFile)
	fmt.Println("---------------------------------------------------------------")

	for {
		if isTTY {
			fmt.Print("\033[H\033[2J") // clear screen
		} else {
			fmt.Println()
		}
		fmt.Printf("Updated at: %s\n", time.Now().Format("2006-01-02 15:04:05"))

		data, err := os.ReadFile(statusSnapshotFile)
		if err != nil {
			fmt.Printf("Status file %s not found (is the service running? make sure monitor runs in the service's working directory)\n", statusSnapshotFile)
		} else {
			var snap statusSnapshot
			if err := json.Unmarshal(data, &snap); err == nil {
				active, cooldown, exhausted, expired, disabled := 0, 0, 0, 0, 0
				for _, a := range snap.Accounts {
					switch a.State {
					case "active":
						active++
					case "cooldown":
						cooldown++
					case "expired":
						expired++
					case "quota_exhausted", "paid_exhausted":
						exhausted++
					case "disabled":
						disabled++
					}
				}
				fmt.Printf("Account pool: %d total | available %d | cooling %d | paid exhausted %d | expired %d | disabled %d\n",
					len(snap.Accounts), active, cooldown, exhausted, expired, disabled)
				fmt.Println(renderAccountTable(snap.Accounts))
				if len(snap.Models) > 0 {
					fmt.Printf("\nModel stats (source %s):\n", modelSourceLabel(modelSourceFromSnapshots(snap.Models)))
					fmt.Println(renderModelTable(snap.Models))
				}
			} else {
				fmt.Println("Failed to parse status file")
			}
		}

		// Recent logs
		if cfg.JournalService != "" {
			if lines, err := journalLines(cfg.JournalService, logN); err == nil && len(lines) > 0 {
				fmt.Printf("\nRecent logs (journalctl -u %s):\n", cfg.JournalService)
				for _, l := range lines {
					fmt.Println("  " + l)
				}
			}
		} else if cfg.LogFile != "" {
			if lines, err := tailLines(cfg.LogFile, logN); err == nil && len(lines) > 0 {
				fmt.Printf("\nRecent logs (%s):\n", cfg.LogFile)
				for _, l := range lines {
					fmt.Println("  " + l)
				}
			}
		}

		fmt.Println("---------------------------------------------------------------")
		time.Sleep(interval)
	}
}

func runLogin() {
	site := "cn"
	switch {
	case cfg.LoginCodebuddy:
		site = "codebuddy"
	case cfg.LoginIntl:
		site = "intl"
	}
	prof := profileForEdition(site)

	fmt.Println("================ WorkBuddy Login ================")
	fmt.Printf("Target site:  %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
	if cfg.LoginCodebuddy || cfg.LoginIntl {
		fmt.Println("International site login completes in the browser (email / verification code / SSO, etc.); the gateway takes over the credentials automatically.")
	}
	fmt.Println("Generating login credentials and QR code...")

	// Shares the same login flow as the web console's "add account" so the CLI and
	// web implementations cannot drift apart.
	s, err := beginAccountLogin(site, cfg.AuthFile)
	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}

	if qr, qrErr := qrcode.New(s.AuthURL, qrcode.Medium); qrErr == nil {
		if cfg.LoginCodebuddy || cfg.LoginIntl {
			fmt.Println("\nScan the QR code below with your phone and complete login in the browser (email / verification code / SSO, etc.):")
		} else {
			fmt.Println("\nScan the QR code below with WeChat or WeCom to log in:")
		}
		fmt.Println(qr.ToSmallString(false))
	}
	fmt.Println("If scanning fails, you can also open this link directly in a browser:")
	fmt.Printf("%s\n\n", s.AuthURL)
	fmt.Println("Waiting for login authorization to complete (Ctrl+C to cancel)...")

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		snap := s.snapshot()
		switch snap["status"] {
		case "done":
			fmt.Println("\nLogin successful")
			fmt.Printf("Site:         %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
			if acct, ok := snap["account"].(map[string]any); ok {
				fmt.Printf("Welcome, %v (UID: %v)\n", acct["nickname"], acct["uid"])
				if exp, ok := acct["tokenExpiresAt"].(int64); ok && exp > 0 {
					fmt.Printf("Access token valid until: %s\n", time.Unix(exp, 0).Format("2006-01-02 15:04:05"))
				}
			}
			fmt.Printf("Credentials saved to: %s\n", cfg.AuthFile)
			fmt.Println("\nYou can now start the gateway service:")
			fmt.Println("  workbuddy-gateway serve")
			return
		case "failed":
			fmt.Printf("\n%v\n", snap["error"])
			return
		case "expired":
			fmt.Println("\nLogin timed out; please run the login command again.")
			return
		case "cancelled":
			fmt.Println("\nLogin canceled.")
			return
		}
	}
}

// -----------------------------------------------------------------------------
// Local HTTP API gateway service (OpenAI-protocol compatible)
// -----------------------------------------------------------------------------

func runServe() {
	// Bind all required ports first; on any failure release the already-bound ports
	// rather than starting half a service.
	servers, listeners, err := prepareGatewayServers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Startup failed: %v\n", err)
		os.Exit(1)
	}
	gatewayServers.register(servers, listeners)
	configCtx, stopConfigReload := context.WithCancel(context.Background())
	defer stopConfigReload()
	loadModelsCache()
	loadOptionalStats()
	if err := loadAccounts(); err != nil {
		fmt.Printf("Warning: no valid credentials detected (%v).\nPlease run first: workbuddy-gateway login to scan the QR code, or make sure credential files exist.\n\n", err)
	} else {
		accountMu.Lock()
		accs := append([]*Account(nil), accounts...)
		accountMu.Unlock()
		for _, acc := range accs {
			if acc.Disabled || acc.Auth == nil {
				continue
			}
			_ = ensureValidTokenFor(acc)
		}
	}

	// Start the background auto-refresh goroutines
	go backgroundTokenRefresher()
	// Proactive renewal (keepalive): refresh every account's login credentials on
	// schedule instead of waiting for the access token to near expiry
	go keepaliveLoop()
	go backgroundQuotaRefresher()
	checkinCtx, stopCheckin := context.WithCancel(configCtx)
	checkinDone := make(chan struct{})
	go func() {
		defer close(checkinDone)
		backgroundDailyCheckin(checkinCtx)
	}()
	defer func() {
		stopCheckin()
		<-checkinDone
	}()
	if cfg.ModelsRefresh > 0 {
		go modelsRefreshLoop(time.Duration(cfg.ModelsRefresh) * time.Minute)
	} else {
		logScheduledTask("model_catalog", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "model catalog auto-refresh disabled")
	}
	// Price probing is independent of catalog refresh: even with catalog refresh
	// disabled, prices can still be probed from cache.
	go modelPriceProbeLoop()

	// Start the status snapshot goroutine (read live by the monitor command)
	go statusSnapshotLoop()
	statsCtx, stopStats := context.WithCancel(context.Background())
	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		statsPersistenceLoop(statsCtx)
	}()
	defer func() {
		stopStats()
		<-statsDone
	}()

	// Start the credential hot-reload goroutine (added/updated/deleted credential
	// files take effect without a restart)
	if cfg.ReloadInterval > 0 {
		go accountReloaderLoop()
	} else {
		logScheduledTask("credential_reload", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "automatic credential scanning disabled")
	}

	listenAddr := servers[0].Addr

	fmt.Println("================================================================")
	fmt.Printf("WorkBuddy local gateway started\n")
	fmt.Printf("   Listen address:     http://%s\n", listenAddr)
	fmt.Printf("   Chat endpoint:      http://%s/v1/chat/completions\n", listenAddr)
	fmt.Printf("   Claude Code:        http://%s/v1/messages (point ANTHROPIC_BASE_URL at this gateway)\n", listenAddr)
	fmt.Printf("   Models endpoint:    http://%s/v1/models\n", listenAddr)
	fmt.Printf("   Model forwarding:   [full passthrough] any model requested by the client is relayed verbatim to the upstream\n")
	_, modelSource := mergedModelIDs()
	fmt.Printf("   Model list source:  %s\n", modelSourceLabel(modelSource))
	if cfg.ReloadInterval > 0 {
		fmt.Printf("   Credential hot reload: auto-scan every %ds; added/updated/deleted credentials take effect without restart\n", cfg.ReloadInterval)
	} else {
		fmt.Printf("   Credential hot reload: disabled (-reload-interval 0)\n")
	}
	if currentAPIKey() != "" {
		printAPIAuthBanner(os.Stdout)
	} else {
		fmt.Printf("   API auth:           disabled (any client can connect directly)\n")
	}
	if cfg.WebUI {
		fmt.Printf("   Web console:        enabled http://%s/ui/ (separate admin key)\n", servers[1].Addr)
		if currentAdminKey() == "" {
			fmt.Println("   Admin setup:        open the web page locally or through an SSH tunnel to generate the admin key; after saving it is shown once in a popup only")
		}
	} else {
		fmt.Println("   Web console:        disabled (use -webui to enable)")
	}
	if cfg.ProxyURL != "" {
		fmt.Printf("   Upstream proxy:     %s\n", cfg.ProxyURL)
	}
	if cfg.DebugEnabled {
		fmt.Printf("   JSON debug logs:    enabled (%s)\n", filepath.Join(logDir, "debug-YYYY-MM-DD.jsonl"))
	} else {
		fmt.Printf("   JSON debug logs:    disabled (debug.enabled=false in %s)\n", runtimeConfigFile)
	}
	fmt.Printf("   Upstream timeouts:  response-header wait %v / stream idle %v (overridable via %s)\n",
		upstreamHeaderTimeout, upstreamIdleTimeout, runtimeConfigFile)
	fmt.Printf("   Network resilience: transient-error retries %d / interval %v (retry only before any output; overridable via %s)\n",
		upstreamTransientRetries, upstreamRetryBackoff, runtimeConfigFile)
	if modelFilterConfigured() {
		blocked, allowed := modelFilterSummary()
		fmt.Printf("   Model allow/block list: enabled (blocked %d / allowed %d; disabled models hidden from the list and rejected)\n", blocked, allowed)
	} else {
		fmt.Printf("   Model allow/block list: disabled (configure in the models section of %s)\n", runtimeConfigFile)
	}
	fmt.Printf("   Model account rules: %d models have credential-file rules configured (%s)\n", modelAccountRuleCount(), runtimeConfigFile)
	fmt.Println("   Config hot reload:  config.json scanned every 1s; the listen port can be switched live; existing connections and streams are unaffected")

	// Show all account states at startup (same as the status command)
	accountMu.Lock()
	accCount := len(accounts)
	activeCount := 0
	disabledCount := 0
	now := time.Now()
	for _, acc := range accounts {
		if acc.Disabled || acc.Auth == nil {
			disabledCount++
		} else {
			activeCount++
		}
	}
	fmt.Printf("   Account pool:       %d accounts (%d valid, %d disabled)\n", accCount, activeCount, disabledCount)
	if accCount > 0 {
		fmt.Println("   ----------------------------------------------------------")
		for i, acc := range accounts {
			fmt.Print(formatAccountStatus(acc, i+1, now))
		}
		fmt.Println("   ----------------------------------------------------------")
	}
	// Final summary: list of loaded credential files (confirmable even if the logs
	// above were truncated)
	fileList := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		fileList = append(fileList, acc.Path)
	}
	if len(fileList) > 0 {
		fmt.Printf("   Loaded credential files (%d): %s\n", len(fileList), strings.Join(fileList, ", "))
	} else {
		fmt.Println("   No credential files loaded; run the login command to scan the QR code first")
	}
	accountMu.Unlock()

	fmt.Println("================================================================")
	fmt.Println("Waiting for client requests (Ctrl+C to stop safely)...")
	go runtimeConfigLoop(configCtx)

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopChan)
	serveErrors := make(chan error, len(servers))
	gatewayServers.start(serveErrors)
	select {
	case <-stopChan:
	case err := <-serveErrors:
		log.Printf("[Server error] traceId=%s reason=%v note=API and admin listeners are both shut down to avoid a half-available state", newTraceID(), err)
	}
	fmt.Println("\nShutting down the gateway service...")
	stopCheckin() // Stop scheduled writes before draining the HTTP listeners.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gatewayServers.shutdown(ctx)
	fmt.Println("Gateway stopped safely.")
}

func printAPIAuthBanner(w io.Writer) {
	fmt.Fprintln(w, "   API auth:           Enabled (key hidden)")
}

func backgroundTokenRefresher() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	logScheduledTask("token_check", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(5*time.Minute), "")
	for tick := range ticker.C {
		next := tick.Add(5 * time.Minute)
		trace, started := startScheduledTask("token_check", next)
		accountMu.Lock()
		accs := make([]*Account, len(accounts))
		copy(accs, accounts)
		accountMu.Unlock()
		failed := 0
		for _, acc := range accs {
			if err := ensureValidTokenFor(acc); err != nil {
				failed++
				log.Printf("[BackgroundAuth] Account %s automatic token check/renewal error: %v", acc.Path, err)
			}
		}
		status := "success"
		if len(accs) == 0 {
			status = "skipped"
		} else if failed == len(accs) {
			status = "failed"
		} else if failed > 0 {
			status = "partial"
		}
		logScheduledTask("token_check", "completed", status, trace, started, next, fmt.Sprintf("accountsChecked=%d, errors=%d; renewal only when near expiry", len(accs), failed))
	}
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *auditResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func requestAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		traceID := r.Header.Get("X-Trace-ID")
		if traceID == "" {
			traceID = uuid.NewString()
		}
		w.Header().Set("X-Trace-ID", traceID)
		// Plain on-disk audit also needs a stable TraceID / requestID; it cannot rely
		// on optional JSON logging.
		ensureDebugRequestContext(r, traceID, start)
		r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, &statsAttempt{}))
		if debugLoggingEnabled() {
			debugEvent(r, "info", "request_received", map[string]any{
				"message": "Request entered the gateway, ready to run middleware and routing",
			})
		}
		debugFullIncomingHeaders(r)
		aw := &auditResponseWriter{ResponseWriter: w}
		log.Printf("[Request received] traceId=%s method=%s path=%s source=%s note=Request entered the gateway", traceID, r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(aw, r)
		if aw.status == 0 {
			aw.status = http.StatusOK
		}
		statsFinishAttempt(r, aw.status)
		debugEvent(r, "info", "response_returned", map[string]any{
			"status_code": aw.status,
			"result":      http.StatusText(aw.status),
		})
		log.Printf("[Response returned] traceId=%s statusCode=%d duration=%v result=%s", traceID, aw.status, time.Since(start), http.StatusText(aw.status))
	})
}

// -----------------------------------------------------------------------------
// Middleware: CORS and optional API key validation
// -----------------------------------------------------------------------------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			debugEvent(r, "info", "cors_preflight_completed", map[string]any{
				"result":  "allowed",
				"message": "CORS preflight returned directly; request never reached business logic",
			})
			log.Printf("[Middleware-CORS] traceId=%s result=passed-with-direct-response note=preflight request did not enter the business method", w.Header().Get("X-Trace-ID"))
			w.WriteHeader(http.StatusOK)
			return
		}
		debugEvent(r, "debug", "cors_check_passed", map[string]any{"result": "allowed"})
		log.Printf("[Middleware-CORS] traceId=%s result=passed", w.Header().Get("X-Trace-ID"))
		next.ServeHTTP(w, r)
	})
}

// The API port keeps only the original health and root paths auth-exempt; the
// admin port uses its own auth chain.
func isAuthExemptPath(p string) bool {
	return p == "/health" || p == "/ping" || p == "/"
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := currentAPIKey()
		if apiKey != "" && !isAuthExemptPath(r.URL.Path) {
			authHeader := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHeader, "Bearer ")
			// Anthropic clients (Claude Code, etc.) carry the key in the x-api-key header.
			if token == "" {
				token = r.Header.Get("x-api-key")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
				debugEvent(r, "warn", "authentication_rejected", map[string]any{
					"status_code":     http.StatusUnauthorized,
					"reason":          "invalid_api_key",
					"business_impact": "request never reached business logic",
				})
				log.Printf("[Request intercepted] traceId=%s interceptionLayer=API-auth result=rejected reason=no valid API key provided statusCode=401 business_impact=request did not enter the business method", w.Header().Get("X-Trace-ID"))
				writeUpstreamError(w, r, http.StatusUnauthorized, "invalid_api_key", "no valid API key provided")
				return
			}
		}
		debugEvent(r, "debug", "authentication_passed", map[string]any{"result": "allowed"})
		log.Printf("[Middleware-Auth] traceId=%s result=passed", w.Header().Get("X-Trace-ID"))
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// Route handling: /v1/chat/completions (any model passed through)
// -----------------------------------------------------------------------------

// upstreamChat runs the upstream scheduling of "multi-account round-robin + 429
// cooldown compensation + disable on failed authorization + single-account
// serialization". On success it returns the 200 response (the caller closes the
// Body) and the matched account/site; on failure it has already written the
// error response and returns ok=false. Both the Chat Completions and Responses
// entry points share this logic.
func upstreamChat(w http.ResponseWriter, r *http.Request, reqID uint64, modelName string, upstreamBytes []byte, startTime time.Time) (*http.Response, *Account, *upstreamProfile, bool) {
	prepareConversationContext(r, nil)
	traceID := w.Header().Get("X-Trace-ID")
	log.Printf("[Business entry] traceId=%s requestId=%d business=upstream-model-call requestBodyBytes=%d", traceID, reqID, len(upstreamBytes))
	recordModelRequest(modelName)
	accountMu.Lock()
	poolSize := len(accounts)
	accountMu.Unlock()
	if poolSize == 0 {
		debugEvent(r, "warn", "request_rejected_no_account", map[string]any{
			"status_code":     http.StatusUnauthorized,
			"reason":          "no_auth",
			"business_impact": "no usable login credentials; upstream model not called",
		})
		recordModelFailure(modelName, "no_auth")
		writeUpstreamError(w, r, http.StatusUnauthorized, "no_auth", "no valid login credentials found; run the login command to scan the QR code first")
		return nil, nil, nil, false
	}

	var lastRateErr string
	var lastAuthErr string
	attempted := make(map[*Account]bool, poolSize)
	for attempt := 0; attempt < poolSize; attempt++ {
		acc, selection, err := nextAccountForModel(modelName, attempted)
		if err != nil {
			if errors.Is(err, errModelAccountPolicy) {
				log.Printf("[Request rejected] traceId=%s requestId=%d model=%s reason=no credentials matching the model account allow/block list statusCode=403 business_impact=request never reached upstream", traceID, reqID, modelName)
				debugEvent(r, "warn", "model_account_policy_rejected", map[string]any{
					"status_code": http.StatusForbidden, "reason": "no credentials matching the model account allow/block list", "business_impact": "request never reached upstream",
				})
				recordModelFailure(modelName, "account allowlist rejected")
				writeUpstreamError(w, r, http.StatusForbidden, "model_account_disabled", fmt.Sprintf("model %s has no usable credential file: disabled by the account allow/block list", modelName))
				return nil, nil, nil, false
			}
			// No account usable (cooling or disabled)
			msg := fmt.Sprintf("no available account: %v", err)
			if lastAuthErr != "" {
				msg += " | last authorization failure: " + truncate(lastAuthErr, 200)
			}
			if lastRateErr != "" {
				msg += " | last rate limit: " + truncate(lastRateErr, 200)
			}
			log.Printf("[#%d] %s", reqID, msg)
			debugEvent(r, "warn", "request_rejected_no_available_account", map[string]any{
				"status_code":     http.StatusServiceUnavailable,
				"reason":          "no_available_account",
				"business_impact": "upstream model not called",
			})
			recordModelFailure(modelName, "no available account")
			writeUpstreamError(w, r, http.StatusServiceUnavailable, "no_available_account", msg)
			return nil, nil, nil, false
		}
		attempted[acc] = true
		debugSetAccount(r, acc.Path)
		debugEvent(r, "debug", "account_selected", map[string]any{
			"selection": string(selection),
			"attempt":   attempt + 1,
		})
		if modelAccountRuleConfigured(modelName) {
			// Attach a short marker only in the log of the account actually hit; no
			// per-account list logging.
			log.Printf("[#%d] account allowlist hit: model=%s credential=%s (filtered by the account allow/block list in config.json)", reqID, modelName, filepath.Base(acc.Path))
			debugEvent(r, "debug", "model_account_policy_hit", map[string]any{
				"model": modelName, "account_file": filepath.Base(acc.Path),
			})
		}
		switch selection {
		case selectionFreeExhausted:
			log.Printf("[FreeModel] requestId=%d requested model %s is a known free model, selecting paid-balance-exhausted account %s to send the request", reqID, modelName, acc.Path)
		case selectionProbeExhausted:
			log.Printf("[ModelProbe] requestId=%d account %s paid balance is exhausted but model %s cost class is unknown; running one controlled probe, not repeated within 5 minutes", reqID, acc.Path, modelName)
		}
		releaseSlot, slotErr := acquireModelConcurrency(r, acc, reqID, traceID)
		if slotErr != nil {
			recordModelFailure(modelName, "concurrent_wait_canceled")
			writeUpstreamError(w, r, http.StatusServiceUnavailable, "concurrent_wait_canceled", "request canceled while waiting for a model concurrency slot; not sent upstream")
			return nil, nil, nil, false
		}
		if err := ensureValidTokenFor(acc); err != nil {
			releaseSlot()
			lastAuthErr = err.Error()
			continue
		}
		if !tryAccountLockWithContext(r.Context(), acc.lock.TryRLock) {
			releaseSlot()
			log.Printf("[Model concurrency] traceId=%s requestId=%d stage=credential-wait-canceled result=not sent upstream", traceID, reqID)
			writeUpstreamError(w, r, http.StatusServiceUnavailable, "concurrent_wait_canceled", "request canceled while waiting for credential maintenance to finish; not sent upstream")
			return nil, nil, nil, false
		}
		// Token refresh disables the account when authorization has failed; skip to
		// the next account if disabled.
		accountMu.Lock()
		disabled := acc.Disabled
		disabledReason := acc.DisabledReason
		prof := acc.Profile()
		accountMu.Unlock()
		if disabled {
			acc.lock.RUnlock()
			releaseSlot()
			lastAuthErr = disabledReason
			continue
		}

		debugEvent(r, "info", "upstream_request_started", map[string]any{
			"upstream_host": prof.chatURL(),
			"attempt":       attempt + 1,
			"payload_bytes": len(upstreamBytes),
		})
		// Inject CodeBuddy credential and fingerprint headers
		// Model sends share the credential read lock; maintenance remains exclusive.
		// Concurrency slots are retained until the response stream ends.
		statsSelectAccount(r, acc, modelName)
		resp, upstreamCancel, err := doUpstreamRequest(r, acc, prof, upstreamBytes, reqID, traceID)
		acc.lock.RUnlock()
		if err != nil {
			releaseSlot()
			debugEvent(r, "error", "upstream_request_failed", map[string]any{
				"upstream_host": prof.chatURL(),
				"error_type":    debugErrorType(err),
				"error":         safeDebugError(err),
			})
			log.Printf("[Exception] traceId=%s requestId=%d stage=upstream-network-call account=%s exception=%v business_impact=model request failed this round handled=yes", traceID, reqID, acc.Path, err)
			log.Printf("[#%d] Account %s [%s] upstream request failed: %v", reqID, acc.Path, prof.Label, err)
			recordModelFailure(modelName, "network error")
			writeUpstreamError(w, r, http.StatusBadGateway, "upstream_network_error", fmt.Sprintf("network forwarding failed: %v", err))
			return nil, nil, nil, false
		}
		upstreamCancel = bindModelConcurrency(r, resp, upstreamCancel, releaseSlot)

		// Handle non-200 upstream responses
		if resp.StatusCode >= 400 {
			statsFinishAttempt(r, resp.StatusCode)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			upstreamCancel()
			errStr := string(errBody)
			debugFullUpstreamError(r, resp, errBody)
			debugEvent(r, "warn", "upstream_response_rejected", map[string]any{
				"upstream_host": prof.chatURL(),
				"status_code":   resp.StatusCode,
				"error_code":    modelStatusFromError(resp.StatusCode, errStr),
			})
			log.Printf("[#%d] Account %s [%s] upstream returned HTTP %d: %s (duration %v)", reqID, acc.Path, prof.Label, resp.StatusCode, errStr, time.Since(startTime))
			log.Printf("[External API] traceId=%s requestId=%d upstream=%s statusCode=%d result=failed account=%s", traceID, reqID, prof.chatURL(), resp.StatusCode, acc.Path)

			if isQuotaExhausted(resp.StatusCode, errStr) {
				markModelQuotaBlocked(acc, modelName, errStr)
				if selection == selectionProbeExhausted {
					msg := fmt.Sprintf("model %s already completed a controlled probe on balance-exhausted account %s and confirmed paid quota is required; no other exhausted accounts probed this round", modelName, acc.Path)
					log.Printf("[#%d] %s", reqID, msg)
					recordModelFailure(modelName, modelStatusFromError(resp.StatusCode, errStr))
					writeUpstreamError(w, r, http.StatusServiceUnavailable, "model_requires_quota", msg)
					return nil, nil, nil, false
				}
				lastRateErr = errStr
				continue
			}

			if isModelRateLimited(errStr) {
				until, ok := parseResetTime(errStr)
				if !ok {
					until = time.Now().Add(60 * time.Second)
				}
				markModelCooldown(acc, modelName, until, errStr)
				if selection == selectionProbeExhausted {
					msg := fmt.Sprintf("model %s hit a model-level rate limit during the controlled probe on balance-exhausted account %s; no other exhausted accounts probed this round", modelName, acc.Path)
					recordModelFailure(modelName, modelStatusFromError(resp.StatusCode, errStr))
					writeUpstreamError(w, r, http.StatusServiceUnavailable, "model_rate_limited", msg)
					return nil, nil, nil, false
				}
				lastRateErr = errStr
				continue
			}

			if isRateLimited(resp.StatusCode, errStr) {
				// 429 rate limit: parse the reset time and block the account, letting
				// other accounts compensate
				until, ok := parseResetTime(errStr)
				if !ok {
					until = time.Now().Add(60 * time.Second) // default 60s cooldown when unparseable
				}
				markCooldown(acc, until, errStr)
				lastRateErr = errStr
				continue // try the next account
			}

			if isAuthFailure(resp.StatusCode, errStr) {
				// Authorization failed (401/403 / invalid token / expired login): disable
				// the account, delete its credential file, automatically switch to the
				// next usable account, and prompt re-login in the console
				disableAccount(acc, fmt.Sprintf("upstream auth failed (HTTP %d): %s", resp.StatusCode, truncate(errStr, 200)))
				lastAuthErr = errStr
				continue // try the next account
			}

			recordModelFailure(modelName, modelStatusFromError(resp.StatusCode, errStr))
			writeUpstreamError(w, r, resp.StatusCode, "upstream_error", fmt.Sprintf("upstream %d: %s", resp.StatusCode, errStr))
			return nil, nil, nil, false
		}

		debugEvent(r, "info", "upstream_response_received", map[string]any{
			"upstream_host": prof.chatURL(),
			"status_code":   resp.StatusCode,
		})
		log.Printf("[External API] traceId=%s requestId=%d upstream=%s statusCode=%d result=success account=%s", traceID, reqID, prof.chatURL(), resp.StatusCode, acc.Path)
		recordModelSuccess(modelName)
		// Wrap the response body with the idle watchdog: data keeps arriving, never
		// time out; only interrupt after upstreamIdleTimeout without data. Closing the
		// body also cancels the upstream context, completing cleanup.
		resp.Body = newIdleReadCloser(resp.Body, currentIdleTimeout(), upstreamCancel)
		return resp, acc, prof, true
	}

	// Theoretically unreachable (after poolSize attempts without success the loop
	// has already returned)
	recordModelFailure(modelName, "all_cooldown")
	debugEvent(r, "warn", "request_rejected_all_cooldown", map[string]any{
		"status_code":     http.StatusTooManyRequests,
		"reason":          "all_accounts_cooldown",
		"business_impact": "upstream model not called",
	})
	writeUpstreamError(w, r, http.StatusTooManyRequests, "all_accounts_cooldown", "all accounts are currently cooling down")
	return nil, nil, nil, false
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST requests are supported")
		return
	}

	reqID := requestIDFor(r)
	startTime := requestStartFor(r)

	readStarted := debugBodyReadStarted(r)
	bodyBytes, err := io.ReadAll(r.Body)
	readDuration := debugElapsedSince(readStarted)
	if err != nil {
		debugBodyReadFailed(r, bodyBytes, readStarted, err)
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "failed to read the request body")
		return
	}
	defer r.Body.Close()

	var reqObj map[string]any
	decodeStarted := time.Now()
	decodeErr := json.Unmarshal(bodyBytes, &reqObj)
	decodeDuration := time.Since(decodeStarted)
	if decodeErr != nil {
		debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, false, decodeErr)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "invalid JSON request body")
		return
	}

	// Core feature: pass the model field through completely.
	// Whatever model the client sends is relayed verbatim to the upstream, with no
	// hardcoded restrictions!
	modelName, _ := reqObj["model"].(string)
	if modelName == "" {
		modelName = "hy4-preview" // default fallback
		reqObj["model"] = modelName
	}

	isStream, _ := reqObj["stream"].(bool)
	debugSetModelAndStream(r, modelName, isStream)
	debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, true, nil)
	prepareConversationContext(r, reqObj)

	// Model allow/block list interception: reject on a hit without consuming any
	// upstream account quota.
	if disabled, reason := modelDisabled(modelName); disabled {
		log.Printf("[Request rejected] traceId=%s requestId=%d interceptionLayer=model-allow-block-list model=%s result=rejected reason=%s statusCode=403 business_impact=request never reached upstream",
			w.Header().Get("X-Trace-ID"), reqID, modelName, reason)
		debugEvent(r, "warn", "model_blocked_by_config", map[string]any{
			"status_code":     http.StatusForbidden,
			"reason":          reason,
			"business_impact": "model disabled by gateway config; request never reached upstream",
		})
		writeOpenAIError(w, http.StatusForbidden, "model_disabled", reason)
		return
	}

	// The Tencent upstream requires stream to be true; non-streaming is rejected (code 11101)
	reqObj["stream"] = true

	prepareUpstreamChatRequest(reqObj, r, reqID, modelName)

	upstreamBytes, err := json.Marshal(reqObj)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "failed to serialize the request")
		return
	}

	if cfg.Verbose {
		log.Printf("[#%d][Req] Model: %s | Stream: %v | BodyLen: %d", reqID, modelName, isStream, len(upstreamBytes))
	} else {
		log.Printf("[#%d] POST /v1/chat/completions -> Upstream [Model: %s, Stream: %v]", reqID, modelName, isStream)
	}

	resp, acc, prof, ok := upstreamChat(w, r, reqID, modelName, upstreamBytes, startTime)
	if !ok {
		return
	}
	if isStream {
		streamChatResponseResilient(w, r, resp, modelName, reqID, acc, prof, startTime, upstreamBytes)
	} else {
		writeChatAggregate(w, r, resp, modelName, reqID, acc, prof, startTime)
	}
}

// streamChatResponse forwards the upstream SSE line by line as an OpenAI Chat
// Completions streaming response.
func streamChatResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	streamChatResponseWithReplay(w, r, resp, modelName, reqID, acc, prof, startTime, nil)
}

// streamChatResponseResilient sends SSE comments during streaming idle periods
// and allows one safe replay for transient network interruptions before any
// valid data has been sent.
func streamChatResponseResilient(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time, upstreamBytes []byte) {
	streamChatResponseWithReplay(w, r, resp, modelName, reqID, acc, prof, startTime, upstreamBytes)
}

func streamChatResponseWithReplay(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time, upstreamBytes []byte) {
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "server does not support streaming response Flush")
		return
	}

	guard := &streamWriteGuard{w: w, flusher: flusher}
	heartbeat := startStreamHeartbeat(guard, currentStreamKeepalive())
	defer heartbeat.Stop()

	var finalBody *ttftReader
	defer func() {
		if finalBody != nil {
			finishFirstToken(r, modelName, reqID, finalBody)
		}
	}()

	replayBudget := 0
	if upstreamBytes != nil && currentFirstByteRetry() {
		replayBudget = 1
	}
	replays := 0

	for attempt := 0; ; attempt++ {
		body := newTTFTReader(resp.Body, startTime)
		finalBody = body
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var usage map[string]any
		finishReason := ""
		sawDone := false
		commentLines, metadataLines := 0, 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, ":") {
				// SSE comments are keepalive signals, not JSON. They must not be wrapped
				// as data: : heartbeat, otherwise CPA's JSON/SSE parser rejects the first
				// frame and cancels the whole request.
				commentLines++
				guard.writeUpstreamComment(line)
				continue
			}
			if !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "{") && line != "[DONE]" {
				// event/id/retry and unknown SSE fields are not Chat Completions data.
				// Keep compatibility with existing bare JSON and repeated data: prefixes.
				metadataLines++
				continue
			}
			cleanData := stripDataPrefix(line)
			if cleanData == "" {
				continue
			}
			if cleanData == "[DONE]" {
				// Remember it; forward only after a real finish_reason is confirmed.
				sawDone = true
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(cleanData), &chunk) == nil {
				observeFirstToken(body, chunk)
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				finishReason = noteFinishReason(finishReason, chunk)
			}
			if cleanedChunk := cleanChunkJSON(cleanData); cleanedChunk != "" {
				guard.writeData(cleanedChunk)
			}
		}
		scanErr := scanner.Err()
		if commentLines > 0 || metadataLines > 0 {
			debugEvent(r, "debug", "stream_sse_framing_checked", map[string]any{
				"comment_lines": commentLines, "metadata_lines": metadataLines,
				"business_impact": "heartbeats forwarded as SSE comments, metadata not wrapped as JSON data frames, avoiding CPA first-packet rejection",
			})
			log.Printf("[Streaming protocol] traceId=%s requestId=%d stage=chat-stream-forward commentLines=%d metadataLines=%d result=separated-by-SSE-field business_impact=heartbeats stay out of JSON data frames; body not logged",
				debugTraceID(r), reqID, commentLines, metadataLines)
		}

		if canReplayStream(scanErr, guard.hasData(), replays < replayBudget, r.Context().Err() != nil) {
			replays++
			log.Printf("[First-byte replay] traceId=%s requestId=%d model=%s account=%s reason=upstream connection dropped before the first valid byte (%v); client has not received content, safe replay attempt %d",
				debugTraceID(r), reqID, modelName, acc.Path, safeDebugError(scanErr), replays)
			debugEvent(r, "warn", "stream_first_byte_replay", map[string]any{
				"reason":          safeDebugError(scanErr),
				"attempt":         attempt + 1,
				"business_impact": "client has not received valid content; replay avoids edge-node silent-period false kills but consumes one extra upstream quota",
			})
			_ = resp.Body.Close()
			resp = nil
			newResp, newAcc, newProf, ok := replayUpstreamChat(w, r, reqID, modelName, upstreamBytes, startTime, guard)
			if !ok {
				return
			}
			resp, acc, prof = newResp, newAcc, newProf
			continue
		}

		completeTokenStream(body, scanErr == nil && finishReason != "")
		if scanErr != nil {
			// When the stream is interrupted (upstream stuck / timeout / client
			// cancel), never send [DONE] late: it would tell downstream "ended
			// normally" and let truncated tool calls run as complete results.
			debugEvent(r, "error", "stream_response_failed", map[string]any{
				"error_type": debugErrorType(scanErr),
				"error":      safeDebugError(scanErr),
			})
			reason := "upstream streaming response interrupted; this reply is incomplete"
			if errors.Is(scanErr, context.DeadlineExceeded) {
				reason = fmt.Sprintf("upstream sent no data for over %v; connection judged stuck and interrupted, this reply is incomplete", currentIdleTimeout())
			}
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": reason,
					"type":    "upstream_stream_interrupted",
					"code":    "stream_interrupted",
				},
			})
			guard.writeData(string(errPayload))
			log.Printf("[Exception] traceId=%s requestId=%d stage=upstream-stream-read account=%s exception=%v business_impact=response incomplete; an interruption error was sent to the client instead of a faked end", debugTraceID(r), reqID, acc.Path, scanErr)
		} else if finishReason == "" {
			// The connection closed normally (no read error), but the whole stream had
			// no non-empty finish_reason. This is not success: [DONE] must not be sent
			// and it must not be recorded as stream_response_completed.
			debugEvent(r, "error", "stream_closed_without_finish", map[string]any{
				"error_type":      "stream_closed_without_finish",
				"finish_reason":   "",
				"saw_done":        sawDone,
				"business_impact": "upstream ended cleanly without finish_reason; refused to treat as success, no late [DONE] sent",
			})
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": errStreamClosedWithoutFinish.Error(),
					"type":    "upstream_stream_incomplete",
					"code":    "stream_closed_without_finish",
				},
			})
			guard.writeData(string(errPayload))
			log.Printf("[Exception] traceId=%s requestId=%d stage=upstream-stream-end account=%s result=refused-to-treat-as-success reason=connection closed normally but no non-empty finish_reason sawDone=%t business_impact=no late [DONE]; client received stream_closed_without_finish handled=yes",
				debugTraceID(r), reqID, acc.Path, sawDone)
		} else {
			if sawDone {
				guard.writeData("[DONE]")
			}
			debugEvent(r, "info", "stream_response_completed", map[string]any{
				"status_code":   http.StatusOK,
				"finish_reason": finishReason,
				"saw_done":      sawDone,
			})
		}
		observeModelCredit(acc, modelName, usage, reqID)
		statsObserveUsage(r, usage, scanErr == nil && finishReason != "")
		recordModelTokens(modelName, usage, reqID)
		recordModelLatency(modelName, time.Since(startTime))
		if scanErr == nil && finishReason != "" {
			log.Printf("[#%d] Streaming output complete (account %s [%s], finishReason=%s, duration %v, firstToken(v2) %s)", reqID, acc.Path, prof.Label, finishReason, time.Since(startTime), body.text())
		}
		return
	}
}

// writeChatAggregate aggregates the upstream SSE into a complete Chat
// Completions JSON response.
func writeChatAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		logAggregateFailure(r, reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "failed to aggregate the upstream streaming response: "+err.Error())
		return
	}
	usage := usageFromCompletion(completionJSON)
	statsObserveUsage(r, usage, true)
	observeModelCredit(acc, modelName, usage, reqID)
	recordModelTokens(modelName, usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(completionJSON)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(completionJSON),
	})
	log.Printf("[#%d] Non-streaming response complete (account %s [%s], duration %v, firstToken(v2) %s)", reqID, acc.Path, prof.Label, time.Since(startTime), body.text())
}

// -----------------------------------------------------------------------------
// Route handling: /v1/models (lists common models for client auto-completion)
// -----------------------------------------------------------------------------

func handleModels(w http.ResponseWriter, r *http.Request) {
	modelIDs, source := mergedModelIDs()
	modelsList := make([]map[string]any, 0, len(modelIDs))
	for _, id := range modelIDs {
		entry := map[string]any{
			"id": id, "object": "model", "owned_by": "workbuddy", "permission": []any{},
		}
		// Capability extension fields: only pass through values that really exist in
		// the catalog, omit unknown ones, and never fall back to guesses like 1M.
		// context_length is the model's capability ceiling, default_context_length is
		// the client's default budget; the semantics differ and must be sent
		// separately, never merged into one value.
		// Only descriptive metadata is added here: no upstream request parameters are
		// injected, messages are not truncated, and output budget logic is unchanged.
		if mc, ok := modelCapabilityFor(id); ok {
			if mc.Ceiling > 0 {
				entry["context_length"] = mc.Ceiling
			}
			if mc.MaxOutput > 0 {
				entry["max_output_tokens"] = mc.MaxOutput
			}
			if mc.Default > 0 {
				entry["default_context_length"] = mc.Default
			}
			if len(mc.Options) >= 2 {
				entry["context_length_options"] = mc.Options
			}
			if len(mc.BySite) > 1 {
				// Capabilities differ between sites: the top level stays empty and the
				// detail lives here, so downstream does not reserve by the larger value.
				entry["capabilities_by_site"] = mc.BySite
			}
		}
		modelsList = append(modelsList, entry)
	}
	resp := map[string]any{
		"object": "list",
		"data":   modelsList,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Model-Source", modelSourceLabel(source))
	_ = json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	modelIDs, source := mergedModelIDs()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "healthy",
		"timestamp":    time.Now().Unix(),
		"version":      version,
		"model_count":  len(modelIDs),
		"model_source": modelSourceLabel(source),
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "WorkBuddy Local Gateway v%s is running.\n\nEndpoints:\n- POST /v1/chat/completions\n- POST /v1/responses\n- POST /v1/messages (Anthropic)\n- POST /v1/messages/count_tokens\n- GET  /v1/models\n- GET  /health\n", version)
}

// -----------------------------------------------------------------------------
// Request translation and sanitization helpers
// -----------------------------------------------------------------------------

func applyThinkingRules(obj map[string]any, modelName string) {
	// Follow the CodeBuddy spec: pass and normalize reasoning_effort only when the
	// client set it explicitly. Never force reasoning_effort into ordinary requests;
	// doing so easily trips Tencent's content and safety policy interception (code 11128).
	currEff, exists := obj["reasoning_effort"].(string)
	if !exists || currEff == "" || currEff == "off" || currEff == "none" {
		delete(obj, "reasoning_effort")
		delete(obj, "reasoning_summary")
		return
	}
	// When the client explicitly requests thinking, fill in auto only when the
	// summary granularity is unspecified. Explicit values such as Codex's detailed
	// must be preserved and never downgraded by the gateway.
	if summary, ok := obj["reasoning_summary"].(string); !ok || strings.TrimSpace(summary) == "" {
		obj["reasoning_summary"] = "auto"
	}
}

// ensureLeadingSystemMessage makes the first message satisfy the Tencent
// upstream's conversation structure validation.
//
// The upstream requires the first message to be a system prompt, otherwise it
// returns HTTP 400
// {"code":11128,"msg":"first message is not system prompt"}. Measurements show
// the domestic site is lenient about user-first streams, but the international
// site (workbuddy.ai) validates strictly; with a mixed account pool about 50% of
// requests failed at random. Some non-harness clients also send assistant / tool
// as the first message when continuing or returning only tool results.
// Normalization here is uniform:
//  1. first message already system: passed through unchanged;
//  2. first message developer (OpenAI's new system alias): renamed to system;
//  3. system/developer later in the list: promoted to the front (developer
//     normalized to system), the rest keep their order;
//  4. otherwise (user / assistant / tool first with no system): a fallback
//     system is injected at the very front.
//
// The return value only reports whether a fallback system was injected this
// round, used by audit logs that never record prompt bodies.
func ensureLeadingSystemMessage(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		obj["messages"] = []any{map[string]any{"role": "system", "content": configuredFallbackSystemPrompt()}}
		return true
	}

	switch roleOfMessage(messages[0]) {
	case "system":
		return false
	case "developer":
		if msg, ok := messages[0].(map[string]any); ok {
			msg["role"] = "system"
		}
		return false
	}

	// system/developer later in the list: promote to the front, rest keep order
	for i := 1; i < len(messages); i++ {
		switch roleOfMessage(messages[i]) {
		case "system", "developer":
			if msg, ok := messages[i].(map[string]any); ok {
				msg["role"] = "system"
			}
			reordered := make([]any, 0, len(messages))
			reordered = append(reordered, messages[i])
			reordered = append(reordered, messages[:i]...)
			reordered = append(reordered, messages[i+1:]...)
			obj["messages"] = reordered
			return false
		}
	}

	// No system at all: inject the fallback system at the front (works for both
	// domestic and international sites)
	injected := make([]any, 0, len(messages)+1)
	injected = append(injected, map[string]any{"role": "system", "content": configuredFallbackSystemPrompt()})
	injected = append(injected, messages...)
	obj["messages"] = injected
	return true
}

// roleOfMessage reads a message's role field, lowercased and trimmed; invalid
// structures return an empty string.
func roleOfMessage(m any) string {
	msg, ok := m.(map[string]any)
	if !ok {
		return ""
	}
	role, _ := msg["role"].(string)
	return strings.ToLower(strings.TrimSpace(role))
}

// backendHeaders sets CodeBuddy upstream fingerprint and auth headers (generated
// from the site profile)
func backendHeaders(req *http.Request, sa *StoredAuth, prof *upstreamProfile) {
	commonHeaders(req, prof)
	reqID := uuid.New().String()
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("X-Trace-ID", reqID)
	setConversationHeaders(req)
	req.Header.Set("X-Client-ID", prof.ClientID)
	req.Header.Set("X-Client-Version", prof.ClientVer)

	if sa != nil && sa.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if sa != nil && sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if sa != nil && sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if sa != nil && sa.Auth.RefreshToken != "" {
		req.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
	}
	if sa != nil && sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", prof.Product)
}

// commonHeaders sets generic spoofing headers per site profile (Origin/Referer/UA
// vary by site).
func commonHeaders(req *http.Request, prof *upstreamProfile) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", prof.Origin)
	req.Header.Set("Referer", prof.Origin+"/")
	req.Header.Set("User-Agent", prof.ClientUA)
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	// Control requests (token refresh, quota queries, model catalog, etc.) must have
	// an explicit cap so they cannot hang; the upstream conversation stream does not
	// go through here, so long streams are unaffected.
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	return doJSONContext(ctx, client, method, fullURL, headers, body)
}

func doJSONContext(ctx context.Context, client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req, &profileCN) // fallback default (all current callers pass a site profile explicitly)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d: %s", resp.StatusCode, string(raw))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to parse upstream JSON: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// Streaming aggregation and format cleanup
// -----------------------------------------------------------------------------

// mergedToolCall accumulates tool-call increments the upstream sends split by index.
type mergedToolCall struct {
	ID   string
	Type string
	Name string
	Args strings.Builder
}

// applyToolCallDelta merges upstream tool_calls increments into the map by
// index; order records the first-appearance index sequence so the final output
// order is stable. Shared by Chat Completions aggregation and Responses streaming.
func applyToolCallDelta(toolCalls map[int]*mergedToolCall, order *[]int, tcs []any) {
	for _, tcAny := range tcs {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		st, exists := toolCalls[idx]
		if !exists {
			st = &mergedToolCall{Type: "function"}
			toolCalls[idx] = st
			*order = append(*order, idx)
		}
		if id, ok := tc["id"].(string); ok && id != "" {
			st.ID = id
		}
		if t, ok := tc["type"].(string); ok && t != "" {
			st.Type = t
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				st.Name = n
			}
			if a, ok := fn["arguments"].(string); ok && a != "" {
				st.Args.WriteString(a)
			}
		}
	}
}

// logAggregateFailure records a non-streaming aggregation failure. Clean endings
// without a finish_reason get their own explanation instead of blending into the
// generic "aggregation failed" message with network interruptions.
func logAggregateFailure(r *http.Request, reqID uint64, err error) {
	if errors.Is(err, errStreamClosedWithoutFinish) {
		log.Printf("[Exception] traceId=%s requestId=%d stage=aggregating-upstream-stream result=refused-to-treat-as-success reason=upstream connection ended normally but without finish_reason statusCode=500 business_impact=truncated output not disguised as a complete reply handled=yes",
			debugTraceID(r), reqID)
		return
	}
	log.Printf("[#%d] Aggregate response failed: %v", reqID, err)
}

// noteFinishReason records the real finish reason from a chunk. The empty string
// must be ignored: the upstream sends finish_reason:"" on every intermediate
// chunk, which is not an ending.
func noteFinishReason(current string, chunk map[string]any) string {
	if chunk == nil {
		return current
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := choice["finish_reason"].(string); ok && v != "" {
			return v
		}
	}
	return current
}

func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	toolCalls := map[int]*mergedToolCall{}
	var toolOrder []int

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		observeFirstToken(r, chunk)
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					applyToolCallDelta(toolCalls, &toolOrder, tcs)
				}
			}
			finish = noteFinishReason(finish, map[string]any{"choices": []any{choice}})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the upstream stream: %w", err)
	}
	if finish == "" {
		return nil, errStreamClosedWithoutFinish
	}
	completeTokenStream(r, true)

	message := map[string]any{"role": ifEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			st := toolCalls[idx]
			id := st.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), idx)
			}
			calls = append(calls, map[string]any{
				"id":    id,
				"type":  ifEmpty(st.Type, "function"),
				"index": idx,
				"function": map[string]any{
					"name":      st.Name,
					"arguments": st.Args.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      ifEmpty(respID, fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())),
		"object":  "chat.completion",
		"created": created,
		"model":   ifEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	return json.Marshal(result)
}

// cleanChunkJSON cleans a single upstream chat chunk so it conforms to the
// OpenAI streaming chunk spec before forwarding.
//
// The upstream is a "quasi-OpenAI" implementation whose chunks carry several
// spec-deviating noises. Plain OpenAI clients tolerate them, but the Anthropic
// protocol translation layer (the Claude Code path) misreads the stream state
// because of them and tools stop executing:
//
//  1. finish_reason:"" → null (the direct cause of this bug). The spec requires
//     intermediate chunks to be null with only the terminal chunk giving a real
//     reason; the upstream instead sends finish_reason:"" on every chunk of the
//     whole stream. The translation layer takes "the first non-null
//     finish_reason" as the final stop_reason, so the very first chunk locks
//     stop_reason to end_turn and the real terminal chunk's "tool_calls" is
//     ignored. Claude Code only executes tools when stop_reason=tool_use,
//     manifesting as "gateway logs success but the client has no result".
//  2. Legacy empty function_call shell: the upstream appends
//     {"function_call":{"name":"","arguments":""}} to the terminal chunk of
//     tool-calling streams; same class of noise, removed too.
//  3. Empty-valued fields in delta (content:"", tool_calls:[], etc.).
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			// Normalize an empty finish_reason to null: only the terminal chunk should
			// carry a real reason.
			if fr, ok := choice["finish_reason"].(string); ok && fr == "" {
				choice["finish_reason"] = nil
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				// Tool increment identity fields may be omitted; empty values mean this
				// chunk carries no update. Keep index, argument fragments, and extension
				// fields so the client can accumulate fully.
				calls, _ := delta["tool_calls"].([]any)
				for _, raw := range calls {
					call, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					for _, key := range []string{"id", "type"} {
						if value, ok := call[key].(string); ok && value == "" {
							delete(call, key)
						}
					}
					if function, ok := call["function"].(map[string]any); ok {
						if name, ok := function["name"].(string); ok && name == "" {
							delete(function, "name")
						}
					}
				}
				for k, v := range delta {
					if k == "function_call" && isEmptyFunctionCall(v) {
						delete(delta, k)
						continue
					}
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

// isEmptyFunctionCall reports a legacy empty function_call shell (both name and
// arguments empty). A real legacy function call carries name or arguments and
// must not be dropped.
func isEmptyFunctionCall(v any) bool {
	fc, ok := v.(map[string]any)
	if !ok {
		return false
	}
	name, _ := fc["name"].(string)
	args, _ := fc["arguments"].(string)
	return strings.TrimSpace(name) == "" && strings.TrimSpace(args) == ""
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func writeOpenAIError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    statusCode,
		},
	})
}

func ifEmpty(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// accSiteLocked returns the account's site key; the caller holds accountMu.
func accSiteLocked(acc *Account) string {
	if acc == nil {
		return ""
	}
	if acc.Auth != nil {
		return profileForEdition(acc.Auth.Edition).Key
	}
	return profileForEdition(acc.Edition).Key
}
