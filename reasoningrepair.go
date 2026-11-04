package main

import (
	"log"
	"net/http"
	"strings"
)

// reasoningRepairReport counts field shapes and quantities only; it never saves or logs chain-of-thought content.
type reasoningRepairReport struct {
	Applied                bool
	ThinkingEnabled        bool
	HasTrace               bool
	AssistantMessages      int
	PreservedContent       int
	CopiedFromReasoning    int
	EmptyContentAdded      int
	InvalidContentReplaced int
	ReasoningMirrored      int
	ReasoningPlaceholders  int
}

// repairReasoningHistory applies one shared repair to Chat message shapes before they are sent upstream.
// Both entry points must be called after tool_call sequence repair and before json.Marshal:
// native Chat input and Responses-converted input therefore follow the same set of DeepSeek thinking rules.
// Existing reasoning strings are never overwritten; assistant turns that received no reasoning only get an empty
// reasoning_content added, and in shapes where upstream requires a non-empty reasoning field, reasoning gets a
// blank placeholder instead of fabricating a chain of thought.
func repairReasoningHistory(obj map[string]any) reasoningRepairReport {
	var report reasoningRepairReport
	model, _ := obj["model"].(string)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return report
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return report
	}

	report.ThinkingEnabled = thinkingExplicitlyEnabled(obj)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		if _, exists := msg["reasoning_content"]; exists {
			report.HasTrace = true
			break
		}
		if reasoning, ok := msg["reasoning"].(string); ok && reasoning != "" {
			report.HasTrace = true
			break
		}
	}
	// Leave history untouched when thinking is explicitly disabled and there is no existing reasoning trace.
	// Disabling subsequent thinking does not mean erasing reasoning already produced: traces must still be
	// replayed to avoid inconsistency across multi-turn context.
	if !report.ThinkingEnabled && !report.HasTrace {
		return report
	}
	report.Applied = true
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		report.AssistantMessages++
		rc, isString := msg["reasoning_content"].(string)
		if isString {
			report.PreservedContent++
		} else {
			if _, exists := msg["reasoning_content"]; exists {
				report.InvalidContentReplaced++
			}
			if original, ok := msg["reasoning"].(string); ok {
				rc = original
				report.CopiedFromReasoning++
			} else {
				rc = ""
				report.EmptyContentAdded++
			}
			msg["reasoning_content"] = rc
		}
		if existing, ok := msg["reasoning"].(string); ok && existing != "" {
			continue
		}
		if rc != "" {
			msg["reasoning"] = rc
			report.ReasoningMirrored++
		} else {
			// Aligns with the len(reasoning)>0 validation some upstream tenants enforce; a single space is not reasoning content.
			msg["reasoning"] = " "
			report.ReasoningPlaceholders++
		}
	}
	return report
}

// thinkingExplicitlyEnabled only recognizes thinking signals the client has explicitly turned on; it never enables
// thinking for the user and never changes existing effort or thinking.type. Explicit disabled takes priority over other fields.
func thinkingExplicitlyEnabled(obj map[string]any) bool {
	if thinking, ok := obj["thinking"].(map[string]any); ok {
		switch strings.ToLower(strings.TrimSpace(stringField(thinking, "type"))) {
		case "disabled":
			return false
		case "enabled":
			return true
		}
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "reasoning_summary"} {
		if enabledEffort(stringField(obj, key)) {
			return true
		}
	}
	if reasoning, ok := obj["reasoning"].(map[string]any); ok {
		return enabledEffort(stringField(reasoning, "effort"))
	}
	return false
}

func stringField(obj map[string]any, key string) string {
	value, _ := obj[key].(string)
	return value
}

func enabledEffort(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "off", "disabled":
		return false
	default:
		return true
	}
}

// logReasoningHistoryRepair reuses the gateway's existing file log/TraceID; sensitive content is never logged.
func logReasoningHistoryRepair(r *http.Request, reqID uint64, model string, report reasoningRepairReport) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return
	}
	result := "skipped (thinking disabled and no history trace)"
	if report.Applied {
		result = "checked and completed assistant history"
	}
	log.Printf("[ReasoningHistoryRepair] traceId=%s requestId=%d model=%s result=%s thinkingEnabled=%t historyHasTrace=%t assistantMessages=%d preservedReasoning=%d copiedFromReasoning=%d emptyReasoningContentAdded=%d nonStringNormalized=%d reasoningMirrored=%d reasoningBlankPlaceholders=%d businessImpact=avoids DeepSeek long-session 11155 without logging reasoning content",
		debugTraceID(r), reqID, model, result, report.ThinkingEnabled, report.HasTrace,
		report.AssistantMessages, report.PreservedContent, report.CopiedFromReasoning,
		report.EmptyContentAdded, report.InvalidContentReplaced,
		report.ReasoningMirrored, report.ReasoningPlaceholders)
	debugEvent(r, "debug", "reasoning_history_repaired", map[string]any{
		"result": result, "thinking_enabled": report.ThinkingEnabled, "has_trace": report.HasTrace,
		"assistant_messages": report.AssistantMessages, "preserved_content": report.PreservedContent,
		"copied_from_reasoning": report.CopiedFromReasoning, "empty_content_added": report.EmptyContentAdded,
		"invalid_content_replaced": report.InvalidContentReplaced,
		"reasoning_mirrored":       report.ReasoningMirrored,
		"reasoning_placeholders":   report.ReasoningPlaceholders,
		"business_impact":          "reasoning history field shapes validated; prompts and reasoning content are not saved",
	})
}
