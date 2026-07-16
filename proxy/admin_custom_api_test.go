package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// newAddCustomApiHandler sets up an isolated config store + pool for add-endpoint tests.
func newAddCustomApiHandler(t *testing.T) *Handler {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p}
}

func postAddCustomApi(t *testing.T, h *Handler, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/admin/add_custom_api_account", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.handleAdminAddCustomApiAccount(rec, req)
	return rec
}

func TestAddCustomApiAccountSuccess(t *testing.T) {
	orig := probeCustomApiQuota
	probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
		return &customApiQuota{CreditsRemaining: 10, CreditLimit: 100, OK: true}, nil
	}
	defer func() { probeCustomApiQuota = orig }()

	h := newAddCustomApiHandler(t)
	rec := postAddCustomApi(t, h, map[string]interface{}{
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
	orig := probeCustomApiQuota
	probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
		return &customApiQuota{OK: false}, http.ErrHandlerTimeout
	}
	defer func() { probeCustomApiQuota = orig }()

	h := newAddCustomApiHandler(t)
	rec := postAddCustomApi(t, h, map[string]interface{}{
		"baseUrl": "https://pool.example.com", "apiKey": "bad", "orderId": "ORD-2",
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 for bad quota, got 200")
	}
}

func TestAddCustomApiRejectDuplicateOrderID(t *testing.T) {
	orig := probeCustomApiQuota
	probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
		return &customApiQuota{CreditsRemaining: 10, CreditLimit: 100, OK: true}, nil
	}
	defer func() { probeCustomApiQuota = orig }()
	h := newAddCustomApiHandler(t)
	postAddCustomApi(t, h, map[string]interface{}{"baseUrl": "https://a.com", "apiKey": "k1", "orderId": "DUP"})
	rec := postAddCustomApi(t, h, map[string]interface{}{"baseUrl": "https://b.com", "apiKey": "k2", "orderId": "DUP"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate orderId, got %d", rec.Code)
	}
}

func TestAddCustomApiRejectMissingOrderID(t *testing.T) {
	h := newAddCustomApiHandler(t)
	rec := postAddCustomApi(t, h, map[string]interface{}{"baseUrl": "https://a.com", "apiKey": "k1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing orderId, got %d", rec.Code)
	}
}
