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
//	POST /admin/delete_api_key   — delete a customer API key (by id or key value)
//	POST /admin/stats            — per-key usage stats, optionally filtered
//	GET  /admin/pool             — pool credit accounting (available vs sold)
//	POST /admin/add_kiro_api_key — add a Kiro API key (ksk_) account to the pool
//	POST /admin/add_kiro_account — add an OAuth (social/idc/external_idp) account to the pool
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

// adminDeleteApiKeyRequest is the body for POST /admin/delete_api_key. Identify the
// key by its entry id or by its full cleartext value; at least one is required.
type adminDeleteApiKeyRequest struct {
	ID     string `json:"id,omitempty"`     // Key entry UUID
	ApiKey string `json:"apiKey,omitempty"` // Full cleartext key value (sk-...)
}

// handleAdminDeleteApiKey POST /admin/delete_api_key — permanently delete a customer
// API key so it can no longer authenticate. The bot uses this to revoke a sold key
// (refund, abuse, expiry). Accepts the entry id or the cleartext key value; returns
// 404 when no such key exists so a caller can tell "deleted" from "never existed".
func (h *Handler) handleAdminDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	var req adminDeleteApiKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.ApiKey = strings.TrimSpace(req.ApiKey)
	if req.ID == "" && req.ApiKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "id or apiKey is required"})
		return
	}

	// Resolve the target entry id. When only the cleartext key is given, look it up.
	id := req.ID
	if id == "" {
		entry := config.FindApiKeyByValue(req.ApiKey)
		if entry == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
			return
		}
		id = entry.ID
	} else {
		// If both id and apiKey are supplied, they must name the same entry — reject a
		// contradictory pair rather than silently trusting id and deleting the wrong key.
		if req.ApiKey != "" {
			if byValue := config.FindApiKeyByValue(req.ApiKey); byValue == nil || byValue.ID != id {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "id and apiKey refer to different keys"})
				return
			}
		}
		// DeleteApiKey is idempotent (nil on unknown id); check first so we can return
		// an honest 404 instead of a false success.
		if config.GetApiKeyEntry(id) == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
			return
		}
	}

	if err := config.DeleteApiKey(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      id,
	})
}

// adminRechargeApiKeyRequest is the body for POST /admin/recharge_api_key.
// Identify the key by entry id or full cleartext value (at least one required),
// and supply the amount to ADD to its limits. Credits and/or Tokens; at least
// one must be > 0. Amounts are additive top-ups, never absolute limits.
type adminRechargeApiKeyRequest struct {
	ID      string  `json:"id,omitempty"`     // Key entry UUID
	ApiKey  string  `json:"apiKey,omitempty"` // Full cleartext key value (sk-...)
	Credits float64 `json:"credits,omitempty"` // Credits to ADD to the key's credit limit
	Tokens  int64   `json:"tokens,omitempty"`  // Optional tokens to ADD to the token limit
}

// handleAdminRechargeApiKey POST /admin/recharge_api_key — top up an existing
// customer key's quota without changing the key value. The bot calls this when a
// buyer purchases more credits for a key they already own: the credit (and
// optional token) limit is raised by the requested amount and the key is
// re-enabled if the top-up brings it back under quota (the exhausted-then-recharged
// scenario). Usage counters are preserved. Accepts the entry id or the cleartext
// key value; 404 when no such key exists, 400 on a contradictory id+apiKey pair or
// a non-positive top-up.
func (h *Handler) handleAdminRechargeApiKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	var req adminRechargeApiKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.ApiKey = strings.TrimSpace(req.ApiKey)
	if req.ID == "" && req.ApiKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "id or apiKey is required"})
		return
	}
	// Negative amounts would silently reduce a limit (or, for tokens, flip a
	// metered key toward "unlimited" semantics); reject them. At least one amount
	// must be a real top-up or the call is a no-op the caller didn't intend.
	if req.Credits < 0 || req.Tokens < 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "credits and tokens must be >= 0"})
		return
	}
	if req.Credits == 0 && req.Tokens == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "credits or tokens must be > 0"})
		return
	}

	// Resolve the target entry id (same rules as delete): look up by value when
	// only the cleartext key is given; reject a contradictory id+apiKey pair.
	id := req.ID
	if id == "" {
		entry := config.FindApiKeyByValue(req.ApiKey)
		if entry == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
			return
		}
		id = entry.ID
	} else {
		if req.ApiKey != "" {
			if byValue := config.FindApiKeyByValue(req.ApiKey); byValue == nil || byValue.ID != id {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "id and apiKey refer to different keys"})
				return
			}
		}
		if config.GetApiKeyEntry(id) == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
			return
		}
	}

	// Guard against topping up an unlimited limit (0). Adding to it converts the
	// key from unlimited to metered, and if the key already has usage above the
	// added amount it flips to over-limit on the next request — a "recharge" that
	// silently kills an unlimited key. Recharge only makes sense for metered keys.
	if current := config.GetApiKeyEntry(id); current != nil {
		if req.Credits > 0 && current.CreditLimit == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "cannot recharge credits on a key with no credit limit (unlimited)"})
			return
		}
		if req.Tokens > 0 && current.TokenLimit == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "cannot recharge tokens on a key with no token limit (unlimited)"})
			return
		}
	}

	entry, err := config.RechargeApiKey(id, req.Credits, req.Tokens)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":          true,
		"id":               entry.ID,
		"enabled":          entry.Enabled,
		"creditLimit":      entry.CreditLimit,
		"creditsUsed":      entry.CreditsUsed,
		"creditsRemaining": creditsRemaining(entry),
		"tokenLimit":       entry.TokenLimit,
		"tokensUsed":       entry.TokensUsed,
		"tokensRemaining":  tokensRemaining(entry),
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
		// Per-account dispatch/health detail (admin-only — carries account
		// identities). Use it to watch session affinity working: a key's traffic
		// concentrating on one account drives that account's latencyMsEwma down as
		// its upstream cache warms, while spread-out accounts stay higher.
		"sessionAffinity": config.GetSessionAffinityEnabled(),
		"health":          h.pool.HealthSnapshots(),
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
		// email is deliberately "" here: this route's probe reliably returns a
		// UserId, and matching an API key against an OAuth account by email alone
		// would be a behavior change for the pre-existing supply path.
		if existing := findAccountForKiroIdentity(info.UserId, "", req.KiroApiKey, region); existing != nil {
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
// SAME underlying Kiro account in the same region, or nil. Shared by BOTH supply
// routes (add_kiro_api_key and add_kiro_account) so they can never disagree about
// what counts as a duplicate — a disagreement means one real Kiro account gets two
// pool slots, doubling its routing weight and double-counting its quota in
// /admin/pool.
//
// Identity is matched in descending order of reliability:
//   - UserId — the Kiro account identity, stable across both the many API keys one
//     account can mint and a re-login that mints fresh tokens.
//   - email — the only identifier an OAuth probe reliably returns when the upstream
//     omits a UserId. Callers that have no trustworthy email pass "".
//   - key value — for API keys whose probe returned no UserId. Only consulted when
//     non-empty: every OAuth account carries an empty KiroApiKey, so an empty `key`
//     would match the first OAuth account in the region and report a bogus duplicate.
//
// `region` is a DATA-PLANE region, and each candidate is resolved the same way
// (ARN first, then its Region field) rather than by reading Region directly: for an
// OAuth account Region is the auth/OIDC region and can legitimately differ from the
// profile's data-plane region. Reading Region directly here would let the same Kiro
// identity land in two different region buckets depending on which route added it.
// Region is part of the match so a deliberate multi-region slot is not a duplicate.
func findAccountForKiroIdentity(userID, email, key, region string) *config.Account {
	userID = strings.TrimSpace(userID)
	email = strings.TrimSpace(email)
	want := normalizeRegion(region)
	for _, a := range config.GetAccounts() {
		candidate := a
		if !strings.EqualFold(kiroRegionForProfile(&candidate, ""), want) {
			continue
		}
		candidateUser := strings.TrimSpace(a.UserId)
		if userID != "" && candidateUser == userID {
			cp := a
			return &cp
		}
		// Email only decides when it is not contradicted: if BOTH sides know their
		// UserId and they disagree, these are two different Kiro accounts that
		// merely share an address (an alias), and matching them would migrate one
		// slot onto the other's identity.
		if email != "" && strings.EqualFold(strings.TrimSpace(a.Email), email) &&
			!(userID != "" && candidateUser != "" && candidateUser != userID) {
			cp := a
			return &cp
		}
		if key != "" && a.KiroApiKey == key {
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

// adminAddKiroAccountRequest is the body for POST /admin/add_kiro_account.
// Account is a complete OAuth credential as emitted by the login helpers
// (kiro-go-login.py), i.e. the same JSON shape the admin panel's
// POST /admin/api/accounts accepts. Nickname/Enabled mirror the add_kiro_api_key
// request so both supply-side routes read the same way from the bot.
type adminAddKiroAccountRequest struct {
	Account  *config.Account `json:"account"`
	Nickname string          `json:"nickname,omitempty"`
	Enabled  *bool           `json:"enabled,omitempty"`
}

// handleAdminAddKiroAccount POST /admin/add_kiro_account — add an OAuth (social /
// idc / external_idp) Kiro account to the serving pool.
//
// This is the OAuth twin of handleAdminAddKiroApiKey. It exists as a separate
// machine endpoint (rather than reusing the panel's POST /admin/api/accounts)
// for three reasons that matter to an automated supply pipeline:
//
//   - Auth: this route uses authenticateAdminKey (Bearer or X-Admin-Password, no
//     cookie), so the bot's existing Bearer client works and no CSRF-capable
//     cookie path is involved.
//   - Dedup: the panel route de-duplicates on ID only, and the login helper mints
//     a fresh UUID per run — so re-uploading one account would silently create a
//     SECOND pool slot for one real Kiro account, doubling its routing weight and
//     double-counting its quota in /admin/pool. Here we de-dup on the underlying
//     Kiro identity, exactly like the API-key route.
//   - Validation: the panel route persists whatever it is handed. Here the token
//     is probed upstream first, which both proves the credential works AND yields
//     the UserId/Email that make dedup (now and on every later re-upload) possible.
//
// Unlike the API-key route there is no multi-region probing: an OAuth account's
// data-plane region is pinned by its profileArn (see kiroRegionForProfile), so
// exactly one region is ever considered.
func (h *Handler) handleAdminAddKiroAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	var req adminAddKiroAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Account == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "account is required"})
		return
	}
	account := *req.Account

	// Route contract: OAuth credentials only. An API key posted here would be
	// persisted without the region probing / ksk_ validation that route performs,
	// so send the caller to the right endpoint instead of silently half-working.
	account.KiroApiKey = strings.TrimSpace(account.KiroApiKey)
	if account.KiroApiKey != "" || account.IsApiKeyCredential() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "api_key credentials must use /admin/add_kiro_api_key"})
		return
	}

	account.AccessToken = strings.TrimSpace(account.AccessToken)
	if account.AccessToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "accessToken is required"})
		return
	}
	account.AuthMethod = strings.TrimSpace(account.AuthMethod)
	if account.AuthMethod == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "authMethod is required"})
		return
	}

	// An OAuth access token lives ~1h. Without the material to refresh it the
	// account would serve briefly and then fail permanently, which is far more
	// expensive to diagnose later than a 400 now. Each auth method refreshes via a
	// different path (auth.RefreshToken), so each needs different material:
	//   external_idp -> refreshExternalIdpToken(refreshToken, clientId, tokenEndpoint, scopes)
	//   idc          -> refreshOIDCToken(refreshToken, clientId, clientSecret, region)
	//   social       -> refreshSocialToken(refreshToken) — refresh token alone
	if account.RefreshToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}
	var missing []string
	switch {
	case strings.EqualFold(account.AuthMethod, "external_idp"):
		if strings.TrimSpace(account.TokenEndpoint) == "" {
			missing = append(missing, "tokenEndpoint")
		}
		if strings.TrimSpace(account.ClientID) == "" {
			missing = append(missing, "clientId")
		}
		if strings.TrimSpace(account.Scopes) == "" {
			missing = append(missing, "scopes")
		}
	case strings.EqualFold(account.AuthMethod, "idc"):
		// idc refreshes against the AWS SSO OIDC endpoint with the OIDC client
		// registration, which is a clientId/clientSecret PAIR — a missing secret
		// fails every refresh with "invalid client".
		if strings.TrimSpace(account.ClientID) == "" {
			missing = append(missing, "clientId")
		}
		if strings.TrimSpace(account.ClientSecret) == "" {
			missing = append(missing, "clientSecret")
		}
	}
	if len(missing) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": account.AuthMethod + " account is missing refresh material: " + strings.Join(missing, ", "),
		})
		return
	}

	// profileArn is required, not optional. It pins the data-plane region, which is
	// the bucket dedup compares in — an ARN-less account would bucket by its AUTH
	// region now and, once the request path lazily resolves and caches an ARN onto
	// it, silently move to a different bucket, so a later re-upload would miss it
	// and create a second slot for one Kiro account. Requiring it also keeps the
	// probe from having to resolve an ARN itself, which can burn a single-use
	// refresh token on a throwaway copy. Every login helper resolves it eagerly.
	if strings.TrimSpace(account.ProfileArn) == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is required"})
		return
	}

	// Region bucket for dedup + reporting is the DATA-PLANE region (ARN first,
	// then account.Region), matching how the request path resolves it. Note this
	// is deliberately NOT written back to account.Region: for "idc" that field is
	// the auth/OIDC region, which can legitimately differ from the profile's
	// data-plane region, and overwriting it would point token refresh at the wrong
	// OIDC endpoint.
	region := kiroRegionForProfile(&account, "")

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if nickname := strings.TrimSpace(req.Nickname); nickname != "" {
		account.Nickname = nickname
	}

	// Always mint our own ID: a caller-supplied one collides with the existing
	// account on a re-upload of the same file and would fail AddAccount's id check.
	account.ID = auth.GenerateAccountID()
	if strings.TrimSpace(account.MachineId) == "" {
		account.MachineId = config.GenerateMachineId()
	}

	// Probe: proves the credential serves this region and returns the identity we
	// de-dup on. A dead token is rejected here rather than becoming a pool slot
	// that only surfaces later as upstream 403s.
	info, err := probeKiroAccount(&account)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "account credential not usable: " + err.Error(),
			"skipped": []map[string]string{{"region": region, "error": err.Error()}},
		})
		return
	}

	type addedView struct {
		ID        string `json:"id"`
		Region    string `json:"region"`
		Email     string `json:"email,omitempty"`
		Duplicate bool   `json:"duplicate,omitempty"`
		Repaired  bool   `json:"repaired,omitempty"`
	}

	// De-dup on the underlying Kiro identity. Email is the fallback because a
	// social/external_idp probe may return no UserId, and without it a re-upload
	// would silently double the account's routing weight.
	identityEmail := info.Email
	if identityEmail == "" {
		identityEmail = account.Email
	}
	// key is "" — an OAuth credential has no API key to match on.
	if existing := findAccountForKiroIdentity(info.UserId, identityEmail, "", region); existing != nil {
		// An api_key slot is NOT adopted onto. Its ksk_ credential never expires,
		// whereas this OAuth chain dies with its refresh token (~90 days), so
		// overwriting would be a DOWNGRADE — and it would leave the slot in the
		// self-contradictory kiroApiKey + external_idp state this very route
		// rejects at the top. Report the duplicate; keep the stronger credential.
		if existing.KiroApiKey != "" || existing.IsApiKeyCredential() {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"enabled": existing.Enabled,
				"added": []addedView{{ID: existing.ID, Region: region,
					Email: existing.Email, Duplicate: true}},
				"skipped": []map[string]string{},
			})
			return
		}

		// ADOPT the freshly-minted credential onto the existing slot rather than
		// discarding it. An OAuth refresh token expires (Entra defaults to ~90
		// days), and when it does the slot is dead but still in the pool — so
		// re-running the login and re-uploading is the operator's natural repair.
		// Discarding the new tokens here would make that repair silently no-op
		// while reporting "already in pool". The probe above just proved this
		// credential is live, so it is strictly better than what the slot holds.
		// "Repaired" means specifically that a BAN was cleared — the slot was dead
		// and now serves again. A merely-disabled slot is not broken (an operator
		// may have turned it off on purpose), so re-enabling one per the request's
		// `enabled` is not something to report as a repair.
		repaired := existing.BanStatus != "" && existing.BanStatus != "ACTIVE"
		updated := *existing
		updated.AccessToken = account.AccessToken
		updated.RefreshToken = account.RefreshToken
		updated.ExpiresAt = account.ExpiresAt
		updated.AuthMethod = account.AuthMethod
		updated.Region = account.Region
		// Refresh material can legitimately change between logins (a re-login mints
		// a NEW idc OIDC client registration; a tenant may move to a new app
		// registration / scope set), and stale material means the next refresh
		// fails — so adopt it wholesale alongside the tokens. ClientSecret must
		// travel WITH ClientID: idc refresh posts them as a pair, so a new id
		// against a stale secret fails every refresh with "invalid client" and the
		// repair would never converge.
		updated.ClientID = account.ClientID
		updated.ClientSecret = account.ClientSecret
		updated.TokenEndpoint = account.TokenEndpoint
		updated.IssuerURL = account.IssuerURL
		updated.Scopes = account.Scopes
		if account.StartUrl != "" {
			updated.StartUrl = account.StartUrl
		}
		if account.Provider != "" {
			updated.Provider = account.Provider
		}
		if account.ProfileArn != "" {
			updated.ProfileArn = account.ProfileArn
		}
		if nickname := strings.TrimSpace(req.Nickname); nickname != "" {
			updated.Nickname = nickname
		}
		// Clear the ban and restore serving: whatever the slot was banned for, the
		// probe proves this credential works now. Honors an explicit enabled:false.
		updated.Enabled = enabled
		updated.BanStatus = ""
		updated.BanReason = ""
		updated.BanTime = 0
		// MachineId is deliberately NOT regenerated — it is a stable per-account
		// device identity for upstream request tracking, and churning it on every
		// re-upload would make one account look like a fleet of new machines.
		if err := config.UpdateAccount(existing.ID, updated); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if updateErr := config.UpdateAccountInfo(existing.ID, *info); updateErr != nil {
			logger.Warnf("[AddKiroAccount] failed to persist account info for %s: %v", existing.ID, updateErr)
		}
		h.pool.Reload()
		if enabled {
			go func(a config.Account) {
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for re-added Kiro account %s: %v", a.Email, err)
				}
			}(updated)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"enabled": enabled,
			"added": []addedView{{ID: existing.ID, Region: region, Email: updated.Email,
				Duplicate: true, Repaired: repaired}},
			"skipped": []map[string]string{},
		})
		return
	}

	if info.Email != "" {
		account.Email = info.Email
	}
	if info.UserId != "" {
		account.UserId = info.UserId
	}
	account.Enabled = enabled

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// Persist the subscription/usage the probe already fetched so /admin/pool
	// reflects real quota immediately, not only after the next background refresh.
	if updateErr := config.UpdateAccountInfo(account.ID, *info); updateErr != nil {
		logger.Warnf("[AddKiroAccount] failed to persist account info for %s: %v", account.ID, updateErr)
	}

	h.pool.Reload()
	if enabled {
		go func(a config.Account) {
			if err := h.fetchAndCacheAccountModels(&a); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new Kiro account %s: %v", a.Email, err)
			}
		}(account)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"enabled": enabled,
		"added":   []addedView{{ID: account.ID, Region: region, Email: account.Email}},
		"skipped": []map[string]string{},
	})
}

// probeKiroAccount validates an OAuth credential against the upstream and returns
// the underlying Kiro account info (identity + subscription + usage). It probes a
// COPY so a ban/refresh write inside RefreshAccountInfo can never mutate the
// caller's struct, and the copy's ID is one that is not yet in config, so the
// ban path's UpdateAccount is a no-op. Package var so tests can stub the
// upstream round-trip.
var probeKiroAccount = func(account *config.Account) (*config.AccountInfo, error) {
	probe := *account
	info, err := RefreshAccountInfo(&probe)
	if err != nil {
		return nil, err
	}
	return info, nil
}
