package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Repeatable browser acceptance fixture: enables only local admin HTTP; no login, no upstream requests.
// WORKBUDDY_BROWSER_FIXTURE=1 go test -run TestPolicyBrowserFixture -v
func TestPolicyBrowserFixture(t *testing.T) {
	if os.Getenv("WORKBUDDY_BROWSER_FIXTURE") != "1" {
		t.Skip("For explicit browser acceptance only")
	}
	isolatePolicies(t)
	cfg.Port, cfg.WebPort = 18317, 18316
	cfg.ModelsRefresh, cfg.ReloadInterval, cfg.KeepaliveHours = 60, 5, []int{22}
	cfg.DisablePriceProbes = true
	writeRuntimeFixture(t, `{"gateway":{"apiPort":18317,"webPort":18316,"adminKey":"visual-test-admin"},"systemPrompt":{"force":"Browser acceptance: the systemPrompt body is displayed here directly."},"models":{"allowlist":["hy3"],"accounts":{"all":{"allowlist":["workbuddy-cn-a.json"],"blocklist":[]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	catalogModels = map[string][]catalogModel{"cn": {
		{ID: "hy3", HasMultiplier: true, Multiplier: 1},
		{ID: "deepseek-v4.1-flash", HasMultiplier: true, Multiplier: 0},
		{ID: "claude-sonnet-4.5", HasMultiplier: true, Multiplier: 2},
	}}
	accounts = []*Account{
		{Path: "workbuddy-cn-a.json", Auth: &StoredAuth{Account: StoredAccount{Nickname: "Domestic test account"}, Auth: StoredTokens{AccessToken: "synthetic-not-a-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, Edition: "cn", QuotaKnown: true, QuotaTotal: 2000, QuotaRemaining: 1800},
		{Path: "workbuddy-intl-b.json", Auth: &StoredAuth{Edition: "intl", Account: StoredAccount{Nickname: "International test account"}, Auth: StoredTokens{AccessToken: "synthetic-not-a-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, Edition: "intl", QuotaKnown: true, QuotaTotal: 1000, QuotaRemaining: 500},
	}
	for _, account := range accounts {
		raw, err := json.MarshalIndent(account.Auth, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(account.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldDay := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	_ = os.WriteFile(filepath.Join(logDir, "gateway-"+oldDay+".log"), []byte("synthetic historical log\n"), 0600)
	writeStatusSnapshot()
	now := time.Now()
	seedStatsFixture(t, now)
	for _, fixture := range []struct {
		id, status, detail string
		next               time.Time
	}{
		{"quota_scan", "partial", "Synthetic test: one account query timed out", now.Add(5 * time.Minute)},
		{"token_check", "success", "Synthetic test: both accounts checked fine", now.Add(5 * time.Minute)},
		{"status_snapshot", "success", "Synthetic test: snapshot written", now.Add(3 * time.Second)},
		{"model_catalog", "success", "Synthetic test: both site catalogs synced", now.Add(time.Hour)},
		{"daily_checkin", "success", "Synthetic test: checkin succeeded", nextDailyCheckin(now)},
		{"keepalive", "success", "Synthetic test: renewal succeeded", nextKeepaliveTime(now, cfg.KeepaliveHours)},
		{"keepalive_catchup", "skipped", "Synthetic test: renewed recently; no catch-up", time.Time{}},
	} {
		logScheduledTask(fixture.id, "completed", fixture.status, newTraceID(), now.Add(-time.Minute), fixture.next, fixture.detail)
	}
	logScheduledTask("price_probe", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "Synthetic test: auto-probing disabled")
	// Synthetic rate-source logs: used to verify the "exclamation mark" display with logs present/unknown/no logs.
	log.Printf("[Models] official multiplier site=cn model=hy3 rawCredits=\"x0.29\" effectiveMultiplier=0.29 hasMultiplier=true promo=\"\" promoEnds=\"\" promoExpired=false source=live API refreshedAt=%s",
		now.Format("2006-01-02T15:04:05"))
	log.Printf("[Models] official multiplier site=intl model=deepseek-v4.1-flash rawCredits=\"x0.00\" effectiveMultiplier=0.00 hasMultiplier=true promo=\"Free now\" promoEnds=\"2026-09-08T00:00:00+08:00\" promoExpired=true source=live API refreshedAt=%s",
		now.Format("2006-01-02T15:04:05"))
	log.Printf("[ModelPrice] cached verdict site=intl model=deepseek-v4.1-flash verdict=paid credit=0.05 tokens=385 probeTime=2026-10-02T00:23:40 note=usage.credit>0 paid source=cache file (not a fresh probe this round)")
	log.Printf("[ModelPrice] traceId=%s stage=auto probe scheduling allowPriceProbes=false legacyDisablePriceProbes=true result=disabled reason=legacy flag -disable-price-probes=true businessImpact=models are not requested automatically", newTraceID())
	log.Printf("[ModelCost] requestId=3472 account workbuddy-cn-a.json site=cn model hy3 live usage.credit=0.29, learned as paid model")
	listener, err := net.Listen("tcp", "127.0.0.1:18316")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: newWebUIHandler(), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer func() {
		cancel()
		_ = server.Close()
		workers.Wait()
	}()
	workers.Add(2)
	go func() {
		defer workers.Done()
		runtimeConfigLoop(ctx)
	}()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeStatusSnapshot()
			}
		}
	}()
	go server.Serve(listener)
	t.Log("Browser acceptance address http://127.0.0.1:18316/ui/, synthetic admin Key visual-test-admin; the fixture does not access upstream")
	duration := 180 * time.Second
	if raw := os.Getenv("WORKBUDDY_BROWSER_DURATION"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(duration)
}

// seedStatsFixture builds a repeatable set of synthetic usage data for the browser
// acceptance overview page's summary cards, line chart, table, and account filter.
// It deliberately leaves empty time buckets and requests missing usage to verify
// "unknown is not drawn as 0" and the coverage count display; no upstream access,
// no real stats.json writes.
func seedStatsFixture(t *testing.T, now time.Time) {
	t.Helper()
	accountMu.Lock()
	ids := map[string]string{}
	views := map[string]statsAccount{}
	for _, acc := range accounts {
		if id, view := statsAccountIDLocked(acc); id != "" {
			ids[acc.Path], views[id] = id, view
		}
	}
	accountMu.Unlock()
	if len(ids) == 0 {
		t.Fatal("stats fixture requires an account pool")
	}
	requestStats.Lock()
	previous := requestStats.document
	previousEnabled := requestStats.enabled
	requestStats.enabled = true
	requestStats.mode = "file"
	requestStats.document = statsDocument{Schema: statsSchema, StartedAt: now.AddDate(0, 0, -21).UnixMilli(),
		Accounts: views, Requests: []statsRequest{}}
	credit := 0.35
	for day := 21; day >= 0; day-- {
		at := now.AddDate(0, 0, -day)
		// Deliberately leaves two empty days, verifying the line breaks where there are no samples.
		if day == 14 || day == 7 {
			continue
		}
		for hour := 9; hour <= 17; hour += 4 {
			when := time.Date(at.Year(), at.Month(), at.Day(), hour, 15, 0, 0, time.Local)
			tokens := int64(180000 + (21-day)*24000 + hour*1500)
			latency := float64(2400 + (21-day)*60 + hour*40)
			ttft := float64(520 + (21-day)*8)
			row := statsRequest{Account: ids["workbuddy-cn-a.json"], Model: "deepseek-v4.1-flash",
				At: when.UnixMilli(), Status: 200, Complete: true, LatencyMS: latency, TTFTMS: &ttft, TTFTMetric: ttftMetric,
				TotalTokens: &tokens, InputTokens: int64p(tokens * 3 / 4), OutputTokens: int64p(tokens / 4),
				CachedTokens: int64p(tokens / 3), Credit: &credit}
			requestStats.document.Requests = append(requestStats.document.Requests, row)
			if id := ids["workbuddy-intl-b.json"]; id != "" && hour == 13 {
				small := int64(24000 + (21-day)*900)
				intlCredit := credit / 2
				requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
					Account: id, Model: "hy3", At: when.UnixMilli(), Status: 200, Complete: true,
					LatencyMS: latency * 0.6, TTFTMS: &ttft, TTFTMetric: ttftMetric, TotalTokens: &small,
					InputTokens: int64p(small * 4 / 5), OutputTokens: int64p(small / 5), Credit: &intlCredit})
				// One record missing usage: must display as unknown, not zero consumption.
				requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
					Account: id, Model: "hy3", At: when.Add(time.Minute).UnixMilli(), Status: 200,
					Complete: true, LatencyMS: latency * 0.7})
			}
		}
	}
	requestStats.Unlock()
	t.Cleanup(func() {
		requestStats.Lock()
		requestStats.document, requestStats.enabled = previous, previousEnabled
		requestStats.Unlock()
	})
}
