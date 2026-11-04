package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func resetStatsState(t *testing.T) {
	t.Helper()
	requestStats.Lock()
	requestStats.document = statsDocument{Schema: statsSchema, Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}
	requestStats.revision, requestStats.saved, requestStats.lastSaved = 0, 0, 0
	requestStats.blocked, requestStats.enabled, requestStats.mode, requestStats.problem = false, false, "pending", ""
	requestStats.Unlock()
	t.Cleanup(func() {
		requestStats.Lock()
		requestStats.document = statsDocument{Schema: statsSchema, Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}
		requestStats.revision, requestStats.saved, requestStats.lastSaved = 0, 0, 0
		requestStats.blocked, requestStats.enabled, requestStats.mode, requestStats.problem = false, false, "pending", ""
		requestStats.Unlock()
	})
}

func statsTestRequest(account, model string, at time.Time, tokens *int64, credit *float64) statsRequest {
	return statsRequest{Account: account, Model: model, At: at.UnixMilli(), Status: 200, Complete: true,
		TotalTokens: tokens, Credit: credit, LatencyMS: 1000}
}

func int64p(v int64) *int64       { return &v }
func float64p(v float64) *float64 { return &v }

func statsQuery(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleWebUIStats(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats?"+query, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil && rec.Code == http.StatusOK {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	return rec.Code, body
}

func TestStatsServerTimePreservesFutureBuckets(t *testing.T) {
	resetStatsState(t)
	before := time.Now().UnixMilli()
	day := time.Now().In(statsZone).AddDate(0, 0, 1).Format("2006-01-02")
	code, body := statsQuery(t, "period=hour&start="+day+"&end="+day)
	after := time.Now().UnixMilli()
	if code != http.StatusOK {
		t.Fatalf("stats status=%d", code)
	}
	serverTime, ok := body["serverTime"].(float64)
	if !ok || int64(serverTime) < before || int64(serverTime) > after {
		t.Fatalf("serverTime must be current Unix milliseconds: %v", body["serverTime"])
	}
	buckets := body["buckets"].([]any)
	if len(buckets) != 24 {
		t.Fatalf("preserve full future-day time axis, got %d buckets", len(buckets))
	}
	for _, item := range buckets {
		bucket := item.(map[string]any)
		if bucket["start"].(float64) <= serverTime || bucket["requests"].(float64) != 0 {
			t.Fatalf("expected future empty bucket, got %v", bucket)
		}
	}
}

// Silently enabled when the file is missing; starts from empty stats without affecting gateway operation.
func TestStatsMissingFileStartsEmpty(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	defer requestStats.Unlock()
	if !requestStats.enabled || requestStats.blocked || requestStats.mode != "pending" {
		t.Fatalf("missing file should enable silently: %+v", requestStats.mode)
	}
	if len(requestStats.document.Requests) != 0 || len(requestStats.document.Accounts) != 0 {
		t.Fatal("should start from empty stats when the file is missing")
	}
}

// A bad file must not be overwritten, nor block operation.
func TestStatsCorruptFileIsPreserved(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	const broken = `{"schema":1,"requests":`
	if err := os.WriteFile(statsFile, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	loadOptionalStats()
	requestStats.Lock()
	blocked, mode := requestStats.blocked, requestStats.mode
	requestStats.Unlock()
	if !blocked || mode != "memory" {
		t.Fatalf("bad file should switch to memory-only: blocked=%t mode=%s", blocked, mode)
	}
	// After appending data, attempt persistence: the original file must be preserved, never overwritten.
	requestStats.Lock()
	requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
		Account: "a", Model: "m", At: time.Now().UnixMilli(), Status: 200, LatencyMS: 1})
	requestStats.revision++
	requestStats.Unlock()
	// Explicitly skips writing (rather than erroring and retrying): the bad file must be kept as-is.
	if err := flushOptionalStats(); err != nil {
		t.Fatalf("bad file state should silently skip persistence, got: %v", err)
	}
	raw, err := os.ReadFile(statsFile)
	if err != nil || string(raw) != broken {
		t.Fatalf("original file was modified: %q err=%v", string(raw), err)
	}
}

func TestStatsValidFileRestores(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	at := time.Now().Add(-time.Hour).UnixMilli()
	// The stats ID is derived from "file + site + UID + enterprise ID"; here a real ID is computed from the same account.
	accountMu.Lock()
	acc := &Account{Path: "workbuddy-a.json", Edition: "cn", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "u1"}}}
	id, view := statsAccountIDLocked(acc)
	accountMu.Unlock()
	view.ModelStates = map[string]modelStateSnapshot{
		"glm-5.3-flash": {CostClass: modelCostPaid, QuotaBlocked: true}}
	doc := statsDocument{Schema: statsSchema, StartedAt: at,
		Accounts: map[string]statsAccount{id: view},
		Requests: []statsRequest{{Account: id, Model: "glm-5.3-flash", At: at, Status: 200,
			Complete: true, TotalTokens: int64p(120), LatencyMS: 500}}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(statsFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loadOptionalStats()
	requestStats.Lock()
	got := requestStats.document
	mode := requestStats.mode
	requestStats.Unlock()
	if mode != "file" || len(got.Requests) != 1 || len(got.Accounts) != 1 {
		t.Fatalf("valid file not restored: mode=%s requests=%d accounts=%d", mode, len(got.Requests), len(got.Accounts))
	}
	// The restored model ledger must land back on the account and keep its original cost class.
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	restoreStatsAccountStatesLocked()
	accounts = oldAccounts
	accountMu.Unlock()
	state := acc.ModelStates["glm-5.3-flash"]
	if state == nil || state.CostClass != modelCostPaid || !state.QuotaBlocked {
		t.Fatalf("model ledger not restored onto account: %+v", state)
	}
}

func TestAggregateModelStatsWindow(t *testing.T) {
	resetStatsState(t)
	now := time.Now()
	tokensA, tokensB := int64(100), int64(50)
	ttft := 800.0
	requestStats.Lock()
	requestStats.document.Requests = []statsRequest{
		{Account: "a", Model: "m1", At: now.Add(-time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokensA, LatencyMS: 2000, TTFTMS: &ttft, TTFTMetric: ttftMetric},
		{Account: "a", Model: "m1", At: now.Add(-2 * time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokensA, LatencyMS: 4000, TTFTMS: &ttft, TTFTMetric: ttftMetric},
		{Account: "a", Model: "m1", At: now.Add(-6 * time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokensB, LatencyMS: 9000},
		{Account: "a", Model: "m2", At: now.Add(-time.Hour).UnixMilli(), Status: 500, Complete: false,
			LatencyMS: 1000},
	}
	requestStats.Unlock()

	window := aggregateModelStatsWindow(now.Add(-5*time.Hour), now)
	m1, ok := window["m1"]
	if !ok || m1.Requests != 2 || m1.Success != 2 || m1.Failed != 0 || m1.Tokens != 200 {
		t.Fatalf("m1 window aggregation wrong: %+v", m1)
	}
	if m1.AvgTTFTMS == nil || *m1.AvgTTFTMS != 800 {
		t.Fatalf("m1 first-token average wrong: %+v", m1.AvgTTFTMS)
	}
	if m1.AvgLatencyMS == nil || *m1.AvgLatencyMS != 3000 {
		t.Fatalf("m1 total duration average wrong: %+v", m1.AvgLatencyMS)
	}
	m2, ok := window["m2"]
	if !ok || m2.Requests != 1 || m2.Failed != 1 || m2.AvgTTFTMS != nil {
		t.Fatalf("m2 failed request must not count toward first token: %+v", m2)
	}
}

func TestHandleWebUIModelsRange5hUsesPersistedStats(t *testing.T) {
	isolatedGatewayConfig(t)
	resetStatsState(t)
	now := time.Now()
	snap := statusSnapshot{UpdatedAt: now.Unix(), ModelCatalog: []modelStatSnapshot{
		{ID: "m1", Requests: 999, Tokens: 999999, HasTTFT: true, AvgTTFTMs: 9999, HasLatency: true, AvgLatencyMs: 9999},
		{ID: "m2", Requests: 0},
	}}
	raw, _ := json.Marshal(snap)
	if err := os.WriteFile(statusSnapshotFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	tokens := int64(100)
	ttft := 800.0
	requestStats.Lock()
	requestStats.document.Requests = []statsRequest{
		{Account: "a", Model: "m1", At: now.Add(-time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokens, LatencyMS: 2000, TTFTMS: &ttft, TTFTMetric: ttftMetric},
		{Account: "a", Model: "m1", At: now.Add(-6 * time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokens, LatencyMS: 9000},
	}
	requestStats.Unlock()

	rec := httptest.NewRecorder()
	handleWebUIModels(rec, httptest.NewRequest(http.MethodGet, "/admin/api/models?range=5h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("range=5h status code=%d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Range  string              `json:"range"`
		Models []modelStatSnapshot `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Range != "5h" {
		t.Fatalf("response did not label the stats window: %#v", body.Range)
	}
	byID := map[string]modelStatSnapshot{}
	for _, m := range body.Models {
		byID[m.ID] = m
	}
	m1 := byID["m1"]
	if m1.Requests != 1 || m1.Tokens != 100 || m1.AvgTTFTMs != 800 || !m1.HasTTFT ||
		m1.AvgLatencyMs != 2000 || !m1.HasLatency {
		t.Fatalf("model list did not reuse the stats.json window: %+v", m1)
	}
	if m2 := byID["m2"]; m2.Requests != 0 || m2.HasTTFT || m2.HasLatency {
		t.Fatalf("models without calls in the window should show empty stats: %+v", m2)
	}

	rec = httptest.NewRecorder()
	handleWebUIModels(rec, httptest.NewRequest(http.MethodGet, "/admin/api/models?range=24h", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid range should return 400: %d %s", rec.Code, rec.Body.String())
	}
}

// One record per actual account call; failed retries kept separate; rejections without an account selection are not recorded.
func TestStatsRecordsAttemptWithUsageAndTiming(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	body := newTTFTReader(strings.NewReader("data: x"), time.Now().Add(-300*time.Millisecond))
	_, _ = body.Read(make([]byte, 4))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	attempt := &statsAttempt{id: "acct", account: statsAccount{File: "workbuddy-a.json", Edition: "cn"},
		model: "glm-5.3-flash", started: time.Now().Add(-1200 * time.Millisecond)}
	r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, attempt))
	observeFirstToken(body, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "hello"}}}})
	completeTokenStream(body, true)
	finishFirstToken(r, "glm-5.3-flash", 1, body)
	statsObserveUsage(r, map[string]any{"total_tokens": float64(300), "prompt_tokens": float64(200),
		"completion_tokens": float64(100), "credit": 1.25}, true)
	statsFinishAttempt(r, http.StatusOK)
	requestStats.Lock()
	defer requestStats.Unlock()
	if len(requestStats.document.Requests) != 1 {
		t.Fatalf("expected 1 record: %d", len(requestStats.document.Requests))
	}
	row := requestStats.document.Requests[0]
	if row.TotalTokens == nil || *row.TotalTokens != 300 || row.Credit == nil || *row.Credit != 1.25 {
		t.Fatalf("usage not persisted: %+v", row)
	}
	if row.TTFTMS == nil || *row.TTFTMS <= 0 {
		t.Fatalf("first-byte duration not recorded: %+v", row.TTFTMS)
	}
	if row.LatencyMS <= 0 {
		t.Fatalf("total duration not recorded: %v", row.LatencyMS)
	}
	if _, ok := requestStats.document.Accounts["acct"]; !ok {
		t.Fatal("account selection must register the account identity")
	}
}

// Rejections without an account selection (auth, parameter errors) produce no account usage record.
func TestStatsIgnoresRequestsWithoutAccountSelection(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, &statsAttempt{}))
	statsFinishAttempt(r, http.StatusBadRequest)
	requestStats.Lock()
	defer requestStats.Unlock()
	if len(requestStats.document.Requests) != 0 {
		t.Fatalf("must not record without account selection: %+v", requestStats.document.Requests)
	}
}

// Missing usage is recorded as an empty sample, not zero usage; 0 credits and unknown credits must be distinguishable.
func TestStatsDistinguishesUnknownFromZero(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	now := time.Now()
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Requests = []statsRequest{
		statsTestRequest("a", "m", now, nil, nil),
		statsTestRequest("a", "m", now, int64p(0), float64p(0)),
	}
	requestStats.Unlock()
	code, body := statsQuery(t, "period=day&account=a")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	summary := body["summary"].(map[string]any)
	if summary["tokenSamples"] != float64(1) || summary["tokens"] != float64(0) {
		t.Fatalf("unknown tokens and zero tokens not distinguished: %+v", summary)
	}
	if summary["creditSamples"] != float64(1) || summary["credits"] != float64(0) {
		t.Fatalf("unknown credits and zero credits not distinguished: %+v", summary)
	}
	if summary["requests"] != float64(2) {
		t.Fatalf("request count should include all calls: %+v", summary)
	}
}

// UTC+8 calendar: hours aligned to the hour, weeks start Monday, months by calendar month; account dimensions never mix data.
func TestStatsBucketsUseUTC8AndIsolateAccounts(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	// 2026-10-14 is a Wednesday; Monday should be 2026-10-12.
	monday := time.Date(2026, 10, 12, 9, 30, 0, 0, statsZone)
	sunday := time.Date(2026, 10, 18, 23, 30, 0, 0, statsZone)
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Accounts["b"] = statsAccount{File: "b.json", Edition: "intl"}
	requestStats.document.Requests = []statsRequest{
		statsTestRequest("a", "m", monday, int64p(100), float64p(1)),
		statsTestRequest("a", "m", sunday, int64p(200), float64p(2)),
		statsTestRequest("b", "m", monday, int64p(999), float64p(9)),
	}
	requestStats.Unlock()
	code, body := statsQuery(t, "period=week&account=a&start=2026-10-01&end=2026-10-31")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	buckets := body["buckets"].([]any)
	var total float64
	for _, raw := range buckets {
		row := raw.(map[string]any)
		if row["label"] == "2026-10-12" {
			if row["tokens"] != float64(300) {
				t.Fatalf("Monday bucket should merge Monday and Sunday: %+v", row)
			}
			total += row["tokens"].(float64)
		}
	}
	if total != 300 {
		t.Fatalf("account dimension not isolated; other account data appeared: total=%v", total)
	}
	if body["summary"].(map[string]any)["tokens"] != float64(300) {
		t.Fatalf("summary not filtered by account: %+v", body["summary"])
	}
}

// Detail ranked by model: only models with calls are listed, higher usage (tokens) first,
// unknown tokens fall back to request count, and account plus time-range filters apply.
func TestStatsModelRanking(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	today := time.Now().In(statsZone).Format("2006-01-02")
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Accounts["b"] = statsAccount{File: "b.json", Edition: "intl"}
	requestStats.document.Requests = []statsRequest{
		statsTestRequest("a", "small-model", time.Now(), int64p(100), float64p(1)),
		statsTestRequest("a", "big-model", time.Now(), int64p(9000), float64p(5)),
		statsTestRequest("a", "mid-model", time.Now(), int64p(3000), float64p(2)),
		// Calls but no usage: still listed, ranked last by request count.
		statsTestRequest("a", "no-usage", time.Now(), nil, nil),
		// Outside the selected date range: must not enter the ranking.
		statsTestRequest("a", "old-model", time.Now().AddDate(0, 0, -40), int64p(999999), float64p(99)),
		// Another account: visible globally, invisible after single-account filtering.
		statsTestRequest("b", "intl-model", time.Now(), int64p(5000), float64p(3)),
	}
	requestStats.Unlock()
	names := func(body map[string]any) []string {
		t.Helper()
		raw, ok := body["models"].([]any)
		if !ok {
			t.Fatalf("response is missing the models field: %v", body["models"])
		}
		out := make([]string, 0, len(raw))
		for _, row := range raw {
			out = append(out, row.(map[string]any)["model"].(string))
		}
		return out
	}
	// Single account + today: descending by tokens, no-usage model last.
	code, body := statsQuery(t, "period=day&account=a&start="+today+"&end="+today)
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	want := []string{"big-model", "mid-model", "small-model", "no-usage"}
	if got := names(body); len(got) != len(want) {
		t.Fatalf("wrong ranking row count: %v", got)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("wrong ranking: got %v want %v", got, want)
			}
		}
	}
	// Global: includes the other account's model, still excludes the out-of-range model.
	if code, body := statsQuery(t, "period=day&start="+today+"&end="+today); code != http.StatusOK {
		t.Fatalf("status=%d", code)
	} else if got := names(body); len(got) != 5 || got[0] != "big-model" {
		t.Fatalf("wrong global ranking: %v", got)
	}
}

// Latency trends must stay per model. Different models in the same bucket must not merge into one average,
// buckets without calls for a model are null, and account and date filters apply.
func TestStatsLatencySeriesStaysPerModel(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	day1 := time.Date(2026, 10, 13, 10, 0, 0, 0, statsZone)
	day2 := time.Date(2026, 10, 14, 11, 0, 0, 0, statsZone)
	req := func(account, model string, at time.Time, tokens int64, latency, ttft float64) statsRequest {
		row := statsTestRequest(account, model, at, int64p(tokens), nil)
		row.LatencyMS = latency
		row.TTFTMS, row.TTFTMetric, row.Complete, row.Status = &ttft, ttftMetric, true, 200
		return row
	}
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Accounts["b"] = statsAccount{File: "b.json", Edition: "intl"}
	requestStats.document.Requests = []statsRequest{
		req("a", "model-fast", day1, 10, 100, 30),
		req("a", "model-fast", day1, 10, 300, 50),
		req("a", "model-fast", day2, 10, 300, 60),
		req("a", "model-slow", day1, 50, 900, 300),
		req("b", "other-model", day1, 999, 50, 20),
		req("a", "old-model", time.Date(2026, 10, 1, 10, 0, 0, 0, statsZone), 999999, 10, 10),
	}
	requestStats.Unlock()

	code, body := statsQuery(t, "period=day&account=a&start=2026-10-13&end=2026-10-14")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%v", code, body)
	}
	raw, _ := body["modelLatency"].([]any)
	if len(raw) != 2 {
		t.Fatalf("should keep only models with calls for the selected account and dates: %+v", raw)
	}
	if raw[0].(map[string]any)["model"] != "model-slow" || raw[1].(map[string]any)["model"] != "model-fast" {
		t.Fatalf("line order should match the usage ranking: %+v", raw)
	}
	slow := raw[0].(map[string]any)["avgLatencyMs"].([]any)
	fast := raw[1].(map[string]any)["avgLatencyMs"].([]any)
	if len(slow) != 2 || slow[0] != float64(900) || slow[1] != nil {
		t.Fatalf("slow model has no calls on day two, must be null, not drawn as 0: %+v", slow)
	}
	if fast[0] != float64(200) || fast[1] != float64(300) {
		t.Fatalf("same model should average separately, not merge with the slow model: %+v", fast)
	}
	slowTtft := raw[0].(map[string]any)["avgTtftMs"].([]any)
	fastTtft := raw[1].(map[string]any)["avgTtftMs"].([]any)
	if len(slowTtft) != 2 || slowTtft[0] != float64(300) || slowTtft[1] != nil {
		t.Fatalf("first-token series must also bucket per model; missing buckets are null: %+v", slowTtft)
	}
	if fastTtft[0] != float64(40) || fastTtft[1] != float64(60) {
		t.Fatalf("first token must not merge different models into one value: %+v", fastTtft)
	}
	blended := body["buckets"].([]any)[0].(map[string]any)["avgLatencyMs"].(float64)
	if blended <= 400 || blended >= 500 {
		t.Fatalf("bucket overall average should still be the average of all calls, got %v", blended)
	}
	if code, body = statsQuery(t, "period=day&account=b&start=2026-10-13&end=2026-10-14"); code != http.StatusOK {
		t.Fatalf("status=%d", code)
	} else if rows, _ := body["modelLatency"].([]any); len(rows) != 1 || rows[0].(map[string]any)["model"] != "other-model" {
		t.Fatalf("account filter not applied to latency lines: %+v", rows)
	}
}

// The first-token series only accepts v2-metric, complete, successful calls; legacy metric, incomplete, and failed calls must not enter the bars.
func TestStatsModelTTFTSeriesIgnoresLegacyAndIncomplete(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	day := time.Date(2026, 10, 13, 10, 0, 0, 0, statsZone)
	row := func(metric string, complete bool, status int, ttft float64) statsRequest {
		r := statsTestRequest("a", "m", day, int64p(10), nil)
		r.LatencyMS = 100
		r.TTFTMS, r.TTFTMetric, r.Complete, r.Status = &ttft, metric, complete, status
		return r
	}
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Requests = []statsRequest{
		row(ttftMetric, true, 200, 100),
		row("", true, 200, 900),          // legacy metric, must not enter the first-token bars
		row(ttftMetric, false, 200, 900), // incomplete
		row(ttftMetric, true, 429, 900),  // failed
	}
	requestStats.Unlock()
	code, body := statsQuery(t, "period=day&account=a&start=2026-10-13&end=2026-10-13")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%v", code, body)
	}
	rows, _ := body["modelLatency"].([]any)
	if len(rows) != 1 {
		t.Fatalf("models=%+v", rows)
	}
	series, _ := rows[0].(map[string]any)["avgTtftMs"].([]any)
	if len(series) != 1 || series[0] != float64(100) {
		t.Fatalf("first-token bars must only accept v2, complete, successful samples: %+v", series)
	}
	latency, _ := rows[0].(map[string]any)["avgLatencyMs"].([]any)
	if len(latency) != 1 || latency[0] != float64(100) {
		t.Fatalf("total duration line should still average all calls in that bucket: %+v", latency)
	}
}

func TestStatsBucketStartBoundaries(t *testing.T) {
	at := time.Date(2026, 10, 14, 23, 59, 59, 0, statsZone)
	if got := statsBucketStart(at, "hour"); !got.Equal(time.Date(2026, 10, 14, 23, 0, 0, 0, statsZone)) {
		t.Fatalf("wrong hour bucket: %v", got)
	}
	if got := statsBucketStart(at, "day"); !got.Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, statsZone)) {
		t.Fatalf("wrong day bucket: %v", got)
	}
	// Sunday 00:00 must roll into this week's Monday, not next Monday.
	sunday := time.Date(2026, 10, 18, 0, 0, 0, 0, statsZone)
	if got := statsBucketStart(sunday, "week"); !got.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, statsZone)) {
		t.Fatalf("wrong week bucket: %v", got)
	}
	if got := statsBucketStart(at, "month"); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, statsZone)) {
		t.Fatalf("wrong month bucket: %v", got)
	}
}

// An account identity change (UID / enterprise ID / site) must change the stats ID so the old ledger is not inherited.
func TestStatsAccountIdentityFollowsCredentials(t *testing.T) {
	accountMu.Lock()
	defer accountMu.Unlock()
	base := &Account{Path: "workbuddy-a.json", Edition: "cn", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-1"}}}
	first, _ := statsAccountIDLocked(base)
	same := *base.Auth
	renewed := &Account{Path: base.Path, Edition: "cn", Auth: &StoredAuth{
		Edition: "cn", Account: same.Account, Auth: StoredTokens{AccessToken: "rotated"}}}
	if second, _ := statsAccountIDLocked(renewed); second != first {
		t.Fatal("token rotation must not change stats ownership")
	}
	// Express changes with freshly constructed accounts, avoiding copying the lock-holding Account value.
	for name, changed := range map[string]*Account{
		"account changed": {Path: base.Path, Edition: "cn", Auth: &StoredAuth{
			Edition: "cn", Account: StoredAccount{UID: "u2", EnterpriseID: "ent-1"}}},
		"enterprise changed": {Path: base.Path, Edition: "cn", Auth: &StoredAuth{
			Edition: "cn", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-2"}}},
		"site changed": {Path: base.Path, Edition: "intl", Auth: &StoredAuth{
			Edition: "intl", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-1"}}},
		"file changed": {Path: "workbuddy-b.json", Edition: "cn", Auth: &StoredAuth{
			Edition: "cn", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-1"}}},
	} {
		if id, _ := statsAccountIDLocked(changed); id == first {
			t.Fatalf("stats ID unchanged after %s", name)
		}
	}
}

// Persistence failure must degrade to memory-only without affecting later stats or queries.
func TestStatsWriteFailureDegradesToMemory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory read-only semantics are unstable on Windows; covered instead by symlink and bad-file cases")
	}
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	if err := os.Mkdir(statsFile, 0700); err != nil {
		t.Fatal(err)
	}
	requestStats.Lock()
	requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
		Account: "a", Model: "m", At: time.Now().UnixMilli(), Status: 200, LatencyMS: 1})
	requestStats.revision++
	requestStats.Unlock()
	if err := flushOptionalStats(); err == nil {
		t.Fatal("persistence should fail when the path is occupied by a directory")
	}
	requestStats.Lock()
	mode, problem := requestStats.mode, requestStats.problem
	requestStats.Unlock()
	if mode != "memory" || problem == "" {
		t.Fatalf("should degrade to memory-only with a plain-language reason: mode=%s problem=%s", mode, problem)
	}
	if code, _ := statsQuery(t, "period=day"); code != http.StatusOK {
		t.Fatalf("persistence failure must not affect queries: %d", code)
	}
}

// After a normal persist, reloading must yield the same records, and file permissions must not be loosened.
func TestStatsRoundTripPersists(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "workbuddy-a.json", Edition: "cn"}
	requestStats.document.Requests = append(requestStats.document.Requests,
		statsTestRequest("a", "glm-5.3-flash", time.Now(), int64p(42), float64p(0.5)))
	requestStats.revision++
	requestStats.Unlock()
	if err := flushOptionalStats(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statsFile)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("stats.json was not created: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("permissions should be 0600, got %o", perm)
		}
	}
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	defer requestStats.Unlock()
	if len(requestStats.document.Requests) != 1 || *requestStats.document.Requests[0].TotalTokens != 42 {
		t.Fatalf("reload lost records: %+v", requestStats.document.Requests)
	}
}

// The stats API is read-only; write methods must be rejected.
func TestStatsEndpointRejectsWrites(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	rec := httptest.NewRecorder()
	handleWebUIStats(rec, httptest.NewRequest(http.MethodPost, "/admin/api/stats", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("stats write should be rejected: %d", rec.Code)
	}
	for _, query := range []string{"period=year", "period=day&start=bad", "period=day&account=missing"} {
		if code, _ := statsQuery(t, query); code != http.StatusBadRequest {
			t.Fatalf("invalid query %s should return 400, got %d", query, code)
		}
	}
}

// Persistence is handled only by the background loop, and it must flush once when the context is cancelled.
func TestStatsPersistenceLoopFlushesOnCancel(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Requests = append(requestStats.document.Requests,
		statsTestRequest("a", "m", time.Now(), int64p(7), nil))
	requestStats.revision++
	requestStats.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); statsPersistenceLoop(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stats persistence loop did not exit on cancel")
	}
	if _, err := os.Stat(filepath.Clean(statsFile)); err != nil {
		t.Fatalf("stats must be flushed on cancel: %v", err)
	}
}
