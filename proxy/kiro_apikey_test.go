package proxy

import (
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// applyKiroBaseHeaders must use the Kiro API key as the bearer token and set the
// "tokentype: API_KEY" header for api_key accounts, never leaking AccessToken as
// a separate credential.
func TestApplyKiroBaseHeadersApiKeyAccount(t *testing.T) {
	account := &config.Account{
		AuthMethod: "api_key",
		KiroApiKey: "ksk_secretvalue",
		// AccessToken mirrors the key for pool compatibility; the header must still
		// come from KiroApiKey and carry the tokentype marker.
		AccessToken: "ksk_secretvalue",
	}
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	applyKiroBaseHeaders(req, account, kiroHeaderValues{})

	if got := req.Header.Get("Authorization"); got != "Bearer ksk_secretvalue" {
		t.Fatalf("expected bearer from KiroApiKey, got %q", got)
	}
	if got := req.Header.Get("tokentype"); got != "API_KEY" {
		t.Fatalf("expected tokentype API_KEY, got %q", got)
	}
}

// A plain api_key account with an empty KiroApiKey must not emit an Authorization
// header (defensive: never send "Bearer ").
func TestApplyKiroBaseHeadersApiKeyEmptyKey(t *testing.T) {
	account := &config.Account{AuthMethod: "api_key"}
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	applyKiroBaseHeaders(req, account, kiroHeaderValues{})

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("expected no Authorization header for empty api key, got %q", got)
	}
	if got := req.Header.Get("tokentype"); got != "" {
		t.Fatalf("expected no tokentype header for empty api key, got %q", got)
	}
}

// OAuth accounts keep the legacy behavior: AccessToken as bearer, no tokentype.
func TestApplyKiroBaseHeadersOAuthAccountUnchanged(t *testing.T) {
	account := &config.Account{AuthMethod: "social", AccessToken: "oauth-token"}
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	applyKiroBaseHeaders(req, account, kiroHeaderValues{})

	if got := req.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("expected bearer from AccessToken, got %q", got)
	}
	if got := req.Header.Get("tokentype"); got != "" {
		t.Fatalf("expected no tokentype for OAuth account, got %q", got)
	}
}

// IsApiKeyCredential is case-insensitive and trims surrounding whitespace.
func TestIsApiKeyCredential(t *testing.T) {
	cases := map[string]bool{
		"api_key": true, "API_KEY": true, "  api_key ": true,
		"social": false, "idc": false, "external_idp": false, "": false,
	}
	for method, want := range cases {
		a := &config.Account{AuthMethod: method}
		if got := a.IsApiKeyCredential(); got != want {
			t.Fatalf("IsApiKeyCredential(%q)=%v want %v", method, got, want)
		}
	}
}

// ensureValidToken must be a no-op for api_key accounts: ExpiresAt stays 0 so the
// refresh path returns immediately without calling upstream.
func TestEnsureValidTokenSkipsApiKeyAccount(t *testing.T) {
	h := &Handler{}
	account := &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_x", AccessToken: "ksk_x", ExpiresAt: 0}
	if err := h.ensureValidToken(account); err != nil {
		t.Fatalf("expected no refresh for api_key account, got %v", err)
	}
}

// apiAddAccount must accept an api_key account, normalize it (AuthMethod=api_key,
// AccessToken mirrors the key, ExpiresAt=0), and reject an empty key.
func TestApiAddAccountApiKeyBranch(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// Missing key → 400.
	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"  "}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty kiroApiKey, got %d: %s", rec.Code, rec.Body.String())
	}

	// Contradictory payload: key plus an explicit OAuth method → 400.
	r = httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"social","kiroApiKey":"ksk_x"}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for key+social, got %d", rec.Code)
	}

	// Valid key → stored and normalized. enabled:false avoids the background
	// model-fetch goroutine making live network calls from the test.
	r = httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_live","nickname":"hw1","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var stored *config.Account
	for i, a := range config.GetAccounts() {
		if a.KiroApiKey == "ksk_live" {
			stored = &config.GetAccounts()[i]
			break
		}
	}
	if stored == nil {
		t.Fatal("api_key account not persisted")
	}
	if stored.AuthMethod != "api_key" || stored.AccessToken != "ksk_live" || stored.ExpiresAt != 0 {
		t.Fatalf("account not normalized: %+v", stored)
	}
	if !stored.IsApiKeyCredential() {
		t.Fatal("expected IsApiKeyCredential true")
	}
}

// The credentials import path must accept an api_key record (no refreshToken) and
// normalize it, so an exported backup restores cleanly.
func TestApiImportCredentialsApiKey(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	r := httptest.NewRequest(http.MethodPost, "/admin/api/auth/credentials",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_restore","region":"eu-west-1"}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 importing api_key, got %d: %s", rec.Code, rec.Body.String())
	}

	var stored *config.Account
	all := config.GetAccounts()
	for i := range all {
		if all[i].KiroApiKey == "ksk_restore" {
			stored = &all[i]
			break
		}
	}
	if stored == nil {
		t.Fatal("imported api_key account not persisted")
	}
	if !stored.IsApiKeyCredential() || stored.AccessToken != "ksk_restore" || stored.ExpiresAt != 0 || stored.Region != "eu-west-1" {
		t.Fatalf("imported account not normalized: %+v", stored)
	}

	// Missing key on an api_key import → 400.
	r = httptest.NewRequest(http.MethodPost, "/admin/api/auth/credentials",
		strings.NewReader(`{"authMethod":"api_key"}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for api_key import without key, got %d", rec.Code)
	}
}
