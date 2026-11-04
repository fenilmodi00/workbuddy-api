package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// This file pins a silent failure: the upstream sends finish_reason:"" on
// **every** chunk, while the Anthropic protocol translation layer (the Claude
// Code path) picks "the first non-null finish_reason" in the stream as the
// final stop_reason. The empty string in the first chunk therefore locks
// stop_reason to end_turn, and the real terminal chunk's "tool_calls" is no
// longer adopted — Claude Code only executes tools when stop_reason=tool_use,
// so tools never run and the terminal shows nothing.
//
// Replay of a fixed chunk sequence (translated via cc-switch), observed:
//   any chunk carrying ""                -> stop_reason=end_turn (tool dropped)
//   all null + terminal "tool_calls"     -> stop_reason=tool_use (tool runs)
// -----------------------------------------------------------------------------

// Empty finish_reason in intermediate chunks must be normalized to null; the
// terminal chunk's real reason must be preserved.
func TestCleanChunkJSONNormalizesEmptyFinishReason(t *testing.T) {
	intermediate := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"","logprobs":null}]}`
	cleaned := cleanChunkJSON(intermediate)
	if strings.Contains(cleaned, `"finish_reason":""`) {
		t.Fatalf("empty finish_reason must be normalized to null, otherwise the translation layer locks stop_reason to end_turn: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"finish_reason":null`) {
		t.Fatalf("empty finish_reason must be normalized to null: %s", cleaned)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(cleaned), &obj); err != nil {
		t.Fatalf("cleaned output should be valid JSON: %v", err)
	}
	if fr := obj["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != nil {
		t.Fatalf("finish_reason should be null, got %#v", fr)
	}

	terminal := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls","logprobs":null}]}`
	cleanedTerm := cleanChunkJSON(terminal)
	if !strings.Contains(cleanedTerm, `"finish_reason":"tool_calls"`) {
		t.Fatalf("terminal chunk finish_reason must be preserved verbatim: %s", cleanedTerm)
	}

	// "stop" must also be preserved and not be mistakenly rewritten
	stopTerm := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	if c := cleanChunkJSON(stopTerm); !strings.Contains(c, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason=stop must be preserved: %s", c)
	}
}

// Legacy function_call stubs (both name/arguments empty) should be removed;
// real legacy calls must be preserved.
func TestCleanChunkJSONRemovesLegacyFunctionCallStub(t *testing.T) {
	stub := `{"id":"c1","choices":[{"index":0,"delta":{"function_call":{"arguments":"","name":""},"role":"assistant"},"finish_reason":"tool_calls"}]}`
	cleaned := cleanChunkJSON(stub)
	if strings.Contains(cleaned, "function_call") {
		t.Fatalf("stub function_call should be removed: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"finish_reason":"tool_calls"`) {
		t.Fatalf("removing the stub should not affect finish_reason: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"role":"assistant"`) {
		t.Fatalf("role is a valid field and should not be deleted: %s", cleaned)
	}

	real := `{"id":"c1","choices":[{"index":0,"delta":{"function_call":{"name":"get_weather","arguments":"{}"}},"finish_reason":"function_call"}]}`
	cleanedReal := cleanChunkJSON(real)
	if !strings.Contains(cleanedReal, `"name":"get_weather"`) {
		t.Fatalf("real legacy function call should not be removed: %s", cleanedReal)
	}
}

// Regression on existing behavior: empty-valued delta fields are still
// removed, and non-JSON input is returned verbatim.
func TestCleanChunkJSONRemovesEmptyDeltaFields(t *testing.T) {
	cleaned := cleanChunkJSON(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"thinking"},"finish_reason":""}]}`)
	if strings.Contains(cleaned, `"content"`) {
		t.Fatalf("empty delta values should be removed: %s", cleaned)
	}
	if !strings.Contains(cleaned, "thinking") {
		t.Fatalf("non-empty fields should not be removed: %s", cleaned)
	}
	if got := cleanChunkJSON("event: ping"); got != "event: ping" {
		t.Fatalf("non-JSON line should be returned verbatim, got: %s", got)
	}
}

func TestCleanChunkJSONPreservesToolDeltas(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"empty_metadata", `{"index":0,"id":"","type":"","function":{"name":"","arguments":""}}`, `{"index":0,"function":{"arguments":""}}`},
		{"argument_fragment", `{"index":1,"function":{"name":"","arguments":"{\"city\":"}}`, `{"index":1,"function":{"arguments":"{\"city\":"}}`},
		{"valid_metadata", `{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}"}}`, `{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}"}}`},
		{"extension_fields", `{"index":0,"extension":"","function":{"name":"","arguments":" ","extension":0}}`, `{"index":0,"extension":"","function":{"arguments":" ","extension":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"choices":[{"index":0,"delta":{"tool_calls":[` + tc.input + `]},"finish_reason":null}]}`
			got := decodeObject(t, []byte(cleanChunkJSON(input)))
			delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			call := delta["tool_calls"].([]any)[0]
			actual, err := json.Marshal(call)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(decodeObject(t, []byte(tc.want)))
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != string(want) {
				t.Fatalf("tool delta = %s, want %s", actual, want)
			}
		})
	}
}

func TestStreamChatToolAndReasoningRoundTrip(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"Checking ","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"two cities.\n","tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"","type":"","function":{"name":"","arguments":"\"Shanghai\"}"}},{"index":0,"id":"","type":"","function":{"name":"","arguments":"\"Beijing\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	code, out := runStreamCase(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	})
	if code != http.StatusOK {
		t.Fatalf("stream status = %d: %s", code, out)
	}
	if strings.Contains(out, `"name":""`) || strings.Contains(out, `"id":""`) || strings.Contains(out, `"type":""`) {
		t.Fatalf("empty metadata would overwrite client tool state: %s", out)
	}
	completion, err := aggregateCompletion(strings.NewReader(out), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	choice := decodeObject(t, completion)["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("tool completion = %#v", choice)
	}
	assistant := choice["message"].(map[string]any)
	trace := "Checking two cities.\n"
	if assistant["reasoning_content"] != trace {
		t.Fatalf("stream changed reasoning: %#v", assistant)
	}
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("tool calls = %#v", calls)
	}
	for i, want := range []struct{ id, arguments string }{{"call_a", `{"city":"Beijing"}`}, {"call_b", `{"city":"Shanghai"}`}} {
		call := calls[i].(map[string]any)
		function := call["function"].(map[string]any)
		if call["id"] != want.id || call["type"] != "function" || function["name"] != "lookup" || function["arguments"] != want.arguments {
			t.Fatalf("tool %d = %#v", i, call)
		}
	}
	body, err := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash", "stream": true, "reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "system", "content": "Use tools to answer."},
			map[string]any{"role": "user", "content": "Query two cities"},
			assistant,
			map[string]any{"role": "tool", "tool_call_id": "call_a", "content": "Sunny"},
			map[string]any{"role": "tool", "tool_call_id": "call_b", "content": "Cloudy"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed := captureUpstreamMessages(t, "/v1/chat/completions", string(body))
	if len(replayed) != 5 {
		t.Fatalf("replayed messages = %#v", replayed)
	}
	history := replayed[2].(map[string]any)
	if history["reasoning_content"] != trace || history["reasoning"] != trace {
		t.Fatalf("replay changed reasoning: %#v", history)
	}
	wantCalls, _ := json.Marshal(calls)
	gotCalls, _ := json.Marshal(history["tool_calls"])
	if string(gotCalls) != string(wantCalls) {
		t.Fatalf("replay changed tool calls: got %s, want %s", gotCalls, wantCalls)
	}
	for i, want := range []struct{ id, content string }{{"call_a", "Sunny"}, {"call_b", "Cloudy"}} {
		result := replayed[i+3].(map[string]any)
		if result["role"] != "tool" || result["tool_call_id"] != want.id || result["content"] != want.content {
			t.Fatalf("tool result %d changed: %#v", i, result)
		}
	}
}

// runStreamCase drives the real handleChatCompletions path with a fake account
// and fake upstream, returning the status code and SSE bytes the client
// ultimately receives.
func runStreamCase(t *testing.T, upstream http.HandlerFunc) (int, string) {
	t.Helper()

	srv := httptest.NewServer(upstream)
	defer srv.Close()

	oldBase := profileCN.Base
	oldClient := cfg.HttpClient
	oldVerbose := cfg.Verbose
	profileCN.Base = srv.URL
	cfg.HttpClient = &http.Client{Timeout: 30 * time.Second}
	cfg.Verbose = false
	defer func() {
		profileCN.Base = oldBase
		cfg.HttpClient = oldClient
		cfg.Verbose = oldVerbose
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "stream-test.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "test", RefreshToken: "rt", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()},
			Account: StoredAccount{UID: "u1", Nickname: "tester"},
		},
	}}
	rrIndex = 0
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"hy3-preview","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// End to end: the upstream sends a tool-call stream where every chunk carries
// finish_reason:""; no empty string may remain in the chunks the client
// receives, and the terminal chunk must be "tool_calls".
func TestStreamChatResponseKeepsToolCallFinishReason(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"I will check the git status."},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": \"git status\"}"}}]},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"","name":""},"role":"assistant"},"finish_reason":"tool_calls","logprobs":null}],"usage":{"prompt_tokens":9,"completion_tokens":6}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	code, out := runStreamCase(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	})
	t.Logf("status=%d client received:\n%s", code, out)

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if strings.Contains(out, `"finish_reason":""`) {
		t.Fatalf("client must not receive an empty finish_reason (it would lock stop_reason to end_turn): %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("terminal chunk must keep finish_reason=tool_calls: %s", out)
	}
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("tool call must be passed through: %s", out)
	}
	if strings.Contains(out, "function_call") {
		t.Fatalf("legacy function_call stub must be removed: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing [DONE] terminator")
	}
}

// The official upstream's first frame is an SSE comment; CPA must see the
// comment rather than data: : heartbeat.
// Also ensure event/id/retry metadata never becomes a non-JSON data frame.
func TestStreamChatResponsePreservesHeartbeatFraming(t *testing.T) {
	upstreamSSE := ": heartbeat\r\n\r\nevent: message\r\nid: 123\r\nretry: 1000\r\nx-extension: ignored\r\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	code, out := runStreamCase(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.HasPrefix(out, ": heartbeat\n\n") {
		t.Fatalf("heartbeat must remain an SSE comment, got %q", out)
	}
	var dataFrames int
	for _, frame := range strings.Split(out, "\n\n") {
		if frame == "" || strings.HasPrefix(frame, ":") {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("unexpected frame: %q", frame)
		}
		data := strings.TrimPrefix(frame, "data: ")
		if data != "[DONE]" && !json.Valid([]byte(data)) {
			t.Fatalf("CPA would reject non-JSON data frame: %q", data)
		}
		dataFrames++
	}
	if dataFrames != 2 || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("completion frames must be preserved: %q", out)
	}
}

func TestStreamChatHeartbeatOnlyIsNotSuccessfulCompletion(t *testing.T) {
	code, out := runStreamCase(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": heartbeat\n\n"))
	})
	if code != http.StatusOK || !strings.HasPrefix(out, ": heartbeat\n\n") {
		t.Fatalf("expected a streaming comment, got status=%d body=%q", code, out)
	}
	if strings.Contains(out, "[DONE]") || !strings.Contains(out, `"code":"stream_closed_without_finish"`) {
		t.Fatalf("heartbeat followed by EOF must remain a failed stream: %q", out)
	}
}

func TestStreamChatPreservesLegacyDataFraming(t *testing.T) {
	for _, prefix := range []string{"", "data: ", "data: data: "} {
		t.Run(prefix, func(t *testing.T) {
			code, out := runStreamCase(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(prefix + "{\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n[DONE]\n\n"))
			})
			if code != http.StatusOK || !strings.Contains(out, `"content":"OK"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
				t.Fatalf("legacy data framing must remain supported: %q", out)
			}
		})
	}
}
