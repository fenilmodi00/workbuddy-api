package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Credential verification: before the two irreversible actions — "overwrite" and
// "delete" — prove the new credential is usable and that the credential being
// deleted really is no longer usable. The goal is to avoid destroying credentials that still work.
const (
	// credentialVerifyTimeout is the timeout for the read-only check; a failed check must not block for long.
	credentialVerifyTimeout = 8 * time.Second
)

// jwtClaims decodes the JWT payload (no signature verification), only for local consistency comparison and expiry display.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	return claims
}

// shortID generates an irreversible short identifier, used in logs to refer to an account without exposing the original identifier.
func shortID(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// credentialIdentity extracts the account identifier and site realm declared by the access token, used to compare whether old and new credentials belong to the same account.
func credentialIdentity(tokens StoredTokens) (accountID, realm string) {
	claims := jwtClaims(tokens.AccessToken)
	if claims == nil {
		return "", ""
	}
	if sub, ok := claims["sub"].(string); ok {
		accountID = sub
	}
	iss, _ := claims["iss"].(string)
	if iss == "" {
		return accountID, ""
	}
	if i := strings.Index(iss, "/auth/realms/"); i >= 0 {
		return accountID, iss[i+len("/auth/realms/"):]
	}
	return accountID, iss
}

// verifyAccountUsable uses a read-only API to confirm whether the credential can still fetch account data.
//
// Returns (true, nil)  credential usable;
// Returns (false, nil) upstream explicitly rejected (401 or body explicitly says login expired), credential no longer usable;
// Returns (false, err) undeterminable (plain 403, network, timeout, upstream 5xx); the caller must not destroy the credential based on this alone.
func verifyAccountUsable(ctx context.Context, auth StoredAuth) (bool, error) {
	registerCredentialSecrets(&auth)
	traceID := newTraceID()
	if state, ok := ctx.Value(debugRequestContextKey{}).(*debugRequestContext); ok {
		state.mu.RLock()
		traceID = state.traceID
		state.mu.RUnlock()
	}
	log.Printf("[Credential verify] traceId=%s stage=readonly check start site=%s note=confirm account API is usable, credentials not written", traceID, auth.Edition)
	if strings.TrimSpace(auth.Auth.AccessToken) == "" {
		log.Printf("[Credential verify] traceId=%s result=undeterminable reason=missing access token note=upstream not called", traceID)
		return false, fmt.Errorf("missing access token, cannot verify")
	}
	if cfg.HttpClient == nil {
		log.Printf("[Credential verify] traceId=%s result=undeterminable reason=HTTP client not ready note=upstream not called", traceID)
		return false, fmt.Errorf("HTTP client not ready, cannot verify")
	}
	prof := profileForEdition(auth.Edition)
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if auth.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
		}
	}
	_, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, prof.quotaSummaryURL(), headers, strings.NewReader("{}"))
	if err != nil {
		// A plain 403 may just be insufficient permission or a risk-control rejection; it cannot confirm the token is invalid.
		// This only fixes the read-only credential check and does not change other callers' 403 deactivation rules.
		if status == http.StatusUnauthorized || isAuthFailure(0, err.Error()) {
			log.Printf("[Credential verify] traceId=%s HTTP=%d result=unusable reason=%v note=upstream explicitly returned authorization failure", traceID, status, err)
			return false, nil // upstream explicitly rejected: credential really unusable
		}
		log.Printf("[Credential verify] traceId=%s HTTP=%d result=undeterminable reason=%v business impact=credential must not be destroyed based on this result alone", traceID, status, err)
		return false, fmt.Errorf("read-only verification incomplete HTTP=%d", status)
	}
	log.Printf("[Credential verify] traceId=%s HTTP=%d result=usable note=account data fetched successfully", traceID, status)
	return true, nil
}

// validateRefreshedCredential performs two checks before writing the new credential back to disk:
//  1. Old and new credentials must belong to the same account and the same site, preventing an upstream account mix-up or response mismatch from overwriting the wrong credential;
//  2. The new access token must be able to fetch account data, preventing an "API 200 but actually unusable" credential from overwriting an old credential that still works.
//
// If the verification API itself is unavailable, treat as passed, avoiding rejection of a valid new credential due to verification jitter.
func validateRefreshedCredential(path string, refreshed *StoredAuth, oldTokens StoredTokens) error {
	oldID, oldRealm := credentialIdentity(oldTokens)
	newID, newRealm := credentialIdentity(refreshed.Auth)
	identityNote := "credential carries no parseable account claims, skipping consistency comparison"
	if newID != "" {
		identityNote = fmt.Sprintf("account=%s site=%s matches old credential", shortID(newID), newRealm)
	}
	if oldID != "" && newID != "" && oldID != newID {
		return fmt.Errorf("new credential account identifier differs from old credential (%s -> %s), refusing overwrite",
			shortID(oldID), shortID(newID))
	}
	if oldRealm != "" && newRealm != "" && oldRealm != newRealm {
		return fmt.Errorf("new credential site differs from old credential (%s -> %s), refusing overwrite", oldRealm, newRealm)
	}

	ctx, cancel := context.WithTimeout(context.Background(), credentialVerifyTimeout)
	defer cancel()
	usable, err := verifyAccountUsable(ctx, *refreshed)
	if err != nil {
		log.Printf("[Credential verify] account=%s stage=pre-overwrite check result=undeterminable, treating as passed consistency=%s reason=%v note=refresh API succeeded, not discarding new credential due to verification jitter",
			path, identityNote, err)
		return nil
	}
	if !usable {
		log.Printf("[Credential verify] account=%s stage=pre-overwrite check result=failed consistency=%s reason=new access token cannot fetch account data business impact=abandoning this overwrite, keeping old credential in use",
			path, identityNote)
		return fmt.Errorf("new credential cannot fetch account data, refusing to overwrite old credential")
	}
	log.Printf("[Credential verify] account=%s stage=pre-overwrite check result=passed consistency=%s readonly check=new access token can fetch account data note=allowing overwrite of old credential",
		path, identityNote)
	return nil
}

// credentialStillUsableForDelete determines whether a credential about to be deleted can actually still be used.
// A return value of keep=true means the credential file should be kept.
func credentialStillUsableForDelete(auth *StoredAuth) (keep bool, note string) {
	if auth == nil || strings.TrimSpace(auth.Auth.AccessToken) == "" {
		return false, "no access token to verify, treating as expired"
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialVerifyTimeout)
	defer cancel()
	usable, err := verifyAccountUsable(ctx, *auth)
	switch {
	case err != nil:
		// When undeterminable, conservatively keep: deletion is irreversible, better to leave an expired file.
		return true, fmt.Sprintf("read-only verification could not complete (%v), conservatively keeping credential", err)
	case usable:
		return true, "read-only verification shows credential can still fetch account data, keeping credential without deleting"
	default:
		return false, "read-only verification confirms credential is no longer usable"
	}
}
