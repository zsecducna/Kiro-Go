package proxy

import (
	"errors"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolveProfileArnReturnsCachedValueWithoutRequest(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request for cached profile ARN")
			return nil, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/test "}
	got, err := ResolveProfileArn(account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/test" {
		t.Fatalf("expected trimmed cached ARN, got %q", got)
	}
}

func TestRegionalizeURLPrefersProfileArnRegion(t *testing.T) {
	account := &config.Account{
		Region:     "ap-southeast-1",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}

	rawURL := "https://q.us-east-1.amazonaws.com/getUsageLimits?origin=AI_EDITOR"
	if got := regionalizeURL(rawURL, account); got != rawURL {
		t.Fatalf("expected profile ARN region to keep us-east-1 URL, got %q", got)
	}
}

func TestRegionalizeURLForProfileUsesPayloadProfileArnRegion(t *testing.T) {
	account := &config.Account{Region: "ap-southeast-1"}

	got := regionalizeURLForProfile(
		"https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		account,
		"arn:aws:codewhisperer:eu-central-1:123456789012:profile/test",
	)
	want := "https://q.eu-central-1.amazonaws.com/generateAssistantResponse"
	if got != want {
		t.Fatalf("expected payload profile ARN region URL %q, got %q", want, got)
	}
}

func TestResolveProfileArnFetchesAndCachesProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:           "acct-1",
		Email:        "user@example.com",
		AccessToken:  "access-token",
		Region:       "us-east-1",
		UsageCurrent: 7,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", req.Method)
			}
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("expected JSON content type, got %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"profiles":[{"arn":" arn:aws:codewhisperer:profile/fetched "}]} `)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	requestAccount := account
	requestAccount.UsageCurrent = 0
	got, err := ResolveProfileArn(&requestAccount)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/fetched" {
		t.Fatalf("expected fetched ARN, got %q", got)
	}
	if requestAccount.ProfileArn != got {
		t.Fatalf("expected account to be updated with fetched ARN, got %q", requestAccount.ProfileArn)
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one persisted account, got %d", len(accounts))
	}
	if accounts[0].ProfileArn != got {
		t.Fatalf("expected persisted account profile ARN %q, got %q", got, accounts[0].ProfileArn)
	}
	if accounts[0].UsageCurrent != 7 {
		t.Fatalf("expected profile cache update to preserve usage fields, got usageCurrent=%v", accounts[0].UsageCurrent)
	}
}

func TestResolveProfileArnSuppressesBuilderIDUnsupportedLookup(t *testing.T) {
	clearProfileArnResolutionCooldowns()
	t.Cleanup(clearProfileArnResolutionCooldowns)

	var calls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"AWS Builder ID is not supported for this operation.","reason":null}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{
		ID:          "builder-1",
		Email:       "builder@example.com",
		AccessToken: "access-token",
		Provider:    "BuilderId",
		Region:      "us-east-1",
	}

	_, err := ResolveProfileArn(account)
	if err == nil || !isProfileArnResolutionUnsupportedError(err) {
		t.Fatalf("expected Builder ID unsupported error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one profile lookup, got %d", got)
	}

	_, err = ResolveProfileArn(account)
	if err == nil || !isProfileArnResolutionSkippedError(err) {
		t.Fatalf("expected skipped profile ARN resolution error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected no repeated profile lookup after suppression, got %d", got)
	}
}

func TestResolveProfileArnKeepsRefreshFallbackForBuilderIDUnsupportedLookup(t *testing.T) {
	clearProfileArnResolutionCooldowns()
	t.Cleanup(clearProfileArnResolutionCooldowns)

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"AWS Builder ID is not supported for this operation.","reason":null}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:profile/from-refresh"}`))
	}))
	t.Cleanup(authServer.Close)

	oldTokenURL := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return authServer.URL })
	t.Cleanup(func() { auth.SetOIDCTokenURLForTest(oldTokenURL) })
	oldAuthClient := auth.SetGlobalAuthClientForTest(authServer.Client())
	t.Cleanup(func() { auth.SetGlobalAuthClientForTest(oldAuthClient) })

	account := &config.Account{
		ID:           "builder-refresh-1",
		Email:        "builder@example.com",
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       "us-east-1",
	}

	got, err := ResolveProfileArn(account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/from-refresh" {
		t.Fatalf("expected refresh fallback ARN, got %q", got)
	}
	if isProfileArnResolutionSuppressed(account) {
		t.Fatalf("refresh fallback success should not suppress future profile resolution")
	}
}

func TestRefreshAccountInfoDoesNotDisableBuilderIDWhenProfileLookupUnsupported(t *testing.T) {
	clearProfileArnResolutionCooldowns()
	t.Cleanup(clearProfileArnResolutionCooldowns)

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:          "builder-refresh-info-1",
		Email:       "builder@example.com",
		AccessToken: "access-token",
		Provider:    "BuilderId",
		Region:      "us-east-1",
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	var profileCalls, usageCalls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/ListAvailableProfiles":
				atomic.AddInt32(&profileCalls, 1)
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Body:       io.NopCloser(strings.NewReader(`{"message":"AWS Builder ID is not supported for this operation.","reason":null}`)),
					Header:     make(http.Header),
				}, nil
			case "/getUsageLimits":
				atomic.AddInt32(&usageCalls, 1)
				if strings.Contains(req.URL.RawQuery, "profileArn=") {
					t.Fatalf("expected Builder ID usage refresh to continue without profileArn, got query %q", req.URL.RawQuery)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			default:
				t.Fatalf("unexpected request path %s", req.URL.Path)
				return nil, nil
			}
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	requestAccount := account
	if _, err := RefreshAccountInfo(&requestAccount); err != nil {
		t.Fatalf("expected refresh to continue without profile ARN, got %v", err)
	}
	if got := atomic.LoadInt32(&profileCalls); got != 1 {
		t.Fatalf("expected one profile lookup, got %d", got)
	}
	if got := atomic.LoadInt32(&usageCalls); got != 1 {
		t.Fatalf("expected one usage request, got %d", got)
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(accounts))
	}
	if !accounts[0].Enabled || accounts[0].BanStatus != "" {
		t.Fatalf("expected account to remain enabled, got enabled=%v banStatus=%q", accounts[0].Enabled, accounts[0].BanStatus)
	}
}

func clearProfileArnResolutionCooldowns() {
	profileArnResolutionCooldowns.Range(func(key, _ interface{}) bool {
		profileArnResolutionCooldowns.Delete(key)
		return true
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// TestFalseBanSubstringNoLongerDisables verifies that a GetUsageLimits error
// whose body merely contains "403"/"401" inside a request ID or timestamp does
// NOT ban the account. The old bare strings.Contains(errMsg, "403") matched
// these and false-banned healthy accounts.
func TestFalseBanSubstringNoLongerDisables(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	// "403" appears only inside a request-id token — bare substring matching
	// would have banned this account; the digit-boundary classifier must not.
	_ = classifyAndBanOnUsageError(&acc, errors.New("request_id req_403abc timestamp 1782568837 failed"))

	got, _ := config.GetAccountByID("acct")
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf("account should NOT be banned for a 403-in-request-id error; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}

// TestRealSuspensionDisablesAccount verifies a real suspension signal still bans.
func TestRealSuspensionDisablesAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	_ = classifyAndBanOnUsageError(&acc, errors.New("TEMPORARILY_SUSPENDED: account suspended"))

	got, _ := config.GetAccountByID("acct")
	if got.Enabled || got.BanStatus != "BANNED" {
		t.Fatalf("suspension should ban the account; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}

// TestProfileUnavailableDoesNotBanAccount verifies a transient "no available
// Kiro profile" error from GetUsageLimits does NOT permanently ban the account.
// The background refresh path must mirror the request path's soft handling
// (account_failover.go), or a good external_idp account is banned on a blip.
func TestProfileUnavailableDoesNotBanAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c", Provider: "external_idp"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	_ = classifyAndBanOnUsageError(&acc, errors.New("no available Kiro profile"))

	got, _ := config.GetAccountByID("acct")
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf("profile-unavailable should NOT ban the account; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}

// TestClassifyAndBanOnUsageErrorBansOnGenuineAuthError verifies the background
// refresh classifier still permanently bans an account on a genuine auth failure
// (401) via the pool.IsAuthFailure branch — the positive complement to
// TestFalseBanSubstringNoLongerDisables, which only proves a stray 403-in-token
// does NOT ban. Without this, a refactor that dropped the auth ban would pass
// all existing tests silently.
func TestClassifyAndBanOnUsageErrorBansOnGenuineAuthError(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	_ = classifyAndBanOnUsageError(&acc, errors.New("HTTP 401 from primary: unauthorized"))

	got, _ := config.GetAccountByID("acct")
	if got.Enabled || got.BanStatus != "BANNED" {
		t.Fatalf("genuine auth error should ban the account via the background classifier; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}
