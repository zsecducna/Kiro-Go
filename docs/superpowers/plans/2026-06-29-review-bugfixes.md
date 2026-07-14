# Plan — Review Bugfixes (2026-06-29)

Branch: `feat/dispatch-cache-hardening` (not main). Merge base for final review: `main`.
Base for per-task reviews: `bd20589` (HEAD before these fixes).
Each task is a single-file, self-contained correctness fix verified by the project review (4 parallel reviewers + controller verification).

## Global Constraints (bind every task)

- **No `config.*` getter while holding `pool.mu`.** Hoist config reads above any pool lock — this is the exact freeze-under-Save-stall anti-pattern fixed in commit `58727ec` for `GetNextForModelExcluding`. Re-introducing it is a regression.
- **Digit-boundary status matching for HTTP status codes.** Use `hasStatusToken` semantics; do not reintroduce bare `strings.Contains(msg, "402"/"401"/"403")` for status detection (false-ban class fixed in commit `3def357`).
- **Classification parity.** The background-refresh path and the request path must agree on whether an error is fatal. Bug #1 is a divergence between them.
- **Tests verify real behavior**, not mocks. TDD: write the test, see it RED, implement, see it GREEN.

---

## Task 1: Background refresh must not BAN accounts on transient "no available Kiro profile"

**Problem:** `RefreshAccountInfo` (background refresh, `proxy/handler.go:313`) → `GetUsageLimits` → `ensureRestProfileArn` → `ResolveProfileArn` returns `fmt.Errorf("no available Kiro profile")` (`proxy/kiro_api.go:309`) when every region probe + token-refresh fallback yields no profile ARN — a TRANSIENT condition (provisioning lag, cross-region probe failure at startup). `classifyAndBanOnUsageError` (`proxy/kiro_api.go:529-539`) routes that through `pool.IsSuspensionError` (matches `"no available kiro profile"` at `pool/account.go:553`) → `banAccountInline(..., "BANNED", ...)` → PERMANENT disable. The request path (`proxy/account_failover.go:105-109`) treats the SAME error as soft (`RecordError(false)`, "never auto-disable"). A good `external_idp` account gets banned forever on a transient blip. This is a regression introduced by commit `3def357` routing classification through `IsSuspensionError`.

**Files:** `proxy/kiro_api.go`, `proxy/kiro_api_test.go`.

**Fix (exact)** — in `classifyAndBanOnUsageError` (`proxy/kiro_api.go:529`), add an early-return for the profile-unavailable error BEFORE the `pool.IsSuspensionError` check:

```go
func classifyAndBanOnUsageError(account *config.Account, err error) error {
	// Profile ARN resolution may fail transiently (provisioning lag, cross-region
	// probe failure). The request path treats this as soft (account_failover.go);
	// the background refresh path must too, or a good external_idp account is
	// permanently banned on a transient blip.
	if isProfileUnavailableErrorMessage(err.Error()) {
		return fmt.Errorf("GetUsageLimits: %w", err)
	}
	switch {
	case pool.IsSuspensionError(err):
		logger.Warnf("[RefreshAccountInfo] Account %s is suspended: %v", account.Email, err)
		banAccountInline(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
		return fmt.Errorf("Account suspended: %w", err)
	case pool.IsAuthFailure(err):
		logger.Warnf("[RefreshAccountInfo] Authentication error for %s: %v", account.Email, err)
		banAccountInline(account, "BANNED", "Authentication failed - token invalid or expired")
	}
	return fmt.Errorf("GetUsageLimits: %w", err)
}
```

`isProfileUnavailableErrorMessage(msg string) bool` already exists in `proxy/account_failover.go:29-32` (same package — directly callable).

**Test (exact)** in `proxy/kiro_api_test.go`, mirroring `TestFalseBanSubstringNoLongerDisables` (`kiro_api_test.go:303-321`):

```go
// TestProfileUnavailableDoesNotBanAccount verifies a transient "no available
// Kiro profile" error from GetUsageLimits does NOT permanently ban the account.
// The background refresh path must mirror the request path's soft handling
// (account_failover.go), or a good external_idp account is banned on a blip.
func TestProfileUnavailableDoesNotBanAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c", Provider: "external_idp"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	_ = classifyAndBanOnUsageError(&acc, errors.New("no available Kiro profile"))

	got, _ := config.GetAccountByID("acct")
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf("profile-unavailable should NOT ban the account; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}
```

**TDD:** Before the fix the test FAILS (account gets BANNED because `IsSuspensionError` matches and bans). After the fix it PASSES. The existing `TestRealSuspensionDisablesAccount` (`:334-340`) must STILL pass (real suspension still bans).

**Acceptance:** new test passes; `TestRealSuspensionDisablesAccount`, `TestFalseBanSubstringNoLongerDisables` still pass; `go test ./proxy/...` clean. Commit with a clear message.

---

## Task 2: Session-affinity must respect model-support and quota eligibility

**Problem:** `GetNextForModelWithApiKey` (`pool/account.go:366-396`) affinity branch gates only on `Enabled`/`!excluded`/`!cooldownActive`/`!isCircuitOpen` (`:381`). It OMITS the hard skips `GetNextForModelExcluding` applies: `accountHasModel` (`:275`), token-near-expiry (`:284`), `isQuotaBlocked` (`:287`). So an affinity-bound account that doesn't support the requested model, or is quota-blocked, is returned anyway → guaranteed upstream failure → failover every request. Worst: 402-overage — `disableAccountOverage` does NOT set `Enabled=false`, so affinity re-routes to the overage account on every request, re-hitting 402 + `FetchOverageStatus` each time.

**Files:** `pool/account.go`, `pool/account_hardening_test.go`.

**Fix:** Add `accountHasModel` and `isQuotaBlocked` gates to the affinity branch. Token-near-expiry is INTENTIONALLY OMITTED for affinity — a near-expiry token is still valid within refresh-skew and the handler refreshes it; breaking affinity on it causes rebind churn every interval. Document this.

**CRITICAL locking constraints:**
- `accountHasModel` (`pool/account.go:221-227`) reads `p.modelLists` WITHOUT its own lock → it MUST be read under `p.mu.RLock`.
- Do NOT call any `config.*` getter while holding `p.mu`. Hoist `allowOverUsage := config.GetAllowOverUsage()` ABOVE the affinity lock. Read `GetNextForModelExcluding`'s start to confirm the exact hoist pattern already used there, and mirror it.

Approximate shape (confirm exact hoist pattern from `GetNextForModelExcluding`):

```go
func (p *AccountPool) GetNextForModelWithApiKey(model string, excluded map[string]bool, apiKey string) *config.Account {
	// Hoist config reads ABOVE any pool lock (same anti-pattern fixed in 58727ec
	// for GetNextForModelExcluding) — never call config.* under p.mu.
	allowOverUsage := config.GetAllowOverUsage()

	if apiKey != "" && config.GetSessionAffinityEnabled() {
		p.mu.RLock()
		binding, ok := p.apiKeyAffinity[apiKey]
		p.mu.RUnlock()
		if ok && time.Since(binding.lastUsed) < sessionAffinityTTL {
			acc := p.GetByID(binding.accountID)
			if acc != nil && acc.Enabled {
				isExcluded := excluded != nil && excluded[acc.ID]
				p.mu.RLock()
				cooldown, hasCooldown := p.cooldowns[acc.ID]
				// Mirror the normal-path eligibility gates: model support + quota.
				// Token-near-expiry is intentionally NOT mirrored — a token within
				// refresh-skew is still valid and the handler refreshes it; breaking
				// affinity on it would cause rebind churn each interval.
				hasModel := model == "" || p.accountHasModel(acc.ID, model)
				quotaBlocked := isQuotaBlocked(*acc, allowOverUsage)
				p.mu.RUnlock()
				cooldownActive := hasCooldown && time.Now().Before(cooldown)
				if !isExcluded && !cooldownActive && !p.isCircuitOpen(acc.ID, time.Now()) && hasModel && !quotaBlocked {
					// ... existing stamp + return (UNCHANGED) ...
				}
			}
		}
	}
	// ... existing fallback (UNCHANGED) ...
}
```

**Tests** in `pool/account_hardening_test.go` (use `healthPool` helper at `:11-22`). The implementer MUST confirm the exact config setter for session affinity (grep the `config` package for the getter `GetSessionAffinityEnabled` used at `pool/account.go:368` and find/add its setter `SetSessionAffinityEnabled(bool)`).

```go
// TestAffinityRespectsModelSupport verifies session affinity does NOT return a
// bound account that lacks the requested model — it must fall through to normal
// selection instead of routing every request to a guaranteed failure.
func TestAffinityRespectsModelSupport(t *testing.T) {
	p := healthPool(
		config.Account{ID: "bound", Enabled: true},
		config.Account{ID: "other", Enabled: true},
	)
	p.modelLists["bound"] = map[string]bool{"sonnet": true}
	p.modelLists["other"] = map[string]bool{"opus": true}
	p.apiKeyAffinity["key"] = apiKeyBinding{accountID: "bound", lastUsed: time.Now()}
	config.SetSessionAffinityEnabled(true) // confirm exact setter name in config package
	t.Cleanup(func() { config.SetSessionAffinityEnabled(false) })

	if acc := p.GetNextForModelWithApiKey("opus", nil, "key"); acc == nil || acc.ID == "bound" {
		t.Fatalf("affinity must skip the bound account that lacks the model; got %#v", acc)
	}
}

// TestAffinityRespectsQuotaBlock verifies a quota-blocked bound account is skipped.
func TestAffinityRespectsQuotaBlock(t *testing.T) {
	p := healthPool(
		config.Account{ID: "bound", Enabled: true, UsageCurrent: 100, UsageLimit: 50}, // over limit, overages off
		config.Account{ID: "other", Enabled: true},
	)
	p.apiKeyAffinity["key"] = apiKeyBinding{accountID: "bound", lastUsed: time.Now()}
	config.SetSessionAffinityEnabled(true)
	t.Cleanup(func() { config.SetSessionAffinityEnabled(false) })

	if acc := p.GetNextForModelWithApiKey("", nil, "key"); acc == nil || acc.ID == "bound" {
		t.Fatalf("affinity must skip a quota-blocked bound account; got %#v", acc)
	}
}
```

Confirm the `config.Account` quota field names (`UsageCurrent`/`UsageLimit`) and `isQuotaBlocked` semantics (`pool/account.go:684-686` + `isOverUsageLimit`) from the code before compiling.

**TDD:** tests fail before the fix (affinity returns the bound account regardless); pass after.

**Acceptance:** `accountHasModel` + `isQuotaBlocked` read under `p.mu.RLock`; no `config.*` call under any pool lock; new tests pass; existing pool tests pass; `go test ./pool/...` clean.

---

## Task 3: message_start cache breakdown must sum to cache_creation_input_tokens

**Problem:** In `Compute` (`proxy/cache_tracker.go`), `creation = lastTokens − matchedTokens` uses `lastTokens` capped at `PromptCacheMaxRatio` (0.85) of total (`:261-264`), but `computePromptCacheTTLBreakdown(profile, matchedTokens)` (`:288`; body `:647-669`) sums breakpoint deltas capped only at `TotalInputTokens` — NOT at the 0.85 cap. When the cap bites, `cache5m+cache1h > creation`. `messageStartUsage` captures the raw `Compute` output (`proxy/handler.go:905`, before `splitAgainstTotal` at `:1253/1530` which re-derives consistently), so `message_start` emits `cache_creation_input_tokens ≠ ephemeral_5m + ephemeral_1h` — violating the Anthropic usage invariant AND disagreeing with later `message_delta`/`message_stop`. Same divergence in the empty-cache path (`:243-255`) when `effectiveCreation` is zeroed below threshold but the breakdown still reports the sub-threshold delta.

**Files:** `proxy/cache_tracker.go`, `proxy/cache_tracker_test.go`.

**Fix (exact)** — add a helper and clamp at both call sites:

```go
// clampCacheBreakdownToCreation scales the 5m/1h cache-creation split down to
// creation when the raw breakpoint deltas (uncapped by the 0.85 cacheable ratio)
// exceed it, preserving the 1h:5m ratio. Guarantees the Anthropic invariant
// cache_creation_input_tokens == ephemeral_5m + ephemeral_1h.
func clampCacheBreakdownToCreation(cache5m, cache1h, creation int) (int, int) {
	total := cache5m + cache1h
	if total <= creation || total <= 0 {
		return cache5m, cache1h
	}
	scale := float64(creation) / float64(total)
	one := int(float64(cache1h)*scale + 0.5)
	if one > creation {
		one = creation
	}
	if one < 0 {
		one = 0
	}
	return creation - one, one
}
```

At `:288`, replace
```go
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, matchedTokens)
```
with
```go
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, matchedTokens)
	cache5m, cache1h = clampCacheBreakdownToCreation(cache5m, cache1h, creation)
```

At the empty-cache path (`:249`), replace
```go
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, 0)
```
with
```go
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, 0)
	cache5m, cache1h = clampCacheBreakdownToCreation(cache5m, cache1h, effectiveCreation)
```

**Test** in `proxy/cache_tracker_test.go` — look at existing `Compute` tests there for the exact way to build a `promptCacheProfile` + tracker + call `Compute` (confirm struct field names `promptCacheProfile`/`promptCacheBreakpoint`/`promptCacheEntry` and the `Compute` receiver/signature from `cache_tracker.go` first):

```go
func TestComputeBreakdownClampedToCreation(t *testing.T) {
	// TotalInputTokens=10000 → maxCacheable=8500 (0.85). Last breakpoint at 10000
	// (1h TTL) so raw breakdown from matched=2000 = 8000, but creation = 6500.
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

	usage := tr.Compute("acct", profile) // confirm Compute receiver/signature from cache_tracker.go

	if usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens != usage.CacheCreationInputTokens {
		t.Fatalf("breakdown must sum to creation: 5m=%d 1h=%d creation=%d",
			usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
	if usage.CacheCreation1hInputTokens > usage.CacheCreationInputTokens {
		t.Fatalf("1h breakdown must not exceed creation: 1h=%d creation=%d",
			usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
}
```

**TDD:** before the fix `5m+1h (8000) != creation (6500)` → fails; after the fix `5m+1h == creation` → passes. Existing `cache_tracker` + `splitAgainstTotal` tests must still pass.

**Acceptance:** `cache5m+cache1h == creation` after clamp at both call sites; new test passes; `go test ./proxy/...` clean.

---

## Task 4: OpenAI stream must emit a finish chunk on mid-stream upstream error

**Problem:** In `handleOpenAIStream` (`proxy/handler.go`), when `CallKiroAPI` fails AFTER `responseStarted` is true, the error path (`:1947-1957`) does `h.recordFailureWithDetails(...); return` — emitting NO chunk. The client's SSE stream ends with no `finish_reason` and no error event → it hangs or reports an ambiguous "incomplete" failure instead of the real error. Compare `handleClaudeStream` (`handler.go:1231-1234`, emits an `error` SSE) and `handleResponsesStream` (`responses_handler.go:481-493`, emits `response.failed`). The normal finish chunk is at `:1994-2013`.

**Files:** `proxy/handler.go`, a handler test file (add `proxy/handler_openai_stream_test.go` or append to an existing handler test file).

**Fix:** Before the `return` at `:1956` (the `responseStarted==true` branch), emit a finish chunk with `finish_reason: "error"` + `[DONE]`, mirroring `:1994-2013`:

```go
		if !responseStarted {
			continue
		}
		// Stream already started: cannot retry or send a JSON error. Terminate
		// the SSE with a finish chunk so the client sees the failure instead of a
		// silent truncation (mirrors handleClaudeStream's error SSE and
		// handleResponsesStream's response.failed).
		errChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "error",
			}},
		}
		errData, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "data: %s\n\n", string(errData))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		h.recordFailureWithDetails("openai", model, account.ID, err)
		return
```

Confirm `chatID`, `model`, `w`, `flusher` are in scope at `:1955` (they are — used at `:1908-1932` and `:1994-2013`). Optionally extract a `sendOpenAIStreamFinish(finishReason string)` helper used by both the normal path and this error path — only if it stays clean; do not over-refactor.

**Test:** Trace what sets `responseStarted` (it is set in `OnToolUse` at `:1933`; confirm whether `processText`/`sendChunk` also set it). Use `httptest` with a mocked `kiroRestHttpStore` (`roundTripFunc` pattern, `proxy/kiro_api_test.go:293-297`) whose transport returns a streaming SSE body that emits one content event (setting `responseStarted`) then errors, so `CallKiroAPI` returns a non-nil error AFTER `responseStarted` is true. Drive the OpenAI stream handler against an `httptest.ResponseRecorder` and assert the response body contains a chunk with `"finish_reason":"error"` and `data: [DONE]`. The test MUST actually exercise the error-after-started path (not a pure mock). If driving the full handler is impractical, extract a minimal testable helper for the finish-chunk emission and assert on it — but the path under test must be the real error path.

**TDD:** before the fix, a mid-stream error yields a response body with no `finish_reason`/`[DONE]` → fails; after the fix it contains `finish_reason:"error"` + `[DONE]` → passes.

**Acceptance:** error-after-started emits `finish_reason:"error"` chunk + `[DONE]`; test exercises the real error path; `go test ./proxy/...` clean.
