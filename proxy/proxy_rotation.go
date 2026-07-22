package proxy

import (
	"net/url"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// proxyRotator rotates the GLOBAL outbound proxy through a configured pool on a fixed
// interval (round-robin). When the pool has fewer than two entries it is inert: the
// single applied proxy (pool[0] or the fallback single ProxyURL) is set once and no
// ticker runs. Per-account proxies are unaffected — they override the global proxy at
// request time regardless of rotation.
type proxyRotator struct {
	mu      sync.RWMutex
	urls    []string
	idx     int
	active  string        // currently applied proxy URL ("" = direct)
	minutes int           // rotation interval
	stop    chan struct{} // closed to stop the running ticker; nil when none
	gen     uint64        // bumped on every configure; identifies the current epoch
	apply   func(string)  // applies a proxy URL to the outbound HTTP clients
	// applyMu serializes applyProxy so overlapping configures/ticks cannot interleave
	// applyProxyConfig's separate client swaps (leaving kiro on proxy A, auth on B).
	applyMu sync.Mutex
}

// newProxyRotator builds a rotator that applies proxies via apply (e.g. applyProxyConfig).
func newProxyRotator(apply func(string)) *proxyRotator {
	return &proxyRotator{apply: apply}
}

// sanitizeProxyURLs trims, drops blank entries, and de-duplicates while preserving
// order. Validation of scheme is done at the API boundary; this just cleans the list.
func sanitizeProxyURLs(urls []string) []string {
	seen := make(map[string]struct{}, len(urls))
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// configure (re)loads the rotation from settings and applies the first proxy
// immediately, then (re)starts the ticker when the pool has more than one entry. Safe
// to call repeatedly — an existing ticker is stopped first. single is the fallback
// applied when the pool is empty.
func (r *proxyRotator) configure(single string, urls []string, minutes int) {
	clean := sanitizeProxyURLs(urls)
	if minutes <= 0 {
		minutes = config.DefaultProxyRotateMinutes
	}

	r.mu.Lock()
	if r.stop != nil { // stop any prior ticker before reconfiguring
		close(r.stop)
		r.stop = nil
	}
	r.urls = clean
	r.idx = 0
	r.minutes = minutes
	r.gen++
	gen := r.gen
	first := single
	if len(clean) > 0 {
		first = clean[0]
	}
	var stop chan struct{}
	if len(clean) > 1 {
		stop = make(chan struct{})
		r.stop = stop
	}
	r.mu.Unlock()

	r.applyProxy(gen, first)
	if len(clean) > 1 {
		logger.Infof("[Proxy] rotation enabled: %d proxies every %dm, starting at %s", len(clean), minutes, maskProxyURL(first))
		go r.loop(stop, gen)
	}
}

// applyProxy applies url to the outbound clients and records it as active, but only if
// gen still matches the current epoch — a configure() that superseded this call (newer
// gen) causes it to skip, so a stale tick or an older overlapping configure cannot
// clobber the newer setting. applyMu serializes the whole apply so the client swaps of
// two calls never interleave.
func (r *proxyRotator) applyProxy(gen uint64, url string) {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()

	r.mu.Lock()
	if gen != r.gen { // superseded by a newer configure — do nothing
		r.mu.Unlock()
		return
	}
	r.active = url
	r.mu.Unlock()

	r.apply(url)
}

// loop advances the round-robin index every interval and applies the next proxy until
// stop is closed. gen ties this loop to the epoch that started it.
func (r *proxyRotator) loop(stop chan struct{}, gen uint64) {
	r.mu.RLock()
	interval := time.Duration(r.minutes) * time.Minute
	r.mu.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if !r.advance(gen) { // superseded epoch or pool shrank out from under us
				return
			}
		}
	}
}

// advance moves to the next proxy in round-robin order and applies it. Returns false
// (caller stops the loop) when gen no longer matches the current epoch or the pool no
// longer has at least two entries.
func (r *proxyRotator) advance(gen uint64) bool {
	r.mu.Lock()
	if gen != r.gen || len(r.urls) < 2 {
		r.mu.Unlock()
		return false
	}
	r.idx = (r.idx + 1) % len(r.urls)
	next := r.urls[r.idx]
	r.mu.Unlock()

	logger.Infof("[Proxy] rotated to %s", maskProxyURL(next))
	r.applyProxy(gen, next)
	return true
}

// activeURL returns the proxy currently applied (empty string means direct).
func (r *proxyRotator) activeURL() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// stopRotation stops any running ticker without changing the active proxy.
func (r *proxyRotator) stopRotation() {
	r.mu.Lock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	r.mu.Unlock()
}

// maskProxyURL hides the password in a proxy URL for logging (user:pass@host ->
// user:xxxxx@host). Empty means a direct connection.
func maskProxyURL(raw string) string {
	if raw == "" {
		return "(direct)"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Redacted()
}
