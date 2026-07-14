package proxy

import (
	"testing"
	"time"
)

// TestPromptCacheLRUEvictsOldestUnused verifies the list-based LRU: with cap 3,
// after inserting a,b,c, touching a, then inserting d, the evicted entry is b
// (the least-recently-used), not a.
func TestPromptCacheLRUEvictsOldestUnused(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 3)
	now := time.Now()

	tr.mu.Lock()
	tr.putLocked([32]byte{1}, now.Add(time.Hour), time.Hour) // a
	tr.putLocked([32]byte{2}, now.Add(time.Hour), time.Hour) // b
	tr.putLocked([32]byte{3}, now.Add(time.Hour), time.Hour) // c
	tr.putLocked([32]byte{1}, now.Add(time.Hour), time.Hour) // touch a → front
	tr.putLocked([32]byte{4}, now.Add(time.Hour), time.Hour) // d
	tr.evictOverflowLocked()                                 // cap 3 → evict back (b)
	tr.mu.Unlock()

	if _, ok := tr.entries[[32]byte{2}]; ok {
		t.Fatalf("expected least-recently-used entry (b) to be evicted")
	}
	for _, want := range [][32]byte{{1}, {3}, {4}} {
		if _, ok := tr.entries[want]; !ok {
			t.Fatalf("expected entry %v to survive", want)
		}
	}
	if got := len(tr.entries); got != 3 {
		t.Fatalf("expected cap=3 after eviction, got %d", got)
	}
}

// TestSplitAgainstTotalAnchorsToRealTotal verifies the cache split is rescaled
// onto the real upstream input total while preserving the accounting identity
// input + creation + read == realTotal.
func TestSplitAgainstTotalAnchorsToRealTotal(t *testing.T) {
	// Estimate domain: covered = 800 (creation 500 + read 300), estTotal = 1000.
	u := promptCacheUsage{
		CacheCreationInputTokens:   500,
		CacheReadInputTokens:       300,
		CacheCreation5mInputTokens: 500,
	}
	got := u.splitAgainstTotal(1000, 2000)

	// ratio = 800/1000 = 0.8 → cacheTotal = 1600; read = 1600*300/800 = 600; creation = 1000.
	if got.CacheReadInputTokens != 600 {
		t.Fatalf("cache_read = %d, want 600", got.CacheReadInputTokens)
	}
	if got.CacheCreationInputTokens != 1000 {
		t.Fatalf("cache_creation = %d, want 1000", got.CacheCreationInputTokens)
	}
	billed := billedClaudeInputTokens(2000, got)
	if billed != 400 {
		t.Fatalf("billed input = %d, want 400", billed)
	}
	if sum := billed + got.CacheCreationInputTokens + got.CacheReadInputTokens; sum != 2000 {
		t.Fatalf("identity broken: input+creation+read = %d, want 2000", sum)
	}
}

// TestSplitAgainstTotalNoCoverageIsAllInput verifies that with no cache
// coverage the whole real total is billed as fresh input.
func TestSplitAgainstTotalNoCoverageIsAllInput(t *testing.T) {
	got := promptCacheUsage{}.splitAgainstTotal(1000, 2000)
	if got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens != 0 {
		t.Fatalf("expected no cache tokens, got read=%d creation=%d", got.CacheReadInputTokens, got.CacheCreationInputTokens)
	}
	if billed := billedClaudeInputTokens(2000, got); billed != 2000 {
		t.Fatalf("expected all 2000 billed as input, got %d", billed)
	}
}

// TestStopIsIdempotent verifies Stop() can be called twice (e.g. test cleanup
// + main shutdown) without panicking on a double close of stopChan.
func TestStopIsIdempotent(t *testing.T) {
	tr := newPromptCacheTracker(time.Hour)
	tr.stopChan = make(chan struct{})
	tr.Stop()
	tr.Stop() // before fix: close of closed channel → panic
}

// TestComputeSetsDirtyOnCacheHit verifies a cache hit marks the tracker dirty
// so the extended TTL/LastHit is persisted even if the save loop flushes
// between Compute and the following Update.
func TestComputeSetsDirtyOnCacheHit(t *testing.T) {
	tr := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 10000,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: [32]byte{1}, CumulativeTokens: 2000, TTL: time.Hour},
		},
	}
	now := time.Now()
	tr.mu.Lock()
	tr.putLocked([32]byte{1}, now.Add(time.Hour), time.Hour)
	tr.mu.Unlock()
	tr.dirty = false

	tr.Compute("acct", profile)

	if !tr.dirty {
		t.Fatal("Compute must set dirty on a cache hit so the refreshed TTL/LastHit is persisted")
	}
}
