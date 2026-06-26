# Changelog

All notable changes to this project are documented here. The format loosely
follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added
- **Import Microsoft 365 / Entra ID (Azure AD) credentials from the login helper.**
  Three converging paths now load a `CLIProxyAPI_*.json` file produced by
  `kiro-login-helper.py`, all funnelling through one `importOne` core so the
  persisted account is identical to an interactive Enterprise SSO login:
  - `apiImportCredentials` (`POST /admin/api/auth/credentials`) now understands the
    `external_idp` auth method and accepts the helper's native snake_case keys
    (`token_endpoint`, `issuer_url`, `scopes`, `profile_arn`) in addition to the
    existing camelCase payload.
  - New `POST /admin/api/auth/import-cli-json` endpoint ingests a single helper
    object, a JSON array, a `{ "files": [...] }` / `{ "accounts": [...] }` wrapper,
    or raw text with several objects, returning per-item results.
- `KIRO_IMPORT_WATCH` / `KIRO_IMPORT_DIR` zero-touch drop-folder watcher
  (`data/imports/`): valid files are imported within ~15s through `config.AddAccount`
  then moved to `processed/`, invalid ones to `failed/` with a `.error.txt` sidecar.
  Enabled by default in `docker-compose.yml`.
- Admin panel file picker for uploading helper JSON; `app.js` credential parsing now
  maps both snake_case and camelCase.
- `testdata/CLIProxyAPI_sample_external_idp.json` sanitized fixture.

### Fixed
- `external_idp` imports previously returned `400 "external IdP refresh requires
  clientId and tokenEndpoint"` because the import endpoint dropped `tokenEndpoint`,
  `issuerUrl`, `scopes`, and `profileArn`. The refresh-before-import step now carries
  the external IdP material so the refresh against the IdP token endpoint succeeds,
  and an `external_idp` import that omits `token_endpoint`/`client_id` is rejected
  up front with an actionable message instead of an opaque refresh failure.

### Security
- The account email is stored as a label only; the password is never persisted or
  sent upstream. Microsoft 365 tenants enforce MFA / Conditional Access, so a headless
  ROPC password grant is not a supported auth path — the interactive helper (browser
  PKCE) remains the canonical credential-mint path.
