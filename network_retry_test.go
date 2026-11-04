package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Transient network errors must be recognized (retryable); non-transient errors must not be misclassified.
func TestIsTransientNetworkError(t *testing.T) {
	// Addresses are only used in synthetic error text: documentation example ranges, no real client or upstream IPs.
	transient := []error{
		io.EOF,
		errors.New(`Post "https://x": write tcp 192.0.2.1:1->198.51.100.2:443: write: connection reset by peer`),
		errors.New("write: broken pipe"),
		errors.New("use of closed network connection"),
		errors.New("dial tcp 192.0.2.1:443: connect: connection refused"),
		errors.New(`http2: server sent GOAWAY and closed the connection`),
		errors.New("unexpected EOF"),
		// Windows Winsock RST messages; do not include the POSIX "connection reset by peer".
		errors.New(`Post "https://x": write tcp 192.0.2.1:1->198.51.100.2:443: wsasend: An existing connection was forcibly closed by the remote host.`),
		errors.New(`read tcp 192.0.2.1:1->198.51.100.2:443: wsarecv: An existing connection was forcibly closed by the remote host.`),
		errors.New(`write tcp 192.0.2.1:1->198.51.100.2:443: wsasend: An established connection was aborted by the software in your host machine.`),
	}
	for _, err := range transient {
		if !isTransientNetworkError(err) {
			t.Errorf("Should be classified as transient: %v", err)
		}
	}

	permanent := []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		errors.New("invalid character 'x' looking for beginning of value"),
		errors.New("json: cannot unmarshal number"),
	}
	for _, err := range permanent {
		if isTransientNetworkError(err) {
			t.Errorf("Should not be classified as transient: %v", err)
		}
	}
}

type retryTransportFunc func(*http.Request) (*http.Response, error)

func (f retryTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A failure during connection establishment, with request headers unwritten, is the only case retryable on a new connection.
func TestUpstreamRetriesTransientFailureAndSucceeds(t *testing.T) {
	var attempts int32
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	oldBackoff := upstreamRetryBackoff
	defer func() {
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
		upstreamRetryBackoff = oldBackoff
	}()
	upstreamTransientRetries = 2
	upstreamRetryBackoff = time.Millisecond
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			return nil, io.EOF // connection establishment failed; neither request headers nor body were sent
		}
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != `{"model":"m"}` {
			t.Errorf("request body on attempt %d: %q, %v", n, body, err)
		}
		if !req.Close {
			t.Error("retry must request a new connection")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	resp, cancel, err := doUpstreamRequest(req, acc, &profileCN, []byte(`{"model":"m"}`), 1, "trace-test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected one safe retry, got %d attempts", got)
	}
}

// The upstream disconnected after reading the request headers; it may have processed the POST, so it must not be replayed.
func TestUpstreamDoesNotRetryEOFOnceHeadersSent(t *testing.T) {
	var attempts int32
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	defer func() { cfg.HttpClient, upstreamTransientRetries = oldClient, oldRetries }()
	upstreamTransientRetries = 2
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
			trace.WroteHeaders()
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return nil, io.EOF
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	_, _, err := doUpstreamRequest(req, acc, &profileCN, []byte(`{"model":"m"}`), 1, "trace-test")
	if !errors.Is(err, io.EOF) || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("already sent POST must not retry: attempts=%d err=%v", attempts, err)
	}
}

// Even a bare EOF must not trigger regeneration if the request body was fully written and the server has not responded.
func TestUpstreamDoesNotRetryEOFOnceBodySent(t *testing.T) {
	var attempts int32
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	defer func() { cfg.HttpClient, upstreamTransientRetries = oldClient, oldRetries }()
	upstreamTransientRetries = 2
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return nil, io.EOF
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	_, _, err := doUpstreamRequest(req, acc, &profileCN, []byte(`{"model":"m"}`), 1, "trace-test")
	if !errors.Is(err, io.EOF) || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("fully sent POST must not retry: attempts=%d err=%v", attempts, err)
	}
}

// A real TCP RST after the server received the request must likewise not be replayed automatically.
func TestUpstreamDoesNotRetryAfterServerReceivesRequest(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			// Simulate a peer RST: hijack the underlying connection and close it directly
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0) // immediate RST
					}
					_ = conn.Close()
					return
				}
			}
			panic("hijack unsupported")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	oldBackoff := upstreamRetryBackoff
	profileCN.Base, profileCN.Origin = srv.URL, srv.URL
	cfg.HttpClient = &http.Client{}
	upstreamTransientRetries = 2
	upstreamRetryBackoff = 10 * time.Millisecond
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
		upstreamRetryBackoff = oldBackoff
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "retry.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
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

	if got := atomic.LoadInt32(&attempts); got != 1 || rec.Code != http.StatusBadGateway {
		t.Fatalf("request reached server, must not retry: attempts=%d status=%d", got, rec.Code)
	}
}

// When a real HTTP server closes the connection right after receiving the full POST, the client still sees EOF but must not replay.
func TestUpstreamDoesNotRetryRealEOF(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read request body: %v", err)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	oldClient, oldRetries := cfg.HttpClient, upstreamTransientRetries
	defer func() { cfg.HttpClient, upstreamTransientRetries = oldClient, oldRetries }()
	cfg.HttpClient = srv.Client()
	upstreamTransientRetries = 2
	prof := profileCN
	prof.Base = srv.URL
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	_, _, err := doUpstreamRequest(req, acc, &prof, []byte(`{"model":"m"}`), 1, "trace-test")
	if !errors.Is(err, io.EOF) || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("upstream received POST, must not retry: attempts=%d err=%v", attempts, err)
	}
}

// When retries are exhausted and the request still fails, honestly return 502 and never fabricate success.
func TestUpstreamRetriesExhaustedReturns502(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				_ = conn.Close()
				return
			}
		}
		panic("hijack unsupported")
	}))
	defer srv.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	oldBackoff := upstreamRetryBackoff
	profileCN.Base, profileCN.Origin = srv.URL, srv.URL
	cfg.HttpClient = &http.Client{}
	upstreamTransientRetries = 2
	upstreamRetryBackoff = 10 * time.Millisecond
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
		upstreamRetryBackoff = oldBackoff
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "fail.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
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

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("Retries exhausted should return 502, got %d", rec.Code)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("RST after request headers must not retry, attempts=%d", got)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatal("Must not fabricate [DONE] on failure")
	}
}

// Non-transient errors (e.g. upstream 400) must not trigger retries, to avoid amplifying the problem.
func TestUpstreamDoesNotRetryPermanentError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":11128,"msg":"bad"}`)
	}))
	defer srv.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	profileCN.Base, profileCN.Origin = srv.URL, srv.URL
	cfg.HttpClient = &http.Client{}
	upstreamTransientRetries = 2
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "p.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
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

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("HTTP 400 should not be retried, actual request count=%d", got)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Upstream 400 should be passed through, got %d", rec.Code)
	}
}

// config.json should be able to override the retry count.
func TestRuntimeConfigOverridesTransientRetries(t *testing.T) {
	old := upstreamTransientRetries
	defer func() { upstreamTransientRetries = old }()

	dir := t.TempDir()
	path := dir + "/config.json"
	if err := writeFile(path, `{"upstream":{"transientRetries":5}}`); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamTransientRetries != 5 {
		t.Fatalf("Retry count override failed: %d", upstreamTransientRetries)
	}

	// Explicitly setting 0 should disable retries (not fall back to the default)
	if err := writeFile(path, `{"upstream":{"transientRetries":0}}`); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamTransientRetries != 0 {
		t.Fatalf("Explicit 0 should disable retries, got %d", upstreamTransientRetries)
	}

	// Omitting the field should return to the default value
	if err := writeFile(path, `{"debug":{"enabled":false}}`); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamTransientRetries != upstreamTransientRetriesDefault {
		t.Fatalf("Omitted config should fall back to the default: %d", upstreamTransientRetries)
	}
}

// Helper: write file
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}
