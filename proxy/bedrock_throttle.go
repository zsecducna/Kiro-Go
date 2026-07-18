package proxy

// Adaptive throttle tracking for the Bedrock provider.
//
// AWS Service Quotas (per-model RPM/TPM) is not reachable in this deployment, so
// throughput is learned from observed 429 responses instead. When an invoke or
// converse call returns HTTP 429 (ThrottlingException), the (account, model) pair
// is put into a cooldown (honoring Retry-After when present). While cooling down,
// doBedrockInvoke / doBedrockConverseInvoke short-circuit BEFORE the network call
// with errBedrockThrottled — a pre-stream error, so the handler excludes that
// account and fails over to another one instead of repeatedly hitting a throttled
// model. Cooldowns are per (account, model): a throttle on one account's model
// never penalizes the same model on a different account.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errBedrockThrottled signals a model is in a throttle cooldown. It is a
// pre-stream failure, so callers fail over to another account. The message
// deliberately omits "429" so it is NOT matched by isQuotaErrorMessage (which
// would escalate this per-model advisory skip into an account-wide cooldown).
var errBedrockThrottled = errors.New("bedrock: model in throttle cooldown, failing over")

const (
	// bedrockDefaultThrottleCooldown applies when a 429 carries no Retry-After.
	bedrockDefaultThrottleCooldown = 20 * time.Second
	// bedrockMaxThrottleCooldown caps a (possibly hostile) Retry-After value.
	bedrockMaxThrottleCooldown = 5 * time.Minute
)

type bedrockThrottleState struct {
	mu    sync.RWMutex
	until map[string]time.Time
}

var bedrockThrottle = &bedrockThrottleState{until: map[string]time.Time{}}

func bedrockThrottleKey(accountID, model string) string {
	return accountID + "\x00" + model
}

// remaining returns the time left in the (account, model) cooldown, or 0.
func (s *bedrockThrottleState) remaining(accountID, model string) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.until[bedrockThrottleKey(accountID, model)]
	if !ok {
		return 0
	}
	if d := time.Until(t); d > 0 {
		return d
	}
	return 0
}

// record starts or extends a cooldown for the (account, model) pair. A
// non-positive duration falls back to the default; oversized values are capped.
// An existing longer cooldown is kept (never shortened by a concurrent 429 that
// carried a smaller Retry-After).
func (s *bedrockThrottleState) record(accountID, model string, d time.Duration) {
	if d <= 0 {
		d = bedrockDefaultThrottleCooldown
	}
	if d > bedrockMaxThrottleCooldown {
		d = bedrockMaxThrottleCooldown
	}
	until := time.Now().Add(d)
	key := bedrockThrottleKey(accountID, model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.until[key]; ok && existing.After(until) {
		return
	}
	s.until[key] = until
}

// noteBedrockResponseThrottle records a cooldown when a Bedrock response is a 429.
func noteBedrockResponseThrottle(accountID, model string, resp *http.Response) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	bedrockThrottle.record(accountID, model, parseRetryAfterSeconds(resp.Header.Get("Retry-After")))
}

// parseRetryAfterSeconds parses the delta-seconds form of a Retry-After header
// (the form Bedrock uses); returns 0 for absent/unparseable values.
func parseRetryAfterSeconds(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}
