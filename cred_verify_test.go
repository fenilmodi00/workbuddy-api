package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestCredentialIdentityExtractsAccountAndRealm(t *testing.T) {
	token := makeTestJWT(t, map[string]any{
		"sub": "user-abc",
		"iss": "https://www.codebuddy.cn/auth/realms/copilot",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	id, realm := credentialIdentity(StoredTokens{AccessToken: token})
	if id != "user-abc" || realm != "copilot" {
		t.Fatalf("id=%q realm=%q", id, realm)
	}
	if id, realm := credentialIdentity(StoredTokens{AccessToken: "opaque"}); id != "" || realm != "" {
		t.Fatalf("unparseable token should return empty: %q %q", id, realm)
	}
	if shortID("user-abc") == shortID("user-abd") {
		t.Fatal("short identifiers should distinguish different accounts")
	}
	if strings.Contains(shortID("user-abc"), "user-abc") {
		t.Fatal("short identifier should not contain the original text")
	}
}

// credTestServer serves both the refresh API and the read-only verification API.
func credTestServer(t *testing.T, refreshToken, refreshSub string, verifyStatus int, verifyBody string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{
				"accessToken":  makeTestJWT(t, map[string]any{"sub": refreshSub, "iss": "https://www.codebuddy.cn/auth/realms/copilot", "exp": time.Now().Add(time.Hour).Unix()}),
				"refreshToken": refreshToken,
				"expiresIn":    3600,
			}})
			_, _ = w.Write(body)
		case "/billing/meter/get-user-resource-summary":
			w.WriteHeader(verifyStatus)
			_, _ = w.Write([]byte(verifyBody))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	oldBase, oldOrigin, oldClient := profileCN.Base, profileCN.Origin, cfg.HttpClient
	profileCN.Base, profileCN.Origin, cfg.HttpClient = server.URL, server.URL, server.Client()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin, cfg.HttpClient = oldBase, oldOrigin, oldClient
	})
}

func newVerifiedTestAccount(t *testing.T, sub string) *Account {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-test.json")
	sa := &StoredAuth{Edition: "cn", Auth: StoredTokens{
		AccessToken:  makeTestJWT(t, map[string]any{"sub": sub, "iss": "https://www.codebuddy.cn/auth/realms/copilot", "exp": time.Now().Add(time.Hour).Unix()}),
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}}
	if err := saveAuthTo(path, sa); err != nil {
		t.Fatal(err)
	}
	return &Account{Path: path, Auth: sa, Edition: "cn"}
}

func TestRefreshCommitsOnlyVerifiedCredential(t *testing.T) {
	for _, tc := range []struct {
		name          string
		refreshSub    string
		verifyStatus  int
		verifyBody    string
		wantCommitted bool
	}{
		{"account consistent and verification passed", "user-A", 200, `{"code":0,"data":{"Packages":[]}}`, true},
		{"account identifier mismatch", "user-B", 200, `{"code":0,"data":{"Packages":[]}}`, false},
		{"new credential verification rejected", "user-A", 401, `{"code":12153,"msg":"invalid_grant"}`, false},
		{"permission 403 undeterminable, new credential not discarded", "user-A", 403, `{"code":10002,"msg":"forbidden"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			credTestServer(t, "new-refresh", tc.refreshSub, tc.verifyStatus, tc.verifyBody)
			acc := newVerifiedTestAccount(t, "user-A")
			oldAccess := acc.Auth.Auth.AccessToken

			err := refreshAccountToken(acc, "unit test")

			var onDisk StoredAuth
			raw, readErr := os.ReadFile(acc.Path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err := json.Unmarshal(raw, &onDisk); err != nil {
				t.Fatal(err)
			}
			if tc.wantCommitted {
				if err != nil {
					t.Fatalf("should commit new credential: %v", err)
				}
				if onDisk.Auth.RefreshToken != "new-refresh" {
					t.Fatalf("new refresh token not written to disk: %+v", onDisk.Auth)
				}
				if acc.RefreshFailCount != 0 {
					t.Fatal("failure count should be reset to zero after successful commit")
				}
			} else {
				if err == nil {
					t.Fatal("commit should be refused")
				}
				if onDisk.Auth.RefreshToken != "old-refresh" || onDisk.Auth.AccessToken != oldAccess {
					t.Fatalf("old credential should be preserved unchanged: %+v", onDisk.Auth)
				}
				if acc.Auth.Auth.RefreshToken != "old-refresh" {
					t.Fatal("old credential in memory should also be preserved")
				}
				// Verification failures should not count toward the "judged expired" counter, otherwise accounts that still work would get disabled.
				if acc.RefreshFailCount != 0 {
					t.Fatalf("pre-overwrite verification failure should not count toward the expiry counter: %d", acc.RefreshFailCount)
				}
				if !strings.Contains(acc.LastRefreshError, "pre-overwrite validation") {
					t.Fatalf("warning should be recorded: %q", acc.LastRefreshError)
				}
			}
		})
	}
}

// Pre-overwrite verification must be visible in the logs, otherwise production cannot confirm it actually ran.
func TestOverwriteVerificationIsObservable(t *testing.T) {
	chdirTemp(t)
	credTestServer(t, "new-refresh", "user-A", 200, `{"code":0,"data":{"Packages":[]}}`)
	acc := newVerifiedTestAccount(t, "user-A")
	var buf bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldWriter)

	if err := refreshAccountToken(acc, "unit test"); err != nil {
		t.Fatal(err)
	}
	log.SetOutput(oldWriter)
	out := buf.String()
	for _, want := range []string{"stage=pre-overwrite check", "result=passed", "matches old credential", "allowing overwrite of old credential"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pre-overwrite verification missing observable log %q:\n%s", want, out)
		}
	}
	// When overwriting is refused, a reason must also be recorded.
	credTestServer(t, "new-refresh", "user-B", 200, `{"code":0,"data":{"Packages":[]}}`)
	acc2 := newVerifiedTestAccount(t, "user-A")
	buf.Reset()
	log.SetOutput(&buf)
	if err := refreshAccountToken(acc2, "unit test"); err == nil {
		t.Fatal("overwrite should be refused on account mismatch")
	}
	log.SetOutput(oldWriter)
	if !strings.Contains(buf.String(), "account identifier differs from old credential") {
		t.Fatalf("refused overwrite should state the reason:\n%s", buf.String())
	}
}

func TestVerifyAccountUsableDistinguishesDeadFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantUsable bool
		wantErr    bool
	}{
		{"usable", 200, `{"code":0,"data":{}}`, true, false},
		{"explicitly expired 401", 401, `{"code":12153,"msg":"invalid_grant"}`, false, false},
		{"permission or risk-control 403 cannot confirm expiry", 403, `{"code":10002,"msg":"forbidden"}`, false, true},
		{"explicitly invalid token 403", 403, `{"code":10002,"msg":"invalid token"}`, false, false},
		{"undeterminable 500", 500, `{"code":500,"msg":"oops"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			oldOrigin, oldClient := profileCN.Origin, cfg.HttpClient
			profileCN.Origin, cfg.HttpClient = server.URL, server.Client()
			defer func() { profileCN.Origin, cfg.HttpClient = oldOrigin, oldClient }()

			usable, err := verifyAccountUsable(t.Context(), StoredAuth{Auth: StoredTokens{AccessToken: "token"}})
			if usable != tc.wantUsable || (err != nil) != tc.wantErr {
				t.Fatalf("usable=%v err=%v want usable=%v err=%v", usable, err, tc.wantUsable, tc.wantErr)
			}
		})
	}
	// When the token is missing, no request may be issued and the credential must not be judged usable.
	if usable, err := verifyAccountUsable(t.Context(), StoredAuth{}); usable || err == nil {
		t.Fatal("missing access token should return unusable with a reason")
	}
}

func TestDisableAccountKeepsOrDeletesBasedOnVerification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantDeleted bool
	}{
		{"credential still usable, keep", 200, `{"code":0,"data":{}}`, false},
		{"undeterminable, conservatively keep", 500, `{"code":500,"msg":"oops"}`, false},
		{"plain 403 keeps file but stops scheduling", 403, `{"code":10002,"msg":"forbidden"}`, false},
		{"explicitly invalid token 403 deletes", 403, `{"code":10002,"msg":"invalid token"}`, true},
		{"confirmed expiry deletes", 401, `{"code":12153,"msg":"invalid_grant"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer still-valid" {
					t.Error("missing Authorization header")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			oldOrigin, oldClient := profileCN.Origin, cfg.HttpClient
			profileCN.Origin, cfg.HttpClient = server.URL, server.Client()
			defer func() { profileCN.Origin, cfg.HttpClient = oldOrigin, oldClient }()

			authPath := filepath.Join(t.TempDir(), "workbuddy-test.json")
			if err := os.WriteFile(authPath, []byte(`{"auth":{"accessToken":"still-valid"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			acc := &Account{Path: authPath, Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "still-valid"}}}

			disableAccount(acc, "test")

			if !acc.Disabled {
				t.Fatal("scheduling should stop regardless of deletion")
			}
			_, statErr := os.Stat(authPath)
			if deleted := os.IsNotExist(statErr); deleted != tc.wantDeleted {
				t.Fatalf("deleted=%v want=%v", deleted, tc.wantDeleted)
			}
			if _, err := os.Stat(markerPath(authPath)); err != nil {
				t.Fatalf("expiry marker should always be written: %v", err)
			}
		})
	}
}

// When the credential file still exists but an expiry marker is present, the account must remain disabled after restart and must not be rescheduled.
func TestDisabledMarkerWinsWhenCredentialFileRemains(t *testing.T) {
	chdirTemp(t)
	// Only a relative path inside the working directory reaches the real "auto-discovery" scan path.
	path := "workbuddy1.json"
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"kept"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// The marker must be newer than the credential file, meaning "the credential was not overwritten by a later re-login".
	marker := disabledMarker{Path: path, Reason: "test kept", DisabledAt: time.Now().Unix()}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(path), data, 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(markerPath(path), later, later); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: path, Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "kept"}}}}
	loadDisabledMarkers()
	got := accounts[0]
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})

	if !got.Disabled {
		t.Fatal("with an expiry marker present, the account must stay disabled even though the credential file remains")
	}
	if got.DisabledReason != "test kept" {
		t.Fatalf("expiry reason not restored: %q", got.DisabledReason)
	}
}

// When the credential file is newer than the marker, the user has re-logged in or manually renewed, and the old marker should be cleared automatically.
func TestStaleDisabledMarkerClearedWhenCredentialIsNewer(t *testing.T) {
	chdirTemp(t)
	path := "workbuddy2.json"
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"renewed"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	marker := disabledMarker{Path: path, Reason: "old marker", DisabledAt: time.Now().Add(-time.Hour).Unix()}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(path), data, 0600); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(markerPath(path), older, older); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: path, Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "renewed"}}}}
	loadDisabledMarkers()
	got := accounts[0]
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})

	if got.Disabled {
		t.Fatal("account should not be judged expired after the credential file was updated")
	}
	if _, err := os.Stat(markerPath(path)); !os.IsNotExist(err) {
		t.Fatalf("expired marker should be cleared: %v", err)
	}
}
