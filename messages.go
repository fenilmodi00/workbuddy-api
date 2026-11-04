package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Anthropic Messages API (/v1/messages) -> upstream Chat Completions translation
//
// The gateway only talks to the upstream /v2/chat/completions endpoint and is
// responsible for bidirectional protocol conversion:
//   - Requests: system / messages(content blocks) / tools(input_schema) / thinking
//     -> chat messages / tools(nested function) / reasoning_effort
//   - Streaming responses: upstream SSE deltas -> Anthropic semantic events
//     (message_start, content_block_start/delta/stop, message_delta, message_stop)
//   - Non-streaming responses: upstream stream data is aggregated locally into
//     a complete Message object
//
// stop_reason mapping: stop->end_turn, tool_calls->tool_use, length->max_tokens.
// The upstream chain of thought (delta.reasoning_content) is only translated
// back into thinking content blocks when the request enabled thinking —
// clients that did not enable it do not handle such blocks, and extra ones are
// treated as a protocol anomaly.
// -----------------------------------------------------------------------------

// anthropicErrFmtKey marks that error responses for this request should use
// the Anthropic shape ({"type":"error","error":{...}}) instead of OpenAI shape.
type anthropicErrFmtKey struct{}

func withAnthropicErrFormat(ctx context.Context) context.Context {
	return context.WithValue(ctx, anthropicErrFmtKey{}, true)
}

// writeUpstreamError is the unified error exit for upstreamChat / auth
// middleware: requests from /v1/messages get the Anthropic shape, others the
// OpenAI shape.
func writeUpstreamError(w http.ResponseWriter, r *http.Request, statusCode int, errType, message string) {
	// Auth happens before the handler, so the protocol marker is not set yet;
	// match the two Anthropic routes exactly to avoid error responses falling
	// back to OpenAI shape or hitting other endpoints.
	if r.Context().Value(anthropicErrFmtKey{}) == true ||
		r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens" {
		writeAnthropicError(w, statusCode, anthropicErrType(errType), message)
		return
	}
	writeOpenAIError(w, statusCode, errType, message)
}

func writeAnthropicError(w http.ResponseWriter, statusCode int, errType, message string) {
	log.Printf("[Request failed] traceId=%s protocol=Anthropic status_code=%d error_type=%s result=Error returned, not treated as a successful response note=Error details are returned only to the caller; request bodies are not logged",
		w.Header().Get("X-Trace-ID"), statusCode, errType)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

// anthropicErrType maps internal error types to Anthropic's official error
// type enum.
func anthropicErrType(errType string) string {
	switch errType {
	case "no_auth", "invalid_api_key":
		return "authentication_error"
	case "model_disabled", "model_account_disabled":
		return "permission_error"
	case "model_rate_limited", "all_accounts_cooldown":
		return "rate_limit_error"
	case "no_available_account", "model_requires_quota":
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// handleMessages handles POST /v1/messages (Anthropic Messages API).
func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "api_error", "Only POST requests are supported")
		return
	}
	// Mark the error exit to use the Anthropic shape (including account
	// scheduling errors inside upstreamChat).
	r = r.WithContext(withAnthropicErrFormat(r.Context()))

	reqID := requestIDFor(r)
	startTime := requestStartFor(r)

	readStarted := debugBodyReadStarted(r)
	bodyBytes, err := io.ReadAll(r.Body)
	readDuration := debugElapsedSince(readStarted)
	if err != nil {
		debugBodyReadFailed(r, bodyBytes, readStarted, err)
		writeAnthropicError(w, http.StatusBadRequest, "api_error", "Failed to read request body")
		return
	}
	defer r.Body.Close()

	var msgReq map[string]any
	decodeStarted := time.Now()
	decodeErr := json.Unmarshal(bodyBytes, &msgReq)
	decodeDuration := time.Since(decodeStarted)
	if decodeErr != nil {
		debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, false, decodeErr)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body")
		return
	}

	modelName, _ := msgReq["model"].(string)
	if modelName == "" {
		modelName = "hy4-preview"
	}
	isStream, _ := msgReq["stream"].(bool)
	wantThinking := anthropicThinkingOn(msgReq)
	debugSetModelAndStream(r, modelName, isStream)
	debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, true, nil)
	prepareConversationContext(r, msgReq)

	// Model allowlist/blocklist interception: reject on hit without consuming
	// any upstream account quota.
	if disabled, reason := modelDisabled(modelName); disabled {
		log.Printf("[Request rejected] traceId=%s requestId=%d interception=Model allowlist/blocklist model=%s result=Rejected reason=%s response_status_code=403 business_impact=Request did not reach the upstream call",
			w.Header().Get("X-Trace-ID"), reqID, modelName, reason)
		debugEvent(r, "warn", "model_blocked_by_config", map[string]any{
			"status_code":     http.StatusForbidden,
			"reason":          reason,
			"business_impact": "Model disabled by gateway config; request did not reach the upstream call",
		})
		writeAnthropicError(w, http.StatusForbidden, "permission_error", reason)
		return
	}

	// Messages merges text blocks. The leading wildcard must be applied on the
	// original blocks; shared steps must not re-run the wildcard on the merged
	// result, to avoid swallowing body text across blocks.
	compatMessages, _ := msgReq["messages"].([]any)
	var compatSystem map[string]any
	if system, exists := msgReq["system"]; exists {
		compatSystem = map[string]any{"role": "system", "content": system}
		compatMessages = append([]any{compatSystem}, compatMessages...)
	}
	applyOriginalIdentityReplacements(map[string]any{"messages": compatMessages}, r, reqID)
	if compatSystem != nil {
		msgReq["system"] = compatSystem["content"]
	}

	chatReq, err := anthropicToChatRequest(msgReq, modelName)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatReq["stream"] = true // Upstream forces streaming; non-streaming is aggregated locally by the gateway

	prepareUpstreamChatRequest(chatReq, r, reqID, modelName)

	upstreamBytes, err := json.Marshal(chatReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Failed to serialize request")
		return
	}

	debugEvent(r, "info", "messages_request_converted", map[string]any{
		"upstream_bytes":  len(upstreamBytes),
		"business_impact": "Anthropic request converted with prompt, tool sequence and reasoning history rules applied; ready to call upstream",
	})
	log.Printf("[Protocol conversion] traceId=%s requestId=%d entry=/v1/messages model=%s stream=%t upstream_request_bytes=%d result=Converted with unified rules applied; prompt bodies not recorded",
		w.Header().Get("X-Trace-ID"), reqID, modelName, isStream, len(upstreamBytes))

	resp, acc, prof, ok := upstreamChat(w, r, reqID, modelName, upstreamBytes, startTime)
	if !ok {
		return
	}
	if isStream {
		streamMessagesResponse(w, r, resp, modelName, reqID, acc, prof, startTime, wantThinking)
	} else {
		writeMessagesAggregate(w, r, resp, modelName, reqID, acc, prof, startTime, wantThinking)
	}
}

// -----------------------------------------------------------------------------
// Request conversion: Anthropic Messages -> Chat Completions
// -----------------------------------------------------------------------------

// anthropicToChatRequest converts an Anthropic Messages request body into the
// upstream Chat request body.
func anthropicToChatRequest(body map[string]any, modelName string) (map[string]any, error) {
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if sys := extractAnthropicSystemText(body["system"]); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			messages = append(messages, convertAnthropicMessage(msg)...)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages field missing or empty")
	}
	chat["messages"] = messages

	if v, ok := body["max_tokens"].(float64); ok && v > 0 {
		chat["max_tokens"] = int(v)
	}
	if v, ok := body["temperature"].(float64); ok {
		chat["temperature"] = v
	}
	if v, ok := body["top_p"].(float64); ok {
		chat["top_p"] = v
	}
	if v, ok := body["top_k"].(float64); ok {
		chat["top_k"] = v
	}
	if ss, ok := body["stop_sequences"].([]any); ok && len(ss) > 0 {
		chat["stop"] = ss
	}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if ct := convertAnthropicTools(tools); len(ct) > 0 {
			chat["tools"] = ct
		}
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		chat["tool_choice"] = convertAnthropicToolChoice(tc)
		if disabled, ok := tc["disable_parallel_tool_use"].(bool); ok {
			chat["parallel_tool_calls"] = !disabled
		}
		if name := strOf(tc["name"]); name != "" {
			// The actual upstream tool_choice only accepts string enums; the
			// OpenAI function object cannot be sent. Expose only the specified
			// tool and set required, preserving forced-selection semantics.
			var selected []any
			tools, _ := chat["tools"].([]any)
			for _, item := range tools {
				tool, _ := item.(map[string]any)
				fn, _ := tool["function"].(map[string]any)
				if strOf(fn["name"]) == name {
					selected = append(selected, item)
				}
			}
			if len(selected) == 0 {
				return nil, fmt.Errorf("tool specified by tool_choice is not defined in tools")
			}
			chat["tools"] = selected
		}
	} else if tc, ok := body["tool_choice"].(string); ok {
		chat["tool_choice"] = tc
	}

	// thinking -> reasoning_effort (the upstream only accepts flat tiers;
	// disabled turns it off explicitly, and applyThinkingRules skips injection
	// for the off tier).
	if th, ok := body["thinking"].(map[string]any); ok {
		switch anthropicThinkingType(th) {
		case "disabled":
			chat["reasoning_effort"] = "off"
		case "enabled", "adaptive":
			chat["reasoning_effort"] = anthropicEffort(th)
		}
	}
	// Newer Messages puts effort in output_config instead of the thinking
	// object. Explicitly disabling thinking still takes priority and cannot be
	// re-enabled by effort or budget.
	th, _ := body["thinking"].(map[string]any)
	if anthropicThinkingType(th) != "disabled" {
		if output, ok := body["output_config"].(map[string]any); ok {
			if effort := strings.TrimSpace(strOf(output["effort"])); effort != "" {
				chat["reasoning_effort"] = effort
			}
		}
	}
	return chat, nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// anthropicThinkingOn reports whether the request enabled extended thinking
// (the response side uses this to decide whether to translate thinking blocks
// back). Both enabled and adaptive count as enabled: newer Claude Code always
// sends adaptive for models not in its capability table, and honoring only
// enabled would drop the thinking process of such requests entirely.
func anthropicThinkingOn(body map[string]any) bool {
	th, ok := body["thinking"].(map[string]any)
	return ok && anthropicThinkingType(th) != "" && anthropicThinkingType(th) != "disabled"
}

func anthropicThinkingType(th map[string]any) string {
	return strings.ToLower(strings.TrimSpace(strOf(th["type"])))
}

// anthropicEffort takes the explicit thinking.effort tier; when absent, tiers
// are derived from the budget magnitude (<4k low / <16k medium / <64k high /
// above max), defaulting to high when all are missing.
func anthropicEffort(th map[string]any) string {
	if e := strings.TrimSpace(strOf(th["effort"])); e != "" {
		return e
	}
	if b, _ := th["budget_tokens"].(float64); b > 0 {
		switch {
		case b >= 65536:
			return "max"
		case b >= 16384:
			return "high"
		case b >= 4096:
			return "medium"
		default:
			return "low"
		}
	}
	return "high"
}

// extractAnthropicSystemText extracts the system field as plain text. Supports
// a string and a [{type:"text",text:...}] array (ignoring extra fields such as
// cache_control).
func extractAnthropicSystemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		parts := make([]string, 0, len(s))
		for _, b := range s {
			blk, ok := b.(map[string]any)
			if ok && blk["type"] == "text" {
				if t := strOf(blk["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// convertAnthropicMessage converts a single Anthropic message into 0..n chat
// messages.
func convertAnthropicMessage(msg map[string]any) []any {
	role := strOf(msg["role"])
	switch content := msg["content"].(type) {
	case string:
		return []any{map[string]any{"role": role, "content": content}}
	case []any:
		return convertAnthropicBlocks(role, content)
	case nil:
		return []any{map[string]any{"role": role, "content": ""}}
	}
	return nil
}

// convertAnthropicBlocks processes the content blocks array.
// user: tool_result blocks -> standalone tool messages (which must directly
// follow the assistant's tool_calls), remaining text/images merged into one
// user message placed after the tool messages; images nested in tool_result
// are hoisted into that user message (a tool message's content can only be a
// string, leaving nowhere for images).
// assistant: text blocks merged into content, tool_use blocks -> tool_calls;
// thinking / redacted_thinking history blocks are not uploaded (the upstream
// manages its own chain of thought).
func convertAnthropicBlocks(role string, blocks []any) []any {
	switch role {
	case "user":
		var toolMsgs, parts []any
		for _, bAny := range blocks {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": strOf(blk["text"])})
			case "image":
				if url := anthropicImageURL(blk); url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			case "tool_result":
				content := anthropicContentText(blk["content"])
				// Hoist images from the tool result and keep tool content
				// non-empty (required by the upstream)
				hasImage := false
				if list, ok := blk["content"].([]any); ok {
					for _, subAny := range list {
						sub, ok := subAny.(map[string]any)
						if !ok || sub["type"] != "image" {
							continue
						}
						if url := anthropicImageURL(sub); url != "" {
							hasImage = true
							parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
						}
					}
				}
				if content == "" && hasImage {
					content = "[image]"
				}
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": strOf(blk["tool_use_id"]),
					"content":      content,
				})
			}
		}
		var out []any
		out = append(out, toolMsgs...)
		if len(parts) == 1 {
			// Flatten pure text into a string; most upstreams are more
			// tolerant of strings
			if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
				return append(out, map[string]any{"role": "user", "content": p["text"]})
			}
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": parts})
		}
		return out
	case "assistant":
		var texts []string
		var toolCalls []any
		for _, bAny := range blocks {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				if t := strOf(blk["text"]); t != "" {
					texts = append(texts, t)
				}
			case "tool_use":
				args, err := json.Marshal(blk["input"])
				if err != nil || blk["input"] == nil {
					args = []byte("{}")
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   strOf(blk["id"]),
					"type": "function",
					"function": map[string]any{
						"name":      strOf(blk["name"]),
						"arguments": string(args),
					},
				})
			}
		}
		m := map[string]any{"role": "assistant", "content": strings.Join(texts, "")}
		if len(toolCalls) > 0 {
			m["tool_calls"] = toolCalls
		}
		return []any{m}
	}
	// Other roles: text fallback
	var texts []string
	for _, bAny := range blocks {
		if blk, ok := bAny.(map[string]any); ok && blk["type"] == "text" {
			if t := strOf(blk["text"]); t != "" {
				texts = append(texts, t)
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return []any{map[string]any{"role": role, "content": strings.Join(texts, "")}}
}

// anthropicContentText extracts tool_result.content (string or block array)
// as plain text.
func anthropicContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, b := range c {
			if blk, ok := b.(map[string]any); ok && blk["type"] == "text" {
				if t := strOf(blk["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// anthropicImageURL converts an Anthropic image block to a data URL (base64
// source) or the original URL. Unsupported shapes return an empty string and
// the caller silently drops them.
func anthropicImageURL(blk map[string]any) string {
	src, _ := blk["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch strOf(src["type"]) {
	case "base64":
		media, data := strOf(src["media_type"]), strOf(src["data"])
		if media == "" || data == "" {
			return ""
		}
		return "data:" + media + ";base64," + data
	case "url":
		return strOf(src["url"])
	}
	return ""
}

// convertAnthropicTools converts Anthropic tools (input_schema) to chat format.
func convertAnthropicTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, tAny := range tools {
		t, ok := tAny.(map[string]any)
		if !ok {
			continue
		}
		if _, hasFn := t["function"]; hasFn {
			out = append(out, t) // Already chat format; pass through
			continue
		}
		fn := map[string]any{"name": strOf(t["name"])}
		if d, ok := t["description"].(string); ok && d != "" {
			fn["description"] = d
		}
		if p, ok := t["input_schema"]; ok && p != nil {
			fn["parameters"] = p
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// convertAnthropicToolChoice converts to the string enum the upstream actually
// accepts. With a tool name present, the caller filters the tool list and then
// uses required to force the call.
func convertAnthropicToolChoice(tc map[string]any) any {
	if name := strOf(tc["name"]); name != "" {
		return "required"
	}
	switch strings.ToLower(strOf(tc["type"])) {
	case "none":
		return "none"
	case "any", "required":
		return "required"
	}
	return "auto"
}

// -----------------------------------------------------------------------------
// Streaming translation: upstream Chat SSE -> Anthropic Messages SSE
// -----------------------------------------------------------------------------

type anthropicTextSlot struct {
	open  bool
	index int
	buf   strings.Builder
}

type anthropicToolSlot struct {
	id, name string
	args     strings.Builder
	index    int
	open     bool
}

// anthropicStreamState translates incoming chat.completion.chunk messages one
// by one into an Anthropic SSE event byte stream. The same instance can also
// go through aggregate() to output a complete non-streaming Message.
type anthropicStreamState struct {
	msgID        string
	model        string
	wantThinking bool
	started      bool
	blockIndex   int
	thinking     *anthropicTextSlot
	text         *anthropicTextSlot
	tools        map[int]*anthropicToolSlot
	toolOrder    []int
	finish       string
	usage        map[string]any
}

func newAnthropicStreamState(modelName string, wantThinking bool) *anthropicStreamState {
	return &anthropicStreamState{
		msgID:        "msg_" + compactUUID(),
		model:        modelName,
		wantThinking: wantThinking,
		tools:        map[int]*anthropicToolSlot{},
	}
}

// emit formats one Anthropic SSE event (event line + data line + blank line).
func (s *anthropicStreamState) emit(eventType string, data map[string]any) []byte {
	data["type"] = eventType
	b, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return []byte("event: " + eventType + "\ndata: " + string(b) + "\n\n")
}

// feed consumes one upstream chunk and returns 0..n Anthropic events.
func (s *anthropicStreamState) feed(chunk map[string]any) []byte {
	var out []byte
	// The upstream may rewrite model (alias resolution etc.); message_start
	// echoes back the value
	if m := strOf(chunk["model"]); m != "" {
		s.model = m
	}
	if !s.started {
		s.started = true
		out = append(out, s.emit("message_start", map[string]any{
			"message": map[string]any{
				"id":      s.msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"model":   s.model,
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})...)
	}
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		s.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, cAny := range choices {
		choice, ok := cAny.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta != nil {
			// Reasoning deltas are only translated back when thinking was enabled
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" && s.wantThinking {
				out = append(out, s.appendThinking(rc)...)
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				out = append(out, s.appendText(ct)...)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tcAny := range tcs {
					tc, ok := tcAny.(map[string]any)
					if ok {
						out = append(out, s.appendToolCall(tc)...)
					}
				}
			}
		}
		if fr := strOf(choice["finish_reason"]); fr != "" {
			s.finish = fr
		}
	}
	return out
}

func (s *anthropicStreamState) stopText(out []byte) []byte {
	if s.text != nil && s.text.open {
		out = append(out, s.emit("content_block_stop", map[string]any{"index": s.text.index})...)
		s.text.open = false
	}
	return out
}

// stopThinking emits signature_delta first, then closes the block — with the
// order reversed, deltas arriving after content_block_stop are dropped by the
// client and the signature would be wasted.
func (s *anthropicStreamState) stopThinking(out []byte) []byte {
	if s.thinking == nil || !s.thinking.open {
		return out
	}
	s.thinking.open = false
	out = append(out, s.emit("content_block_delta", map[string]any{
		"index": s.thinking.index,
		"delta": map[string]any{"type": "signature_delta", "signature": anthropicSignature(s.thinking.buf.String())},
	})...)
	out = append(out, s.emit("content_block_stop", map[string]any{"index": s.thinking.index})...)
	return out
}

func (s *anthropicStreamState) appendThinking(rc string) []byte {
	var out []byte
	// The chain of thought only appears before the body; if a text block is
	// open, close it first before opening a new thinking block.
	out = s.stopText(out)
	if s.thinking == nil || !s.thinking.open {
		s.thinking = &anthropicTextSlot{open: true, index: s.blockIndex}
		s.blockIndex++
		out = append(out, s.emit("content_block_start", map[string]any{
			"index":         s.thinking.index,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})...)
	}
	s.thinking.buf.WriteString(rc)
	out = append(out, s.emit("content_block_delta", map[string]any{
		"index": s.thinking.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
	})...)
	return out
}

func (s *anthropicStreamState) appendText(ct string) []byte {
	var out []byte
	// Body starts: close the thinking block first to guarantee block order
	// thinking -> text.
	out = s.stopThinking(out)
	if s.text == nil || !s.text.open {
		s.text = &anthropicTextSlot{open: true, index: s.blockIndex}
		s.blockIndex++
		out = append(out, s.emit("content_block_start", map[string]any{
			"index":         s.text.index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})...)
	}
	s.text.buf.WriteString(ct)
	out = append(out, s.emit("content_block_delta", map[string]any{
		"index": s.text.index,
		"delta": map[string]any{"type": "text_delta", "text": ct},
	})...)
	return out
}

func (s *anthropicStreamState) appendToolCall(tc map[string]any) []byte {
	idx := 0
	if v, ok := tc["index"].(float64); ok {
		idx = int(v)
	}
	slot, exists := s.tools[idx]
	if !exists {
		slot = &anthropicToolSlot{index: s.blockIndex}
		s.blockIndex++
		s.tools[idx] = slot
		s.toolOrder = append(s.toolOrder, idx)
	}
	// Tool block starts: close thinking / text first to guarantee block order
	// thinking -> text -> tool_use.
	var out []byte
	out = s.stopThinking(out)
	out = s.stopText(out)
	if id := strOf(tc["id"]); id != "" {
		slot.id = id
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if n := strOf(fn["name"]); n != "" {
			slot.name = n
		}
	}
	if !slot.open {
		slot.open = true
		out = append(out, s.emit("content_block_start", map[string]any{
			"index": slot.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": slot.id, "name": slot.name, "input": map[string]any{},
			},
		})...)
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		// Argument shards pass through verbatim; reassembly is left to the
		// client (there is no way to tell when the JSON is complete)
		if a := strOf(fn["arguments"]); a != "" {
			slot.args.WriteString(a)
			out = append(out, s.emit("content_block_delta", map[string]any{
				"index": slot.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": a},
			})...)
		}
	}
	return out
}

// mapStopReason maps chat finish_reason to Anthropic stop_reason. Values not
// listed, such as content_filter, always finish as end_turn.
func mapStopReason(finish string) string {
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	}
	return "end_turn"
}

func intFloat(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// anthropicUsage converts upstream usage to the Anthropic convention. The
// upstream prompt_tokens already include cache-hit tokens; Anthropic's
// input_tokens exclude the cache — without subtracting, the client would count
// the cache twice when adding input and cache_read.
func (s *anthropicStreamState) anthropicUsage() map[string]any {
	in, out, cached := 0, 0, 0
	if s.usage != nil {
		in = intFloat(s.usage["prompt_tokens"])
		out = intFloat(s.usage["completion_tokens"])
		if d, ok := s.usage["prompt_tokens_details"].(map[string]any); ok {
			cached = intFloat(d["cached_tokens"])
		}
	}
	if cached > in {
		cached = in // Clamp to non-negative when the upstream reports a malformed hit count
	}
	return map[string]any{
		"input_tokens":                in - cached,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
		"output_tokens":               out,
	}
}

// finishEvents closes all unclosed blocks and outputs message_delta +
// message_stop.
func (s *anthropicStreamState) finishEvents() []byte {
	var out []byte
	out = s.stopThinking(out)
	out = s.stopText(out)
	for _, idx := range s.toolOrder {
		slot := s.tools[idx]
		if slot.open {
			out = append(out, s.emit("content_block_stop", map[string]any{"index": slot.index})...)
			slot.open = false
		}
	}
	out = append(out, s.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": mapStopReason(s.finish), "stop_sequence": nil},
		"usage": s.anthropicUsage(),
	})...)
	out = append(out, s.emit("message_stop", map[string]any{})...)
	return out
}

// aggregate outputs the complete Anthropic Message object (non-streaming
// response).
func (s *anthropicStreamState) aggregate() map[string]any {
	content := []any{}
	if s.thinking != nil && s.thinking.buf.Len() > 0 {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": s.thinking.buf.String(),
			"signature": anthropicSignature(s.thinking.buf.String()),
		})
	}
	if s.text != nil && s.text.buf.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": s.text.buf.String()})
	}
	for _, idx := range s.toolOrder {
		slot := s.tools[idx]
		var input any
		args := slot.args.String()
		if args == "" {
			input = map[string]any{}
		} else if err := json.Unmarshal([]byte(args), &input); err != nil {
			input = args // Return arguments verbatim when not valid JSON, rather than failing the call
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": slot.id, "name": slot.name, "input": input,
		})
	}
	return map[string]any{
		"id":            s.msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         s.model,
		"stop_reason":   mapStopReason(s.finish),
		"stop_sequence": nil,
		"usage":         s.anthropicUsage(),
	}
}

// anthropicSignature generates the echo signature for thinking blocks. The
// request side does not upload thinking history (the upstream manages its own
// chain of thought); the signature only exists to satisfy strict clients'
// protocol validation, so hashing the reasoning body suffices and reproduces
// deterministically.
func anthropicSignature(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// -----------------------------------------------------------------------------
// Response outlets: streaming / aggregate / count_tokens
// -----------------------------------------------------------------------------

// scanUpstreamSSE scans the upstream SSE line by line, feeding each chunk to
// the state machine. Returns the scanner error (nil means a clean EOF).
func scanUpstreamSSE(body io.Reader, state *anthropicStreamState, sink func([]byte)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		observeFirstToken(body, chunk)
		if out := state.feed(chunk); sink != nil && len(out) > 0 {
			sink(out)
		}
	}
	err := scanner.Err()
	completeTokenStream(body, err == nil && state.finish != "")
	return err
}

// streamMessagesResponse translates the upstream streaming response into
// Anthropic SSE in real time.
func streamMessagesResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time, wantThinking bool) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Server does not support streaming Flush")
		return
	}
	writeEvent := func(b []byte) {
		if len(b) == 0 {
			return
		}
		_, _ = w.Write(b)
		flusher.Flush()
	}

	state := newAnthropicStreamState(modelName, wantThinking)
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	scanErr := scanUpstreamSSE(body, state, writeEvent)
	statsObserveUsage(r, state.usage, scanErr == nil && state.finish != "")
	if scanErr != nil {
		debugEvent(r, "error", "stream_response_failed", map[string]any{
			"error_type": debugErrorType(scanErr),
			"error":      safeDebugError(scanErr),
		})
		// On upstream stream interruption we must not fake message_stop: that
		// would let the client treat partial output as a complete result.
		// Instead emit an error event and return directly, making the
		// incompleteness explicit.
		reason := "Upstream streaming response interrupted; this reply is incomplete"
		if errors.Is(scanErr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("Upstream sent no data for over %v; connection judged stuck and interrupted, this reply is incomplete", currentIdleTimeout())
		}
		writeEvent(state.emit("error", map[string]any{
			"error": map[string]any{"type": "api_error", "message": reason},
		}))
		log.Printf("[Exception] traceId=%s requestId=%d stage=Upstream streaming read account=%s exception=%v business_impact=This response is incomplete; an error event was sent instead of faking completion", debugTraceID(r), reqID, acc.Path, scanErr)
		recordModelLatency(modelName, time.Since(startTime))
		return
	}
	if state.finish == "" {
		// Clean EOF but no finish_reason: refuse to fake message_stop, emit
		// an error event instead.
		debugEvent(r, "error", "stream_closed_without_finish", map[string]any{
			"error_type":      "stream_closed_without_finish",
			"finish_reason":   "",
			"business_impact": "Upstream ended cleanly but without finish_reason; message_stop was rejected in favor of an error event",
		})
		writeEvent(state.emit("error", map[string]any{
			"error": map[string]any{"type": "api_error", "message": "Upstream stream ended without finish_reason; this reply is incomplete"},
		}))
		log.Printf("[Exception] traceId=%s requestId=%d stage=Messages finalize account=%s result=Refused to treat as success reason=Upstream ended cleanly but without finish_reason", debugTraceID(r), reqID, acc.Path)
		recordModelLatency(modelName, time.Since(startTime))
		return
	}

	writeEvent(state.finishEvents())
	observeModelCredit(acc, modelName, state.usage, reqID)
	statsObserveUsage(r, state.usage, true)
	recordModelTokens(modelName, state.usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	debugEvent(r, "info", "stream_response_completed", map[string]any{"status_code": http.StatusOK})
	log.Printf("[Response complete] traceId=%s requestId=%d Messages streaming output completed account=%s site=%s duration=%v first_token(v2)=%s", debugTraceID(r), reqID, acc.Path, prof.Label, time.Since(startTime), body.text())
}

// writeMessagesAggregate aggregates the upstream stream locally and outputs a
// complete Anthropic Message.
func writeMessagesAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time, wantThinking bool) {
	defer resp.Body.Close()
	state := newAnthropicStreamState(modelName, wantThinking)
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	if err := scanUpstreamSSE(body, state, nil); err != nil {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		log.Printf("[Exception] traceId=%s requestId=%d stage=Upstream aggregate read account=%s exception=%v", debugTraceID(r), reqID, acc.Path, err)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Failed to aggregate upstream streaming response: "+err.Error())
		return
	}
	if state.finish == "" {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type":      "stream_closed_without_finish",
			"business_impact": "Upstream ended cleanly but without finish_reason; refusing to aggregate into a complete Message",
		})
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "Upstream stream ended without finish_reason; this reply is incomplete")
		return
	}
	msg := state.aggregate()
	observeModelCredit(acc, modelName, state.usage, reqID)
	statsObserveUsage(r, state.usage, true)
	recordModelTokens(modelName, state.usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	out, err := json.Marshal(msg)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Failed to serialize response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(out),
	})
	log.Printf("[Response complete] traceId=%s requestId=%d Messages non-streaming response completed account=%s site=%s duration=%v", debugTraceID(r), reqID, acc.Path, prof.Label, time.Since(startTime))
}

// handleCountTokens handles POST /v1/messages/count_tokens.
// Estimated locally without hitting the upstream: CJK ~1 token/char, ASCII
// ~4 chars/token.
func handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "api_error", "Only POST requests are supported")
		return
	}
	readStarted := debugBodyReadStarted(r)
	bodyBytes, err := io.ReadAll(r.Body)
	readDuration := debugElapsedSince(readStarted)
	if err != nil {
		debugBodyReadFailed(r, bodyBytes, readStarted, err)
		writeAnthropicError(w, http.StatusBadRequest, "api_error", "Failed to read request body")
		return
	}
	defer r.Body.Close()
	var msgReq map[string]any
	decodeStarted := time.Now()
	err = json.Unmarshal(bodyBytes, &msgReq)
	debugBodyReadCompleted(r, bodyBytes, readDuration, time.Since(decodeStarted), err == nil, err)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body")
		return
	}
	tokens := estimateAnthropicInputTokens(msgReq)
	debugEvent(r, "info", "tokens_estimated_locally", map[string]any{
		"input_tokens": tokens, "business_impact": "Only client input is estimated; no upstream call, no account quota consumed, gateway-injected prompts excluded",
	})
	log.Printf("[Token estimate] traceId=%s requestId=%d input_bytes=%d estimated_tokens=%d result=Only client input estimated locally; gateway-injected prompts excluded; upstream not called",
		w.Header().Get("X-Trace-ID"), requestIDFor(r), len(bodyBytes), tokens)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"input_tokens": tokens,
	})
}

func estimateAnthropicInputTokens(body map[string]any) int {
	var sb strings.Builder
	if sys := extractAnthropicSystemText(body["system"]); sys != "" {
		sb.WriteString(sys)
		sb.WriteByte('\n')
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			if msg, ok := m.(map[string]any); ok {
				appendAnthropicContentText(&sb, msg["content"])
				sb.WriteByte('\n')
			}
		}
	}
	n := approxTokens(sb.String())
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			n += len(b) / 4
		}
	}
	return n
}

func appendAnthropicContentText(sb *strings.Builder, content any) {
	switch c := content.(type) {
	case string:
		sb.WriteString(c)
	case []any:
		for _, bAny := range c {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				sb.WriteString(strOf(blk["text"]))
			case "tool_result":
				sb.WriteString(anthropicContentText(blk["content"]))
			case "tool_use":
				sb.WriteString(strOf(blk["name"]))
				if b, err := json.Marshal(blk["input"]); err == nil {
					sb.Write(b)
				}
			}
		}
	}
}

func approxTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}
