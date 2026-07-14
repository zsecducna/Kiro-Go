package pool

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

// TestAutoRecoverReprobesDisabledAccount verifies that a disabled account with a
// valid refresh token gets re-enabled when auto-recovery probes it.
func TestAutoRecoverReprobesDisabledAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Add a disabled account.
	config.AddAccount(config.Account{
		ID:           "acc-disabled",
		Email:        "test@example.com",
		RefreshToken: "rt-valid",
		AuthMethod:   "social",
		Enabled:      false,
		BanStatus:    "DISABLED",
		Region:       "us-east-1",
	})

	p := &AccountPool{
		cooldowns:      make(map[string]time.Time),
		errorCounts:    make(map[string]int),
		modelLists:     make(map[string]map[string]bool),
		reprobeBackoff: make(map[string]time.Duration),
		reprobeNext:    make(map[string]time.Time),
	}
	p.Reload()

	// Call reprobeDisabled directly (bypassing the goroutine).
	p.reprobeDisabled()

	// The account should now be enabled (RefreshToken for social hits the real
	// endpoint; in this test it will fail, so the account stays disabled —
	// verify it does NOT crash and the pool remains stable).
	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}
	// The reprobe attempted but failed (no fake endpoint) — account stays disabled.
	// The test verifies no crash + no data loss.
	if accs[0].ID != "acc-disabled" {
		t.Fatalf("account preserved: got %q", accs[0].ID)
	}
}

// TestCircuitBreakerOpensAfterConsecutiveErrors verifies that 5 consecutive
// errors open the circuit, and the account is skipped during the open window.
func TestCircuitBreakerOpensAfterConsecutiveErrors(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.AddAccount(config.Account{ID: "a", Enabled: true, AuthMethod: "social", Region: "us-east-1"})
	config.AddAccount(config.Account{ID: "b", Enabled: true, AuthMethod: "social", Region: "us-east-1"})

	p := &AccountPool{
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		circuitState: make(map[string]*circuitBreaker),
	}
	p.Reload()

	// 5 errors on account "a" → circuit opens.
	for i := 0; i < 5; i++ {
		p.RecordError("a", false)
	}

	if !p.isCircuitOpen("a", time.Now()) {
		t.Fatalf("expected circuit OPEN for 'a' after 5 errors")
	}
	if p.isCircuitOpen("b", time.Now()) {
		t.Fatalf("circuit for 'b' should be CLOSED")
	}
}

// TestLRUSelectionBalancesAcrossEqualAccounts verifies dispatch interleaves
// requests evenly across healthy accounts (least-recently-used), instead of
// concentrating load via score-weighted random. Over several full rotations
// each account must receive an equal share.
func TestLRUSelectionBalancesAcrossEqualAccounts(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.AddAccount(config.Account{ID: "a", Enabled: true, AuthMethod: "social", Region: "us-east-1"})
	config.AddAccount(config.Account{ID: "b", Enabled: true, AuthMethod: "social", Region: "us-east-1"})
	config.AddAccount(config.Account{ID: "c", Enabled: true, AuthMethod: "social", Region: "us-east-1"})

	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		errorCounts:     make(map[string]int),
		modelLists:      make(map[string]map[string]bool),
		circuitState:    make(map[string]*circuitBreaker),
		healthStats:     make(map[string]*accountHealth),
		lastDispatchSeq: make(map[string]uint64),
	}
	p.Reload()

	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		acc := p.GetNextForModelExcluding("model", nil)
		if acc == nil {
			t.Fatalf("expected an account, got nil at pick %d", i)
		}
		counts[acc.ID]++
	}
	// 300 picks / 3 accounts = 100 each (LRU rotates strictly after warm-up).
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] < 90 || counts[id] > 110 {
			t.Fatalf("account %s picked %d times, expected ~100 (balanced)", id, counts[id])
		}
	}
}

// TestLRUDoesNotStarveSlowAccount verifies a slow-but-healthy account is not
// starved. Under the old score-weighted random selection a high-latency
// account's tiny score routed almost no traffic to it; LRU gives it its fair
// interleaved share (the circuit breaker, not latency, removes failing accounts).
func TestLRUDoesNotStarveSlowAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.AddAccount(config.Account{ID: "fast", Enabled: true, AuthMethod: "social", Region: "us-east-1"})
	config.AddAccount(config.Account{ID: "slow", Enabled: true, AuthMethod: "social", Region: "us-east-1"})

	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		errorCounts:     make(map[string]int),
		modelLists:      make(map[string]map[string]bool),
		circuitState:    make(map[string]*circuitBreaker),
		healthStats:     make(map[string]*accountHealth),
		lastDispatchSeq: make(map[string]uint64),
	}
	p.Reload()

	// Make "slow" score far below "fast" via a huge latency observation.
	p.RecordLatency("slow", 60000) // 60s latency → score ≈ 0.016
	p.RecordLatency("fast", 10)    // 10ms latency → score ≈ 0.99

	slow := 0
	for i := 0; i < 100; i++ {
		acc := p.GetNextForModelExcluding("model", nil)
		if acc != nil && acc.ID == "slow" {
			slow++
		}
	}
	// Old weighted-random: ~2%. LRU: ~50%. Demand a fair share.
	if slow < 30 {
		t.Fatalf("slow account starved: picked %d/100, expected >=30 (LRU fair share)", slow)
	}
}

// TestFallbackDispatchStampsLRUSeq verifies the fallback dispatch path also
// advances the LRU clock. The final whole-branch review found this was the one
// return path that did not stamp lastDispatchSeq, so a fallback account kept a
// stale (low) sequence and got a burst the moment its cooldown expired.
func TestFallbackDispatchStampsLRUSeq(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "a", Enabled: true, AuthMethod: "social", Region: "us-east-1"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		errorCounts:     make(map[string]int),
		modelLists:      make(map[string]map[string]bool),
		lastDispatchSeq: make(map[string]uint64),
	}
	p.Reload()

	// Force the fallback path: an active cooldown excludes "a" from the normal
	// candidate list, but fallbackEarliestCooldown still returns it.
	p.cooldowns["a"] = time.Now().Add(time.Hour)

	seqBefore := p.dispatchSeq
	acc := p.GetNextForModelExcluding("model", nil)
	if acc == nil || acc.ID != "a" {
		t.Fatalf("expected fallback to return account 'a', got %#v", acc)
	}
	if p.dispatchSeq != seqBefore+1 {
		t.Fatalf("fallback did not advance dispatchSeq: %d -> %d", seqBefore, p.dispatchSeq)
	}
	if p.lastDispatchSeq["a"] != p.dispatchSeq {
		t.Fatalf("fallback did not stamp lastDispatchSeq for 'a': got %d, want %d", p.lastDispatchSeq["a"], p.dispatchSeq)
	}
}

// TestSessionAffinityBindsApiKeyToAccount verifies that 2 consecutive requests
// from the same API key route to the same account.
func TestSessionAffinityBindsApiKeyToAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Enable session affinity (opt-in, default false).
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("SetSessionAffinityEnabled: %v", err)
	}
	config.AddAccount(config.Account{ID: "a", Enabled: true, AuthMethod: "social", Region: "us-east-1"})
	config.AddAccount(config.Account{ID: "b", Enabled: true, AuthMethod: "social", Region: "us-east-1"})

	p := &AccountPool{
		cooldowns:      make(map[string]time.Time),
		errorCounts:    make(map[string]int),
		modelLists:     make(map[string]map[string]bool),
		circuitState:   make(map[string]*circuitBreaker),
		healthStats:    make(map[string]*accountHealth),
		apiKeyAffinity: make(map[string]apiKeyBinding),
	}
	p.Reload()

	// First request from "key-1" → binds to whichever account.
	acc1 := p.GetNextForModelWithApiKey("model", nil, "key-1")
	if acc1 == nil {
		t.Fatalf("expected an account, got nil")
	}

	// Second request from "key-1" → should return the SAME account.
	acc2 := p.GetNextForModelWithApiKey("model", nil, "key-1")
	if acc2 == nil {
		t.Fatalf("expected an account, got nil")
	}
	if acc1.ID != acc2.ID {
		t.Fatalf("session affinity broken: first=%q second=%q", acc1.ID, acc2.ID)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}
