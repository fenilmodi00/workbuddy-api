package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Web search forwarding (POST /v1/search)
//
// The upstream API was located and live-verified by REA from WorkBuddy client artifacts:
//
//	C:\Program Files\WorkBuddy\resources\app.asar.unpacked\cli\dist\codebuddy-lite-wb.mjs
//	  WEB_SEARCH_API_PATH = "/agenttool/v1/search"
//	  WEB_SEARCH_TIMEOUT_MS = 20000
//	  URL    = productManager.endpoint + WEB_SEARCH_API_PATH
//	  body   = {query, type:"text2text", max_results:5[, allowed_domains][, freshness]}
//	  Response = {query, type, provider, results[{title,url,snippet,site?,favicon?}],
//	            total_results, response_time_ms}
//
// DSH uses count and a stricter result schema than WorkBuddy's upstream endpoint.
// -----------------------------------------------------------------------------

const (
	webSearchAPIPath = "/agenttool/v1/search"

	// The official client caps search requests at 20s; keep the same limit here.
	webSearchTimeout = 20 * time.Second

	// The official client hardcodes max_results=5.
	webSearchDefaultMaxResults = 5
	webSearchMaxResults        = 20

	webSearchMaxQueryLength = 2000
	webSearchMaxBodyBytes   = 1 << 20

	// -------------------------------------------------------------------------
	// Web fetch (POST /v1/fetch) -> upstream /agenttool/v1/webfetch
	// -------------------------------------------------------------------------
	webFetchAPIPath = "/agenttool/v1/webfetch"

	// Official client WEB_FETCH constants: cc=2e4 (API timeout 20s), cu=1e5 (max_length).
	webFetchUpstreamTimeout = 30 * time.Second
	webFetchMaxLength       = 100000

	// webFetchDefaultPrompt is the prompt required by upstream webfetch (the client
	// generates it via a model; the gateway has no model context, so use a stable
	// "extract main content" instruction).
	webFetchDefaultPrompt = "Extract the main content of this page as clean Markdown. Keep headings, lists, code blocks and key facts. Remove navigation, ads and boilerplate."
)

func (p *upstreamProfile) agentToolBase() string {
	if p == nil {
		return ""
	}
	base := p.Base
	if p.ChatBase != "" && (p.DefaultBase == "" || p.Base == p.DefaultBase) {
		base = p.ChatBase
	}
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

// webSearchRequest is the DSH Search API request shape.
type webSearchRequest struct {
	Query       string `json:"query"`
	Count       int    `json:"count,omitempty"`
	NeedContent *bool  `json:"need_content,omitempty"`
	OriQuestion string `json:"ori_question,omitempty"`
}

type workbuddyWebSearchRequest struct {
	Query      string `json:"query"`
	Type       string `json:"type"`
	MaxResults int    `json:"max_results"`
}

// webSearchFetchRequest is the POST /v1/fetch request body (DSH Fetch API shape).
type webSearchFetchRequest struct {
	URL string `json:"url"`
}

// webSearchSearchError carries an upstream request failure.
// status==0 means a network-layer failure (no HTTP response was received).
type webSearchSearchError struct {
	status int
	err    error
}

func (e *webSearchSearchError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("upstream status %d", e.status)
	}
	return e.err.Error()
}

func (e *webSearchSearchError) Unwrap() error { return e.err }

func newWebSearchError(status int, format string, args ...any) *webSearchSearchError {
	return &webSearchSearchError{status: status, err: fmt.Errorf(format, args...)}
}

// handleWebSearch maps the DSH Search API request and response shapes.
func handleWebSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST requests are supported")
		return
	}
	traceID := w.Header().Get("X-Trace-ID")
	if traceID == "" {
		traceID = newTraceID()
	}

	body, readErr := io.ReadAll(io.LimitReader(r.Body, webSearchMaxBodyBytes))
	_ = r.Body.Close()
	if readErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "Failed to read request body")
		return
	}

	var req webSearchRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON request body")
			return
		}
	}
	if err := normalizeWebSearchRequest(&req); err != nil {
		log.Printf("[Web search] traceId=%s stage=parameter validation result=rejected reason=%s business impact=upstream not called, account quota not consumed", traceID, err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Search also needs a set of conversation identifiers: upstream attributes call
	// sources by X-Conversation-* headers. If the client sends X-Conversation-ID,
	// it gets registered and reused here.
	prepareConversationContext(r, nil)

	acc := webSearchAccount()
	if acc == nil {
		log.Printf("[Web search] traceId=%s stage=account selection result=failed reason=no available credentials", traceID)
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "No login credentials available, please log in to an account first")
		return
	}
	if err := ensureValidTokenFor(acc); err != nil {
		log.Printf("[Web search] traceId=%s stage=credential renewal result=failed reason=%s", traceID, safeDebugError(err))
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_failed", "Failed to renew account credentials")
		return
	}
	auth, prof := webSearchAccountSnapshot(acc)
	if auth == nil || prof == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "No login credentials available, please log in to an account first")
		return
	}

	payload, err := json.Marshal(workbuddyWebSearchRequest{
		Query: req.Query, Type: "text2text", MaxResults: req.Count,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "Failed to build upstream request body")
		return
	}

	started := time.Now()
	obj, callErr := requestWebSearch(r, payload, auth, prof, prof.agentToolBase())
	if callErr == nil {
		log.Printf("[Web search] traceId=%s stage=completed account=%s result=success duration=%v", traceID, baseNameOf(acc), time.Since(started))
		writeWebSearchSuccess(w, obj)
		return
	}
	status, errType, message := classifyWebSearchFailure(callErr.status, callErr)
	log.Printf("[Web search] traceId=%s stage=upstream call account=%s status code=%d result=failed reason=%s",
		traceID, baseNameOf(acc), callErr.status, safeDebugError(callErr))
	writeUpstreamError(w, r, status, errType, message)
}

// handleWebFetch handles POST /v1/fetch: forwards to upstream /agenttool/v1/webfetch and returns the DSH Fetch API shape.
//
// Upstream request body (evidence gathered by REA from the CLI artifact fetchContentViaApi):
//
//	{url, format:"markdown", prompt, timeout, max_length[, model_name]}
//
// Upstream response: {url, title, content, content_type, content_length, truncated, status_code, context}
// The decoder of DSH's newer Search / Fetch API (deepseek-fetch-api) only reads the top-level
// {content: string}; it then wraps it itself into {url, statusCode, body:{kind:"text",content}, truncated}.
// DSH reads the top-level content field and wraps it into its Fetch result type.
func handleWebFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST requests are supported")
		return
	}
	traceID := w.Header().Get("X-Trace-ID")
	if traceID == "" {
		traceID = newTraceID()
	}

	body, readErr := io.ReadAll(io.LimitReader(r.Body, webSearchMaxBodyBytes))
	_ = r.Body.Close()
	if readErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "Failed to read request body")
		return
	}
	var req webSearchFetchRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON request body")
			return
		}
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "url must not be empty")
		return
	}
	parsed, err := url.Parse(req.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "url must be an absolute http/https address")
		return
	}

	prepareConversationContext(r, nil)
	acc := webSearchAccount()
	if acc == nil {
		log.Printf("[Web fetch] traceId=%s stage=account selection result=failed reason=no available credentials", traceID)
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "No login credentials available, please log in to an account first")
		return
	}
	if err := ensureValidTokenFor(acc); err != nil {
		log.Printf("[Web fetch] traceId=%s stage=credential renewal result=failed reason=%s", traceID, safeDebugError(err))
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_failed", "Failed to renew account credentials")
		return
	}
	auth, prof := webSearchAccountSnapshot(acc)
	if auth == nil || prof == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "No login credentials available, please log in to an account first")
		return
	}

	payload, err := json.Marshal(map[string]any{
		"url":        req.URL,
		"format":     "markdown",
		"prompt":     webFetchDefaultPrompt,
		"timeout":    int(webFetchUpstreamTimeout / time.Second),
		"max_length": webFetchMaxLength,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "Failed to build upstream request body")
		return
	}

	started := time.Now()
	obj, callErr := requestWebFetch(r, payload, auth, prof, prof.agentToolBase())
	if callErr == nil {
		log.Printf("[Web fetch] traceId=%s stage=completed account=%s result=success duration=%v", traceID, baseNameOf(acc), time.Since(started))
		writeWebFetchSuccess(w, traceID, obj)
		return
	}
	status, errType, message := classifyWebSearchFailure(callErr.status, callErr)
	log.Printf("[Web fetch] traceId=%s stage=upstream call account=%s status code=%d result=failed reason=%s",
		traceID, baseNameOf(acc), callErr.status, safeDebugError(callErr))
	writeUpstreamError(w, r, status, errType, message)
}

// writeWebFetchSuccess projects the upstream webfetch result into the DSH Fetch API shape.
func writeWebFetchSuccess(w http.ResponseWriter, traceID string, obj map[string]any) {
	content := webSearchString(obj["content"])
	if content == "" {
		// DSH treats empty content as unusable; report the error honestly instead of returning an empty body.
		log.Printf("[Web fetch] traceId=%s result=failed reason=upstream returned no content business impact=no fabricated body", traceID)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "Upstream returned no usable page content")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content": content,
	})
}

func requestWebFetch(r *http.Request, payload []byte, auth *StoredAuth, prof *upstreamProfile, base string) (map[string]any, *webSearchSearchError) {
	return postAgentTool(r, payload, auth, prof, base, webFetchAPIPath, webFetchUpstreamTimeout)
}

// applyAgentToolHeaders adds the official desktop identity headers for domestic-site /agenttool/* requests.
//
// The same headers for model requests are handled centrally by applyObservedDesktopHeaders; search/fetch
// do not go through the model routing, so they are added independently at the endpoint side. Keeps the
// Origin/Referer and X-Client-ID set by backendHeaders, without the full model-request replacement set.
func applyAgentToolHeaders(req *http.Request, auth *StoredAuth, prof *upstreamProfile) {
	if prof == nil || prof.Key != "cn" {
		return
	}
	for name, value := range map[string]string{
		"X-Requested-With":    "XMLHttpRequest",
		"X-IDE-Type":          "WorkBuddy",
		"X-IDE-Name":          "WorkBuddy",
		"X-IDE-Version":       "5.8.3",
		"X-Private-Data":      "true",
		"X-Codebuddy-Request": "1",
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-Agent-Type":        "main",
	} {
		req.Header.Set(name, value)
	}
	if auth != nil && auth.Account.EnterpriseID != "" {
		req.Header.Set("X-Tenant-Id", auth.Account.EnterpriseID)
	}
	if traceparent := agentToolTraceparent(); traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
}

// agentToolTraceparent generates a W3C-compliant traceparent; the header is omitted on failure.
func agentToolTraceparent() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return "00-" + hex.EncodeToString(buf[:16]) + "-" + hex.EncodeToString(buf[16:]) + "-01"
}

func postAgentTool(r *http.Request, payload []byte, auth *StoredAuth, prof *upstreamProfile, base, path string, timeout time.Duration) (map[string]any, *webSearchSearchError) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	endpoint := strings.TrimRight(strings.TrimSpace(base), "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, newWebSearchError(0, "Failed to build upstream request: %w", err)
	}
	backendHeaders(req, auth, prof)
	applyAgentToolHeaders(req, auth, prof)

	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, newWebSearchError(0, "Upstream API timeout (%v)", timeout)
		}
		return nil, newWebSearchError(0, "Upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, webSearchMaxBodyBytes))
	if resp.StatusCode >= 400 {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, newWebSearchError(resp.StatusCode, "Upstream rejected credentials (401), response is not JSON")
		}
		return nil, newWebSearchError(resp.StatusCode, "Upstream status code %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, newWebSearchError(http.StatusBadGateway, "Failed to parse upstream response: %w", err)
	}
	// Successful upstream responses have no code field (observed: {"query":...,"type":...,"provider":...,"results":[...]});
	// Only business errors carry a non-zero code (e.g. 15003 query is required). So code must not be required to exist.
	if code, present := webSearchUpstreamCode(obj); present && code != 0 {
		return nil, &webSearchSearchError{status: http.StatusBadRequest, err: &webSearchUpstreamError{code: code, msg: webSearchUpstreamMessage(obj)}}
	}
	return obj, nil
}

func requestWebSearch(r *http.Request, payload []byte, auth *StoredAuth, prof *upstreamProfile, base string) (map[string]any, *webSearchSearchError) {
	obj, callErr := postAgentTool(r, payload, auth, prof, base, webSearchAPIPath, webSearchTimeout)
	if callErr != nil {
		return nil, callErr
	}
	if _, ok := obj["results"]; !ok {
		return nil, newWebSearchError(http.StatusBadGateway, "Upstream search response missing results field")
	}
	return obj, nil
}

// webSearchUpstreamError represents a business error inside an upstream 200 envelope (non-zero code).
type webSearchUpstreamError struct {
	code int
	msg  string
}

func (e *webSearchUpstreamError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("Upstream search API returned code=%d", e.code)
	}
	return fmt.Sprintf("Upstream search API returned code=%d: %s", e.code, e.msg)
}

func writeWebSearchSuccess(w http.ResponseWriter, obj map[string]any) {
	writeJSON(w, http.StatusOK, map[string]any{"results": webSearchResults(obj)})
}

// normalizeWebSearchRequest validates the DSH input and applies the supported count range.
func normalizeWebSearchRequest(req *webSearchRequest) error {
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return fmt.Errorf("query must not be empty")
	}
	if utf8Len(req.Query) < 2 {
		return fmt.Errorf("query requires at least 2 characters")
	}
	if utf8Len(req.Query) > webSearchMaxQueryLength {
		return fmt.Errorf("query too long (max %d characters)", webSearchMaxQueryLength)
	}
	if req.Count <= 0 {
		req.Count = webSearchDefaultMaxResults
	}
	if req.Count > webSearchMaxResults {
		req.Count = webSearchMaxResults
	}
	return nil
}

func webSearchAccount() *Account {
	accountMu.Lock()
	defer accountMu.Unlock()
	now := time.Now()
	for _, acc := range accounts {
		if acc == nil || acc.Disabled || acc.Auth == nil || strings.TrimSpace(acc.Auth.Auth.AccessToken) == "" || acc.CooldownUntil.After(now) {
			continue
		}
		return acc
	}
	return nil
}

// webSearchAccountSnapshot takes a copy of credentials and site parameters under lock, avoiding half-updated state reads during concurrent scheduling.
func webSearchAccountSnapshot(acc *Account) (*StoredAuth, *upstreamProfile) {
	accountMu.Lock()
	defer accountMu.Unlock()
	if acc == nil || acc.Auth == nil {
		return nil, nil
	}
	auth := *acc.Auth
	return &auth, profileForEdition(auth.Edition)
}

func classifyWebSearchFailure(lastStatus int, lastErr error) (int, string, string) {
	switch {
	case lastStatus == http.StatusUnauthorized:
		return http.StatusUnauthorized, "upstream_auth_failed", "Upstream rejected current credentials (401), please log in again"
	case lastStatus == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "upstream_rate_limited", "Upstream rate limited (429), please retry later"
	case lastStatus == http.StatusForbidden:
		return http.StatusForbidden, "upstream_forbidden", "Upstream rejected this search request (403)"
	case lastStatus >= 400 && lastStatus < 500:
		return http.StatusBadRequest, "upstream_error",
			fmt.Sprintf("Upstream rejected search request (%d): %s", lastStatus, safeDebugError(lastErr))
	default:
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("Upstream search API call failed: %s", safeDebugError(lastErr))
	}
}

// webSearchUpstreamCode reads the upstream business code.
// The returned bool indicates "the code field actually appeared in the response"; successful responses lack it.
func webSearchUpstreamCode(obj map[string]any) (int, bool) {
	value, ok := obj["code"]
	if !ok {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return int(v), true
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, false
		}
		// A non-numeric code is treated as a business-error envelope; the message comes from msg/message.
		return -1, true
	case nil:
		return 0, false
	default:
		return -1, true
	}
}

func webSearchUpstreamMessage(obj map[string]any) string {
	if msg := webSearchString(obj["msg"]); msg != "" {
		return msg
	}
	return webSearchString(obj["message"])
}

// webSearchResults normalizes the results array.
//
// Upstream may return site-type results with only a site and no url (observed: market/database results have an empty url).
// But DSH's mapSearchResponse requires url to be a non-empty string, otherwise the whole response is judged invalid,
// so entries without a url are downgraded to the site homepage link; only dropped when nothing can be constructed.
//
// DSH requires string title, url, and snippet fields, plus a nullable date.
func webSearchResults(obj map[string]any) []map[string]any {
	list, _ := obj["results"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		site := webSearchString(raw["site"])
		url := strings.TrimSpace(webSearchString(raw["url"]))
		if url == "" {
			url = webSearchSiteFallbackURL(site)
		}
		if url == "" {
			// Neither a url nor a site to infer from; DSH would reject it anyway, so dropping it outright is more honest.
			continue
		}
		out = append(out, map[string]any{
			"title":   webSearchString(raw["title"]),
			"url":     url,
			"snippet": webSearchString(raw["snippet"]),
			"date":    nil,
		})
	}
	return out
}

// webSearchSiteFallbackURL turns results that only carry a site name into clickable links.
// site may be a domain (go.dev) or a Chinese name (stock database); returns empty when no URL can be constructed.
func webSearchSiteFallbackURL(site string) string {
	site = strings.TrimSpace(site)
	if site == "" || strings.ContainsAny(site, " \t") {
		return ""
	}
	// Only treat dot-containing values as domains; pure Chinese names are not treated as domains.
	if !strings.Contains(site, ".") {
		return ""
	}
	return "https://" + strings.Trim(site, ".")
}

func webSearchString(value any) string {
	s, _ := value.(string)
	return s
}

func baseNameOf(acc *Account) string {
	if acc == nil {
		return ""
	}
	if idx := strings.LastIndexAny(acc.Path, `/\`); idx >= 0 {
		return acc.Path[idx+1:]
	}
	return acc.Path
}

// utf8Len counts by characters, avoiding Chinese queries being misjudged as too long by byte length.
func utf8Len(s string) int {
	return len([]rune(s))
}
