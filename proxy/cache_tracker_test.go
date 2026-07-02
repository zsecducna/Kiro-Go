package proxy

import (
	"crypto/sha256"
	"kiro-go/config"
	"strings"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048)
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048)
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}

// TestPromptCacheCrossAccountSharing verifies C1: two different accountIDs with
// the SAME prompt fingerprint share cache entries. Account B's request should
// HIT on the fingerprint Account A stored — no per-account isolation.
func TestPromptCacheCrossAccountSharing(t *testing.T) {
	tracker := newPromptCacheTracker(5 * time.Minute)

	// Build a profile with one explicit cache_control breakpoint above the
	// min-token threshold.
	block := cacheablePromptBlock{
		Value: map[string]interface{}{"kind": "system", "block": map[string]interface{}{
			"type": "text", "text": strings.Repeat("x ", 600), // ~600 tokens > 1024? use more
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		}},
		Tokens: 1200,
		TTL:    5 * time.Minute,
	}
	hasher := sha256.New()
	writeHashChunk(hasher, canonicalizeCacheValue(block.Value))
	var fp [32]byte
	copy(fp[:], hasher.Sum(nil))

	profile := &promptCacheProfile{
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fp, CumulativeTokens: 1200, TTL: 5 * time.Minute}},
		TotalInputTokens: 1200,
		Model:            "claude-sonnet-4-5",
	}

	// Account A: first request → cache_creation.
	usageA := tracker.Compute("account-A", profile)
	if usageA.CacheCreationInputTokens == 0 {
		t.Fatalf("account A: expected cache_creation > 0, got %d", usageA.CacheCreationInputTokens)
	}
	tracker.Update("account-A", profile)

	// Account B: SAME prompt, DIFFERENT account → should be cache_read (C1 fix).
	// Before C1: account B had its own empty store → cache_creation.
	// After C1:  account B shares the global store → cache_read.
	usageB := tracker.Compute("account-B", profile)
	if usageB.CacheReadInputTokens == 0 {
		t.Fatalf("account B: expected cache_read > 0 (cross-account sharing), got 0. usage=%+v", usageB)
	}
}

// TestPromptCacheCapConfigurable verifies C2: the cache-read cap can be set
// above the default 0.85 via config, so a request where 90% of input is from
// cache reports the full 90% (not clamped to 85%).
func TestPromptCacheCapConfigurable(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// Build a profile: 1024 tokens cached (>= defaultMinCacheableTokens), total
	// 1100. Default cap clamps cache_read to 0.85*1100=935; cap=0.95 allows up
	// to 0.95*1100=1045 >= 1024, so the full breakpoint is reported.
	hasher := sha256.New()
	writeHashChunk(hasher, canonicalizeCacheValue(map[string]interface{}{"k": strings.Repeat("v ", 500)}))
	var fp [32]byte
	copy(fp[:], hasher.Sum(nil))
	profile := &promptCacheProfile{
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fp, CumulativeTokens: 1024, TTL: 5 * time.Minute}},
		TotalInputTokens: 1100,
		Model:            "claude-sonnet-4-5",
	}
	tracker := newPromptCacheTracker(5 * time.Minute)
	tracker.Update("acc", profile) // store it

	// Default cap 0.85: cache_read clamped to min(1000, 0.85*1100=935) = 935.
	usage85 := tracker.Compute("acc", profile)
	if usage85.CacheReadInputTokens > 940 {
		t.Fatalf("default cap: expected cache_read ~935, got %d", usage85.CacheReadInputTokens)
	}

	// Raise cap to 0.95: cache_read should be min(1000, 0.95*1100=1045) = 1000.
	config.UpdatePromptCacheMaxRatio(0.95)
	defer config.UpdatePromptCacheMaxRatio(0.85)
	usage95 := tracker.Compute("acc", profile)
	if usage95.CacheReadInputTokens < 990 {
		t.Fatalf("cap 0.95: expected cache_read ~1000, got %d", usage95.CacheReadInputTokens)
	}
}

// TestPromptCacheDiskPersistence verifies C3: entries saved to disk are
// reloaded on startup, surviving a "restart" (new tracker instance).
func TestPromptCacheDiskPersistence(t *testing.T) {
	path := t.TempDir() + "/prompt_cache.json"

	// Tracker 1: store an entry, flush to disk.
	t1 := newPromptCacheTracker(5 * time.Minute)
	hasher := sha256.New()
	writeHashChunk(hasher, "test-cache-value-disk")
	var fp [32]byte
	copy(fp[:], hasher.Sum(nil))
	t1.mu.Lock()
	t1.entries[fp] = promptCacheEntry{
		ExpiresAt: time.Now().Add(3 * time.Minute),
		TTL:       5 * time.Minute,
	}
	t1.dirty = true
	t1.mu.Unlock()
	t1.flush(path)

	// Tracker 2: load from disk → should have the entry.
	t2 := newPromptCacheTracker(5 * time.Minute)
	t2.Load(path)
	t2.mu.Lock()
	_, ok := t2.entries[fp]
	t2.mu.Unlock()
	if !ok {
		t.Fatalf("C3: entry not reloaded from disk after 'restart'")
	}

	// Expired entry should NOT reload.
	path2 := t.TempDir() + "/expired.json"
	t1b := newPromptCacheTracker(5 * time.Minute)
	t1b.mu.Lock()
	fpExpired := sha256.Sum256([]byte("expired"))
	t1b.entries[fpExpired] = promptCacheEntry{
		ExpiresAt: time.Now().Add(-1 * time.Minute), // already expired
		TTL:       5 * time.Minute,
	}
	t1b.dirty = true
	t1b.mu.Unlock()
	t1b.flush(path2)

	t3 := newPromptCacheTracker(5 * time.Minute)
	t3.Load(path2)
	t3.mu.Lock()
	_, okExpired := t3.entries[fpExpired]
	t3.mu.Unlock()
	if okExpired {
		t.Fatalf("C3: expired entry should not be reloaded")
	}
}

// TestComputeBreakdownClampedToCreation verifies the Anthropic usage invariant
// cache_creation_input_tokens == ephemeral_5m + ephemeral_1h holds even when the
// 0.85 cacheable cap reduces creation below the raw breakpoint deltas.
// Profile: TotalInputTokens=10000 → maxCacheable=8500. A matched breakpoint at
// 2000 leaves creation=6500, but the uncapped breakdown from matched=2000 to the
// 10000 breakpoint is 8000 (1h). Without clamping, 5m+1h (8000) != creation (6500).
func TestComputeBreakdownClampedToCreation(t *testing.T) {
	tr := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 10000,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: [32]byte{1}, CumulativeTokens: 2000, TTL: 5 * time.Minute},
			{Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: time.Hour},
		},
	}
	now := time.Now()
	tr.entries[[32]byte{1}] = promptCacheEntry{ExpiresAt: now.Add(time.Hour), TTL: time.Hour, LastHit: now}

	usage := tr.Compute("acct", profile)

	if usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens != usage.CacheCreationInputTokens {
		t.Fatalf("breakdown must sum to creation: 5m=%d 1h=%d creation=%d",
			usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
	if usage.CacheCreation1hInputTokens > usage.CacheCreationInputTokens {
		t.Fatalf("1h breakdown must not exceed creation: 1h=%d creation=%d",
			usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
}

// TestComputeBreakdownClampedToCreationEmptyCacheBelowMin verifies the empty-cache
// (first-request) path still preserves the Anthropic invariant
// cache_creation_input_tokens == ephemeral_5m + ephemeral_1h when the input is
// below the minimum-cacheable threshold. effectiveCreation is zeroed
// (lastTokens < minTokens), but computePromptCacheTTLBreakdown(profile, 0) still
// returns lastTokens > 0 — the empty-path clamp (cache_tracker.go:255) must force
// 5m/1h to 0 so message_start doesn't emit an invariant-violating usage. Guards
// the empty-cache clamp site, which the matched-path TestComputeBreakdownClampedToCreation
// does not exercise (it always has lastTokens >= minTokens).
func TestComputeBreakdownClampedToCreationEmptyCacheBelowMin(t *testing.T) {
	tr := newPromptCacheTracker(time.Hour) // empty → empty-cache (first-request) path
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6", // minTokens = 1024 (non-opus default)
		TotalInputTokens: 200,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: [32]byte{1}, CumulativeTokens: 100, TTL: time.Hour}, // 100 < 1024 → effectiveCreation = 0
		},
	}

	usage := tr.Compute("acct", profile)

	if usage.CacheCreationInputTokens != 0 {
		t.Fatalf("below-minTokens first request should yield zero creation; got %d", usage.CacheCreationInputTokens)
	}
	if sum := usage.CacheCreation5mInputTokens + usage.CacheCreation1hInputTokens; sum != usage.CacheCreationInputTokens {
		t.Fatalf("breakdown must sum to creation (empty path, below min): 5m=%d 1h=%d creation=%d",
			usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
}

// TestPromptCacheMaxEntriesConfigurable verifies the cache LRU bound is
// configurable via config and defaults to 131072.
func TestPromptCacheMaxEntriesConfigurable(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if got := config.GetPromptCacheMaxEntries(); got != 131072 {
		t.Fatalf("default cap: expected 131072, got %d", got)
	}

	if err := config.UpdatePromptCacheMaxEntries(50000); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := config.GetPromptCacheMaxEntries(); got != 50000 {
		t.Fatalf("after update: expected 50000, got %d", got)
	}

	// ≤ 0 falls back to the default.
	if err := config.UpdatePromptCacheMaxEntries(0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := config.GetPromptCacheMaxEntries(); got != 131072 {
		t.Fatalf("zero should fall back to default 131072, got %d", got)
	}
}
