# Dispatch & Cache Improvements — Design

- **Ngày:** 2026-06-27
- **Trạng thái:** Chờ duyệt (design)
- **Branch:** `feat/azure-tenant-sso`
- **Scope:** Cải thiện cơ chế điều phối round-robin (Part A) + prompt-cache accounting (Part B, cả 3 fix C1/C2/C3)

## 1. Bối cảnh

### Dispatch hiện tại (`pool/account.go`)
Weighted round-robin: `weight N → N slot vật lý` trong list. `currentIndex++` atomic modulo `n`. Skip: excluded (failover), cooldown (1min/3 errors, 1h/quota error), token near-expiry (120s skew), quota-blocked. Failover (`account_failover.go`), `IsAuthFailure` → `DisableAccount` (permanent + persist). Fallback: account có cooldown sớm nhất.

### Cache hiện tại (`proxy/cache_tracker.go`)
**Prompt-cache accounting** (KHÔNG phải response cache). In-memory, per-account (`entriesByAccount[accountID]`), TTL 5m/1h, min 1024/4096 tokens, cap 85%. SHA-256 running hash của cacheable blocks (canonicalized). `BuildClaudeProfile` → `Compute` (check hit) → `Update` (store). Mirror Anthropic upstream caching để report `cache_creation/cache_read` tokens.

## 2. Mục tiêu & non-goals

**Mục tiêu:**
- **Part A (dispatch):** health-aware routing, circuit breaker, session affinity, auto-recovery, weight = probability. Giảm account die, cân bằng theo sức khỏe thật.
- **Part B (cache):** 3 fix — C1 cross-account sharing, C2 cap adjustable, C3 disk persistence. Tăng hit rate accuracy + sống qua restart.

**Non-goals:**
- Không thêm response cache thật (chỉ cải thiện accounting accuracy).
- Không đổi refresh/dispatch logic trong `auth/`.
- Không thay đổi endpoint routing (`/v1/messages` etc.).

## 3. Part A — Dispatch improvements

### A1 Health-aware scoring
- Track per-account (new `pool/health.go`): EWMA latency (rolling 20 requests, α=0.3), success rate (sliding window 50), consecutive errors.
- `Score(acc) = weight × (1 - errorRate) × (1 / (1 + latencyMs/1000))`. Account khỏe + nhanh → score cao.
- Selection: score-weighted random (thay slot-count). `currentIndex` → `weightedRandom(scores)`.

### A2 Circuit breaker
- 5 consecutive errors → **open** (skip 30s) → **half-open** (1 probe request) → **close** (success).
- Khác cooldown hiện tại (chỉ đếm 3 errors = 1min, không probe). Circuit breaker tách biệt: `circuitState[id] = CLOSED|OPEN|HALF_OPEN`.

### A3 Session affinity (per API key)
- `apiKeyAffinity map[apiKey]→{accountID, lastUsed}` (sync.Mutex).
- Request từ 1 key → ưu tiên account đã bind (nếu available + healthy).
- TTL 10 min idle → rebind sang account có score cao nhất.
- Fallback: bound account unavailable → score-weighted selection.

### A4 Auto-recovery
- Account bị `DisableAccount` (auth failure) → đánh dấu `disabledAt` + `reprobeNext`.
- Goroutine riêng (`autoRecover`, check mỗi 1 min): nếu `now > reprobeNext` → `auth.RefreshToken` → success → `re-enable + Reload`. Fail → backoff: 1m → 5m → 30m → max 2h.
- Config: `AutoRecoverEnabled` (default true), `AutoRecoverMaxInterval` (default 2h).

### A5 Weight = probability
- `effectiveWeight` = `max(weight, 1)`. Score nhân với weight. Weight 2 = 2× probability.
- Bỏ slot-count approach trong `Reload()` (không duplicate account N lần).

## 4. Part B — Cache improvements

### C1 Cross-account sharing (fix lớn nhất)
- **Đổi key:** `entriesByAccount[accountID]` → **global** `entries[fingerprint]` (bỏ accountID).
- Cùng prompt + model = cùng fingerprint = cùng hit, bất kể account nào serve.
- Hit rate: từ `~(1/N) × 70%` → `~70%` trong pool N account.
- Trade-off: nếu account ở org khác nhau (upstream cache thật khác nhau) → over-report hit. Acceptable (cùng tenant codezdevbatman = cùng org).

### C2 85% cap adjustable
- `maxCacheableRatio` config (default 0.85). Cho phép set 0.95 cho "continue" requests (turn mới rất ngắn).
- Hoặc: bỏ cap cho external_idp (account PRO, ít lo over-report). Đơn giản nhất: config global.

### C3 Disk persistence
- Load `data/prompt_cache.json` (hoặc `{configDir}/prompt_cache.json`) khi `newPromptCacheTracker`.
- Write debounce: goroutine flush mỗi 30s nếu có thay đổi (`dirty` flag).
- Format: `{version, entries: [{fingerprint, expiresAt, ttl}]}`.
- File `data/prompt_cache.json` đã tồn tại (legacy) — load nó, migrate format nếu cần.

## 5. Luồng dữ liệu

### Dispatch (sau cải thiện)
```
request → extract apiKey + model
  → sessionAffinity[apiKey] → bound account? available+healthy? → use it
  → else: score-weighted random over non-excluded, non-open-circuit, non-cooldown, non-expired accounts
  → ensureValidToken → forward
  → RecordSuccess/RecordError → update health + circuit + affinity
  → auth failure → DisableAccount → autoRecover later
```

### Cache (sau cải thiện)
```
request → BuildClaudeProfile (breakpoints + fingerprints)
  → Compute(accountID="") → global lookup (cross-account)
  → hit → cache_read; miss → cache_creation
  → Update → store in global map + dirty=true
  → debounce goroutine → flush to disk every 30s
```

## 6. Error handling & testing

**Dispatch:** circuit breaker state machine unit test; scoring test (high-latency vs low-latency account); session affinity binding + TTL expiry; auto-recovery backoff (1m→5m→30m→2h) + re-enable on RefreshToken success.

**Cache:** cross-account hit test (2 accounts, same prompt → 1 creation + 1 read); cap adjustment (0.85 vs 0.95); disk round-trip (save → reload → same entries).

**Regression:** existing cooldown/quota/failover tests still pass.

## 7. File ảnh hưởng

| File | Thay đổi |
|---|---|
| `pool/account.go` | score-weighted selection, circuit breaker, auto-recovery, weight=probability |
| `pool/health.go` (new) | EWMA latency, success rate, scoring |
| `pool/session_affinity.go` (new) | apiKey→account binding |
| `proxy/cache_tracker.go` | global entries (C1), cap config (C2), disk load/save (C3) |
| `config/config.go` | new config: CircuitBreakerThreshold, SessionAffinityTTL, AutoRecover*, CacheCapRatio, CachePersistEnabled |

## 8. Rủi ro

- **Score-weighted vs round-robin:** round-robin đảm bảo fairness tuyệt đối; score-weighted thiên về healthy accounts → có thể "bỏ đói" account chậm. Mitigation: weight factor đảm bảo account chậm vẫn được chọn đôi khi.
- **Cross-account cache accuracy:** nếu upstream cache thật per-org, tracker over-reports hit. Acceptable cho tenant codezdevbatman (cùng org).
- **Disk persistence race:** concurrent Update + flush. Mitigation: mutex + dirty flag + debounce.
- **Auto-recovery churn:** re-probe liên tục tốn Microsoft quota. Mitigation: exponential backoff (max 2h).
