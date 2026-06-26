# Plan: Import Kiro Login Helper JSON + Enterprise SSO (M365 / Entra ID) credentials

Target repo: `/home/harry-riddle/dev/github.com/0xharryriddle/Kiro-Go`
Branch (current): `feat/import-json-for-azure-tenant-sso`
Author of upstream SSO work referenced: PR https://github.com/Quorinex/Kiro-Go/pull/131

---

## 0. Problem statement (verified, not assumed)

You can already complete an interactive Enterprise SSO browser login (PR #131 is merged
into this fork: `auth/kiro_sso.go`, `/auth/kiro-sso/{start,poll,cancel}` routes exist).
What you CANNOT do today is import a credential that the standalone helper
(`/home/harry-riddle/dev/kiro-login-helper/kiro-login-helper.py`) already produced as a
`CLIProxyAPI_<username>.json` file, by calling the proxy's API.

Root cause (read from source, not guessed):

1. The helper writes an **external_idp** JSON with snake_case keys:
   `access_token, auth_method, client_id, refresh_token, region, profile_arn,
    token_endpoint, issuer_url, scopes, expired, timestamp, type`.
   (See `kiro-login-helper.py:603-631` `build_auth_json`.)

2. The import endpoint `apiImportCredentials` (`proxy/handler.go:3008-3109`) only decodes:
   `accessToken, refreshToken, clientId, clientSecret, authMethod, provider, region`.
   It DROPS `tokenEndpoint`, `issuerUrl`, `scopes`, `profileArn` — and reads camelCase only.

3. Import is gated on a mandatory refresh (`handler.go:3065`
   `auth.RefreshToken(tempAccount)`). For `external_idp`, `RefreshToken` routes to
   `refreshExternalIdpToken` (`auth/oidc.go:39-40,54-57`), which **hard-fails** unless
   BOTH `clientId` AND `tokenEndpoint` are present:
   `"external IdP refresh requires clientId and tokenEndpoint"`.
   Because the endpoint never passes `tokenEndpoint`, every external_idp import 400s.

4. The web UI parsers (`web/app.js` `importCredentials():2281` and
   `importLocalKiro():2250`) only map camelCase keys, so pasting the helper file
   verbatim maps nothing useful for the Azure path.

5. Email/password: an M365 tenant credential cannot be refreshed headlessly from a
   password. Azure AD's ROPC (password) grant is disabled for most tenants, breaks under
   MFA / Conditional Access, and is why the helper uses interactive browser PKCE in the
   first place. So **password is metadata, not an auth input** for the reliable path.
   We will store email as the account label and treat password as optional/no-op (with a
   clearly-flagged, opt-in best-effort ROPC attempt only — see §6).

The canonical persisted shape for an external_idp account already exists at
`apiPollKiroSso` (`handler.go:2891-2907`): it sets
`AuthMethod="external_idp", ClientID, TokenEndpoint, IssuerURL, Scopes, ProfileArn,
 Region, ExpiresAt`. Our import must produce an **identical** account so the pool,
refresh path, and `TokenType: EXTERNAL_IDP` header injection all work the same way.

---

## 1. Goal & deliverables

Make it trivial and repeatable to load M365/Azure (external_idp) credentials produced by
the login helper, three ways, all converging on the same account shape:

- **D1 — Backend import upgrade.** `apiImportCredentials` understands `external_idp`
  (tokenEndpoint/issuerUrl/scopes/profileArn) and accepts snake_case aliases, so the raw
  helper JSON works.
- **D2 — Dedicated helper-JSON endpoint** `POST /auth/import-cli-json` that ingests one or
  many `CLIProxyAPI_*.json` documents (array or newline/`---`-separated), normalizes them,
  and imports. Returns per-item success/error.
- **D3 — Zero-touch auto-ingest.** A watched directory (default `data/imports/`) scanned at
  startup and on an interval; valid files are imported then moved to
  `data/imports/processed/` (or `data/imports/failed/` with an error sidecar). Lets you
  `docker cp` / volume-mount helper output and have it appear automatically.
- **D4 — Web UI.** File picker on the existing "Enterprise SSO" / "Local Kiro" card to
  upload the helper JSON; snake_case mapping in `app.js`.
- **D5 — Email/password.** Stored as account label (`Email`, optional `Nickname`).
  Optional, off-by-default ROPC best-effort behind an explicit flag, clearly documented as
  likely to fail under MFA.
- **D6 — Tests** for every backend path + a sample helper JSON fixture.
- **D7 — Docs** (`README` section + `docs/` note) and CHANGELOG entry.

Out of scope: changing the interactive `/auth/kiro-sso/*` flow (it already works);
changing refresh logic in `auth/oidc.go` (it already handles external_idp correctly).

---

## 2. Files to touch (grounded in current tree)

| File | Change |
|------|--------|
| `proxy/handler.go` | Upgrade `apiImportCredentials` (3008-3109); add `apiImportCliJson`; register route `/auth/import-cli-json` near 2222. |
| `proxy/cli_json_import.go` (NEW) | Pure helper: parse + normalize helper JSON → `importCredentialRequest`. Unit-testable without HTTP. |
| `proxy/import_watcher.go` (NEW) | Directory watcher: scan, import, move to processed/failed. Started from `NewHandler()`. |
| `proxy/handler.go` (`NewHandler`) | Kick off the watcher goroutine (guard with env/config toggle). |
| `config/config.go` | Add `ImportWatchDir` + `ImportWatchEnabled` (or env-only) if we want it configurable; reuse `Account` fields (already have TokenEndpoint/IssuerURL/Scopes). |
| `web/app.js` | `importLocalKiro()` / `importCredentials()`: map snake_case + external_idp; add file input for helper JSON. |
| `web/locales/en.json`, `web/locales/zh.json` | Strings for the new upload control + toasts. |
| `proxy/cli_json_import_test.go` (NEW) | Table tests for the normalizer. |
| `proxy/import_credentials_test.go` | Add external_idp happy-path + missing-tokenEndpoint rejection cases. |
| `proxy/import_watcher_test.go` (NEW) | Temp-dir scan → import → file moved assertions (with stubbed RefreshToken). |
| `docs/`, `README.md`, `CHANGELOG.md` | Document the three import paths. |
| `testdata/CLIProxyAPI_sample_external_idp.json` (NEW) | Sanitized fixture mirroring the helper output. |

---

## 3. Canonical internal request shape

Define one normalized struct that every path funnels into (so there is a single import
code path, mirroring `apiPollKiroSso`'s account assembly):

```go
type importCredentialRequest struct {
    AccessToken   string
    RefreshToken  string
    ClientID      string
    ClientSecret  string
    AuthMethod    string // "idc" | "social" | "external_idp"
    Provider      string // e.g. "AzureAD"
    Region        string
    // external_idp refresh material:
    TokenEndpoint string
    IssuerURL     string
    Scopes        string
    ProfileArn    string
    // label-only:
    Email         string
    Nickname      string
}
```

Mapping rules (snake_case OR camelCase accepted; snake_case wins if both present because
that is the helper's native format):

- `access_token|accessToken` → AccessToken
- `refresh_token|refreshToken` → RefreshToken
- `client_id|clientId` → ClientID
- `client_secret|clientSecret` → ClientSecret
- `auth_method|authMethod` → AuthMethod (normalized, see below)
- `token_endpoint|tokenEndpoint` → TokenEndpoint
- `issuer_url|issuerUrl` → IssuerURL
- `scopes` → Scopes
- `profile_arn|profileArn` → ProfileArn
- `region` → Region (default `us-east-1` when empty)
- `email|preferred_username|upn` → Email
- helper top-level `type:"kiro"` is validated but ignored otherwise.

AuthMethod normalization (extend the existing switch at `handler.go:3042-3053`):

```text
"external_idp", "azure", "azuread", "entra", "entraid", "m365", "microsoft365",
"microsoft"  -> "external_idp"
"idc", "builderid", "enterprise"                 -> "idc"
"social", "google", "github"                     -> "social"
default: if TokenEndpoint != "" && ClientID != "" -> "external_idp"
         else if ClientID != "" && ClientSecret != "" -> "idc"
         else -> "social"
```

Provider default: `external_idp` with empty Provider → `"AzureAD"` (matches the screenshot
account you imported manually and the PR's UI label).

---

## 4. D1 — Backend `apiImportCredentials` upgrade

Edit `proxy/handler.go:3008-3109`.

1. Extend the decoded request struct with snake_case+camelCase via a permissive decode.
   Cleanest: decode into `map[string]json.RawMessage` OR add both-tag fields. Recommended:
   add a small `decodeImportRequest(r.Body) (importCredentialRequest, error)` in
   `cli_json_import.go` that reads both casings. Keep the HTTP handler thin.

2. Preconditions:
   - RefreshToken required (keep existing 400).
   - If AuthMethod resolves to `external_idp`: require `TokenEndpoint` AND `ClientID`
     (return a clear 400 "external_idp import requires token_endpoint and client_id"
     BEFORE calling RefreshToken, so the error is actionable rather than the opaque
     refresh failure).

3. Build `tempAccount` including the external_idp fields so the mandatory refresh works:

```go
tempAccount := &config.Account{
    RefreshToken:  req.RefreshToken,
    ClientID:      req.ClientID,
    ClientSecret:  req.ClientSecret,
    AuthMethod:    req.AuthMethod,
    Region:        req.Region,
    TokenEndpoint: req.TokenEndpoint, // NEW - unblocks refreshExternalIdpToken
    IssuerURL:     req.IssuerURL,     // NEW
    Scopes:        req.Scopes,        // NEW
}
accessToken, newRefreshToken, expiresAt, newProfileArn, err := auth.RefreshToken(tempAccount)
```

4. Persist the full external_idp account (mirror `apiPollKiroSso:2891-2907`):

```go
account := config.Account{
    ID:            auth.GenerateAccountID(),
    Email:         emailOrFromToken,
    Nickname:      req.Nickname,
    AccessToken:   accessToken,
    RefreshToken:  req.RefreshToken, // already rotated if upstream returned one
    ClientID:      req.ClientID,
    ClientSecret:  req.ClientSecret,
    AuthMethod:    req.AuthMethod,
    Provider:      providerWithDefault(req),
    Region:        req.Region,
    TokenEndpoint: req.TokenEndpoint, // NEW
    IssuerURL:     req.IssuerURL,     // NEW
    Scopes:        req.Scopes,        // NEW
    ProfileArn:    pickProfileArn(newProfileArn, req.ProfileArn), // prefer freshly-resolved, else helper's
    ExpiresAt:     expiresAt,
    Enabled:       true,
    MachineId:     config.GenerateMachineId(),
}
```

   `pickProfileArn`: external_idp refresh returns "" for profileArn (by design,
   `auth/oidc.go:75`), so fall back to the helper-provided `profile_arn`. If both empty,
   leave empty — `ResolveProfileArn`/`resolveProfileArnAcrossRegions`
   (`proxy/kiro_api.go:256,398`) will discover it lazily on first use, including the
   cross-region probe gated to external_idp accounts (`shouldProbeFallbackRegions:140-148`).

5. Email: prefer the request `email`; if empty, keep the existing
   `auth.GetUserInfo(accessToken)` best-effort (`handler.go:3076`).

Why this is safe: the refresh-before-import invariant is preserved (the comment at
`handler.go:3055-3057` explains why a working refresh is mandatory — a bogus short TTL
makes the pool skip the account forever). We are only giving the external_idp branch the
inputs it needs to actually succeed.

---

## 5. D2 — `POST /auth/import-cli-json`

New handler `apiImportCliJson` + route registration next to the other `/auth/*` cases
(`handler.go:2220-2223`). Admin-password protected like all `/admin/api/*` (handled by
`handleAdminAPI:2147`).

Accept body either:
- a single helper JSON object,
- a JSON array of them,
- or raw text with multiple objects separated by blank lines (lenient),

plus an optional wrapper `{ "files": ["<json string>", ...], "email": "...", "password": "..." }`
so the UI can post file contents directly.

Pipeline per item:
1. `normalizeCliJson(raw) -> importCredentialRequest` (in `cli_json_import.go`).
2. Validate `type == "kiro"` (warn-only) and required fields per auth method.
3. Reuse the **same** import core as D1 (extract the body of `apiImportCredentials` into
   `func (h *Handler) importOne(req importCredentialRequest) (config.Account, error)` and
   call it from both handlers — single source of truth).
4. Collect `{file/index, accountId, email, error}`.

Response: `{ "success": bool, "imported": [...], "errors": [...] }`, 200 when >=1 ok,
500 when all fail (match the batch convention in `apiImportSsoToken:2992-3005`).

Call `h.pool.Reload()` once after the batch.

---

## 6. D5 — Email / password handling (honest design)

- `email` → `Account.Email` (label) when the JSON lacks an email claim. Also used as
  `Nickname` if you want a friendly admin label.
- `password`:
  - Default behavior: **ignored** for auth, never persisted. (We will NOT store the
    plaintext password in `config.json`.)
  - Optional best-effort: behind env `KIRO_ALLOW_ROPC=1` AND only when a refresh_token is
    absent, attempt Azure AD ROPC (`grant_type=password`) against the tenant token
    endpoint to bootstrap tokens. This is explicitly documented as: fails under MFA /
    Conditional Access, disabled on most tenants, and not the recommended path. If it
    fails, return a clear message pointing back to the interactive helper.
  - Recommendation in docs: use the helper (browser PKCE) to mint the JSON, then import the
    JSON. Password is a convenience label only.

Decision needed from you (see §11): implement ROPC best-effort, or drop password entirely
as a label. Default in this plan: **drop to label-only**, add ROPC only if you ask.

---

## 7. D3 — Zero-touch auto-ingest watcher

`proxy/import_watcher.go`:

- Config: dir from `KIRO_IMPORT_DIR` env (default `data/imports/`), toggle via
  `KIRO_IMPORT_WATCH=1` (default ON if the dir exists, to be conservative).
- On `NewHandler()` start a goroutine:
  - Initial scan, then `time.Ticker` every N seconds (default 15s) — simple + dependency
    free (no fsnotify needed; the volume is tiny).
  - For each `*.json` not in `processed/`/`failed/`: read, `normalizeCliJson`, `importOne`.
  - On success: move file to `data/imports/processed/<name>` and log
    `[Import] added <email> from <file>`.
  - On failure: move to `data/imports/failed/<name>` and write `<name>.error.txt` with the
    reason. Never loop on the same broken file.
  - Skip partial writes: require the file mtime to be >2s old (helper writes atomically via
    `O_TRUNC` but the 2s guard avoids racing a `docker cp`).
- De-dupe: before import, check `config.GetAccounts()` for an existing account with the same
  `RefreshToken` or same `Email`+`AuthMethod`; skip with a logged note to avoid duplicates
  on repeated mounts.

Docker wiring (optional, document it): mount the helper output dir, e.g.
`./data/imports:/app/data/imports`, then any `CLIProxyAPI_*.json` dropped there is imported
within ~15s — no restart, no manual API call. This directly fixes the "config overwritten
on restart" pain you hit earlier, because the watcher imports through `config.AddAccount`
(the same persisted path the running server owns) instead of editing `config.json` under
the live process.

---

## 8. D4 — Web UI

In `web/app.js`:

1. `importLocalKiro()` (2250) and `importCredentials()` (2281): add snake_case mapping and
   external_idp fields so a pasted helper JSON works:

```js
function mapHelperCred(o) {
  return {
    accessToken:  o.accessToken  || o.access_token  || '',
    refreshToken: o.refreshToken || o.refresh_token || '',
    clientId:     o.clientId     || o.client_id     || '',
    clientSecret: o.clientSecret || o.client_secret || '',
    authMethod:   o.authMethod   || o.auth_method   || '',
    provider:     o.provider     || o.idp           || '',
    region:       o.region       || 'us-east-1',
    tokenEndpoint:o.tokenEndpoint|| o.token_endpoint|| '',
    issuerUrl:    o.issuerUrl    || o.issuer_url    || '',
    scopes:       o.scopes       || '',
    profileArn:   o.profileArn   || o.profile_arn   || '',
    email:        o.email        || o.preferred_username || ''
  };
}
```

2. Add a file `<input type="file" multiple accept=".json">` to the Enterprise/Local card
   that reads file text and POSTs to `/auth/import-cli-json` (D2). Reuse `loadLocalFile`
   pattern (2241).

3. Locales: add `local.helperJsonLabel`, `local.helperJsonHint`,
   `credentials.cliJsonSuccess`, etc. to `en.json` and `zh.json`.

---

## 9. D6 — Tests (must pass before commit)

- `proxy/cli_json_import_test.go`:
  - snake_case helper JSON → correct `importCredentialRequest` (external_idp).
  - camelCase still works.
  - authMethod inference (tokenEndpoint+clientId ⇒ external_idp; clientId+secret ⇒ idc; bare ⇒ social).
  - array + multi-object text parsing.
- `proxy/import_credentials_test.go` (extend existing):
  - external_idp happy path: stub `auth.SetOIDCTokenURLForTest` is for OIDC; for
    external_idp we hit `tokenEndpoint`, so stand up an `httptest` token server and pass its
    URL as `token_endpoint`; assert account persisted with TokenEndpoint/IssuerURL/Scopes
    and ExpiresAt from upstream `expires_in`.
  - external_idp missing token_endpoint ⇒ 400 with actionable message, no account stored.
- `proxy/import_watcher_test.go`:
  - temp dir + one valid file (token endpoint = httptest) ⇒ account added, file moved to
    `processed/`.
  - one invalid file ⇒ moved to `failed/` + `.error.txt`, no account.
  - duplicate refreshToken ⇒ skipped.
- Reuse `installCleanAuthClient` (import_credentials_test.go:21) to avoid proxy-env caching.

Run: `cd <repo> && go build ./... && go test ./proxy/... ./auth/... ./config/...`.

---

## 10. Step-by-step execution order

1. Create `testdata/CLIProxyAPI_sample_external_idp.json` (sanitized).
2. Add `proxy/cli_json_import.go` (`normalizeCliJson`, `decodeImportRequest`,
   `mapHelper*` helpers) + table tests; `go test ./proxy/ -run CliJson`.
3. Refactor `apiImportCredentials` to call a shared `importOne`; wire external_idp fields
   (D1). Extend `import_credentials_test.go`; run tests.
4. Add `apiImportCliJson` + route (D2); test via httptest.
5. Add `import_watcher.go` + start in `NewHandler`; add watcher test (D3).
6. Wire `web/app.js` + locales (D4).
7. Email/password label-only (D5); ROPC only if approved.
8. `go build ./... && go vet ./... && go test ./...`.
9. Docs + CHANGELOG (D7).
10. Manual end-to-end: run the helper for `britta.huotari@codezdev-cn.cc`, drop the JSON in
    `data/imports/`, confirm the account appears via
    `GET /admin/api/accounts` and `GET /admin/api/status` (available count +1) without a
    container restart.
11. Commit on `feat/import-json-for-azure-tenant-sso`, push to `origin` (your fork).

---

## 11. Decisions (chosen from the live-import evidence — confirm or override)

These are decided, not open. Each is grounded in what actually worked when the
`britta.huotari@codezdev-cn.cc` credential was imported into the running container. Review
them; override any you disagree with before implementation starts.

### D-1. Password handling → **label-only. No ROPC.** (CHOSEN)
Evidence: the live import never touched the email/password once. The working credential came
entirely from the helper's `external_idp` JSON (access/refresh + `token_endpoint`/`issuer_url`/
`scopes`/`profile_arn`). Azure enterprise tenants enforce MFA / Conditional Access, which makes
the OAuth2 ROPC password grant fail by policy in the common case — building it would add a code
path that mostly returns errors and tempts persisting a plaintext password we never need.
- The email is stored as the account label only (`Account.Email`).
- The password is **never persisted** and never sent upstream.
- The interactive `kiro-login-helper.py` stays the canonical mint path; this feature only
  *ingests* what it produces.
- `KIRO_ALLOW_ROPC` is **not** built. (If you ever want it, it's a clean, isolated follow-up.)

### D-2. Watcher default → **opt-in, but auto-on inside Docker.** (CHOSEN)
Evidence: the one thing that broke last time was a background actor (the running app) silently
rewriting `config.json`. The lesson is *no surprising background file mutation*. So the watcher
is **off by default** on a bare `go run` / local dev, and **on inside the container** where
zero-touch is the actual goal:
- Gate: watcher starts only when `KIRO_IMPORT_WATCH=1` (or `=true`).
- `docker-compose.yml` sets `KIRO_IMPORT_WATCH=1` and mounts `./data/imports:/app/data/imports`,
  so `docker compose up -d` gives you the drop-a-file flow with no extra step.
- A plain local build does nothing unexpected — matches the strict-scoping expectation.
- Safe by construction: the watcher imports through `config.AddAccount` (the path the live
  server owns), so it can never race the in-memory config the way the manual `config.json`
  edit did. That is the structural fix for the exact failure hit earlier.

### D-3. Branch scope → **ship D1+D2 first, then D3+D4 as a follow-up commit.** (CHOSEN)
Evidence: the immediate, confirmed blocker is that `apiImportCredentials` 400s on external_idp
because it drops `tokenEndpoint` — that is *precisely* "I can't import the account by calling
you." D1 fixes that exact bug; D2 (`/auth/import-cli-json`) makes the helper file directly
ingestible. Land that minimal, verifiable slice first:
- **Commit 1 (this branch):** D1 (external_idp + snake_case in `apiImportCredentials`) + D2
  (`/auth/import-cli-json`) + D5 (label-only) + tests + sample fixture + docs. End-to-end
  verified by importing the real `britta` JSON via the API with no restart.
- **Commit 2 (same branch, after Commit 1 is green):** D3 (watcher, opt-in/Docker-on) + D4
  (web UI file picker). Each is additive and independently testable.

This keeps blast radius per commit small, gets the real fix in fastest, and means a failure in
the watcher work can't hold the API fix hostage.

---

## 12. Verification checklist (definition of done)

- [ ] `go build ./...` clean.
- [ ] `go test ./...` green, including new external_idp + watcher tests.
- [ ] Pasting raw helper JSON into the UI imports an `external_idp` account.
- [ ] `POST /auth/import-cli-json` with the helper file body returns success and the
      account shows `authMethod=external_idp`, correct `tokenEndpoint/issuerUrl/scopes`.
- [ ] Dropping a file in `data/imports/` auto-imports within ~15s; file moves to
      `processed/`; duplicate drop is skipped.
- [ ] Imported account survives a container restart (persisted via `config.AddAccount`,
      not a racy direct config.json edit).
- [ ] No plaintext password persisted in `config.json`.
- [ ] README + CHANGELOG updated; docs describe all three import paths and the Docker mount.
