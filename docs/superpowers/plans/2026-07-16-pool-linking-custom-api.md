# Pool Linking — Custom API Upstream Accounts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a new "Custom API" account type that transparently forwards requests to another Kiro-Go pool (base URL + that pool's API key), added through an admin flow with quota/order-id/tags validation.

**Architecture:** New `AuthMethod == "custom_api"` on `config.Account` carrying `BaseURL`/`OrderID`/`Tags` (reusing `KiroApiKey` as the upstream bearer). A new admin endpoint validates the upstream key via `GET {baseUrl}/api/me` before adding. At runtime the 4 dispatch handlers fork: when the pool selects a custom_api account they call a new `forwardToUpstream` that proxies the raw request body to `{baseUrl}/v1/...` and streams the reply back, reusing the existing failover loop, circuit breaker, stats, and per-key metering.

**Tech Stack:** Go (net/http, stdlib), existing `config`/`pool`/`proxy` packages, `httptest` for tests.

## Global Constraints

- Language: Go. Follow existing file conventions in `proxy/` and `config/`.
- Detailed comments required before each new function / non-obvious line (repo CLAUDE.md rule).
- `/api/*` and `/admin/*` contracts stay backward compatible — only ADD new endpoint + optional JSON fields.
- Upstream bearer token stored in the existing `Account.KiroApiKey` field. No new secret field.
- `custom_api` is NOT an OAuth method: it must never be token-refreshed, region-probed, or expiry-checked.
- Build check: `rtk go build ./...`. Test check: `rtk go test ./proxy/ ./config/`.
- New package-var seams (`probeCustomApiQuota`, `forwardUpstreamRequest`) so tests can stub upstream round-trips without network (mirror existing `probeKiroApiKey` at `proxy/admin_bot_api.go:799`).

---

### Task 1: Account data-model fields

**Files:**
- Modify: `config/config.go` (Account struct, ~line 37–120)
- Test: `config/config_test.go`

**Interfaces:**
- Produces: `Account.BaseURL string`, `Account.OrderID string`, `Account.Tags []string`; the string constant `"custom_api"` used as `AuthMethod`.

- [ ] **Step 1: Write the failing test**

Add to `config/config_test.go`:
```go
// Custom API accounts persist their upstream base URL, order id, and tags
// through JSON round-trip so /admin/pool and the panel can display them.
func TestAccountCustomApiFieldsRoundTrip(t *testing.T) {
	a := Account{
		ID:         "acc1",
		AuthMethod: "custom_api",
		BaseURL:    "https://pool.example.com",
		KiroApiKey: "sk-upstream",
		OrderID:    "ORD-1234",
		Nickname:   "ORD-1234",
		Tags:       []string{"Custom API"},
		Enabled:    true,
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Account
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.BaseURL != a.BaseURL || got.OrderID != a.OrderID || got.AuthMethod != "custom_api" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "Custom API" {
		t.Fatalf("tags round-trip mismatch: %+v", got.Tags)
	}
}
```
Ensure `encoding/json` is imported in the test file (it likely already is).

- [ ] **Step 2: Run test to verify it fails**

Run: `rtk go test ./config/ -run TestAccountCustomApiFieldsRoundTrip`
Expected: FAIL — `got.BaseURL`/`OrderID`/`Tags` unknown fields (compile error).

- [ ] **Step 3: Add fields to the Account struct**

In `config/config.go`, inside `type Account struct`, near the identity block (after `Nickname`, around line 42) and the credential block, add:
```go
	// Custom API (pool-linking) fields. Present only when AuthMethod == "custom_api":
	// the account is a transparent proxy to ANOTHER Kiro-Go pool rather than a direct
	// Kiro credential. The upstream bearer token is stored in KiroApiKey (its existing
	// "upstream bearer, never refreshed" role); these fields carry the rest.
	BaseURL string   `json:"baseUrl,omitempty"` // Upstream pool root, e.g. https://pool.example.com (no trailing /v1)
	OrderID string   `json:"orderId,omitempty"` // Order id; also used as the account name/nickname
	Tags    []string `json:"tags,omitempty"`    // Labels; custom_api accounts carry ["Custom API"]
```

- [ ] **Step 4: Run test to verify it passes**

Run: `rtk go test ./config/ -run TestAccountCustomApiFieldsRoundTrip`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
rtk git add config/config.go config/config_test.go
rtk git commit -m "feat(config): add Custom API account fields (baseUrl/orderId/tags)"
```

---

### Task 2: Upstream quota probe helper

**Files:**
- Create: `proxy/custom_api_forward.go`
- Test: `proxy/custom_api_forward_test.go`

**Interfaces:**
- Produces:
  - `type customApiQuota struct { CreditsRemaining float64; TokensRemaining int64; CreditLimit float64; TokenLimit int64; OK bool }`
  - `var probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error)` — GET `{baseURL}/api/me`, 5s timeout.
  - `func customApiQuotaAcceptable(q *customApiQuota) bool` — accept rule.
  - `func normalizeBaseURL(raw string) (string, error)` — trims trailing `/`, requires `http(s)://`.

- [ ] **Step 1: Write the failing test**

Create `proxy/custom_api_forward_test.go`:
```go
package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A live upstream key with remaining credits passes the quota gate.
func TestProbeCustomApiQuotaAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-up" {
			t.Fatalf("missing bearer, got %q", got)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"creditsRemaining": 12.5, "tokensRemaining": 0,
			"creditLimit": 100.0, "tokenLimit": 0,
		})
	}))
	defer srv.Close()

	q, err := probeCustomApiQuota(srv.URL, "sk-up")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !customApiQuotaAcceptable(q) {
		t.Fatalf("expected acceptable, got %+v", q)
	}
}

// A key whose limits are set but remaining is zero is rejected.
func TestCustomApiQuotaRejectZero(t *testing.T) {
	q := &customApiQuota{CreditsRemaining: 0, TokensRemaining: 0, CreditLimit: 100, TokenLimit: 0, OK: true}
	if customApiQuotaAcceptable(q) {
		t.Fatalf("expected rejection for zero remaining with a limit")
	}
}

// Unlimited (both limits zero) is accepted even with zero "remaining".
func TestCustomApiQuotaAcceptUnlimited(t *testing.T) {
	q := &customApiQuota{CreditsRemaining: 0, TokensRemaining: 0, CreditLimit: 0, TokenLimit: 0, OK: true}
	if !customApiQuotaAcceptable(q) {
		t.Fatalf("expected unlimited to be accepted")
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	got, err := normalizeBaseURL("https://pool.example.com/")
	if err != nil || got != "https://pool.example.com" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := normalizeBaseURL("pool.example.com"); err == nil {
		t.Fatalf("expected error for scheme-less url")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `rtk go test ./proxy/ -run 'TestProbeCustomApiQuota|TestCustomApiQuota|TestNormalizeBaseURL'`
Expected: FAIL — undefined `probeCustomApiQuota`, `customApiQuota`, etc.

- [ ] **Step 3: Create the helper file**

Create `proxy/custom_api_forward.go`:
```go
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// customApiQuota is the subset of an upstream Kiro-Go pool's GET /api/me
// response used to gate adding a Custom API account. OK is false when the
// upstream returned a non-2xx status (bad/expired key).
type customApiQuota struct {
	CreditsRemaining float64
	TokensRemaining  int64
	CreditLimit      float64
	TokenLimit       int64
	OK               bool
}

// probeCustomApiQuota calls {baseURL}/api/me with the supplied bearer token and
// returns the parsed quota. It is a package var so tests can stub the round-trip.
// A 5s timeout keeps a dead upstream from hanging the add-account request.
var probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/me"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return &customApiQuota{OK: false}, fmt.Errorf("upstream /api/me returned %d", resp.StatusCode)
	}
	var raw struct {
		CreditsRemaining float64 `json:"creditsRemaining"`
		TokensRemaining  int64   `json:"tokensRemaining"`
		CreditLimit      float64 `json:"creditLimit"`
		TokenLimit       int64   `json:"tokenLimit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return &customApiQuota{OK: false}, fmt.Errorf("upstream /api/me: bad JSON: %w", err)
	}
	return &customApiQuota{
		CreditsRemaining: raw.CreditsRemaining,
		TokensRemaining:  raw.TokensRemaining,
		CreditLimit:      raw.CreditLimit,
		TokenLimit:       raw.TokenLimit,
		OK:               true,
	}, nil
}

// customApiQuotaAcceptable is the add-time gate: accept when the upstream key is
// valid and either has remaining quota, or is unlimited (both limits zero).
func customApiQuotaAcceptable(q *customApiQuota) bool {
	if q == nil || !q.OK {
		return false
	}
	if q.CreditLimit == 0 && q.TokenLimit == 0 {
		return true // unlimited key
	}
	return q.CreditsRemaining > 0 || q.TokensRemaining > 0
}

// normalizeBaseURL trims a trailing slash and requires an http(s) scheme so a
// stored base URL is always safe to concatenate with /v1/... paths.
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return "", fmt.Errorf("baseUrl must start with http:// or https://")
	}
	return strings.TrimRight(s, "/"), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `rtk go test ./proxy/ -run 'TestProbeCustomApiQuota|TestCustomApiQuota|TestNormalizeBaseURL'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
rtk git add proxy/custom_api_forward.go proxy/custom_api_forward_test.go
rtk git commit -m "feat(proxy): add upstream quota probe for Custom API accounts"
```

---

### Task 3: Add-account admin endpoint + route + dedup

**Files:**
- Modify: `proxy/admin_bot_api.go` (add handler + request type near the other add handlers, after line ~711)
- Modify: `proxy/handler.go` (route registration, after line 459)
- Test: `proxy/admin_custom_api_test.go`

**Interfaces:**
- Consumes: `probeCustomApiQuota`, `customApiQuotaAcceptable`, `normalizeBaseURL` (Task 2); `config.AddAccount`, `config.GetAccounts`, `auth.GenerateAccountID`, `config.GenerateMachineId`.
- Produces: `func (h *Handler) handleAdminAddCustomApiAccount(w http.ResponseWriter, r *http.Request)`; route `POST /admin/add_custom_api_account`; helper `func findCustomApiDuplicate(orderID, baseURL, apiKey string) *config.Account`.

- [ ] **Step 1: Write the failing test**

Create `proxy/admin_custom_api_test.go`:
```go
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
)

// helper: POST JSON to the handler with admin auth satisfied via test setup.
func postAddCustomApi(t *testing.T, h *Handler, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/admin/add_custom_api_account", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.handleAdminAddCustomApiAccount(rec, req)
	return rec
}

func TestAddCustomApiAccountSuccess(t *testing.T) {
	resetTestConfig(t) // see note in Step 3 on test config isolation
	orig := probeCustomApiQuota
	probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
		return &customApiQuota{CreditsRemaining: 10, CreditLimit: 100, OK: true}, nil
	}
	defer func() { probeCustomApiQuota = orig }()

	rec := postAddCustomApi(t, testHandler(t), map[string]interface{}{
		"baseUrl": "https://pool.example.com/", "apiKey": "sk-up", "orderId": "ORD-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var found *config.Account
	for _, a := range config.GetAccounts() {
		if a.AuthMethod == "custom_api" {
			ac := a
			found = &ac
		}
	}
	if found == nil {
		t.Fatalf("account not persisted")
	}
	if found.BaseURL != "https://pool.example.com" || found.OrderID != "ORD-1" || found.Nickname != "ORD-1" {
		t.Fatalf("bad account: %+v", found)
	}
	if len(found.Tags) != 1 || found.Tags[0] != "Custom API" {
		t.Fatalf("tags: %+v", found.Tags)
	}
}

func TestAddCustomApiRejectBadQuota(t *testing.T) {
	resetTestConfig(t)
	orig := probeCustomApiQuota
	probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
		return &customApiQuota{OK: false}, http.ErrHandlerTimeout
	}
	defer func() { probeCustomApiQuota = orig }()

	rec := postAddCustomApi(t, testHandler(t), map[string]interface{}{
		"baseUrl": "https://pool.example.com", "apiKey": "bad", "orderId": "ORD-2",
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 for bad quota, got 200")
	}
}

func TestAddCustomApiRejectDuplicateOrderID(t *testing.T) {
	resetTestConfig(t)
	orig := probeCustomApiQuota
	probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
		return &customApiQuota{CreditsRemaining: 10, CreditLimit: 100, OK: true}, nil
	}
	defer func() { probeCustomApiQuota = orig }()
	h := testHandler(t)
	postAddCustomApi(t, h, map[string]interface{}{"baseUrl": "https://a.com", "apiKey": "k1", "orderId": "DUP"})
	rec := postAddCustomApi(t, h, map[string]interface{}{"baseUrl": "https://b.com", "apiKey": "k2", "orderId": "DUP"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate orderId, got %d", rec.Code)
	}
}
```
> **Test scaffolding note:** `testHandler(t)`, `resetTestConfig(t)` — reuse whatever the existing `proxy` tests use to build a `*Handler` and isolate config state. Inspect `proxy/customer_admin_api_test.go` and `proxy/kiro_apikey_test.go` for the established helper names; if they differ, use the existing ones instead of introducing new helpers. The handler is invoked directly (bypassing `authenticateAdminKey`) so no admin credential is needed in the unit test.

- [ ] **Step 2: Run test to verify it fails**

Run: `rtk go test ./proxy/ -run TestAddCustomApi`
Expected: FAIL — undefined `handleAdminAddCustomApiAccount` / `findCustomApiDuplicate`.

- [ ] **Step 3: Add the request type, dedup helper, and handler**

In `proxy/admin_bot_api.go`, after `handleAdminAddKiroApiKey` (around line 711), add:
```go
// adminAddCustomApiRequest is the body for POST /admin/add_custom_api_account.
// A Custom API account is a transparent proxy to ANOTHER Kiro-Go pool: BaseURL
// is that pool's root and ApiKey is a key that pool issued to us.
type adminAddCustomApiRequest struct {
	BaseURL  string   `json:"baseUrl"`            // Upstream pool root (required)
	ApiKey   string   `json:"apiKey"`             // Upstream bearer token (required)
	OrderID  string   `json:"orderId"`            // Order id; required; doubles as the account name
	Nickname string   `json:"nickname,omitempty"` // Optional display name; defaults to orderId
	Tags     []string `json:"tags,omitempty"`     // Extra tags; "Custom API" is always added
	Enabled  *bool    `json:"enabled,omitempty"`  // Route traffic immediately (default true)
}

// findCustomApiDuplicate returns an existing account that would collide with a new
// Custom API add, or nil. A collision is the same OrderID, or the same
// (BaseURL, ApiKey) pair — either means the operator is adding the same upstream twice.
func findCustomApiDuplicate(orderID, baseURL, apiKey string) *config.Account {
	orderID = strings.TrimSpace(orderID)
	for _, a := range config.GetAccounts() {
		if !strings.EqualFold(strings.TrimSpace(a.AuthMethod), "custom_api") {
			continue
		}
		if orderID != "" && strings.EqualFold(strings.TrimSpace(a.OrderID), orderID) {
			cp := a
			return &cp
		}
		if a.BaseURL == baseURL && a.KiroApiKey == apiKey {
			cp := a
			return &cp
		}
	}
	return nil
}

// handleAdminAddCustomApiAccount POST /admin/add_custom_api_account — add a
// pool-linking account that forwards traffic to another Kiro-Go pool. Validates
// (order id → dedup → upstream quota) before persisting. Mirrors the shape of
// handleAdminAddKiroApiKey so the bot and panel read the same way.
func (h *Handler) handleAdminAddCustomApiAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	var req adminAddCustomApiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ApiKey = strings.TrimSpace(req.ApiKey)
	req.OrderID = strings.TrimSpace(req.OrderID)
	if req.ApiKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "apiKey is required"})
		return
	}
	if req.OrderID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "orderId is required"})
		return
	}
	baseURL, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Dedup BEFORE the network probe so a repeat add is cheap and can't be
	// rejected for a transient upstream blip.
	if dup := findCustomApiDuplicate(req.OrderID, baseURL, req.ApiKey); dup != nil {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "duplicate custom API account", "id": dup.ID})
		return
	}

	// Quota gate: prove the upstream key is live and has capacity.
	quota, err := probeCustomApiQuota(baseURL, req.ApiKey)
	if err != nil || !customApiQuotaAcceptable(quota) {
		msg := "upstream quota check failed"
		if err != nil {
			msg = err.Error()
		}
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}

	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		nickname = req.OrderID
	}
	// Final tag set: "Custom API" plus any extras, de-duplicated.
	tags := []string{"Custom API"}
	for _, t := range req.Tags {
		if t = strings.TrimSpace(t); t != "" && !strings.EqualFold(t, "Custom API") {
			tags = append(tags, t)
		}
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	account := config.Account{
		ID:          auth.GenerateAccountID(),
		Nickname:    nickname,
		AuthMethod:  "custom_api",
		BaseURL:     baseURL,
		KiroApiKey:  req.ApiKey,   // upstream bearer
		AccessToken: req.ApiKey,   // mirror for pool compatibility (see apiAddAccount)
		OrderID:     req.OrderID,
		Tags:        tags,
		Enabled:     enabled,
		ExpiresAt:   0, // never refreshed
		MachineId:   config.GenerateMachineId(),
	}
	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"enabled": enabled,
		"id":      account.ID,
		"orderId": account.OrderID,
		"tags":    account.Tags,
	})
}
```
Confirm `auth` and `strings` are already imported in `admin_bot_api.go` (they are — used by the sibling handlers).

- [ ] **Step 4: Register the route**

In `proxy/handler.go`, after the `add_kiro_account` case (line 459), add:
```go
	case path == "/admin/add_custom_api_account" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddCustomApiAccount(w, r)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `rtk go build ./... && rtk go test ./proxy/ -run TestAddCustomApi`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
rtk git add proxy/admin_bot_api.go proxy/handler.go proxy/admin_custom_api_test.go
rtk git commit -m "feat(admin): add /admin/add_custom_api_account with quota+dedup checks"
```

---

### Task 4: Skip token-refresh / region logic for custom_api

**Files:**
- Modify: `proxy/handler.go` (`ensureValidToken`) and any region/refresh guard that would touch a custom_api account.
- Test: `proxy/custom_api_forward_test.go` (add a case)

**Interfaces:**
- Consumes: `Account.AuthMethod`.
- Produces: `ensureValidToken` returns nil immediately for custom_api accounts.

- [ ] **Step 1: Locate the guard**

Run: `rtk grep -n "func (h \*Handler) ensureValidToken" proxy/handler.go`
Read the function. Identify where `api_key` accounts short-circuit (they are never refreshed).

- [ ] **Step 2: Write the failing test**

Add to `proxy/custom_api_forward_test.go`:
```go
// custom_api accounts must never enter token refresh — ensureValidToken is a no-op.
func TestEnsureValidTokenSkipsCustomApi(t *testing.T) {
	h := testHandler(t)
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "k"}
	if err := h.ensureValidToken(acc); err != nil {
		t.Fatalf("expected nil for custom_api, got %v", err)
	}
}
```
Add `"kiro-go/config"` import if not present.

- [ ] **Step 3: Run test to verify it fails or passes**

Run: `rtk go test ./proxy/ -run TestEnsureValidTokenSkipsCustomApi`
Expected: FAIL if the current guard tries to refresh; if it already returns nil for unknown methods, it may pass — in that case still add the explicit guard in Step 4 for clarity and commit.

- [ ] **Step 4: Add the explicit guard**

At the top of `ensureValidToken` (after the nil check), add:
```go
	// Custom API accounts carry a static upstream bearer (KiroApiKey); there is no
	// OAuth token to refresh and no expiry to honor.
	if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "custom_api") {
		return nil
	}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `rtk go test ./proxy/ -run TestEnsureValidTokenSkipsCustomApi`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
rtk git add proxy/handler.go proxy/custom_api_forward_test.go
rtk git commit -m "feat(proxy): skip token refresh for custom_api accounts"
```

---

### Task 5: forwardToUpstream — non-streaming passthrough + loop guard + metering

**Files:**
- Modify: `proxy/custom_api_forward.go`
- Test: `proxy/custom_api_forward_test.go`

**Interfaces:**
- Consumes: `Account.BaseURL`, `Account.KiroApiKey`; `h.recordSuccessForApiKey`, `h.pool.RecordSuccess`, `h.pool.UpdateStats`, `h.recordSuccessLog`.
- Produces:
  - `var forwardUpstreamRequest = func(method, url, apiKey string, body []byte, stream bool) (*http.Response, error)` — the stubbable upstream round-trip.
  - `type forwardParams struct { account *config.Account; body []byte; endpoint string; streaming bool; model string; apiKeyID string; forwarded bool }` where `endpoint` is `"anthropic"` or `"openai"`.
  - `func (h *Handler) forwardToUpstream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error` — returns a non-nil error WITHOUT writing to `w` when the request should fail over; returns nil after a successful reply is written.
  - `func upstreamPath(endpoint string) string` — `"/v1/messages"` or `"/v1/chat/completions"`.
  - `func parseUpstreamUsage(endpoint string, body []byte) (inputTokens, outputTokens int)` — best-effort usage extraction from a non-stream JSON body.

- [ ] **Step 1: Write the failing test**

Add to `proxy/custom_api_forward_test.go`:
```go
// Non-streaming forward proxies the body to {baseURL}/v1/messages and copies the
// JSON reply back verbatim.
func TestForwardNonStreamPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-up" {
			t.Fatalf("auth %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("hello")) {
			t.Fatalf("body not forwarded: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer upstream.Close()

	h := testHandler(t)
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up"}
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: acc, body: []byte(`{"model":"x","messages":[{"role":"user","content":"hello"}]}`),
		endpoint: "anthropic", streaming: false, model: "x",
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"m1"`)) {
		t.Fatalf("reply not copied: %s", rec.Body.String())
	}
}

// A request that already carries the forwarded marker is refused (loop guard).
func TestForwardLoopGuard(t *testing.T) {
	h := testHandler(t)
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "k"}
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: acc, body: []byte(`{}`), endpoint: "anthropic", forwarded: true,
	})
	if err == nil {
		t.Fatalf("expected loop-guard error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `rtk go test ./proxy/ -run 'TestForwardNonStream|TestForwardLoopGuard'`
Expected: FAIL — undefined `forwardToUpstream` / `forwardParams`.

- [ ] **Step 3: Implement the forward core**

Append to `proxy/custom_api_forward.go` (add `"bytes"` and `"kiro-go/logger"` imports as needed; use the module's real logger import path — check the top of `handler.go`):
```go
// forwardHeader marks a request that has already been forwarded once, so a
// downstream pool that links back to us refuses to add another custom_api hop.
const forwardHeader = "X-KiroGo-Forwarded"

// forwardParams bundles everything forwardToUpstream needs from a dispatch handler.
type forwardParams struct {
	account   *config.Account
	body      []byte
	endpoint  string // "anthropic" | "openai"
	streaming bool
	model     string
	apiKeyID  string
	forwarded bool // incoming request already carried forwardHeader
}

// upstreamPath maps the incoming API surface to the upstream pool's path.
func upstreamPath(endpoint string) string {
	if endpoint == "openai" {
		return "/v1/chat/completions"
	}
	return "/v1/messages"
}

// forwardUpstreamRequest performs the actual round-trip to the upstream pool. It
// is a package var so tests can stub it; the default does a real HTTP call.
var forwardUpstreamRequest = func(method, url, apiKey string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set(forwardHeader, "1")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// No client timeout: streaming replies are long-lived. Failover on connect
	// errors is handled by the caller.
	client := &http.Client{}
	return client.Do(req)
}

// parseUpstreamUsage best-effort extracts token counts from a non-stream reply.
// Anthropic: usage.input_tokens/output_tokens. OpenAI: usage.prompt_tokens/completion_tokens.
func parseUpstreamUsage(endpoint string, body []byte) (int, int) {
	var raw struct {
		Usage struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, 0
	}
	if endpoint == "openai" {
		return raw.Usage.PromptTokens, raw.Usage.CompletionTokens
	}
	return raw.Usage.InputTokens, raw.Usage.OutputTokens
}

// forwardToUpstream proxies the raw request to the account's upstream pool. On any
// failure BEFORE a 2xx reply it returns an error and writes nothing, so the caller
// can fail over to another account. After a 2xx it writes the reply and returns nil.
func (h *Handler) forwardToUpstream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	// Loop guard: never add a second hop to an already-forwarded request.
	if p.forwarded {
		return fmt.Errorf("custom_api: refusing to forward an already-forwarded request (loop guard)")
	}

	reqStart := time.Now()
	url := strings.TrimRight(p.account.BaseURL, "/") + upstreamPath(p.endpoint)
	resp, err := forwardUpstreamRequest(http.MethodPost, url, p.account.KiroApiKey, p.body, p.streaming)
	if err != nil {
		return fmt.Errorf("custom_api upstream connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a little for the error message; do NOT write to client (allow failover).
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("custom_api upstream HTTP %d: %s", resp.StatusCode, string(msg))
	}

	if p.streaming {
		return h.streamUpstream(w, flusher, resp, p, reqStart) // Task 6
	}

	// Non-streaming: copy the JSON reply verbatim, then meter.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("custom_api upstream read: %w", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(body)

	in, out := parseUpstreamUsage(p.endpoint, body)
	h.recordCustomApiSuccess(p, in, out, reqStart)
	return nil
}

// recordCustomApiSuccess mirrors the Kiro success path's metering so custom_api
// traffic shows up in pool stats, per-key usage, and request logs. Credits are
// left at 0 (the upstream pool bills us separately); token counts drive local stats.
func (h *Handler) recordCustomApiSuccess(p forwardParams, inputTokens, outputTokens int, reqStart time.Time) {
	endpoint := "claude"
	if p.endpoint == "openai" {
		endpoint = "openai"
	}
	credits := 0.0
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(p.account.ID)
	h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
	h.pool.UpdateStats(p.account.ID, inputTokens+outputTokens, credits)
	h.recordSuccessLog(endpoint, p.model, p.account.ID, p.apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
}
```
> Note: `streamUpstream` is defined in Task 6; this task will not compile until Task 6 adds it. Implement Task 6 immediately after Step 3 here, then run Step 4. (If you prefer a green build between tasks, add a temporary stub `func (h *Handler) streamUpstream(...) error { return fmt.Errorf("todo") }` and replace it in Task 6 — but do not commit the stub.)

- [ ] **Step 4: Run tests to verify they pass** (after Task 6 is implemented)

Run: `rtk go build ./... && rtk go test ./proxy/ -run 'TestForwardNonStream|TestForwardLoopGuard'`
Expected: PASS

- [ ] **Step 5: Commit** (combined with Task 6)

---

### Task 6: forwardToUpstream — streaming passthrough

**Files:**
- Modify: `proxy/custom_api_forward.go`
- Test: `proxy/custom_api_forward_test.go`

**Interfaces:**
- Consumes: everything from Task 5.
- Produces: `func (h *Handler) streamUpstream(w http.ResponseWriter, flusher http.Flusher, resp *http.Response, p forwardParams, reqStart time.Time) error`.

- [ ] **Step 1: Write the failing test**

Add to `proxy/custom_api_forward_test.go`:
```go
// Streaming forward copies the upstream SSE bytes straight to the client and taps
// usage from the terminal event.
func TestForwardStreamPassthrough(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	h := testHandler(t)
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up"}
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: acc, body: []byte(`{"stream":true}`), endpoint: "anthropic", streaming: true, model: "x",
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("message_stop")) {
		t.Fatalf("stream not copied: %s", rec.Body.String())
	}
}
```
> The `httptest.ResponseRecorder` implements `http.Flusher`, so passing `nil` for `flusher` in the test requires `streamUpstream` to tolerate a nil flusher (guard each Flush with `if flusher != nil`). In production the real handlers pass a real flusher.

- [ ] **Step 2: Run test to verify it fails**

Run: `rtk go test ./proxy/ -run TestForwardStreamPassthrough`
Expected: FAIL — undefined `streamUpstream` (or the todo stub error).

- [ ] **Step 3: Implement streaming copy**

Add to `proxy/custom_api_forward.go`:
```go
// streamUpstream copies the upstream SSE stream to the client byte-for-byte,
// flushing as data arrives, and taps token usage from the stream on the way past.
// Called only after a 2xx upstream status, so headers are safe to commit here.
func (h *Handler) streamUpstream(w http.ResponseWriter, flusher http.Flusher, resp *http.Response, p forwardParams, reqStart time.Time) error {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var outputTokens, inputTokens int
	buf := make([]byte, 16*1024)
	var tail bytes.Buffer // accumulates so we can parse usage from the tail without holding the whole stream
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := w.Write(chunk); werr != nil {
				return nil // client gone; nothing to fail over to
			}
			if flusher != nil {
				flusher.Flush()
			}
			// Keep a bounded tail for usage parsing (last ~64KB is plenty for the
			// terminal message_delta / [DONE] chunk).
			tail.Write(chunk)
			if tail.Len() > 64*1024 {
				trimmed := tail.Bytes()[tail.Len()-64*1024:]
				nb := bytes.NewBuffer(nil)
				nb.Write(trimmed)
				tail = *nb
			}
		}
		if readErr != nil {
			break // io.EOF or upstream drop; stream already delivered what it had
		}
	}
	inputTokens, outputTokens = parseStreamUsage(p.endpoint, tail.Bytes())
	h.recordCustomApiSuccess(p, inputTokens, outputTokens, reqStart)
	return nil
}

// parseStreamUsage scans SSE tail bytes for the last usage object. Anthropic emits
// usage in message_delta; OpenAI emits it in the final chunk when stream_options
// include_usage is set. Best-effort: returns 0,0 if none found.
func parseStreamUsage(endpoint string, tail []byte) (int, int) {
	lines := bytes.Split(tail, []byte("\n"))
	var lastIn, lastOut int
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if !bytes.HasPrefix(ln, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(ln, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if in, out := parseUpstreamUsage(endpoint, payload); in != 0 || out != 0 {
			if in != 0 {
				lastIn = in
			}
			if out != 0 {
				lastOut = out
			}
		}
	}
	return lastIn, lastOut
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `rtk go build ./... && rtk go test ./proxy/ -run 'TestForward'`
Expected: PASS (all forward tests, streaming + non-stream + loop guard).

- [ ] **Step 5: Commit**

```bash
rtk git add proxy/custom_api_forward.go proxy/custom_api_forward_test.go
rtk git commit -m "feat(proxy): forwardToUpstream passthrough (stream + non-stream) with usage metering"
```

---

### Task 7: Wire the fork into the 4 dispatch handlers

**Files:**
- Modify: `proxy/handler.go` — `handleClaudeMessagesInternal`, `handleClaudeStream`, `handleClaudeNonStream`, `handleOpenAIChat`, `handleOpenAIStream`, `handleOpenAINonStream`.
- Test: `proxy/custom_api_forward_test.go` (end-to-end through the entry handler)

**Interfaces:**
- Consumes: `forwardToUpstream`, `forwardParams` (Tasks 5–6).
- Produces: the 4 stream/non-stream handlers gain two params: `rawBody []byte, forwarded bool`. Inside each retry loop, custom_api accounts are dispatched via `forwardToUpstream` instead of `CallKiroAPI`.

- [ ] **Step 1: Thread rawBody + forwarded from the entry handlers**

In `handleClaudeMessagesInternal` (line ~860), after `apiKeyID := apiKeyIDFromContext(r.Context())`:
```go
	forwarded := r.Header.Get(forwardHeader) != ""
```
Change the two calls to pass `body` and `forwarded`:
```go
	if req.Stream {
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
	} else {
		h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
	}
```
Update the two handler signatures (`handleClaudeStream` line 905, `handleClaudeNonStream` line 1514) to add `rawBody []byte, forwarded bool` at the end.

Do the same in `handleOpenAIChat` (line ~1655): add `forwarded := r.Header.Get(forwardHeader) != ""`, pass `body, forwarded` to `handleOpenAIStream` (line 1694) and `handleOpenAINonStream` (line 2105), and extend those two signatures.

- [ ] **Step 2: Add the fork inside each retry loop**

In each of the 4 handlers, immediately after the account is selected and validated — i.e. after the `if err := h.ensureValidToken(account); err != nil { ... continue }` block and before `CallKiroAPI` — insert:
```go
		// Custom API accounts are transparent proxies to another Kiro-Go pool: forward
		// the raw request instead of translating to Kiro. Success ends the request;
		// any pre-reply failure falls over to the next account like a Kiro error.
		if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "custom_api") {
			ep := "anthropic"
			streaming := true // set per handler: true in *Stream, false in *NonStream
			if fwdErr := h.forwardToUpstream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: ep, streaming: streaming,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); fwdErr != nil {
				lastErr = fwdErr
				excluded[account.ID] = true
				h.handleAccountFailure(account, fwdErr)
				continue
			}
			return
		}
```
Per-handler specifics:
- `handleClaudeStream`: `ep := "anthropic"`, `streaming := true`, `flusher` already in scope.
- `handleClaudeNonStream`: `ep := "anthropic"`, `streaming := false`; pass `nil` for flusher.
- `handleOpenAIStream`: `ep := "openai"`, `streaming := true`, `flusher` in scope.
- `handleOpenAINonStream`: `ep := "openai"`, `streaming := false`; pass `nil` for flusher.

For the non-stream handlers, the response is already committed by `forwardToUpstream`, so `return` after success is correct.

- [ ] **Step 3: Write the failing end-to-end test**

Add to `proxy/custom_api_forward_test.go`:
```go
// A request that routes to a custom_api account is proxied end-to-end through the
// public Claude entry handler.
func TestClaudeEntryForwardsCustomApi(t *testing.T) {
	resetTestConfig(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":3,"output_tokens":4}}`))
	}))
	defer upstream.Close()

	// Add a single custom_api account pointed at the upstream test server.
	config.AddAccount(config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	})
	h := testHandler(t)
	h.pool.Reload()

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"m1"`)) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}
```
> If `testHandler`/`resetTestConfig` are not the real helper names, adapt to the existing ones (see `proxy/handler_test.go`). If the pool requires model-list warmup to route, note that an empty model list routes optimistically (`pool/account.go:221`), so no warmup is needed.

- [ ] **Step 4: Run the full suite**

Run: `rtk go build ./... && rtk go test ./proxy/ -run 'TestForward|TestClaudeEntryForwards|TestAddCustomApi'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
rtk git add proxy/handler.go proxy/custom_api_forward_test.go
rtk git commit -m "feat(proxy): route custom_api accounts through upstream forwarding in dispatch"
```

---

### Task 8: Failover integration test (upstream 5xx → next account)

**Files:**
- Test: `proxy/custom_api_forward_test.go`

**Interfaces:**
- Consumes: dispatch fork (Task 7).

- [ ] **Step 1: Write the failing test**

Add:
```go
// When the upstream pool returns 500, the custom_api account is excluded and the
// request fails over. With no other account, the caller sees an error (not a hang).
func TestForwardFailoverOn5xx(t *testing.T) {
	resetTestConfig(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer upstream.Close()

	config.AddAccount(config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	})
	h := testHandler(t)
	h.pool.Reload()

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 after upstream 5xx with no fallback account")
	}
}
```

- [ ] **Step 2: Run test**

Run: `rtk go test ./proxy/ -run TestForwardFailoverOn5xx`
Expected: PASS (the fork already excludes on error and the loop exhausts to an error response). If it hangs or 200s, revisit Task 7's error branch.

- [ ] **Step 3: Commit**

```bash
rtk git add proxy/custom_api_forward_test.go
rtk git commit -m "test(proxy): custom_api upstream 5xx fails over"
```

---

### Task 9: Telegram bot command `/add_custom_api`

**Files:**
- Modify: `proxy/admin_bot_api.go` (Telegram command dispatch)

**Interfaces:**
- Consumes: `handleAdminAddCustomApiAccount` logic (reuse the validation by calling the same helpers, or POST to the endpoint internally).

- [ ] **Step 1: Locate the bot command dispatch**

Run: `rtk grep -n "add_kiro_api_key\|/add_kiro\|command ==\|case \"/\|BotCommand\|switch cmd" proxy/admin_bot_api.go`
Find where existing slash-commands (e.g. the one that maps to `add_kiro_api_key`) are parsed and dispatched.

- [ ] **Step 2: Add the command**

Mirror the existing `add_kiro_api_key` bot command. Parse args `baseUrl apiKey orderId [tags...]`, build an `adminAddCustomApiRequest`, and invoke the same validation+persist path (extract the body of `handleAdminAddCustomApiAccount` into a shared helper `func (h *Handler) addCustomApiAccount(req adminAddCustomApiRequest) (id string, status int, err error)` if the current handler is HTTP-coupled, and have both the HTTP handler and the bot command call it). Reply to the chat with the resulting id or the error message.

> This task has no unit test harness in-repo for the bot transport; verify by build + manual `/add_custom_api` against a staging bot. If the repo has bot-command tests (grep `func Test.*Bot`), add one mirroring them.

- [ ] **Step 3: Build check**

Run: `rtk go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
rtk git add proxy/admin_bot_api.go
rtk git commit -m "feat(bot): /add_custom_api command for pool-linking accounts"
```

---

### Task 10: Web panel — add form + list badge

**Files:**
- Modify: `web/index.html`, `web/app.js`, `web/locales/en.json`, `web/locales/zh.json`

**Interfaces:**
- Consumes: `POST /admin/add_custom_api_account`.

- [ ] **Step 1: Locate the existing add-account UI**

Run: `rtk grep -n "add_kiro_api_key\|addAccount\|api/accounts\|Add Account\|authMethod\|api_key" web/app.js web/index.html`
Identify the existing "add account" form + submit handler and the account-list row renderer.

- [ ] **Step 2: Add the form**

Add a "Custom API" add form next to the existing add-account controls with inputs: Base URL, API Key, Order ID, Tags (optional). On submit, POST JSON to `/admin/add_custom_api_account`:
```js
// Submit a pool-linking (Custom API) account: forwards our traffic to another
// Kiro-Go pool identified by baseUrl + apiKey.
async function addCustomApiAccount(baseUrl, apiKey, orderId, tags) {
  const res = await fetch('/admin/add_custom_api_account', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...adminAuthHeaders() },
    body: JSON.stringify({ baseUrl, apiKey, orderId, tags }),
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || 'Add failed');
  return data;
}
```
Reuse whatever admin-auth header helper the existing admin fetches use (grep `adminAuthHeaders` / `X-Admin-Password` / `Authorization` in `app.js`); if the name differs, match the existing pattern.

- [ ] **Step 3: Show the badge + fields in the account list**

In the account-row renderer, when `account.authMethod === 'custom_api'`, render a "Custom API" tag/badge, the masked key, `orderId`, and `baseUrl`. Follow the existing badge styling used for other auth methods.

- [ ] **Step 4: Add i18n strings**

Add keys for the form labels + badge to `web/locales/en.json` and `web/locales/zh.json`, following the existing key structure.

- [ ] **Step 5: Manual verification**

Load the panel, add a Custom API account against a staging upstream, confirm it appears with the badge and that a request routes through it.

- [ ] **Step 6: Commit**

```bash
rtk git add web/index.html web/app.js web/locales/en.json web/locales/zh.json
rtk git commit -m "feat(web): Custom API account add form + list badge"
```

---

### Task 11: Full build, test, and code-review pass

- [ ] **Step 1: Full build + test**

Run: `rtk go build ./... && rtk go test ./proxy/ ./config/`
Expected: all PASS.

- [ ] **Step 2: Mandatory code-review pass**

Per repo CLAUDE.md: spawn a dedicated code-review sub-agent (model `claude-fable-5`, xhigh thinking) to review the full diff and fix issues before handoff. Do not self-approve in the same context.

- [ ] **Step 3: Final commit of any review fixes**

```bash
rtk git add -A
rtk git commit -m "fix(proxy): address code-review findings for Custom API pool linking"
```

---

## Self-Review

**Spec coverage:**
- Data model (BaseURL/OrderID/Tags/custom_api) → Task 1. ✅
- Add endpoint + quota check → Tasks 2–3. ✅
- Order ID as name + dedup → Task 3. ✅
- Tags default "Custom API" → Task 3. ✅
- No token refresh for custom_api → Task 4. ✅
- Transparent passthrough (stream + non-stream) → Tasks 5–6. ✅
- Dispatch fork in 4 handlers → Task 7. ✅
- Metering → Tasks 5–6 (`recordCustomApiSuccess`). ✅
- Failover on upstream error → Tasks 7–8. ✅
- Loop guard (`X-KiroGo-Forwarded`) → Tasks 5, 7. ✅
- All-models default → Task 7 note (no code needed). ✅
- Telegram command → Task 9. ✅
- Web UI → Task 10. ✅
- Tests (quota accept/reject, dedup, passthrough, metering, failover, loop guard) → Tasks 2,3,5,6,7,8. ✅

**Type consistency:** `forwardParams` fields, `forwardToUpstream`/`streamUpstream`/`recordCustomApiSuccess`/`parseUpstreamUsage`/`parseStreamUsage` signatures are consistent across Tasks 5–8. `probeCustomApiQuota`/`customApiQuotaAcceptable`/`normalizeBaseURL` consistent across Tasks 2–3.

**Known adaptation points (not placeholders — real seams the implementer confirms against the repo):**
- Test helper names (`testHandler`, `resetTestConfig`) — use the repo's existing equivalents.
- `ensureValidToken` exact short-circuit location (Task 4).
- Bot command dispatch structure (Task 9) and web add-account UI location (Task 10) — both instruct locating the existing sibling and mirroring it.
- Module import path for the logger (`kiro-go/logger` assumed; confirm from `handler.go`).
