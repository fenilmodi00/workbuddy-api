package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Model attribute probe (probe)
//
// Purpose: let accounts that were "never scheduled" (typically international
// accounts with exhausted balance) actively learn whether a model is free or
// paid for them, without waiting for a client request to happen to hit them.
//
// Why HTTP instead of a standalone process connecting directly to upstream:
//   the free/paid ledger lives in the serve process's memory (persisted only
//   via periodic workbuddy-status.json snapshots); a state file written by a
//   standalone process would be overwritten by the running service. The probe
//   subcommand therefore acts only as a client; the running service's
//   /admin/probe endpoint actually performs the probe and updates the ledger.
//
// Security: /admin/probe only accepts loopback origins; when the service has
// -api-key set, the key must still be presented.
// -----------------------------------------------------------------------------

const probePrompt = "Please write a factual paragraph of about 150 words about the water cycle, without markdown."

type probeRequest struct {
	Auth   string   `json:"auth,omitempty"`   // credential file path or file name; empty = all accounts
	Models []string `json:"models,omitempty"` // model list; empty = first limit models of the catalog
	Limit  int      `json:"limit,omitempty"`  // max models when Models is empty; default 5
}

type probeResult struct {
	Account string  `json:"account"`
	Edition string  `json:"edition"`
	Model   string  `json:"model"`
	Status  string  `json:"status"` // free | paid | unknown | quota | rate_limited | auth_failed | skipped | error
	Credit  float64 `json:"credit"` // usage.credit (meaningful only on success)
	Tokens  int64   `json:"tokens"` // usage.total_tokens
	HTTP    int     `json:"http"`   // upstream status code
	Detail  string  `json:"detail,omitempty"`
}

type probeResponse struct {
	Results []probeResult  `json:"results"`
	Summary map[string]int `json:"summary"`
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func handleAdminProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST requests are supported")
		return
	}
	if !isLoopbackRequest(r) {
		log.Printf("[Probe] rejected probe request from non-loopback origin: %s", r.RemoteAddr)
		writeOpenAIError(w, http.StatusForbidden, "probe_local_only", "probe endpoint may only be called from the local loopback address")
		return
	}

	var req probeRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "invalid JSON request body")
			return
		}
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}
	if req.Limit > 50 {
		req.Limit = 50
	}

	accountMu.Lock()
	candidates := append([]*Account(nil), accounts...)
	accountMu.Unlock()

	targets := make([]*Account, 0, len(candidates))
	for _, acc := range candidates {
		if acc.Disabled {
			continue
		}
		if req.Auth != "" && !probeAuthMatches(acc, req.Auth) {
			continue
		}
		targets = append(targets, acc)
	}
	if len(targets) == 0 {
		writeOpenAIError(w, http.StatusNotFound, "no_account", "no matching account (check the -auth path/file name)")
		return
	}

	models := req.Models
	if len(models) == 0 {
		all, _ := mergedModelIDs()
		if len(all) > req.Limit {
			all = all[:req.Limit]
		}
		models = all
	}

	log.Printf("[Probe] starting probe: accounts=%d, models=%d, accounts=%s, models=%s",
		len(targets), len(models), probeAccountNames(targets), strings.Join(models, ","))

	resp := probeResponse{Summary: map[string]int{}}
	for _, acc := range targets {
		for _, model := range models {
			if r.Context().Err() != nil {
				break
			}
			if disabled, reason := modelDisabled(model); disabled {
				result := probeResult{Account: acc.Path, Edition: acc.Profile().Key, Model: model, Status: "skipped", Detail: reason}
				resp.Results = append(resp.Results, result)
				resp.Summary[result.Status]++
				log.Printf("[Probe] traceId=%s model=%s result=skipped reason=model disabled note=no upstream call", debugTraceID(r), model)
				continue
			}
			if allowed, reason := modelAccountAllowed(model, acc, candidates); !allowed {
				result := probeResult{Account: acc.Path, Edition: acc.Profile().Key, Model: model, Status: "skipped", Detail: "account list exclusion: " + reason}
				resp.Results = append(resp.Results, result)
				resp.Summary[result.Status]++
				log.Printf("[Probe] traceId=%s model=%s credential=%s result=skipped reason=%s", w.Header().Get("X-Trace-ID"), model, filepath.Base(acc.Path), reason)
				debugEvent(r, "debug", "model_account_policy_checked", map[string]any{
					"account_file": filepath.Base(acc.Path), "model": model, "allowed": false, "reason": reason,
				})
				continue
			}
			result := probeAccountModel(r.Context(), acc, model)
			resp.Results = append(resp.Results, result)
			resp.Summary[result.Status]++
			log.Printf("[Probe] account %s [%s] model %s -> %s (%s)", result.Account, result.Edition, model, result.Status, result.Detail)
		}
	}
	writeStatusSnapshot()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func probeAuthMatches(acc *Account, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return true
	}
	return acc.Path == want || filepath.Base(acc.Path) == filepath.Base(want)
}

func probeAccountNames(accs []*Account) string {
	names := make([]string, 0, len(accs))
	for _, acc := range accs {
		names = append(names, filepath.Base(acc.Path))
	}
	return strings.Join(names, ",")
}

// probeAccountModel sends one minimal request for a single account + model and
// learns its free/paid attribute.
func probeAccountModel(ctx context.Context, acc *Account, model string) probeResult {
	result := probeResult{Account: filepath.Base(acc.Path), Edition: acc.Profile().Key, Model: model}

	if !lockAccountWithContext(ctx, &acc.lock) {
		result.Status = "error"
		result.Detail = "timed out waiting for the account lock"
		return result
	}
	defer acc.lock.Unlock()

	accountMu.Lock()
	if allowed, reason := modelAccountAllowed(model, acc, accounts); !allowed {
		accountMu.Unlock()
		result.Status, result.Detail = "skipped", "account list exclusion: "+reason
		log.Printf("[Probe] traceId=%s model=%s credential=%s result=skipped reason=%s note=hot config rechecked after waiting for the account lock; no upstream call", newTraceID(), model, filepath.Base(acc.Path), reason)
		return result
	}
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
		accountMu.Unlock()
		result.Status = "skipped"
		result.Detail = "account disabled or missing credentials"
		return result
	}
	auth := *acc.Auth
	prof := acc.Profile()
	accountMu.Unlock()
	if disabled, reason := modelDisabled(model); disabled {
		result.Status, result.Detail = "skipped", reason
		return result
	}

	body := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 300,
		"messages": []any{
			map[string]any{"role": "system", "content": defaultSystemPrompt},
			map[string]any{"role": "user", "content": probePrompt},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		result.Status = "error"
		result.Detail = err.Error()
		return result
	}

	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, prof.chatURL(), bytes.NewReader(payload))
	if err != nil {
		result.Status = "error"
		result.Detail = err.Error()
		return result
	}
	backendHeaders(req, &auth, prof)

	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		result.Status = "error"
		result.Detail = "upstream request failed: " + err.Error()
		return result
	}
	defer resp.Body.Close()
	result.HTTP = resp.StatusCode

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		errStr := string(errBody)
		switch {
		case isQuotaExhausted(resp.StatusCode, errStr):
			markModelQuotaBlocked(acc, model, errStr)
			result.Status = "quota"
			result.Detail = "quota exhausted (recorded as a paid model for this account)"
		case isModelRateLimited(errStr):
			until, ok := parseResetTime(errStr)
			if !ok {
				until = time.Now().Add(60 * time.Second)
			}
			markModelCooldown(acc, model, until, errStr)
			result.Status = "rate_limited"
			result.Detail = "model-level rate limited until " + until.Format("2006-01-02 15:04:05")
		case isAuthFailure(resp.StatusCode, errStr):
			result.Status = "auth_failed"
			result.Detail = "authorization invalid (not disabled automatically; please log in again): " + truncate(errStr, 120)
		default:
			result.Status = "error"
			result.Detail = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(errStr, 120))
		}
		return result
	}

	usage := readUsageFromSSE(resp.Body)
	credit, hasCredit := usageCredit(usage)
	tokens, _ := usageTotalTokens(usage)
	if !hasCredit {
		result.Status = "unknown"
		result.Detail = "upstream returned no usage.credit; cannot classify"
		return result
	}
	result.Credit = credit
	result.Tokens = tokens

	if credit > 0 {
		observeModelCredit(acc, model, usage, 0)
		result.Status = "paid"
		result.Detail = fmt.Sprintf("usage.credit=%s, paid", formatQuota(credit))
		return result
	}
	if tokens < modelFreeMinTokens {
		result.Status = "unknown"
		result.Detail = fmt.Sprintf("credit=0 but sample too small (total_tokens=%d < %d); not classified as free", tokens, modelFreeMinTokens)
		return result
	}
	observeModelCredit(acc, model, usage, 0)
	result.Status = "free"
	result.Detail = fmt.Sprintf("usage.credit=0, total_tokens=%d, free", tokens)
	return result
}

// readUsageFromSSE reads the last usage object from the upstream SSE stream.
func readUsageFromSSE(r io.Reader) map[string]any {
	var usage map[string]any
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
	}
	return usage
}

// runProbe is the probe subcommand: as a client, it calls the running service's /admin/probe.
func runProbe() {
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	if cfg.Addr == "0.0.0.0" || cfg.Addr == "::" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	url := "http://" + addr + "/admin/probe"

	reqBody := probeRequest{Limit: cfg.ProbeLimit}
	if cfg.AuthExplicit {
		reqBody.Auth = cfg.AuthFile
	}
	for _, m := range strings.Split(cfg.ProbeModels, ",") {
		if m = strings.TrimSpace(m); m != "" {
			reqBody.Models = append(reqBody.Models, m)
		}
	}
	payload, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		fmt.Printf("failed to build request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if key := currentAPIKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	fmt.Printf("requesting %s (account=%s, models=%s)...\n", url, ifEmpty(reqBody.Auth, "all"), ifEmpty(strings.Join(reqBody.Models, ","), "first catalog entries"))

	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		fmt.Printf("call failed: %v\nPlease confirm serve is running and -addr/-port match it.\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("service returned HTTP %d: %s\n", resp.StatusCode, truncate(string(body), 300))
		return
	}
	var out probeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Printf("failed to parse response: %v\n", err)
		return
	}

	sort.SliceStable(out.Results, func(i, j int) bool {
		if out.Results[i].Account != out.Results[j].Account {
			return out.Results[i].Account < out.Results[j].Account
		}
		return out.Results[i].Model < out.Results[j].Model
	})
	fmt.Printf("\n%-22s %-8s %-24s %-12s %-8s %-8s %s\n", "Account", "Site", "Model", "Result", "credit", "tokens", "Detail")
	for _, r := range out.Results {
		fmt.Printf("%-22s %-8s %-24s %-12s %-8s %-8d %s\n",
			r.Account, ifEmpty(r.Edition, "-"), r.Model, r.Status,
			ifEmpty(formatQuota(r.Credit), "-"), r.Tokens, r.Detail)
	}
	fmt.Printf("\nSummary: ")
	keys := make([]string, 0, len(out.Summary))
	for k := range out.Summary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%d ", k, out.Summary[k])
	}
	fmt.Println()
}
