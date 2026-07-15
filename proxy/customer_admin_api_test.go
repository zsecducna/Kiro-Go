package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// The self-service introspection endpoints must answer on both GET and POST so
// the bot can query with either verb; a POST body is ignored.
func TestCustomerIntrospectionAcceptsPost(t *testing.T) {
	mustInitConfig(t)
	entry := seedKey(t, "buyer-post", 1000, 250)
	h := &Handler{}

	for _, path := range []string{"/api/me", "/api/stats", "/api/logs"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"ignored":true}`))
		r.Header.Set("X-Api-Key", entry.Key)
		rec := serve(h, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
		}
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

func TestAdminDeleteApiKey(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	h := &Handler{}

	// Missing selector → 400.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/delete_api_key", `{}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no id/apiKey, got %d", rec.Code)
	}
	// Unknown id → 404.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/delete_api_key", `{"id":"nope"}`, "topsecret")); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown id, got %d", rec.Code)
	}
	// Wrong admin key → 401.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/delete_api_key", `{"id":"x"}`, "wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong admin key, got %d", rec.Code)
	}

	// Delete by id.
	a := seedKey(t, "byid", 100, 0)
	rec := serve(h, adminReq(http.MethodPost, "/admin/delete_api_key", `{"id":"`+a.ID+`"}`, "topsecret"))
	if rec.Code != http.StatusOK || decodeBody(t, rec)["id"] != a.ID {
		t.Fatalf("delete by id failed: %d %s", rec.Code, rec.Body.String())
	}
	if config.GetApiKeyEntry(a.ID) != nil {
		t.Fatal("key still present after delete-by-id")
	}

	// Delete by cleartext value.
	b := seedKey(t, "byvalue", 100, 0)
	rec = serve(h, adminReq(http.MethodPost, "/admin/delete_api_key", `{"apiKey":"`+b.Key+`"}`, "topsecret"))
	if rec.Code != http.StatusOK || decodeBody(t, rec)["id"] != b.ID {
		t.Fatalf("delete by value failed: %d %s", rec.Code, rec.Body.String())
	}
	if config.GetApiKeyEntry(b.ID) != nil {
		t.Fatal("key still present after delete-by-value")
	}

	// Deleting the same key again → 404 (no false success).
	if rec := serve(h, adminReq(http.MethodPost, "/admin/delete_api_key", `{"id":"`+b.ID+`"}`, "topsecret")); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 re-deleting, got %d", rec.Code)
	}

	// Contradictory id+apiKey (naming different keys) → 400, nothing deleted.
	c := seedKey(t, "keepC", 100, 0)
	d := seedKey(t, "keepD", 100, 0)
	rec = serve(h, adminReq(http.MethodPost, "/admin/delete_api_key",
		`{"id":"`+c.ID+`","apiKey":"`+d.Key+`"}`, "topsecret"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for mismatched id/apiKey, got %d", rec.Code)
	}
	if config.GetApiKeyEntry(c.ID) == nil || config.GetApiKeyEntry(d.ID) == nil {
		t.Fatal("mismatched request must not delete either key")
	}
}

func TestAdminRechargeApiKey(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	h := &Handler{}

	// Missing selector → 400.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key", `{"credits":10}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no id/apiKey, got %d", rec.Code)
	}
	// No amount → 400.
	a0 := seedKey(t, "noamount", 100, 0)
	if rec := serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key", `{"id":"`+a0.ID+`"}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no credits/tokens, got %d", rec.Code)
	}
	// Negative amount → 400.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key", `{"id":"`+a0.ID+`","credits":-5}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative credits, got %d", rec.Code)
	}
	// Unknown id → 404.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key", `{"id":"nope","credits":10}`, "topsecret")); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown id, got %d", rec.Code)
	}
	// Wrong admin key → 401.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key", `{"id":"x","credits":10}`, "wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong admin key, got %d", rec.Code)
	}

	// Core scenario: a key sold with 100 credits, fully consumed (auto-disabled),
	// recharged by 50 → limit 150, still 100 used, 50 remaining, re-enabled.
	exhausted := seedKey(t, "exhausted", 100, 100)
	if exhausted.Enabled {
		t.Fatal("precondition: key should be auto-disabled after consuming its full quota")
	}
	rec := serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key",
		`{"apiKey":"`+exhausted.Key+`","credits":50}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("recharge failed: %d %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["enabled"] != true {
		t.Fatalf("expected key re-enabled after top-up, got %v", body["enabled"])
	}
	if got := body["creditLimit"].(float64); got != 150 {
		t.Fatalf("expected creditLimit 150, got %v", got)
	}
	if got := body["creditsRemaining"].(float64); got != 50 {
		t.Fatalf("expected creditsRemaining 50, got %v", got)
	}
	// Persisted: the key authenticates again and usage history is preserved.
	stored := config.GetApiKeyEntry(exhausted.ID)
	if stored == nil || !stored.Enabled || stored.CreditLimit != 150 || stored.CreditsUsed != 100 {
		t.Fatalf("recharge not persisted correctly: %+v", stored)
	}

	// Partial recharge (less than the overage) must NOT re-enable: 100 used,
	// limit raised 100→120 is still under? No — 100 < 120, so it WOULD re-enable.
	// Construct a genuine still-over case: 200 used against a 100 limit, top up 50
	// → limit 150 < 200 used, stays disabled.
	over := seedKey(t, "over", 100, 200)
	rec = serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key",
		`{"id":"`+over.ID+`","credits":50}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("recharge(over) failed: %d %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["enabled"] != false {
		t.Fatal("key still over limit after partial top-up must stay disabled")
	}

	// Recharging credits on an unlimited (limit 0) key → 400, no mutation: adding
	// to a 0 limit would convert it to metered and could instantly over-limit it.
	unlimited := seedKey(t, "unlimited", 0, 0)
	rec = serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key",
		`{"id":"`+unlimited.ID+`","credits":100}`, "topsecret"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 recharging an unlimited key, got %d %s", rec.Code, rec.Body.String())
	}
	if e := config.GetApiKeyEntry(unlimited.ID); e == nil || e.CreditLimit != 0 {
		t.Fatalf("unlimited key must not be converted to metered: %+v", e)
	}

	// Contradictory id+apiKey → 400, no top-up applied.
	c := seedKey(t, "keepC", 100, 0)
	d := seedKey(t, "keepD", 100, 0)
	rec = serve(h, adminReq(http.MethodPost, "/admin/recharge_api_key",
		`{"id":"`+c.ID+`","apiKey":"`+d.Key+`","credits":10}`, "topsecret"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for mismatched id/apiKey, got %d", rec.Code)
	}
	if e := config.GetApiKeyEntry(c.ID); e == nil || e.CreditLimit != 100 {
		t.Fatalf("mismatched request must not recharge: %+v", e)
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

	// Stub the upstream probe: any key resolves to the same Kiro account identity
	// (userId "kiro-user-A") in eu-west-1 only, with a 10k quota. This lets us drive
	// the dedup-by-account logic without live network.
	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if region != "eu-west-1" {
			return nil, errTest("HTTP 403: not served in " + region)
		}
		return &config.AccountInfo{
			Email:            "acct-a@example.com",
			UserId:           "kiro-user-A",
			SubscriptionType: "POWER",
			UsageLimit:       10000,
			UsageCurrent:     2500,
		}, nil
	}

	// Add with explicit region → single account, identity + usage persisted so the
	// admin sees the real quota immediately.
	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_keyOne","nickname":"cap-1","region":"eu-west-1","enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	addedList, ok := body["added"].([]interface{})
	if !ok || len(addedList) != 1 {
		t.Fatalf("expected 1 added entry, got %v", body["added"])
	}

	var stored *config.Account
	for i, a := range config.GetAccounts() {
		if a.KiroApiKey == "ksk_keyOne" {
			stored = &config.GetAccounts()[i]
			break
		}
	}
	if stored == nil {
		t.Fatal("kiro api key account not persisted")
	}
	// Normalization + identity/usage persisted at add time.
	if !stored.IsApiKeyCredential() || stored.AccessToken != "ksk_keyOne" || stored.ExpiresAt != 0 ||
		stored.Region != "eu-west-1" || stored.Nickname != "cap-1" || stored.Enabled {
		t.Fatalf("account not normalized: %+v", stored)
	}
	if stored.UserId != "kiro-user-A" || stored.Email != "acct-a@example.com" ||
		stored.UsageLimit != 10000 || stored.UsageCurrent != 2500 {
		t.Fatalf("identity/usage not persisted at add: %+v", stored)
	}

	// The KEY DUPLICATE CASE: a DIFFERENT key minted from the SAME Kiro account, same
	// region → must dedupe to the existing account, not create a second pool slot.
	rec = serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_keyTwo_sameAccount","region":"eu-west-1","enabled":false}`, "topsecret"))
	dupBody := decodeBody(t, rec)
	dupAdded := dupBody["added"].([]interface{})
	if len(dupAdded) != 1 || dupAdded[0].(map[string]interface{})["duplicate"] != true ||
		dupAdded[0].(map[string]interface{})["id"] != stored.ID {
		t.Fatalf("expected same-account dedupe, got %v", dupBody)
	}
	accts := 0
	for _, a := range config.GetAccounts() {
		if a.UserId == "kiro-user-A" {
			accts++
		}
	}
	if accts != 1 {
		t.Fatalf("expected 1 pool account for the Kiro identity, got %d", accts)
	}

	// Empty-Region OAuth account for the SAME Kiro user must dedupe against a
	// us-east-1 probe (empty region ≡ us-east-1). Seed one, then probe us-east-1.
	if err := config.AddAccount(config.Account{
		ID: "oauth-a", Email: "acct-a@example.com", UserId: "kiro-user-A", AuthMethod: "social",
		RefreshToken: "rt", AccessToken: "at", Region: "", Enabled: true,
	}); err != nil {
		t.Fatalf("seed oauth account: %v", err)
	}
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		return &config.AccountInfo{Email: "acct-a@example.com", UserId: "kiro-user-A", UsageLimit: 10000}, nil
	}
	rec = serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_keyThree","region":"us-east-1","enabled":false}`, "topsecret"))
	oauthDup := decodeBody(t, rec)["added"].([]interface{})
	if len(oauthDup) != 1 || oauthDup[0].(map[string]interface{})["duplicate"] != true {
		t.Fatalf("expected dedupe against empty-region OAuth account, got %v", oauthDup)
	}

	// Multi-region: the same account served in a NEW region is a distinct slot.
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		return &config.AccountInfo{Email: "acct-a@example.com", UserId: "kiro-user-A", UsageLimit: 10000}, nil
	}
	rec = serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_keyOne","region":"ap-southeast-1","enabled":false}`, "topsecret"))
	multi := decodeBody(t, rec)["added"].([]interface{})
	if len(multi) != 1 || multi[0].(map[string]interface{})["duplicate"] == true {
		t.Fatalf("expected new slot for same account in new region, got %v", multi)
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

// --- POST /admin/add_kiro_account (OAuth supply route) --------------------

// validExternalIdpBody is a complete external_idp credential as emitted by the
// kiro-go-login.py helper, with the fields under test overridable by the caller.
func validExternalIdpBody(extra string) string {
	base := `"accessToken":"at-1","refreshToken":"rt-1","authMethod":"external_idp",` +
		`"provider":"AzureAD","region":"eu-central-1","email":"m365@example.com",` +
		`"profileArn":"arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",` +
		`"clientId":"cid-1","tokenEndpoint":"https://login.microsoftonline.com/t/oauth2/v2.0/token",` +
		`"issuerUrl":"https://login.microsoftonline.com/t/v2.0","scopes":"codewhisperer:conversations offline_access"`
	if extra != "" {
		base += "," + extra
	}
	return `{"account":{` + base + `}}`
}

func TestAdminAddKiroAccountValidation(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// Wrong admin key → 401 (route is behind authenticateAdminKey, like its twin).
	if rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account", validExternalIdpBody(""), "wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong admin key, got %d", rec.Code)
	}

	// Missing account object → 400.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account", `{}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing account, got %d", rec.Code)
	}

	// An API key posted here belongs on the other route → 400, never a half-added slot.
	if rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"kiroApiKey":"ksk_x","accessToken":"ksk_x","authMethod":"api_key"}}`, "topsecret")); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for api_key credential, got %d", rec.Code)
	}

	// Missing accessToken / authMethod / refreshToken → 400 each.
	for _, body := range []string{
		`{"account":{"refreshToken":"rt","authMethod":"external_idp"}}`,
		`{"account":{"accessToken":"at","refreshToken":"rt"}}`,
		`{"account":{"accessToken":"at","authMethod":"social"}}`,
	} {
		if rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account", body, "topsecret")); rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", body, rec.Code)
		}
	}

	// An external_idp account with no refresh material would serve for ~1h and then
	// die permanently — reject at add time instead.
	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at","refreshToken":"rt","authMethod":"external_idp"}}`, "topsecret"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for external_idp without refresh material, got %d", rec.Code)
	}
	if msg, _ := decodeBody(t, rec)["error"].(string); !strings.Contains(msg, "tokenEndpoint") ||
		!strings.Contains(msg, "clientId") || !strings.Contains(msg, "scopes") {
		t.Fatalf("error should name every missing field, got %q", msg)
	}
}

func TestAdminAddKiroAccountProbeFailureIsRejected(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	origProbe := probeKiroAccount
	defer func() { probeKiroAccount = origProbe }()
	probeKiroAccount = func(account *config.Account) (*config.AccountInfo, error) {
		return nil, errTest("HTTP 403: token rejected")
	}

	// A dead credential must never become a pool slot that only surfaces as 403s.
	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account", validExternalIdpBody(""), "topsecret"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for unusable credential, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatalf("failed probe must not persist an account, got %d", len(config.GetAccounts()))
	}
}

func TestAdminAddKiroAccountAddsAndDedupes(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// Stub the upstream probe: the credential resolves to one Kiro identity with a
	// real quota, so dedup can be driven without live network.
	origProbe := probeKiroAccount
	defer func() { probeKiroAccount = origProbe }()
	probeCalls := 0
	probeKiroAccount = func(account *config.Account) (*config.AccountInfo, error) {
		probeCalls++
		return &config.AccountInfo{
			Email:            "m365@example.com",
			UserId:           "kiro-user-M",
			SubscriptionType: "POWER",
			UsageLimit:       10000,
			UsageCurrent:     1500,
		}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at-1","refreshToken":"rt-1","authMethod":"external_idp",`+
			`"provider":"AzureAD","region":"eu-central-1","email":"m365@example.com",`+
			`"profileArn":"arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",`+
			`"clientId":"cid-1","tokenEndpoint":"https://login.microsoftonline.com/t/oauth2/v2.0/token",`+
			`"issuerUrl":"https://login.microsoftonline.com/t/v2.0","scopes":"codewhisperer:conversations offline_access"},`+
			`"nickname":"tg-pool-m365","enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	added, ok := body["added"].([]interface{})
	if !ok || len(added) != 1 {
		t.Fatalf("expected 1 added entry, got %v", body["added"])
	}
	if added[0].(map[string]interface{})["region"] != "eu-central-1" {
		t.Fatalf("expected the profile ARN's region, got %v", added[0])
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 persisted account, got %d", len(accounts))
	}
	stored := accounts[0]
	// Identity + usage persisted at add time, refresh material preserved verbatim.
	if stored.UserId != "kiro-user-M" || stored.Email != "m365@example.com" ||
		stored.UsageLimit != 10000 || stored.UsageCurrent != 1500 {
		t.Fatalf("identity/usage not persisted at add: %+v", stored)
	}
	if stored.AuthMethod != "external_idp" || stored.RefreshToken != "rt-1" ||
		stored.TokenEndpoint == "" || stored.ClientID != "cid-1" || stored.Scopes == "" {
		t.Fatalf("refresh material not preserved: %+v", stored)
	}
	// enabled:false is deliberate (as in the add_kiro_api_key test): an enabled add
	// spawns the background model-warm goroutine, whose real HTTP call latches Go's
	// ProxyFromEnvironment sync.Once and breaks later proxy-env tests in the suite.
	if stored.Nickname != "tg-pool-m365" || stored.Enabled || stored.MachineId == "" || stored.ID == "" {
		t.Fatalf("account not normalized: %+v", stored)
	}

	// THE DUPLICATE CASE: a re-login of the same account yields a fresh token and a
	// fresh UUID. It must dedupe to the existing slot — a second slot would double
	// this one account's routing weight and double-count its quota in /admin/pool.
	rec = serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at-2-relogin","refreshToken":"rt-2","authMethod":"external_idp",`+
			`"provider":"AzureAD","region":"eu-central-1","email":"m365@example.com",`+
			`"profileArn":"arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",`+
			`"clientId":"cid-1","tokenEndpoint":"https://login.microsoftonline.com/t/oauth2/v2.0/token",`+
			`"issuerUrl":"https://login.microsoftonline.com/t/v2.0","scopes":"codewhisperer:conversations offline_access"},`+
			`"enabled":false}`,
		"topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on re-add, got %d: %s", rec.Code, rec.Body.String())
	}
	dup := decodeBody(t, rec)["added"].([]interface{})
	if len(dup) != 1 || dup[0].(map[string]interface{})["duplicate"] != true ||
		dup[0].(map[string]interface{})["id"] != stored.ID {
		t.Fatalf("expected dedupe to the existing slot, got %v", dup)
	}
	if len(config.GetAccounts()) != 1 {
		t.Fatalf("expected still 1 pool account after re-add, got %d", len(config.GetAccounts()))
	}
	if probeCalls != 2 {
		t.Fatalf("expected the re-add to be probed too, got %d probes", probeCalls)
	}
	// The duplicate must ADOPT the fresh tokens, not discard them: an expired
	// refresh token is repaired by re-running the login and re-uploading.
	after := config.GetAccounts()[0]
	if after.AccessToken != "at-2-relogin" || after.RefreshToken != "rt-2" {
		t.Fatalf("re-add did not adopt the fresh credential: %+v", after)
	}
	// A healthy duplicate is not "repaired" — nothing was broken.
	if dup[0].(map[string]interface{})["repaired"] == true {
		t.Fatalf("healthy duplicate should not report repaired")
	}
}

// Re-uploading is the operator's repair for a slot whose OAuth token died (Entra
// refresh tokens expire ~90d, and banAccountInline disables the slot on the
// resulting auth failure). The probe proves the new credential is live, so the
// re-add must adopt it AND bring the slot back — otherwise the obvious fix
// silently no-ops while the endpoint reports "already in pool".
func TestAdminAddKiroAccountRepairsDeadSlot(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	dead := config.Account{
		ID: "dead-1", Email: "m365@example.com", UserId: "kiro-user-M",
		AccessToken: "at-expired", RefreshToken: "rt-expired",
		AuthMethod: "external_idp", Region: "eu-central-1",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",
		ClientID:   "cid-old", TokenEndpoint: "https://idp/old", Scopes: "old",
		Nickname:   "keep-my-nickname",
		Enabled:    false, BanStatus: "BANNED", BanReason: "Authentication failed - token invalid or expired",
		BanTime: 12345,
	}
	if err := config.AddAccount(dead); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	origProbe := probeKiroAccount
	defer func() { probeKiroAccount = origProbe }()
	probeKiroAccount = func(account *config.Account) (*config.AccountInfo, error) {
		return &config.AccountInfo{Email: "m365@example.com", UserId: "kiro-user-M"}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at-fresh","refreshToken":"rt-fresh","authMethod":"external_idp",`+
			`"region":"eu-central-1","email":"m365@example.com",`+
			`"profileArn":"arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",`+
			`"clientId":"cid-new","tokenEndpoint":"https://idp/new","scopes":"new offline_access"},`+
			`"enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	added := decodeBody(t, rec)["added"].([]interface{})[0].(map[string]interface{})
	if added["duplicate"] != true || added["repaired"] != true || added["id"] != "dead-1" {
		t.Fatalf("expected a repaired duplicate of dead-1, got %v", added)
	}

	if len(config.GetAccounts()) != 1 {
		t.Fatalf("repair must not add a slot, got %d", len(config.GetAccounts()))
	}
	got := config.GetAccounts()[0]
	// Fresh credential + refresh material adopted (stale material would fail the
	// next refresh even though the tokens are good).
	if got.AccessToken != "at-fresh" || got.RefreshToken != "rt-fresh" ||
		got.ClientID != "cid-new" || got.TokenEndpoint != "https://idp/new" || got.Scopes != "new offline_access" {
		t.Fatalf("credential/refresh material not adopted: %+v", got)
	}
	// Ban cleared so the slot can serve again.
	if got.BanStatus != "" || got.BanReason != "" || got.BanTime != 0 {
		t.Fatalf("ban not cleared on repair: %+v", got)
	}
	// enabled:false was explicit here, so it must be honored over the revive.
	if got.Enabled {
		t.Fatalf("explicit enabled:false must be honored on repair")
	}
	// Operator-owned fields survive.
	if got.Nickname != "keep-my-nickname" {
		t.Fatalf("repair clobbered the nickname: %+v", got)
	}
}

// An OAuth account carries an empty KiroApiKey. Dedup must key on identity, not on
// that empty string — otherwise the first OAuth account in a region would match
// every later probe and report a bogus duplicate.
func TestAdminAddKiroAccountDoesNotDedupeDistinctAccounts(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	origProbe := probeKiroAccount
	defer func() { probeKiroAccount = origProbe }()
	probeKiroAccount = func(account *config.Account) (*config.AccountInfo, error) {
		// Identity follows the credential, so the two adds are genuinely different.
		return &config.AccountInfo{
			Email:  account.Email,
			UserId: "user-of-" + account.Email,
		}, nil
	}

	for _, email := range []string{"a@example.com", "b@example.com"} {
		rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
			`{"account":{"accessToken":"at","refreshToken":"rt",`+
				`"authMethod":"external_idp","region":"eu-central-1","email":"`+email+`",`+
				`"profileArn":"arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",`+
				`"clientId":"cid","tokenEndpoint":"https://idp/token","scopes":"s"},"enabled":false}`, "topsecret"))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for %s, got %d: %s", email, rec.Code, rec.Body.String())
		}
		if dup := decodeBody(t, rec)["added"].([]interface{})[0].(map[string]interface{})["duplicate"]; dup == true {
			t.Fatalf("%s wrongly reported as a duplicate", email)
		}
	}
	if len(config.GetAccounts()) != 2 {
		t.Fatalf("expected 2 distinct pool accounts, got %d", len(config.GetAccounts()))
	}
}

// Cross-route dedup: the api_key and OAuth supply routes must agree on which
// region bucket an account lives in. An external_idp login's Region field is its
// AUTH region and can differ from the data-plane region pinned by its profileArn,
// so reading Region directly would hide an existing OAuth slot from an api_key
// probe of the SAME Kiro identity — yielding two pool slots for one real account
// (doubled routing weight, doubled quota in /admin/pool). A mixed upload makes
// this reachable.
func TestAdminAddKiroApiKeyDedupesAgainstOAuthAccountByProfileRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// An m365-provisioned account: auth region us-east-1, data-plane eu-central-1.
	oauth := config.Account{
		ID: "oauth-1", Email: "shared@example.com", UserId: "kiro-user-S",
		AccessToken: "at", RefreshToken: "rt", AuthMethod: "external_idp",
		Region:     "us-east-1",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/ABC",
		Enabled:    true,
	}
	if err := config.AddAccount(oauth); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// A ksk_ key for the SAME underlying Kiro account, serving eu-central-1.
	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if region != "eu-central-1" {
			return nil, errTest("HTTP 403: not served in " + region)
		}
		return &config.AccountInfo{Email: "shared@example.com", UserId: "kiro-user-S"}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_sameAccount","region":"eu-central-1","enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	added := decodeBody(t, rec)["added"].([]interface{})
	if len(added) != 1 || added[0].(map[string]interface{})["duplicate"] != true ||
		added[0].(map[string]interface{})["id"] != "oauth-1" {
		t.Fatalf("expected dedupe against the existing OAuth slot, got %v", added)
	}
	if len(config.GetAccounts()) != 1 {
		t.Fatalf("expected 1 pool account for the Kiro identity, got %d", len(config.GetAccounts()))
	}
}

// An api_key slot must never be adopted onto by an OAuth upload of the same Kiro
// identity: the ksk_ credential never expires while an OAuth chain dies with its
// refresh token, so overwriting is a downgrade — and it would leave the slot in
// the kiroApiKey+external_idp state this route rejects on input.
func TestAdminAddKiroAccountDoesNotDowngradeAnApiKeySlot(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	keySlot := config.Account{
		ID: "key-1", Email: "shared@example.com", UserId: "kiro-user-K",
		KiroApiKey: "ksk_forever", AccessToken: "ksk_forever", AuthMethod: "api_key",
		Region: "eu-central-1", Enabled: true,
	}
	if err := config.AddAccount(keySlot); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	origProbe := probeKiroAccount
	defer func() { probeKiroAccount = origProbe }()
	probeKiroAccount = func(account *config.Account) (*config.AccountInfo, error) {
		return &config.AccountInfo{Email: "shared@example.com", UserId: "kiro-user-K"}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at-oauth","refreshToken":"rt-oauth","authMethod":"external_idp",`+
			`"region":"eu-central-1","email":"shared@example.com",`+
			`"profileArn":"arn:aws:codewhisperer:eu-central-1:1:profile/ABC",`+
			`"clientId":"cid","tokenEndpoint":"https://idp/token","scopes":"s"},"enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	added := decodeBody(t, rec)["added"].([]interface{})[0].(map[string]interface{})
	if added["duplicate"] != true || added["id"] != "key-1" || added["repaired"] == true {
		t.Fatalf("expected a plain duplicate of the api_key slot, got %v", added)
	}
	got := config.GetAccounts()[0]
	if got.KiroApiKey != "ksk_forever" || got.AccessToken != "ksk_forever" || !got.IsApiKeyCredential() {
		t.Fatalf("api_key slot was downgraded by an OAuth upload: %+v", got)
	}
	if len(config.GetAccounts()) != 1 {
		t.Fatalf("expected no extra slot, got %d", len(config.GetAccounts()))
	}
}

// idc refresh posts clientId+clientSecret as a PAIR (auth.refreshOIDCToken), and a
// re-login mints a brand-new OIDC client registration. Adopting the new id while
// keeping the old secret would fail every refresh with "invalid client", so the
// documented repair would never converge. Also covers the add-time validation.
func TestAdminAddKiroAccountIdcCarriesClientSecret(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// Validation: an idc account without its clientSecret dies ~1h later — reject now.
	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at","refreshToken":"rt","authMethod":"idc","region":"us-east-1",`+
			`"profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/ABC","clientId":"cid"}}`, "topsecret"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for idc without clientSecret, got %d", rec.Code)
	}
	if msg, _ := decodeBody(t, rec)["error"].(string); !strings.Contains(msg, "clientSecret") {
		t.Fatalf("error should name clientSecret, got %q", msg)
	}

	// A banned idc slot, re-uploaded after a re-login with a NEW client registration.
	dead := config.Account{
		ID: "idc-1", Email: "idc@example.com", UserId: "kiro-user-I",
		AccessToken: "at-old", RefreshToken: "rt-old", AuthMethod: "idc",
		ClientID: "cid-old", ClientSecret: "secret-old", Region: "us-east-1",
		StartUrl:   "https://d-old.awsapps.com/start",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/ABC",
		Enabled:    false, BanStatus: "BANNED", BanReason: "Authentication failed",
	}
	if err := config.AddAccount(dead); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	origProbe := probeKiroAccount
	defer func() { probeKiroAccount = origProbe }()
	probeKiroAccount = func(account *config.Account) (*config.AccountInfo, error) {
		return &config.AccountInfo{Email: "idc@example.com", UserId: "kiro-user-I"}, nil
	}

	rec = serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at-new","refreshToken":"rt-new","authMethod":"idc","region":"us-east-1",`+
			`"profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/ABC",`+
			`"clientId":"cid-new","clientSecret":"secret-new","startUrl":"https://d-new.awsapps.com/start"},`+
			`"enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := config.GetAccounts()[0]
	// The pair must move together — a new id with the old secret is unusable.
	if got.ClientID != "cid-new" || got.ClientSecret != "secret-new" {
		t.Fatalf("idc client registration not adopted as a pair: id=%q secret=%q", got.ClientID, got.ClientSecret)
	}
	if got.StartUrl != "https://d-new.awsapps.com/start" {
		t.Fatalf("startUrl not adopted: %+v", got)
	}
}

// profileArn pins the data-plane region that dedup buckets by. Without it an
// account buckets by its auth region, then silently moves once the request path
// caches a resolved ARN onto it — so a later re-upload would miss it and create a
// second slot for one Kiro account.
func TestAdminAddKiroAccountRequiresProfileArn(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	h := &Handler{pool: accountpool.GetPool()}

	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_account",
		`{"account":{"accessToken":"at","refreshToken":"rt","authMethod":"external_idp",`+
			`"region":"eu-central-1","clientId":"cid","tokenEndpoint":"https://idp/token","scopes":"s"}}`,
		"topsecret"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without profileArn, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg, _ := decodeBody(t, rec)["error"].(string); !strings.Contains(msg, "profileArn") {
		t.Fatalf("error should name profileArn, got %q", msg)
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatalf("nothing should be persisted, got %d", len(config.GetAccounts()))
	}
}

// --- Pool census --------------------------------------------------------

// seedAccount adds a pool account with the given identity/state.
func seedAccount(t *testing.T, id, userID, email, banStatus string, enabled bool) {
	t.Helper()
	if err := config.AddAccount(config.Account{
		ID:           id,
		UserId:       userID,
		Email:        email,
		Enabled:      enabled,
		BanStatus:    banStatus,
		AuthMethod:   "external_idp",
		AccessToken:  "at-" + id,
		RefreshToken: "rt-" + id,
		Region:       "us-east-1",
		UsageLimit:   1000,
		UsageCurrent: 0,
	}); err != nil {
		t.Fatalf("AddAccount(%s): %v", id, err)
	}
}

func poolStates(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	st, ok := body["accountStates"].(map[string]interface{})
	if !ok {
		t.Fatalf("no accountStates in body: %v", body)
	}
	return st
}

// The census must count from the FULL config: the pool only ever holds enabled,
// routable rows, so a banned account is invisible to it and `accounts.total`
// undercounts real inventory.
func TestAdminPoolCensusCountsBannedFromConfig(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")

	seedAccount(t, "acc-live", "user-live", "live@x.com", "ACTIVE", true)
	seedAccount(t, "acc-banned", "user-banned", "banned@x.com", "BANNED", false)
	// SUSPENDED is defensive: no writer emits it today (the failover path writes
	// BANNED with a "suspended" reason), but config documents it as a ban state.
	seedAccount(t, "acc-susp", "user-susp", "susp@x.com", "SUSPENDED", false)
	seedAccount(t, "acc-off", "user-off", "off@x.com", "DISABLED", false)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	rec := serve(h, adminReq(http.MethodGet, "/admin/pool", "", "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	st := poolStates(t, decodeBody(t, rec))
	for _, tc := range []struct {
		field string
		want  float64
	}{
		{"active", 1},
		{"banned", 2}, // BANNED + SUSPENDED
		{"disabled", 1},
		{"total", 4}, // inventory, NOT the pool's enabled-row count
	} {
		if got := st[tc.field].(float64); got != tc.want {
			t.Fatalf("%s: want %v, got %v (states=%v)", tc.field, tc.want, got, st)
		}
	}
	// The pool's own view can't see the banned/disabled stock at all — which is
	// exactly why the census reads config instead.
	acc := decodeBody(t, rec)["accounts"].(map[string]interface{})
	if acc["total"].(float64) != 1 {
		t.Fatalf("pool total should only count enabled rows, got %v", acc["total"])
	}
}

// /admin/pool must never make an upstream call: it is also the customer buy-path
// headroom gate (kiro_go_sellable_credits) with a 5s client timeout, so live I/O
// here would put unbounded latency in front of purchases and fail the cap open.
func TestAdminPoolCensusMakesNoUpstreamCall(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	seedAccount(t, "acc-banned", "user-banned", "banned@x.com", "BANNED", false)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		t.Errorf("/admin/pool must not probe upstream")
		return nil, errTest("unexpected probe")
	}

	done := make(chan int, 1)
	go func() {
		done <- serve(h, adminReq(http.MethodGet, "/admin/pool", "", "topsecret")).Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/admin/pool blocked >2s — it must be a pure read")
	}
}

// An enabled account the pool dropped (quota-blocked) or that is cooling down is
// not dead stock — it must read as cooldown, not active.
func TestAdminPoolCensusCountsCooldown(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	// Enabled but quota-exhausted → Reload drops it → not pool-resident.
	if err := config.AddAccount(config.Account{
		ID: "acc-spent", UserId: "user-spent", Email: "spent@x.com",
		Enabled: true, UsageLimit: 1000, UsageCurrent: 1000,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	seedAccount(t, "acc-live", "user-live", "live@x.com", "ACTIVE", true)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	st := poolStates(t, decodeBody(t, serve(h, adminReq(http.MethodGet, "/admin/pool", "", "topsecret"))))
	if st["cooldown"].(float64) != 1 {
		t.Fatalf("quota-blocked account must read as cooldown, got %v", st)
	}
	if st["active"].(float64) != 1 {
		t.Fatalf("expected active=1, got %v", st)
	}
}

// Multi-region rows are one identity. The census collapses them to their BEST
// state, and the counts must partition the inventory.
func TestAdminPoolCensusDedupesIdentityToBestState(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	// Same Kiro identity, two region rows: one banned, one serving.
	seedAccount(t, "acc-r1", "user-multi", "multi@x.com", "BANNED", false)
	seedAccount(t, "acc-r2", "user-multi", "multi@x.com", "ACTIVE", true)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	st := poolStates(t, decodeBody(t, serve(h, adminReq(http.MethodGet, "/admin/pool", "", "topsecret"))))
	if st["total"].(float64) != 1 {
		t.Fatalf("two rows of one identity must count once, got total=%v", st["total"])
	}
	// It is serving from one region, so it is active — not banned.
	if st["active"].(float64) != 1 || st["banned"].(float64) != 0 {
		t.Fatalf("identity with a live row must read active: %v", st)
	}
	sum := st["active"].(float64) + st["cooldown"].(float64) + st["banned"].(float64) + st["disabled"].(float64)
	if sum != st["total"].(float64) {
		t.Fatalf("census must partition inventory: sum=%v total=%v (%v)", sum, st["total"], st)
	}
}
