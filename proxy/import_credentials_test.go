package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// installCleanAuthClient replaces the global auth HTTP client with one whose
// Transport does not consult http.ProxyFromEnvironment — that function caches
// env vars on first call and would otherwise poison TestBuildKiroTransport*
// when tests run in the default order. Returns a cleanup that restores the
// previous client.
func installCleanAuthClient(t *testing.T) func() {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	prev := auth.SetGlobalAuthClientForTest(c)
	return func() { auth.SetGlobalAuthClientForTest(prev) }
}

// TestApiImportCredentialsRejectsWhenRefreshFails verifies the regression:
// previously, when auth.RefreshToken failed and the user supplied an accessToken,
// the handler stored that accessToken with ExpiresAt = now+300, producing an
// account that the pool would skip (Pick uses now > ExpiresAt-120 → ~3 min) and
// that the on-demand refresh path could never repair (Pick filters it out before
// ensureValidToken runs). The fix is to reject the import outright; the caller
// must provide a refreshToken that actually works.
func TestApiImportCredentialsRejectsWhenRefreshFails(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	// Stand up a fake OIDC endpoint that always 400s, simulating an unreachable
	// or invalid refresh.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"refreshToken":"rt-broken","accessToken":"at-still-valid-elsewhere","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when refresh fails, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "Token refresh failed") {
		t.Fatalf("expected refresh-failed error, got %q", resp["error"])
	}

	// Crucial: no account should have been created. The previous bug stored a
	// half-broken account with ExpiresAt ~now+300 that would die in 3 minutes.
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no accounts to be persisted on failed import, got %+v", accs)
	}
}

// TestApiImportCredentialsUsesUpstreamExpiresAt verifies the happy path: when
// refresh succeeds, the persisted ExpiresAt reflects the upstream expiresIn,
// not a hard-coded 300s.
func TestApiImportCredentialsUsesUpstreamExpiresAt(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":"at-new","refreshToken":"rt-rotated","expiresIn":%d,"profileArn":"arn:aws:codewhisperer:profile/test"}`, upstreamExpiresIn)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	before := time.Now().Unix()
	body := `{"refreshToken":"rt-good","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)
	after := time.Now().Unix()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on successful refresh, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.AccessToken != "at-new" {
		t.Fatalf("expected upstream-issued accessToken, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-rotated" {
		t.Fatalf("expected rotated refreshToken to be persisted, got %q", got.RefreshToken)
	}
	// Allow ±5s of drift but require the value to clearly come from upstream's
	// expiresIn rather than the old 300s fallback.
	expectMin := before + upstreamExpiresIn - 5
	expectMax := after + upstreamExpiresIn + 5
	if got.ExpiresAt < expectMin || got.ExpiresAt > expectMax {
		t.Fatalf("expected ExpiresAt ≈ now+%d ([%d..%d]), got %d", upstreamExpiresIn, expectMin, expectMax, got.ExpiresAt)
	}
	if got.ExpiresAt-time.Now().Unix() < 1500 {
		t.Fatalf("ExpiresAt too short — looks like the 300s fallback is still in play: %d (delta %d)", got.ExpiresAt, got.ExpiresAt-time.Now().Unix())
	}
}

// authOidcURL captures the current oidc URL builder so the test can restore it.
func authOidcURL() func(string) string { return auth.GetOIDCTokenURLForTest() }

// TestApiImportCredentialsExternalIdpHappyPath verifies that a raw helper
// (kiro-login-helper.py) external_idp document — snake_case keys, token_endpoint,
// issuer_url, scopes, profile_arn — is decoded, refreshed against the IdP token
// endpoint, and persisted as a complete external_idp account. This is the exact
// path that previously 400'd because apiImportCredentials dropped token_endpoint.
func TestApiImportCredentialsExternalIdpHappyPath(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	// external_idp refreshes against the IdP token endpoint (snake_case OAuth2
	// response), NOT the AWS OIDC endpoint. Stand up that token server and pass
	// its URL as token_endpoint in the helper JSON.
	var gotGrant, gotClientID string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotClientID = r.Form.Get("client_id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at-idp","refresh_token":"rt-idp-rotated","expires_in":%d}`, upstreamExpiresIn)
	}))
	defer idp.Close()

	h := &Handler{pool: accountpool.GetPool()}

	body := `{
	  "access_token": "stale",
	  "auth_method": "external_idp",
	  "client_id": "azure-client-123",
	  "refresh_token": "rt-idp",
	  "region": "us-east-1",
	  "profile_arn": "arn:aws:codewhisperer:us-east-1:000000000000:profile/SAMPLE",
	  "scopes": "api://azure-client-123/codewhisperer:conversations offline_access",
	  "token_endpoint": "` + idp.URL + `",
	  "issuer_url": "https://login.microsoftonline.com/tenant/v2.0",
	  "type": "kiro"
	}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if gotGrant != "refresh_token" {
		t.Fatalf("expected refresh_token grant at IdP, got %q", gotGrant)
	}
	if gotClientID != "azure-client-123" {
		t.Fatalf("expected client_id forwarded to IdP, got %q", gotClientID)
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "external_idp" {
		t.Fatalf("expected authMethod external_idp, got %q", got.AuthMethod)
	}
	if got.AccessToken != "at-idp" {
		t.Fatalf("expected IdP-issued accessToken, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-idp-rotated" {
		t.Fatalf("expected rotated refreshToken, got %q", got.RefreshToken)
	}
	if got.TokenEndpoint != idp.URL {
		t.Fatalf("expected tokenEndpoint persisted, got %q", got.TokenEndpoint)
	}
	if got.IssuerURL != "https://login.microsoftonline.com/tenant/v2.0" {
		t.Fatalf("expected issuerUrl persisted, got %q", got.IssuerURL)
	}
	if !strings.Contains(got.Scopes, "offline_access") {
		t.Fatalf("expected scopes persisted, got %q", got.Scopes)
	}
	// external_idp refresh returns "" for profileArn, so the helper-provided ARN
	// must be the fallback.
	if got.ProfileArn != "arn:aws:codewhisperer:us-east-1:000000000000:profile/SAMPLE" {
		t.Fatalf("expected helper profileArn fallback, got %q", got.ProfileArn)
	}
	if got.Provider != "AzureAD" {
		t.Fatalf("expected default provider AzureAD, got %q", got.Provider)
	}
}

// TestApiImportCredentialsExternalIdpRejectsMissingTokenEndpoint verifies the
// actionable up-front 400 (before any refresh attempt) when an external_idp
// import omits token_endpoint, and that no account is persisted.
func TestApiImportCredentialsExternalIdpRejectsMissingTokenEndpoint(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"auth_method":"external_idp","client_id":"azure-client","refresh_token":"rt","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "token_endpoint") {
		t.Fatalf("expected actionable token_endpoint error, got %q", resp["error"])
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no account persisted, got %d", len(accs))
	}
}

// TestApiImportCliJsonBatch verifies the dedicated helper-JSON endpoint imports a
// JSON array of external_idp documents and reports per-item results.
func TestApiImportCliJsonBatch(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt2","expires_in":3600}`)
	}))
	defer idp.Close()

	h := &Handler{pool: accountpool.GetPool()}

	body := `[
	  {"auth_method":"external_idp","client_id":"c1","refresh_token":"rt1","token_endpoint":"` + idp.URL + `","email":"a@example.com","type":"kiro"},
	  {"auth_method":"external_idp","client_id":"c2","refresh_token":"rt1","token_endpoint":"` + idp.URL + `","email":"b@example.com","type":"kiro"}
	]`
	req := httptest.NewRequest("POST", "/auth/import-cli-json", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCliJson(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success  bool                     `json:"success"`
		Imported []map[string]interface{} `json:"imported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || len(resp.Imported) != 2 {
		t.Fatalf("expected 2 imported, got success=%v imported=%d body=%s", resp.Success, len(resp.Imported), rec.Body.String())
	}
	if accs := config.GetAccounts(); len(accs) != 2 {
		t.Fatalf("expected 2 accounts persisted, got %d", len(accs))
	}
}
