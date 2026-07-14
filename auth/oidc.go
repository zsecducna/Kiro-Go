package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oidcTokenURL 构造 idc/builderId 刷新 endpoint。测试可替换以拦截网络调用。
var oidcTokenURL = func(region string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
}

// socialTokenURL 构造 social 刷新 endpoint。测试可替换以拦截网络调用。
var socialTokenURL = func() string {
	return "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
}

// refreshSkewSeconds mirrors pool.tokenRefreshSkewSeconds: a token within this
// many seconds of expiry is treated as expiring and refreshed proactively.
// Kept local to auth to avoid a pool→auth import cycle.
const refreshSkewSeconds int64 = 120

var (
	// refreshMu guards refreshLocks creation only; each per-account lock is then
	// held for the duration of one refresh.
	refreshMu sync.Mutex
	// refreshLocks maps accountID → that account's refresh mutex. One lock per
	// account serializes only that account's background refreshes, reducing the
	// global contention the handler-level tokenRefreshMu imposed. tokenRefreshMu
	// still guards the request-path refresh (handler.go:2148) and remains part of
	// the documented lock order (account.go:250) — intentionally not removed.
	// Account IDs are stable UUIDs; the map is bounded by the total number of
	// accounts ever seen, which is negligible for this deployment.
	refreshLocks = map[string]*sync.Mutex{}
)

// refreshLockFor returns the per-account refresh mutex, creating it on first use.
func refreshLockFor(id string) *sync.Mutex {
	refreshMu.Lock()
	lock, ok := refreshLocks[id]
	if !ok {
		lock = &sync.Mutex{}
		refreshLocks[id] = lock
	}
	refreshMu.Unlock()
	return lock
}

// refreshTokenDirect resolves the proxy-aware HTTP client and dispatches to the
// provider-specific refresh. It performs no locking or persistence — used both
// for id-less accounts (login validation, which has no shared state to
// coordinate against) and as the inner step of the locked RefreshToken.
func refreshTokenDirect(account *config.Account) (string, string, int64, string, error) {
	proxyURL := account.ProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	client := GetAuthClientForProxy(proxyURL)
	switch account.AuthMethod {
	case "external_idp":
		return refreshExternalIdpToken(account.RefreshToken, account.ClientID, account.TokenEndpoint, account.Scopes, client)
	case "social":
		return refreshSocialToken(account.RefreshToken, client)
	default:
		return refreshOIDCToken(account.RefreshToken, account.ClientID, account.ClientSecret, account.Region, client)
	}
}

// RefreshToken refreshes the account's access token. For id-bearing accounts it
// serializes per account with double-checked locking: concurrent callers for one
// account collapse to a single IdP POST (the leader refreshes + persists under
// the lock; followers re-read the now-fresh token from config and skip the
// POST). This replaces the refresh-storm + lost-update race that occurred when
// every concurrent request POSTed independently and raced writing back the
// (rotated) refresh token.
//
// Returns: accessToken, refreshToken, expiresAt, profileArn, error.
func RefreshToken(account *config.Account) (string, string, int64, string, error) {
	if account == nil {
		return "", "", 0, "", fmt.Errorf("RefreshToken: nil account")
	}

	// Id-less accounts (e.g. the login/add-account validation flow builds a
	// temporary account) have no persisted state to coordinate against, so skip
	// the lock + double-check and refresh directly (original behavior).
	if account.ID == "" {
		return refreshTokenDirect(account)
	}

	lock := refreshLockFor(account.ID)
	lock.Lock()
	defer lock.Unlock()

	// Double-checked locking: a concurrent refresh may have renewed the token
	// while we waited. Re-read the canonical expiry from config; if it is still
	// valid, propagate it and skip the IdP POST. Either way adopt the canonical
	// fields (a concurrent refresh may have rotated the refresh token).
	if live, ok := config.GetAccountByID(account.ID); ok {
		*account = live
		if now := time.Now().Unix(); live.ExpiresAt > 0 && now < live.ExpiresAt-refreshSkewSeconds {
			return live.AccessToken, live.RefreshToken, live.ExpiresAt, live.ProfileArn, nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := refreshTokenDirect(account)
	if err != nil {
		return "", "", 0, "", err
	}

	// Persist under the lock so the next caller's double-check sees the fresh
	// token — this is what collapses N concurrent refreshes into one IdP POST.
	// (Existing handler call sites also persist; those writes are now idempotent
	// no-ops and are left untouched per the no-call-site-change constraint.)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
	}
	if perr := config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt); perr != nil {
		logger.Warnf("[RefreshToken] failed to persist refreshed token for %s: %v", account.Email, perr)
	}
	if profileArn != "" {
		if perr := config.UpdateAccountProfileArn(account.ID, profileArn); perr != nil {
			logger.Warnf("[RefreshToken] failed to persist profileArn for %s: %v", account.Email, perr)
		}
	}
	return accessToken, refreshToken, expiresAt, profileArn, nil
}

// refreshExternalIdpToken refreshes an external-IdP (enterprise SSO) access token
// through the IdP token endpoint using the OAuth2 refresh_token grant for a public
// client (no client secret). offline_access in the original scopes is what makes a
// refresh token available. The IdP issues no profileArn (it is resolved separately
// via ListAvailableProfiles using the EXTERNAL_IDP token type), so "" is returned
// for the profileArn.
func refreshExternalIdpToken(refreshToken, clientID, tokenEndpoint, scopes string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || tokenEndpoint == "" {
		return "", "", 0, "", fmt.Errorf("external IdP refresh requires clientId and tokenEndpoint")
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if scopes != "" {
		form.Set("scope", scopes)
	}
	accessToken, newRefreshToken, expiresIn, err := postExternalIdpToken(client, tokenEndpoint, form)
	if err != nil {
		return "", "", 0, "", err
	}
	// Some IdPs (Azure AD) rotate refresh tokens; others omit it on refresh. Keep the
	// existing refresh token when the response does not carry a new one.
	if newRefreshToken == "" {
		newRefreshToken = refreshToken
	}
	expiresAt := time.Now().Unix() + int64(expiresIn)
	return accessToken, newRefreshToken, expiresAt, "", nil
}

// postExternalIdpToken performs a form-encoded POST to an external-IdP token
// endpoint and maps the snake_case OAuth2 token response onto the standard return
// shape. Shared by the authorization-code exchange (login) and the refresh_token
// grant (renewal).
func postExternalIdpToken(client *http.Client, tokenEndpoint string, form url.Values) (accessToken, refreshToken string, expiresIn int, err error) {
	if strings.TrimSpace(tokenEndpoint) == "" {
		return "", "", 0, fmt.Errorf("external IdP token endpoint is empty")
	}
	// Defense-in-depth: re-validate the endpoint at the outbound-POST boundary so the
	// refresh token is never sent to a non-allow-listed host — even if a persisted
	// account's TokenEndpoint was set out-of-band (backup restore, an external file
	// write, or a future caller that stores an endpoint without validating). This makes
	// allow-list validation an invariant of the exfiltration-sensitive operation itself
	// rather than of every caller. Uses the exported ValidateExternalIdpEndpoint so the
	// test seam (SetExternalIdpValidatorForTest) still relaxes it for httptest servers.
	if err := ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return "", "", 0, fmt.Errorf("external IdP token endpoint rejected: %w", err)
	}
	req, err := http.NewRequest("POST", tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to build external IdP token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to read external IdP token response: %w", err)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || out.AccessToken == "" {
		if out.Error != "" {
			return "", "", 0, fmt.Errorf("external IdP token exchange failed (status %d): %s: %s", resp.StatusCode, out.Error, out.ErrorDesc)
		}
		return "", "", 0, fmt.Errorf("external IdP token exchange failed (status %d): %s", resp.StatusCode, string(body))
	}
	return out.AccessToken, out.RefreshToken, out.ExpiresIn, nil
}

// refreshOIDCToken IdC/Builder ID token 刷新
func refreshOIDCToken(refreshToken, clientID, clientSecret, region string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || clientSecret == "" {
		return "", "", 0, "", fmt.Errorf("OIDC refresh requires clientId and clientSecret")
	}
	if region == "" {
		region = "us-east-1"
	}

	url := oidcTokenURL(region)

	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}

// refreshSocialToken Social (GitHub/Google) token 刷新
func refreshSocialToken(refreshToken string, client *http.Client) (string, string, int64, string, error) {
	url := socialTokenURL()

	payload := map[string]string{
		"refreshToken": refreshToken,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}
