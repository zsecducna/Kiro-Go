# Kiro-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Convert Kiro accounts to OpenAI / Anthropic compatible API service.

[English](README.md) | [中文](README_CN.md)

If this project helps you, a Star would mean a lot.

## Features

- Anthropic `/v1/messages` & OpenAI `/v1/chat/completions`
- Multi-account pool with round-robin load balancing
- Auto token refresh, SSE streaming, Web admin panel
- Multiple auth: AWS Builder ID, IAM Identity Center (Enterprise SSO), SSO Token, local cache, credentials JSON
- Usage tracking, account import/export, i18n (CN / EN)
- Support configuring outbound proxy (SOCKS5 / HTTP)

## Quick Start

### Docker Compose (Recommended)

```bash
git clone https://github.com/Quorinex/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker-compose up -d
```

### Docker Run

```bash
docker run -d \
  --name kiro-go \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/quorinex/kiro-go:latest
```

### Build from Source

```bash
git clone https://github.com/Quorinex/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go

# Run on a different port (flag > PORT env > config.json):
./kiro-go -port 9090
# or: PORT=9090 ./kiro-go
```

### Deploy on Zeabur

The repo already includes a `Dockerfile`, so it builds and runs on Zeabur out of the box.

**Option 1: Dashboard (one-click)**

1. Fork this repo to your GitHub account.
2. In Zeabur, create a new service and choose **Deploy from GitHub**, then select your fork.
3. Zeabur auto-detects the `Dockerfile` and builds the image.
4. In the **Networking** tab, expose port `8080` and bind a domain.
5. In the **Variables** tab, set at least `ADMIN_PASSWORD` (admin panel password).
6. Mount a Volume at `/app/data` if you want accounts / config to survive redeploys.

**Option 2: CLI**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> Run the commands from the project root. The CLI writes `.zeabur/context.json` to remember the target project / service — it contains personal IDs, so don't commit it.

Once the service is up, open `https://<your-domain>/admin` to log in.

Config is auto-created at `data/config.json`. Mount `/app/data` for persistence. The default admin password is `changeme` — override it via the `ADMIN_PASSWORD` env var or change it in the admin panel before going to production.

## Usage

Open `http://localhost:8080/admin`, log in, add accounts, then call the API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'
```

## Thinking Mode

Append a suffix (default `-thinking`) to the model name, e.g. `claude-sonnet-4.5-thinking`. Claude-compatible requests that include a top-level `thinking` config such as `{"type":"enabled","budget_tokens":2048}` or `{"type":"adaptive"}` also enable thinking mode automatically. Configure output format in the admin panel under Settings - Thinking Mode.

## Outbound Proxy

For users in restricted network regions, configure an outbound proxy in the admin panel under **Settings - Outbound Proxy Settings**. Supports SOCKS5 and HTTP proxies.

The setting takes effect immediately without restarting.

## Importing Microsoft 365 / Entra ID (Azure AD) credentials

Enterprise SSO (Microsoft 365 / Entra ID) accounts are neither AWS Builder ID nor
IAM Identity Center accounts, so they are minted through the interactive browser
sign-in helper `kiro-login-helper.py`, which writes a `CLIProxyAPI_<user>.json`
credential file (`auth_method: external_idp`). There are three ways to load that file:

1. **Paste / upload in the admin panel.** Add Account → Credentials JSON (or the
   Enterprise SSO card's file picker) accepts the helper's native `CLIProxyAPI_*.json`
   verbatim — snake_case keys (`token_endpoint`, `issuer_url`, `scopes`, `profile_arn`)
   are understood.

2. **API.** `POST /admin/api/auth/import-cli-json` accepts a single helper object, a
   JSON array, a `{ "files": ["<json>", ...] }` / `{ "accounts": [...] }` wrapper, or
   raw text with several objects. It returns per-item results.

   ```bash
   curl -X POST http://localhost:8080/admin/api/auth/import-cli-json \
     -H "X-Admin-Password: $ADMIN_PASSWORD" \
     --data-binary @CLIProxyAPI_user.json
   ```

3. **Zero-touch drop folder (Docker).** With `KIRO_IMPORT_WATCH=1` (set by default in
   `docker-compose.yml`), any `CLIProxyAPI_*.json` placed in `data/imports/` is imported
   within ~15s, then moved to `data/imports/processed/` (or `failed/` with a `.error.txt`
   sidecar). Imports go through the same persisted path the running server owns, so they
   never race the in-memory config.

4. **Import from the Kiro IDE cache (no browser, no helper).** If the Kiro IDE is already
   signed in on the same host as the proxy, it keeps a live credential at
   `~/.aws/sso/cache/kiro-auth-token.json`. The admin panel's Enterprise SSO card has an
   **Import from Kiro IDE (this host)** button, or call the API directly:

   ```bash
   curl -X POST http://localhost:8080/admin/api/auth/import-ide-cache \
     -H "X-Admin-Password: $ADMIN_PASSWORD"
   # custom location: -d '{"path":"/path/to/kiro-auth-token.json"}'
   ```

   The proxy reads the file **server-side**, so this works only when the IDE and the proxy
   share a host — or, in Docker, when that file is mounted into the container and
   `KIRO_IDE_CACHE` points at it. The cache's stale `expiresAt` is ignored: the import
   performs a mandatory refresh, so the persisted expiry always comes from a fresh upstream
   response.

> The account email is stored as a label only. The password is **never** persisted or
> sent upstream — Microsoft 365 tenants enforce MFA / Conditional Access, so a headless
> password (ROPC) grant is not a reliable auth path. Use the interactive helper to mint
> the credential, then import the JSON.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `data/config.json` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config) | - |
| `PORT` | HTTP listen port (overrides config; `-port` flag wins over this) | `8080` |
| `HOST` | HTTP bind host (overrides config; `-host` flag wins over this) | `127.0.0.1` |
| `KIRO_IMPORT_WATCH` | Enable the `data/imports/` auto-ingest watcher (`1`/`true`) | off (on in Docker) |
| `KIRO_IMPORT_DIR` | Directory the watcher scans for `CLIProxyAPI_*.json` | `data/imports` |
| `KIRO_IDE_CACHE` | Path to the Kiro IDE credential cache for `import-ide-cache` | `~/.aws/sso/cache/kiro-auth-token.json` |
| `KIRO_PROFILE_REGIONS` | Comma-separated fallback regions for external_idp profile probing | `us-east-1,eu-central-1` |

## Contributing

Friendly discussion is welcome. If you run into issues, try asking Claude Code, Codex, or similar tools for help first — most problems can be solved that way. PRs are even better.

## Friend Links

- [LINUX DO](https://linux.do)

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE)
