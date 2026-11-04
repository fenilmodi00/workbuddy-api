package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"unicode/utf8"
)

const configEditBudget = 1 << 20

func configRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Only edits the fixed config.json; does not accept client-specified paths, nor follow symlinks.
// The caller holds runtimeConfigMu, serializing with web settings, allowlist toggles, and config hot reload.
func readConfigEditorDocument() ([]byte, error) {
	info, err := os.Lstat(runtimeConfigFile)
	if err != nil {
		return nil, errors.New("config.json is unreadable; check that the file exists and its permissions")
	}
	if !info.Mode().IsRegular() {
		return nil, gatewayValidationError("config.json must be a regular file; web editing does not support symlinks or directories")
	}
	f, err := os.Open(runtimeConfigFile)
	if err != nil {
		return nil, errors.New("config.json is unreadable; check file permissions")
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		return nil, errConfigChange
	}
	data, err := io.ReadAll(io.LimitReader(f, configEditBudget+1))
	if err != nil {
		return nil, errors.New("failed to read config.json; config content not returned")
	}
	if len(data) > configEditBudget || !utf8.Valid(data) {
		return nil, gatewayValidationError("web editing only supports UTF-8 config files up to 1 MiB")
	}
	return data, nil
}

func validateConfigEditorDocument(content string) (runtimeFileConfig, error) {
	if len(content) > configEditBudget || !utf8.ValidString(content) {
		return runtimeFileConfig{}, gatewayValidationError("config must be a single UTF-8 JSON object up to 1 MiB")
	}
	c, err := parseRuntimeConfig([]byte(content))
	if err != nil {
		// Parse errors may carry user-supplied field names or values; never write their raw text into responses or the audit log.
		return c, gatewayValidationError("config validation failed: must be a single JSON object; field names, types, ports, log retention days, and account/model allowlists must follow the config rules")
	}
	for _, key := range []string{c.Gateway.AdminKey, c.Gateway.APIKey} {
		if key == "[REDACTED]" {
			return c, gatewayValidationError("cannot save the redacted placeholder [REDACTED] as a Key; reload to edit the original text")
		}
	}
	return c, nil
}

func handleWebUIConfigEdit(w http.ResponseWriter, r *http.Request) {
	trace := debugTraceID(r)
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "only GET/POST are supported")
		return
	}
	var input struct {
		Revision               string `json:"revision"`
		Content                string `json:"content"`
		Acknowledged           bool   `json:"acknowledged"`
		ConfirmAdminKeyRemoval bool   `json:"confirmAdminKeyRemoval"`
	}
	if r.Method == http.MethodGet {
		input.Acknowledged = r.URL.Query().Get("acknowledged") == "1"
	} else if err := decodeAdminJSONLimit(w, r, &input, configEditBudget*6+4096); err != nil {
		// Escaped characters in JSON strings can inflate the request length; the config content itself is still limited to 1 MiB.
		log.Printf("[Config edit] traceId=%s stage=parameter-validation result=rejected status-code=400 reason=invalid request format or length body-omitted=true", trace)
		writeJSONError(w, http.StatusBadRequest, "config edit request format or length invalid")
		return
	}
	if !input.Acknowledged {
		log.Printf("[Config edit] traceId=%s stage=risk-acknowledgement result=rejected status-code=400 note=edit warning not acknowledged; no plaintext returned, no config written", trace)
		writeJSONError(w, http.StatusBadRequest, "please acknowledge the config edit risk warning first")
		return
	}
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	// The request may encounter an admin Key rotation while waiting for the config lock; must re-validate inside the lock.
	key := currentAdminKey()
	if key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) != 1 {
		log.Printf("[Config edit] traceId=%s stage=in-lock-auth result=rejected status-code=401 note=admin Key has changed; config neither read nor written", trace)
		writeJSONError(w, http.StatusUnauthorized, "admin Key has changed, please log in again")
		return
	}
	old, err := readConfigEditorDocument()
	if err != nil {
		writeAdminOperationError(w, r, "config edit load", err)
		return
	}
	// When a valid on-disk config has changed the admin Key but hot reload has not run yet, an old session must not overwrite it.
	if onDisk, err := parseRuntimeConfig(old); err == nil && onDisk.Gateway.AdminKey != key {
		writeAdminOperationError(w, r, "config edit", errConfigChange)
		return
	}
	if r.Method == http.MethodGet {
		log.Printf("[Config edit] traceId=%s stage=load result=success bytes=%d risk-acknowledged=true note=only this admin endpoint returns editable original text; config content and Keys never enter logs nor persist in the browser", trace, len(old))
		writeJSON(w, http.StatusOK, map[string]any{
			"path": runtimeConfigFile, "content": string(old), "revision": configRevision(old),
		})
		return
	}
	if input.Revision == "" || input.Revision != configRevision(old) {
		log.Printf("[Config edit] traceId=%s stage=revision-check result=rejected status-code=409 note=config was modified by another operation; new content not overwritten", trace)
		writeJSONError(w, http.StatusConflict, "the config has been edited by another page or the server; reload before saving; the current changes were not written")
		return
	}
	c, err := validateConfigEditorDocument(input.Content)
	if err != nil {
		writeAdminOperationError(w, r, "config edit validation", err)
		return
	}
	adminKeyChanged := c.Gateway.AdminKey != key
	if c.Gateway.AdminKey == "" && !input.ConfirmAdminKeyRemoval {
		writeAdminOperationError(w, r, "config edit validation", gatewayValidationError("clearing gateway.adminKey makes admin login unusable and requires extra confirmation; it can be re-set locally on the server"))
		return
	}
	if r.Context().Err() != nil {
		log.Printf("[Config edit] traceId=%s stage=pre-save-check result=cancelled note=request cancelled; config not written", trace)
		writeJSONError(w, http.StatusRequestTimeout, "request cancelled; config not written")
		return
	}
	// External edits are not constrained by the in-process config lock; re-check disk after full validation
	// to avoid overwriting server edits made while the larger config was being validated.
	current, err := readConfigEditorDocument()
	if err != nil {
		writeAdminOperationError(w, r, "config edit pre-save check", err)
		return
	}
	if configRevision(current) != input.Revision {
		log.Printf("[Config edit] traceId=%s stage=pre-save-revision-recheck result=rejected status-code=409 note=on-disk config changed during validation; new content not overwritten", trace)
		writeJSONError(w, http.StatusConflict, "the config was modified during validation; reload before saving; the current changes were not written")
		return
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal([]byte(input.Content), &doc) // Guaranteed to be a single object with valid field types by full config validation.
	log.Printf("[Config edit] traceId=%s stage=save old-bytes=%d new-bytes=%d admin-key-changed=%t api-auth-enabled=%t config-api-port=%d config-web-port=%d note=validation passed; persist first then hot-apply; JSON content and Keys are never logged", trace, len(old), len(input.Content), adminKeyChanged, c.Gateway.APIKeyEnabled, c.Gateway.APIPort, c.Gateway.WebPort)
	if err := persistRuntimeDocument(doc, old, trace); err != nil {
		log.Printf("[Config edit] traceId=%s stage=persist-or-apply result=failed exception-type=%T reason=%s body-omitted=true", trace, err, safeDebugError(err))
		writeAdminOperationError(w, r, "config edit save", errors.New("config save or apply failed; check file permissions and server logs; update not completed"))
		return
	}
	settings := gatewaySettingsView()
	log.Printf("[Config edit] traceId=%s result=save-success admin-key-changed=%t admin-key-cleared=%t listeners-unswitched=%t note=following requests use the full new config; old connections can still complete after new listeners bind successfully; original text not recorded", trace, adminKeyChanged, c.Gateway.AdminKey == "", settings["restartRequired"])
	writeJSON(w, http.StatusOK, map[string]any{
		"saved": true, "revision": hex.EncodeToString(runtimeAppliedHash[:]),
		"adminKeyChanged": adminKeyChanged, "adminKeyCleared": c.Gateway.AdminKey == "",
		"settings": settings,
	})
}
