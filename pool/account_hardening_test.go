package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// healthPool builds a pool with all health/circuit maps initialised, for
// hardening tests that exercise RecordError/RecordSuccess/RecordLatency.
func healthPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:      make(map[string]time.Time),
		errorCounts:    make(map[string]int),
		modelLists:     make(map[string]map[string]bool),
		circuitState:   make(map[string]*circuitBreaker),
		healthStats:    make(map[string]*accountHealth),
		apiKeyAffinity: make(map[string]apiKeyBinding),
	}
	p.accounts = accounts
	return p
}

// TestErrorRateDecaysOnSuccessNotReset verifies that a burst of errors followed
// by a single success leaves the account's health score still penalised — the
// error history must decay gradually, not reset to a clean slate on one success.
func TestErrorRateDecaysOnSuccessNotReset(t *testing.T) {
	p := healthPool()

	for i := 0; i < 5; i++ {
		p.RecordError("a", false)
	}
	p.RecordSuccess("a")

	score := p.healthScore("a", 1)
	if score >= 1.0 {
		t.Fatalf("expected decayed penalty < 1.0 after 5 errors + 1 success, got %v", score)
	}
	if score <= 0 {
		t.Fatalf("expected a positive (still-selectable) score, got %v", score)
	}
}

// TestCircuitHalfOpenPersistsViaSelection verifies that when the open window has
// elapsed, the *selection* path (not just the affinity path) persists the
// open->half-open transition, so the breaker actually enforces single-probe
// semantics instead of silently un-blocking every request.
func TestCircuitHalfOpenPersistsViaSelection(t *testing.T) {
	p := healthPool(config.Account{ID: "a", Enabled: true})

	cb := &circuitBreaker{
		state:          circuitOpen,
		consecutiveErr: circuitErrorThreshold,
		openedAt:       time.Now().Add(-circuitOpenDuration - time.Second),
	}
	p.circuitState["a"] = cb

	// Selection evaluates 'a': the open window has elapsed, so it should be
	// allowed through as a probe AND its state should be persisted as half-open.
	if acc := p.GetNextForModelExcluding("", nil); acc == nil || acc.ID != "a" {
		t.Fatalf("expected 'a' to be selectable as a probe, got %#v", acc)
	}
	if cb.state != circuitHalfOpen {
		t.Fatalf("expected circuit state half-open (%d) persisted via selection, got %d", circuitHalfOpen, cb.state)
	}
}

// TestRecordLatencyExportedInfluencesScore verifies the now-exported
// RecordLatency feeds the EWMA so a faster account outscores a slower one
// (the signal handlers are expected to supply).
func TestRecordLatencyExportedInfluencesScore(t *testing.T) {
	p := healthPool()
	p.RecordSuccess("fast")
	p.RecordLatency("fast", 50)
	p.RecordSuccess("slow")
	p.RecordLatency("slow", 8000)

	fast := p.healthScore("fast", 1)
	slow := p.healthScore("slow", 1)
	if fast <= slow {
		t.Fatalf("expected faster account to score higher: fast=%v slow=%v", fast, slow)
	}
}

// TestPruneExpiredAffinityRemovesStaleBindings verifies the affinity map is
// bounded: bindings older than the TTL are swept while fresh ones survive.
func TestPruneExpiredAffinityRemovesStaleBindings(t *testing.T) {
	p := healthPool()
	now := time.Now()
	p.apiKeyAffinity["fresh"] = apiKeyBinding{accountID: "a", lastUsed: now}
	p.apiKeyAffinity["stale"] = apiKeyBinding{accountID: "b", lastUsed: now.Add(-2 * sessionAffinityTTL)}

	p.mu.Lock()
	p.pruneExpiredAffinityLocked(now)
	p.mu.Unlock()

	if _, ok := p.apiKeyAffinity["stale"]; ok {
		t.Fatal("expected expired binding to be pruned")
	}
	if _, ok := p.apiKeyAffinity["fresh"]; !ok {
		t.Fatal("expected fresh binding to be retained")
	}
}

// withAffinityConfig initialises config in a temp file and turns session
// affinity on, restoring it to off on cleanup. SetSessionAffinityEnabled
// dereferences the package-global cfg (nil until Init) and persists via Save,
// so a config.Init on a throwaway path is mandatory — see pool/account_test.go
// TestSessionAffinityBindsApiKeyToAccount for the same pattern.
func withAffinityConfig(t *testing.T) {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("SetSessionAffinityEnabled(true): %v", err)
	}
	t.Cleanup(func() { _ = config.SetSessionAffinityEnabled(false) })
}

// TestAffinityRespectsModelSupport verifies session affinity does NOT return a
// bound account that lacks the requested model — it must fall through to normal
// selection instead of routing every request to a guaranteed failure.
func TestAffinityRespectsModelSupport(t *testing.T) {
	withAffinityConfig(t)
	p := healthPool(
		config.Account{ID: "bound", Enabled: true},
		config.Account{ID: "other", Enabled: true},
	)
	p.modelLists["bound"] = map[string]bool{"sonnet": true}
	p.modelLists["other"] = map[string]bool{"opus": true}
	p.apiKeyAffinity["key"] = apiKeyBinding{accountID: "bound", lastUsed: time.Now()}

	if acc := p.GetNextForModelWithApiKey("opus", nil, "key"); acc == nil || acc.ID == "bound" {
		t.Fatalf("affinity must skip the bound account that lacks the model; got %#v", acc)
	}
}

// TestAffinityRespectsQuotaBlock verifies a quota-blocked bound account is skipped.
func TestAffinityRespectsQuotaBlock(t *testing.T) {
	withAffinityConfig(t)
	p := healthPool(
		config.Account{ID: "bound", Enabled: true, UsageCurrent: 100, UsageLimit: 50}, // over limit, overages off
		config.Account{ID: "other", Enabled: true},
	)
	p.apiKeyAffinity["key"] = apiKeyBinding{accountID: "bound", lastUsed: time.Now()}

	if acc := p.GetNextForModelWithApiKey("", nil, "key"); acc == nil || acc.ID == "bound" {
		t.Fatalf("affinity must skip a quota-blocked bound account; got %#v", acc)
	}
}

// TestRecordErrorDoesNotShortenQuotaCooldown verifies a transient (non-quota)
// error reaching errorCounts>=3 does NOT clobber a longer existing quota cooldown
// (+1h) with the short +1min transient backoff. A quota-exhausted account must
// stay backed off for the full hour regardless of subsequent generic errors, or
// it becomes selectable again after one minute and re-hammers the exhausted
// upstream. RecordError must assign cooldowns monotonically (never shorten).
func TestRecordErrorDoesNotShortenQuotaCooldown(t *testing.T) {
	p := healthPool(config.Account{ID: "a", Enabled: true})

	p.RecordError("a", true) // quota → cooldown now + 1h
	quotaExpiry := p.cooldowns["a"]

	p.RecordError("a", false) // errorCounts 2
	p.RecordError("a", false) // errorCounts 3 → transient +1min branch (must NOT shorten the 1h)

	got := p.cooldowns["a"]
	if got.Before(quotaExpiry) {
		t.Fatalf("transient 3-error cooldown shortened the quota cooldown: quota=%v got=%v", quotaExpiry, got)
	}
	// A transient +1min clobber would land ~59min sooner; the real 1h backoff
	// must still be more than 30min out.
	if !got.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("quota cooldown was clobbered to a short transient backoff; got %v", got)
	}
}

// TestRecordSuccessDoesNotWipeQuotaCooldown verifies a late in-flight success
// (a request dispatched before the quota hit, completing after) does NOT wipe
// the protective quota cooldown via RecordSuccess's blanket delete(cooldowns).
// Only transient (3-error, +1min) cooldowns should clear on success; a quota/
// overage backoff must persist to its natural expiry or the exhausted upstream
// gets re-selected immediately.
func TestRecordSuccessDoesNotWipeQuotaCooldown(t *testing.T) {
	p := healthPool(config.Account{ID: "a", Enabled: true})

	p.RecordError("a", true) // quota → +1h hard cooldown
	p.RecordSuccess("a")     // a late in-flight success

	cd, ok := p.cooldowns["a"]
	if !ok {
		t.Fatal("RecordSuccess must not wipe a hard (quota) cooldown on a late in-flight success")
	}
	if !cd.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("quota cooldown wiped/shortened by RecordSuccess; got %v", cd)
	}
}
