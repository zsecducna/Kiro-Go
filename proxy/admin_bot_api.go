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

	// --- Account side. Dedupe by the underlying Kiro account, not the pool row:
	// the same account can appear multiple times — multi-region profiles, or several
	// API keys minted from one Kiro account — and its single quota must be counted
	// ONCE. Key on UserId (the stable Kiro identity), falling back to the account ID
	// when UserId is unknown (behaves as before for un-refreshed accounts).
	accounts := h.pool.GetAllAccounts()
	seen := make(map[string]bool, len(accounts))
	var accountsTotal, accountsUsed, accountsAvailable float64
	usableAccounts := 0
	for _, acc := range accounts {
		// Skip unusable rows BEFORE claiming the identity slot, so a dead row
		// encountered first doesn't shadow a healthy same-account row behind it.
		if !acc.Enabled || strings.EqualFold(acc.BanStatus, "BANNED") || strings.EqualFold(acc.BanStatus, "SUSPENDED") {
			continue
		}
		dedupeKey := strings.TrimSpace(acc.UserId)
		if dedupeKey == "" {
			dedupeKey = acc.ID
		}
		if seen[dedupeKey] {
			continue
		}
		seen[dedupeKey] = true
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
	Region     string `json:"region,omitempty"`   // Explicit region limits the probe set to that one; omit to probe all candidates
	Enabled    *bool  `json:"enabled,omitempty"`  // Whether to route traffic immediately (default true)
}

// handleAdminAddKiroApiKey POST /admin/add_kiro_api_key — add a Kiro API key
// (ksk_) account to the upstream pool. This is the supply-side counterpart to
// /admin/new_api_key (which mints customer keys): the Telegram bot uses it to
// provision the Kiro capacity that backs the keys it sells. Normalizes each
// account exactly like the admin-panel add-account flow (AuthMethod=api_key,
// AccessToken mirrored, ExpiresAt=0 so it is never token-refreshed) and persists
// the fetched subscription/usage so the admin sees real quota immediately.
//
// Region behavior: the key is probed against each candidate data-plane region
// (us-east-1, eu-central-1; an explicit region limits the set to that one) and an
// account is added for EVERY region it serves — a multi-region key yields one pool
// slot per region. Deduplicated per (Kiro account identity, region): one Kiro
// account can mint many keys, so a second key from the same account in the same
// region returns the existing slot instead of doubling its routing weight/quota.
// Response: {success, enabled, added:[{id,region,email,duplicate?}], skipped:[{region,error}]}.
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

	// Determine which regions to consider. An explicit region limits the set to just
	// that one; otherwise use the candidate set (us-east-1, eu-central-1 by default;
	// KIRO_PROFILE_REGIONS overrides). Every region is probed — the probe both
	// validates the key there AND returns the underlying Kiro account identity
	// (UserId/Email), which is what lets us deduplicate.
	var targetRegions []string
	if explicit := strings.TrimSpace(req.Region); explicit != "" {
		targetRegions = []string{explicit}
	} else {
		targetRegions = kiroApiKeyCandidateRegions()
	}

	type addedView struct {
		ID        string `json:"id"`
		Region    string `json:"region"`
		Email     string `json:"email,omitempty"`
		Duplicate bool   `json:"duplicate,omitempty"`
	}
	added := make([]addedView, 0, len(targetRegions))
	skipped := make([]map[string]string, 0)
	var warm []config.Account

	for _, region := range targetRegions {
		// Probe: validates the key in this region and returns the account identity.
		info, err := probeKiroApiKey(req.KiroApiKey, region)
		if err != nil {
			skipped = append(skipped, map[string]string{"region": region, "error": err.Error()})
			continue
		}

		// Deduplicate on the UNDERLYING Kiro account, not the key string: one Kiro
		// account can mint many API keys, and adding two of them (or an api_key next
		// to an existing OAuth login for the same account) would double that single
		// account's routing weight and double-count its quota in /admin/pool. Match by
		// UserId within the same region; a different region for the same account is a
		// deliberate multi-region slot and is allowed. Fall back to the key value when
		// the upstream did not return a UserId.
		if existing := findAccountForKiroIdentity(info.UserId, req.KiroApiKey, region); existing != nil {
			added = append(added, addedView{ID: existing.ID, Region: region, Email: existing.Email, Duplicate: true})
			continue
		}

		account := config.Account{
			ID:          auth.GenerateAccountID(),
			Email:       info.Email,
			UserId:      info.UserId,
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
		// Persist the subscription/usage the probe already fetched so /admin/pool
		// reflects this account's real quota immediately (not only after the next
		// background refresh).
		if updateErr := config.UpdateAccountInfo(account.ID, *info); updateErr != nil {
			logger.Warnf("[AddKiroApiKey] failed to persist account info for %s: %v", account.ID, updateErr)
		}
		added = append(added, addedView{ID: account.ID, Region: region, Email: info.Email})
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

// normalizeRegion trims and treats an empty region as us-east-1, matching how the
// rest of the code resolves the effective data-plane region.
func normalizeRegion(region string) string {
	if r := strings.TrimSpace(region); r != "" {
		return r
	}
	return "us-east-1"
}

// findAccountForKiroIdentity returns an existing pool account that represents the
// SAME underlying Kiro account in the same region, or nil. It matches on UserId
// (the Kiro account identity — stable across the multiple API keys one account can
// mint) and falls back to the key value when UserId is unknown. Region is part of
// the match so a deliberate multi-region slot for the same account is not treated
// as a duplicate.
func findAccountForKiroIdentity(userID, key, region string) *config.Account {
	userID = strings.TrimSpace(userID)
	want := normalizeRegion(region)
	for _, a := range config.GetAccounts() {
		// Empty Region means us-east-1 everywhere else in the codebase, so normalize
		// both sides — otherwise an OAuth account with Region:"" wouldn't dedupe
		// against a us-east-1 probe of the same Kiro account.
		if !strings.EqualFold(normalizeRegion(a.Region), want) {
			continue
		}
		if userID != "" && strings.TrimSpace(a.UserId) == userID {
			cp := a
			return &cp
		}
		if a.KiroApiKey == key {
			cp := a
			return &cp
		}
	}
	return nil
}

// probeKiroApiKey validates the key in a specific data-plane region and returns the
// underlying Kiro account info (identity + subscription + usage) fetched in the same
// call. Uses a throwaway in-memory account (never persisted). A non-nil error means
// the key does not serve that region. It is a package var so tests can stub the
// upstream round-trip.
var probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
	acc := &config.Account{
		KiroApiKey:  key,
		AccessToken: key,
		AuthMethod:  "api_key",
		Region:      region,
	}
	info, err := RefreshAccountInfo(acc)
	if err != nil {
		return nil, err
	}
	return info, nil
}
