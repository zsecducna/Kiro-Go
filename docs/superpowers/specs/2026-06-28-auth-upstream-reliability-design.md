# Auth / Upstream Reliability — Design

- **Ngày:** 2026-06-28
- **Trạng thái:** Đã duyệt (design)
- **Branch:** `feat/dispatch-cache-hardening`
- **Approach:** A1 — Mutex+DCL (B1) + Sweep (B2). Không thêm dependency.

## 1. Bối cảnh

Phần auth/upstream là "src gốc" chưa được harden (dispatch `pool/` + cache `proxy/cache_tracker.go` đã xong 2026-06-27/28). Audit 4-agent (handler / config / upstream+auth / translator) tìm ra 3 bug H-sev trong cụm này:

- **B1 — refresh-storm race** (`auth/oidc.go:27,121,169`): `RefreshToken` → `refreshOIDCToken` / `refreshSocialToken` **không có lock per-account**. Khi token 1 account hết hạn, mỗi request concurrent tự POST lên Microsoft IdP rồi đè `account.AccessToken` lên nhau → refresh-storm (lãng phí quota IdP) + lost-update (token mới nhất bị ghi đè bởi token cũ). 7+ call site trong `handler.go` không phối hợp. Ngoài ra `tokenRefreshMu` (`handler.go:57`) là **1 mutex global** cho toàn bộ account → bottleneck.
- **B2 — false-ban** (`proxy/kiro_api.go:533,550` + `kiro.go:384-394`): `RefreshAccountInfo` phân loại lỗi bằng `strings.Contains("403"/"401"/"invalid"/"expired"/"TEMPORARILY_SUSPENDED")` + inline `config.UpdateAccount(banStatus=DISABLED)`, re-implement logic đã có `pool.IsAuthFailure`/`pool.IsSuspensionError`. Substring `"403"`/`"401"` trần khớp **cả request-id / timestamp** → disable account nhầm. `CallKiroAPI` hand-roll `==401||==403||==402`, không phát hiện suspension, 402 xử lý không nhất quán.

B2 liên quan trực tiếp đến symptom vận hành: 4/4 account trong `data/config.json` đang `BANNED` ("AWS temporarily suspended - unusual user activity detected").

## 2. Mục tiêu & non-goals

**Mục tiêu:**
- **B1:** Mỗi account, tại 1 thời điểm, chỉ có tối đa 1 goroutine gọi IdP refresh; follower tái dùng token mới qua double-checked locking. = `refresh_lock` của Rust (`token_manager.rs:964`).
- **B2:** Mọi phân loại "có nên disable/ban account không?" đi qua `pool.IsAuthFailure`/`IsSuspensionError` + `pool.DisableAccount` duy nhất; ở chỗ có status code thật thì classify theo int. Triệt false-ban.

**Non-goals (spec khác):**
- **B3 `context.Context` trên upstream call** — refactor signature rộng, làm mechanical riêng cho sạch.
- **Auth-provider OIDC primitive dedup** (cụm D, ~350-400 dòng gộp iam/builderid/sso_token/kiro_sso).
- **`x/sync/singleflight` dependency** — chỉ cân nhắc nếu quan sát IdP spam khi token chết hàng loạt (collapse cả failure-case). A1 không collapse failure-case, chấp nhận bounded churn.

## 3. B1 — Per-account refresh lock (Mutex + DCL)

### Vị trí & granularity
- Package-level trong `auth`:
  ```go
  var (
      refreshMu    sync.Mutex              // guard map tạo lock
      refreshLocks = map[string]*sync.Mutex{} // accountID → per-account lock
  )
  func refreshLockFor(id string) *sync.Mutex // lazy create dưới refreshMu
  ```
- Key = `account.ID` (UUID ổn định, set bounded vài chục → không dọn dẹp, leak không đáng kể).

### Luồng (bên trong `auth.RefreshToken`, bao trước phần POST lên IdP)
1. `lock := refreshLockFor(acc.ID); lock.Lock(); defer lock.Unlock()`.
2. **Double-check:** re-read `ExpiresAt` hiện tại của account từ **config** (source of truth đã persist — qua accessor hiện có; nếu chưa có getter per-id thì thêm `config.GetAccountByID(id)` trả copy, trivial). Nếu `now < ExpiresAt − tokenRefreshSkewSeconds` → đã có goroutine khác refresh xong → copy token mới vào `*acc`, return **skip IdP POST**.
3. Nếu vẫn hết hạn → giữ nguyên logic POST + persist hiện tại.
4. `defer Unlock()` đảm bảo nhả lock trên mọi path (kể cả error).

### Kết quả
- N request concurrent cho 1 account → đúng **1 POST lên IdP** trên path success; follower thấy token mới qua DCL → tái sử dụng.
- 7 call site trong `handler.go` + `reprobeDisabled` (`pool/account.go:746`) **không cần đụng** — tất cả đều gọi `auth.RefreshToken`, serialization tự có.

### Tradeoff (đã confirm với user)
- Path **failure** (refresh token bị thu hồi): mutex+DCL **không collapse** như singleflight — mỗi follower đợi xong, tự re-check thấy vẫn hết hạn, tự POST 1 lần (đã serialize, không concurrent-storm). Bounded bởi số request đang in-flight lúc token chết; account bị `pool.DisableAccount` ngay (24h cooldown + remove khỏi pool) nên churn có giới hạn.
- Nếu sau này quan sát Microsoft IdP bị spam khi token chết hàng loạt → nâng lên singleflight (A2) là nhỏ.

## 4. B2 — Sweep error classification

| Chỗ | Hiện tại | Sửa thành |
|---|---|---|
| `kiro_api.go:533,550` (`RefreshAccountInfo`) | `strings.Contains("403"/"401"/"invalid"/"expired"/"TEMPORARILY_SUSPENDED")` + inline `config.UpdateAccount(banStatus=DISABLED)` | `pool.IsSuspensionError(err)` → `pool.DisableAccount(id, reason)`; `pool.IsAuthFailure(err)` → `pool.DisableAccount`. Bỏ substring `"403"`/`"401"` trần (false-ban). |
| `kiro.go:384-394` (`CallKiroAPI`) | hand-roll `==401\|\|==403\|\|==402` | classify theo **`resp.StatusCode` thật** (int, không parse string): `401/403` → auth failure; `402` → `pool.MarkOverLimit` (overage, KHÔNG phải auth); suspension string → `pool.IsSuspensionError`. |
| Routing disable | inline `config.UpdateAccount` rải rác | tất cả đi qua `pool.DisableAccount(id, reason)` duy nhất (persist ban + 24h cooldown + Reload). |

**Helper đã tồn tại, đã digit-boundary-aware** (`pool.IsAuthFailure` dùng `hasStatusToken` nên "403" chỉ match token độc lập, không match `req_403abc`). Không thêm helper mới.

### Behavior change (có chủ đích)
Chạy qua helper chung → **stricter** với suspension (disable ngay) + **precise hơn** với auth (token-boundary match, không false-ban). Net: **ít false-ban hơn** (mục tiêu). Một số edge-case error đổi cách classify — cải thiện, không regression.

## 5. Luồng dữ liệu

```
handler retry loop (6 entry point — không đụng)
  → pool.GetNextForModelWithApiKey(model, excluded, apiKey) → account
  → ensureValidToken(account)
      → nếu token sắp hết hạn: auth.RefreshToken(account)
          → acquire refreshLockFor(account.ID)
          → DCL: re-read ExpiresAt từ config
              → fresh?  → copy vào *acc + return (SKIP IdP POST)
              → stale?  → POST IdP + persist (logic hiện tại)
          → release lock
  → proxy.CallKiroAPI(model, account, …)
      → classify theo resp.StatusCode:
          401 / 403  → auth failure  → failover pool.IsAuthFailure → pool.DisableAccount
          402        → overage       → pool.MarkOverLimit
          susp.string→ pool.IsSuspensionError → pool.DisableAccount
          khác       → pool.RecordError → cooldown (1min/3err) / circuit (5err)
  → success → pool.RecordSuccess + RecordLatency + UpdateStats
```

## 6. Error handling & testing

**Error handling:**
- B1: lock luôn nhả (`defer`); refresh error lan truyền nguyên vẹn; DCL re-check chỉ skip POST khi token thực sự đã tươi.
- B2: classify deterministic theo status code khi có; substring fallback (đã digit-boundary-aware) chỉ cho error opaque.

**Tests mới (TDD — viết trước, fail rồi fix):**
- `TestRefreshLockCollapsesConcurrentRefreshes` (auth): httptest token endpoint đếm POST; account với `ExpiresAt` quá khứ; spawn N goroutine gọi `auth.RefreshToken`; assert **POST count == 1** + tất cả goroutine nhận cùng `AccessToken`.
- `TestFalseBanSubstringNoLongerDisables` (proxy): error `"request req_403abc failed"` (bare 403 trong request-id) → account **không** bị disable (regression guard cho false-ban).
- `TestRealSuspensionDisablesAccount` (proxy): error `"temporarily suspended"` → `pool.DisableAccount` được gọi.
- `TestCallKiroAPIClassifiesByStatusCode` (proxy): 401/403 → `DisableAccount`; 402 → `MarkOverLimit`; suspension body → `DisableAccount`.

**Regression:** toàn bộ auth/proxy test hiện có stay green; `go vet ./...` + `go test ./...`.

## 7. File ảnh hưởng

| File | Thay đổi |
|---|---|
| `auth/oidc.go` | refresh-lock map + `refreshLockFor` + DCL trong `RefreshToken` (cover sub-path `refreshOIDCToken`/`refreshSocialToken`) |
| `proxy/kiro_api.go` | `RefreshAccountInfo`: sweep classifier → `pool.IsAuthFailure`/`IsSuspensionError` + `pool.DisableAccount` |
| `proxy/kiro.go` | `CallKiroAPI`: classify theo `resp.StatusCode` thật |
| `auth/*_test.go`, `proxy/*_test.go` | 4 test mới |
| `config/config.go` (nếu cần) | `GetAccountByID(id) *Account` trả copy — chỉ thêm nếu chưa có accessor per-id cho DCL re-check |
| `docs/superpowers/specs/2026-06-28-auth-upstream-reliability-design.md` | spec này |

## 8. Rủi ro

- **B1 DCL re-read source:** phải đọc từ config (persisted source of truth), KHÔNG phải copy local của caller — nếu không follower sẽ re-POST. Mitigation: re-read qua accessor config.
- **B1 failure-case no-collapse:** bounded churn; singleflight (A2) là upgrade path.
- **B2 behavior shift:** edge error bị reclassify — cải thiện có chủ đích, nhưng phải chạy full proxy test suite + check thủ công symptom account-BANNED không tái diễn sai.
- **Per-account lock map leak:** account ID là UUID ổn định, set bounded; không dọn dẹp, negligible.
