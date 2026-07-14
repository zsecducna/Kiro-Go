package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

// Machine-friendly "/admin/*" endpoints for external integrations (e.g. the
// Telegram sales bot). These are authenticated with the ADMIN key (the same
// password that guards the /admin panel), never with customer API keys:
//
//	POST /admin/new_api_key      — mint a customer API key with a credit quota
//	POST /admin/stats            — per-key usage stats, optionally filtered
//	GET  /admin/pool             — pool credit accounting (available vs sold)
//	POST /admin/add_kiro_api_key — add a Kiro API key (ksk_) account to the pool
//
// The admin key is read from "Authorization: Bearer <key>" or the
// "X-Admin-Password" header. Deliberately NO cookie fallback: these are
// state-changing machine endpoints, and honoring the admin panel's
// "admin_password" cookie would open a CSRF vector (a cross-site POST from a
// logged-in admin's browser could mint keys). Bots send explicit headers.

// authenticateAdminKey verifies the caller holds the admin key. Writes a JSON
// 401 and returns false on failure. Fails closed when no admin password is
// configured, so a fresh deployment can't expose key-minting unauthenticated.
func (h *Handler) authenticateAdminKey(w http.ResponseWriter, r *http.Request) bool {
	provided := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		provided = strings.TrimPrefix(auth, "Bearer ")
	}
	if provided == "" {
		provided = r.Header.Get("X-Admin-Password")
	}

	expected := config.GetPassword()
	// Constant-time compare; also rejects the empty-password case because an
	// empty `provided` never reaches here with a non-empty expected value.
	if expected == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return false
	}
	return true
}

// adminNewApiKeyRequest is the body for POST /admin/new_api_key.
// Credits is the credit quota to sell (must be > 0 — this endpoint exists to
// mint metered keys; unlimited keys can still be created via the admin panel).
type adminNewApiKeyRequest struct {
	Name       string  `json:"name,omitempty"`       // Optional label, e.g. Telegram order ID
	Credits    float64 `json:"credits"`              // Credit quota for the key
	TokenLimit int64   `json:"tokenLimit,omitempty"` // Optional extra token cap (0 = none)
}

// handleAdminNewApiKey POST /admin/new_api_key — generates a fresh API key with
// the requested credit quota. The cleartext key is returned exactly once in the
// response; afterwards only the masked form is ever exposed. Once the key's
// CreditsUsed reaches the quota it is automatically deactivated (see
// config.RecordApiKeyUsage).
func (h *Handler) handleAdminNewApiKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	var req adminNewApiKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Credits <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "credits must be > 0"})
		return
	}
	// A negative token limit would silently mean "unlimited" (ApiKeyOverLimit
	// only checks > 0), which is never what a metered-key caller intends.
	if req.TokenLimit < 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "tokenLimit must be >= 0"})
		return
	}

	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Name:        req.Name,
		Key:         config.GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: req.Credits,
		TokenLimit:  req.TokenLimit,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      entry.ID,
		"key":     entry.Key, // cleartext, shown once — the bot forwards this to the buyer
		"name":    entry.Name,
		"credits": entry.CreditLimit,
	})
}

// adminStatsRequest is the body for POST /admin/stats. All filters are
// optional and AND-ed together; an empty body returns every key.
type adminStatsRequest struct {
	ApiKey string `json:"apiKey,omitempty"` // Full cleartext key value to look up
	ID     string `json:"id,omitempty"`     // Key entry UUID
	Name   string `json:"name,omitempty"`   // Exact label match
}

// adminKeyStatsView extends the panel's apiKeyView with the derived status and
// remaining-quota fields the bot shows to buyers.
type adminKeyStatsView struct {
	apiKeyView
	Status           string  `json:"status"` // active | exhausted | disabled
	CreditsRemaining float64 `json:"creditsRemaining"`
	TokensRemaining  int64   `json:"tokensRemaining"`
}

// handleAdminBotStats POST /admin/stats — usage stats for all API keys, or a
// subset when a filter is supplied. Keys are masked; use /admin/new_api_key's
// response if the cleartext is needed at mint time.
func (h *Handler) handleAdminBotStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// An empty body is allowed (means "no filter"), but malformed JSON is a
	// hard 400: silently ignoring a broken filter would return EVERY key to a
	// caller who thought they were querying one.
	var req adminStatsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	entries := config.ListApiKeys()
	out := make([]adminKeyStatsView, 0, len(entries))
	var totalTokens int64
	var totalCredits float64
	var totalRequests int64
	for _, e := range entries {
		// Apply AND-ed filters; skip entries that fail any provided filter.
		if req.ApiKey != "" && e.Key != req.ApiKey {
			continue
		}
		if req.ID != "" && e.ID != req.ID {
			continue
		}
		if req.Name != "" && e.Name != req.Name {
			continue
		}
		out = append(out, adminKeyStatsView{
			apiKeyView:       toApiKeyView(e),
			Status:           customerKeyStatus(e),
			CreditsRemaining: creditsRemaining(e),
			TokensRemaining:  tokensRemaining(e),
		})
		totalTokens += e.TokensUsed
		totalCredits += e.CreditsUsed
		totalRequests += e.RequestsCount
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKeys": out,
		"totals": map[string]interface{}{
			"keys":          len(out),
			"tokensUsed":    totalTokens,
			"creditsUsed":   totalCredits,
			"requestsCount": totalRequests,
		},
	})
}

// handleAdminPool GET /admin/pool — credit accounting across the Kiro account
// pool vs sold API key quota. Answers "how many more credits can I sell?":
//
//	accountsAvailableCredits = Σ max(0, UsageLimit - UsageCurrent) over usable accounts
//	soldOutstandingCredits   = Σ max(0, CreditLimit - CreditsUsed) over enabled metered keys
//	sellableCredits          = accountsAvailableCredits - soldOutstandingCredits
//
// The two sides stay consistent as customers consume: each request raises the
// account's UsageCurrent and the key's CreditsUsed by the same credits, so both
// terms shrink together and sellableCredits only moves when accounts are
// added/reset or keys are sold/expired.
func (h *Handler) handleAdminPool(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// --- Account side. The pool slice can contain duplicate entries for the
	// same account ID (multi-region profiles), so dedupe before summing.
	accounts := h.pool.GetAllAccounts()
	seen := make(map[string]bool, len(accounts))
	var accountsTotal, accountsUsed, accountsAvailable float64
	usableAccounts := 0
	for _, acc := range accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		// Banned/suspended or manually disabled accounts can't serve traffic,
		// so their remaining credits must not be counted as sellable.
		if !acc.Enabled || strings.EqualFold(acc.BanStatus, "BANNED") || strings.EqualFold(acc.BanStatus, "SUSPENDED") {
			continue
		}
		usableAccounts++
		accountsTotal += acc.UsageLimit
		accountsUsed += acc.UsageCurrent
		if rem := acc.UsageLimit - acc.UsageCurrent; rem > 0 {
			accountsAvailable += rem
		}
	}

	// --- Sold side. Only enabled keys hold outstanding quota: exhausted keys
	// are auto-disabled with ~0 remaining, and manually disabled keys can't
	// spend, so neither reserves pool credits.
	keys := config.ListApiKeys()
	var soldQuota, soldUsed, soldOutstanding float64
	activeKeys := 0
	unlimitedKeys := 0
	for _, k := range keys {
		if !k.Enabled {
			continue
		}
		if k.CreditLimit <= 0 {
			// Unlimited keys make outstanding-quota math meaningless; report
			// their count so the operator knows the estimate is optimistic.
			unlimitedKeys++
			continue
		}
		activeKeys++
		soldQuota += k.CreditLimit
		soldUsed += k.CreditsUsed
		if rem := k.CreditLimit - k.CreditsUsed; rem > 0 {
			soldOutstanding += rem
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"time":   time.Now().Unix(),
		"accounts": map[string]interface{}{
			"usable":           usableAccounts,
			"total":            h.pool.Count(),
			"creditsLimit":     accountsTotal,
			"creditsUsed":      accountsUsed,
			"creditsAvailable": accountsAvailable,
		},
		"sold": map[string]interface{}{
			"activeKeys":         activeKeys,
			"unlimitedKeys":      unlimitedKeys,
			"quotaCredits":       soldQuota,
			"usedCredits":        soldUsed,
			"outstandingCredits": soldOutstanding,
		},
		// Headline number: credits still available to sell as new API keys.
		"sellableCredits": accountsAvailable - soldOutstanding,
	})
}

// adminAddKiroApiKeyRequest is the body for POST /admin/add_kiro_api_key.
type adminAddKiroApiKeyRequest struct {
	KiroApiKey string `json:"kiroApiKey"`         // The Kiro API key (ksk_...); required
	Nickname   string `json:"nickname,omitempty"` // Optional label for the admin panel
	Region     string `json:"region,omitempty"`   // AWS region (default us-east-1)
	Enabled    *bool  `json:"enabled,omitempty"`  // Whether to route traffic immediately (default true)
}

// handleAdminAddKiroApiKey POST /admin/add_kiro_api_key — add a Kiro API key
// (ksk_) account to the upstream pool. This is the supply-side counterpart to
// /admin/new_api_key (which mints customer keys): the Telegram bot uses it to
// provision the Kiro capacity that backs the keys it sells. Normalizes the
// account exactly like the admin-panel add-account flow (AuthMethod=api_key,
// AccessToken mirrored, ExpiresAt=0 so it is never token-refreshed).
func (h *Handler) handleAdminAddKiroApiKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	var req adminAddKiroApiKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.KiroApiKey = strings.TrimSpace(req.KiroApiKey)
	if req.KiroApiKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
		return
	}
	// The route contract is Kiro API keys; a strict prefix check protects against a
	// bot bug posting an order ID or OAuth blob, which would otherwise become a
	// permanently-broken pool account that only surfaces as upstream 403s.
	if !strings.HasPrefix(req.KiroApiKey, "ksk_") {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey must start with ksk_"})
		return
	}

	// Idempotency: the caller is an automated bot whose normal failure mode is a
	// timeout + retry. Deduplicate on the key value so a retry doesn't create a
	// second pool account backed by the same upstream key (which would double its
	// routing weight and burn rate). Return the existing account's ID instead.
	for _, existing := range config.GetAccounts() {
		if existing.KiroApiKey == req.KiroApiKey {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":   true,
				"id":        existing.ID,
				"enabled":   existing.Enabled,
				"duplicate": true,
			})
			return
		}
	}

	region := strings.TrimSpace(req.Region)
	if region == "" {
		region = "us-east-1"
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	account := config.Account{
		ID:          auth.GenerateAccountID(),
		Nickname:    strings.TrimSpace(req.Nickname),
		KiroApiKey:  req.KiroApiKey,
		AccessToken: req.KiroApiKey, // mirror for pool compatibility (see apiAddAccount)
		AuthMethod:  "api_key",
		Region:      region,
		ExpiresAt:   0, // never refreshed
		Enabled:     enabled,
		MachineId:   config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// Warm the model cache in the background when the account is live, mirroring
	// apiAddAccount; failures are logged, not fatal to the add.
	if enabled {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new Kiro API key account %s: %v", acc.ID, err)
			}
		}(account)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      account.ID,
		"enabled": enabled,
	})
}
