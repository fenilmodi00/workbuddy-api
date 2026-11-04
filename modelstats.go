package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Model-level statistics (/v1/models side table)
//
// Records per-model request counts, success/failure, upstream usage token
// totals, time-to-first-token (TTFT), average latency, last request time,
// last status, and the number of accounts currently able to serve the model.
// Statistics serve only monitor display and play no part in scheduling decisions.
// -----------------------------------------------------------------------------

// statsWindow is the rolling window for "average first token / average total latency".
const statsWindow = 5 * time.Hour

// hourlyBucket holds TTFT / latency samples accumulated within one clock-hour.
type hourlyBucket struct {
	Hour           int64 // start of that hour (Unix seconds)
	TTFTSum        time.Duration
	TTFTSamples    int64
	LatencySum     time.Duration
	LatencySamples int64
}

type modelStat struct {
	Requests      int64
	Success       int64
	Failed        int64
	Tokens        int64 // upstream usage total_tokens total (requests without usage are not counted)
	LastRequestAt time.Time
	LastStatus    string
	// Latency samples bucketed by hour within the recent statsWindow (expired buckets are evicted automatically).
	Buckets []hourlyBucket
	// Per-site free/paid observations: the same model name may be free on cn and paid on intl.
	CNFreeSeen   bool
	CNPaidSeen   bool
	IntlFreeSeen bool
	IntlPaidSeen bool
}

var (
	modelStatsMu sync.Mutex
	modelStats   = map[string]*modelStat{}
)

func hourStart(t time.Time) int64 {
	return t.Truncate(time.Hour).Unix()
}

// bucketLocked returns the current hour's bucket and evicts buckets outside the window.
func (st *modelStat) bucketLocked(now time.Time) *hourlyBucket {
	cur := hourStart(now)
	// evict buckets outside the window
	kept := st.Buckets[:0]
	for _, b := range st.Buckets {
		if now.Unix()-b.Hour < int64(statsWindow/time.Second) {
			kept = append(kept, b)
		}
	}
	st.Buckets = kept
	for i := range st.Buckets {
		if st.Buckets[i].Hour == cur {
			return &st.Buckets[i]
		}
	}
	st.Buckets = append(st.Buckets, hourlyBucket{Hour: cur})
	return &st.Buckets[len(st.Buckets)-1]
}

// windowAveragesLocked aggregates average first-token and total latency within the window.
func (st *modelStat) windowAveragesLocked(now time.Time) (ttft time.Duration, hasTTFT bool, latency time.Duration, hasLatency bool) {
	var tSum, lSum time.Duration
	var tN, lN int64
	cutoff := now.Unix() - int64(statsWindow/time.Second)
	for _, b := range st.Buckets {
		if b.Hour < cutoff {
			continue
		}
		tSum += b.TTFTSum
		tN += b.TTFTSamples
		lSum += b.LatencySum
		lN += b.LatencySamples
	}
	if tN > 0 {
		ttft, hasTTFT = tSum/time.Duration(tN), true
	}
	if lN > 0 {
		latency, hasLatency = lSum/time.Duration(lN), true
	}
	return
}

func modelStatLocked(model string) *modelStat {
	model = normalizeModelName(model)
	if model == "" {
		model = "(default)"
	}
	stat := modelStats[model]
	if stat == nil {
		stat = &modelStat{}
		modelStats[model] = stat
	}
	return stat
}

func recordModelRequest(model string) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.Requests++
	stat.LastRequestAt = time.Now()
	modelStatsMu.Unlock()
}

func recordModelSuccess(model string) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.Success++
	stat.LastStatus = "Success"
	modelStatsMu.Unlock()
}

func recordModelFailure(model, reason string) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.Failed++
	stat.LastStatus = "Failed:" + reason
	modelStatsMu.Unlock()
}

func recordModelTTFT(model string, d time.Duration) {
	if d <= 0 {
		return
	}
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.bucketLocked(time.Now()).TTFTSum += d
	stat.bucketLocked(time.Now()).TTFTSamples++
	modelStatsMu.Unlock()
}

func recordModelLatency(model string, d time.Duration) {
	if d <= 0 {
		return
	}
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	b := stat.bucketLocked(time.Now())
	b.LatencySum += d
	b.LatencySamples++
	modelStatsMu.Unlock()
}

// recordModelTokens accumulates the upstream usage token count of a successful response onto the model.
// Prefers usage.total_tokens; otherwise uses prompt_tokens + completion_tokens.
// When upstream gives no usage, nothing is estimated to keep guessed numbers out of the table.
func recordModelTokens(model string, usage map[string]any, reqID uint64) {
	direct, hasDirect := usageNumber(usage, "total_tokens")
	prompt, hasPrompt := usageNumber(usage, "prompt_tokens")
	completion, hasCompletion := usageNumber(usage, "completion_tokens")
	n, ok := usageTotalTokens(usage)
	if !ok || n <= 0 {
		log.Printf("[Model usage total] requestId=%d model=%s result=skipped reason=upstream usage has no valid tokens has total=%v has prompt=%v has completion=%v business impact=not counted toward total consumption this time",
			reqID, normalizeModelName(model), hasDirect, hasPrompt, hasCompletion)
		return
	}
	source := "usage.total_tokens"
	if !hasDirect {
		source = fmt.Sprintf("prompt_tokens(%d)+completion_tokens(%d)", prompt, completion)
	}
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	before := stat.Tokens
	stat.Tokens += n
	after := stat.Tokens
	modelStatsMu.Unlock()
	log.Printf("[Model usage total] requestId=%d model=%s source=%s tokens this time=%d before=%d after=%d display=%s note=counted toward total consumption per upstream usage",
		reqID, normalizeModelName(model), source, n, before, after, formatTokensM(after))
	if hasDirect && (hasPrompt || hasCompletion) && direct != prompt+completion {
		log.Printf("[Model usage total] requestId=%d model=%s branch=total and itemized disagree verdict=still using total_tokens reason=upstream gave both total and itemized but they are unequal total=%d prompt=%d completion=%d sum=%d",
			reqID, normalizeModelName(model), direct, prompt, completion, prompt+completion)
	}
}

// recordModelCostClass records one free/paid observation, accumulated per site.
func recordModelCostClass(model, edition string, free bool) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	if edition == "intl" {
		if free {
			stat.IntlFreeSeen = true
		} else {
			stat.IntlPaidSeen = true
		}
	} else {
		if free {
			stat.CNFreeSeen = true
		} else {
			stat.CNPaidSeen = true
		}
	}
	modelStatsMu.Unlock()
}

// TTFT v2: gateway request entry → first valid content/reasoning/tool-call delta parsed.
// Follows dsh's token-delta rules; role/heartbeat/usage/finish control frames are not counted.
const ttftMetric = "first_token_v2"

type ttftReader struct {
	r        io.Reader
	start    time.Time
	once     sync.Once
	d        time.Duration
	complete bool
}

func newTTFTReader(r io.Reader, start time.Time) *ttftReader {
	return &ttftReader{r: r, start: start}
}

func (t *ttftReader) Read(p []byte) (int, error) {
	return t.r.Read(p) // Raw bytes are not tokens; never time here.
}

func (t *ttftReader) duration() time.Duration { return t.d }

func (t *ttftReader) text() string {
	if t.d <= 0 {
		return "unknown (no valid token)"
	}
	return t.d.String()
}

func hasFirstTokenDelta(chunk map[string]any) bool {
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		for _, field := range []string{"content", "reasoning_content", "reasoning"} {
			// Same as dsh: empty strings don't count, but non-empty whitespace strings are valid deltas.
			if text, ok := delta[field].(string); ok && text != "" {
				return true
			}
		}
		calls, _ := delta["tool_calls"].([]any)
		if legacy, ok := delta["function_call"].(map[string]any); ok {
			calls = append(calls, map[string]any{"function": legacy})
		}
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if _, named := fn["name"].(string); named {
				return true
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				return true
			}
		}
	}
	return false
}

func observeFirstToken(reader io.Reader, chunk map[string]any) {
	if t, ok := reader.(*ttftReader); ok && hasFirstTokenDelta(chunk) {
		t.once.Do(func() { t.d = time.Since(t.start) })
	}
}

func completeTokenStream(reader io.Reader, complete bool) {
	if t, ok := reader.(*ttftReader); ok {
		t.complete = complete
	}
}

// One settlement path for all three protocols, streaming and non-streaming. Completeness
// is decided only by the upstream stream itself: read errors or a missing finish reason
// are not counted; after completeness is confirmed, the client disconnecting on [DONE]
// is normal and the first-token sample must not be retroactively erased because the
// request Context is later observed as cancelled.
func finishFirstToken(r *http.Request, model string, reqID uint64, body *ttftReader) {
	complete := body.complete
	known := complete && body.duration() > 0
	if known {
		recordModelTTFT(model, body.duration())
	}
	if p := statsAttemptFor(r); p != nil {
		p.ttft = nil
		if known {
			v := durationMilliseconds(body.duration())
			p.ttft = &v
		}
	}
	log.Printf("[TTFT metrics] traceId=%s requestId=%d model=%s metric=%s complete=%t has sample=%t duration=%s start=gateway request entry end=valid text/reasoning/tool delta note=completeness depends only on the upstream stream ending normally; client disconnect after full completion does not erase the sample; heartbeats and role announcements are not counted and no body text is recorded",
		debugTraceID(r), reqID, model, ttftMetric, complete, known, body.text())
}

// modelStatusFromError extracts a short status label from an error response (business code first, then HTTP status code).
func modelStatusFromError(statusCode int, body string) string {
	if code := extractBusinessCode(body); code != "" {
		return code
	}
	if statusCode > 0 {
		return strconv.Itoa(statusCode)
	}
	return "error"
}

// extractBusinessCode extracts a business code of the form "code":NNNN from the response body.
func extractBusinessCode(body string) string {
	const marker = `"code":`
	idx := strings.Index(body, marker)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return ""
	}
	return rest[:end]
}

// modelStatSnapshot is a single model statistic written into the status snapshot.
type modelStatSnapshot struct {
	ID             string `json:"id"`
	Enabled        *bool  `json:"enabled,omitempty"`
	PolicyReason   string `json:"policyReason,omitempty"`
	Source         string `json:"source,omitempty"`
	CNFree         string `json:"cnFree,omitempty"`         // Domestic site: Yes | No | Mixed | -
	IntlFree       string `json:"intlFree,omitempty"`       // International site: Yes | No | Mixed | -
	CNMultiplier   string `json:"cnMultiplier,omitempty"`   // Domestic site multiplier display value, e.g. 0.29x / 0.00x / -
	IntlMultiplier string `json:"intlMultiplier,omitempty"` // International site multiplier display value
	// AvailableAccounts is the total number of servable accounts across both sites (already filtered by model account allow/block lists).
	AvailableAccounts int    `json:"availableAccounts"`
	Requests          int64  `json:"requests,omitempty"`
	Success           int64  `json:"success,omitempty"`
	Failed            int64  `json:"failed,omitempty"`
	Tokens            int64  `json:"tokens,omitempty"`
	AvgTTFTMs         int64  `json:"avgTtftMs,omitempty"`
	HasTTFT           bool   `json:"hasTtft,omitempty"`
	TTFTMetric        string `json:"ttftMetric,omitempty"`
	AvgLatencyMs      int64  `json:"avgLatencyMs,omitempty"`
	HasLatency        bool   `json:"hasLatency,omitempty"`
	LastRequestAt     int64  `json:"lastRequestAt,omitempty"`
	LastStatus        string `json:"lastStatus,omitempty"`
}

// modelServableLocked read-only check of whether an account can currently serve the model (no state is changed).
// Availability rules match nextAccountForModel.
func modelServableLocked(acc *Account, model string, now time.Time) bool {
	if acc.Disabled || acc.CooldownUntil.After(now) {
		return false
	}
	state := acc.ModelStates[normalizeModelName(model)]
	if state == nil {
		if acc.QuotaExhausted {
			return true // unknown model allows one controlled probe
		}
		return true
	}
	if state.CooldownUntil.After(now) || state.QuotaBlocked {
		return false
	}
	if !acc.QuotaExhausted {
		return true
	}
	switch state.CostClass {
	case modelCostFree:
		return true
	case modelCostPaid:
		return false
	default:
		return !now.Before(state.NextProbeAt)
	}
}

// modelSourceFromSnapshots takes the catalog source version from the model stat rows (for the monitor title).
func modelSourceFromSnapshots(rows []modelStatSnapshot) string {
	for _, row := range rows {
		if row.Source != "" {
			return row.Source
		}
	}
	return ""
}

func freeLabel(freeSeen, paidSeen bool) string {
	switch {
	case freeSeen && paidSeen:
		return "Mixed"
	case freeSeen:
		return "Yes"
	case paidSeen:
		return "No"
	default:
		return "-"
	}
}

// freeModelDisplayCounts reuses only the site multipliers finally displayed by the model stats table.
// These display counts must not be used to rewrite account measured ledgers, or to decide
// whether an account may request a model.
func freeModelDisplayCounts(rows []modelStatSnapshot) (cn, intl int) {
	for _, row := range rows {
		if row.CNMultiplier == "0.00x" {
			cn++
		}
		if row.IntlMultiplier == "0.00x" {
			intl++
		}
	}
	return cn, intl
}

// The caller holds accountMu; log only when display counts change, avoiding a repeat every 3 seconds.
var lastFreeModelDisplay struct {
	initialized bool
	cn, intl    int
}

// buildModelStatSnapshots aggregates stat rows for "official/static model catalog ∪ models with statistics".
func buildModelStatSnapshots(now time.Time, accs []*Account) []modelStatSnapshot {
	return buildModelRows(now, accs, false)
}

func buildAllModelStatSnapshots(now time.Time, accs []*Account) []modelStatSnapshot {
	return buildModelRows(now, accs, true)
}

func buildModelRows(now time.Time, accs []*Account, includeDisabled bool) []modelStatSnapshot {
	ids, source := collectModelIDs(includeDisabled)
	if includeDisabled {
		modelFilterMu.RLock()
		for id := range currentFilter.blocklist {
			ids = append(ids, id)
		}
		for id := range currentFilter.allowlist {
			ids = append(ids, id)
		}
		modelFilterMu.RUnlock()
	}
	seen := make(map[string]bool, len(ids))
	rows := make([]modelStatSnapshot, 0, len(ids))

	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()

	appendRow := func(id string) {
		id = normalizeModelName(id)
		if id == "" || seen[id] {
			return
		}
		// Models disabled by config are hidden directly in the stats side table (even with past requests).
		disabled, reason := modelDisabled(id)
		if disabled && !includeDisabled {
			return
		}
		seen[id] = true
		row := modelStatSnapshot{ID: id, Source: source}
		enabled := !disabled
		row.Enabled, row.PolicyReason = &enabled, reason
		// Free/paid is aggregated per site: the same model name may have different
		// verdicts on cn and intl; mixing them would display as a meaningless "Mixed".
		var cnFree, cnPaid, intlFree, intlPaid bool
		for _, acc := range accs {
			allowed, _ := modelAccountAllowed(id, acc, accs)
			if !disabled && allowed && modelServableLocked(acc, id, now) {
				row.AvailableAccounts++
			}
			state := acc.ModelStates[id]
			if state == nil {
				continue
			}
			isIntl := acc.Profile().Key == "intl"
			switch state.CostClass {
			case modelCostFree:
				if isIntl {
					intlFree = true
				} else {
					cnFree = true
				}
			case modelCostPaid:
				if isIntl {
					intlPaid = true
				} else {
					cnPaid = true
				}
			}
		}
		if stat := modelStats[id]; stat != nil {
			row.Requests = stat.Requests
			row.Success = stat.Success
			row.Failed = stat.Failed
			row.Tokens = stat.Tokens
			cnFree = cnFree || stat.CNFreeSeen
			cnPaid = cnPaid || stat.CNPaidSeen
			intlFree = intlFree || stat.IntlFreeSeen
			intlPaid = intlPaid || stat.IntlPaidSeen
			row.LastStatus = stat.LastStatus
			row.LastRequestAt = unixOrZero(stat.LastRequestAt)
			// Averages use the rolling statsWindow (5 hours).
			if ttft, okT, latency, okL := stat.windowAveragesLocked(now); true {
				if okT {
					row.AvgTTFTMs = ttft.Milliseconds()
					row.HasTTFT = true
					row.TTFTMetric = ttftMetric
				}
				if okL {
					row.AvgLatencyMs = latency.Milliseconds()
					row.HasLatency = true
				}
			}
		}
		row.CNFree = freeLabel(cnFree, cnPaid)
		row.IntlFree = freeLabel(intlFree, intlPaid)
		row.CNMultiplier = modelDisplayMultiplier("cn", id, row.CNFree)
		row.IntlMultiplier = modelDisplayMultiplier("intl", id, row.IntlFree)
		rows = append(rows, row)
	}

	for _, id := range ids {
		appendRow(id)
	}
	for _, id := range modelAccountRuleIDs() {
		appendRow(id)
	}
	for id := range modelStats {
		appendRow(id)
	}
	return rows
}

func formatMilliseconds(ms int64, has bool) string {
	if !has {
		return "-"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// formatTokensM formats a token count in millions for the model stats table.
func formatTokensM(n int64) string {
	if n <= 0 {
		return "0.00M"
	}
	m := float64(n) / 1_000_000
	if m < 0.01 {
		return fmt.Sprintf("%.4fM", m)
	}
	return fmt.Sprintf("%.2fM", m)
}

// renderModelTable renders the /v1/models stats side table.
func renderModelTable(rows []modelStatSnapshot) string {
	widths := []int{22, 14, 16, 15, 10, 20, 15, 17}
	headers := []string{"Model", "CN multiplier", "Intl multiplier", "Avail. accounts", "Requests", "Avg first token (5h)", "Avg total (5h)", "Total tokens (M)"}
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
	for _, r := range rows {
		cnMult := r.CNMultiplier
		if cnMult == "" {
			cnMult = "-"
		}
		intlMult := r.IntlMultiplier
		if intlMult == "" {
			intlMult = "-"
		}
		b.WriteString(row([]string{
			r.ID, cnMult, intlMult, strconv.Itoa(r.AvailableAccounts),
			strconv.FormatInt(r.Requests, 10),
			formatMilliseconds(r.AvgTTFTMs, r.HasTTFT && r.TTFTMetric == ttftMetric),
			formatMilliseconds(r.AvgLatencyMs, r.HasLatency),
			formatTokensM(r.Tokens),
		}) + "\n")
	}
	b.WriteString(border())
	return b.String()
}

// modelRequestCount returns a model's client request count (to decide whether a price probe is worthwhile).
func modelRequestCount(model string) int64 {
	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()
	if stat := modelStats[normalizeModelName(model)]; stat != nil {
		return stat.Requests
	}
	return 0
}
