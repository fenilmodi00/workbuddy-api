package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------------
// OpenAI Responses API (/v1/responses) -> upstream Chat Completions translation
//
// The gateway only talks to the upstream /v2/chat/completions endpoint and is
// responsible for bidirectional protocol conversion:
//   - Requests: input / instructions / tools(flat) / tool_choice / max_output_tokens /
//     reasoning.effort  ->  chat messages / tools(nested function) / max_tokens / reasoning_effort
//   - Non-streaming responses: chat.completion -> response{object:"response", output:[...]}
//   - Streaming responses: upstream SSE deltas -> Responses semantic events
//     (response.output_text.delta, response.function_call_arguments.delta,
//     response.completed ...)
// -----------------------------------------------------------------------------

// handleResponses handles POST /v1/responses.
func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST requests are supported")
		return
	}

	reqID := requestIDFor(r)
	startTime := requestStartFor(r)

	readStarted := debugBodyReadStarted(r)
	bodyBytes, err := io.ReadAll(r.Body)
	readDuration := debugElapsedSince(readStarted)
	if err != nil {
		debugBodyReadFailed(r, bodyBytes, readStarted, err)
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "Failed to read request body")
		return
	}
	defer r.Body.Close()

	var respReq map[string]any
	decodeStarted := time.Now()
	decodeErr := json.Unmarshal(bodyBytes, &respReq)
	decodeDuration := time.Since(decodeStarted)
	if decodeErr != nil {
		debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, false, decodeErr)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON request body")
		return
	}

	modelName, _ := respReq["model"].(string)
	if modelName == "" {
		modelName = "hy4-preview"
	}
	isStream, _ := respReq["stream"].(bool)
	debugSetModelAndStream(r, modelName, isStream)
	debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, true, nil)
	prepareConversationContext(r, respReq)

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
		writeOpenAIError(w, http.StatusForbidden, "model_disabled", reason)
		return
	}

	chatReq, toolMap, err := responsesToChatRequestDetailed(respReq, modelName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatReq["stream"] = true // Upstream forces streaming; non-streaming is aggregated locally by the gateway
	// The downstream Responses restores the original namespace/custom shapes;
	// the Chat upstream only understands flat function names.
	r = withResponsesToolMap(r, toolMap)

	prepareUpstreamChatRequest(chatReq, r, reqID, modelName)
	logResponsesToolOutputConversion(r, reqID, respReq)

	upstreamBytes, err := json.Marshal(chatReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "Failed to serialize request")
		return
	}

	log.Printf("[#%d] POST /v1/responses -> Upstream [Model: %s, Stream: %v]", reqID, modelName, isStream)

	resp, acc, prof, ok := upstreamChat(w, r, reqID, modelName, upstreamBytes, startTime)
	if !ok {
		return
	}
	if isStream {
		streamResponsesResponse(w, r, resp, modelName, reqID, acc, prof, startTime)
	} else {
		writeResponsesAggregate(w, r, resp, modelName, reqID, acc, prof, startTime)
	}
}

// responsesToChatRequest keeps the legacy test/call interface; the main path
// uses the detailed version with tool mapping.
func responsesToChatRequest(respReq map[string]any, modelName string) (map[string]any, error) {
	chat, _, err := responsesToChatRequestDetailed(respReq, modelName)
	return chat, err
}

// responsesToChatRequestDetailed converts a Responses request body into the
// upstream Chat Completions request body and returns a reverse mapping from
// upstream function names to client tool identities.
func responsesToChatRequestDetailed(respReq map[string]any, modelName string) (map[string]any, map[string]responsesToolMeta, error) {
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if instructions, ok := respReq["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := respReq["input"].(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		// DSH splits one model reply into separate reasoning, message,
		// function_call, etc. items, but the Chat upstream requires the same
		// reply to be a single assistant message. Consecutive assistant items
		// end at a user/tool result; a new reasoning item also means the next
		// assistant reply. Even an output without reasoning must merge the
		// body and the tool calls together.
		var pendingReasoning string
		var pendingAssistant map[string]any
		flushAssistant := func() {
			if pendingAssistant != nil {
				messages = append(messages, pendingAssistant)
				pendingAssistant = nil
			}
			pendingReasoning = ""
		}
		for _, itemAny := range input {
			switch item := itemAny.(type) {
			case string:
				flushAssistant()
				messages = append(messages, map[string]any{"role": "user", "content": item})
			case map[string]any:
				typ, _ := item["type"].(string)
				if typ == "reasoning" {
					if pendingAssistant != nil {
						flushAssistant() // A new reasoning item means the next assistant reply
					}
					if text := reasoningReplayText(item); text != "" {
						if pendingReasoning != "" {
							pendingReasoning += "\n\n"
						}
						pendingReasoning += text
					}
					continue
				}
				if typ == "web_search_call" {
					continue // Do not mistakenly turn historical search items into messages that would break tool calls
				}
				for _, msgAny := range convertResponsesInputItem(item) {
					msg, ok := msgAny.(map[string]any)
					if !ok {
						continue
					}
					if role, _ := msg["role"].(string); role == "assistant" {
						if pendingAssistant == nil {
							pendingAssistant = msg
							if pendingReasoning != "" {
								if _, explicit := pendingAssistant["reasoning_content"]; !explicit {
									pendingAssistant["reasoning_content"] = pendingReasoning
								}
							}
						} else {
							mergeResponsesAssistantItem(pendingAssistant, msg)
						}
						continue
					}
					flushAssistant() // user/tool boundary; do not carry old reasoning into the next round
					messages = append(messages, msg)
				}
			}
		}
		flushAssistant()
	}

	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("input is empty: Responses requests must provide input or instructions")
	}
	chat["messages"] = messages

	if v, ok := respReq["temperature"]; ok && v != nil {
		chat["temperature"] = v
	}
	if v, ok := respReq["top_p"]; ok && v != nil {
		chat["top_p"] = v
	}
	if v, ok := respReq["parallel_tool_calls"].(bool); ok {
		chat["parallel_tool_calls"] = v
	}
	if text, ok := respReq["text"].(map[string]any); ok {
		if verbosity, ok := text["verbosity"].(string); ok && verbosity != "" {
			chat["verbosity"] = verbosity
		}
	}
	if v, ok := respReq["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	tools, toolMap := convertResponsesToolsDetailed(respReq["tools"])
	if len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc, selected := convertResponsesToolChoiceWithMap(respReq["tool_choice"], toolMap); tc != nil {
		chat["tool_choice"] = tc
		if selected != "" {
			allTools, _ := chat["tools"].([]any)
			filtered := make([]any, 0, 1)
			for _, raw := range allTools {
				item, _ := raw.(map[string]any)
				fn, _ := item["function"].(map[string]any)
				if strOf(fn["name"]) == selected {
					filtered = append(filtered, item)
				}
			}
			if len(filtered) == 0 {
				return nil, nil, fmt.Errorf("tool specified by tool_choice is not defined in tools")
			}
			chat["tools"] = filtered
		}
	}
	if reasoning, ok := respReq["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" && effort != "none" {
			chat["reasoning_effort"] = effort
		}
		if summary, ok := reasoning["summary"].(string); ok && summary != "" {
			chat["reasoning_summary"] = summary
		}
	}
	return chat, toolMap, nil
}

// reasoningReplayText extracts the reasoning body from a DSH replayed reasoning
// item. Items the gateway sent itself put the full text in summary; official
// Responses may also put it in content. Use content when present, else summary.
// encrypted_content is not plaintext and must not be treated as
// reasoning_content.
func reasoningReplayText(item map[string]any) string {
	if text := joinReasoningParts(item["content"]); text != "" {
		return text
	}
	return joinReasoningParts(item["summary"])
}

func joinReasoningParts(raw any) string {
	parts, ok := raw.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, partAny := range parts {
		part, ok := partAny.(map[string]any)
		if !ok {
			continue
		}
		text, _ := part["text"].(string)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return b.String()
}

// mergeResponsesAssistantItem merges the body and tool calls of one Responses
// reply into the same Chat assistant message. reasoning_content already sits on
// the first item; later items only append body and tool lists, without copying
// the reasoning.
func mergeResponsesAssistantItem(dst, item map[string]any) {
	if assistantMessageHasPayload(item) {
		if !assistantMessageHasPayload(dst) {
			dst["content"] = item["content"]
		} else {
			dst["content"] = append(responsesChatContentParts(dst["content"]), responsesChatContentParts(item["content"])...)
		}
	}
	if calls, ok := item["tool_calls"].([]any); ok && len(calls) > 0 {
		if existing, ok := dst["tool_calls"].([]any); ok {
			dst["tool_calls"] = append(existing, calls...)
		} else {
			dst["tool_calls"] = calls
		}
	}
}

func responsesChatContentParts(content any) []any {
	switch value := content.(type) {
	case []any:
		return value
	case string:
		if value != "" {
			return []any{map[string]any{"type": "text", "text": value}}
		}
	}
	return nil
}

func reasoningPassthroughStats(chat map[string]any) (attached int, chars int, missing int) {
	list, _ := chat["messages"].([]any)
	for _, msgAny := range list {
		msg, ok := msgAny.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		text, _ := msg["reasoning_content"].(string)
		if text == "" {
			missing++
			continue
		}
		attached++
		chars += utf8.RuneCountInString(text)
	}
	return attached, chars, missing
}

// convertResponsesInputItem converts a single Responses input item into 0..1
// chat messages.
func convertResponsesInputItem(item map[string]any) []any {
	switch typ, _ := item["type"].(string); typ {
	case "function_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		if namespace, _ := item["namespace"].(string); namespace != "" {
			name = flattenNamespaceToolName(namespace, name)
		}
		args, _ := item["arguments"].(string)
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       ifEmpty(callID, "call_"+compactUUID()),
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}
	case "custom_tool_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		input, _ := item["input"].(string)
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       ifEmpty(callID, "call_"+compactUUID()),
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": customToolArguments(input)},
			}},
		}}
	case "function_call_output", "custom_tool_call_output":
		callID, _ := item["call_id"].(string)
		text, images := convertResponsesToolOutput(item["output"])
		messages := []any{map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      text,
		}}
		if len(images) > 0 {
			// Same image handling as Anthropic tool_result: the tool message
			// keeps the text and images are hoisted into user multimodal
			// content; Base64 must not be replayed as plain text. The tool
			// sequence repair in handleResponses then moves parallel batches'
			// image messages after all tool results, avoiding breaking
			// call/result pairing (11148).
			messages = append(messages, map[string]any{"role": "user", "content": images})
		}
		return messages
	case "reasoning", "web_search_call":
		// reasoning body is attached to the assistant message's
		// reasoning_content inside responsesToChatRequest.
		// web_search_call must not become a message: inserting it between a
		// parallel function_call and its output triggers 11148.
		return nil
	}

	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	message := map[string]any{"role": role, "content": convertResponsesContent(item["content"])}
	if role == "assistant" {
		// Some clients carry Chat-style reasoning fields directly on
		// assistant history messages; do not drop them again during the
		// Responses-to-Chat conversion.
		if reasoning, ok := item["reasoning_content"].(string); ok {
			message["reasoning_content"] = reasoning
		}
	}
	return []any{message}
}

// convertResponsesContent converts Responses content (string or parts array)
// into chat content.
func convertResponsesContent(content any) any {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]any, 0, len(c))
		for _, pAny := range c {
			p, ok := pAny.(map[string]any)
			if !ok {
				if s, ok := pAny.(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
				continue
			}
			switch typ, _ := p["type"].(string); typ {
			case "input_text", "output_text", "text":
				if t, ok := p["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t, ok := p["refusal"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image":
				url, _ := p["image_url"].(string)
				if url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return fmt.Sprintf("%v", c)
	}
}

// convertResponsesToolOutput handles Responses string or multimodal tool
// results. Plain JSON objects/arrays keep the old serialization behavior;
// explicit text blocks are extracted verbatim and image blocks are returned as
// multimodal content. Unknown blocks remain JSON text; tool data is never
// silently dropped.
func convertResponsesToolOutput(output any) (string, []any) {
	blocks, ok := output.([]any)
	if !ok {
		return stringifyToolOutput(output), nil
	}
	typed := false
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(block, "type") {
		case "input_text", "output_text", "text", "input_image", "image_url":
			typed = true
		}
	}
	if !typed {
		return stringifyToolOutput(output), nil
	}
	var texts []string
	var images []any
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok {
			switch stringField(block, "type") {
			case "input_text", "output_text", "text":
				if text, ok := block["text"].(string); ok {
					texts = append(texts, text)
					continue
				}
			case "input_image", "image_url":
				url, _ := block["image_url"].(string)
				detail, _ := block["detail"].(string)
				if nested, ok := block["image_url"].(map[string]any); ok {
					url, _ = nested["url"].(string)
					detail, _ = nested["detail"].(string)
				}
				if url != "" {
					image := map[string]any{"url": url}
					if detail != "" {
						image["detail"] = detail
					}
					images = append(images, map[string]any{"type": "image_url", "image_url": image})
					continue
				}
			}
		}
		texts = append(texts, stringifyToolOutput(raw))
	}
	text := strings.Join(texts, "\n")
	if text == "" && len(images) > 0 {
		text = "[image]"
	}
	return text, images
}

// logResponsesToolOutputConversion logs only block counts, positions, and
// lengths; image URLs, Base64, tool bodies, and prompts never enter the logs,
// reusing the request's TraceID and file logging.
func logResponsesToolOutputConversion(r *http.Request, reqID uint64, request map[string]any) {
	input, _ := request["input"].([]any)
	outputs, textChars, imageCount := 0, 0, 0
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "function_call_output" {
			continue
		}
		text, images := convertResponsesToolOutput(item["output"])
		outputs++
		textChars += utf8.RuneCountInString(text)
		imageCount += len(images)
	}
	log.Printf("[Responses tool output conversion] traceId=%s requestId=%d tool_outputs=%d text_chars=%d image_blocks=%d image_position=user multimodal message after tool results result=Images are no longer counted as Base64 text in context; original tool text preserved and bodies not logged",
		debugTraceID(r), reqID, outputs, textChars, imageCount)
	debugEvent(r, "debug", "responses_tool_output_converted", map[string]any{
		"tool_outputs": outputs, "text_chars": textChars, "image_blocks": imageCount,
		"image_position":  "user_after_tool_results",
		"business_impact": "Tool text and images preserved; avoids Base64 being billed as text or filling up context; bodies not logged",
	})
}

// stringifyToolOutput converts the function_call_output output uniformly to a
// string.
func stringifyToolOutput(output any) string {
	switch o := output.(type) {
	case nil:
		return ""
	case string:
		return o
	default:
		if b, err := json.Marshal(o); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", o)
	}
}

// convertResponsesToolChoice converts a Responses tool_choice to a chat
// tool_choice.
func convertResponsesToolChoice(tc any) any {
	choice, _ := convertResponsesToolChoiceWithMap(tc, nil)
	return choice
}

// convertResponsesToolChoiceWithMap only returns the string enums the upstream
// accepts; for named tools it also returns the flat tool name, which the
// caller uses to trim tools. The Tencent upstream does not accept OpenAI's
// function object form.
func convertResponsesToolChoiceWithMap(tc any, tools map[string]responsesToolMeta) (any, string) {
	switch v := tc.(type) {
	case string:
		if v == "auto" || v == "none" || v == "required" {
			return v, ""
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		if typ == "function" || typ == "custom" {
			name, _ := v["name"].(string)
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			namespace, _ := v["namespace"].(string)
			name = flattenNamespaceToolName(namespace, name)
			if name != "" {
				if tools != nil {
					if meta, ok := tools[name]; ok {
						return "required", meta.UpstreamName
					}
				}
				return "required", name
			}
		}
	}
	return nil, ""
}

// writeResponsesAggregate aggregates the upstream SSE and converts it into a
// non-streaming Responses response.
func writeResponsesAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		logAggregateFailure(r, reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "Failed to aggregate upstream streaming response: "+err.Error())
		return
	}
	usage := usageFromCompletion(completionJSON)
	statsObserveUsage(r, usage, true)
	observeModelCredit(acc, modelName, usage, reqID)
	recordModelTokens(modelName, usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	out, err := chatCompletionToResponsesWithTools(completionJSON, modelName, responsesToolMapFor(r))
	if err != nil {
		debugEvent(r, "error", "response_translation_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		log.Printf("[#%d] Responses conversion failed: %v", reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "translate_error", "Failed to convert Responses response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(out),
	})
	log.Printf("[#%d] Responses non-streaming response completed (account %s [%s], duration %v)", reqID, acc.Path, prof.Label, time.Since(startTime))
}

// chatCompletionToResponses keeps the legacy interface; the main path uses the
// version with tool mapping.
func chatCompletionToResponses(chatJSON []byte, modelName string) ([]byte, error) {
	return chatCompletionToResponsesWithTools(chatJSON, modelName, nil)
}

// chatCompletionToResponsesWithTools converts chat.completion JSON into a
// Responses response object.
func chatCompletionToResponsesWithTools(chatJSON []byte, modelName string, toolMap map[string]responsesToolMeta) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(chatJSON, &chat); err != nil {
		return nil, err
	}

	var content, reasoning string
	var toolCalls []any
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				content, _ = msg["content"].(string)
				reasoning, _ = msg["reasoning_content"].(string)
				toolCalls, _ = msg["tool_calls"].([]any)
			}
		}
	}

	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":      "rs_" + compactUUID(),
			"type":    "reasoning",
			"status":  "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if content != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + compactUUID(),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": content, "annotations": []any{}},
			},
		})
	}
	for _, tcAny := range toolCalls {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		callID, _ := tc["id"].(string)
		name, args := "", ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		output = append(output, responsesToolCallItem(callID, name, args, toolMap))
	}

	now := time.Now().Unix()
	created := now
	if v, ok := chat["created"].(float64); ok && v > 0 && int64(v) <= now {
		created = int64(v)
	}
	respID, _ := chat["id"].(string)
	if respID == "" {
		respID = "resp_" + compactUUID()
	} else {
		respID = "resp_" + strings.TrimPrefix(respID, "chatcmpl-")
	}

	result := buildResponsesEnvelope(respID, modelName, created)
	result["status"] = "completed"
	result["output"] = output
	result["completed_at"] = now
	if usage, ok := chat["usage"].(map[string]any); ok {
		result["usage"] = toResponsesUsage(usage)
	}
	return json.Marshal(result)
}

// buildResponsesEnvelope constructs the Responses response skeleton (including
// common fields required by the spec).
func buildResponsesEnvelope(id, model string, createdAt int64) map[string]any {
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           createdAt,
		"completed_at":         nil,
		"status":               "in_progress",
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                true,
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"usage":                nil,
		"metadata":             map[string]any{},
	}
}

// toResponsesUsage converts chat usage into the Responses usage structure.
func toResponsesUsage(u map[string]any) map[string]any {
	in := numOr0(u["prompt_tokens"])
	out := numOr0(u["completion_tokens"])
	total := numOr0(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	cached, reasoningTokens := 0.0, 0.0
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = numOr0(d["cached_tokens"])
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoningTokens = numOr0(d["reasoning_tokens"])
	}
	return map[string]any{
		"input_tokens":          int64(in),
		"input_tokens_details":  map[string]any{"cached_tokens": int64(cached)},
		"output_tokens":         int64(out),
		"output_tokens_details": map[string]any{"reasoning_tokens": int64(reasoningTokens)},
		"total_tokens":          int64(total),
	}
}

func numOr0(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// streamResponsesResponse translates the upstream Chat Completions SSE into a
// Responses semantic event stream in real time.
func streamResponsesResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "Server does not support streaming Flush")
		return
	}

	seq := 0
	emit := func(eventType string, data map[string]any) {
		data["type"] = eventType
		data["sequence_number"] = seq
		seq++
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
		flusher.Flush()
	}

	created := time.Now().Unix()
	respID := "resp_" + compactUUID()
	envelope := buildResponsesEnvelope(respID, modelName, created)
	emit("response.created", map[string]any{"response": envelope})
	emit("response.in_progress", map[string]any{"response": envelope})

	nextIndex := 0
	idxToItem := map[int]map[string]any{}

	// reasoning item state
	reasoningOpen := false
	reasoningIndex := -1
	reasoningItemID := ""
	var reasoningSB strings.Builder

	// message item state
	msgOpen := false
	msgIndex := -1
	msgItemID := ""
	var msgSB strings.Builder

	// function_call item state
	toolCalls := map[int]*mergedToolCall{}
	var toolOrder []int
	tcItemID := map[int]string{}
	tcIndex := map[int]int{}
	tcMeta := map[int]responsesToolMeta{}

	openReasoning := func() {
		if reasoningOpen {
			return
		}
		reasoningOpen = true
		reasoningIndex = nextIndex
		nextIndex++
		reasoningItemID = "rs_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": reasoningIndex,
			"item":         map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		})
		emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}

	openMessage := func() {
		if msgOpen {
			return
		}
		msgOpen = true
		msgIndex = nextIndex
		nextIndex++
		msgItemID = "msg_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": msgIndex,
			"item":         map[string]any{"id": msgItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		})
		emit("response.content_part.added", map[string]any{
			"item_id": msgItemID, "output_index": msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "annotations": []any{}, "text": ""},
		})
	}

	var usage map[string]any
	finishReason := ""
	sawDone := false

	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		observeFirstToken(body, chunk)
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		finishReason = noteFinishReason(finishReason, chunk)
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				openReasoning()
				reasoningSB.WriteString(rc)
				emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "delta": rc,
				})
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				openMessage()
				msgSB.WriteString(ct)
				emit("response.output_text.delta", map[string]any{
					"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "delta": ct,
				})
			}
			if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
				// Extract this round's argument deltas first, to emit delta
				// events after merging
				type argDelta struct {
					idx int
					arg string
				}
				var argDeltas []argDelta
				for _, tcAny := range tcs {
					tc, ok := tcAny.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := tc["index"].(float64); ok {
						idx = int(v)
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if a, ok := fn["arguments"].(string); ok && a != "" {
							argDeltas = append(argDeltas, argDelta{idx: idx, arg: a})
						}
					}
				}
				before := len(toolOrder)
				applyToolCallDelta(toolCalls, &toolOrder, tcs)
				for i := before; i < len(toolOrder); i++ {
					idx := toolOrder[i]
					st := toolCalls[idx]
					if st.ID == "" {
						st.ID = "call_" + compactUUID()
					}
					meta, _ := responsesToolMetaFor(r, st.Name)
					tcMeta[idx] = meta
					tcIndex[idx] = nextIndex
					nextIndex++
					if meta.Kind == responsesToolCustom {
						tcItemID[idx] = "ctc_" + compactUUID()
						emit("response.output_item.added", map[string]any{
							"output_index": tcIndex[idx],
							"item": map[string]any{
								"id": tcItemID[idx], "type": "custom_tool_call", "status": "in_progress",
								"call_id": st.ID, "name": meta.Name, "input": "",
							},
						})
						continue
					}
					tcItemID[idx] = "fc_" + compactUUID()
					itemName := st.Name
					item := map[string]any{
						"id": tcItemID[idx], "type": "function_call", "status": "in_progress",
						"call_id": st.ID, "name": itemName, "arguments": "",
					}
					if meta.Namespace != "" {
						item["name"] = meta.Name
						item["namespace"] = meta.Namespace
					}
					emit("response.output_item.added", map[string]any{"output_index": tcIndex[idx], "item": item})
				}
				for _, ad := range argDeltas {
					if tcMeta[ad.idx].Kind == responsesToolCustom {
						continue // The custom tool's JSON arguments are restored to the original input at finalization
					}
					emit("response.function_call_arguments.delta", map[string]any{
						"item_id": tcItemID[ad.idx], "output_index": tcIndex[ad.idx], "delta": ad.arg,
					})
				}
			}
		}
	}
	scanErr := scanner.Err()
	completeTokenStream(body, scanErr == nil && finishReason != "")
	statsObserveUsage(r, usage, scanErr == nil && finishReason != "")
	if scanErr != nil {
		debugEvent(r, "error", "stream_response_failed", map[string]any{
			"error_type": debugErrorType(scanErr),
			"error":      safeDebugError(scanErr),
		})
		// On upstream stream interruption we must not fake response.completed
		// and [DONE]: that would let the downstream treat partial output as a
		// complete result. Instead emit a failed event and return directly,
		// making the incompleteness explicit.
		reason := "Upstream streaming response interrupted; this reply is incomplete"
		if errors.Is(scanErr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("Upstream sent no data for over %v; connection judged stuck and interrupted, this reply is incomplete", upstreamIdleTimeout)
		}
		failed := buildResponsesEnvelope(respID, modelName, created)
		failed["status"] = "failed"
		failed["error"] = map[string]any{"code": "stream_interrupted", "message": reason}
		emit("response.failed", map[string]any{"response": failed})
		log.Printf("[Exception] traceId=%s requestId=%d stage=Upstream streaming read account=%s exception=%v business_impact=This response is incomplete; response.failed was sent instead of faking completion", debugTraceID(r), reqID, acc.Path, scanErr)
		recordModelLatency(modelName, time.Since(startTime))
		return
	}
	if finishReason == "" {
		// Clean EOF: the read error is empty, but there is no non-empty
		// finish_reason. Already-received reasoning must not be stuffed into
		// output_item.done / response.completed — that would let the client
		// treat partial output as a complete result; only a small
		// response.failed is sent.
		debugEvent(r, "error", "stream_closed_without_finish", map[string]any{
			"error_type":      "stream_closed_without_finish",
			"finish_reason":   "",
			"saw_done":        sawDone,
			"reasoning_chars": reasoningSB.Len(),
			"message_chars":   msgSB.Len(),
			"tool_call_count": len(toolOrder),
			"business_impact": "Upstream ended cleanly but without finish_reason; response.completed was rejected in favor of a small response.failed",
		})
		failed := buildResponsesEnvelope(respID, modelName, created)
		failed["status"] = "failed"
		failed["error"] = map[string]any{
			"code":    "stream_closed_without_finish",
			"message": errStreamClosedWithoutFinish.Error(),
		}
		emit("response.failed", map[string]any{"response": failed})
		log.Printf("[Exception] traceId=%s requestId=%d stage=Responses finalize account=%s result=Refused to treat as success reason=Upstream ended cleanly but without finish_reason saw_DONE=%t reasoning_chars_received=%d body_chars_received=%d tool_call_count=%d business_impact=output_item.done and response.completed are not sent; the client receives response.failed handled=yes",
			debugTraceID(r), reqID, acc.Path, sawDone, reasoningSB.Len(), msgSB.Len(), len(toolOrder))
		recordModelLatency(modelName, time.Since(startTime))
		return
	}

	// If there is no output at all, add an empty message so output is non-empty
	// and the event sequence is complete
	if !reasoningOpen && !msgOpen && len(toolOrder) == 0 {
		openMessage()
	}

	// Finalize: reasoning
	if reasoningOpen {
		text := reasoningSB.String()
		emit("response.reasoning_summary_text.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "text": text})
		emit("response.reasoning_summary_part.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}})
		item := map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
		emit("response.output_item.done", map[string]any{"output_index": reasoningIndex, "item": item})
		idxToItem[reasoningIndex] = item
	}

	// Finalize: message
	if msgOpen {
		text := msgSB.String()
		emit("response.output_text.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "text": text})
		emit("response.content_part.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "annotations": []any{}, "text": text}})
		item := map[string]any{"id": msgItemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "text": text}}}
		emit("response.output_item.done", map[string]any{"output_index": msgIndex, "item": item})
		idxToItem[msgIndex] = item
	}

	// Finalize: function calls
	for _, idx := range toolOrder {
		st := toolCalls[idx]
		args := st.Args.String()
		meta := tcMeta[idx]
		if meta.Kind == responsesToolCustom {
			input := customToolInputFromArguments(args)
			emit("response.custom_tool_call_input.delta", map[string]any{
				"item_id": tcItemID[idx], "output_index": tcIndex[idx], "delta": input,
			})
			emit("response.custom_tool_call_input.done", map[string]any{
				"item_id": tcItemID[idx], "output_index": tcIndex[idx], "input": input,
			})
			item := map[string]any{
				"id": tcItemID[idx], "type": "custom_tool_call", "status": "completed",
				"call_id": st.ID, "name": meta.Name, "input": input,
			}
			emit("response.output_item.done", map[string]any{"output_index": tcIndex[idx], "item": item})
			idxToItem[tcIndex[idx]] = item
			continue
		}
		eventName := st.Name
		if meta.Namespace != "" {
			eventName = meta.Name
		}
		emit("response.function_call_arguments.done", map[string]any{"item_id": tcItemID[idx], "output_index": tcIndex[idx], "name": eventName, "arguments": args})
		item := map[string]any{"id": tcItemID[idx], "type": "function_call", "status": "completed", "call_id": st.ID, "name": st.Name, "arguments": args}
		if meta.Namespace != "" {
			item["name"] = meta.Name
			item["namespace"] = meta.Namespace
		}
		emit("response.output_item.done", map[string]any{"output_index": tcIndex[idx], "item": item})
		idxToItem[tcIndex[idx]] = item
	}

	output := make([]any, 0, len(idxToItem))
	for i := 0; i < nextIndex; i++ {
		if item, ok := idxToItem[i]; ok {
			output = append(output, item)
		}
	}

	final := buildResponsesEnvelope(respID, modelName, created)
	final["status"] = "completed"
	final["output"] = output
	final["completed_at"] = time.Now().Unix()
	if usage != nil {
		final["usage"] = toResponsesUsage(usage)
	}
	observeModelCredit(acc, modelName, usage, reqID)
	recordModelTokens(modelName, usage, reqID)
	emit("response.completed", map[string]any{"response": final})

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	if scanErr == nil {
		debugEvent(r, "info", "stream_response_completed", map[string]any{"status_code": http.StatusOK})
	}
	recordModelLatency(modelName, time.Since(startTime))
	log.Printf("[#%d] Responses streaming output completed (account %s [%s], duration %v, first_token(v2) %s)", reqID, acc.Path, prof.Label, time.Since(startTime), body.text())
}

// compactUUID returns a random UUID without hyphens.
func compactUUID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}
