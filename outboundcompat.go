package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

const (
	requestProfileLegacy   = "legacy"
	requestProfileOfficial = "official"
	// Fixed to the 2026-10-21 domestically captured desktop version; does not claim to follow the latest official site.
	observedDesktopUA = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
)

var officialRequests atomic.Bool

type requestProfileContextKey struct{}

func normalizeRequestProfile(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", requestProfileLegacy:
		return requestProfileLegacy, nil
	case requestProfileOfficial:
		return requestProfileOfficial, nil
	default:
		return "", fmt.Errorf("upstream.requestProfile only supports legacy or official")
	}
}

func setRequestProfile(value string) {
	profile, _ := normalizeRequestProfile(value) // The whole config has already been validated before applying.
	officialRequests.Store(profile == requestProfileOfficial)
}

// Snapshot the profile once per logical request so hot reload cannot make body, headers, and retries use different configs.
func requestProfileFor(r *http.Request) string {
	if r != nil {
		if value, ok := r.Context().Value(requestProfileContextKey{}).(string); ok {
			return value
		}
	}
	profile := requestProfileLegacy
	if officialRequests.Load() {
		profile = requestProfileOfficial
	}
	if r != nil {
		*r = *r.WithContext(context.WithValue(r.Context(), requestProfileContextKey{}, profile))
	}
	return profile
}

// All three entry points share this after protocol translation. Do not promote user roles, or replace client instructions or tools.
func prepareUpstreamChatRequest(obj map[string]any, r *http.Request, requestID uint64, model string) {
	profile := requestProfileFor(r)
	trace := debugTraceID(r)
	applyThinkingRules(obj, model)
	normalizeDeveloperRoles(obj, r, requestID)
	// The leading-wildcard rule runs once on the client's original blocks, not across protocol-merged text block boundaries.
	applyOriginalIdentityReplacements(obj, r, requestID)
	if profile == requestProfileOfficial {
		mergeLeadingInstructionMessages(obj, r, requestID)
	}
	prepareSystemPromptForUpstream(obj, r, requestID, trace)
	applyIdentityRulePass(obj, r, requestID, false)
	ensureUpstreamMaxTokens(obj, r, requestID, trace, model)
	if profile == requestProfileOfficial {
		applyOfficialBodyDefaults(obj, r, requestID, model)
	}
	report := repairToolMessageSequence(obj)
	logToolSequenceRepair(r, trace, requestID, model, report)
	logReasoningHistoryRepair(r, requestID, model, repairReasoningHistory(obj))
}

func applyOfficialBodyDefaults(obj map[string]any, r *http.Request, id uint64, model string) {
	var added []string
	put := func(key string, value any) {
		if old, exists := obj[key]; !exists || old == nil {
			obj[key] = value
			added = append(added, key)
		}
	}
	// Chat's old and new output budget fields are equivalent; convert only when max_tokens is absent, and never override conflicting values.
	if value := obj["max_completion_tokens"]; value != nil && obj["max_tokens"] == nil {
		obj["max_tokens"] = value
		delete(obj, "max_completion_tokens")
		added = append(added, "max_completion_tokens->max_tokens")
	}
	// The upstream always uses SSE; a separate usage event helps all three downstream protocols get the same billing data.
	if value, exists := obj["stream_options"]; !exists || value == nil {
		obj["stream_options"] = map[string]any{"include_usage": true}
		added = append(added, "stream_options.include_usage")
	} else if options, ok := value.(map[string]any); ok {
		if old, exists := options["include_usage"]; !exists || old == nil {
			options["include_usage"] = true
			added = append(added, "stream_options.include_usage")
		}
	}
	// Only this model has a complete desktop sample. Do not guess temperature/verbosity support for other models.
	if normalizeModelName(model) == "deepseek-v4.1-flash" {
		if !clientDeclaredOutputBudget(obj) {
			put("max_tokens", 128000) // Use the verified sample output cap while the model catalog is not ready.
		}
		if obj["top_p"] == nil {
			put("temperature", 1)
		}
		put("verbosity", "high")
	}
	sort.Strings(added)
	log.Printf("[Outbound compat] traceId=%s requestId=%d profile=official stage=body-preparation model=%s added-fields=%v result=completed note=keeps explicit effort, user roles, original text, and tools; does not copy the official private prompt",
		debugTraceID(r), id, model, added)
	debugEvent(r, "debug", "official_request_body_prepared", map[string]any{
		"profile": requestProfileOfficial, "defaulted_fields": added,
		"reasoning_effort": obj["reasoning_effort"],
		"business_impact":  "applies verified default fields across the three entry points; does not force high to max, does not promote user instructions",
	})
}

// Merges only consecutive leading plain-text system messages without extra metadata. user, images, cache_control,
// system messages inserted mid-conversation, and other fields are never guessed or discarded; forced prompts are appended after merging.
func mergeLeadingInstructionMessages(obj map[string]any, r *http.Request, id uint64) {
	messages, _ := obj["messages"].([]any)
	var texts []string
	count := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || roleOfMessage(message) != "system" || len(message) != 2 {
			break
		}
		var parts []string
		switch content := message["content"].(type) {
		case string:
			parts = []string{content}
		case []any:
			for _, raw := range content {
				block, ok := raw.(map[string]any)
				if !ok || len(block) != 2 || block["type"] != "text" {
					parts = nil
					break
				}
				text, ok := block["text"].(string)
				if !ok {
					parts = nil
					break
				}
				parts = append(parts, text)
			}
		}
		if parts == nil {
			break
		}
		texts = append(texts, parts...)
		count++
	}
	if count == 0 {
		return
	}
	first := messages[0].(map[string]any)
	_, wasString := first["content"].(string)
	if count == 1 && wasString {
		return
	}
	first["content"] = strings.Join(texts, "\n\n")
	out := make([]any, 0, len(messages)-count+1)
	out = append(out, first)
	out = append(out, messages[count:]...)
	obj["messages"] = out
	log.Printf("[Outbound compat] traceId=%s requestId=%d stage=instruction-structure result=merged consecutive leading plain-text system merged-message-count=%d note=keeps text order, does not cross user boundary, does not log body", debugTraceID(r), id, count)
	debugEvent(r, "debug", "official_instruction_messages_merged", map[string]any{
		"messages_merged": count, "business_impact": "merges only explicitly labeled system consecutive plain text; does not promote user roles",
	})
}

func usesObservedDesktopHeaders(profile string, prof *upstreamProfile) bool {
	return profile == requestProfileOfficial && prof.Key == "cn"
}

// Only for model requests. Login/refresh/quota endpoints keep the original authentication logic.
// Does not fake X-Device-Token, SDK/Node runtime info, or other client credentials that cannot be obtained.
func applyObservedDesktopHeaders(req *http.Request, prof *upstreamProfile, profile string) {
	if !usesObservedDesktopHeaders(profile, prof) || req.Method != http.MethodPost || req.URL.Path != "/v2/chat/completions" {
		return
	}
	for _, name := range []string{
		"Origin", "Referer", "X-Client-ID", "X-Client-Version", "X-Refresh-Token",
		"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info",
	} {
		req.Header.Del(name)
	}
	// Static client identity captured from a domestic desktop on 2026-10-21. Device tokens, user identity, and session
	// identifiers still come from the real account/session; X-Device-Token that cannot be obtained is not faked.
	for name, value := range map[string]string{
		"Accept":                      "application/json",
		"User-Agent":                  observedDesktopUA,
		"X-Requested-With":            "XMLHttpRequest",
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "Windows",
		"x-stainless-package-version": "6.25.0",
		"x-stainless-retry-count":     "0",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.21.1",
		"X-IDE-Type":                  "WorkBuddy",
		"X-IDE-Name":                  "WorkBuddy",
		"X-IDE-Version":               "5.8.3",
		"X-Private-Data":              "true",
		"X-Codebuddy-Request":         "1",
		"X-Agent-Intent":              "craft",
		"X-Agent-Purpose":             "conversation",
		"X-Agent-Type":                "main",
		"X-Product":                   "SaaS",
	} {
		req.Header.Set(name, value)
	}
	// Keep message/request IDs identical for the same logical request; retries reuse them, and the local audit TraceID is unchanged.
	message := strings.ReplaceAll(req.Header.Get(conversationMessageIDHeader), "-", "")
	req.Header.Set(conversationMessageIDHeader, message)
	req.Header.Set("X-Request-ID", message)
}

func encodeModelRequest(raw []byte, profile string, prof *upstreamProfile) ([]byte, error) {
	if !usesObservedDesktopHeaders(profile, prof) {
		return raw, nil
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
