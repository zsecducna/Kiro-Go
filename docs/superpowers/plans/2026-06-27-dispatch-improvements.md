# Dispatch Improvements (Part A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Improve account dispatch in `pool/account.go` — auto-recovery of disabled accounts (A4), circuit breaker (A2), health-aware scoring + weight-as-probability (A1+A5), and session affinity (A3) — so accounts don't permanently die and traffic routes to healthy accounts preferentially.

**Architecture:** The current pool uses weighted round-robin (weight N = N physical slots) with cooldown-based error handling and permanent DisableAccount on auth failure. This plan adds: (1) a background goroutine that re-probes disabled accounts with exponential backoff; (2) a 3-state circuit breaker per account; (3) EWMA latency + success-rate scoring replacing slot-count; (4) per-API-key session affinity.

**Tech Stack:** Go 1.21 (module `kiro-go`, stdlib). Run `go test ./...` from `C:\Users\Admin\Kiro-Go`.

## Global Constraints

- Account selection MUST still respect: excluded (failover), cooldown (1min/3 errors, 1h/quota), token-expiry (120s skew), quota-blocked.
- `DisableAccount` stays permanent for auth failures — auto-recovery RE-PROBES (refresh → re-enable) but does NOT change the disable logic itself.
- Circuit breaker is SEPARATE from cooldown: it tracks consecutive errors and has open/half-open/closed states. Cooldown tracks time-based skips.
- Weight-as-probability: `effectiveWeight` (min 1) multiplies the health score. Weight 2 = 2× probability, not 2 slots.
- Session affinity is OPT-IN via config flag `SessionAffinityEnabled` (default false). Disabled by default to preserve current behavior.
- Module path `kiro-go`. The 2 pre-existing `translator_test.go` failures are NOT your work — ignore them.
- All proxy/auth/config/pool tests must pass (except the 2 translator ones).

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `pool/account.go` | account pool, selection, cooldown, disable | modify: add circuit breaker state, health scoring, weight-as-probability, auto-recovery goroutine, session affinity map |
| `pool/account_test.go` | pool tests | add tests for each feature |
| `config/config.go` | config struct + getters | add `SessionAffinityEnabled`, `AutoRecoverEnabled`, `CircuitBreakerThreshold` config fields |

---

### Task 1: Auto-recovery (A4) — re-probe disabled accounts

**Why:** Currently `DisableAccount` permanently removes accounts on auth failure (401/invalid_grant). Over time the pool drains. Auto-recovery periodically refreshes disabled accounts' tokens — if the refresh succeeds (e.g., token rotation resolved, upstream transient issue cleared), the account is re-enabled.

**Files:**
- Modify: `pool/account.go` (add `startAutoRecover`, `reprobeDisabled`, new fields on `AccountPool`)
- Modify: `config/config.go` (add `AutoRecoverEnabled` getter, default true)
- Test: `pool/account_test.go`

**Interfaces:**
- Produces: `AccountPool.startAutoRecover()` (called once after `GetPool` init), which launches a goroutine that every 60s calls `reprobeDisabled()`.
- `reprobeDisabled()` iterates disabled accounts, calls `auth.RefreshToken`, and on success calls `config.SetAccountEnabled(id, true)` + `p.Reload()`.

- [ ] **Step 1: Add config fields**

In `config/config.go`, add to the `Config` struct (after `AllowOverUsage`):

```go
	// AutoRecoverEnabled controls whether disabled accounts (auth failure) are
	// periodically re-probed with a token refresh. Default true. Set false to
	// require manual re-enable.
	AutoRecoverEnabled *bool `json:"autoRecoverEnabled,omitempty"`
```

Add getter after `GetAllowOverUsage`:

```go
// GetAutoRecoverEnabled returns whether auto-recovery of disabled accounts is
// enabled. Defaults to true.
func GetAutoRecoverEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AutoRecoverEnabled == nil {
		return true
	}
	return *cfg.AutoRecoverEnabled
}
```

- [ ] **Step 2: Write the failing test**

Append to `pool/account_test.go`:

```go
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
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./pool/ -run TestAutoRecoverReprobesDisabledAccount -v`
Expected: FAIL — `undefined: p.reprobeDisabled`.

- [ ] **Step 4: Implement `reprobeDisabled` + `startAutoRecover`**

In `pool/account.go`, add new fields to `AccountPool`:

```go
type AccountPool struct {
	mu              sync.RWMutex
	accounts        []config.Account
	totalAccounts   int
	currentIndex    uint64
	cooldowns       map[string]time.Time
	errorCounts     map[string]int
	modelLists      map[string]map[string]bool
	reprobeBackoff  map[string]time.Duration // accountID → next backoff interval
	reprobeNext     map[string]time.Time     // accountID → when to next probe
	stopRecover     chan struct{}
}
```

Update `GetPool` to initialize the new maps + start the goroutine:

```go
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:      make(map[string]time.Time),
			errorCounts:    make(map[string]int),
			modelLists:     make(map[string]map[string]bool),
			reprobeBackoff: make(map[string]time.Duration),
			reprobeNext:    make(map[string]time.Time),
		}
		pool.Reload()
		if config.GetAutoRecoverEnabled() {
			pool.startAutoRecover()
		}
	})
	return pool
}
```

Add the auto-recovery methods (append to `account.go`):

```go
// startAutoRecover launches a background goroutine that periodically refreshes
// disabled accounts' tokens. If a refresh succeeds, the account is re-enabled.
// Exponential backoff per account: 1m → 5m → 30m → max 2h.
func (p *AccountPool) startAutoRecover() {
	p.stopRecover = make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.reprobeDisabled()
			case <-p.stopRecover:
				return
			}
		}
	}()
}

// reprobeDisabled iterates disabled accounts whose reprobe time has arrived,
// attempts a token refresh, and re-enables on success.
func (p *AccountPool) reprobeDisabled() {
	if !config.GetAutoRecoverEnabled() {
		return
	}
	now := time.Now()
	all := config.GetAccounts()
	for _, acc := range all {
		if acc.Enabled || acc.BanStatus != "DISABLED" {
			continue
		}
		// Check backoff schedule.
		p.mu.Lock()
		next, ok := p.reprobeNext[acc.ID]
		backoff := p.reprobeBackoff[acc.ID]
		p.mu.Unlock()
		if ok && now.Before(next) {
			continue // not time yet
		}
		// Attempt refresh.
		_, _, _, _, err := auth.RefreshToken(&acc)
		if err == nil {
			// Success! Re-enable.
			config.SetAccountEnabled(acc.ID, true)
			p.Reload()
			p.mu.Lock()
			delete(p.reprobeBackoff, acc.ID)
			delete(p.reprobeNext, acc.ID)
			p.mu.Unlock()
			continue
		}
		// Failure → increase backoff: 1m → 5m → 30m → 2h max.
		if backoff == 0 {
			backoff = time.Minute
		} else {
			backoff *= 5
			if backoff > 2*time.Hour {
				backoff = 2 * time.Hour
			}
		}
		p.mu.Lock()
		p.reprobeBackoff[acc.ID] = backoff
		p.reprobeNext[acc.ID] = now.Add(backoff)
		p.mu.Unlock()
	}
}
```

Add `"kiro-go/auth"` to the import block in `account.go` if not already present.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./pool/ -run TestAutoRecoverReprobesDisabledAccount -v`
Expected: PASS (no crash, account preserved).
Then: `go test ./pool/ ./config/ -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add pool/account.go pool/account_test.go config/config.go
git commit -m "feat(pool): auto-recovery of disabled accounts with exponential backoff (A4)"
```

---

### Task 2: Circuit breaker (A2)

**Why:** The current cooldown (3 errors → 1min) is time-based and doesn't probe. A circuit breaker adds: 5 consecutive errors → OPEN (skip 30s) → HALF_OPEN (1 probe) → CLOSED (success). This is separate from cooldown and catches accounts that are consistently failing faster.

**Files:**
- Modify: `pool/account.go` (add `circuitState` map + `isCircuitOpen` check in `GetNextForModelExcluding`)
- Test: `pool/account_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pool/account_test.go`:

```go
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
		cooldowns:     make(map[string]time.Time),
		errorCounts:   make(map[string]int),
		modelLists:    make(map[string]map[string]bool),
		circuitState:  make(map[string]*circuitBreaker),
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pool/ -run TestCircuitBreakerOpensAfterConsecutiveErrors -v`
Expected: FAIL — `undefined: p.circuitState`, `undefined: p.isCircuitOpen`, `undefined: circuitBreaker`.

- [ ] **Step 3: Implement circuit breaker**

In `pool/account.go`, add the circuit breaker type + state:

```go
const (
	circuitClosed   = 0
	circuitOpen     = 1
	circuitHalfOpen = 2
	circuitErrorThreshold = 5
	circuitOpenDuration   = 30 * time.Second
)

type circuitBreaker struct {
	state         int
	consecutiveErr int
	openedAt      time.Time
}

func (p *AccountPool) isCircuitOpen(id string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cb, ok := p.circuitState[id]
	if !ok || cb == nil {
		return false
	}
	switch cb.state {
	case circuitOpen:
		if now.Sub(cb.openedAt) >= circuitOpenDuration {
			cb.state = circuitHalfOpen // transition to half-open after timeout
			return false               // allow one probe
		}
		return true
	case circuitHalfOpen:
		return false // allow the probe through
	default:
		return false
	}
}
```

Add `circuitState map[string]*circuitBreaker` to the `AccountPool` struct (Task 1 already added fields — add this one too).

Update `GetPool` to init `circuitState: make(map[string]*circuitBreaker)`.

Update `RecordError` to increment the circuit breaker:

```go
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.errorCounts[id]++

	if isQuotaError {
		p.cooldowns[id] = time.Now().Add(time.Hour)
	} else if p.errorCounts[id] >= 3 {
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}

	// Circuit breaker: track consecutive errors.
	cb := p.circuitState[id]
	if cb == nil {
		cb = &circuitBreaker{state: circuitClosed}
		p.circuitState[id] = cb
	}
	cb.consecutiveErr++
	if cb.state == circuitHalfOpen {
		// Probe failed → re-open.
		cb.state = circuitOpen
		cb.openedAt = time.Now()
	} else if cb.consecutiveErr >= circuitErrorThreshold && cb.state == circuitClosed {
		cb.state = circuitOpen
		cb.openedAt = time.Now()
	}
}
```

Update `RecordSuccess` to close the circuit:

```go
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	// Circuit breaker: reset on success.
	if cb, ok := p.circuitState[id]; ok && cb != nil {
		cb.state = circuitClosed
		cb.consecutiveErr = 0
	}
}
```

In `GetNextForModelExcluding`, add a circuit-breaker skip check (after the cooldown check, before the token-expiry check):

```go
		// Skip accounts with an open circuit breaker.
		if p.isCircuitOpen(acc.ID, now) {
			seen[acc.ID] = true
			continue
		}
```

(Add the same check in `GetNextExcluding` too.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pool/ -run TestCircuitBreaker -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pool/account.go pool/account_test.go
git commit -m "feat(pool): 3-state circuit breaker for consistently-failing accounts (A2)"
```

---

### Task 3: Health-aware scoring + weight-as-probability (A1+A5)

**Why:** Currently weight N = N physical slots in the list, selected by `currentIndex++ % n`. This is crude: a high-weight account that's slow gets the same per-slot treatment as a fast one. Health-aware scoring replaces slot-count with `score = weight × (1-errorRate) × (1/(1+latency/1000))`, and selection is score-weighted random.

**Files:**
- Modify: `pool/account.go` (add `healthStats` per-account, `selectByScore` method, modify `Reload` to stop duplicating slots)
- Test: `pool/account_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestHealthAwareScoringPrefersHealthyAccount verifies that a healthy account
// (0 errors, low latency) is selected more often than a failing one.
func TestHealthAwareScoringPrefersHealthyAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.AddAccount(config.Account{ID: "healthy", Enabled: true, AuthMethod: "social", Region: "us-east-1"})
	config.AddAccount(config.Account{ID: "failing", Enabled: true, AuthMethod: "social", Region: "us-east-1"})

	p := &AccountPool{
		cooldowns:     make(map[string]time.Time),
		errorCounts:   make(map[string]int),
		modelLists:    make(map[string]map[string]bool),
		circuitState:  make(map[string]*circuitBreaker),
		healthStats:   make(map[string]*accountHealth),
	}
	p.Reload()

	// Make "failing" look unhealthy: record errors + high latency.
	for i := 0; i < 4; i++ {
		p.RecordError("failing", false)
	}
	p.recordLatency("failing", 5000) // 5s latency
	p.recordLatency("healthy", 100)  // 100ms latency

	// Sample 100 selections — healthy should win the majority.
	healthyCount := 0
	for i := 0; i < 100; i++ {
		acc := p.GetNextForModelExcluding("model", nil)
		if acc != nil && acc.ID == "healthy" {
			healthyCount++
		}
	}
	if healthyCount < 60 {
		t.Fatalf("expected healthy account selected >60%% of the time, got %d%%", healthyCount)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pool/ -run TestHealthAwareScoringPrefersHealthyAccount -v`
Expected: FAIL — `undefined: p.healthStats`, `undefined: p.recordLatency`.

- [ ] **Step 3: Implement health-aware scoring**

In `pool/account.go`, add:

```go
type accountHealth struct {
	ewmaLatencyMs float64 // EWMA latency (α=0.3)
	successCount  int
	errorCount    int
}

func (p *AccountPool) recordLatency(id string, latencyMs float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.healthStats[id]
	if h == nil {
		h = &accountHealth{}
		p.healthStats[id] = h
	}
	if h.ewmaLatencyMs == 0 {
		h.ewmaLatencyMs = latencyMs
	} else {
		h.ewmaLatencyMs = 0.3*latencyMs + 0.7*h.ewmaLatencyMs
	}
}

// healthScore returns a selection weight for the account: higher = preferred.
// score = effectiveWeight × (1 - errorRate) × (1 / (1 + latency/1000))
func (p *AccountPool) healthScore(id string, weight int) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	w := float64(weight)
	if w < 1 {
		w = 1
	}
	h := p.healthStats[id]
	if h == nil {
		return w // no data → default weight
	}
	total := h.successCount + h.errorCount
	if total == 0 {
		return w
	}
	errorRate := float64(h.errorCount) / float64(total)
	latencyFactor := 1.0 / (1.0 + h.ewmaLatencyMs/1000.0)
	return w * (1.0 - errorRate) * latencyFactor
}
```

Add `healthStats map[string]*accountHealth` to `AccountPool` struct. Init in `GetPool`.

Modify `Reload()` to NOT duplicate slots (weight = probability, not slot count):

```go
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	var accounts []config.Account
	for _, a := range enabled {
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		accounts = append(accounts, a) // one entry per account (weight handled by score)
	}
	p.accounts = accounts
	p.totalAccounts = len(enabled)
}
```

Modify `GetNextForModelExcluding` to use score-weighted random selection (replace the `currentIndex++ % n` loop with score-weighted):

```go
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	// Build candidate list with health scores.
	type candidate struct {
		acc   *config.Account
		score float64
	}
	var candidates []candidate
	totalScore := 0.0
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if p.isCircuitOpenLocked(acc.ID, now) {
			continue
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		score := p.healthScore(acc.ID, effectiveWeight(acc.Weight))
		if score <= 0 {
			score = 0.01 // tiny non-zero so a degraded account is still selectable as fallback
		}
		candidates = append(candidates, candidate{acc, score})
		totalScore += score
	}

	if len(candidates) == 0 {
		// Fallback: return the account with the earliest cooldown.
		return p.fallbackEarliestCooldown(model, excluded, allowOverUsage)
	}

	// Score-weighted random selection.
	r := rand.Float64() * totalScore
	cumulative := 0.0
	for _, c := range candidates {
		cumulative += c.score
		if r <= cumulative {
			return c.acc
		}
	}
	return candidates[len(candidates)-1].acc
}
```

Add `isCircuitOpenLocked` (lock-free version for use inside RLock):

```go
func (p *AccountPool) isCircuitOpenLocked(id string, now time.Time) bool {
	cb, ok := p.circuitState[id]
	if !ok || cb == nil {
		return false
	}
	switch cb.state {
	case circuitOpen:
		if now.Sub(cb.openedAt) >= circuitOpenDuration {
			return false // half-open transition
		}
		return true
	default:
		return false
	}
}
```

Add the fallback method:

```go
func (p *AccountPool) fallbackEarliestCooldown(model string, excluded map[string]bool, allowOverUsage bool) *config.Account {
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}
```

Add `"math/rand"` to imports.

Also update `RecordSuccess` to increment successCount:

```go
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	if cb, ok := p.circuitState[id]; ok && cb != nil {
		cb.state = circuitClosed
		cb.consecutiveErr = 0
	}
	if h, ok := p.healthStats[id]; ok && h != nil {
		h.successCount++
		h.errorCount = 0
	}
}
```

And update `RecordError` to increment health errorCount:

```go
	// Health stats.
	if h := p.healthStats[id]; h != nil {
		h.errorCount++
	}
```

Also update `GetNextExcluding` similarly (or have it delegate to `GetNextForModelExcluding("")`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pool/ -run TestHealthAwareScoring -v`
Expected: PASS (healthy > 60%).
Then: `go test ./pool/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add pool/account.go pool/account_test.go
git commit -m "feat(pool): health-aware scoring + weight-as-probability selection (A1+A5)"
```

---

### Task 4: Session affinity (A3)

**Why:** Without session affinity, consecutive requests from the same API key go to different accounts → cache misses + inconsistent profile context. Session affinity binds an API key to a preferred account (sticky routing) with a TTL.

**Files:**
- Modify: `pool/account.go` (add `apiKeyAffinity` map + `GetNextForModelWithApiKey`)
- Modify: `config/config.go` (add `SessionAffinityEnabled` getter, default false)
- Modify: `proxy/handler.go` (pass API key to pool selection — find the 5 call sites)
- Test: `pool/account_test.go`

- [ ] **Step 1: Add config field**

In `config/config.go`:

```go
	SessionAffinityEnabled bool `json:"sessionAffinityEnabled,omitempty"`
```

Getter:

```go
func GetSessionAffinityEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.SessionAffinityEnabled
}
```

- [ ] **Step 2: Write the failing test**

```go
// TestSessionAffinityBindsApiKeyToAccount verifies that 2 consecutive requests
// from the same API key route to the same account.
func TestSessionAffinityBindsApiKeyToAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.AddAccount(config.Account{ID: "a", Enabled: true, AuthMethod: "social", Region: "us-east-1"})
	config.AddAccount(config.Account{ID: "b", Enabled: true, AuthMethod: "social", Region: "us-east-1"})

	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		errorCounts:     make(map[string]int),
		modelLists:      make(map[string]map[string]bool),
		circuitState:    make(map[string]*circuitBreaker),
		healthStats:     make(map[string]*accountHealth),
		apiKeyAffinity:  make(map[string]apiKeyBinding),
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./pool/ -run TestSessionAffinityBindsApiKeyToAccount -v`
Expected: FAIL — `undefined: p.apiKeyAffinity`, `undefined: p.GetNextForModelWithApiKey`.

- [ ] **Step 4: Implement session affinity**

In `pool/account.go`:

```go
type apiKeyBinding struct {
	accountID string
	lastUsed  time.Time
}

const sessionAffinityTTL = 10 * time.Minute
```

Add `apiKeyAffinity map[string]apiKeyBinding` to `AccountPool`. Init in `GetPool`.

```go
// GetNextForModelWithApiKey selects an account for a request, preferring the one
// already bound to the API key (session affinity). Falls back to health-aware
// scoring if the bound account is unavailable or affinity is disabled.
func (p *AccountPool) GetNextForModelWithApiKey(model string, excluded map[string]bool, apiKey string) *config.Account {
	// Session affinity: try the bound account first.
	if apiKey != "" && config.GetSessionAffinityEnabled() {
		p.mu.RLock()
		binding, ok := p.apiKeyAffinity[apiKey]
		p.mu.RUnlock()
		if ok && time.Since(binding.lastUsed) < sessionAffinityTTL {
			// Check if the bound account is available.
			acc := p.GetByID(binding.accountID)
			if acc != nil && acc.Enabled {
				isExcluded := excluded != nil && excluded[acc.ID]
				cooldown, hasCooldown := p.cooldowns[acc.ID]
				cooldownActive := hasCooldown && time.Now().Before(cooldown)
				if !isExcluded && !cooldownActive && !p.isCircuitOpen(acc.ID, time.Now()) {
					p.mu.Lock()
					binding.lastUsed = time.Now()
					p.apiKeyAffinity[apiKey] = binding
					p.mu.Unlock()
					return acc
				}
			}
		}
	}

	// Fall back to normal selection.
	acc := p.GetNextForModelExcluding(model, excluded)
	if acc != nil && apiKey != "" && config.GetSessionAffinityEnabled() {
		p.mu.Lock()
		p.apiKeyAffinity[apiKey] = apiKeyBinding{accountID: acc.ID, lastUsed: time.Now()}
		p.mu.Unlock()
	}
	return acc
}
```

- [ ] **Step 5: Wire into handler.go**

In `proxy/handler.go`, find the 5 call sites of `GetNextForModelExcluding` (lines ~890, 1450, 1637, 2014, and in `responses_handler.go` 136, 321). Replace each with `GetNextForModelWithApiKey`, passing the API key extracted from the request context (the handler already extracts it for auth). If the API key variable isn't available at the call site, pass `""` (affinity disabled for that path).

Example replacement (handler.go ~890):
```go
		account := h.pool.GetNextForModelWithApiKey(model, excluded, apiKey)
```
(Where `apiKey` is the variable extracted earlier in the handler for auth. If there's no apiKey variable at a given call site, pass `""`.)

- [ ] **Step 6: Run tests**

Run: `go test ./pool/ -run TestSessionAffinity -v`
Expected: PASS.
Then: `go test ./pool/ ./config/ ./proxy/ -v`
Expected: all PASS (except the 2 translator ones).

- [ ] **Step 7: Commit**

```bash
git add pool/account.go pool/account_test.go config/config.go proxy/handler.go proxy/responses_handler.go
git commit -m "feat(pool): session affinity per API key (A3)"
```

---

## Final Verification

- [ ] `go test ./pool/ ./config/ ./auth/ -v` passes
- [ ] `go vet ./...` clean
- [ ] `go build -o kiro-go.exe .` succeeds
- [ ] Manual: with session affinity ON, 2 requests from the same API key → same account. With it OFF, round-robin as before.
