package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

// promptSettings is the optional prompts in the config file. The zero value fully preserves the old behavior.
// Logs record only the source and length; the prompt content from config or the client is never logged.
type promptSettings struct {
	fallback string
	force    string
}

var (
	promptSettingsMu sync.RWMutex
	currentPrompts   promptSettings
)

func setSystemPromptConfig(fallback, force string) {
	promptSettingsMu.Lock()
	currentPrompts = promptSettings{fallback: fallback, force: force}
	promptSettingsMu.Unlock()
}

func configuredSystemPrompts() promptSettings {
	promptSettingsMu.RLock()
	settings := currentPrompts
	promptSettingsMu.RUnlock()
	return settings
}

func configuredFallbackSystemPrompt() string {
	fallback := configuredSystemPrompts().fallback
	if strings.TrimSpace(fallback) == "" {
		return defaultSystemPrompt
	}
	return fallback
}

// prepareSystemPromptForUpstream runs uniformly at the Chat, Responses, and Messages entry points.
// It first ensures the first message is a system message, then appends the experimental global force text to the end of that system content (post-positioned),
// making it the last instruction in the whole prompt, immediately preceding the user message;
// the client's original system is neither deleted nor overwritten. The content array keeps the original content-block order,
// with the force text appended as the last content block at the end of the array.
func prepareSystemPromptForUpstream(obj map[string]any, r *http.Request, requestID uint64, traceID string) {
	injected := ensureLeadingSystemMessage(obj)
	settings := configuredSystemPrompts()
	force := settings.force
	forced := strings.TrimSpace(force) != ""
	kind := "unmodified"
	messages, _ := obj["messages"].([]any)
	if injected && len(messages) > 0 {
		applyIdentityReplacements(map[string]any{"messages": []any{messages[0]}}, r, requestID)
	}
	if forced {
		forceMessage := map[string]any{"role": "system", "content": force}
		applyIdentityReplacements(map[string]any{"messages": []any{forceMessage}}, r, requestID)
		force, _ = forceMessage["content"].(string)
	}
	if forced && len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok {
			old := first["content"]
			switch content := old.(type) {
			case string:
				kind = "string"
				if content == "" {
					first["content"] = force
				} else {
					first["content"] = content + "\n\n" + force
				}
			case []any:
				kind = "content-block array"
				blocks := make([]any, 0, len(content)+1)
				blocks = append(blocks, content...)
				blocks = append(blocks, map[string]any{"type": "text", "text": force})
				first["content"] = blocks
			case nil:
				kind = "empty content"
				first["content"] = force
			default:
				// Invalid system content is still validated by upstream parameter checks; client data is not silently dropped.
				forced = false
				kind = "unsupported content type, force-append skipped"
			}
		}
	}
	fallbackSource := "not used (client provides system)"
	if injected {
		fallbackSource = "built-in fallback"
		if strings.TrimSpace(settings.fallback) != "" {
			fallbackSource = "config fallback"
		}
	}
	if traceID == "" {
		traceID = debugTraceID(r)
	}
	if traceID == "" {
		traceID = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[System prompt policy] traceId=%s requestId=%d stage=before-upstream-request-serialization fallback-source=%s forced-global-applied=%t forced-position=append-after-client-system forced-content-kind=%s forced-char-count=%d result=client original system preserved and prompt content never logged",
		traceID, requestID, fallbackSource, forced, kind, utf8.RuneCountInString(force))
	debugEvent(r, "debug", "system_prompt_policy_applied", map[string]any{
		"fallback_source": fallbackSource, "forced_applied": forced,
		"forced_position": "append_after_client_system", "forced_content_kind": kind,
		"forced_chars":    utf8.RuneCountInString(force),
		"business_impact": "processes system before the request leaves, per config; force text appended after the system content and prompt content never logged",
	})
}
