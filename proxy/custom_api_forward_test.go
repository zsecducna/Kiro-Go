package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// newCustomApiTestHandler initializes an isolated config store, adds the given
// custom_api account, and returns a Handler wired to the reloaded pool.
func newCustomApiTestHandler(t *testing.T, acc config.Account) *Handler {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if acc.ID != "" {
		if err := config.AddAccount(acc); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p}
}

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
	if _, err := normalizeBaseURL("http://"); err == nil {
		t.Fatalf("expected error for empty host")
	}
	if _, err := normalizeBaseURL("https://x.com/?a=1"); err == nil {
		t.Fatalf("expected error for query string")
	}
	if got, err := normalizeBaseURL("https://x.com/gw/"); err != nil || got != "https://x.com/gw" {
		t.Fatalf("path preserved: got %q err %v", got, err)
	}
}

// A request already marked as forwarded must NOT be re-forwarded to a custom_api
// account, and the healthy account must not be disabled (loop-guard is not a failure).
func TestForwardedRequestSkipsCustomApiWithoutBan(t *testing.T) {
	// forwardUpstreamRequest must never be called on this path.
	orig := forwardUpstreamRequest
	called := false
	forwardUpstreamRequest = func(method, url, apiKey string, body []byte, stream bool) (*http.Response, error) {
		called = true
		return nil, nil
	}
	defer func() { forwardUpstreamRequest = orig }()

	acc := config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	}
	h := newCustomApiTestHandler(t, acc)

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	req.Header.Set(forwardHeader, "1")
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if called {
		t.Fatalf("forwardUpstreamRequest must not run for an already-forwarded request")
	}
	if got, _ := config.GetAccountByID("c1"); got.Enabled != true || got.BanStatus != "" {
		t.Fatalf("healthy custom_api account must not be banned; enabled=%v ban=%q", got.Enabled, got.BanStatus)
	}
}

// custom_api accounts must never enter token refresh — ensureValidToken is a no-op.
func TestEnsureValidTokenSkipsCustomApi(t *testing.T) {
	h := newCustomApiTestHandler(t, config.Account{})
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "k"}
	if err := h.ensureValidToken(acc); err != nil {
		t.Fatalf("expected nil for custom_api, got %v", err)
	}
}

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
		if r.Header.Get(forwardHeader) != "1" {
			t.Fatalf("missing forward marker header")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("hello")) {
			t.Fatalf("body not forwarded: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer upstream.Close()

	acc := config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: &acc, body: []byte(`{"model":"x","messages":[{"role":"user","content":"hello"}]}`),
		endpoint: "anthropic", streaming: false, model: "x",
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"m1"`)) {
		t.Fatalf("reply not copied: %s", rec.Body.String())
	}
}

// Custom API traffic bills the customer's key/pool in credits derived from tokens at
// the configured rate (upstream reply has no credit figure). Without this, credit-
// limited keys never exhaust.
func TestCustomApiChargesCredits(t *testing.T) {
	t.Setenv("CUSTOM_API_CREDITS_PER_1K_TOKENS", "2.0")
	if got := customApiCreditsPer1kTokens(); got != 2.0 {
		t.Fatalf("rate override: got %v", got)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","usage":{"input_tokens":300,"output_tokens":700}}`))
	}))
	defer upstream.Close()

	acc := config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	rec := httptest.NewRecorder()
	if err := h.forwardToUpstream(rec, nil, forwardParams{
		account: &acc, body: []byte(`{}`), endpoint: "anthropic", streaming: false, model: "x",
	}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	// 1000 tokens / 1000 * 2.0 = 2.0 credits.
	if got := h.getCredits(); got < 1.99 || got > 2.01 {
		t.Fatalf("expected ~2.0 credits charged, got %v", got)
	}
}

// Upstream /api/me credits map onto the panel's main-quota fields.
func TestCustomApiQuotaToAccountInfo(t *testing.T) {
	q := &customApiQuota{CreditsUsed: 25, CreditLimit: 100, OK: true}
	info := q.toAccountInfo(12345)
	if info.UsageCurrent != 25 || info.UsageLimit != 100 {
		t.Fatalf("usage: %+v", info)
	}
	if info.UsagePercent < 0.249 || info.UsagePercent > 0.251 {
		t.Fatalf("percent: %v", info.UsagePercent)
	}
	if info.LastRefresh != 12345 || info.SubscriptionType != "Custom API" {
		t.Fatalf("meta: %+v", info)
	}
	// Unlimited (limit 0) → no percent, no div-by-zero.
	if u := (&customApiQuota{CreditsUsed: 5, CreditLimit: 0, OK: true}).toAccountInfo(1); u.UsagePercent != 0 {
		t.Fatalf("unlimited percent should be 0, got %v", u.UsagePercent)
	}
}

// Model list comes from the upstream provider's /v1/models, not Kiro.
func TestProbeCustomApiModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-up" {
			t.Fatalf("auth %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"claude-opus-4.8"},{"id":"gpt-4o"},{"id":""}]}`))
	}))
	defer upstream.Close()

	models, err := probeCustomApiModels(upstream.URL, "sk-up")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(models) != 2 || models[0].ModelId != "claude-opus-4.8" || models[1].ModelId != "gpt-4o" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

// The admin test button sends a real chat request through the upstream and returns
// the assistant reply text.
func TestCustomApiTestReply(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok!"}}]}`))
	}))
	defer upstream.Close()

	reply, err := customApiTestReply(upstream.URL, "sk-up", "claude-opus-4.8")
	if err != nil {
		t.Fatalf("test reply: %v", err)
	}
	if reply != "ok!" {
		t.Fatalf("reply %q", reply)
	}
}

// A request that already carries the forwarded marker is refused (loop guard).
func TestForwardLoopGuard(t *testing.T) {
	h := newCustomApiTestHandler(t, config.Account{})
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "k"}
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: acc, body: []byte(`{}`), endpoint: "anthropic", forwarded: true,
	})
	if err == nil {
		t.Fatalf("expected loop-guard error")
	}
}

// Streaming forward copies the upstream SSE bytes straight to the client.
func TestForwardStreamPassthrough(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	acc := config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: &acc, body: []byte(`{"stream":true}`), endpoint: "anthropic", streaming: true, model: "x",
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("message_stop")) {
		t.Fatalf("stream not copied: %s", rec.Body.String())
	}
}

// A request that routes to a custom_api account is proxied end-to-end through the
// public Claude entry handler.
func TestClaudeEntryForwardsCustomApi(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":3,"output_tokens":4}}`))
	}))
	defer upstream.Close()

	acc := config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	}
	h := newCustomApiTestHandler(t, acc)

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"m1"`)) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

// When the upstream pool returns 500, the custom_api account is excluded and the
// request fails over. With no other account, the caller sees an error (not a hang).
func TestForwardFailoverOn5xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer upstream.Close()

	acc := config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	}
	h := newCustomApiTestHandler(t, acc)

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 after upstream 5xx with no fallback account")
	}
}
