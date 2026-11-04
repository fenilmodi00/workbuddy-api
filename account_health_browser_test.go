package main

import (
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// Synthetic data, isolated working directory; reads no production credentials, accesses no upstream.
func TestHealthBrowserFixture(t *testing.T) {
	if os.Getenv("WORKBUDDY_HEALTH_BROWSER_FIXTURE") != "1" {
		t.Skip("only for explicit health browser acceptance")
	}
	isolatePolicies(t)
	resetStatsState(t)
	writeRuntimeFixture(t, `{"gateway":{"apiPort":18327,"webPort":18326,"adminKey":"health-visual-admin"},"models":{"accounts":{"all":{"blocklist":["paused.json"]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	auth := func(exp int64) *StoredAuth {
		return &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic-health-not-a-token", ExpiresAt: exp}, Account: StoredAccount{UID: "fixture-user"}}
	}
	now := time.Now()
	accounts = []*Account{
		{Path: "workbuddy-cn-a.json", Auth: auth(now.Add(time.Hour).Unix())},
		{Path: "cooling.json", Auth: auth(now.Add(time.Hour).Unix()), CooldownUntil: now.Add(15 * time.Minute)},
		{Path: "empty-credit.json", Auth: auth(now.Add(time.Hour).Unix()), QuotaKnown: true, QuotaExhausted: true},
		{Path: "expired.json", Auth: auth(now.Add(-time.Hour).Unix())},
		{Path: "invalid.json", Disabled: true, UID: "fixture-user", Edition: "cn"},
		{Path: "paused.json", Auth: auth(now.Add(time.Hour).Unix())},
	}
	seedStatsFixture(t, now)
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}
	accountAlertCache.Lock()
	accountAlertCache.at = time.Time{}
	accountAlertCache.Unlock()
	t.Cleanup(func() { accountAlertCache.Lock(); accountAlertCache.at = time.Time{}; accountAlertCache.Unlock() })
	listener, err := net.Listen("tcp", "127.0.0.1:18326")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: newWebUIHandler(), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go server.Serve(listener)
	duration := 180 * time.Second
	if raw := os.Getenv("WORKBUDDY_BROWSER_DURATION"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			duration = parsed
		}
	}
	t.Log("Synthetic page http://127.0.0.1:18326/ui/; Key health-visual-admin; five anomaly kinds, no production access")
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if err := writeStatusSnapshot(); err != nil {
			t.Fatal(err)
		}
	}
}
