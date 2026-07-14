// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"kiro-go/auth"
	"kiro-go/config"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

const (
	circuitClosed         = 0
	circuitOpen           = 1
	circuitHalfOpen       = 2
	circuitErrorThreshold = 5
	circuitOpenDuration   = 30 * time.Second
	sessionAffinityTTL    = 10 * time.Minute

	// healthEWMAAlpha is the smoothing factor for the per-account EWMA health
	// signals (error rate and latency). Higher = react faster to recent events.
	healthEWMAAlpha = 0.3

	// maxAffinityEntries bounds the session-affinity map; when exceeded, expired
	// bindings are swept before inserting a new one.
	maxAffinityEntries = 1024
)

// circuitBreaker is a per-account 3-state breaker. Its own mutex guards the
// state transitions so it can be evaluated from the selection path (which only
// holds the pool's RLock) and still persist open->half-open correctly, instead
// of silently un-blocking every request once the open window elapses.
type circuitBreaker struct {
	mu             sync.Mutex
	state          int
	consecutiveErr int
	openedAt       time.Time
}

// isOpen reports whether the breaker is currently blocking requests. After the
// open window elapses it persists the open->half-open transition and returns
// false to let a single probe through.
func (cb *circuitBreaker) isOpen(now time.Time) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case circuitOpen:
		if now.Sub(cb.openedAt) >= circuitOpenDuration {
			cb.state = circuitHalfOpen // persist transition; allow one probe
			return false
		}
		return true
	case circuitHalfOpen:
		return false
	default:
		return false
	}
}

// recordError advances the breaker on a failed request.
func (cb *circuitBreaker) recordError(now time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveErr++
	if cb.state == circuitHalfOpen {
		cb.state = circuitOpen // probe failed → re-open
		cb.openedAt = now
	} else if cb.consecutiveErr >= circuitErrorThreshold && cb.state == circuitClosed {
		cb.state = circuitOpen
		cb.openedAt = now
	}
}

// reset closes the breaker after a successful request.
func (cb *circuitBreaker) reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = circuitClosed
	cb.consecutiveErr = 0
}

// accountHealth tracks per-account health signals used by score-weighted
// selection: EWMA latency and a decaying EWMA error rate.
type accountHealth struct {
	ewmaLatencyMs float64 // EWMA latency (α=healthEWMAAlpha)
	ewmaErrorRate float64 // EWMA error rate in [0,1]; 1 per error, 0 per success
	samples       int     // number of recorded observations
}

// apiKeyBinding binds an API key to a preferred account for session affinity.
type apiKeyBinding struct {
	accountID string
	lastUsed  time.Time
}

// isCircuitOpen reports whether an account's circuit breaker is currently open
// (and should be skipped). It transitions open→half-open after the open
// duration elapses, allowing a single probe through. Safe to call without
// holding p.mu — it takes its own RLock for the map read, and the breaker's own
// mutex guards the state transition.
func (p *AccountPool) isCircuitOpen(id string, now time.Time) bool {
	p.mu.RLock()
	cb := p.circuitState[id]
	p.mu.RUnlock()
	return cb != nil && cb.isOpen(now)
}

// AccountPool 账号池
type AccountPool struct {
	mu              sync.RWMutex
	accounts        []config.Account
	totalAccounts   int
	lastDispatchSeq map[string]uint64          // accountID → last dispatch sequence (lower = used longer ago; LRU clock)
	dispatchSeq     uint64                     // monotonically increases per dispatch (stable LRU ordering)
	cooldowns       map[string]time.Time       // 账号冷却时间
	errorCounts     map[string]int             // 连续错误计数
	modelLists      map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	reprobeBackoff  map[string]time.Duration   // accountID → next backoff interval
	reprobeNext     map[string]time.Time       // accountID → when to next probe
	stopRecover     chan struct{}
	circuitState    map[string]*circuitBreaker // accountID → circuit breaker state
	healthStats     map[string]*accountHealth  // accountID → EWMA latency + error/success counts
	apiKeyAffinity  map[string]apiKeyBinding   // apiKeyID → preferred account (sticky routing)
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:       make(map[string]time.Time),
			lastDispatchSeq: make(map[string]uint64),
			errorCounts:     make(map[string]int),
			modelLists:      make(map[string]map[string]bool),
			reprobeBackoff:  make(map[string]time.Duration),
			reprobeNext:     make(map[string]time.Time),
			circuitState:    make(map[string]*circuitBreaker),
			healthStats:     make(map[string]*accountHealth),
			apiKeyAffinity:  make(map[string]apiKeyBinding),
		}
		pool.Reload()
		if config.GetAutoRecoverEnabled() {
			pool.startAutoRecover()
		}
	})
	return pool
}

// Reload rebuilds the account list from config (one entry per account; weight is
// handled as selection probability by healthScore, not as duplicated slots).
// Over-quota accounts are dropped unless either the per-account upstream
// Overages switch (OverageStatus=ENABLED) or the global AllowOverUsage
// setting permits over-quota routing.
func (p *AccountPool) Reload() {
	// Read config (takes cfgLock) BEFORE acquiring p.mu so the pool lock never
	// nests cfgLock — see GetNextForModelExcluding for the ordering rationale.
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()

	p.mu.Lock()
	defer p.mu.Unlock()
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

// GetNext 获取下一个可用账号（加权轮询）
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（LRU 轮询，最少最近使用），并跳过指定账号。
// Delegates to GetNextForModelExcluding with model="" (any model).
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	return p.GetNextForModelExcluding("", excluded)
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // 冷启动：列表未就绪，乐观放行
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding selects an account supporting the model using
// least-recently-used (LRU) selection: the eligible account dispatched longest
// ago is chosen first, which interleaves requests evenly across healthy
// accounts (replacing score-weighted random, whose EWMA-driven skew
// concentrated load on a few "lucky" accounts and starved the rest). Health
// score acts as a tie-breaker when several accounts share the lowest dispatch
// sequence (cold start with several never-dispatched accounts). Skips
// excluded, cooled-down, circuit-open, token-expiring, and quota-blocked
// accounts. model="" means "any model".
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	// Read config (takes cfgLock) BEFORE acquiring p.mu so the pool lock never
	// nests cfgLock. Holding p.mu while blocking on cfgLock would freeze every
	// other pool operation behind the write lock if a config writer is mid-Save
	// (synchronous os.WriteFile). Keeping cfgLock a strict leaf preserves the
	// global order tokenRefreshMu → p.mu → refreshLockFor → cfgLock.
	allowOverUsage := config.GetAllowOverUsage()

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.accounts) == 0 {
		return nil
	}

	now := time.Now()

	// Build candidate list: each eligible account paired with its last
	// dispatch sequence (LRU key) and health score (tie-breaker).
	type candidate struct {
		acc     *config.Account
		lastSeq uint64
		score   float64
	}
	var candidates []candidate
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if cb := p.circuitState[acc.ID]; cb != nil && cb.isOpen(now) {
			continue
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		candidates = append(candidates, candidate{
			acc:     acc,
			lastSeq: p.lastDispatchSeq[acc.ID],
			score:   p.healthScore(acc.ID, effectiveWeight(acc.Weight)),
		})
	}

	if len(candidates) == 0 {
		// Fallback: return the account with the earliest cooldown. Stamp the
		// LRU clock so a later healthy pick doesn't immediately re-select this
		// account (every other dispatch path stamps; the fallback must too, or
		// a degraded account's stale seq wins the next pick and gets a burst).
		acc := p.fallbackEarliestCooldown(model, excluded, allowOverUsage)
		if acc != nil {
			p.dispatchSeq++
			if p.lastDispatchSeq == nil {
				p.lastDispatchSeq = make(map[string]uint64)
			}
			p.lastDispatchSeq[acc.ID] = p.dispatchSeq
		}
		return acc
	}

	// Least-recently-used: pick the candidate dispatched longest ago (lowest
	// sequence; never-dispatched accounts share the zero value). Ties are broken
	// by health score (higher preferred), then at random so a cold pool spreads
	// its first picks instead of always hitting the first account. A monotonic
	// sequence counter (not wall-clock) keys the ordering, so two accounts can
	// never share a key — LRU stays exact round-robin regardless of clock
	// resolution.
	oldest := candidates[0].lastSeq
	for _, c := range candidates[1:] {
		if c.lastSeq < oldest {
			oldest = c.lastSeq
		}
	}
	var top []candidate
	topScore := -1.0
	for _, c := range candidates {
		if c.lastSeq != oldest {
			continue
		}
		if len(top) == 0 || c.score > topScore {
			top = []candidate{c}
			topScore = c.score
		} else if c.score == topScore {
			top = append(top, c)
		}
	}
	chosen := top[rand.Intn(len(top))].acc

	// Advance the monotonic LRU clock and stamp the chosen account so the next
	// pick goes to a different one.
	p.dispatchSeq++
	if p.lastDispatchSeq == nil {
		p.lastDispatchSeq = make(map[string]uint64)
	}
	p.lastDispatchSeq[chosen.ID] = p.dispatchSeq
	return chosen
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}

// GetNextForModelWithApiKey selects an account for a request, preferring the one
// already bound to the API key (session affinity). Falls back to health-aware
// scoring if the bound account is unavailable or affinity is disabled.
func (p *AccountPool) GetNextForModelWithApiKey(model string, excluded map[string]bool, apiKey string) *config.Account {
	// Hoist config reads ABOVE any pool lock (same anti-pattern fixed in 58727ec
	// for GetNextForModelExcluding) — never call config.* under p.mu, or a config
	// Save() mid-write nests cfgLock under the pool lock and freezes every other
	// pool operation. allowOverUsage feeds the quota gate below.
	allowOverUsage := config.GetAllowOverUsage()

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
				p.mu.RLock()
				cooldown, hasCooldown := p.cooldowns[acc.ID]
				// Mirror the normal-path eligibility gates (GetNextForModelExcluding):
				// model support (accountHasModel reads p.modelLists with no lock of its
				// own, so it MUST run under p.mu.RLock) and quota. isQuotaBlocked is a
				// pure function on the account value, but we read it here under the same
				// lock to keep both gates consistent with one snapshot.
				// Token-near-expiry is INTENTIONALLY NOT mirrored: a near-expiry token is
				// still valid within refresh-skew and the handler refreshes it; gating on
				// it would rebind the session to a different account every refresh window.
				hasModel := model == "" || p.accountHasModel(acc.ID, model)
				quotaBlocked := isQuotaBlocked(*acc, allowOverUsage)
				p.mu.RUnlock()
				cooldownActive := hasCooldown && time.Now().Before(cooldown)
				if !isExcluded && !cooldownActive && !p.isCircuitOpen(acc.ID, time.Now()) && hasModel && !quotaBlocked {
					now := time.Now()
					p.mu.Lock()
					binding.lastUsed = now
					p.dispatchSeq++
					if p.lastDispatchSeq == nil {
						p.lastDispatchSeq = make(map[string]uint64)
					}
					p.lastDispatchSeq[acc.ID] = p.dispatchSeq
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
		if len(p.apiKeyAffinity) >= maxAffinityEntries {
			p.pruneExpiredAffinityLocked(time.Now())
		}
		p.apiKeyAffinity[apiKey] = apiKeyBinding{accountID: acc.ID, lastUsed: time.Now()}
		p.mu.Unlock()
	}
	return acc
}

// pruneExpiredAffinityLocked removes session-affinity bindings whose TTL has
// elapsed, keeping the map from growing unbounded with one entry per distinct
// API key ever seen. Caller must hold p.mu (write lock).
func (p *AccountPool) pruneExpiredAffinityLocked(now time.Time) {
	for key, binding := range p.apiKeyAffinity {
		if now.Sub(binding.lastUsed) >= sessionAffinityTTL {
			delete(p.apiKeyAffinity, key)
		}
	}
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Don't wipe a hard (quota/overage/disable) backoff on a late in-flight
	// success — only clear transient (3-error, +1min) cooldowns. A quota backoff
	// (+1h) must persist to its natural expiry or the exhausted upstream gets
	// re-selected immediately.
	if cd, ok := p.cooldowns[id]; ok && time.Until(cd) > cooldownClearThreshold {
		// keep the hard cooldown
	} else {
		delete(p.cooldowns, id)
	}
	p.errorCounts[id] = 0
	// Circuit breaker: reset on success.
	if cb := p.circuitState[id]; cb != nil {
		cb.reset()
	}
	// Health stats: a success decays the error rate toward 0 without erasing
	// recent failures (so a previously-flapping account stays slightly penalised).
	p.recordHealthObservation(id, false)
}

// cooldownClearThreshold is the remaining duration below which RecordSuccess
// will clear a cooldown. Transient (3-error) cooldowns are +1min; quota/overage
// backoffs are +1h. A late in-flight success must not wipe a hard backoff —
// only short transient ones — or the exhausted upstream gets re-selected.
const cooldownClearThreshold = 10 * time.Minute

// setCooldownIfLater sets the cooldown to newExpiry only if it extends the
// existing one (or none exists), so a transient short backoff (the 3-error
// +1min cooldown) can never shorten a longer quota/overage backoff (+1h) for
// the same account. Caller must hold p.mu.
func setCooldownIfLater(cooldowns map[string]time.Time, id string, newExpiry time.Time) {
	if ex, ok := cooldowns[id]; !ok || newExpiry.After(ex) {
		cooldowns[id] = newExpiry
	}
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	p.errorCounts[id]++

	if isQuotaError {
		// 配额错误，冷却 1 小时（不缩短已有的更长冷却）
		setCooldownIfLater(p.cooldowns, id, now.Add(time.Hour))
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次错误，冷却 1 分钟（不缩短已有的更长冷却）
		setCooldownIfLater(p.cooldowns, id, now.Add(time.Minute))
	}

	// Circuit breaker: track consecutive errors.
	cb := p.circuitState[id]
	if cb == nil {
		cb = &circuitBreaker{}
		if p.circuitState == nil {
			p.circuitState = make(map[string]*circuitBreaker)
		}
		p.circuitState[id] = cb
	}
	cb.recordError(now)

	// Health stats: raise the EWMA error rate.
	p.recordHealthObservation(id, true)
}

// recordHealthObservation updates the account's EWMA error rate. isError=true
// pushes the rate toward 1, false decays it toward 0. Caller must hold p.mu.
func (p *AccountPool) recordHealthObservation(id string, isError bool) {
	if p.healthStats == nil {
		p.healthStats = make(map[string]*accountHealth)
	}
	h := p.healthStats[id]
	if h == nil {
		h = &accountHealth{}
		p.healthStats[id] = h
	}
	h.samples++
	sample := 0.0
	if isError {
		sample = 1.0
	}
	h.ewmaErrorRate = healthEWMAAlpha*sample + (1-healthEWMAAlpha)*h.ewmaErrorRate
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if HasStatusToken(msg, "401") || HasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// HasStatusToken returns true when status appears in s with non-alphanumeric
// boundaries on both sides, so "401" matches "HTTP 401 from ..." but not
// "4011", "14013", or an alphanumeric token like "request_401abc". Exported so
// the proxy package can match upstream status codes by the same boundary rule
// (quota/overage classifiers) instead of bare strings.Contains on the body.
func HasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isAlphaNum(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isAlphaNum(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isAlphaNum(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			if !updated {
				p.accounts[i].RequestCount++
				p.accounts[i].TotalTokens += tokens
				p.accounts[i].TotalCredits += credits
				p.accounts[i].LastUsed = time.Now().Unix()

				requestCount = p.accounts[i].RequestCount
				errorCount = p.accounts[i].ErrorCount
				totalTokens = p.accounts[i].TotalTokens
				totalCredits = p.accounts[i].TotalCredits
				lastUsed = p.accounts[i].LastUsed
				updated = true
				continue
			}
			p.accounts[i].RequestCount = requestCount
			p.accounts[i].ErrorCount = errorCount
			p.accounts[i].TotalTokens = totalTokens
			p.accounts[i].TotalCredits = totalCredits
			p.accounts[i].LastUsed = lastUsed
		}
	}
	if updated {
		go config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
	}
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}

// RecordLatency records an observed request latency into the account's EWMA
// (α=healthEWMAAlpha). Called from request handlers after a response completes
// so health-aware selection can prefer faster accounts.
func (p *AccountPool) RecordLatency(id string, latencyMs float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.healthStats == nil {
		p.healthStats = make(map[string]*accountHealth)
	}
	h := p.healthStats[id]
	if h == nil {
		h = &accountHealth{}
		p.healthStats[id] = h
	}
	if h.ewmaLatencyMs == 0 {
		h.ewmaLatencyMs = latencyMs
	} else {
		h.ewmaLatencyMs = healthEWMAAlpha*latencyMs + (1-healthEWMAAlpha)*h.ewmaLatencyMs
	}
	if h.samples == 0 {
		h.samples = 1 // a latency observation counts as evidence for scoring
	}
}

// healthScore returns a tie-breaker weight for the account: higher = preferred.
// score = effectiveWeight × (1 - ewmaErrorRate) × (1 / (1 + latency/1000))
// Used only to break ties in LRU selection. Lock-free: the caller holds p.mu.
func (p *AccountPool) healthScore(id string, weight int) float64 {
	w := float64(weight)
	if w < 1 {
		w = 1
	}
	h := p.healthStats[id]
	if h == nil || h.samples == 0 {
		return w // no data → default weight
	}
	latencyFactor := 1.0 / (1.0 + h.ewmaLatencyMs/1000.0)
	return w * (1.0 - h.ewmaErrorRate) * latencyFactor
}

// fallbackEarliestCooldown returns the account with the earliest cooldown
// (or one with no cooldown at all) when no fully-healthy candidate exists.
// model="" means "any model". Caller must hold p.mu (at least RLock).
func (p *AccountPool) fallbackEarliestCooldown(model string, excluded map[string]bool, allowOverUsage bool) *config.Account {
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
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

// startAutoRecover launches a background goroutine that periodically refreshes
// disabled accounts' tokens. If a refresh succeeds, the account is re-enabled.
// Exponential backoff per account: 1m → 5m → 25m → max 2h.
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
		// Failure → increase backoff: 1m → 5m → 25m → 2h max.
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

// AccountHealthSnapshot is a read-only view of one account's dispatch/health
// signals, surfaced to operators (admin /admin/pool) so the effect of session
// affinity and health-aware routing is observable: a warm, stuck-to account
// shows lower LatencyMsEWMA than a cold one that keeps getting re-picked.
type AccountHealthSnapshot struct {
	ID              string  `json:"id"`
	Email           string  `json:"email,omitempty"`
	LatencyMsEWMA   float64 `json:"latencyMsEwma"`
	ErrorRateEWMA   float64 `json:"errorRateEwma"`
	Samples         int     `json:"samples"`
	Circuit         string  `json:"circuit"` // closed | open | half-open
	CooldownActive  bool    `json:"cooldownActive"`
	LastDispatchSeq uint64  `json:"lastDispatchSeq"` // monotonic LRU clock; higher = more recently dispatched
}

// HealthSnapshots returns a per-account health view for every account currently
// in the pool. Lock order is p.mu (RLock) then each breaker's own mutex — the
// same order the dispatch path uses (p.mu held while calling cb.isOpen) — so
// this introduces no new deadlock risk.
func (p *AccountPool) HealthSnapshots() []AccountHealthSnapshot {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AccountHealthSnapshot, 0, len(p.accounts))
	for i := range p.accounts {
		acc := &p.accounts[i]
		snap := AccountHealthSnapshot{
			ID:              acc.ID,
			Email:           acc.Email,
			Circuit:         "closed",
			LastDispatchSeq: p.lastDispatchSeq[acc.ID],
		}
		if h := p.healthStats[acc.ID]; h != nil {
			snap.LatencyMsEWMA = h.ewmaLatencyMs
			snap.ErrorRateEWMA = h.ewmaErrorRate
			snap.Samples = h.samples
		}
		if cb := p.circuitState[acc.ID]; cb != nil {
			cb.mu.Lock()
			switch cb.state {
			case circuitOpen:
				snap.Circuit = "open"
			case circuitHalfOpen:
				snap.Circuit = "half-open"
			}
			cb.mu.Unlock()
		}
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd) {
			snap.CooldownActive = true
		}
		out = append(out, snap)
	}
	return out
}

// LatencyAggregate is a customer-safe summary of dispatch latency across the
// pool: no account identities, just the distribution. Surfaced in /v1/stats so
// the effect of session affinity is visible without leaking pool internals.
type LatencyAggregate struct {
	AccountsWithData int     `json:"accountsWithData"`
	LatencyMsMean    float64 `json:"latencyMsMean"`
	LatencyMsMin     float64 `json:"latencyMsMin"`
	LatencyMsMax     float64 `json:"latencyMsMax"`
}

// LatencyAggregate summarizes per-account EWMA latency without exposing which
// account is which. Accounts with no recorded latency are ignored (includes
// accounts that only logged errors, since they have no latency to average).
func (p *AccountPool) LatencyAggregate() LatencyAggregate {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var agg LatencyAggregate
	var sum float64
	for i := range p.accounts {
		h := p.healthStats[p.accounts[i].ID]
		if h == nil || h.samples == 0 || h.ewmaLatencyMs <= 0 {
			continue
		}
		l := h.ewmaLatencyMs
		if agg.AccountsWithData == 0 || l < agg.LatencyMsMin {
			agg.LatencyMsMin = l
		}
		if l > agg.LatencyMsMax {
			agg.LatencyMsMax = l
		}
		sum += l
		agg.AccountsWithData++
	}
	if agg.AccountsWithData > 0 {
		agg.LatencyMsMean = sum / float64(agg.AccountsWithData)
	}
	return agg
}
