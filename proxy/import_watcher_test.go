package proxy

import (
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newExternalIdpTokenServer stands up a fake IdP token endpoint that answers the
// refresh_token grant with a snake_case OAuth2 token response, mirroring Azure AD.
func newExternalIdpTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-watch","refresh_token":"rt-watch-rotated","expires_in":3600}`)
	}))
}

// writeHelperFile writes a helper-style external_idp credential JSON into dir and
// backdates its mtime so the watcher's "still being written" guard does not skip it.
func writeHelperFile(t *testing.T, dir, name, tokenEndpoint, refreshToken, email string) {
	t.Helper()
	body := fmt.Sprintf(`{
	  "auth_method": "external_idp",
	  "client_id": "azure-client",
	  "refresh_token": %q,
	  "region": "us-east-1",
	  "token_endpoint": %q,
	  "issuer_url": "https://login.microsoftonline.com/tenant/v2.0",
	  "scopes": "offline_access",
	  "email": %q,
	  "type": "kiro"
	}`, refreshToken, tokenEndpoint, email)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write helper file: %v", err)
	}
}

// TestImportWatcherProcessesValidFile verifies a valid drop is imported and the
// file is moved into processed/.
func TestImportWatcherProcessesValidFile(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	idp := newExternalIdpTokenServer(t)
	defer idp.Close()

	dir := t.TempDir()
	writeHelperFile(t, dir, "CLIProxyAPI_user.json", idp.URL, "rt-watch", "user@example.com")

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account imported, got %d", len(accs))
	}
	if accs[0].AuthMethod != "external_idp" {
		t.Fatalf("expected external_idp, got %q", accs[0].AuthMethod)
	}

	// File must have moved into processed/ and be gone from the root.
	if _, err := os.Stat(filepath.Join(dir, "CLIProxyAPI_user.json")); !os.IsNotExist(err) {
		t.Fatalf("expected source file removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "processed", "CLIProxyAPI_user.json")); err != nil {
		t.Fatalf("expected file in processed/, got %v", err)
	}
}

// TestImportWatcherMovesInvalidFileToFailed verifies a malformed file is moved to
// failed/ with an .error.txt sidecar and no account is created.
func TestImportWatcherMovesInvalidFileToFailed(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("write broken file: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no accounts, got %d", len(accs))
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", "broken.json")); err != nil {
		t.Fatalf("expected file in failed/, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", "broken.json"+importErrorSidecarSuffix)); err != nil {
		t.Fatalf("expected .error.txt sidecar, got %v", err)
	}
}

// TestImportWatcherSkipsDuplicate verifies a credential whose refresh token already
// exists is skipped (no duplicate account), and the file is still cleared to processed/.
func TestImportWatcherSkipsDuplicate(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	// Seed an existing account whose refresh token matches the drop.
	if err := config.AddAccount(config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        "dup@example.com",
		AuthMethod:   "external_idp",
		RefreshToken: "rt-dup",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	idp := newExternalIdpTokenServer(t)
	defer idp.Close()

	dir := t.TempDir()
	writeHelperFile(t, dir, "dup.json", idp.URL, "rt-dup", "dup@example.com")

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	// Still exactly one account — the duplicate was skipped, not added.
	if accs := config.GetAccounts(); len(accs) != 1 {
		t.Fatalf("expected duplicate skipped (1 account), got %d", len(accs))
	}
	// An all-duplicate file is cleared to processed/ so it is not retried forever.
	if _, err := os.Stat(filepath.Join(dir, "processed", "dup.json")); err != nil {
		t.Fatalf("expected duplicate-only file moved to processed/, got %v", err)
	}
}
