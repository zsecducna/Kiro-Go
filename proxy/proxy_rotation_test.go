package proxy

import (
	"sync"
	"testing"
)

// sanitizeProxyURLs trims blanks and de-duplicates while preserving order.
func TestSanitizeProxyURLs(t *testing.T) {
	got := sanitizeProxyURLs([]string{" a ", "b", "a", "", "  ", "c", "b"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// configure applies the first pool entry immediately and sets it active; a single
// entry (or the fallback) does not start rotation.
func TestProxyRotatorConfigureAppliesFirst(t *testing.T) {
	var mu sync.Mutex
	var applied []string
	r := newProxyRotator(func(u string) {
		mu.Lock()
		applied = append(applied, u)
		mu.Unlock()
	})

	// Empty pool → the single fallback is applied.
	r.configure("socks5://single", nil, 10)
	if r.activeURL() != "socks5://single" {
		t.Fatalf("fallback active = %q", r.activeURL())
	}

	// Pool of two → first entry applied and active.
	r.configure("socks5://single", []string{"http://p1", "http://p2"}, 5)
	if r.activeURL() != "http://p1" {
		t.Fatalf("pool active = %q", r.activeURL())
	}
	r.stopRotation()
}

// advance cycles the pool round-robin and applies each proxy in turn.
func TestProxyRotatorRoundRobin(t *testing.T) {
	var applied []string
	r := newProxyRotator(func(u string) { applied = append(applied, u) })
	r.configure("", []string{"http://p1", "http://p2", "http://p3"}, 10)
	r.stopRotation() // stop the timer; drive advance() manually below
	gen := r.gen

	if r.activeURL() != "http://p1" {
		t.Fatalf("start = %q", r.activeURL())
	}
	want := []string{"http://p2", "http://p3", "http://p1", "http://p2"}
	for i, w := range want {
		if !r.advance(gen) {
			t.Fatalf("advance %d returned false", i)
		}
		if r.activeURL() != w {
			t.Fatalf("advance %d active = %q want %q", i, r.activeURL(), w)
		}
	}
}

// A stale tick (an advance carrying an old epoch) is a no-op after reconfigure.
func TestProxyRotatorStaleAdvanceIgnored(t *testing.T) {
	r := newProxyRotator(func(string) {})
	r.configure("", []string{"http://p1", "http://p2"}, 10)
	staleGen := r.gen
	r.stopRotation()
	// Reconfigure to a new pool → new epoch. The stale-epoch advance must not touch it.
	r.configure("", []string{"http://q1", "http://q2"}, 10)
	r.stopRotation()
	if r.advance(staleGen) {
		t.Fatalf("stale-epoch advance should return false")
	}
	if r.activeURL() != "http://q1" {
		t.Fatalf("stale advance changed active to %q", r.activeURL())
	}
}

// A single-entry pool never rotates: advance is a no-op.
func TestProxyRotatorSingleNoRotate(t *testing.T) {
	r := newProxyRotator(func(string) {})
	r.configure("", []string{"http://only"}, 10)
	if r.advance(r.gen) {
		t.Fatalf("single-entry pool should not advance")
	}
	if r.activeURL() != "http://only" {
		t.Fatalf("active = %q", r.activeURL())
	}
}

func TestMaskProxyURL(t *testing.T) {
	cases := map[string]string{
		"":                             "(direct)",
		"socks5://host:1080":           "socks5://host:1080",
		"http://user:secret@host:8080": "http://user:xxxxx@host:8080",
		"socks5://u:p@1.2.3.4:1080":    "socks5://u:xxxxx@1.2.3.4:1080",
	}
	for in, want := range cases {
		if got := maskProxyURL(in); got != want {
			t.Fatalf("maskProxyURL(%q) = %q want %q", in, got, want)
		}
	}
}
