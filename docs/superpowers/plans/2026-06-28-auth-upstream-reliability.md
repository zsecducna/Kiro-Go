# Auth / Upstream Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the refresh-storm race (B1) and the false-ban substring matching (B2) in the auth/upstream layer, without adding any dependency.

**Architecture:** B1 wraps `auth.RefreshToken` in a per-account mutex with double-checked locking, persisting the refreshed token under the lock so N concurrent callers collapse to one IdP POST. B2 routes error classification through the existing `pool.IsAuthFailure` / `pool.IsSuspensionError` helpers (digit-boundary-aware) instead of bare `strings.Contains`, killing false-bans where `"401"`/`"403"` matched request IDs.

**Tech Stack:** Go 1.21, stdlib only (no `x/sync/singleflight`), packages `auth`, `config`, `proxy`, `pool`.

**Spec:** `docs/superpowers/specs/2026-06-28-auth-upstream-reliability-design.md`

## Global Constraints

- **No new dependencies.** Approach A1 only — no `golang.org/x/sync`. (The failure-case collapse it would add is accepted as bounded churn; see spec §3.)
- **Branch:** `feat/dispatch-cache-hardening`. One commit per task.
- **Preserve existing `banStatus` semantics.** `RefreshAccountInfo` keeps banning as `"BANNED"` via inline `config.UpdateAccount`. Do NOT switch to `pool.DisableAccount` (which sets `"DISABLED"`) — that changes which accounts `reprobeDisabled` auto-recovers and is out of scope for B2.
- **`auth.RefreshToken` gains a persist side-effect under the lock** (for id-bearing accounts only) so the DCL collapses. The 7 existing call sites are unchanged; their own `config.UpdateAccountToken` calls become harmless idempotent writes.
- **Id-less accounts (login/add-account `tempAccount`, `handler.go:3136`) bypass the lock + DCL + persist** and refresh directly, preserving current behavior.
- **`reprobeDisabled` (`pool/account.go:785`) now persists refreshed tokens** (previously discarded — a latent bug). This is a positive side-effect; verify the auto-recover test still passes.
- **TDD:** every task writes the failing test first, runs it red, implements, runs it green, then commits.
- **Verify after each task:** `go build ./...` and `go test ./...` must pass.

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `config/config.go` | Thread-safe config store | Add `GetAccountByID(id) (Account, bool)` — canonical read for B1's DCL |
| `auth/oidc.go` | Token refresh | Add per-account lock map + `refreshLockFor`; rewrite `RefreshToken` (lock + DCL + persist); extract `refreshTokenDirect` for the dispatch; add `refreshSkewSeconds` const |
| `auth/oidc_test.go` (new) | B1 test | `TestRefreshLockCollapsesConcurrentRefreshes` |
| `proxy/kiro_api.go` | Background account-info refresh | `RefreshAccountInfo`: replace bare `strings.Contains` with `pool.IsSuspensionError`/`pool.IsAuthFailure` via extracted `classifyAndBanOnUsageError` + `banAccountInline` |
| `proxy/kiro_api_test.go` | B2a tests | `TestFalseBanSubstringNoLongerDisables`, `TestRealSuspensionDisablesAccount` |
| `proxy/kiro.go` | Upstream Kiro HTTP call | Add `upstreamError(statusCode, endpoint, body)`; use it in `CallKiroAPI` so 402 carries an "overage" marker |
| `proxy/kiro_test.go` | B2b test | `TestCallKiroAPIClassifiesByStatusCode` |

---

## Task 1: B1 — per-account refresh lock with double-checked locking

**Files:**
- Modify: `config/config.go` (add `GetAccountByID`)
- Modify: `auth/oidc.go` (lock map + rewrite `RefreshToken` + `refreshTokenDirect`)
- Create: `auth/oidc_test.go`

**Interfaces:**
- Produces: `config.GetAccountByID(id string) (config.Account, bool)` — returns a copy of the account or `ok=false`. Used by the DCL re-check.
- Produces: unchanged `auth.RefreshToken(account *config.Account) (accessToken, refreshToken string, expiresAt int64, profileArn string, err error)` — same signature, now serialized + collapsing per account.
- Consumes (existing, unchanged): `config.UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error`, `config.UpdateAccountProfileArn(id, profileArn string) error`, `config.GetProxyURL() string`, `GetAuthClientForProxy(proxyURL string) *http.Client`.

- [ ] **Step 1: Write the failing test**

Create `auth/oidc_test.go`:

```go
package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

// TestRefreshLockCollapsesConcurrentRefreshes verifies that N goroutines
// refreshing the same expired-token account collapse into a single IdP POST:
// the first caller refreshes+persists under the per-account lock, and every
// follower's double-check reads the now-fresh token from config and skips the
// POST. Before the fix each goroutine POSTed independently (refresh-storm +
// lost-update on refresh-token rotation).
func TestRefreshLockCollapsesConcurrentRefreshes(t *testing.T) {
	var postCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&postCount, 1)
		// Hold the response so the other goroutines are definitely waiting on
		// the lock while the leader POSTs (makes the test robust to scheduling).
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "at-fresh",
			"refresh_token": "rt-rotated",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:            "acct",
		AuthMethod:    "external_idp",
		ClientID:      "cid",
		TokenEndpoint: srv.URL,
		Scopes:        "offline_access",
		RefreshToken:  "rt-old",
		ExpiresAt:     1, // expired → every caller wants a refresh
		Enabled:       true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			acc := config.Account{
				ID: "acct", AuthMethod: "external_idp", ClientID: "cid",
				TokenEndpoint: srv.URL, Scopes: "offline_access",
				RefreshToken: "rt-old", ExpiresAt: 1,
			}
			<-start // release all goroutines at once to maximise overlap
			at, _, _, _, err := RefreshToken(&acc)
			tokens[idx] = at
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d refresh failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("expected exactly 1 IdP POST (collapse), got %d", got)
	}
	for i, tk := range tokens {
		if tk != "at-fresh" {
			t.Fatalf("goroutine %d got token %q, want the shared fresh token", i, tk)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./auth/ -run TestRefreshLockCollapsesConcurrentRefreshes -v`
Expected: FAIL — either `postCount` is > 1 (today every goroutine POSTs), or a compile error because `GetAccountByID` does not exist yet. Either confirms the test exercises the unfixed behavior.

- [ ] **Step 3: Add `config.GetAccountByID`**

In `config/config.go`, add (next to `GetAccounts`, around line 422). Match the existing lock variable name used by sibling accessors (verify by reading `GetAccounts` — it is `cfgLock`):

```go
// GetAccountByID returns a copy of the account with the given ID, or ok=false
// if no such account exists. Used by auth.RefreshToken's double-checked
// locking to read the canonical token state.
func GetAccountByID(id string) (Account, bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			return cfg.Accounts[i], true
		}
	}
	return Account{}, false
}
```

- [ ] **Step 4: Add the lock map + `refreshTokenDirect` and rewrite `RefreshToken` in `auth/oidc.go`**

Add `"kiro-go/logger"` and `"sync"` to the import block of `auth/oidc.go` (current imports: `bytes, encoding/json, fmt, io, kiro-go/config, net/http, net/url, strings, time`).

Add the lock infra + const near the top of the file (after the existing `socialTokenURL` var, before `RefreshToken`):

```go
// refreshSkewSeconds mirrors pool.tokenRefreshSkewSeconds: a token within this
// many seconds of expiry is treated as expiring and refreshed proactively.
// Kept local to auth to avoid a pool→auth import cycle.
const refreshSkewSeconds int64 = 120

var (
	// refreshMu guards refreshLocks creation only; each per-account lock is then
	// held for the duration of one refresh.
	refreshMu sync.Mutex
	// refreshLocks maps accountID → that account's refresh mutex. One lock per
	// account serializes only that account's refreshes, replacing the old global
	// handler-level tokenRefreshMu which serialized every account (a bottleneck).
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
```

Extract the dispatch into `refreshTokenDirect` (resolves client + POSTs, no coordination):

```go
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
```

Replace the entire current `RefreshToken` function (lines 25–46) with:

```go
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
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./auth/ -run TestRefreshLockCollapsesConcurrentRefreshes -v`
Expected: PASS — `postCount == 1`, all goroutines received `at-fresh`.

- [ ] **Step 6: Verify the whole suite still passes**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build + vet clean; all packages pass. Pay attention to `kiro-go/pool` (`TestAutoRecoverReprobesDisabledAccount` exercises `reprobeDisabled`, which now persists refreshed tokens — it should still pass since the social refresh in that test fails against no endpoint).

- [ ] **Step 7: Commit**

```bash
git add config/config.go auth/oidc.go auth/oidc_test.go
git commit -m "fix(auth): serialize per-account token refresh with double-checked locking"
```

---

## Task 2: B2a — route RefreshAccountInfo classification through shared helpers

**Files:**
- Modify: `proxy/kiro_api.go` (`RefreshAccountInfo`, lines ~528–568)
- Modify: `proxy/kiro_api_test.go` (add two tests)

**Interfaces:**
- Produces (unexported, same package): `classifyAndBanOnUsageError(account *config.Account, err error) error` and `banAccountInline(account *config.Account, banStatus, banReason string)`.
- Consumes (existing): `pool.IsSuspensionError(err error) bool`, `pool.IsAuthFailure(err error) bool`, `config.UpdateAccount(id string, account config.Account) error`.

- [ ] **Step 1: Write the failing tests**

Append to `proxy/kiro_api_test.go` (ensure imports include `"errors"`, `"testing"`, `"kiro-go/config"`):

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./proxy/ -run 'TestFalseBanSubstringNoLongerDisables|TestRealSuspensionDisablesAccount' -v`
Expected: FAIL — `classifyAndBanOnUsageError` is undefined (compile error).

- [ ] **Step 3: Add the helpers and rewire `RefreshAccountInfo`**

In `proxy/kiro_api.go`, add `"kiro-go/pool"` to the import block.

Add the two helpers (near `RefreshAccountInfo`):

```go
// classifyAndBanOnUsageError inspects a GetUsageLimits error and disables the
// account when it signals a hard upstream state (suspension or auth failure).
// Classification routes through the shared pool.IsSuspensionError /
// pool.IsAuthFailure helpers (digit-boundary-aware) instead of bare
// strings.Contains, which previously false-banned accounts when "401"/"403"
// appeared inside request IDs or timestamps. Returns the caller-facing error.
func classifyAndBanOnUsageError(account *config.Account, err error) error {
	switch {
	case pool.IsSuspensionError(err):
		logger.Warnf("[RefreshAccountInfo] Account %s is suspended: %v", account.Email, err)
		banAccountInline(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
		return fmt.Errorf("Account suspended: %w", err)
	case pool.IsAuthFailure(err):
		logger.Warnf("[RefreshAccountInfo] Authentication error for %s: %v", account.Email, err)
		banAccountInline(account, "BANNED", "Authentication failed - token invalid or expired")
	}
	return fmt.Errorf("GetUsageLimits: %w", err)
}

// banAccountInline disables an account (banStatus + reason) via config. Used by
// background-refresh paths that have no Handler/pool handle. No-op if the
// account is already disabled with the same status/reason.
func banAccountInline(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}
	updated := *account
	if !updated.Enabled && updated.BanStatus == banStatus && updated.BanReason == banReason {
		return
	}
	updated.Enabled = false
	updated.BanStatus = banStatus
	updated.BanReason = banReason
	updated.BanTime = time.Now().Unix()
	if err := config.UpdateAccount(account.ID, updated); err != nil {
		logger.Errorf("[RefreshAccountInfo] Failed to update account ban status: %v", err)
	}
}
```

Replace the error-handling block inside `RefreshAccountInfo` (the `if err != nil { ... }` over lines ~530–568, from `errMsg := err.Error()` through the closing of the `if err != nil`) with a single call:

```go
	usage, err := GetUsageLimits(account)
	if err != nil {
		return nil, classifyAndBanOnUsageError(account, err)
	}
```

(The `strings` import remains used elsewhere in the file — leave it.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./proxy/ -run 'TestFalseBanSubstringNoLongerDisables|TestRealSuspensionDisablesAccount' -v`
Expected: PASS — request-id-403 does not ban; TEMPORARILY_SUSPENDED bans as `BANNED`.

- [ ] **Step 5: Verify the whole suite still passes**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build + vet clean; all packages pass.

- [ ] **Step 6: Commit**

```bash
git add proxy/kiro_api.go proxy/kiro_api_test.go
git commit -m "fix(proxy): classify usage-refresh errors via shared helpers (kill false-ban)"
```

---

## Task 3: B2b — tag 402 as overage in CallKiroAPI error classification

**Files:**
- Modify: `proxy/kiro.go` (add `upstreamError`, use in `CallKiroAPI` around lines 384–394)
- Modify: `proxy/kiro_test.go` (add one test)

**Interfaces:**
- Produces (unexported, same package): `upstreamError(statusCode int, endpoint, body string) error` — 402 is tagged `HTTP 402 overage`, all other codes `HTTP <code>`.
- Consumes (existing): `pool.IsAuthFailure`, `pool.IsSuspensionError`, `isOverageErrorMessage(msg string) bool` (from `proxy/account_failover.go`).

- [ ] **Step 1: Write the failing test**

Append to `proxy/kiro_test.go` (ensure imports include `"testing"`, `"kiro-go/pool"`):

```go
// TestCallKiroAPIClassifiesByStatusCode verifies the error CallKiroAPI builds
// from a non-200 upstream response classifies correctly downstream: 401/403 →
// auth failure (digit-boundary safe), 402 → overage (NOT auth), and a
// suspension marker in the body → suspension. CallKiroAPI delegates error
// construction to upstreamError.
func TestCallKiroAPIClassifiesByStatusCode(t *testing.T) {
	// 401 / 403 → auth failure, even when the body carries unrelated digits.
	if !pool.IsAuthFailure(upstreamError(401, "primary", "request req_999 failed")) {
		t.Fatal("401 should classify as auth failure")
	}
	if !pool.IsAuthFailure(upstreamError(403, "primary", "unrelated body")) {
		t.Fatal("403 should classify as auth failure")
	}
	// 402 → overage, NOT auth.
	e402 := upstreamError(402, "primary", "Usage limit exceeded")
	if pool.IsAuthFailure(e402) {
		t.Fatal("402 must NOT classify as auth failure")
	}
	if !isOverageErrorMessage(e402.Error()) {
		t.Fatal("402 should classify as overage")
	}
	// Suspension signalled in the body of a 403 still classifies as suspension.
	if !pool.IsSuspensionError(upstreamError(403, "primary", "TEMPORARILY_SUSPENDED")) {
		t.Fatal("suspension body should classify as suspension")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./proxy/ -run TestCallKiroAPIClassifiesByStatusCode -v`
Expected: FAIL — `upstreamError` is undefined (compile error).

- [ ] **Step 3: Add `upstreamError` and use it in `CallKiroAPI`**

In `proxy/kiro.go`, add (near `CallKiroAPI`):

```go
// upstreamError builds a classifiable error from a non-200 Kiro response.
// 402 is tagged "overage" so the failover layer routes it to overage handling
// (disableAccountOverage → refresh OverageStatus) instead of falling through to
// the generic RecordError path. All other codes produce "HTTP <code> ...", which
// pool.IsAuthFailure reads via its digit-boundary status-token matcher.
func upstreamError(statusCode int, endpoint, body string) error {
	if statusCode == 402 {
		return fmt.Errorf("HTTP 402 overage from %s: %s", endpoint, body)
	}
	return fmt.Errorf("HTTP %d from %s: %s", statusCode, endpoint, body)
}
```

In `CallKiroAPI`, replace the non-200 block (lines ~384–394):

```go
		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = upstreamError(resp.StatusCode, ep.Name, string(errBody))
			// Auth failures (401/403) and overage (402) are account-level: do not
			// retry across endpoints. Other status codes fall through to the next
			// endpoint.
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
				return lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./proxy/ -run TestCallKiroAPIClassifiesByStatusCode -v`
Expected: PASS.

- [ ] **Step 5: Verify the whole suite still passes**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build + vet clean; all packages pass.

- [ ] **Step 6: Commit**

```bash
git add proxy/kiro.go proxy/kiro_test.go
git commit -m "fix(proxy): tag 402 as overage in upstream error classification"
```

---

## Self-Review (run after writing; fix inline)

**1. Spec coverage:**
- Spec B1 (per-account refresh lock, DCL, collapse, no dep, Rust-matching): **Task 1** ✓.
- Spec B1 "7 call sites unchanged": preserved — `RefreshToken` signature unchanged; id-less path keeps `tempAccount` working ✓.
- Spec B2 site 1 (`kiro_api.go:533` TEMPORARILY_SUSPENDED → `pool.IsSuspensionError`): **Task 2** ✓.
- Spec B2 site 2 (`kiro_api.go:550` 403/401/invalid/expired → `pool.IsAuthFailure`): **Task 2** ✓.
- Spec B2 site 3 (`kiro.go:384-394` → structural status-code; 402 → overage): **Task 3** ✓.
- Spec test 1 `TestRefreshLockCollapsesConcurrentRefreshes`: **Task 1** ✓.
- Spec test 2 `TestFalseBanSubstringNoLongerDisables`: **Task 2** ✓.
- Spec test 3 `TestRealSuspensionDisablesAccount`: **Task 2** ✓.
- Spec test 4 `TestCallKiroAPIClassifiesByStatusCode`: **Task 3** ✓.
- Spec non-goal B3 (`context.Context`): correctly absent — no task touches request context.

**2. Placeholder scan:** None. All steps contain complete code and exact commands.

**3. Type consistency:**
- `config.GetAccountByID(id string) (config.Account, bool)` — defined Task 1 Step 3, used Task 1 Step 4 + tests.
- `auth.RefreshToken(account *config.Account) (string, string, int64, string, error)` — return order matches all 7 call sites (accessToken, refreshToken, expiresAt, profileArn, err).
- `refreshTokenDirect(account *config.Account) (string, string, int64, string, error)` — same return shape; called in both id-less and locked paths.
- `classifyAndBanOnUsageError(account *config.Account, err error) error` + `banAccountInline(account *config.Account, banStatus, banReason string)` — defined Task 2 Step 3, used Task 2 tests.
- `upstreamError(statusCode int, endpoint, body string) error` — defined Task 3 Step 3, used in `CallKiroAPI` + Task 3 test.
- `pool.IsAuthFailure` / `pool.IsSuspensionError` signatures match `pool/account.go:431,482`. `isOverageErrorMessage(msg string) bool` matches `proxy/account_failover.go:17`.

**One spec refinement documented in-plan (not a deviation):** the spec said "route through `pool.DisableAccount`" for RefreshAccountInfo. The plan keeps the inline `banAccountInline` (banStatus `"BANNED"`) instead, because `pool.DisableAccount` sets `"DISABLED"` — which would silently change which accounts `reprobeDisabled` auto-recovers. Consolidating the disable path (and the BANNED/DISABLED split) is explicitly left as a follow-up, since it needs separate auto-recover validation. This keeps B2 a pure false-ban fix with no behavior drift.

**Another in-plan refinement:** spec §3 said "POST + persist (logic hiện tại)". The plan makes explicit that the persist must move *inside* `auth.RefreshToken` (under the lock) for the DCL to collapse — otherwise a waiting follower re-reads stale config and re-POSTs (and with refresh-token rotation, posts a consumed token). Callers' existing persists become idempotent no-ops. This is the correct realization of approach A1.
