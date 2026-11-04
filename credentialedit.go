package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const credentialEditBudget = 256 << 10

var errCredentialChanged = errors.New("credential file was updated by background renewal or another operation; reload before saving to avoid overwriting the new token")

func credentialRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func openEditableCredential(target string, flag int) (*os.Root, *os.File, error) {
	allowed := false
	for _, configured := range collectConfiguredAuthPaths() {
		if target == configured {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, nil, gatewayValidationError("only credential files loaded by the current config may be edited; arbitrary paths are not allowed")
	}
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return nil, nil, errors.New("credential directory is not accessible")
	}
	info, err := root.Lstat(filepath.Base(target))
	if err != nil || !info.Mode().IsRegular() {
		root.Close()
		return nil, nil, gatewayValidationError("credential file does not exist, is not a regular file, or is a symbolic link")
	}
	f, err := root.OpenFile(filepath.Base(target), flag, 0600)
	if err != nil {
		root.Close()
		return nil, nil, errors.New("credential file is not readable/writable; check file permissions")
	}
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		f.Close()
		root.Close()
		return nil, nil, errCredentialChanged
	}
	return root, f, nil
}

func readEditableCredential(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, credentialEditBudget+1))
	if err == nil && len(data) > credentialEditBudget {
		err = gatewayValidationError("credential file exceeds 256 KiB and cannot be edited in the web UI")
	}
	return data, err
}

func validateCredentialDocument(raw []byte) (StoredAuth, error) {
	var auth StoredAuth
	if len(raw) > credentialEditBudget || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return auth, gatewayValidationError("credential must be a non-empty JSON object, max 256 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&auth); err != nil || ensureJSONEOF(dec) != nil {
		return auth, gatewayValidationError("credential JSON format or field types are invalid")
	}
	if strings.TrimSpace(auth.Auth.AccessToken) == "" || strings.Contains(auth.Auth.AccessToken, "****") || strings.Contains(auth.Auth.RefreshToken, "****") ||
		strings.Contains(auth.Auth.AccessToken, "[REDACTED]") || strings.Contains(auth.Auth.RefreshToken, "[REDACTED]") {
		return auth, gatewayValidationError("auth.accessToken must not be empty, and redacted token placeholders must not be saved")
	}
	if auth.Edition != "" && auth.Edition != "cn" && auth.Edition != "intl" && auth.Edition != "codebuddy" {
		return auth, gatewayValidationError("edition must be empty, cn, intl, or codebuddy")
	}
	registerCredentialSecrets(&auth)
	return auth, nil
}

// lockCredentialAccount re-checks the pooled object after acquiring the account lock, avoiding operating on a stale object when hot reload removes/re-adds the account.
func lockCredentialAccount(ctx context.Context, target string) (*Account, func(), error) {
	for {
		accountMu.Lock()
		var acc *Account
		for _, item := range accounts {
			if item.Path == target {
				acc = item
				break
			}
		}
		accountMu.Unlock()
		if acc != nil && !lockAccountWithContext(ctx, &acc.lock) {
			return nil, nil, errors.New("cancelled while waiting for the account's in-flight requests to finish; credential not modified")
		}
		accountMu.Lock()
		var current *Account
		for _, item := range accounts {
			if item.Path == target {
				current = item
				break
			}
		}
		if current == acc {
			return acc, func() {
				accountMu.Unlock()
				if acc != nil {
					acc.lock.Unlock()
				}
			}, nil
		}
		accountMu.Unlock()
		if acc != nil {
			acc.lock.Unlock()
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
	}
}

func saveEditedCredential(ctx context.Context, target, revision, content, trace string) error {
	auth, err := validateCredentialDocument([]byte(content))
	if err != nil {
		return err
	}
	acc, release, err := lockCredentialAccount(ctx, target)
	if err != nil {
		return err
	}
	defer release()
	root, f, err := openEditableCredential(target, os.O_RDWR)
	if err != nil {
		return err
	}
	defer root.Close()
	defer f.Close()
	old, err := readEditableCredential(f)
	if err != nil {
		return err
	}
	if revision == "" || revision != credentialRevision(old) {
		return errCredentialChanged
	}
	if ctx.Err() != nil {
		return errors.New("request cancelled; credential not modified")
	}
	if same, err := root.Stat(filepath.Base(target)); err != nil {
		return errCredentialChanged
	} else if opened, err := f.Stat(); err != nil || !os.SameFile(same, opened) {
		return errCredentialChanged
	}
	log.Printf("[Credential edit] traceId=%s stage=save file=%q old bytes=%d new bytes=%d note=revision check passed; only file and length logged, never JSON body or tokens", trace, target, len(old), len(content))
	write := func(data []byte) error {
		n, err := f.WriteAt(data, 0)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = f.Truncate(int64(len(data)))
		}
		if err == nil {
			err = f.Sync()
		}
		return err
	}
	if err := write([]byte(content)); err != nil {
		if restoreErr := write(old); restoreErr != nil {
			return errors.New("credential save failed and restoring the old file failed; check server disk status")
		}
		return errors.New("credential save failed; old file restored")
	}
	clearDisabledMarker(target)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	added := acc == nil
	if added {
		acc = &Account{Path: target}
		accounts = append(accounts, acc)
	}
	identityChanged := acc.Auth != nil && (acc.Auth.Account.UID != auth.Account.UID ||
		acc.Profile().Key != profileForEdition(auth.Edition).Key ||
		strings.TrimSpace(acc.Auth.Account.EnterpriseID) != strings.TrimSpace(auth.Account.EnterpriseID))
	if identityChanged {
		acc.QuotaKnown, acc.QuotaExhausted, acc.IsPaidUser = false, false, false
		acc.QuotaUnlimited = false
		acc.QuotaTotal, acc.QuotaUsed, acc.QuotaRemaining = 0, 0, 0
		acc.PlanLabel, acc.PlanCheckedAt, acc.PlanStale = "", 0, true
		acc.ModelStates = nil
		acc.CooldownUntil, acc.CooldownMsg = time.Time{}, ""
	}
	acc.Auth, acc.Edition, acc.fingerprint = &auth, profileForEdition(auth.Edition).Key, accountFingerprint(info)
	acc.Disabled, acc.DisabledReason = false, ""
	log.Printf("[Credential edit] traceId=%s result=save succeeded file=%q added to account pool=%t account identity changed=%t note=tokens never logged, no upstream verification called; same account keeps its ledger, identity or site change resets the corresponding runtime state", trace, target, added, identityChanged)
	return nil
}

func handleWebUICredentialEdit(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		target := r.URL.Query().Get("file")
		_, release, err := lockCredentialAccount(r.Context(), target)
		if err != nil {
			writeJSONError(w, http.StatusRequestTimeout, "cancelled while waiting for the account's requests to finish")
			return
		}
		root, f, err := openEditableCredential(target, os.O_RDONLY)
		var data []byte
		if err == nil {
			data, err = readEditableCredential(f)
			f.Close()
			root.Close()
		}
		release()
		if err != nil {
			writeAdminOperationError(w, r, "credential load", err)
			return
		}
		var auth StoredAuth
		if json.Unmarshal(data, &auth) == nil {
			registerCredentialSecrets(&auth)
		}
		log.Printf("[Credential edit] traceId=%s stage=load file=%q bytes=%d note=plaintext returned only when an admin actively opens the editor; body never logged, never persisted in the browser", debugTraceID(r), target, len(data))
		// The only plaintext credential edit endpoint protected by admin authentication; no redacted view replaces the editable original.
		writeJSON(w, http.StatusOK, map[string]any{"path": target, "content": string(data), "revision": credentialRevision(data)})
	case http.MethodPost:
		var input struct {
			Path     string `json:"path"`
			Revision string `json:"revision"`
			Content  string `json:"content"`
		}
		if err := decodeAdminJSONLimit(w, r, &input, credentialEditBudget*2+4096); err != nil {
			log.Printf("[Credential edit] traceId=%s stage=parameter validation result=rejected reason=edit request format or length invalid, body not logged", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "edit request format or length invalid")
			return
		}
		if err := saveEditedCredential(r.Context(), input.Path, input.Revision, input.Content, debugTraceID(r)); err != nil {
			writeAdminOperationError(w, r, "credential save", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": input.Path, "saved": true, "revision": credentialRevision([]byte(input.Content))})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "only GET/POST are supported")
	}
}
