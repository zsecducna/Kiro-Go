# Kiro-Go Pool Linking — "Custom API" Upstream Accounts

**Date:** 2026-07-16
**Status:** Approved design, pre-implementation

## Problem

Operator A runs a Kiro-Go pool and sells metered API keys. Operator B (a customer)
also runs a Kiro-Go pool. B wants to add A's key + A's pool URL as a *backend account*
in B's own pool, so B's downstream traffic is served by A's pool. This is upstream
chaining / reselling.

Today an `Account` only represents a direct Kiro credential (idc / social /
external_idp / api_key), and the runtime always translates incoming Anthropic/OpenAI
requests into Amazon CodeWhisperer calls against hardcoded AWS endpoints
(`proxy/kiro.go`). There is no way to point an account at an arbitrary base URL that
already speaks the Anthropic/OpenAI API.

## Goal

A new account type — **Custom API** (`AuthMethod == "custom_api"`) — that:

1. Is added through an admin flow analogous to "add account by API key", with
   pre-add validation (quota, order id, tags).
2. At runtime, **transparently forwards** the original request to the upstream
   pool instead of translating to CodeWhisperer.
3. Participates in the existing pool: model routing, failover, circuit breaker,
   stats, and per-key metering.

## Non-Goals

- No re-translation through the Kiro pipeline (upstream speaks Anthropic/OpenAI already).
- No change to how direct Kiro accounts work.
- No multi-hop quota federation beyond a simple loop guard.

## Decisions (from brainstorming)

- **Scope:** full — add-account flow *and* runtime forwarding.
- **Forward style:** transparent passthrough of the original request body.
- **Data model:** new `AuthMethod = "custom_api"` with extra fields on `Account`.
- **Model scope:** all models by default (empty model list = optimistic routing).

## Design

### 1. Data model — `config/config.go`

Add to `Account`:

| Field | JSON | Purpose |
|-------|------|---------|
| `BaseURL string` | `baseUrl,omitempty` | Upstream pool root, e.g. `https://pool.example.com` (no trailing `/v1`). |
| `OrderID string` | `orderId,omitempty` | Order id. Required for custom_api. Also copied into `Nickname` for display. |
| `Tags []string` | `tags,omitempty` | Labels; custom_api accounts get `["Custom API"]`. |

- Reuse the existing `KiroApiKey` field to store the upstream bearer token — its
  documented role is already "used directly as the upstream bearer token, never
  refreshed", which is exactly what a custom_api account needs. No new secret field.
- `AuthMethod` gains the value `"custom_api"`. It is NOT one of the OAuth/idc
  methods, so token-refresh paths (`ensureValidToken`, region derivation, etc.)
  must early-return for it (no refresh, no expiry).

`BaseURL` normalization: trim trailing slashes; reject if not `http(s)://`.

### 2. Add-account endpoint — `proxy/admin_bot_api.go`

`POST /admin/add_custom_api_account`

Request body:
```json
{
  "baseUrl":  "https://pool.example.com",
  "apiKey":   "<upstream key>",
  "orderId":  "ORD-1234",
  "nickname": "optional; defaults to orderId",
  "tags":     ["optional extra tags"],
  "enabled":  true
}
```

**Checks before adding (in order):**

1. **Order ID** — required, non-empty. Stored as `OrderID` and (if `nickname`
   empty) as `Nickname`.
2. **Dedup** — reject (HTTP 409) if an existing account has the same `OrderID`,
   or the same `BaseURL` + `KiroApiKey` pair. Mirrors the api_key dedup logic at
   `admin_bot_api.go:639`.
3. **Quota** — `GET {baseUrl}/api/me` with header `Authorization: Bearer {apiKey}`
   (and `x-api-key: {apiKey}` for compatibility), 5s timeout. Accept when:
   - HTTP 200, and
   - `creditsRemaining > 0` **OR** `tokensRemaining > 0` **OR** both `creditLimit == 0`
     and `tokenLimit == 0` (treated as unlimited).
   Reject (HTTP 400/402) on non-200, 401 (bad key), or zero remaining quota, or
   unreachable host.
4. **Tags** — final tag set = `["Custom API"]` ∪ request `tags`.

On success build:
```go
config.Account{
    ID:         uuid,
    AuthMethod: "custom_api",
    BaseURL:    normalizedBaseURL,
    KiroApiKey: apiKey,
    OrderID:    orderId,
    Nickname:   nicknameOrOrderId,
    Tags:       tags,
    Enabled:    true,
}
```
and persist through the same account-add path used by `add_kiro_api_key`
(so config write + pool reload behave identically).

Mirror the flow into:
- Telegram bot command `/add_custom_api` (`proxy/admin_bot_api.go`).
- Web panel add-form (`web/app.js`, `web/index.html`).

### 3. Runtime dispatch — `proxy/handler.go` + new `proxy/custom_api_forward.go`

The 4 dispatch handlers (`handleClaudeStream`, `handleClaudeNonStream`, openai
stream, openai non-stream) currently: translate → select account in a retry loop →
`CallKiroAPI(account, payload, callback)`.

Change:
- Thread the raw request `body []byte` (already read at `handler.go:867` / `:1661`)
  down into each of the 4 handlers.
- Inside each retry loop, right after
  `account := h.pool.GetNextForModelWithApiKey(...)`:
  ```go
  if account.AuthMethod == "custom_api" {
      err := h.forwardToUpstream(account, body, endpointKind, streaming, w, flusher, apiKeyID)
      // success → stop; error → handleAccountFailure + exclude + continue (failover)
  }
  ```

**`forwardToUpstream` (new file `proxy/custom_api_forward.go`):**
- URL = `account.BaseURL` + path, where path = `/v1/messages` for Anthropic,
  `/v1/chat/completions` for OpenAI (chosen by which handler called it).
- Headers: `Authorization: Bearer {KiroApiKey}`, `x-api-key: {KiroApiKey}`,
  `Content-Type: application/json`, `Accept: text/event-stream` when streaming,
  and `X-KiroGo-Forwarded: 1`.
- Body = raw incoming bytes, unchanged.
- **Loop guard:** if the *incoming* request already carries `X-KiroGo-Forwarded`,
  refuse to add a custom_api hop (return an error that triggers failover / 400) to
  prevent A→B→A cycles.
- Streaming: copy the upstream SSE stream straight to the client writer, flushing.
- Non-streaming: copy the upstream JSON body straight through.
- **Metering:** parse the upstream `usage` object (final `message_delta` / `[DONE]`
  for SSE, or the JSON `usage` for non-stream) → `pool.UpdateStats(id, tokens, credits)`,
  `pool.RecordSuccess(id)`, and the same per-key metering the Kiro path performs.
  If usage is absent, record request count only.
- **Errors:** upstream connect error / non-2xx → `h.handleAccountFailure(account, err)`,
  add to `excluded`, `continue` the loop so another account (custom_api or Kiro) can serve.

Because forwarding lives inside the existing retry loop, pool selection, circuit
breaker, and failover are reused with no new machinery.

### 4. Model routing

Custom API accounts are added with no explicit model list. `accountHasModel`
(`pool/account.go:221`) treats an empty list as "supports all models" (optimistic),
so a custom_api account is eligible for every model by default. No extra work.

### 5. UI surfaces

- **Web panel:** new "Add Custom API account" form (baseUrl, apiKey, orderId, tags);
  account list shows a "Custom API" badge, masked key, orderId, baseURL.
- **Telegram bot:** `/add_custom_api` command mirroring the endpoint.
- **`/admin/pool` census:** already enumerates accounts; custom_api ones appear
  tagged, no change needed beyond including the tag/type in the row.

### 6. Error handling summary

| Situation | Behavior |
|-----------|----------|
| Add: bad/expired upstream key | `/api/me` non-200 → reject add (400/402). |
| Add: zero quota | reject add (402). |
| Add: duplicate orderId / baseURL+key | reject add (409). |
| Add: upstream unreachable | reject add (400, "quota check failed"). |
| Runtime: upstream 4xx/5xx | RecordError + failover to next account. |
| Runtime: forward loop detected | refuse hop, failover / 400. |
| Runtime: no usage in response | count request, zero tokens/credits. |

## Testing

New tests (httptest upstream server), following `proxy/customer_admin_api_test.go`
and `proxy/kiro_apikey_test.go` patterns:

1. `/admin/add_custom_api_account` — quota-check accept path (upstream `/api/me`
   returns remaining quota).
2. Add reject: upstream 401, zero quota, unreachable.
3. Add reject: duplicate orderId; duplicate baseURL+key.
4. Add: tags default to `["Custom API"]` and merge extras; nickname defaults to orderId.
5. Forward passthrough: streaming SSE echoed byte-for-byte to client.
6. Forward passthrough: non-streaming JSON echoed.
7. Metering: upstream usage parsed → pool + per-key stats updated.
8. Failover: upstream 500 → account excluded, next account serves.
9. Loop guard: incoming `X-KiroGo-Forwarded` header → hop refused.

## Files touched

- `config/config.go` — Account fields.
- `proxy/admin_bot_api.go` — add endpoint + Telegram command + dedup/quota helpers.
- `proxy/handler.go` — thread `body`, fork to forward in 4 dispatch loops.
- `proxy/custom_api_forward.go` — new: `forwardToUpstream`, usage tap, loop guard.
- `web/app.js`, `web/index.html` — add form + list badge.
- `proxy/custom_api_forward_test.go`, `proxy/admin_custom_api_test.go` — tests.

## Contract stability

`/api/*` and `/admin/*` contracts stay backward compatible — this only *adds*
`/admin/add_custom_api_account` and new optional Account JSON fields. Existing keys,
accounts, and the resale business contract are unaffected.
