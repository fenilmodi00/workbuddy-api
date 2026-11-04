package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Upstream errors may contain user identifiers or token fragments; redact them
// uniformly before logging.
var (
	uuidLikeRe   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	bearerLikeRe = regexp.MustCompile(`(?i)(bearer\s+)?eyJ[A-Za-z0-9_\-]{10,}(\.[A-Za-z0-9_\-]+){1,2}`)
)

// Proactive renewal (keepalive) parameters. Modeled after ckldy/workbuddy2api's
// keepalive_hours scheduling: proactively refresh all accounts at fixed local
// times instead of waiting until access tokens near expiry.
const (
	// keepaliveAccountGap is the serial gap between accounts. Refreshing login
	// states in concurrent batches is an obvious machine fingerprint; serial
	// plus throttling better matches how a real person uses them one by one.
	keepaliveAccountGap = 1 * time.Second
	// keepaliveCatchUpDelay is the startup catch-up delay: let the service
	// stabilize first, then redo a missed renewal.
	keepaliveCatchUpDelay = 90 * time.Second
	// keepaliveOverdueAfter: an account overdue by this long since its last
	// successful renewal is considered in need of a catch-up run.
	keepaliveOverdueAfter = 20 * time.Hour
	// refreshDisableThreshold is how many consecutive refresh failures are
	// needed before the login state is declared dead. A single failure may be
	// upstream jitter, risk control, or a network problem; deleting credentials
	// on one failure would be a false kill.
	refreshDisableThreshold = 3
)

// refreshFailureKind distinguishes "the login state is really gone" from "this
// attempt just did not succeed".
type refreshFailureKind int

const (
	refreshFailureTransient refreshFailureKind = iota
	refreshFailureSessionDead
)

func (k refreshFailureKind) String() string {
	if k == refreshFailureSessionDead {
		return "session dead"
	}
	return "transient failure"
}

// sessionDeadMarkers are markers the upstream uses to explicitly say "the
// refresh token is no longer usable". Only hits on these markers are treated as
// a dead login state; ordinary 401/403 may be permissions or risk control and
// requires several consecutive failures to conclude, avoiding deleting user
// credentials over one jitter.
var sessionDeadMarkers = []string{
	"12153",
	"offline user session not found",
	"invalid_grant",
	"token is not active",
	"refresh token failed",
	"session not found",
}

func classifyRefreshFailure(status int, body string) refreshFailureKind {
	low := strings.ToLower(body)
	for _, marker := range sessionDeadMarkers {
		if strings.Contains(low, marker) {
			return refreshFailureSessionDead
		}
	}
	return refreshFailureTransient
}

// hourList parses a local hour list like "-keepalive-hours 22" or
// "-keepalive-hours 10,22". An empty value disables proactive renewal.
type hourList []int

func (h *hourList) String() string {
	if h == nil || len(*h) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*h))
	for _, v := range *h {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
}

func (h *hourList) Set(value string) error {
	var out []int
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		n, err := strconv.Atoi(item)
		if err != nil {
			return fmt.Errorf("renewal hour %q is not an integer in 0-23", item)
		}
		if n < 0 || n > 23 {
			return fmt.Errorf("renewal hour %d is out of the 0-23 range", n)
		}
		out = append(out, n)
	}
	sort.Ints(out)
	*h = out
	return nil
}

// nextKeepaliveTime returns the nearest configured time after now (local
// timezone).
func nextKeepaliveTime(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

func formatKeepaliveHours(hours []int) string {
	parts := make([]string, 0, len(hours))
	for _, h := range hours {
		parts = append(parts, fmt.Sprintf("%02d:00", h))
	}
	return strings.Join(parts, ", ")
}

// jwtExpiry reads exp from the JWT payload, only for display and expiry
// warning; no signature verification.
// Manually imported credentials can also show the refresh token's expiry this
// way.
func jwtExpiry(token string) (time.Time, bool) {
	claims := jwtClaims(token)
	if claims == nil {
		return time.Time{}, false
	}
	switch v := claims["exp"].(type) {
	case float64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(v), 0), true
	case json.Number:
		n, err := v.Int64()
		if err != nil || n <= 0 {
			return time.Time{}, false
		}
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

// refreshTokenExpiry returns the refresh token's expiry time: prefer the field
// recorded at renewal, else read unverified token claims; if neither is
// available it is unknown.
func refreshTokenExpiry(tokens StoredTokens) (time.Time, string, bool) {
	if tokens.RefreshExpiresAt > 0 {
		return time.Unix(tokens.RefreshExpiresAt, 0), "renewal API response", true
	}
	if exp, ok := jwtExpiry(tokens.RefreshToken); ok {
		return exp, "token claims", true
	}
	return time.Time{}, "", false
}

// isKeepaliveOverdue reports whether the account has not renewed successfully
// for a long time and needs a catch-up run at startup.
func isKeepaliveOverdue(acc *Account, now time.Time) bool {
	if acc == nil || acc.Auth == nil {
		return false
	}
	last := acc.Auth.Auth.LastRefreshTime
	if last <= 0 {
		return true
	}
	return now.Sub(time.Unix(last, 0)) >= keepaliveOverdueAfter
}

// keepaliveLoop is the proactive renewal scheduler: refresh all accounts at
// each configured time, plus a catch-up run at startup for a missed round.
// Only the token refresh API is used; no model requests, no quota consumed.
func keepaliveLoop() {
	if len(cfg.KeepaliveHours) == 0 {
		logScheduledTask("keepalive", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "no proactive renewal times configured")
		logScheduledTask("keepalive_catchup", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "proactive renewal disabled; startup catch-up does not run")
		log.Printf("[Renewal keepalive] stage=Startup result=Disabled reason=No proactive renewal times configured business_impact=Refresh only happens when access tokens near expiry; long-idle accounts may silently die")
		return
	}
	log.Printf("[Renewal keepalive] stage=Startup result=Enabled renewal_hours=%s note=Proactively refreshes all accounts' login credentials at the configured times; only calls the refresh API, no model requests, no quota consumed",
		formatKeepaliveHours(cfg.KeepaliveHours))

	go func() {
		next := time.Now().Add(keepaliveCatchUpDelay)
		logScheduledTask("keepalive_catchup", "scheduled", "", newTraceID(), time.Time{}, next, "90 seconds after startup; only checks long-unrenewed accounts")
		time.Sleep(keepaliveCatchUpDelay)
		now := time.Now()
		accountMu.Lock()
		overdue := 0
		for _, acc := range accounts {
			if acc.Disabled || acc.Auth == nil {
				continue
			}
			if isKeepaliveOverdue(acc, now) {
				overdue++
			}
		}
		accountMu.Unlock()
		if overdue == 0 {
			logScheduledTask("keepalive_catchup", "completed", "skipped", newTraceID(), now, time.Time{}, "all accounts renewed recently; no catch-up needed")
			log.Printf("[Renewal keepalive] stage=Startup catch-up result=Skipped reason=All accounts successfully renewed within %v", keepaliveOverdueAfter)
			return
		}
		log.Printf("[Renewal keepalive] stage=Startup catch-up accounts_due=%d reason=More than %v since last successful renewal or never renewed note=Service is stable; starting a catch-up round", overdue, keepaliveOverdueAfter)
		runKeepaliveOnce("startup catch-up", true)
	}()

	for {
		next := nextKeepaliveTime(time.Now(), cfg.KeepaliveHours)
		logScheduledTask("keepalive", "scheduled", "", newTraceID(), time.Time{}, next, "server local time; accounts renewed serially with a 1-second gap")
		log.Printf("[Renewal keepalive] stage=Waiting for next renewal time=%s until=%s", next.Format("2006-01-02 15:04:05"), time.Until(next).Truncate(time.Second))
		timer := time.NewTimer(time.Until(next))
		<-timer.C
		runKeepaliveOnce("scheduled", false)
	}
}

// runKeepaliveOnce refreshes the account pool serially; when onlyOverdue is
// true only long-unrenewed accounts are processed.
func runKeepaliveOnce(trigger string, onlyOverdue bool) {
	task := "keepalive"
	next := nextKeepaliveTime(time.Now(), cfg.KeepaliveHours)
	if onlyOverdue {
		task, next = "keepalive_catchup", time.Time{}
	}
	trace, started := startScheduledTask(task, next)
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()

	now := time.Now()
	log.Printf("[Renewal keepalive] stage=Execution trigger=%s account_total=%d only_overdue=%t result=Started", trigger, len(accs), onlyOverdue)

	refreshed, skipped, failed, disabled := 0, 0, 0, 0
	first := true
	for _, acc := range accs {
		accountMu.Lock()
		disabledAccount := acc.Disabled
		hasAuth := acc.Auth != nil
		path := acc.Path
		hasRefresh := hasAuth && acc.Auth.Auth.RefreshToken != ""
		overdue := hasAuth && isKeepaliveOverdue(acc, now)
		accountMu.Unlock()

		if disabledAccount {
			skipped++
			log.Printf("[Renewal keepalive] account=%s result=Skipped reason=Account already marked dead; not scheduled", path)
			continue
		}
		if !hasAuth || !hasRefresh {
			skipped++
			log.Printf("[Renewal keepalive] account=%s result=Skipped reason=Missing refresh token; cannot renew, re-login required", path)
			continue
		}
		if onlyOverdue && !overdue {
			skipped++
			continue
		}
		if !first {
			time.Sleep(keepaliveAccountGap) // Serial throttling avoids the machine fingerprint of concurrent batch refreshes
		}
		first = false

		if err := refreshAccountToken(acc, "proactive renewal/"+trigger); err != nil {
			failed++
			accountMu.Lock()
			isDisabled := acc.Disabled
			accountMu.Unlock()
			if isDisabled {
				disabled++
			}
			continue
		}
		refreshed++
	}

	log.Printf("[Renewal keepalive] stage=Execution completed trigger=%s success=%d failed=%d skipped=%d disabled=%d business_impact=Successful accounts' login states extended; failed accounts keep original credentials and wait for the next retry",
		trigger, refreshed, failed, skipped, disabled)
	status := "success"
	if failed > 0 && refreshed == 0 {
		status = "failed"
	} else if failed > 0 {
		status = "partial"
	} else if refreshed == 0 {
		status = "skipped"
	}
	logScheduledTask(task, "completed", status, trace, started, next, fmt.Sprintf("trigger=%s, success=%d, failed=%d, skipped=%d, disabled=%d", trigger, refreshed, failed, skipped, disabled))
	if refreshed > 0 || disabled > 0 {
		writeStatusSnapshot()
	}
}

// refreshFailureSummary generates a failure summary without tokens or upstream
// user identifiers, for logging and status display.
func refreshFailureSummary(kind refreshFailureKind, status int, err error) string {
	detail := ""
	if err != nil {
		detail = sanitizeUpstreamText(err.Error())
	}
	if len(detail) > 160 {
		detail = detail[:160] + "..."
	}
	return fmt.Sprintf("%s (HTTP %d) %s", kind, status, detail)
}

// sanitizeUpstreamText removes account identifiers and token fragments that may
// appear in upstream errors.
func sanitizeUpstreamText(text string) string {
	text = uuidLikeRe.ReplaceAllString(text, "[redacted]")
	text = bearerLikeRe.ReplaceAllString(text, "[redacted]")
	text = strings.ReplaceAll(text, "\n", " ")
	return strings.TrimSpace(text)
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	if days := int(d.Hours()) / 24; days > 0 {
		return fmt.Sprintf("%d days", days)
	}
	return fmt.Sprintf("%d hours %d minutes", int(d.Hours()), int(d.Minutes())%60)
}

// formatRenewalStatus generates the login renewal info for the status command.
// The caller must hold accountMu (this function does not lock again).
func formatRenewalStatus(acc *Account) string {
	if acc == nil || acc.Auth == nil {
		return ""
	}
	tokens := acc.Auth.Auth
	var sb strings.Builder
	if exp, source, ok := refreshTokenExpiry(tokens); ok {
		remain := time.Until(exp)
		state := "valid"
		switch {
		case remain <= 0:
			state = "expired, re-login required"
		case remain < 7*24*time.Hour:
			state = "expiring soon; re-login recommended as soon as possible"
		}
		sb.WriteString(fmt.Sprintf("Refresh token:  until %s (%s, remaining %s, %s)\n",
			exp.Local().Format("2006-01-02 15:04:05"), source, humanDuration(remain), state))
	} else {
		sb.WriteString("Refresh token:  expiry unknown (renewal API returned nothing and the token is unparseable)\n")
	}
	if tokens.LastRefreshTime > 0 {
		sb.WriteString(fmt.Sprintf("Last renewal:   %s\n", time.Unix(tokens.LastRefreshTime, 0).Format("2006-01-02 15:04:05")))
	} else {
		sb.WriteString("Last renewal:   no record yet (written after a successful proactive renewal)\n")
	}
	if acc.RefreshFailCount > 0 {
		sb.WriteString(fmt.Sprintf("Renewal failed: %d consecutive failures, last reason=%s\n", acc.RefreshFailCount, acc.LastRefreshError))
	} else if acc.LastRefreshError != "" {
		sb.WriteString(fmt.Sprintf("Renewal warning: %s\n", acc.LastRefreshError))
	}
	return sb.String()
}
