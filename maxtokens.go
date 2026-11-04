package main

import (
	"log"
	"net/http"
)

// Upstream decides whether the model window is exceeded by "input tokens + output
// budget": when a request carries no max_tokens, upstream reserves 384000 by default
// for output, leaving deepseek-v4.1-flash's nominal 1048576 window with only about
// 664576 usable input. Long sessions get rejected with 400 code=11133
// (model_param_invalid; the error text does not name the specific parameter) well
// before the window ceiling, appearing as "400 out of nowhere mid-conversation".
//
// Measured (2026-10-10, deepseek-v4.1-flash, window 1048576):
//
//	678912 input + no declared output budget  -> 400 code=11133
//	678912 input + max_tokens=128000          -> 200
//	678912 input + max_tokens=400000          -> 400 code=11133
//
// Therefore, only when the client declared no output budget, add the output ceiling
// declared in the model catalog to reclaim the quota reserved by default. Values the
// client passes explicitly are never overridden; models missing from the catalog are
// left as-is, with no guessing.
func ensureUpstreamMaxTokens(obj map[string]any, r *http.Request, requestID uint64, traceID, modelName string) {
	if clientDeclaredOutputBudget(obj) {
		return
	}
	limit := modelMaxOutputTokens(modelName)
	if limit <= 0 {
		return
	}
	obj["max_tokens"] = limit
	if traceID == "" {
		traceID = debugTraceID(r)
	}
	if traceID == "" {
		traceID = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[Output budget] traceId=%s requestId=%d model=%s stage=before upstream request serialization client declared output budget=false result=max_tokens injected=%d business impact=reclaims output quota reserved by upstream default, preventing 11133 rejections on long sessions",
		traceID, requestID, modelName, limit)
	debugEvent(r, "debug", "max_tokens_budget_injected", map[string]any{
		"model": modelName, "injected_max_tokens": limit,
		"business_impact": "client declared no output budget; the gateway injects the output ceiling from the model catalog to reclaim upstream default reserved quota; explicit client values are never overridden",
	})
}

// clientDeclaredOutputBudget decides whether the client has declared an output budget.
// max_tokens of null counts as undeclared; max_completion_tokens is the equivalent
// newer field — as long as either carries a valid value, the request is left untouched.
func clientDeclaredOutputBudget(obj map[string]any) bool {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if value, ok := obj[key]; ok && value != nil {
			return true
		}
	}
	return false
}

// modelMaxOutputTokens returns the maximum output token count declared in the site catalogs for the model.
// Returns 0 when the model is not in any catalog; the caller should leave the request untouched.
func modelMaxOutputTokens(modelID string) int {
	modelID = normalizeModelName(modelID)
	if modelID == "" {
		return 0
	}
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	best := 0
	for _, site := range catalogSites {
		for _, m := range catalogModels[site] {
			if m.ID == modelID && m.MaxOutputTokens > best {
				best = m.MaxOutputTokens
			}
		}
	}
	return best
}
