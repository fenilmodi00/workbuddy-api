package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// A stream with ongoing data must not be interrupted even if its total
// duration exceeds the idle threshold (the old implementation was wrongly
// killed by the 180s total timeout).
func TestIdleReadCloserKeepsActiveStream(t *testing.T) {
	old := upstreamIdleTimeout
	upstreamIdleTimeout = 200 * time.Millisecond
	defer func() { upstreamIdleTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		// Total duration about 1s, far beyond the 200ms idle threshold, but
		// data keeps arriving every 50ms
		for i := 0; i < 20; i++ {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			f.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = newIdleReadCloser(resp.Body, upstreamIdleTimeout, cancel)
	defer resp.Body.Close()

	start := time.Now()
	total := 0
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("active stream must not be interrupted, got %v after %v", err, time.Since(start))
		}
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("stream finished too early, watchdog likely cut it: %v", time.Since(start))
	}
	if total == 0 {
		t.Fatal("no data read")
	}
}

// When the upstream stalls (no data beyond the idle threshold) it must be
// interrupted, not hang forever.
func TestIdleReadCloserInterruptsStalledStream(t *testing.T) {
	old := upstreamIdleTimeout
	upstreamIdleTimeout = 200 * time.Millisecond
	defer func() { upstreamIdleTimeout = old }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		f.Flush()
		<-release // stalls, sends no more data
	}))
	defer func() { close(release); srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = newIdleReadCloser(resp.Body, upstreamIdleTimeout, cancel)
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	// read the first block first
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read should succeed: %v", err)
	}
	start := time.Now()
	// subsequent reads must return an error near the idle threshold instead of
	// blocking forever
	for {
		_, err := resp.Body.Read(buf)
		if err != nil {
			elapsed := time.Since(start)
			if elapsed > 2*time.Second {
				t.Fatalf("stalled stream took too long to interrupt: %v", elapsed)
			}
			return
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("stalled stream was never interrupted")
		}
	}
}

// For 3MB+ large requests the upstream may take a long time to return
// response headers (measured: 2.3MB once needed 50.8s).
// Such requests must not be wrongly killed as 502 "timeout awaiting response headers".
func TestLargeSlowUpstreamHeaderIsNotKilled(t *testing.T) {
	oldHeader := upstreamHeaderTimeout
	oldIdle := upstreamIdleTimeout
	upstreamHeaderTimeout = 3 * time.Second
	upstreamIdleTimeout = 3 * time.Second
	defer func() {
		upstreamHeaderTimeout = oldHeader
		upstreamIdleTimeout = oldIdle
	}()

	// the upstream thinks for 1.2s before returning response headers, then
	// streams normally
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		f.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Base, profileCN.Origin = upstream.URL, upstream.URL
	cfg.HttpClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: upstreamHeaderTimeout}}
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "slow-header.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("slow but valid response headers must not be wrongly killed, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("normal stream should complete fully:\n%s", rec.Body.String())
	}
}

// The upstream section of config.json should override built-in timeout defaults.
func TestRuntimeConfigOverridesUpstreamTimeouts(t *testing.T) {
	oldHeader := upstreamHeaderTimeout
	oldIdle := upstreamIdleTimeout
	defer func() {
		upstreamHeaderTimeout = oldHeader
		upstreamIdleTimeout = oldIdle
	}()

	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"upstream":{"headerTimeoutSeconds":420,"idleTimeoutSeconds":75}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamHeaderTimeout != 420*time.Second {
		t.Fatalf("header timeout override failed: %v", upstreamHeaderTimeout)
	}
	if upstreamIdleTimeout != 75*time.Second {
		t.Fatalf("idle timeout override failed: %v", upstreamIdleTimeout)
	}

	// omitting the upstream section should restore defaults
	if err := os.WriteFile(path, []byte(`{"debug":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamHeaderTimeout != upstreamHeaderTimeoutDefault || upstreamIdleTimeout != upstreamIdleTimeoutDefault {
		t.Fatalf("omitted upstream section must restore defaults: header=%v idle=%v",
			upstreamHeaderTimeout, upstreamIdleTimeout)
	}
}
func TestInterruptedChatStreamDoesNotSendDone(t *testing.T) {
	oldTimeout := upstreamIdleTimeout
	upstreamIdleTimeout = 300 * time.Millisecond
	defer func() { upstreamIdleTimeout = oldTimeout }()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":""}]}`+"\n\n")
		f.Flush()
		<-release
	}))
	defer func() { close(release); upstream.Close() }()

	oldBase := profileCN.Base
	oldClient := cfg.HttpClient
	profileCN.Base = upstream.URL
	cfg.HttpClient = &http.Client{}
	defer func() {
		profileCN.Base = oldBase
		cfg.HttpClient = oldClient
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "idle-test.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("interrupted stream must not send [DONE]:\n%s", out)
	}
	if !strings.Contains(out, "upstream_stream_interrupted") {
		t.Fatalf("interrupted stream must report an explicit error event:\n%s", out)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("already-received partial content should be forwarded:\n%s", out)
	}
}

// useSingleUpstreamAccount points the domestic site to the test upstream and
// keeps a single account.
func useSingleUpstreamAccount(t *testing.T, upstreamURL string) {
	t.Helper()
	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Base, profileCN.Origin = upstreamURL, upstreamURL
	cfg.HttpClient = &http.Client{}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "eof-rule.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}
	accountMu.Unlock()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
}

// Clean EOF with only an empty finish_reason: [DONE] must not be added; the
// client must be told explicitly that the stream is incomplete.
func TestChatCleanEOFWithoutFinishReasonIsNotSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":""}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("[DONE] must not be forwarded without a real finish_reason:\n%s", out)
	}
	if !strings.Contains(out, "stream_closed_without_finish") {
		t.Fatalf("must report explicitly that the stream did not finish completely:\n%s", out)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("already received content should still be forwarded:\n%s", out)
	}
}

// With a real finish_reason, even if the upstream never sends [DONE], it is a
// normal completion and must not be treated as an interruption.
func TestChatFinishReasonWithoutDoneIsSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"length"}]}`+"\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if strings.Contains(out, "stream_closed_without_finish") || strings.Contains(out, "stream_interrupted") {
		t.Fatalf("finish_reason=length already present, must not be judged failed:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) || !strings.Contains(out, "done") {
		t.Fatalf("terminal chunk should be forwarded verbatim:\n%s", out)
	}
}

// Responses: on a clean end without finish_reason, only a small response.failed
// may be sent; the whole output must no longer be collapsed into
// output_item.done / response.completed.
func TestResponsesCleanEOFWithoutFinishReasonEmitsFailed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"loop"},"finish_reason":""}]}`+"\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleResponses)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_closed_without_finish") {
		t.Fatalf("response.failed should be emitted:\n%s", out)
	}
	if strings.Contains(out, "response.completed") || strings.Contains(out, "response.output_item.done") || strings.Contains(out, "data: [DONE]") {
		t.Fatalf("incomplete stream must not masquerade as completed:\n%s", out)
	}
	if !strings.Contains(out, "loop") {
		t.Fatalf("already emitted increments may be kept, but the closing must not replay the whole output:\n%s", out)
	}
}

// The terminal chunk may lack a delta. As long as finish_reason is non-empty,
// Responses should still complete normally.
func TestResponsesFinishReasonEmitsCompleted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":""}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleResponses)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "response.completed") || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("a stream with finish_reason should complete normally:\n%s", out)
	}
	if strings.Contains(out, "stream_closed_without_finish") {
		t.Fatalf("a stream with finish_reason must not be judged incomplete:\n%s", out)
	}
}

func TestAggregateCompletionWithoutFinishReasonIsNotSuccess(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":""}]}`,
		`data: [DONE]`,
	}, "\n")
	_, err := aggregateCompletion(strings.NewReader(sse), "m")
	if !errors.Is(err, errStreamClosedWithoutFinish) {
		t.Fatalf("aggregation without finish_reason should fail, got: %v", err)
	}
}
