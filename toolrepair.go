package main

import (
	"log"
	"net/http"
	"strings"
)

type toolSequenceRepairReport struct {
	OriginalMessages   int
	FinalMessages      int
	ParallelBatches    int
	MovedMessages      int
	MergedCallMessages int
	DroppedCalls       int
	DroppedOutputs     int
	TopologyBefore     string
	TopologyAfter      string
}

func (r toolSequenceRepairReport) changed() bool {
	return r.OriginalMessages != r.FinalMessages || r.MovedMessages > 0 || r.MergedCallMessages > 0 ||
		r.DroppedCalls > 0 || r.DroppedOutputs > 0
}

// repairToolMessageSequence repairs a modern tool_calls/tool sequence before it is sent upstream.
//
// Rules:
//   - First, symmetric pruning by tool call ID: calls without results, results without calls, and duplicates are all removed;
//   - Only when a batch contains at least two calls, ordinary messages wedged between calls and results are moved after the results;
//   - Consecutive/interleaved single-call assistant messages produced by Responses API conversion are merged into one parallel batch;
//   - Results keep their original appearance order and are not reordered by call order.
func repairToolMessageSequence(obj map[string]any) toolSequenceRepairReport {
	messages, ok := obj["messages"].([]any)
	report := toolSequenceRepairReport{OriginalMessages: len(messages), FinalMessages: len(messages)}
	if !ok || len(messages) == 0 {
		return report
	}
	// The topology signature must be captured before in-place modification (the filtering stage rewrites assistant messages' tool_calls).
	topologyBefore := toolMessageTopology(messages)

	// Keep only the first call per ID, and the first result located after that call.
	callMessageIndex := make(map[string]int)
	callEntryIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOfMessage(message) != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for entryIndex, callAny := range calls {
			id := toolCallID(callAny)
			if id == "" {
				continue
			}
			if _, exists := callMessageIndex[id]; !exists {
				callMessageIndex[id] = messageIndex
				callEntryIndex[id] = entryIndex
			}
		}
	}

	outputMessageIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOfMessage(message) != "tool" {
			continue
		}
		id := toolOutputID(message)
		callIndex, exists := callMessageIndex[id]
		if !exists || messageIndex <= callIndex {
			continue
		}
		if _, exists := outputMessageIndex[id]; !exists {
			outputMessageIndex[id] = messageIndex
		}
	}

	pairedIDs := make(map[string]bool, len(outputMessageIndex))
	for id := range outputMessageIndex {
		pairedIDs[id] = true
	}

	filtered := make([]any, 0, len(messages))
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok {
			filtered = append(filtered, messageAny)
			continue
		}

		switch roleOfMessage(message) {
		case "assistant":
			calls, hasCalls := message["tool_calls"].([]any)
			if !hasCalls || len(calls) == 0 {
				filtered = append(filtered, messageAny)
				continue
			}
			kept := make([]any, 0, len(calls))
			for entryIndex, callAny := range calls {
				id := toolCallID(callAny)
				if id != "" && pairedIDs[id] && callMessageIndex[id] == messageIndex && callEntryIndex[id] == entryIndex {
					kept = append(kept, callAny)
				} else {
					report.DroppedCalls++
				}
			}
			if len(kept) > 0 {
				message["tool_calls"] = kept
				filtered = append(filtered, messageAny)
				continue
			}
			delete(message, "tool_calls")
			if assistantMessageHasPayload(message) {
				filtered = append(filtered, messageAny)
			}

		case "tool":
			id := toolOutputID(message)
			if id != "" && pairedIDs[id] && outputMessageIndex[id] == messageIndex {
				filtered = append(filtered, messageAny)
			} else {
				report.DroppedOutputs++
			}

		default:
			filtered = append(filtered, messageAny)
		}
	}

	repaired := repairParallelToolBatches(filtered, &report)
	obj["messages"] = repaired
	report.FinalMessages = len(repaired)
	if report.changed() {
		report.TopologyBefore = topologyBefore
		report.TopologyAfter = toolMessageTopology(repaired)
	}
	return report
}

// toolMessageTopology builds a bounded message topology signature: roles and tool call IDs only, no body content.
// It records at most the first 40 entries, so the tool sequence structure can be checked on real requests without leaking prompts or tool results.
func toolMessageTopology(messages []any) string {
	const maxEntries = 200
	parts := make([]string, 0, maxEntries)
	for i, messageAny := range messages {
		if i >= maxEntries {
			parts = append(parts, "...")
			break
		}
		message, ok := messageAny.(map[string]any)
		if !ok {
			parts = append(parts, "?")
			continue
		}
		role := roleOfMessage(message)
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				ids := make([]string, 0, len(calls))
				for _, callAny := range calls {
					ids = append(ids, toolCallID(callAny))
				}
				parts = append(parts, "assistant["+strings.Join(ids, ",")+"]")
				continue
			}
		}
		if role == "tool" {
			parts = append(parts, "tool["+toolOutputID(message)+"]")
			continue
		}
		parts = append(parts, role)
	}
	return strings.Join(parts, " > ")
}

func repairParallelToolBatches(messages []any, report *toolSequenceRepairReport) []any {
	out := make([]any, 0, len(messages))
	for i := 0; i < len(messages); {
		first, firstCalls, ok := assistantCalls(messages[i])
		if !ok {
			out = append(out, messages[i])
			i++
			continue
		}

		batchCalls := append([]any(nil), firstCalls...)
		deferred := make([]any, 0)
		mergedMessages := 0
		j := i + 1
		for j < len(messages) {
			if next, calls, hasCalls := assistantCalls(messages[j]); hasCalls {
				if !assistantCallOnly(next) {
					break
				}
				batchCalls = append(batchCalls, calls...)
				mergedMessages++
				j++
				continue
			}
			if message, ok := messages[j].(map[string]any); ok && roleOfMessage(message) == "tool" {
				break
			}
			deferred = append(deferred, messages[j])
			j++
		}

		if len(batchCalls) < 2 {
			out = append(out, messages[i])
			i++
			continue
		}

		ids := make(map[string]bool, len(batchCalls))
		for _, callAny := range batchCalls {
			if id := toolCallID(callAny); id != "" {
				ids[id] = true
			}
		}
		outputs := make([]any, 0, len(ids))
		found := make(map[string]bool, len(ids))
		k := j
		for k < len(messages) && len(found) < len(ids) {
			if _, _, nextBatch := assistantCalls(messages[k]); nextBatch {
				break
			}
			if message, ok := messages[k].(map[string]any); ok && roleOfMessage(message) == "tool" {
				id := toolOutputID(message)
				if ids[id] && !found[id] {
					outputs = append(outputs, messages[k])
					found[id] = true
					k++
					continue
				}
			}
			deferred = append(deferred, messages[k])
			k++
		}

		if len(found) != len(ids) {
			out = append(out, messages[i])
			i++
			continue
		}

		first["tool_calls"] = batchCalls
		out = append(out, first)
		out = append(out, outputs...)
		out = append(out, deferred...)
		report.ParallelBatches++
		report.MovedMessages += len(deferred)
		report.MergedCallMessages += mergedMessages
		i = k
	}
	return out
}

func assistantCalls(messageAny any) (map[string]any, []any, bool) {
	message, ok := messageAny.(map[string]any)
	if !ok || roleOfMessage(message) != "assistant" {
		return nil, nil, false
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return nil, nil, false
	}
	return message, calls, true
}

func assistantCallOnly(message map[string]any) bool {
	if assistantMessageHasPayload(message) {
		return false
	}
	for key := range message {
		switch key {
		case "role", "content", "tool_calls":
		default:
			return false
		}
	}
	return true
}

func assistantMessageHasPayload(message map[string]any) bool {
	content, exists := message["content"]
	if !exists || content == nil {
		return false
	}
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	default:
		return true
	}
}

func toolCallID(callAny any) string {
	call, ok := callAny.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := call["id"].(string)
	return strings.TrimSpace(id)
}

func toolOutputID(message map[string]any) string {
	id, _ := message["tool_call_id"].(string)
	return strings.TrimSpace(id)
}

func logToolSequenceRepair(r *http.Request, traceID string, requestID uint64, model string, report toolSequenceRepairReport) {
	result := "unchanged"
	if report.changed() {
		result = "repaired"
		log.Printf("[ToolSequenceRepair] traceId=%s requestId=%d model=%s result=repaired originalMessages=%d finalMessages=%d parallelBatches=%d movedInsertedMessages=%d mergedCallMessages=%d droppedCallsWithoutResult=%d droppedOrphanOrDuplicateResults=%d businessImpact=avoids upstream 11148 tool call sequence break",
			traceID, requestID, model, report.OriginalMessages, report.FinalMessages, report.ParallelBatches,
			report.MovedMessages, report.MergedCallMessages, report.DroppedCalls, report.DroppedOutputs)
	}
	if !debugLoggingEnabled() {
		return
	}
	fields := map[string]any{
		"result":                       result,
		"original_messages":            report.OriginalMessages,
		"final_messages":               report.FinalMessages,
		"parallel_batches":             report.ParallelBatches,
		"moved_messages":               report.MovedMessages,
		"merged_call_messages":         report.MergedCallMessages,
		"dropped_calls_without_output": report.DroppedCalls,
		"dropped_orphan_outputs":       report.DroppedOutputs,
		"business_impact":              "outbound tool call sequence validated, prevents upstream 11148 error",
	}
	if report.changed() {
		fields["topology_before"] = report.TopologyBefore
		fields["topology_after"] = report.TopologyAfter
	}
	debugEvent(r, "debug", "tool_sequence_checked", fields)
}
