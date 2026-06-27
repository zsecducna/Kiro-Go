package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempCache writes content to a temp file and returns its path.
func writeTempCache(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kiro-auth-token.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp cache: %v", err)
	}
	return path
}

// TestReadIdeCacheCredentialExternalIdp verifies the Kiro IDE's camelCase
// external_idp cache maps onto a complete importCredentialRequest.
func TestReadIdeCacheCredentialExternalIdp(t *testing.T) {
	path := writeTempCache(t, `{
	  "accessToken": "at-ide",
	  "refreshToken": "rt-ide",
	  "expiresAt": "2026-06-26T09:46:28.654Z",
	  "authMethod": "external_idp",
	  "provider": "ExternalIdp",
	  "tokenEndpoint": "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
	  "issuerUrl": "https://login.microsoftonline.com/tenant/v2.0",
	  "clientId": "azure-client-123",
	  "scopes": "api://azure-client-123/codewhisperer:conversations offline_access"
	}`)

	req, err := readIdeCacheCredential(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.AuthMethod != "external_idp" {
		t.Fatalf("authMethod = %q, want external_idp", req.AuthMethod)
	}
	if req.RefreshToken != "rt-ide" {
		t.Fatalf("refreshToken = %q", req.RefreshToken)
	}
	if req.TokenEndpoint == "" || req.ClientID != "azure-client-123" {
		t.Fatalf("missing external_idp material: endpoint=%q clientId=%q", req.TokenEndpoint, req.ClientID)
	}
	if !strings.Contains(req.Scopes, "offline_access") {
		t.Fatalf("scopes = %q", req.Scopes)
	}
}

// TestReadIdeCacheCredentialMissingFile returns an actionable error, not a panic.
func TestReadIdeCacheCredentialMissingFile(t *testing.T) {
	_, err := readIdeCacheCredential(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if _, ok := err.(*importValidationError); !ok {
		t.Fatalf("expected *importValidationError, got %T", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want 'not found'", err.Error())
	}
}

// TestReadIdeCacheCredentialNoRefreshToken rejects a cache lacking refresh material.
func TestReadIdeCacheCredentialNoRefreshToken(t *testing.T) {
	path := writeTempCache(t, `{"accessToken":"at","authMethod":"social"}`)
	_, err := readIdeCacheCredential(path)
	if err == nil {
		t.Fatal("expected error when refreshToken absent")
	}
	if !strings.Contains(err.Error(), "refreshToken") {
		t.Fatalf("error = %q, want 'refreshToken'", err.Error())
	}
}

// TestReadIdeCacheCredentialExternalIdpMissingEndpoint rejects an external_idp
// cache that lacks the tokenEndpoint/clientId needed to refresh.
func TestReadIdeCacheCredentialExternalIdpMissingEndpoint(t *testing.T) {
	path := writeTempCache(t, `{"accessToken":"at","refreshToken":"rt","authMethod":"external_idp"}`)
	_, err := readIdeCacheCredential(path)
	if err == nil {
		t.Fatal("expected error for external_idp without tokenEndpoint/clientId")
	}
	if !strings.Contains(err.Error(), "tokenEndpoint") {
		t.Fatalf("error = %q, want 'tokenEndpoint'", err.Error())
	}
}

// TestIdeCachePathPrecedence verifies explicit > env > default resolution.
func TestIdeCachePathPrecedence(t *testing.T) {
	if got := ideCachePath("/explicit/path.json"); got != "/explicit/path.json" {
		t.Fatalf("explicit arg should win, got %q", got)
	}
	t.Setenv("KIRO_IDE_CACHE", "/env/path.json")
	if got := ideCachePath(""); got != "/env/path.json" {
		t.Fatalf("env should win when no explicit arg, got %q", got)
	}
	t.Setenv("KIRO_IDE_CACHE", "")
	got := ideCachePath("")
	if !strings.HasSuffix(got, defaultIdeCacheRelPath) {
		t.Fatalf("default path should end with %q, got %q", defaultIdeCacheRelPath, got)
	}
}
