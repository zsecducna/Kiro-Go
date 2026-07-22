package proxy

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfterSeconds(t *testing.T) {
	cases := map[string]time.Duration{
		"5":   5 * time.Second,
		" 30": 30 * time.Second,
		"":    0,
		"0":   0,
		"-1":  0,
		"abc": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfterSeconds(in); got != want {
			t.Errorf("parseRetryAfterSeconds(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBedrockThrottleRecordAndRemaining(t *testing.T) {
	s := &bedrockThrottleState{until: map[string]time.Time{}}
	acct, model := "acct-thr", "us.anthropic.claude-haiku-4-5-20251001-v1:0"

	// No record -> not throttled.
	if s.remaining(acct, model) != 0 {
		t.Fatal("fresh state should not be throttled")
	}
	// Record a cooldown; must be active and bounded by the requested duration.
	s.record(acct, model, 10*time.Second)
	if d := s.remaining(acct, model); d <= 0 || d > 10*time.Second {
		t.Errorf("remaining = %v, want (0,10s]", d)
	}
	// A different model on the same account is unaffected.
	if s.remaining(acct, "amazon.nova-lite-v1:0") != 0 {
		t.Error("throttle must be per (account, model)")
	}
	// Default applies when duration is non-positive.
	s.record(acct, "m2", 0)
	if d := s.remaining(acct, "m2"); d <= 0 || d > bedrockDefaultThrottleCooldown {
		t.Errorf("default cooldown remaining = %v", d)
	}
	// Oversized Retry-After is capped.
	s.record(acct, "m3", time.Hour)
	if d := s.remaining(acct, "m3"); d > bedrockMaxThrottleCooldown {
		t.Errorf("cooldown not capped: %v", d)
	}
}

func TestBedrockThrottleRecordKeepsLonger(t *testing.T) {
	s := &bedrockThrottleState{until: map[string]time.Time{}}
	s.record("a", "m", 5*time.Minute) // capped at max (5m)
	long := s.remaining("a", "m")
	s.record("a", "m", 2*time.Second) // shorter must NOT shorten the active cooldown
	if got := s.remaining("a", "m"); got < long-time.Second {
		t.Errorf("shorter record shortened cooldown: was %v now %v", long, got)
	}
}

// The throttle sentinel must NOT be classified as a quota error, or the handler
// would escalate a per-model skip into an account-wide 1h cooldown.
func TestErrBedrockThrottledNotQuota(t *testing.T) {
	if isQuotaErrorMessage(errBedrockThrottled.Error()) {
		t.Errorf("errBedrockThrottled %q must not match isQuotaErrorMessage", errBedrockThrottled.Error())
	}
}

func TestBedrockThrottleExpires(t *testing.T) {
	s := &bedrockThrottleState{until: map[string]time.Time{}}
	s.until[bedrockThrottleKey("a", "m")] = time.Now().Add(-time.Second) // already past
	if s.remaining("a", "m") != 0 {
		t.Error("expired cooldown should read as 0")
	}
}

func TestNoteBedrockResponseThrottle(t *testing.T) {
	s := &bedrockThrottleState{until: map[string]time.Time{}}
	// Swap the package global for isolation.
	prev := bedrockThrottle
	bedrockThrottle = s
	defer func() { bedrockThrottle = prev }()

	// Non-429 does nothing.
	noteBedrockResponseThrottle("a", "m", &http.Response{StatusCode: 200})
	if s.remaining("a", "m") != 0 {
		t.Error("200 must not throttle")
	}
	// 429 with Retry-After records the cooldown.
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp.Header.Set("Retry-After", "15")
	noteBedrockResponseThrottle("a", "m", resp)
	if d := s.remaining("a", "m"); d <= 0 || d > 15*time.Second {
		t.Errorf("429 remaining = %v, want (0,15s]", d)
	}
	// nil response is safe.
	noteBedrockResponseThrottle("a", "m2", nil)
	if s.remaining("a", "m2") != 0 {
		t.Error("nil response must not throttle")
	}
}
