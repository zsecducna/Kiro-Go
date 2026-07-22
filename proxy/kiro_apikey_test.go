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
	// model-fetch goroutine making live network calls from the test; the probe stub
	// keeps the region-discovery step (no region is sent here) off the network too.
	stubProbeKiroApiKey(t, "us-east-1")
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
	// The explicit region is validated against the key, so keep that probe off the
	// network: the stub serves exactly the region this record was exported with.
	stubProbeKiroApiKey(t, "eu-west-1")

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

// stubProbeKiroApiKey swaps the upstream region probe for a deterministic fake and
// restores the original when the test ends. Returns a pointer to a call counter so
// tests can assert the probe was (or was not) consulted.
func stubProbeKiroApiKey(t *testing.T, serves string) *int {
	t.Helper()
	calls := 0
	orig := probeKiroApiKey
	t.Cleanup(func() { probeKiroApiKey = orig })
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		calls++
		if region != serves {
			return nil, errTest("HTTP 403: not served in " + region)
		}
		return &config.AccountInfo{Email: "probed@example.com", UserId: "kiro-user-probe"}, nil
	}
	return &calls
}

// findAccountByKey returns the persisted account holding key, or nil.
func findAccountByKey(key string) *config.Account {
	all := config.GetAccounts()
	for i := range all {
		if all[i].KiroApiKey == key {
			return &all[i]
		}
	}
	return nil
}

// A region-less ksk_ add must DISCOVER the region the key actually serves rather
// than stamping the us-east-1 default. Regression: the panel deliberately omits
// region for api_key adds, so an EU-provisioned key was pinned to us-east-1 and
// every getUsageLimits call 403'd forever (api_key accounts never re-probe region,
// because ResolveProfileArn short-circuits for key-bound profiles).
func TestApiAddAccountApiKeyDiscoversRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	stubProbeKiroApiKey(t, "eu-central-1")

	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_eu","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	stored := findAccountByKey("ksk_eu")
	if stored == nil {
		t.Fatal("api_key account not persisted")
	}
	if stored.Region != "eu-central-1" {
		t.Fatalf("expected discovered region eu-central-1, got %q", stored.Region)
	}
}

// A key that serves no candidate region must be rejected outright. Persisting it
// would create a permanently-403ing pool slot that only surfaces later as an
// unexplained upstream failure.
func TestApiAddAccountApiKeyRejectsUnusableKey(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	stubProbeKiroApiKey(t, "no-such-region")

	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_dead","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for key serving no region, got %d: %s", rec.Code, rec.Body.String())
	}
	if findAccountByKey("ksk_dead") != nil {
		t.Fatal("unusable key must not be persisted")
	}
}

// An explicit region narrows the probe to that one region and is validated, not
// trusted: a mistyped or stale region would otherwise persist as a permanently-403ing
// slot, which is the same bug class as the us-east-1 guess. Mirrors the bot route.
func TestApiAddAccountApiKeyRejectsWrongExplicitRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	calls := stubProbeKiroApiKey(t, "eu-central-1")

	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_pinned","region":"ap-southeast-1","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a region the key does not serve, got %d: %s", rec.Code, rec.Body.String())
	}
	if findAccountByKey("ksk_pinned") != nil {
		t.Fatal("key must not be persisted for a region it does not serve")
	}
	// Only the named region is probed — an explicit region must not silently fall
	// back to a candidate the caller did not ask for.
	if *calls != 1 {
		t.Fatalf("expected exactly 1 probe (the named region), got %d", *calls)
	}
}

// A correct explicit region is accepted and preserved.
func TestApiAddAccountApiKeyAcceptsValidExplicitRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	stubProbeKiroApiKey(t, "ap-southeast-1")

	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_ok","region":"ap-southeast-1","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored := findAccountByKey("ksk_ok")
	if stored == nil {
		t.Fatal("api_key account not persisted")
	}
	if stored.Region != "ap-southeast-1" {
		t.Fatalf("explicit region must be preserved, got %q", stored.Region)
	}
	// The probe's identity is kept rather than discarded.
	if stored.Email != "probed@example.com" {
		t.Fatalf("expected probed identity to be persisted, got email %q", stored.Email)
	}
}

// A transient upstream failure must not be reported as a bad key: the caller needs
// 502 (retry) rather than 400, or a blip during a restore looks like a dead key.
func TestApiAddAccountApiKeyTransientProbeFailureIs502(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	orig := probeKiroApiKey
	t.Cleanup(func() { probeKiroApiKey = orig })
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		return nil, errTest("GetUsageLimits: dial tcp: connection refused")
	}

	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_blip","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for transient upstream failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if findAccountByKey("ksk_blip") != nil {
		t.Fatal("key must not be persisted when its region could not be determined")
	}
}

// The import path carries the same trap as apiAddAccount: a region-less api_key
// record must discover its region instead of defaulting to us-east-1.
func TestApiImportCredentialsApiKeyDiscoversRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	stubProbeKiroApiKey(t, "eu-central-1")

	r := httptest.NewRequest(http.MethodPost, "/admin/api/auth/credentials",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_importeu"}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	stored := findAccountByKey("ksk_importeu")
	if stored == nil {
		t.Fatal("imported api_key account not persisted")
	}
	if stored.Region != "eu-central-1" {
		t.Fatalf("expected discovered region eu-central-1, got %q", stored.Region)
	}
	// UserId drives findAccountForKiroIdentity; dropping it lets a later supply-side
	// add of the same Kiro account create a second pool slot.
	if stored.UserId != "kiro-user-probe" {
		t.Fatalf("expected probed UserId to be persisted, got %q", stored.UserId)
	}
}

// Mixed outcome: one region rejects the key outright, another fails transiently. The
// key may well live in the region that timed out, so this must report 502 (retry) and
// never 400 (bad key).
func TestApiAddAccountApiKeyMixedProbeFailureIs502(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	orig := probeKiroApiKey
	t.Cleanup(func() { probeKiroApiKey = orig })
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if region == "us-east-1" {
			return nil, errTest("GetUsageLimits: HTTP 403: {\"message\":\"The bearer token included in the request is invalid.\"}")
		}
		return nil, errTest("GetUsageLimits: context deadline exceeded")
	}

	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_mixed","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when a candidate region failed transiently, got %d: %s", rec.Code, rec.Body.String())
	}
	if findAccountByKey("ksk_mixed") != nil {
		t.Fatal("key must not be persisted when its region could not be determined")
	}
}

// A key rejected (403) by every candidate region is a caller error, not an upstream
// one: it must report 400 so a bot does not retry a dead key forever.
func TestApiAddAccountApiKeyAllAuthFailuresIs400(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("pw")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	orig := probeKiroApiKey
	t.Cleanup(func() { probeKiroApiKey = orig })
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		return nil, errTest("GetUsageLimits: HTTP 403: {\"message\":\"The bearer token included in the request is invalid.\"}")
	}

	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts",
		strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_revoked","enabled":false}`))
	r.Header.Set("X-Admin-Password", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when every region auth-rejected the key, got %d: %s", rec.Code, rec.Body.String())
	}
}
