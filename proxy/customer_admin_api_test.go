package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedKey adds an API key entry with the given quota/usage and returns it.
func seedKey(t *testing.T, name string, creditLimit, creditsUsed float64) config.ApiKeyEntry {
	t.Helper()
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Name:        name,
		Key:         config.GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: creditLimit,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if creditsUsed > 0 {
		if err := config.RecordApiKeyUsage(entry.ID, 0, creditsUsed); err != nil {
			t.Fatalf("RecordApiKeyUsage: %v", err)
		}
	}
	updated := config.GetApiKeyEntry(entry.ID)
	if updated == nil {
		t.Fatalf("entry vanished after seeding")
	}
	return *updated
}

// serve routes a request through the full ServeHTTP router so tests cover
// routing + auth + handler together.
func serve(h *Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON response: %v (%s)", err, rec.Body.String())
	}
	return m
}

// --- Customer endpoints -------------------------------------------------

func TestCustomerMeRequiresValidKey(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{}

	// No key at all → 401.
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without key, got %d", rec.Code)
	}

	// Unknown key → 401.
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer sk-unknown")
	if rec := serve(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown key, got %d", rec.Code)
	}
}

func TestCustomerMeReturnsQuota(t *testing.T) {
	mustInitConfig(t)
	entry := seedKey(t, "buyer-1", 1000, 250)
	h := &Handler{}

	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("X-Api-Key", entry.Key)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["status"] != "active" {
		t.Fatalf("expected status active, got %v", body["status"])
	}
	if got := body["creditsRemaining"].(float64); got != 750 {
		t.Fatalf("expected creditsRemaining 750, got %v", got)
	}
	if strings.Contains(rec.Body.String(), entry.Key) {
		t.Fatalf("cleartext key leaked in /api/me response")
	}
}

// Exhausted (auto-disabled) keys must still be able to read their own stats.
func TestCustomerMeWorksAfterExhaustion(t *testing.T) {
	mustInitConfig(t)
	entry := seedKey(t, "buyer-2", 100, 100) // usage == limit → auto-disabled

	refreshed := config.GetApiKeyEntry(entry.ID)
	if refreshed.Enabled {
		t.Fatalf("expected auto-deactivation at quota, still enabled")
	}

	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("X-Api-Key", entry.Key)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for exhausted key introspection, got %d", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["status"] != "exhausted" {
		t.Fatalf("expected status exhausted, got %v", body["status"])
	}
	if got := body["creditsRemaining"].(float64); got != 0 {
		t.Fatalf("expected creditsRemaining 0, got %v", got)
	}
}

func TestCustomerStatsScopedToKey(t *testing.T) {
	mustInitConfig(t)
	entry := seedKey(t, "buyer-3", 500, 10)
	other := seedKey(t, "buyer-4", 500, 400)
	_ = other

	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	r.Header.Set("Authorization", "Bearer "+entry.Key)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := decodeBody(t, rec)
	// Must reflect only this key's 10 used credits, not the other key's 400.
	if got := body["creditsUsed"].(float64); got != 10 {
		t.Fatalf("expected creditsUsed 10, got %v", got)
	}
}

func TestCustomerLogsFilteredByKey(t *testing.T) {
	mustInitConfig(t)
	entry := seedKey(t, "buyer-5", 500, 0)
	other := seedKey(t, "buyer-6", 500, 0)

	h := &Handler{}
	// Two logs for our key (one success, one failure), one for another key.
	h.recordSuccessLog("claude", "m1", "acc-1", entry.ID, 100, 1.5, 20)
	h.recordFailureWithDetails("claude", "m2", "acc-1", entry.ID, errTest("boom"))
	h.recordSuccessLog("openai", "m3", "acc-2", other.ID, 50, 0.5, 10)

	r := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	r.Header.Set("X-Api-Key", entry.Key)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Logs  []customerLogView `json:"logs"`
		Count int               `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if body.Count != 2 || len(body.Logs) != 2 {
		t.Fatalf("expected 2 logs for key, got %d", body.Count)
	}
	// Newest first: failure logged after success.
	if body.Logs[0].Status != "error" || body.Logs[1].Status != "success" {
		t.Fatalf("unexpected order/status: %+v", body.Logs)
	}
	// Internal pool account IDs must not leak to customers.
	if strings.Contains(rec.Body.String(), "acc-1") {
		t.Fatalf("accountId leaked in customer logs")
	}
}

// errTest is a tiny error helper (avoids importing errors just for tests).
type errTest string

func (e errTest) Error() string { return string(e) }

// --- Admin endpoints ----------------------------------------------------

func adminReq(method, path, body, password string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if password != "" {
		r.Header.Set("X-Admin-Password", password)
	}
	return r
}

func TestAdminEndpointsRejectBadKey(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	h := &Handler{}

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/admin/new_api_key"},
		{http.MethodPost, "/admin/stats"},
		{http.MethodGet, "/admin/pool"},
	} {
		// Wrong password → 401.
		if rec := serve(h, adminReq(tc.method, tc.path, "{}", "wrong")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 with wrong password, got %d", tc.method, tc.path, rec.Code)
		}
		// Customer API key must NOT work as admin key.
		entry := seedKey(t, "not-admin", 10, 0)
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+entry.Key)
		if rec := serve(h, r); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: customer key accepted as admin, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAdminNewApiKeyMintsQuotaKey(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	h := &Handler{}

	rec := serve(h, adminReq(http.MethodPost, "/admin/new_api_key", `{"name":"order-42","credits":1000}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	key, _ := body["key"].(string)
	if !strings.HasPrefix(key, "sk-") {
		t.Fatalf("expected cleartext sk- key in response, got %v", body["key"])
	}
	if got := body["credits"].(float64); got != 1000 {
		t.Fatalf("expected credits 1000, got %v", got)
	}

	// The minted key must authenticate as a customer key with the right quota.
	entry := config.FindApiKeyByValue(key)
	if entry == nil || entry.CreditLimit != 1000 || !entry.Enabled {
		t.Fatalf("minted key not persisted correctly: %+v", entry)
	}

	// credits <= 0 must be rejected.
	rec = serve(h, adminReq(http.MethodPost, "/admin/new_api_key", `{"credits":0}`, "topsecret"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for credits=0, got %d", rec.Code)
	}
}

func TestAdminStatsFilters(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	a := seedKey(t, "alpha", 1000, 100)
	_ = seedKey(t, "beta", 2000, 500)
	h := &Handler{}

	// Unfiltered → both keys.
	rec := serve(h, adminReq(http.MethodPost, "/admin/stats", `{}`, "topsecret"))
	body := decodeBody(t, rec)
	totals := body["totals"].(map[string]interface{})
	if got := totals["keys"].(float64); got != 2 {
		t.Fatalf("expected 2 keys, got %v", got)
	}
	if got := totals["creditsUsed"].(float64); got != 600 {
		t.Fatalf("expected creditsUsed 600, got %v", got)
	}

	// Filtered by full key value → just alpha.
	rec = serve(h, adminReq(http.MethodPost, "/admin/stats", `{"apiKey":"`+a.Key+`"}`, "topsecret"))
	body = decodeBody(t, rec)
	keys := body["apiKeys"].([]interface{})
	if len(keys) != 1 {
		t.Fatalf("expected 1 filtered key, got %d", len(keys))
	}
	kv := keys[0].(map[string]interface{})
	if kv["name"] != "alpha" || kv["creditsRemaining"].(float64) != 900 {
		t.Fatalf("unexpected filtered entry: %v", kv)
	}
}

func TestAdminAddKiroApiKey(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// Missing key → 400.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key", `{"kiroApiKey":"  "}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty key, got %d", rec.Code)
	}

	// Wrong prefix → 400.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key", `{"kiroApiKey":"order-123"}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-ksk_ key, got %d", rec.Code)
	}

	// Wrong admin key → 401.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key", `{"kiroApiKey":"ksk_x"}`, "wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong admin key, got %d", rec.Code)
	}

	// Valid add (enabled:false to avoid the live model-fetch goroutine).
	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_pool1","nickname":"cap-1","region":"eu-west-1","enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["success"] != true || body["enabled"] != false {
		t.Fatalf("unexpected response: %v", body)
	}

	var stored *config.Account
	all := config.GetAccounts()
	for i := range all {
		if all[i].KiroApiKey == "ksk_pool1" {
			stored = &all[i]
			break
		}
	}
	if stored == nil {
		t.Fatal("kiro api key account not persisted")
	}
	if body["id"] != stored.ID {
		t.Fatalf("response id %v != stored id %s", body["id"], stored.ID)
	}
	if !stored.IsApiKeyCredential() || stored.AccessToken != "ksk_pool1" || stored.ExpiresAt != 0 ||
		stored.Region != "eu-west-1" || stored.Nickname != "cap-1" || stored.Enabled {
		t.Fatalf("account not normalized: %+v", stored)
	}

	// Idempotent retry: same key → same id, duplicate flag, no second account.
	rec = serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_pool1","enabled":false}`, "topsecret"))
	dupBody := decodeBody(t, rec)
	if dupBody["duplicate"] != true || dupBody["id"] != stored.ID {
		t.Fatalf("expected idempotent duplicate, got %v", dupBody)
	}
	count := 0
	for _, a := range config.GetAccounts() {
		if a.KiroApiKey == "ksk_pool1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 account for the key, got %d", count)
	}
}

func TestAdminPoolMath(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")

	// Two usable accounts with 10k credits each, one already half used.
	for _, acc := range []config.Account{
		{ID: "a1", Email: "a1@x", Enabled: true, UsageLimit: 10000, UsageCurrent: 0},
		{ID: "a2", Email: "a2@x", Enabled: true, UsageLimit: 10000, UsageCurrent: 5000},
		// Banned account must be excluded entirely.
		{ID: "a3", Email: "a3@x", Enabled: true, BanStatus: "BANNED", UsageLimit: 10000},
	} {
		if err := config.AddAccount(acc); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}

	// Sold keys: 5k quota with 1k used (4k outstanding) + one exhausted
	// (auto-disabled, 0 outstanding).
	_ = seedKey(t, "sold-1", 5000, 1000)
	_ = seedKey(t, "sold-2", 2000, 2000)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	rec := serve(h, adminReq(http.MethodGet, "/admin/pool", "", "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)

	accounts := body["accounts"].(map[string]interface{})
	// Available: a1 10000 + a2 5000; banned a3 excluded.
	if got := accounts["creditsAvailable"].(float64); got != 15000 {
		t.Fatalf("expected creditsAvailable 15000, got %v", got)
	}
	sold := body["sold"].(map[string]interface{})
	if got := sold["outstandingCredits"].(float64); got != 4000 {
		t.Fatalf("expected outstandingCredits 4000, got %v", got)
	}
	if got := body["sellableCredits"].(float64); got != 11000 {
		t.Fatalf("expected sellableCredits 11000, got %v", got)
	}
}

// Auto-deactivation regression: crossing the quota boundary flips Enabled off
// and further inference auth fails, while introspection keeps working.
func TestQuotaExhaustionDeactivatesKey(t *testing.T) {
	mustInitConfig(t)
	requireAuth(t)
	entry := seedKey(t, "meter", 100, 0)
	h := &Handler{}

	// Consume 99.5 credits → still active.
	if err := config.RecordApiKeyUsage(entry.ID, 0, 99.5); err != nil {
		t.Fatalf("RecordApiKeyUsage: %v", err)
	}
	r := newAuthTestRequest(t, "X-Api-Key", entry.Key)
	if _, err := h.authenticate(r); err != nil {
		t.Fatalf("expected auth success below quota: %v", err)
	}

	// Cross the boundary → disabled + rejected.
	if err := config.RecordApiKeyUsage(entry.ID, 0, 1); err != nil {
		t.Fatalf("RecordApiKeyUsage: %v", err)
	}
	if got := config.GetApiKeyEntry(entry.ID); got.Enabled {
		t.Fatalf("expected key disabled after exhaustion")
	}
	if _, err := h.authenticate(r); err == nil {
		t.Fatalf("expected auth failure after exhaustion")
	}
}
