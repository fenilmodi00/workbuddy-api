package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withSystemPrompts(t *testing.T, fallback, force string) {
	t.Helper()
	old := configuredSystemPrompts()
	setSystemPromptConfig(fallback, force)
	t.Cleanup(func() { setSystemPromptConfig(old.fallback, old.force) })
}

func TestSystemPromptConfigurationAndMissingConfig(t *testing.T) {
	old := configuredSystemPrompts()
	oldDebug := cfg.DebugEnabled
	t.Cleanup(func() {
		setSystemPromptConfig(old.fallback, old.force)
		cfg.DebugEnabled = oldDebug
	})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"systemPrompt":{"fallback":"new fallback","force":"global experimental rules"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := configuredSystemPrompts(); got.fallback != "new fallback" || got.force != "global experimental rules" {
		t.Fatalf("config did not take effect: %#v", got)
	}
	if err := loadRuntimeConfig(path + ".missing"); err != nil {
		t.Fatal(err)
	}
	if got := configuredSystemPrompts(); got.fallback != "" || got.force != "" {
		t.Fatalf("missing config file should reset to empty: %#v", got)
	}
}

func TestFallbackPromptOnlyChangesMissingSystem(t *testing.T) {
	withSystemPrompts(t, "custom fallback", "")
	userOnly := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(userOnly, httptest.NewRequest("POST", "/v1/chat/completions", nil), 1, "test-trace")
	msgs := userOnly["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "custom fallback" {
		t.Fatalf("configured fallback prompt was not used: %#v", msgs)
	}
	original := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "client rules"}, map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(original, httptest.NewRequest("POST", "/v1/chat/completions", nil), 2, "test-trace")
	if got := original["messages"].([]any)[0].(map[string]any)["content"]; got != "client rules" {
		t.Fatalf("existing system must not be overwritten by the config fallback: %#v", got)
	}
}

func TestForcedGlobalPromptPreservesExistingSystem(t *testing.T) {
	withSystemPrompts(t, "", "global experimental rules")
	obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "client rules"}, map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 3, "test-trace")
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "client rules\n\nglobal experimental rules" {
		t.Fatalf("global force text not appended to the end of the client system, or the original text was altered: %#v", msgs)
	}
	obj = map[string]any{"messages": []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "original content block"}}}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/responses", nil), 4, "test-trace")
	parts := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "original content block" || parts[1].(map[string]any)["text"] != "global experimental rules" {
		t.Fatalf("global force text should be appended as the last content block without dropping the original system: %#v", parts)
	}
}

func TestEmptyPromptConfigPreservesLegacyBehavior(t *testing.T) {
	withSystemPrompts(t, "  ", "\n\t")
	obj := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 5, "test-trace")
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != defaultSystemPrompt {
		t.Fatalf("empty config must use the original fallback text: %#v", got)
	}
}

func TestSystemPromptConfigurationReachesChatAndResponses(t *testing.T) {
	withSystemPrompts(t, "custom fallback", "unified rules")
	chat := captureUpstreamMessages(t, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if got := chat[0].(map[string]any)["content"]; got != "custom fallback\n\nunified rules" {
		t.Fatalf("chat without system should keep the fallback and append the global rules at the end: %#v", got)
	}
	responses := captureUpstreamMessages(t, "/v1/responses", `{"model":"m","stream":true,"instructions":"client-provided system","input":"hi"}`)
	if got := responses[0].(map[string]any)["content"]; got != "client-provided system\n\nunified rules" {
		t.Fatalf("responses should preserve instructions and append the global rules at the end: %#v", got)
	}
	if strings.Contains(chat[1].(map[string]any)["content"].(string), "unified rules") {
		t.Fatal("global rules must not be written into the user message twice")
	}
}

func TestSystemPromptConfigurationReachesMessages(t *testing.T) {
	withSystemPrompts(t, "custom fallback", "unified rules")
	for _, tc := range []struct {
		name, body, want string
	}{
		{"fallback", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "custom fallback\n\nunified rules"},
		{"client_system", `{"model":"m","system":"client rules","messages":[{"role":"user","content":"hi"}]}`, "client rules\n\nunified rules"},
		{"system_blocks", `{"model":"m","system":[{"type":"text","text":"rule one"},{"type":"text","text":"rule two"}],"messages":[{"role":"user","content":"hi"}]}`, "rule one\nrule two\n\nunified rules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := captureUpstreamMessages(t, "/v1/messages", tc.body)
			if got := messages[0].(map[string]any)["content"]; got != tc.want {
				t.Fatalf("messages prompt not applied uniformly: got=%#v want=%q", got, tc.want)
			}
			if got := messages[1].(map[string]any)["content"]; got != "hi" {
				t.Fatalf("user content must not be modified: %#v", got)
			}
		})
	}
}

// TestForcedPromptIsAppendedAfterClientSystem pins the contract "client text first, forced prompt after".
//
// This order directly determines the practical effect on conflict: the gateway sends a single system message whose content is
// "<client's own system prompt>\n\n<forced text>". The forced text sits at the very end of the whole prompt,
// immediately preceding the user message, making it the "most recent" instruction, so persona and style instructions usually carry the most influence.
// This is not a priority field; it is ordering within the same text.
func TestForcedPromptIsAppendedAfterClientSystem(t *testing.T) {
	withSystemPrompts(t, "", "[FORCED-APPEND]")
	body := `{"model":"m","stream":true,"messages":[{"role":"system","content":"[CLIENT-OWN-SYSTEM-PROMPT]"},{"role":"user","content":"hi"}]}`
	messages := captureUpstreamMessages(t, "/v1/chat/completions", body)
	content, ok := messages[0].(map[string]any)["content"].(string)
	if !ok {
		t.Fatalf("system content should be a string: %#v", messages[0])
	}
	forcedAt := strings.Index(content, "[FORCED-APPEND]")
	clientAt := strings.Index(content, "[CLIENT-OWN-SYSTEM-PROMPT]")
	if clientAt != 0 {
		t.Fatalf("client text should be at the very front of the system content: %q", content)
	}
	if forcedAt <= clientAt {
		t.Fatalf("forced text should come after the client text: %q", content)
	}
	// The forced text must close the content, i.e. sit at the very end of the system content, immediately preceding the user message.
	if !strings.HasSuffix(content, "[FORCED-APPEND]") {
		t.Fatalf("forced text should be at the very end of the system content: %q", content)
	}
	// The two are separated by a blank line, keeping it a single valid system message.
	if !strings.Contains(content, "[CLIENT-OWN-SYSTEM-PROMPT]\n\n[FORCED-APPEND]") {
		t.Fatalf("concatenation format changed: %q", content)
	}
}
