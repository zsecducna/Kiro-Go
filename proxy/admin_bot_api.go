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
	"os"
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
	Region     string `json:"region,omitempty"`   // Explicit region (skips probing); omit to auto-probe candidates
	Enabled    *bool  `json:"enabled,omitempty"`  // Whether to route traffic immediately (default true)
}

// handleAdminAddKiroApiKey POST /admin/add_kiro_api_key — add a Kiro API key
// (ksk_) account to the upstream pool. This is the supply-side counterpart to
// /admin/new_api_key (which mints customer keys): the Telegram bot uses it to
// provision the Kiro capacity that backs the keys it sells. Normalizes each
// account exactly like the admin-panel add-account flow (AuthMethod=api_key,
// AccessToken mirrored, ExpiresAt=0 so it is never token-refreshed).
//
// Region behavior: when the caller omits region, the key is probed against each
// candidate data-plane region (us-east-1, eu-central-1) and an account is added
// for EVERY region it serves — a multi-region key yields one pool slot per region.
// An explicit region is trusted without probing. Idempotent per (key, region).
// Response: {success, enabled, added:[{id,region,duplicate?}], skipped:[{region,error}]}.
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

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Determine which regions to add. An explicit region is trusted as-is (operator
	// override, no probe). When omitted, probe each candidate data-plane region
	// (us-east-1, eu-central-1 by default; KIRO_PROFILE_REGIONS overrides) and add an
	// account for EVERY region the key actually serves — a key provisioned in more
	// than one region then gets a pool slot per region for redundancy/throughput.
	var targetRegions []string
	probe := false
	if explicit := strings.TrimSpace(req.Region); explicit != "" {
		targetRegions = []string{explicit}
	} else {
		targetRegions = kiroApiKeyCandidateRegions()
		probe = true
	}

	type addedView struct {
		ID        string `json:"id"`
		Region    string `json:"region"`
		Duplicate bool   `json:"duplicate,omitempty"`
	}
	added := make([]addedView, 0, len(targetRegions))
	skipped := make([]map[string]string, 0)
	var warm []config.Account

	for _, region := range targetRegions {
		// Idempotency is per (key, region): a retry — or a second region on the same
		// key — must not duplicate an existing pool account.
		if existing := findApiKeyAccount(req.KiroApiKey, region); existing != nil {
			added = append(added, addedView{ID: existing.ID, Region: region, Duplicate: true})
			continue
		}
		// Only add regions the key can actually use, so a us-east-1-only key doesn't
		// leave a dead eu-central-1 account that 403s on every request.
		if probe {
			if err := probeKiroApiKeyRegion(req.KiroApiKey, region); err != nil {
				skipped = append(skipped, map[string]string{"region": region, "error": err.Error()})
				continue
			}
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
			skipped = append(skipped, map[string]string{"region": region, "error": err.Error()})
			continue
		}
		added = append(added, addedView{ID: account.ID, Region: region})
		warm = append(warm, account)
	}

	// If probing found no working region (and nothing pre-existed), the key is dead
	// everywhere probed — report it rather than silently succeeding with zero accounts.
	if len(added) == 0 {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "kiroApiKey not usable in any probed region",
			"skipped": skipped,
		})
		return
	}

	h.pool.Reload()
	// Warm the model cache in the background for newly-added live accounts, mirroring
	// apiAddAccount; failures are logged, not fatal.
	if enabled {
		for _, acc := range warm {
			go func(a config.Account) {
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for new Kiro API key account %s: %v", a.ID, err)
				}
			}(acc)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"enabled": enabled,
		"added":   added,
		"skipped": skipped,
	})
}

// kiroApiKeyCandidateRegions is the ordered set of data-plane regions probed when a
// Kiro API key is added without an explicit region. Defaults to us-east-1 then
// eu-central-1; the KIRO_PROFILE_REGIONS env var (comma-separated) overrides it,
// sharing the same knob as OAuth profile-region discovery.
func kiroApiKeyCandidateRegions() []string {
	if env := strings.TrimSpace(os.Getenv("KIRO_PROFILE_REGIONS")); env != "" {
		var out []string
		for _, r := range strings.Split(env, ",") {
			if r = strings.TrimSpace(r); r != "" {
				out = append(out, r)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return defaultKiroProfileRegions
}

// findApiKeyAccount returns a pointer to the existing api_key account matching the
// given key and region, or nil. Used for per-(key,region) idempotency.
func findApiKeyAccount(key, region string) *config.Account {
	for _, a := range config.GetAccounts() {
		if a.KiroApiKey == key && strings.EqualFold(strings.TrimSpace(a.Region), strings.TrimSpace(region)) {
			cp := a
			return &cp
		}
	}
	return nil
}

// probeKiroApiKeyRegion tests whether the key is usable in a specific data-plane
// region by issuing a region-targeted getUsageLimits with it. Returns nil when the
// key serves that region. Uses a throwaway in-memory account (never persisted).
func probeKiroApiKeyRegion(key, region string) error {
	acc := &config.Account{
		KiroApiKey:  key,
		AccessToken: key,
		AuthMethod:  "api_key",
		Region:      region,
	}
	if _, err := GetUsageLimits(acc); err != nil {
		return err
	}
	return nil
}
