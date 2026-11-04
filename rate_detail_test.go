package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetRateDetailCache(t *testing.T) {
	t.Helper()
	rateDetailCache.Lock()
	rateDetailCache.payload, rateDetailCache.at = nil, time.Time{}
	rateDetailCache.Unlock()
}

func TestParseRateLineCoversEveryLoggedSource(t *testing.T) {
	const prefix = "2026/10/13 10:59:36 "
	cases := []struct {
		name      string
		line      string
		site      string
		model     string
		value     string
		unknown   bool
		priority  int
		sourceHas string
	}{
		{
			name: "Official multiplier - promo expired",
			line: prefix + `[Models] official multiplier site=intl model=hy4-preview rawCredits="x0.00" effectiveMultiplier=0 hasMultiplier=true promo="Free now" promoEnds="2026-09-08T00:00:00+08:00" promoExpired=true source=live API refreshedAt=2026-10-13T10:59:36`,
			site: "intl", model: "hy4-preview", value: "-", unknown: true,
			priority: ratePriorityPromoStale, sourceHas: "Official catalog",
		},
		{
			name: "Official multiplier - normal value",
			line: prefix + `[Models] official multiplier site=cn model=hy3 rawCredits="x0.29" effectiveMultiplier=0.29 hasMultiplier=true promo="" promoEnds="" promoExpired=false source=live API refreshedAt=2026-10-13T10:59:36`,
			site: "cn", model: "hy3", value: "0.29x", unknown: false,
			priority: ratePriorityOfficial, sourceHas: "Official catalog",
		},
		{
			name: "Probe cached verdict - paid",
			line: prefix + `[ModelPrice] cached verdict site=intl model=hy4-preview verdict=paid credit=0.05 tokens=385 probeTime=2026-10-02T00:23:40 note=usage.credit>0 paid source=cache file (not a fresh probe this round)`,
			site: "intl", model: "hy4-preview", value: "Paid (unknown multiplier)", unknown: true,
			priority: ratePriorityProbePaid, sourceHas: "cached verdict",
		},
		{
			name: "Probe measurement - free",
			line: prefix + `[ModelPrice] site=cn account=workbuddy1.json model=hy3 -> verdict=free credit=0 tokens=420 (usage.credit=0 free)`,
			site: "cn", model: "hy3", value: "0.00x", unknown: false,
			priority: ratePriorityProbeFree, sourceHas: "local measurement",
		},
		{
			name: "Normal request measurement - paid",
			line: prefix + `[ModelCost] requestId=3472 account workbuddy2.json site=intl model hy4-preview live usage.credit=0.05, learned as paid model`,
			site: "intl", model: "hy4-preview", value: "Measured paid", unknown: false,
			priority: ratePriorityLearnedPaid, sourceHas: "Normal request measurement",
		},
		{
			name: "Normal request measurement - free",
			line: prefix + `[ModelCost] requestId=84 account workbuddy2.json site=cn model hy3 live usage.credit=0, learned as free model`,
			site: "cn", model: "hy3", value: "0.00x", unknown: false,
			priority: ratePriorityLearnedFree, sourceHas: "Normal request measurement",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record, ok := parseRateLine(tc.line, "gateway-2026-10-13.log")
			if !ok {
				t.Fatalf("Not parsed: %s", tc.line)
			}
			if record.Site != tc.site || record.Model != tc.model || record.Value != tc.value ||
				record.Unknown != tc.unknown || record.Priority != tc.priority {
				t.Fatalf("Parse result mismatch: %+v", record)
			}
			if !strings.Contains(record.Source, tc.sourceHas) {
				t.Fatalf("Source=%q, expected to contain %q", record.Source, tc.sourceHas)
			}
			if record.UpdatedAt != time.Date(2026, 10, 13, 10, 59, 36, 0, time.Local).Unix() {
				t.Fatalf("Time parse error: %d", record.UpdatedAt)
			}
			if record.LogLine == "" || record.LogFile != "gateway-2026-10-13.log" {
				t.Fatal("Missing log origin")
			}
			// The page uses this plain-language explanation instead of long log excerpts; it must exist and stay short.
			if record.Explain == "" {
				t.Fatal("Missing short user-facing explanation")
			}
			if len([]rune(record.Explain)) > 60 {
				t.Fatalf("Explanation too long to grasp at a glance: %q", record.Explain)
			}
		})
	}
	for _, line := range []string{
		"2026/10/13 10:59:36 [Models] Live catalog success: site=intl, modelCount=18, activePromos=0",
		"2026/10/13 10:59:36 [Request received] traceId=abc method=GET path=/v1/models",
		"[Models] official multiplier site=cn model=hy3",
	} {
		if _, ok := parseRateLine(line, "gateway-2026-10-13.log"); ok {
			t.Fatalf("Unrelated log line should not be parsed: %s", line)
		}
	}
}

func TestAggregateRateDetailsMatchesDisplayPriority(t *testing.T) {
	now := time.Now().Unix()
	records := []rateLogRecord{
		{Site: "intl", Model: "hy4-preview", Source: "Official catalog (live API)", UpdatedAt: now, Value: "-",
			Unknown: true, Priority: ratePriorityPromoStale, Detail: "Promo expired"},
		{Site: "intl", Model: "hy4-preview", Source: "Price probe (cached verdict)", UpdatedAt: now - 100,
			Value: "Paid (unknown multiplier)", Unknown: true, Priority: ratePriorityProbePaid, Detail: "credit=0.05"},
		{Site: "cn", Model: "hy3", Source: "Official catalog (live API)", UpdatedAt: now, Value: "0.29x", Priority: ratePriorityOfficial},
		{Site: "cn", Model: "hy3", Source: "Normal request measurement", UpdatedAt: now - 50, Value: "0.00x", Priority: ratePriorityLearnedFree},
		{Site: "intl", Model: "hy3", Source: "Official catalog (live API)", UpdatedAt: now, Value: "0.50x", Priority: ratePriorityOfficial},
		{Site: "intl", Model: "hy3", Source: "Normal request measurement", UpdatedAt: now, Value: "Measured paid", Priority: ratePriorityLearnedPaid},
	}
	rates := aggregateRateDetails(records)
	if len(rates) != 3 {
		t.Fatalf("Group count=%d", len(rates))
	}
	intl := rates["intl|hy4-preview"]
	if intl["source"] != "Price probe (cached verdict)" || intl["value"] != "Paid (unknown multiplier)" || intl["unknown"] != true {
		t.Fatalf("Measured verdict should take priority over stale promo: %+v", intl)
	}
	if list, ok := intl["records"].([]rateLogRecord); !ok || len(list) != 2 {
		t.Fatalf("All source records should be kept: %+v", intl["records"])
	}
	cn := rates["cn|hy3"]
	if cn["source"] != "Normal request measurement" || cn["value"] != "0.00x" {
		t.Fatalf("Free measurement should take priority: %+v", cn)
	}
	if cn["hasLog"] != true {
		t.Fatal("hasLog must be set when records exist")
	}
	if intlHy3 := rates["intl|hy3"]; intlHy3["source"] != "Official catalog (live API)" || intlHy3["value"] != "0.50x" {
		t.Fatalf("Measured paid is supporting evidence only and must not override the official multiplier: %+v", intlHy3)
	}
}

func TestRateDetailEndpointReadsLogsAndReportsMissingLog(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	handler := newWebUIHandler()
	path := filepath.Join(logDir, "gateway-2026-10-13.log")
	lines := strings.Join([]string{
		`2026/10/13 10:59:32 [ModelPrice] cached verdict site=intl model=hy4-preview verdict=paid credit=0.05 tokens=385 probeTime=2026-10-02T00:23:40 note=usage.credit>0 paid source=cache file (not a fresh probe this round)`,
		`2026/10/13 10:59:36 [Models] official multiplier site=cn model=hy3 rawCredits="x0.29" effectiveMultiplier=0.29 hasMultiplier=true promo="" promoEnds="" promoExpired=false source=live API refreshedAt=2026-10-13T10:59:36`,
		`2026/10/13 10:59:36 [Models] Live catalog success: site=intl, modelCount=18, activePromos=0`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "feature-api-key"} {
		if w := adminRequest(handler, "GET", "/admin/api/rate-detail", key, ""); w.Code != http.StatusUnauthorized {
			t.Fatal("Rate detail endpoint bypassed admin auth")
		}
	}
	if w := adminRequest(handler, "POST", "/admin/api/rate-detail", "feature-admin-key", "{}"); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("Rate detail endpoint should be read-only")
	}
	w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("Read failed: %d %s", w.Code, w.Body.String())
	}
	var payload struct {
		Rates       map[string]map[string]any `json:"rates"`
		RecordCount int                       `json:"recordCount"`
		Source      string                    `json:"source"`
		Note        string                    `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RecordCount != 2 {
		t.Fatalf("Only price-related log lines should be parsed: %+v", payload)
	}
	if payload.Rates["intl|hy4-preview"]["value"] != "Paid (unknown multiplier)" {
		t.Fatalf("Probe verdict not applied: %+v", payload.Rates)
	}
	if payload.Rates["cn|hy3"]["value"] != "0.29x" {
		t.Fatalf("Official multiplier not applied: %+v", payload.Rates)
	}
	if _, exists := payload.Rates["cn|not-in-log"]; exists {
		t.Fatal("Models without logs must not appear in results")
	}
	if !strings.Contains(payload.Note, "no log") {
		t.Fatal("Endpoint should explain the no-log display behavior")
	}
	var hints struct {
		RefreshHint     string `json:"refreshHint"`
		NoLogHint       string `json:"noLogHint"`
		AutoProbeKnown  bool   `json:"autoProbeKnown"`
		AutoProbeEnable bool   `json:"autoProbeEnabled"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &hints); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(hints.RefreshHint), "request this model once") || !strings.Contains(strings.ToLower(hints.NoLogHint), "request this model once") {
		t.Fatalf("Both unknown and no-log states must give an actionable refresh method: %+v", hints)
	}
	var detailed struct {
		Rates map[string]map[string]any `json:"rates"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &detailed)
	if unknown := detailed.Rates["intl|hy4-preview"]; unknown["refreshHint"] == nil || unknown["explain"] == "" {
		t.Fatalf("Unknown multiplier must include refresh hint and short explanation: %+v", unknown)
	}
	if known := detailed.Rates["cn|hy3"]; known["refreshHint"] != nil {
		t.Fatalf("Known multiplier should not show a refresh hint: %+v", known)
	}
	// After a cache hit no rescan happens; once the cache expires the new log must be read again.
	if err := os.WriteFile(path, []byte(`2026/10/13 11:30:00 [Models] official multiplier site=cn model=hy3 rawCredits="x0.50" effectiveMultiplier=0.50 hasMultiplier=true promo="" promoEnds="" promoExpired=false source=live API refreshedAt=2026-10-13T11:30:00`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", ""); !strings.Contains(w.Body.String(), "0.29x") {
		t.Fatal("Cache should be hit within a short window so each click does not rescan logs")
	}
	resetRateDetailCache(t)
	if w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", ""); !strings.Contains(w.Body.String(), "0.50x") {
		t.Fatal("Latest log must be read once the cache expires")
	}
}

// Production logs can be huge: the endpoint must return within the caller timeout and must not run the redaction regex on every line.
func TestRateDetailEndpointStaysFastOnLargeLogs(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	handler := newWebUIHandler()
	path := filepath.Join(logDir, "gateway-2026-10-13.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	unrelated := "2026/10/13 12:00:00 [Request received] traceId=synthetic method=POST path=/v1/chat/completions source=127.0.0.1:12345 note=Request entered the gateway authorization=Bearer synthetic-unrelated-token\n"
	for i := 0; i < 40000; i++ {
		if _, err := writer.WriteString(unrelated); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.WriteString(`2026/10/13 12:00:01 [Models] official multiplier site=cn model=hy3 rawCredits="x0.29" effectiveMultiplier=0.29 hasMultiplier=true promo="" promoEnds="" promoExpired=false source=live API refreshedAt=2026-10-13T12:00:01` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	size, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if size.Size() < 4<<20 {
		t.Fatalf("Test log too small, large-file scenario not covered: %d", size.Size())
	}
	start := time.Now()
	w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("Read failed on large log: %d", w.Code)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Rate detail endpoint too slow on large log (%v); deployment validation will time out", elapsed)
	}
	var payload struct {
		RecordCount int `json:"recordCount"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RecordCount != 1 {
		t.Fatalf("Only price-related lines should be parsed: %d", payload.RecordCount)
	}
	if strings.Contains(w.Body.String(), "synthetic-unrelated-token") {
		t.Fatal("Unrelated log body leaked into the response")
	}
	t.Logf("Log %d bytes, took %v", size.Size(), elapsed)
}

func TestAggregateRateDetailsDeduplicatesAndCapsOtherRecords(t *testing.T) {
	now := time.Now().Unix()
	var records []rateLogRecord
	// Simulate multiple restarts: the same verdict gets written to the log repeatedly.
	for i := 0; i < 6; i++ {
		records = append(records,
			rateLogRecord{Site: "intl", Model: "hy4-preview", Source: "Official catalog (live API)",
				Explain: "Official promo has ended but the API still returns 0.00x", Value: "-", Unknown: true,
				Priority: ratePriorityPromoStale, UpdatedAt: now - int64(i)},
			rateLogRecord{Site: "intl", Model: "hy4-preview", Source: "Price probe (cached verdict)",
				Explain: "Last real probe verdict: paid (not re-probed yet)", Value: "Paid (unknown multiplier)", Unknown: true,
				Priority: ratePriorityProbePaid, UpdatedAt: now - int64(i)},
		)
	}
	records = append(records, rateLogRecord{Site: "intl", Model: "hy4-preview", Source: "Normal request measurement",
		Explain: "Measured paid from a normal client request; supporting evidence only, does not change the official multiplier above", Value: "Measured paid",
		Priority: ratePriorityLearnedPaid, UpdatedAt: now})
	rates := aggregateRateDetails(records)
	entry := rates["intl|hy4-preview"]
	if entry["source"] != "Price probe (cached verdict)" {
		t.Fatalf("Wrong value priority: %+v", entry)
	}
	others, ok := entry["others"].([]rateLogRecord)
	if !ok {
		t.Fatalf("Wrong type for others")
	}
	if len(others) != 2 {
		t.Fatalf("Other records should dedupe by source+explanation to 2, got %d: %+v", len(others), others)
	}
	if others[0].UpdatedAt < others[1].UpdatedAt {
		t.Fatalf("Other records should keep the newer one first: %+v", others)
	}
	if list, ok := entry["records"].([]rateLogRecord); !ok || len(list) != 13 {
		t.Fatalf("Original records must be fully kept for collapsed viewing: %+v", entry["records"])
	}
	// When distinct explanations exceed the cap, only the newest few are kept.
	var many []rateLogRecord
	for i := 0; i < 8; i++ {
		many = append(many, rateLogRecord{Site: "cn", Model: "hy3", Source: "Official catalog (live API)",
			Explain: fmt.Sprintf("Explanation %d", i), Value: "0.29x", Priority: ratePriorityOfficial, UpdatedAt: now - int64(i)})
	}
	capped := aggregateRateDetails(many)["cn|hy3"]["others"].([]rateLogRecord)
	if len(capped) != rateMaxOtherRecords {
		t.Fatalf("Other records should be capped at %d: %d", rateMaxOtherRecords, len(capped))
	}
}

func TestRateRefreshHintFollowsLoggedProbeSwitch(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	handler := newWebUIHandler()
	path := filepath.Join(logDir, "gateway-2026-10-13.log")
	content := strings.Join([]string{
		`2026/10/13 10:59:32 [ModelPrice] traceId=synthetic stage=auto probe scheduling allowPriceProbes=true legacyDisablePriceProbes=false result=enabled reason=explicitly specified -allow-price-probes costWarning=may consume credits`,
		`2026/10/13 10:59:32 [ModelPrice] cached verdict site=intl model=hy4-preview verdict=paid credit=0.05 tokens=385 probeTime=2026-10-02T00:23:40 note=usage.credit>0 paid source=cache file (not a fresh probe this round)`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	var payload struct {
		RefreshHint      string `json:"refreshHint"`
		AutoProbeEnabled bool   `json:"autoProbeEnabled"`
		AutoProbeKnown   bool   `json:"autoProbeKnown"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.AutoProbeKnown || !payload.AutoProbeEnabled {
		t.Fatalf("Probe switch state should come from the normal log: %+v", payload)
	}
	if !strings.Contains(payload.RefreshHint, "automatically re-probe") {
		t.Fatalf("When probing is enabled the hint should mention automatic re-probing: %q", payload.RefreshHint)
	}
}

func TestRateLogLinesStayInNormalLogNotDebug(t *testing.T) {
	isolateAdminFeatures(t)
	var audit strings.Builder
	oldWriter := log.Writer()
	log.SetOutput(&audit)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	rateLogState.Lock()
	rateLogState.seen = map[string]string{}
	rateLogState.Unlock()
	if err := switchDebugLogging(false); err != nil {
		t.Fatal(err)
	}
	logOfficialRates("cn", []catalogModel{{ID: "hy3", Credits: "x0.29", HasMultiplier: true, Multiplier: 0.29, FromLive: true}}, time.Now())
	if !strings.Contains(audit.String(), "[Models] official multiplier site=cn model=hy3") {
		t.Fatal("Official multiplier must be written to the normal log")
	}
	if debugLoggingEnabled() {
		t.Fatal("Test should have debug logging disabled")
	}
	// The same signature is not logged again, avoiding hourly spam.
	before := strings.Count(audit.String(), "[Models] official multiplier site=cn model=hy3")
	logOfficialRates("cn", []catalogModel{{ID: "hy3", Credits: "x0.29", HasMultiplier: true, Multiplier: 0.29, FromLive: true}}, time.Now())
	if after := strings.Count(audit.String(), "[Models] official multiplier site=cn model=hy3"); after != before {
		t.Fatal("Unchanged multiplier must not be logged again")
	}
}
