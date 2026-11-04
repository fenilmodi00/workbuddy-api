package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// WorkBuddy 5.8.4's buildConversationHeaders uses session.id, the current turn
// ID, and a UUID generated per model call (message ID without hyphens). Do not
// substitute an ordinary TraceID for the session.
const (
	conversationIDHeader        = "X-Conversation-ID"
	conversationRequestIDHeader = "X-Conversation-Request-ID"
	conversationMessageIDHeader = "X-Conversation-Message-ID"
)

type conversationContextKey struct{}

type conversationHeaders struct {
	ID        string
	RequestID string
	MessageID string
	Source    string
}

// Client session keys that are not UUIDs are normalized to UUIDv5; arbitrary
// client text is never sent or logged. The namespace is fixed and separate from
// the standard DNS/URL namespaces; the API key only participates in the digest
// and is never logged.
var conversationNamespace = uuid.MustParse("7518352d-f297-5ede-9583-a32f3413b74d")

func usableConversationSeed(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func normalizedConversationID(value, kind, caller string) string {
	if id, err := uuid.Parse(value); err == nil && id != uuid.Nil {
		// Preserve the 32-char hyphen-free format used by the official main
		// path; other UUID spellings normalize to the standard format.
		if len(value) == 32 {
			return strings.ToLower(value)
		}
		return id.String()
	}
	return uuid.NewSHA1(conversationNamespace, []byte(caller+"\x00"+kind+"\x00"+value)).String()
}

func conversationHeadersFor(r *http.Request) *conversationHeaders {
	if r == nil {
		return nil
	}
	value, _ := r.Context().Value(conversationContextKey{}).(*conversationHeaders)
	return value
}

// prepareConversationContext must read the original request's conversation
// metadata before protocol conversion. After conversion the same request
// context is still used: all three protocols, streaming/aggregate, and
// account-switch/retry share the same set of headers. When no client session
// key exists, each HTTP call generates its own; sessions are never inferred or
// merged from prompt/model names.
func prepareConversationContext(r *http.Request, body map[string]any) *conversationHeaders {
	if existing := conversationHeadersFor(r); existing != nil {
		return existing
	}
	h := &conversationHeaders{
		ID: uuid.NewString(), RequestID: uuid.NewString(),
		MessageID: strings.ReplaceAll(uuid.NewString(), "-", ""), Source: "independently generated",
	}
	if r == nil {
		return h
	}
	caller := apiKeyFingerprint(r.Header.Get("Authorization"))
	if caller == "" {
		caller = apiKeyFingerprint(r.Header.Get("x-api-key"))
	}
	metadata, _ := body["metadata"].(map[string]any)
	bodyValue := func(object map[string]any, names ...string) string {
		for _, name := range names {
			if value, ok := object[name].(string); ok {
				if seed := usableConversationSeed(value); seed != "" {
					return seed
				}
			}
		}
		return ""
	}
	seed := usableConversationSeed(r.Header.Get(conversationIDHeader))
	if seed != "" {
		h.Source = "client conversation header"
	} else if seed = usableConversationSeed(r.Header.Get("X-Session-ID")); seed != "" {
		h.Source = "client session header"
	} else if seed = bodyValue(body, "conversation_id", "conversationId", "session_id", "sessionId"); seed != "" {
		h.Source = "request session field"
	} else if seed = bodyValue(metadata, "conversation_id", "conversationId", "session_id", "sessionId"); seed != "" {
		h.Source = "request metadata session field"
	}
	if seed != "" {
		h.ID = normalizedConversationID(seed, "conversation", caller)
	}
	for _, field := range []struct {
		header string
		names  []string
		target *string
		kind   string
	}{
		{conversationRequestIDHeader, []string{"conversation_request_id", "conversationRequestId"}, &h.RequestID, "request"},
		{conversationMessageIDHeader, []string{"conversation_message_id", "conversationMessageId"}, &h.MessageID, "message"},
	} {
		value := usableConversationSeed(r.Header.Get(field.header))
		if value == "" {
			value = bodyValue(body, field.names...)
		}
		if value == "" {
			value = bodyValue(metadata, field.names...)
		}
		if value != "" {
			*field.target = normalizedConversationID(value, field.kind, caller+"\x00"+h.ID)
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), conversationContextKey{}, h))
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[Conversation headers prepared] traceId=%s source=%s conversation_id_ready=true turn_id_ready=true message_id_ready=true note=Retries and account switches of the same logical request reuse the same identifiers; session keys, prompts, and tokens are not logged",
		trace, h.Source)
	return h
}

// setConversationHeaders' unified fallback also covers the CLI probe and
// background price probing.
// Only model inference calls go through here; login, renewal, and quota queries
// do not inject conversation headers.
func setConversationHeaders(req *http.Request) {
	h := conversationHeadersFor(req)
	if h == nil {
		h = prepareConversationContext(req, nil)
	}
	req.Header.Set(conversationIDHeader, h.ID)
	req.Header.Set(conversationRequestIDHeader, h.RequestID)
	req.Header.Set(conversationMessageIDHeader, h.MessageID)
}
