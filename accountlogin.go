package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// accountFileNamePattern restricts new accounts to a single file name; directories and wildcards are not accepted.
var accountFileNamePattern = regexp.MustCompile(`^workbuddy[A-Za-z0-9._-]*\.json$`)

// accountCredentialDir returns the credential directory: -auth-dir takes precedence, otherwise the program working directory.
func accountCredentialDir() (string, error) {
	accountMu.Lock()
	dir := strings.TrimSpace(cfg.AuthDir)
	accountMu.Unlock()
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("Failed to resolve credential directory: %w", err)
	}
	return abs, nil
}

// resolveCredentialPath resolves the user-provided file name into an absolute path under the working directory; directories are never accepted.
func resolveCredentialPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("Please provide a credential name")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\*?[]`) {
		return "", errors.New("Credential name must be a file name; directories and wildcards are not allowed")
	}
	if !accountFileNamePattern.MatchString(name) {
		return "", errors.New("Credential name must start with workbuddy, end with .json, and use only letters, digits and . _ -")
	}
	dir, err := accountCredentialDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// suggestCredentialFileName picks the first unused workbuddyN.json in the directory.
func suggestCredentialFileName() string {
	used := map[int]bool{}
	dir, err := accountCredentialDir()
	if err == nil {
		if entries, readErr := os.ReadDir(dir); readErr == nil {
			for _, e := range entries {
				name := strings.TrimSuffix(e.Name(), ".disabled")
				if !strings.HasPrefix(name, "workbuddy") || !strings.HasSuffix(name, ".json") {
					continue
				}
				if n, convErr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "workbuddy"), ".json")); convErr == nil {
					used[n] = true
				}
			}
		}
	}
	for n := 1; ; n++ {
		if !used[n] {
			return fmt.Sprintf("workbuddy%d.json", n)
		}
	}
}

// loginSession is an in-progress QR login. The CLI login and the web account-add flow share this path;
// only who renders the QR code and who assembles the credential path differ.
type loginSession struct {
	ID      string
	Site    string
	File    string
	AuthURL string

	state    string
	deadline time.Time
	client   *http.Client
	prof     *upstreamProfile
	cancel   context.CancelFunc

	mu      sync.Mutex
	status  string // pending / done / failed / expired / cancelled
	errText string
	auth    *StoredAuth
	savedTo string
}

var (
	loginSessionsMu sync.Mutex
	loginSessions   = map[string]*loginSession{}
)

// loginSessionRetention keeps finished login sessions in memory for a while longer.
// The CLI holds *loginSession directly and does not depend on the session table; the web UI only gets an id
// and must query again to see expired / failed / done. Deleting immediately on finish would make frontend
// polling almost always get a 404, appearing as "the link is dead but the console keeps waiting".
var loginSessionRetention = 2 * time.Minute

func newLoginSessionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("login-%d", time.Now().UnixNano())
	}
	return "login-" + hex.EncodeToString(buf)
}

// qrDataURI generates the QR code image; failure only affects the display, not the login flow itself.
func (s *loginSession) qrDataURI() string {
	code, err := qrcode.New(s.AuthURL, qrcode.Medium)
	if err != nil {
		return ""
	}
	png, err := code.PNG(240)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func (s *loginSession) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"id": s.ID, "site": s.Site, "file": s.File, "authUrl": s.AuthURL,
		"status": s.status, "expiresAt": s.deadline.Unix(),
	}
	if s.status == "pending" {
		remaining := int(time.Until(s.deadline).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		out["remainingSeconds"] = remaining
	} else {
		out["remainingSeconds"] = 0
	}
	if s.errText != "" {
		out["error"] = s.errText
	}
	if s.status == "done" {
		out["savedAs"] = filepath.Base(s.savedTo)
		if s.auth != nil {
			out["account"] = map[string]any{
				"nickname": s.auth.Account.Nickname, "uid": s.auth.Account.UID,
				"enterpriseId": s.auth.Account.EnterpriseID, "tokenExpiresAt": s.auth.Auth.ExpiresAt,
			}
		}
	}
	return out
}

func (s *loginSession) finish(status, errText string, sa *StoredAuth, saved string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != "pending" {
		return
	}
	s.status, s.errText, s.auth, s.savedTo = status, errText, sa, saved
}

func forgetLoginSession(id string) {
	loginSessionsMu.Lock()
	delete(loginSessions, id)
	loginSessionsMu.Unlock()
}

// scheduleForgetLoginSession delays cleanup of finished sessions, giving web polling time to read the final state.
func scheduleForgetLoginSession(id string) {
	time.AfterFunc(loginSessionRetention, func() {
		forgetLoginSession(id)
	})
}

// beginAccountLogin reuses the same flow as CLI login: fetch state and QR code first, then poll for the token and save credentials.
func beginAccountLogin(site, targetPath string) (*loginSession, error) {
	prof := profileForEdition(site)
	client := &http.Client{Timeout: 30 * time.Second, Jar: cfg.HttpClient.Jar}
	pollHeaders := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("X-No-Authorization", "1")
	}
	data, _, err := doJSON(client, http.MethodPost, prof.authStateURL(), nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("Failed to get login state: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, errors.New("Upstream returned abnormal login state information, please retry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &loginSession{
		ID: newLoginSessionID(), Site: prof.Key, File: filepath.Base(targetPath),
		AuthURL: st.AuthURL, state: st.State, deadline: time.Now().Add(prof.LoginTTL),
		client: client, prof: prof, cancel: cancel, status: "pending",
	}
	go s.run(ctx, targetPath, pollHeaders)
	return s, nil
}

func (s *loginSession) run(ctx context.Context, targetPath string, pollHeaders func(*http.Request)) {
	trace := newTraceID()
	log.Printf("[Web login] traceId=%s stage=started site=%s credential=%s note=reused the same CLI login flow, waiting for QR authorization",
		trace, s.Site, filepath.Base(targetPath))
	defer scheduleForgetLoginSession(s.ID)
	defer s.cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish("cancelled", "", nil, "")
			return
		case <-ticker.C:
			if time.Now().After(s.deadline) {
				log.Printf("[Web login] traceId=%s stage=waiting for scan result=timeout site=%s business impact=no credentials written, account pool unchanged", trace, s.Site)
				s.finish("expired", "Login timed out, please request the login endpoint again", nil, "")
				return
			}
			tokRaw, _, errTok := doJSON(s.client, http.MethodGet, s.prof.authTokenURL(s.state), pollHeaders, nil)
			if errTok != nil {
				continue
			}
			var tok tokenData
			if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
				continue
			}
			var acct accountData
			acctHeaders := func(r *http.Request) {
				commonHeaders(r, s.prof)
				r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
			}
			if acctRaw, _, errAcct := doJSON(s.client, http.MethodGet, s.prof.loginAcctURL(s.state), acctHeaders, nil); errAcct == nil {
				_ = json.Unmarshal(acctRaw, &acct)
			}
			sa := &StoredAuth{
				Edition: s.prof.Key,
				Auth: StoredTokens{
					AccessToken:  tok.AccessToken,
					RefreshToken: tok.RefreshToken,
					ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
					Domain:       tok.Domain,
				},
				Account: StoredAccount{UID: acct.UID, EnterpriseID: acct.EnterpriseID, Nickname: acct.Nickname},
			}
			if err := saveAuthTo(targetPath, sa); err != nil {
				log.Printf("[Web login] traceId=%s stage=write credentials result=failed credential=%s reason=%v business impact=account not added to the account pool",
					trace, filepath.Base(targetPath), err)
				s.finish("failed", "Failed to save credentials: "+err.Error(), nil, "")
				return
			}
			if err := ensureAccountLoaded(targetPath); err != nil {
				log.Printf("[Web login] traceId=%s stage=join account pool result=failed credential=%s reason=%v business impact=file written, will load on next scan",
					trace, filepath.Base(targetPath), err)
			}
			requestQuotaScan()
			log.Printf("[Web login] traceId=%s stage=completed site=%s credential=%s account=%s result=success note=credentials loaded automatically, no restart needed",
				trace, s.Site, filepath.Base(targetPath), sa.Account.UID)
			s.finish("done", "", sa, targetPath)
			return
		}
	}
}

// ensureAccountLoaded makes freshly written credentials enter the account pool immediately, without relying on the directory scan covering the file.
func ensureAccountLoaded(path string) error {
	if _, err := reloadAccountsResult(); err != nil {
		return err
	}
	accountMu.Lock()
	for _, acc := range accounts {
		if acc.Path == path {
			accountMu.Unlock()
			return nil
		}
	}
	accountMu.Unlock()
	sa, err := loadAccountFile(path)
	if err != nil {
		return err
	}
	accountMu.Lock()
	accounts = append(accounts, &Account{Path: path, Auth: sa, Edition: profileForEdition(sa.Edition).Key})
	accountMu.Unlock()
	log.Printf("[Account added] credential=%s result=added to account pool note=added directly by the web add flow when the directory scan missed the file", filepath.Base(path))
	return nil
}

func findLoginSession(id string) *loginSession {
	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	return loginSessions[id]
}

// deleteAccountCredential deletes the given credential file (only the path already in the account pool is recognized).
func deleteAccountCredential(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) {
		return "", errors.New("Invalid credential name")
	}
	accountMu.Lock()
	target, duplicates := "", 0
	for _, acc := range accounts {
		if filepath.Base(acc.Path) == name {
			duplicates++
			target = acc.Path
		}
	}
	accountMu.Unlock()
	if duplicates > 1 {
		return "", errors.New("Duplicate credential names exist; cannot determine which one to delete")
	}
	if target == "" {
		return "", errors.New("Account does not exist or was already deleted")
	}
	if !accountFileNamePattern.MatchString(filepath.Base(target)) {
		return "", errors.New("Credential name does not match workbuddy*.json; refusing to delete")
	}
	info, err := os.Lstat(target)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return "", errors.New("Credential is not a regular file; refusing to delete")
		}
		if err := os.Remove(target); err != nil {
			return "", fmt.Errorf("Failed to delete credential: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		// The authorization-expiry flow may already have deleted the credential file, leaving only an expired-marker
		// entry in the account pool. Such entries should still be deletable explicitly from the web UI.
	default:
		return "", fmt.Errorf("Credential is unreadable: %w", err)
	}
	if err := os.Remove(markerPath(target)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[Account delete] credential=%s failed to clear expiry marker: %v", filepath.Base(target), err)
	}
	// Explicit deletion must unconditionally remove the entry from the account pool: hot-reload reconciliation keeps
	// Disabled expired-marker entries, so deleting only the credential file would not remove entries from the web UI and CLI.
	accountMu.Lock()
	kept := make([]*Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc.Path == target {
			continue
		}
		kept = append(kept, acc)
	}
	accounts = kept
	if len(accounts) > 0 {
		rrIndex %= len(accounts)
	} else {
		rrIndex = 0
	}
	accountMu.Unlock()
	reloadAccountsResult()
	requestQuotaScan()
	// Refresh the status snapshot immediately so the web UI stops showing the deleted account before the next 3-second snapshot.
	if err := writeStatusSnapshot(); err != nil {
		log.Printf("[Account delete] credential=%s failed to refresh status snapshot: %v", filepath.Base(target), err)
	}
	return target, nil
}

// handleWebUIAccountAdd supports reading the default name (GET), starting a login (POST), and querying progress (GET ?id=).
func handleWebUIAccountAdd(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			log.Printf("[Account added] traceId=%s stage=read default credential name result=success business impact=only the file name is returned; absolute paths are not sent to the frontend",
				debugTraceID(r))
			writeAdminData(w, r, map[string]any{"defaultFile": suggestCredentialFileName()})
			return
		}
		s := findLoginSession(id)
		if s == nil {
			log.Printf("[Account added] traceId=%s stage=query login progress id=%s result=session missing or finished business impact=frontend should prompt to re-request the login endpoint",
				debugTraceID(r), id)
			writeJSONError(w, http.StatusNotFound, "Login session does not exist or has finished, please request the login endpoint again")
			return
		}
		writeAdminData(w, r, s.snapshot())
	case http.MethodPost:
		var input struct {
			Site string `json:"site"`
			File string `json:"file"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid add-account request format")
			return
		}
		trace := debugTraceID(r)
		site := strings.ToLower(strings.TrimSpace(input.Site))
		if site != "cn" && site != "intl" {
			log.Printf("[Account added] traceId=%s stage=parameter validation result=rejected site=%s reason=site must be cn or intl response status code=400 business impact=no login session created",
				trace, input.Site)
			writeJSONError(w, http.StatusBadRequest, "Site must be the domestic site or the international site")
			return
		}
		target, err := resolveCredentialPath(input.File)
		if err != nil {
			log.Printf("[Account added] traceId=%s stage=parameter validation result=rejected reason=%v response status code=400 business impact=no login session created", trace, err)
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := os.Lstat(target); err == nil {
			log.Printf("[Account added] traceId=%s stage=parameter validation result=rejected credential=%s reason=file with the same name already exists response status code=409 business impact=existing credentials not overwritten",
				trace, filepath.Base(target))
			writeJSONError(w, http.StatusConflict, "A credential with the same name already exists, please choose another name")
			return
		}
		s, err := beginAccountLogin(site, target)
		if err != nil {
			log.Printf("[Account added] traceId=%s stage=fetch login endpoint result=failed site=%s reason=%v business impact=no login session created", trace, site, err)
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		loginSessionsMu.Lock()
		loginSessions[s.ID] = s
		loginSessionsMu.Unlock()
		log.Printf("[Account added] traceId=%s stage=fetch login endpoint result=success site=%s credential=%s session=%s business impact=auto-write and load after QR scan",
			trace, site, filepath.Base(target), s.ID)
		body := s.snapshot()
		body["qrPng"] = s.qrDataURI()
		writeAdminData(w, r, body)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "Only GET/POST are supported")
	}
}

// handleWebUIAccountCancel cancels an in-progress login session, stopping background polling.
func handleWebUIAccountCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only POST is supported")
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid cancel-login request format")
		return
	}
	trace := debugTraceID(r)
	s := findLoginSession(strings.TrimSpace(input.ID))
	if s == nil {
		log.Printf("[Account added] traceId=%s stage=cancel login result=session missing business impact=nothing to cancel", trace)
		writeAdminData(w, r, map[string]any{"cancelled": false})
		return
	}
	s.cancel()
	log.Printf("[Account added] traceId=%s stage=cancel login result=success site=%s credential=%s business impact=polling stopped, no credentials written",
		trace, s.Site, s.File)
	writeAdminData(w, r, map[string]any{"cancelled": true})
}

// handleWebUIAccountDelete deletes the given credential file.
func handleWebUIAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Only POST is supported")
		return
	}
	var input struct {
		File string `json:"file"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid delete-account request format")
		return
	}
	trace := debugTraceID(r)
	target, err := deleteAccountCredential(input.File)
	if err != nil {
		log.Printf("[Account delete] traceId=%s stage=delete credential result=failed credential=%s reason=%v response status code=400 business impact=credentials and account pool unchanged",
			trace, input.File, err)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("[Account delete] traceId=%s stage=delete credential result=success credential=%s business impact=credential file deleted, account pool updated in sync, statistics and history logs retained",
		trace, filepath.Base(target))
	writeAdminData(w, r, map[string]any{"deleted": filepath.Base(target)})
}
