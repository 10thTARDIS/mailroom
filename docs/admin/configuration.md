<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Configuration reference

Every environment variable and secret file that Mailroom reads, with its default.

**On this page:** [How to set things](#how-to-set-things) ·
[Secret files](#secret-files) · [Server and logging](#server-and-logging) ·
[Database](#database) · [Storage](#storage) · [Sign-in](#sign-in) ·
[Mail and sync](#mail-and-sync) · [Sending](#sending) ·
[Attachments and ClamAV](#attachments-and-clamav) · [Image proxy](#image-proxy) ·
[AI tools and MCP](#ai-tools-and-mcp) · [Metrics](#metrics) · [Limits](#limits) ·
[Retention](#retention) · [Container memory limits](#container-memory-limits)

## How to set things

- Put non-secret values in `deploy/.env` (copy `deploy/.env.example`). Compose reads it
  automatically. An unset or empty value means "use the default shown below".
- Secrets are files under `deploy/secrets/`, mounted at `/run/secrets/`. Never put a
  secret in `.env`: it ends up in the container environment, where `docker inspect` shows it.
- Settings that take a secret accept `NAME_FILE` (a path) as well as `NAME`. The shipped
  compose file uses the `_FILE` form for the secrets listed below.
- Booleans accept `1`, `true`, `yes`, `on`, `0`, `false`, `no`, `off`, in any case.
  **Any other value stops the server at boot**, naming the variable.
- Durations are Go durations (`30s`, `5m`, `720h`). A malformed value stops the server at boot.
- The server only starts with exactly one usable sign-in path. See [authentication](authentication.md).

## Secret files

All must exist before the first `docker compose up`, even unused ones. An empty file is fine; a
missing one is not (see [troubleshooting](troubleshooting.md#a-secrets-path-became-a-directory)).

| File in `deploy/secrets/` | Held by | Contents |
|---|---|---|
| `postgres_password.txt` | postgres | Postgres superuser password (48 bytes). |
| `mailroom_owner_password.txt` | migrate | Password of the schema owner role `mailroom_owner` (48 bytes). |
| `mailroom_app_password.txt` | mailroom, crypto | Password of the `mailroom_app` role (48 bytes). |
| `mailroom_master_key.txt` | mailroom | Master key, 64 hex characters. Losing it loses the whole mailbox. |
| `mailroom_pgp_root_key.txt` | crypto only | PGP root, 64 hex characters. A second, independent root. |
| `mailroom_session_key.txt` | mailroom | Session secret, at least 32 bytes (64 hex characters as generated). |
| `mailroom_oidc_client_secret.txt` | mailroom | Your sign-in provider's client secret. Empty without OIDC. |
| `accounts.json` | mailroom | Declared mail accounts. See [accounts](accounts.md). |

Generation commands are in the [install guide](../../deploy/README.md#3-generate-the-secrets).
Escrow both roots: [key escrow](../key-escrow.md).

## Server and logging

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_IMAGE` | `mailroom:test` | Image for `mailroom`, `migrate` and `crypto`. Set an exact version by digest. Never `:latest`. |
| `MAILROOM_HTTP_PORT` | `8080` | Host port published for the web app. `deploy/.env.example` sets `8090`. |
| `MAILROOM_HTTP_BIND` | `127.0.0.1` | Host interface the port is published on. Set `0.0.0.0` only to reach it from another machine, with a firewall. |
| `MAILROOM_LISTEN_ADDR` | `:8080` | Listen address inside the container. Compose fixes it. |
| `MAILROOM_WEB_ROOT` | `/srv/web` | Where the built web client lives in the image. |
| `MAILROOM_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `MAILROOM_LOG_FORMAT` | `json` | `json` or `text`. Log greps must match the format in use. |
| `MAILROOM_TRUSTED_PROXIES` | empty (trust none) | Comma-separated IPs or CIDRs of your reverse proxies. Their `X-Forwarded-For` is used so the signed-in devices list shows real addresses (display only). A malformed entry stops startup. |
| `MAILROOM_PUBLIC_URL` | origin of `MAILROOM_OIDC_REDIRECT_URL` | The origin browsers reach Mailroom at. Used for OAuth callbacks and for the addresses given to mail apps. Behind an https proxy, make sure it (or the OIDC redirect URL) names the https address, or apps are given `http://` addresses. Required for OAuth under dev-auth. |

## Database

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_DATABASE_URL` | none (required) | Connection URL for the `mailroom_app` role. Compose sets it, without a password. |
| `MAILROOM_DATABASE_PASSWORD_FILE` | `/run/secrets/mailroom_app_password` in compose | Password file merged into the URL in memory. Setting this and a password in the URL is an error. |
| `MAILROOM_MIGRATE_DATABASE_URL` | set by compose | URL for the `migrate` job, which connects as `mailroom_owner`. |
| `MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE` | `/run/secrets/mailroom_owner_password` in compose | Password file for the migrate job. |

No password is ever written into a connection string on disk.

## Storage

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_MASTER_KEY_FILE` | `/run/secrets/mailroom_master_key` | Master key file. |
| `MAILROOM_BLOB_ROOT` | `/var/lib/mailroom/blobs` | Encrypted message store (`blobs` volume). Must be writable. |
| `MAILROOM_SEARCH_ROOT` | `/var/lib/mailroom/search` | Search index (`search` volume). A disposable cache holding plaintext. See [security](security.md#encrypt-the-volumes). |
| `MAILROOM_INDEX_INTERVAL` | `5s` | How often the indexer drains pending changes when idle. Positive. |
| `MAILROOM_SEARCH_DISABLED` | off | `1` turns the indexer and search off, so no plaintext index exists on disk. Text filters are then rejected. |

## Sign-in

Pick exactly one block. A partly filled OIDC block stops the server. See [authentication](authentication.md).

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_OIDC_ISSUER` | empty | Your provider's issuer URL. |
| `MAILROOM_OIDC_CLIENT_ID` | empty | Client id. |
| `MAILROOM_OIDC_CLIENT_SECRET_FILE` | `/run/secrets/mailroom_oidc_client_secret` | Client secret file. |
| `MAILROOM_OIDC_REDIRECT_URL` | empty | Callback URL registered at the provider. Its scheme decides `Secure` cookies and HSTS. |
| `MAILROOM_SESSION_KEY_FILE` | `/run/secrets/mailroom_session_key` | Session secret file. At least 32 bytes. |
| `MAILROOM_OPERATOR_SUBJECT` | empty | Optional. Provisions a boot-time user who owns the accounts file. Must equal the provider's `sub` exactly. |
| `MAILROOM_OPERATOR_EMAIL` | empty | Optional. Email for that user. |
| `MAILROOM_DEV_AUTH_SUBJECT` | empty | Dev-auth identity. **Turns authentication off.** |
| `MAILROOM_DEV_AUTH_EMAIL` | empty | Email for the dev-auth identity. |
| `MAILROOM_DEV_AUTH_I_UNDERSTAND` | empty | Must equal `this-instance-has-no-authentication` or dev-auth is refused. |
| `MAILROOM_OAUTH_GOOGLE_CLIENT_ID` | empty | Google OAuth client for connecting Gmail. Needs the secret too. |
| `MAILROOM_OAUTH_GOOGLE_CLIENT_SECRET_FILE` | empty | Path to the Google client secret file. |
| `MAILROOM_OAUTH_MICROSOFT_CLIENT_ID` | empty | Microsoft application id for connecting Outlook. Needs the secret too. |
| `MAILROOM_OAUTH_MICROSOFT_CLIENT_SECRET_FILE` | empty | Path to the Microsoft client secret file. |
| `MAILROOM_OAUTH_MICROSOFT_TENANT` | `common` | `consumers`, `organizations`, a tenant id or a verified domain narrow it. |

A provider is offered only when both its id and secret are set; one without the other stops the
server. The secret files need a `compose.override.yml` `secrets:` entry. See
[Gmail and Outlook](../oauth-providers.md#4-configure-mailroom).

## Mail and sync

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_ACCOUNTS_FILE` | `/run/secrets/mailroom_accounts` in compose | The declared accounts file. |
| `MAILROOM_SYNC_INTERVAL` | `5m` | How often each account is polled. Positive. |
| `MAILROOM_SYNC_MAX_BACKOFF` | `30m` | Cap on a failing account's retry backoff. |
| `MAILROOM_REAP_INTERVAL` | `1h` | How quickly mail deleted or moved in another client leaves Mailroom. `0` disables it, and then such mail never leaves its Mailroom folder. |
| `MAILROOM_IMAP_IDLE` | on | IMAP IDLE for new mail within seconds. Servers without it fall back to polling. |
| `MAILROOM_IMAP_IDLE_TIMEOUT` | `29m` | How often IDLE is re-issued. |
| `MAILROOM_IMAP_POOL` | on | Reuse one warm connection per account. |
| `MAILROOM_IMAP_POOL_IDLE_TIMEOUT` | `5m` | Evict a pooled connection unused this long. |
| `MAILROOM_IMAP_SEARCH_RATE` | `30` | `UID SEARCH` commands per minute per account. Half of a common provider limit, shared with your phone and webmail. `0` disables pacing. |
| `MAILROOM_FLAG_SWEEP_INTERVAL` | `5m` | For servers without CONDSTORE: how often the newest messages' flags are re-read. `0` disables. |
| `MAILROOM_FLAG_SWEEP_WINDOW` | `500` | Newest UIDs per folder re-read each sweep. Positive. |
| `MAILROOM_FLAG_SWEEP_FULL_INTERVAL` | `6h` | Slower whole-folder flag pass. `0` disables only this pass. |
| `MAILROOM_GMAIL_DAILY_DOWNLOAD_MB` | `2000` | Gmail download budget per 24 hours, below Google's 2,500 MB suspension limit. Positive. |
| `MAILROOM_GMAIL_LABEL_SWEEP_INTERVAL` | `6h` | How often Gmail labels are re-read. `0` disables. |
| `MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS` | off | Lets IMAP and SMTP hosts resolve to private, loopback or link-local addresses. Needed only when the mail server is on the same network. Off blocks port-scanning through account validation. |
| `MAILROOM_WRITEBACK_DISABLED` | off | Instance-wide ingest-only switch: nothing is written back to any provider and nothing is sent. See [accounts](accounts.md#ingest-only-mode). |
| `MAILROOM_AUTHSERV_IDS` | empty | Authentication-Results ids you trust. See [security](security.md#trust-sender-authentication-results). |
| `MAILROOM_LOCAL_DKIM` | on | Mailroom checks DKIM itself on mail with no trusted result. Needs outbound DNS. |
| `MAILROOM_WKD_DOMAIN` | empty (off) | Mail domain whose published PGP keys are served over WKD. See [PGP keys](pgp-keys.md). |
| `MAILROOM_PGP_SOCKET` | empty; compose sets `/run/mailroom-crypto/crypto.sock` | Crypto process socket. Unset disables signing. |
| `MAILROOM_PGP_ROOT_KEY_FILE` | `/run/secrets/mailroom_pgp_root_key` | Root key file, read by the `crypto` service only. |

## Sending

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_UNDO_SEND_WINDOW` | `10s` | How long a message waits cancelable before it goes out. `0` sends immediately. |

Sending is refused for any account with writeback off. See [accounts](accounts.md#ingest-only-mode).

## Attachments and ClamAV

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_CLAMD_SOCKET` | empty; compose sets `/run/clamav/clamd.sock` | Scanner socket. **Unset turns scanning off.** Set, a dead scanner pauses sync. |
| `MAILROOM_CLAMD_MAX_BYTES` | `104857600` | Largest attachment sent to the scanner. Must match `StreamMaxLength` in `deploy/clamd.conf`. |
| `MAILROOM_CLAMAV_IMAGE` | pinned `clamav/clamav` | Image for `clamd` and `clamav-signatures`. |

See [security](security.md#attachment-scanning-with-clamav).

## Image proxy

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_IMAGE_PROXY` | on | Remote images load through the server instead of the reader's browser. Off restores direct browser fetches. |
| `MAILROOM_IMAGE_PROXY_PREFETCH` | on | Fetch images from allowed senders when mail arrives. Tracking pixels are never fetched. No effect with the proxy off. |
| `MAILROOM_IMAGE_PROXY_PREFETCH_MAX_AGE` | `168h` | Mail older than this is never prefetched. Positive. |
| `MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES` | `17179869184` (16 GiB) | Per-user ceiling on kept images. At the ceiling, new mail's images load on open and a WARN appears at most hourly. Nothing is deleted. `0` or empty means the default, negative means no limit. |

Prefetch only runs for mail that passes the same authentication check as learned placement
(a DMARC pass when `MAILROOM_AUTHSERV_IDS` is set; otherwise mail that did not clearly fail), never for mail in Junk,
and only from ports 443 and 80.

## AI tools and MCP

Mailroom has no model-server settings. These control whether AI tools can connect to `/mcp` by
OAuth. API keys always work. See [MCP tools](../mcp.md).

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_OAUTH` | on | `off` turns the whole OAuth surface off: its addresses answer not found, `/mcp` accepts keys only, and issued tokens stop working (grants remain and work again when switched on). OAuth also needs a known https public address (`MAILROOM_PUBLIC_URL` or the OIDC redirect URL). |
| `MAILROOM_OAUTH_DCR` | on | Dynamic client registration (`POST /oauth/register`). `off` removes it. Limited to 50 registrations a day, a short burst per address, and 10,000 unapproved registrations; an unapproved one is deleted after 24 hours. |
| `MAILROOM_OAUTH_ALLOW_PRIVATE_CLIENT_HOSTS` | off | Lets Mailroom fetch an app's metadata from a private or loopback address. Link-local stays refused. |

## Metrics

None. `/metrics` is always served, unauthenticated. Keep it off the public internet. See
[monitoring](monitoring.md).

## Limits

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_RATE_LIMIT_RPS` | `50` | Per-user API requests per second. `0` here or in the burst disables the limiter. Negative is refused. |
| `MAILROOM_RATE_LIMIT_BURST` | `100` | Per-user burst allowance. |
| `MAILROOM_WKD_RATE_LIMIT_RPS` | `5` | Global budget for anonymous key lookups. Unset means on. |
| `MAILROOM_WKD_RATE_LIMIT_BURST` | `20` | Burst for anonymous key lookups. |

## Retention

> **Important:** the Trash and purge settings hard-delete mail. There is no recovery afterwards.

| Variable | Default | Meaning |
|---|---|---|
| `MAILROOM_TRASH_RETENTION` | `720h` (30 days) | How long trashed mail stays restorable before the purge sweep deletes it and its blob. Positive. |
| `MAILROOM_PURGE_INTERVAL` | `1h` | How often the purge runs, up to 500 due messages per user per run. Positive; there is no "off". |
| `MAILROOM_CHANGE_LOG_RETENTION` | `2160h` (90 days) | How long change history is kept. Minimum `168h`. A client offline longer gets one forced full resync. |
| `MAILROOM_JOB_RETENTION` | `720h` | How long finished background jobs are kept. Minimum `24h`. |
| `MAILROOM_DEAD_JOB_RETENTION` | `2160h` | How long permanently failed jobs are kept. Minimum `24h`. |

## Container memory limits

Every service has a `mem_limit`. These are the overridable ones (set them in `.env`):

| Variable | Default | Notes |
|---|---|---|
| `MAILROOM_SERVER_MEM_LIMIT` | `1g` | `mailroom`. Scales with the mailbox. |
| `MAILROOM_POSTGRES_MEM_LIMIT` | `1g` | `postgres`. Scales with the mailbox; peak is mostly reclaimable cache. |
| `MAILROOM_POSTGRES_MEM_RESERVATION` | `256m` | A floor for `postgres`. |
| `MAILROOM_CRYPTO_MEM_LIMIT` | `384m` | `crypto`. One decrypt holds up to 32 MiB several times over. |
| `MAILROOM_CLAMAV_SIGNATURES_MEM_LIMIT` | `2g` | `clamav-signatures`. Measured peak 910 MiB, because freshclam test-loads each new database. |
| `MAILROOM_CLAMD_MEM_LIMIT` | `3500m` | `clamd`. Do not go below about 2.5 GiB. |

The one-shot jobs (`role-bootstrap-*`, `migrate`) have fixed limits.

Size `mailroom` and `postgres` from the real mailbox: run `mailroom-census` (see the
[first-mailbox runbook](../production-cutover-runbook.md#12-census-the-real-mailbox)).

**Why clamd needs 3500m.** A signature reload roughly doubles clamd's memory (about 991 MiB to
1,989 MiB measured), because the new engine loads while the old one serves. Its 512 MiB `/tmp`
tmpfs is charged to the same limit. With a tight limit and swap the reload pages heavily. Without
swap it is OOM-killed, and the container stays `running` with a clamd that does not answer, so
`restart: unless-stopped` never fires and sync stays paused until you restart `clamd`.

## Related

- [Admin guide](README.md)
- [Install guide](../../deploy/README.md)
- [Security](security.md)
