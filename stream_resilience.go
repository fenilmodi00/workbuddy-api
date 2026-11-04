package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// streamWriteGuard serializes heartbeat and data writes to the downstream
// ResponseWriter. net/http does not guarantee ResponseWriter concurrency
// safety, so the heartbeat goroutine and the main stream reader must share
// the same lock.
type streamWriteGuard struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	flusher  http.Flusher
	dataSeen bool
}

func (g *streamWriteGuard) writeData(payload string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, _ = fmt.Fprintf(g.w, "data: %s\n\n", payload)
	g.flusher.Flush()
	g.dataSeen = true
}

// writeUpstreamComment forwards an existing upstream SSE comment without
// counting the comment as valid data.
func (g *streamWriteGuard) writeUpstreamComment(line string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, _ = fmt.Fprintf(g.w, "%s\n\n", line)
	g.flusher.Flush()
}

func (g *streamWriteGuard) writeHeartbeat() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dataSeen {
		return
	}
	_, _ = fmt.Fprint(g.w, ": keepalive\n\n")
	g.flusher.Flush()
}

func (g *streamWriteGuard) hasData() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dataSeen
}

type streamHeartbeat struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// startStreamHeartbeat periodically emits SSE comments until the first valid
// data frame; Stop waits for the goroutine to exit so the ResponseWriter is
// not written after the handler returns.
func startStreamHeartbeat(g *streamWriteGuard, interval time.Duration) *streamHeartbeat {
	if interval <= 0 {
		return nil
	}
	h := &streamHeartbeat{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-ticker.C:
				g.writeHeartbeat()
			}
		}
	}()
	return h
}

func (h *streamHeartbeat) Stop() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.stop) })
	<-h.done
}

// canReplayStream only allows a replay when the client has received no valid
// data, the budget is not exhausted, and the error is a transient network
// failure or an upstream idle timeout.
// clientCanceled is decided by the caller from r.Context().Err(); the internal
// idle reader timeout also surfaces as context.Canceled, so it must be
// distinguished from client cancellation.
func canReplayStream(scanErr error, sawFirstData, budgetLeft, clientCanceled bool) bool {
	if scanErr == nil || sawFirstData || !budgetLeft || clientCanceled {
		return false
	}
	if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
		return true
	}
	return isTransientNetworkError(scanErr)
}

// replayResponseWriter captures the error response of upstreamChat during a
// replay, avoiding a second WriteHeader after the heartbeat already sent the
// 200 response headers, or JSON errors leaking into the SSE stream.
type replayResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newReplayResponseWriter(source http.Header) *replayResponseWriter {
	return &replayResponseWriter{header: source.Clone()}
}

func (w *replayResponseWriter) Header() http.Header { return w.header }

func (w *replayResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *replayResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func replayUpstreamChat(w http.ResponseWriter, r *http.Request, reqID uint64, modelName string, upstreamBytes []byte, startTime time.Time, guard *streamWriteGuard) (*http.Response, *Account, *upstreamProfile, bool) {
	captured := newReplayResponseWriter(w.Header())
	resp, acc, prof, ok := upstreamChat(captured, r, reqID, modelName, upstreamBytes, startTime)
	if ok {
		return resp, acc, prof, true
	}
	payload := bytes.TrimSpace(captured.body.Bytes())
	if len(payload) == 0 || !json.Valid(payload) {
		status := captured.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		payload, _ = json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "Replay before first byte failed; this response is incomplete",
				"type":    "upstream_stream_interrupted",
				"code":    "stream_replay_failed",
				"status":  status,
			},
		})
	}
	guard.writeData(string(payload))
	log.Printf("[Exception] traceId=%s requestId=%d stage=replay before first byte model=%s result=replay did not obtain a usable upstream response businessImpact=client notified via SSE error event, no fabricated [DONE]",
		debugTraceID(r), reqID, modelName)
	debugEvent(r, "error", "stream_first_byte_replay_failed", map[string]any{
		"status_code":     http.StatusBadGateway,
		"business_impact": "client already started receiving SSE, HTTP status can no longer be rewritten; switched to in-stream error",
	})
	return nil, nil, nil, false
}
