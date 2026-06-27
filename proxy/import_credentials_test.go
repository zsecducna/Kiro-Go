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

// TestNormalizeImportAuthMethod pins the auth-method normalization for import,
// including the key regression: external_idp accounts carry clientId but NO
// clientSecret, so the old default branch misclassified them as "social".
func TestNormalizeImportAuthMethod(t *testing.T) {
	cases := []struct {
		name           string
		authMethod     string
		clientID       string
		clientSecret   string
		tokenEndpoint  string
		want           string
	}{
		{"explicit external_idp", "external_idp", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"azure alias", "AzureAD", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"microsoft alias", "microsoft", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"inferred from tokenEndpoint", "", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"external_idp even with clientSecret", "external_idp", "c", "s", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"enterprise stays idc", "enterprise", "c", "s", "", "idc"},
		{"idc with clientid+secret", "idc", "c", "s", "", "idc"},
		{"empty + clientid (no secret) -> idc", "", "c", "", "", "idc"},
		{"empty no clientid -> social", "", "", "", "", "social"},
		{"social explicit", "social", "", "", "", "social"},
		{"google alias", "google", "", "", "", "social"},
		{"unrecognized with clientid+secret -> idc", "weird", "c", "s", "", "idc"},
		{"unrecognized without secret -> social", "weird", "c", "", "", "social"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeImportAuthMethod(tc.authMethod, tc.clientID, tc.clientSecret, tc.tokenEndpoint); got != tc.want {
				t.Fatalf("normalizeImportAuthMethod(%q,%q,%q,%q) = %q, want %q",
					tc.authMethod, tc.clientID, tc.clientSecret, tc.tokenEndpoint, got, tc.want)
			}
		})
	}
}

// TestApiImportCredentialsExternalIdpHappyPath verifies an external_idp credential
// imports successfully: authMethod normalizes to external_idp, refresh hits the
// (fake) IdP token endpoint, and the account is persisted with all refresh material.
func TestApiImportCredentialsExternalIdpHappyPath(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Errorf("expected grant_type=refresh_token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// external IdP token responses are snake_case.
		fmt.Fprintf(w, `{"access_token":"at-ext","refresh_token":"rt-rotated","expires_in":%d}`, upstreamExpiresIn)
	}))
	defer fake.Close()

	// fake.URL is http + 127.0.0.1; bypass the allow-list validator for this test.
	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	h := &Handler{pool: accountpool.GetPool()}

	body := fmt.Sprintf(`{"authMethod":"external_idp","refreshToken":"rt-ext","clientId":"ext-client","tokenEndpoint":%q,"issuerUrl":"https://login.microsoftonline.com/t/v2.0","scopes":"api://x/codewhisperer:conversations offline_access","region":"eu-central-1"}`, fake.URL)
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	before := time.Now().Unix()
	h.apiImportCredentials(rec, req)
	after := time.Now().Unix()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "external_idp" {
		t.Fatalf("AuthMethod: want external_idp, got %q", got.AuthMethod)
	}
	if got.AccessToken != "at-ext" {
		t.Fatalf("AccessToken: want at-ext, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-rotated" {
		t.Fatalf("RefreshToken: want rt-rotated (rotated), got %q", got.RefreshToken)
	}
	if got.TokenEndpoint != fake.URL {
		t.Fatalf("TokenEndpoint not persisted: got %q", got.TokenEndpoint)
	}
	if got.ClientID != "ext-client" {
		t.Fatalf("ClientID not persisted: got %q", got.ClientID)
	}
	if got.Scopes == "" {
		t.Fatalf("Scopes not persisted: got %q", got.Scopes)
	}
	if got.Provider != "AzureAD" {
		t.Fatalf("Provider default: want AzureAD, got %q", got.Provider)
	}
	if got.Region != "eu-central-1" {
		t.Fatalf("Region: want eu-central-1, got %q", got.Region)
	}
	if got.ExpiresAt < before+upstreamExpiresIn-5 || got.ExpiresAt > after+upstreamExpiresIn+5 {
		t.Fatalf("ExpiresAt not from upstream expiresIn: got %d (want ~now+%d)", got.ExpiresAt, upstreamExpiresIn)
	}
}

// TestApiImportCredentialsExternalIdpRejectsNonAllowListedEndpoint verifies the SSRF
// guard: a tokenEndpoint outside the IdP allow-list is rejected with 400 before any
// refresh POST, and nothing is persisted. (Validator is NOT bypassed here.)
func TestApiImportCredentialsExternalIdpRejectsNonAllowListedEndpoint(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"authMethod":"external_idp","refreshToken":"rt","clientId":"c","tokenEndpoint":"https://evil.example.com/oauth/token","region":"us-east-1"}`
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
	if !strings.Contains(resp["error"], "endpoint rejected") {
		t.Fatalf("expected endpoint-rejected error, got %q", resp["error"])
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no account persisted, got %d", len(accs))
	}
}

// TestApiImportCredentialsExternalIdpRejectsWhenRefreshFails verifies the refresh
// gate holds for external_idp: a refresh that 400s (invalid_grant) must reject the
// import and persist nothing.
func TestApiImportCredentialsExternalIdpRejectsWhenRefreshFails(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer fake.Close()

	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	h := &Handler{pool: accountpool.GetPool()}

	body := fmt.Sprintf(`{"authMethod":"external_idp","refreshToken":"rt-broken","clientId":"c","tokenEndpoint":%q,"region":"us-east-1"}`, fake.URL)
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
	if !strings.Contains(resp["error"], "Token refresh failed") {
		t.Fatalf("expected refresh-failed error, got %q", resp["error"])
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no account persisted, got %d", len(accs))
	}
}

// TestApiImportCredentialsExternalIdpPreservesFullRecordIdentity verifies that when
// a full account record (with id/email/profileArn) is pasted, those are preserved
// rather than regenerated, so re-importing a backup does not duplicate accounts.
func TestApiImportCredentialsExternalIdpPreservesFullRecordIdentity(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-ext","refresh_token":"rt-rotated","expires_in":3600}`)
	}))
	defer fake.Close()

	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	h := &Handler{pool: accountpool.GetPool()}

	const providedID = "11111111-2222-3333-4444-555555555555"
	body := fmt.Sprintf(`{"id":%q,"email":"ada@example.com","profileArn":"arn:aws:codewhisperer:eu-central-1:1:profile/PRESERVED","authMethod":"external_idp","refreshToken":"rt","clientId":"c","tokenEndpoint":%q,"region":"eu-central-1"}`, providedID, fake.URL)
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetAccounts()[0]
	if got.ID != providedID {
		t.Fatalf("ID: want reused %q, got %q", providedID, got.ID)
	}
	if got.Email != "ada@example.com" {
		t.Fatalf("Email: want ada@example.com (GetUserInfo empty in test → fallback), got %q", got.Email)
	}
	if got.ProfileArn != "arn:aws:codewhisperer:eu-central-1:1:profile/PRESERVED" {
		t.Fatalf("ProfileArn: want preserved, got %q", got.ProfileArn)
	}
}
