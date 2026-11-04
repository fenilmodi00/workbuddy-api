package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

func currentModelPolicyMode() string {
	modelFilterMu.RLock()
	defer modelFilterMu.RUnlock()
	if len(currentFilter.allowlist) > 0 {
		return "allowlist"
	}
	return "blocklist"
}

// Global disable is a scheduling policy; it does not reuse auth-invalidated fields and never deletes credentials or historical stats.
func annotateAccountPolicies(rows []accountSnapshot) {
	pool := make([]*Account, len(rows))
	for i := range rows {
		pool[i] = &Account{Path: rows[i].Path}
	}
	for i := range rows {
		enabled, reason := modelAccountAllowed("all", pool[i], pool)
		rows[i].SchedulingEnabled, rows[i].SchedulingReason = &enabled, reason
	}
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	return obj, nil
}

func sortedNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		if names[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Merge using the newest config on disk rather than the possibly stale full config from the page; unrelated config sections are preserved.
func savePolicyToggle(kind, target string, enabled bool, trace string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	doc, gateway, old, err := readGatewayDocumentLocked()
	if err == nil && (cfg.AdminKey == "" || gateway.AdminKey != cfg.AdminKey) {
		err = errConfigChange
	}
	gatewayConfigMu.Unlock()
	if err != nil {
		return err
	}
	c := runtimeFileConfig{}
	if len(old) > 0 {
		c, err = parseRuntimeConfig(old)
		if err != nil {
			return err
		}
	}
	models, err := rawObject(doc["models"])
	if err != nil {
		return err
	}
	// When upgrading from old examples, remove the disabled model list so both contradictory fields are no longer written.
	if hasModelNames(c.Models.Allowlist) {
		delete(models, "blocklist")
	} else {
		delete(models, "allowlist")
		if _, exists := models["blocklist"]; !exists {
			models["blocklist"] = json.RawMessage("[]")
		}
	}

	switch kind {
	case "account":
		accountMu.Lock()
		exists, duplicates := false, 0
		name := filepath.Base(target)
		accountNames := map[string]int{}
		for _, acc := range accounts {
			accountNames[filepath.Base(acc.Path)]++
			if acc.Path == target {
				exists = true
			}
			if filepath.Base(acc.Path) == name {
				duplicates++
			}
		}
		accountMu.Unlock()
		if !exists || duplicates != 1 {
			return gatewayValidationError("Account does not exist or credential file name is duplicated; cannot set the global toggle")
		}
		// Modify only the raw JSON of "all", preserving model-specific rule fields and null/empty values.
		accountRules, parseErr := rawObject(models["accounts"])
		if parseErr != nil {
			return parseErr
		}
		// Stay case-compatible with an existing ALL; delete the old spelling so only one "all" is written.
		global := modelAccountFileConfig{}
		for id, rule := range c.Models.Accounts {
			if normalizeModelName(id) == "all" {
				global = rule
				delete(accountRules, id)
			}
		}
		blocks, allows := map[string]bool{}, map[string]bool{}
		for _, n := range global.Blocklist {
			blocks[strings.TrimSpace(n)] = true
		}
		for _, n := range global.Allowlist {
			allows[strings.TrimSpace(n)] = true
		}
		sourceMode := "blocklist"
		if len(allows) > 0 {
			sourceMode = "allowlist"
			// Explicitly disable accounts in the current pool not granted by the allowlist;
			// simply deleting the allowlist would unintentionally open up other accounts.
			for n := range accountNames {
				if !allows[n] {
					blocks[n] = true
				}
			}
		}
		if len(allows) > 0 || len(global.Blocklist) > 0 {
			for n, count := range accountNames {
				if count > 1 {
					blocks[n] = true // Preserve the rejection state for duplicated credential names while the rule is enabled.
				}
			}
		}
		if enabled {
			delete(blocks, name)
		} else {
			blocks[name] = true
		}
		finalBlocks := sortedNames(blocks)
		log.Printf("[Account toggle rules] traceId=%s stage=converting global account list source=%s currentAccountFileNames=%d originalAllowlist=%q originalBlocklist=%q target=%q enabled=%t finalBlocklist=%q note=deleted all.allowlist and writes all.blocklist only; other accounts' global state is kept, model-specific rules unchanged, accounts added later default to enabled",
			trace, sourceMode, len(accountNames), global.Allowlist, global.Blocklist, name, enabled, finalBlocks)
		accountRules["all"], err = json.Marshal(map[string][]string{"blocklist": finalBlocks})
		if err == nil {
			models["accounts"], err = json.Marshal(accountRules)
		}
	case "model":
		target = normalizeModelName(target)
		ids := managementModelIDs(c)
		if target == "" || !ids[target] {
			return gatewayValidationError("Model is not in the current model list or configured lists")
		}
		blocks, allows := map[string]bool{}, map[string]bool{}
		for _, id := range c.Models.Blocklist {
			blocks[normalizeModelName(id)] = true
		}
		for _, id := range c.Models.Allowlist {
			allows[normalizeModelName(id)] = true
		}
		delete(blocks, "")
		delete(allows, "")
		if len(allows) > 0 {
			for id := range ids {
				if !allows[id] {
					blocks[id] = true
				}
			}
		}
		if enabled {
			delete(blocks, target)
		} else {
			blocks[target] = true
		}
		delete(models, "allowlist")
		models["blocklist"], err = json.Marshal(sortedNames(blocks))
	default:
		return errors.New("Undefined toggle type")
	}
	if err != nil {
		return err
	}
	doc["models"], err = json.Marshal(models)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	c, err = parseRuntimeConfig(data)
	if err != nil {
		return err
	}
	log.Printf("[Admin toggle] traceId=%s stage=config persisted type=%s target=%q targetEnabled=%t note=modifies only the target rule; other config is preserved; credentials and stats are never deleted", trace, kind, target, enabled)
	if err := writeConfigPreservingFile(append(data, '\n'), old); err != nil {
		return err
	}
	if err := applyRuntimeConfig(c, true, trace); err != nil {
		if restoreErr := writeConfigPreservingFile(old, data); restoreErr != nil {
			return fmt.Errorf("apply failed: %v; restoring config failed: %w", err, restoreErr)
		}
		return err
	}
	runtimeAppliedHash, runtimeHasAppliedHash = sha256.Sum256(append(data, '\n')), true
	log.Printf("[Admin toggle] traceId=%s result=success type=%s target=%q enabled=%t note=config saved and applied immediately; in-flight upstream requests are not forcibly interrupted", trace, kind, target, enabled)
	return nil
}

func managementModelIDs(c runtimeFileConfig) map[string]bool {
	ids := map[string]bool{}
	add := func(id string) {
		if id = normalizeModelName(id); id != "" {
			ids[id] = true
		}
	}
	catalog, _ := collectModelIDs(true)
	for _, id := range catalog {
		add(id)
	}
	for _, id := range c.Models.Allowlist {
		add(id)
	}
	for _, id := range c.Models.Blocklist {
		add(id)
	}
	for id := range c.Models.Accounts {
		if normalizeModelName(id) != "all" {
			add(id)
		}
	}
	modelStatsMu.Lock()
	for id := range modelStats {
		add(id)
	}
	modelStatsMu.Unlock()
	if snap, err := readStatusSnapshot(); err == nil {
		for _, row := range snap.Models {
			add(row.ID)
		}
		for _, row := range snap.ModelCatalog {
			add(row.ID)
		}
	}
	return ids
}

func handleWebUIAccountToggle(w http.ResponseWriter, r *http.Request) {
	handleWebUIPolicyToggle(w, r, "account")
}

func handleWebUIModelToggle(w http.ResponseWriter, r *http.Request) {
	handleWebUIPolicyToggle(w, r, "model")
}

func handleWebUIPolicyToggle(w http.ResponseWriter, r *http.Request, kind string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only POST is supported")
		return
	}
	var input struct {
		Target  string `json:"target"`
		Enabled *bool  `json:"enabled"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil || input.Enabled == nil || strings.TrimSpace(input.Target) == "" {
		log.Printf("[Admin toggle] traceId=%s stage=parameter validation result=rejected type=%s statusCode=400 note=target and boolean toggle are required; config not modified", debugTraceID(r), kind)
		writeJSONError(w, http.StatusBadRequest, "A target string and an enabled boolean are required")
		return
	}
	log.Printf("[Admin toggle] traceId=%s stage=parameter validation type=%s target=%q enabled=%t result=passed", debugTraceID(r), kind, input.Target, *input.Enabled)
	if err := savePolicyToggle(kind, input.Target, *input.Enabled, debugTraceID(r)); err != nil {
		status := http.StatusInternalServerError
		var validation gatewayValidationError
		if errors.As(err, &validation) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, errConfigChange) {
			status = http.StatusConflict
		}
		log.Printf("[Admin toggle] traceId=%s result=failed type=%s statusCode=%d reason=%s note=modification did not succeed; page must keep the original toggle", debugTraceID(r), kind, status, safeDebugError(err))
		writeJSONError(w, status, safeDebugError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": input.Target, "enabled": *input.Enabled, "saved": true})
}
