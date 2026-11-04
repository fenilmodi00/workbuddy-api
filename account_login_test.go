package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveCredentialPathRejectsEscapes(t *testing.T) {
	chdirTemp(t)
	path, err := resolveCredentialPath("workbuddy4.json")
	if err != nil {
		t.Fatalf("valid file name was rejected: %v", err)
	}
	if filepath.Base(path) != "workbuddy4.json" || !filepath.IsAbs(path) {
		t.Fatalf("should resolve to an absolute path under the working directory: %s", path)
	}
	for _, bad := range []string{"", "  ", "other.json", "../workbuddy1.json", "sub/workbuddy1.json",
		`sub\workbuddy1.json`, "workbuddy1.json/../workbuddy2.json", "workbuddy*.json"} {
		if _, err := resolveCredentialPath(bad); err == nil {
			t.Fatalf("should reject out-of-bounds or invalid credential names: %q", bad)
		}
	}
}

func TestSuggestCredentialFileNameSkipsUsed(t *testing.T) {
	chdirTemp(t)
	for _, name := range []string{"workbuddy1.json", "workbuddy3.json", "workbuddy3.json.disabled", "other.json"} {
		if err := os.WriteFile(name, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := suggestCredentialFileName(); got != "workbuddy2.json" {
		t.Fatalf("should skip used 1 and 3, got %s", got)
	}
}

func TestDeleteAccountCredentialGuardsAndRemoval(t *testing.T) {
	isolatedGatewayConfig(t)
	chdirTemp(t)
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "workbuddy7.json")
	if err := os.WriteFile(target, []byte(`{"auth":{"accessToken":"synthetic-not-a-token"},"account":{"uid":"u"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	old := accounts
	accounts = []*Account{{Path: target, Auth: &StoredAuth{}}}
	accountMu.Unlock()
	t.Cleanup(func() { accountMu.Lock(); accounts = old; accountMu.Unlock() })

	// Directories must not be deletable as credentials.
	if err := os.Mkdir(filepath.Join(dir, "workbuddy9.json"), 0700); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	accounts = append(accounts, &Account{Path: filepath.Join(dir, "workbuddy9.json"), Auth: &StoredAuth{}})
	accountMu.Unlock()
	if _, err := deleteAccountCredential("workbuddy9.json"); err == nil {
		t.Fatal("directories must be rejected, no recursive deletion")
	}

	for _, bad := range []string{"", "../x.json", "other.json", "missing.json"} {
		if _, err := deleteAccountCredential(bad); err == nil {
			t.Fatalf("should refuse to delete: %q", bad)
		}
	}
	if _, err := deleteAccountCredential("workbuddy7.json"); err != nil {
		t.Fatalf("normal deletion failed: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("credential file was not deleted")
	}
}

// Accounts whose authorization expired stay in the account pool as Disabled entries (the credential file may
// already be deleted, leaving only the expiry marker to prompt a re-login). Explicit deletion from the web UI
// must remove the account entry too, otherwise both the web UI and CLI keep showing the deleted workbuddyN.json.
func TestDeleteAccountCredentialRemovesDisabledPhantom(t *testing.T) {
	isolatedGatewayConfig(t)
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "workbuddy9.json")
	marker := disabledMarker{
		Path: target, Reason: "upstream authentication failed (HTTP 403)", Nickname: "synthetic-user",
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(target), raw, 0600); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	old := accounts
	accounts = []*Account{{Path: target, Disabled: true, DisabledReason: marker.Reason, Nickname: marker.Nickname}}
	accountMu.Unlock()
	t.Cleanup(func() { accountMu.Lock(); accounts = old; accountMu.Unlock() })
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}

	if _, err := deleteAccountCredential("workbuddy9.json"); err != nil {
		t.Fatalf("failed to delete disabled account: %v", err)
	}
	accountMu.Lock()
	remaining := len(accounts)
	accountMu.Unlock()
	if remaining != 0 {
		t.Fatalf("disabled account should be removed from the account pool, %d remaining", remaining)
	}
	if _, err := os.Stat(markerPath(target)); !os.IsNotExist(err) {
		t.Fatal("expiry marker should be cleared")
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, acc := range snap.Accounts {
		if filepath.Base(acc.Path) == "workbuddy9.json" {
			t.Fatal("status snapshot still contains the deleted account; the web UI would keep showing it")
		}
	}
}

func TestAccountAddEndpointValidatesInputWithoutUpstream(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-account-admin"}); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t)
	handler := newWebUIHandler()
	if got := adminRequest(handler, "GET", "/admin/api/account-add", "", ""); got.Code != 401 {
		t.Fatalf("unauthenticated requests must be rejected: %d", got.Code)
	}
	got := adminRequest(handler, "GET", "/admin/api/account-add", "synthetic-account-admin", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), "defaultFile") {
		t.Fatalf("failed to read default name: %d %s", got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), "/") {
		t.Fatal("default name must not leak directory information")
	}
	for _, body := range []string{`{"site":"mars","file":"workbuddy1.json"}`,
		`{"site":"cn","file":"../workbuddy1.json"}`, `{"site":"cn","file":"other.json"}`} {
		got = adminRequest(handler, "POST", "/admin/api/account-add", "synthetic-account-admin", body)
		if got.Code != 400 {
			t.Fatalf("invalid parameters must give 400: %s -> %d %s", body, got.Code, got.Body.String())
		}
	}
	if err := os.WriteFile("workbuddy1.json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got = adminRequest(handler, "POST", "/admin/api/account-add", "synthetic-account-admin",
		`{"site":"cn","file":"workbuddy1.json"}`)
	if got.Code != 409 {
		t.Fatalf("same-name credentials must refuse overwrite: %d %s", got.Code, got.Body.String())
	}
	got = adminRequest(handler, "POST", "/admin/api/account-delete", "synthetic-account-admin", `{"file":"../x.json"}`)
	if got.Code != 400 {
		t.Fatalf("out-of-bounds delete path must give 400: %d", got.Code)
	}
	got = adminRequest(handler, "DELETE", "/admin/api/account-delete", "synthetic-account-admin", "")
	// Admin write operations are protected by the same-origin JSON POST policy; DELETE must be blocked before reaching the business logic.
	if got.Code == 200 {
		t.Fatalf("delete endpoint must not accept DELETE: %d %s", got.Code, got.Body.String())
	}
}

func TestLoginSessionSnapshotReportsRemainingTime(t *testing.T) {
	s := &loginSession{
		ID: "login-snapshot-test", Site: "cn", File: "workbuddy1.json",
		AuthURL: "https://example.test/login", status: "pending",
		deadline: time.Now().Add(90 * time.Second),
	}
	snap := s.snapshot()
	if snap["status"] != "pending" {
		t.Fatalf("initial status should be pending: %#v", snap)
	}
	remaining, ok := snap["remainingSeconds"].(int)
	if !ok || remaining < 85 || remaining > 90 {
		t.Fatalf("remaining time is wrong: %#v", snap["remainingSeconds"])
	}

	s.finish("expired", "Login timed out, please request the login endpoint again", nil, "")
	snap = s.snapshot()
	if snap["status"] != "expired" {
		t.Fatalf("final status should be expired: %#v", snap)
	}
	if got := snap["remainingSeconds"]; got != 0 {
		t.Fatalf("finished sessions must not show remaining time: %#v", got)
	}
}

func TestFinishedLoginSessionRetainedForWebPolling(t *testing.T) {
	oldRetention := loginSessionRetention
	loginSessionRetention = 40 * time.Millisecond
	t.Cleanup(func() { loginSessionRetention = oldRetention })

	s := &loginSession{
		ID: "login-retention-test", Site: "cn", File: "workbuddy1.json",
		status: "pending", deadline: time.Now().Add(-time.Second),
	}
	loginSessionsMu.Lock()
	loginSessions[s.ID] = s
	loginSessionsMu.Unlock()
	t.Cleanup(func() { forgetLoginSession(s.ID) })

	s.finish("expired", "Login timed out, please request the login endpoint again", nil, "")
	scheduleForgetLoginSession(s.ID)
	if findLoginSession(s.ID) == nil {
		t.Fatal("finished sessions should remain immediately pollable by the web UI for the final state")
	}

	time.Sleep(150 * time.Millisecond)
	if findLoginSession(s.ID) != nil {
		t.Fatal("sessions should be cleaned up after the retention period")
	}
}

func TestAccountAddEndpointReturnsExpiredStatus(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-account-admin"}); err != nil {
		t.Fatal(err)
	}
	s := &loginSession{
		ID: "login-expired-endpoint", Site: "cn", File: "workbuddy1.json",
		status: "expired", deadline: time.Now().Add(-time.Minute),
	}
	loginSessionsMu.Lock()
	loginSessions[s.ID] = s
	loginSessionsMu.Unlock()
	t.Cleanup(func() { forgetLoginSession(s.ID) })

	handler := newWebUIHandler()
	got := adminRequest(handler, "GET", "/admin/api/account-add?id="+s.ID, "synthetic-account-admin", "")
	if got.Code != 200 {
		t.Fatalf("querying a finished session should return 200: %d %s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"status":"expired"`) {
		t.Fatalf("web polling should be able to read the expired final state: %s", got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"remainingSeconds":0`) {
		t.Fatalf("finished sessions should not return remaining time: %s", got.Body.String())
	}
}
