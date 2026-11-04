package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func newWebUIHandler() http.Handler {
	if !cfg.WebUI {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	registerWebUIRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	return requestAuditMiddleware(adminSecurityMiddleware(adminAuthMiddleware(mux)))
}

func adminSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		reject := func(reason string) {
			log.Printf("[Admin request blocked] traceId=%s layer=admin origin validation reason=%s statusCode=403 businessImpact=request did not reach admin business logic", debugTraceID(r), reason)
			writeJSONError(w, http.StatusForbidden, reason)
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") ||
				!strings.EqualFold(u.Host, r.Host) || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				reject("The admin interface only accepts same-origin requests")
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			reject("Cross-site admin requests are rejected")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
			if r.Header.Get("X-Workbuddy-Admin") != "1" || !strings.EqualFold(contentType, "application/json") {
				reject("Admin write operations must be submitted as same-origin JSON requests")
				return
			}
		}
		log.Printf("[Admin origin validation] traceId=%s result=passed note=admin CORS is not opened to other origins", debugTraceID(r))
		next.ServeHTTP(w, r)
	})
}

func adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/admin/api/") || r.URL.Path == "/admin/api/setup" {
			next.ServeHTTP(w, r)
			return
		}
		key := currentAdminKey()
		header := r.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(header, "Bearer ") {
			token = strings.TrimPrefix(header, "Bearer ")
		}
		if key == "" || subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
			log.Printf("[Admin request blocked] traceId=%s layer=admin auth statusCode=401 businessImpact=request did not reach business methods adminKeyConfigured=%t", debugTraceID(r), key != "")
			writeJSONError(w, http.StatusUnauthorized, "Please log in with the admin Key; model API keys are not accepted")
			return
		}
		log.Printf("[Admin auth] traceId=%s result=passed note=admin Key auth is independent of model API auth", debugTraceID(r))
		next.ServeHTTP(w, r)
	})
}

// Does not trust proxy-declared client IPs; initialization can only be done over loopback connections and loopback Hosts.
func localAdminSetupRequest(r *http.Request) bool {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(remote).IsLoopback() {
		return false
	}
	u, err := url.Parse("http://" + r.Host)
	if err != nil || u.User != nil {
		return false
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
		return false
	}
	return r.Header.Get("Forwarded") == "" && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Real-IP") == ""
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeAdminJSONLimit(w, r, dst, 16<<10)
}

func decodeAdminJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit) // Admin forms only; does not affect model requests.
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureJSONEOF(dec)
}

func handleWebUISetup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"setupRequired": currentAdminKey() == "", "localSetupOnly": true,
			"canInitialize": localAdminSetupRequest(r),
		})
	case http.MethodPost:
		if !localAdminSetupRequest(r) {
			log.Printf("[Admin setup] traceId=%s result=rejected reason=not a direct local access statusCode=403 businessImpact=admin Key not generated or written", debugTraceID(r))
			writeJSONError(w, http.StatusForbidden,
				"Only the local machine is allowed to automatically generate an admin Key for the first time. Options: 1) open this page on the server itself; "+
					"2) use SSH local port forwarding (e.g. ssh -L 8316:127.0.0.1:8316 user@server) and then access; "+
					"3) edit config.json on the server directly, set gateway.adminKey to a random string, and it takes effect about 1 second after saving")
			return
		}
		var empty map[string]json.RawMessage
		if err := decodeAdminJSON(w, r, &empty); err != nil || empty == nil || len(empty) != 0 {
			log.Printf("[Admin setup] traceId=%s result=rejected reason=invalid request JSON statusCode=400", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "The setup request must be an empty JSON object")
			return
		}
		log.Printf("[Admin setup] traceId=%s stage=started note=generates a 32-character random admin Key exactly once; the raw value never enters logs", debugTraceID(r))
		key, err := initializeAdminKey(debugTraceID(r))
		if err != nil {
			status := http.StatusInternalServerError
			message := "Failed to save the admin Key; initialization not completed. Check the server logs and config file permissions"
			if errors.Is(err, errSetupDone) {
				status, message = http.StatusConflict, errSetupDone.Error()
			}
			log.Printf("[Admin setup] traceId=%s result=failed statusCode=%d reason=%v", debugTraceID(r), status, err)
			writeJSONError(w, status, message)
			return
		}
		log.Printf("[Admin setup] traceId=%s result=success keyLength=32 note=persisted; the Key is returned only in this setup response", debugTraceID(r))
		// Intentionally returns the raw value this one time only, to satisfy the page popup; all other admin endpoints return no secrets.
		writeJSON(w, http.StatusCreated, map[string]string{"adminKey": key})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET/POST are supported")
	}
}

func handleWebUISettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		log.Printf("[Admin settings] traceId=%s stage=read result=success note=returns the auth toggle, whether a Key is configured, and the actual listening ports", debugTraceID(r))
		writeJSON(w, http.StatusOK, gatewaySettingsView())
	case http.MethodPost:
		var input struct {
			Enabled *bool  `json:"apiKeyEnabled"`
			Key     string `json:"apiKey"`
			APIPort *int   `json:"apiPort"`
			WebPort *int   `json:"webPort"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil || input.Enabled == nil && input.APIPort == nil && input.WebPort == nil && input.Key == "" {
			log.Printf("[Admin settings] traceId=%s stage=parameter validation result=rejected statusCode=400 reason=a valid API auth toggle and JSON are required", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "Provide at least the API auth settings or port settings; field types must be correct")
			return
		}
		log.Printf("[Admin settings] traceId=%s stage=parameter validation authToggleProvided=%t portsProvided=%t newKeyProvided=%t note=raw Key value is never logged", debugTraceID(r), input.Enabled != nil, input.APIPort != nil || input.WebPort != nil, input.Key != "")
		if err := saveGatewaySettings(input.Enabled, input.Key, input.APIPort, input.WebPort, debugTraceID(r)); err != nil {
			log.Printf("[Admin settings] traceId=%s result=failed reason=%v businessImpact=current runtime auth state is kept", debugTraceID(r), err)
			status := http.StatusInternalServerError
			var validation gatewayValidationError
			if errors.As(err, &validation) {
				status = http.StatusBadRequest
			}
			if errors.Is(err, errConfigChange) {
				status = http.StatusConflict
			}
			writeJSONError(w, status, safeDebugError(err))
			return
		}
		writeJSON(w, http.StatusOK, gatewaySettingsView())
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET/POST are supported")
	}
}

// Persists frontend validation/request results as a limited set of structured fields; accepts no body, Key, or arbitrary log strings.
func handleWebUIClientEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only POST is supported")
		return
	}
	var input struct {
		Event   string `json:"event"`
		Route   string `json:"route"`
		Status  int    `json:"status"`
		Enabled *bool  `json:"enabled,omitempty"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid frontend audit event")
		return
	}
	allowed := map[string]bool{"request_completed": true, "request_failed": true, "settings_validation_failed": true, "credential_validation_failed": true, "log_validation_failed": true,
		"config_edit_warning": true, "config_edit_acknowledged": true, "config_edit_cancelled": true, "config_validation_failed": true,
		"list_filter_changed": true, "list_filter_restored": true, "list_filter_cookie_failed": true,
		"stats_chart_hover_started": true, "stats_chart_hover_ended": true}
	routes := map[string]bool{
		"/admin/api/status": true, "/admin/api/models": true, "/admin/api/logs": true,
		"/admin/api/config": true, "/admin/api/credentials": true, "/admin/api/settings": true,
		"/admin/api/account-toggle": true, "/admin/api/model-toggle": true,
		"/admin/api/credential-edit": true, "/admin/api/log-settings": true, "/admin/api/log-cleanup": true,
		"/admin/api/tasks":          true,
		"/admin/api/config-edit":    true,
		"/admin/api/rate-detail":    true,
		"/admin/api/stats":          true,
		"/admin/api/account-alerts": true,
		"/admin/api/account-add":    true,
		"/admin/api/account-cancel": true,
		"/admin/api/account-delete": true,
	}
	if !allowed[input.Event] || !routes[input.Route] || input.Status < 0 || input.Status > 599 {
		writeJSONError(w, http.StatusBadRequest, "Undefined frontend audit events are not accepted")
		return
	}
	filterEvent := strings.HasPrefix(input.Event, "list_filter_")
	if strings.HasPrefix(input.Event, "stats_chart_hover_") && (input.Route != "/admin/api/stats" || input.Status != 0 || input.Enabled != nil) {
		log.Printf("[Admin frontend] traceId=%s stage=chart interaction audit validation result=rejected statusCode=400 note=only fixed stats chart events are accepted; coordinates, values, bodies, or credentials are not", debugTraceID(r))
		writeJSONError(w, http.StatusBadRequest, "Chart interaction audits are only for stats charts")
		return
	}
	if filterEvent && (input.Enabled == nil || input.Route != "/admin/api/status" && input.Route != "/admin/api/models") ||
		!filterEvent && input.Enabled != nil {
		log.Printf("[Admin frontend] traceId=%s stage=filter preference validation result=rejected statusCode=400 note=only boolean filter states for the account or model list are accepted; arbitrary content is not", debugTraceID(r))
		writeJSONError(w, http.StatusBadRequest, "Filter audits must provide the boolean state of the corresponding list")
		return
	}
	if filterEvent {
		log.Printf("[Admin frontend filter] traceId=%s event=%s list=%s onlyUsable=%t statusCode=%d note=changes the web display preference only; does not modify config, scheduling rules, or historical stats; cookies, bodies, and Keys are not logged",
			debugTraceID(r), input.Event, input.Route, *input.Enabled, input.Status)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	log.Printf("[Admin frontend] traceId=%s event=%s endpoint=%s statusCode=%d note=frontend request or parameter validation result persisted; contains no Key or business body",
		debugTraceID(r), input.Event, input.Route, input.Status)
	w.WriteHeader(http.StatusNoContent)
}

// Redacts only the output copy; the original state used for business decisions and disk files must not be modified.
func writeAdminData(w http.ResponseWriter, r *http.Request, value any) {
	raw, err := json.Marshal(value)
	var copyValue any
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err = decoder.Decode(&copyValue)
	}
	if err != nil {
		log.Printf("[Admin response] traceId=%s stage=encode result=failed reason=%v", debugTraceID(r), err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to encode admin data")
		return
	}
	writeJSON(w, http.StatusOK, redactDebugValue(copyValue))
}
