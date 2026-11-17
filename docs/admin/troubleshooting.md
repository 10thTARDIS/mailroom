<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Troubleshooting

Find the symptom, then the cause and fix. Start with `docker compose -f deploy/compose.yml ps` and the
logs of the service that is unhealthy. Commands use `mradmin` from [admin CLI](admin-cli.md#run-a-command).

**On this page:** [Stack won't start](#stack-wont-start) · [Sign-in](#sign-in) · [Mail not arriving](#mail-not-arriving) ·
[Sending](#sending) · [Disk](#disk)

## Stack won't start

### Crypto never turns healthy (PGP root mismatch)

- **Symptom:** the `crypto` container restarts and never turns healthy. Its log says `PGP ROOT MISMATCH`
  (`"outcome":"mismatch"`).
- **Cause:** `mailroom_pgp_root_key.txt` is not the root this database recorded at first boot. Usually a
  restore where the secrets were generated again.
- **Fix:** install the original root from escrow. Do **not** generate a new one: a wrong root that the
  database never recorded is silent until someone signs or decrypts, and by then the stored private
  keys cannot be opened. See [key escrow](../key-escrow.md#recovery-disaster-restore) and the
  [restore runbook](../backup-restore-runbook.md) step 5.
  `docker compose -f deploy/compose.yml logs crypto | grep -o '"outcome":"[a-z]*"'` gives the state.

### `blob: not found` for every message

- **Cause:** the wrong master key. Blob paths derive from it, so the wrong key looks for files that were
  never there.
- **Fix:** install the master key from escrow ([restore runbook](../backup-restore-runbook.md) step 5).

### A secrets path became a directory

- **Symptom:** a service fails at boot with "is a directory". `docker compose config` still exits 0.
- **Cause:** a secret file was missing at `up`. The daemon created the path as a **directory** under
  `deploy/secrets/`. Nothing started with an empty key.
- **Fix:** `rmdir deploy/secrets/<name>`, create the file (an empty file is fine for unused ones:
  `mailroom_pgp_root_key.txt`, `mailroom_oidc_client_secret.txt`), and `docker compose up -d`. Writing
  the file first fails with `Is a directory`.

### The server exits at boot with a config error

- **Symptom:** `mailroom` exits straight away and the log names a variable.
- **Cause:** a malformed value. Booleans accept only `1/true/yes/on/0/false/no/off`; durations must be
  Go durations; a partly set OIDC block ("OIDC is partially configured"), an unset session key, a
  malformed `MAILROOM_TRUSTED_PROXIES` or `MAILROOM_AUTHSERV_IDS` bound, or a Google or Microsoft id
  without its secret all stop the server. With no sign-in path at all it also refuses to start.
- **Fix:** correct the named variable. See [configuration](configuration.md).

### `migrate` or `postgres` is unhealthy

- **Cause:** a Postgres that has not finished starting, or a password file with trailing `\r` or spaces.
- **Fix:** read `docker compose -f deploy/compose.yml logs postgres migrate`. Check `wc -c deploy/secrets/*.txt`:
  48 bytes for the three passwords, 64 for each root key and the session key.

## Sign-in

### Sign-in loops or expired behind a proxy

- **Symptom:** the browser returns to the sign-in page with `?error=expired`, or loops.
- **Cause:** the redirect URL's scheme. If `MAILROOM_OIDC_REDIRECT_URL` says `https://` but the instance
  is reached over plain http, the `Secure` login-state cookie is never sent back. In the other direction,
  an `http://` URL behind a TLS proxy gives non-`Secure` session cookies.
- **Fix:** make the scheme match how browsers actually reach Mailroom, and register the same URL at the
  provider. There is no `MAILROOM_COOKIE_SECURE`. See [authentication](authentication.md#set-up-oidc).

### "The account could not be set up", or a login lands in an empty mailbox

- **Cause:** `MAILROOM_OPERATOR_SUBJECT` is not the provider's `sub`. The log shows
  `login refused: this email already belongs to another user`, or `login created a NEW user`.
- **Fix:** follow [Recover from a wrong operator subject](authentication.md#recover-from-a-wrong-operator-subject).
  Setting the variable and restarting will stop the server booting.

### A tab says "Offline" after a long idle, behind a forward-auth proxy

**Cause:** a sign-in gate in front of Mailroom (forward authentication at the reverse proxy)
expired its own session and answers API requests with a redirect to its sign-in page. Before
v1.103 the web app read that as a network failure.

**Fix:** from v1.103 the app treats the redirect as signed out and reloads through the gate. To
make it rarer, give the gate's session a lifetime at least as long as a working day; Mailroom's
own session renews itself while the tab is in use.

### Login refused for a disabled user

- **Cause:** the token has no email, or the user is disabled or deleted.
- **Fix:** `mradmin user enable <id-or-email>`. Give the provider account an email.

### The provider shows an error page when signing out

- **Cause:** the post-logout return address is not registered. The Mailroom session is gone regardless.
- **Fix:** register `https://mail.example.com/auth/login?notice=signed_out` against the client.

### Nobody gets the admin role

- **Cause:** the provider emits no `groups` claim, or the user is not in `mailroom-admin`.
- **Fix:** add a groups scope mapping, add the user to the group, sign in again (it is re-read each login).

### Everyone is signed out every 12 hours

- **Cause:** the provider issues no refresh tokens.
- **Fix:** add the `offline_access` scope mapping and set a Refresh Token validity (for example
  `days=30`). Without them, sessions last 12 hours.

## Mail not arriving

### Sync paused by a scanner outage

- **Symptom:** every account stops syncing. Account health says the scanner is unreachable. The log
  shows `PING timeout exceeded; No response from clamd`.
- **Cause:** scanning is fail-closed. `clamd` is down, still loading (about 70 seconds), or was
  OOM-killed on a signature reload because `MAILROOM_CLAMD_MEM_LIMIT` is too tight. An OOM-killed `clamd`
  stays `running` and never restarts itself.
- **Fix:** `docker compose -f deploy/compose.yml ps clamd`, then `restart clamd`. Keep the limit at
  about 2.5 GiB or more (default 3500m). Nothing is lost: mail stays on the provider and sync resumes
  by itself. To run without scanning, unset `MAILROOM_CLAMD_SOCKET`.

### A scanner that scans one update behind

- **Cause:** a signature update landed while `clamd` was loading.
- **Fix:** compare and reload as in [security](security.md#attachment-scanning-with-clamav).

### One account is not syncing

- **Cause:** the provider rejected its credentials at onboarding, so sync was disabled
  (`mailroom_sync_disabled_accounts` above 0, and a WARN each pass).
- **Fix:** correct the credentials with `set-account-credentials`, then
  `mradmin enable-account --user-id <id> --account-id <id>`. A restart does not re-enable it.
  See [accounts](accounts.md#change-a-live-account).

### The accounts file edit had no effect

- **Cause:** the file is additive. Credential, endpoint and `writeback_enabled` edits, and removals,
  are ignored for an existing account.
- **Fix:** see [what editing the file does](accounts.md#what-editing-the-file-does) and run
  `accounts-report --fail-on-drift`.

### Searches are rate-limited, or `mailboxes_deferred` stays above 0

- **Cause:** the provider caps `UID SEARCH` per minute, shared with your phone and webmail.
- **Fix:** lower `MAILROOM_IMAP_SEARCH_RATE` (default 30). It is pacing, not failure.

### Reconnect needed for a Gmail or Outlook account

- **Symptom:** the account says "needs reconnecting", "server sign-in problem" or a connect error code.
- **Cause:** the provider no longer accepts the saved access (the person revoked it or changed their
  password), or your OAuth client secret expired or the client was changed.
- **Fix:** the person uses **Reconnect** and signs in as the same account. For "server sign-in problem",
  fix the client secret at the provider and in your configuration (Microsoft secrets expire). The
  error-code table is in [Gmail and Outlook](../oauth-providers.md#6-troubleshooting).
  Gmail sync is currently held (`gmail_sync_pending`), which is not a fault.

### Search finds nothing after a restore or index loss

- **Cause:** the index is a rebuildable cache and is rebuilding. Search is incomplete until it finishes.
- **Fix:** wait. Watch `mailroom_index_cursor_lag_seconds`.

## Sending

### Sends from an alias bounce with `550 5.7.1 sender address rejected`

- **Cause:** the provider has not allowed this mailbox to send as that address. Receiving at an address
  and sending as it are separate permissions, granted per mailbox.
- **Fix:** grant it at the provider, then prove it with one real send. See
  [accounts](accounts.md#send-as-aliases-is-a-permission-at-the-provider).

### Nothing sends from an account

- **Cause:** the account has writeback off, or `MAILROOM_WRITEBACK_DISABLED` is set. Ingest-only also
  refuses sending. For signed identities, `crypto` may be down.
- **Fix:** see [ingest-only mode](accounts.md#ingest-only-mode). Check `ps crypto`.

### A send waits behind the attachment sweep

- **Cause:** a large first sweep shares the sequential job queue; a send can wait up to about two minutes.
- **Fix:** none needed. Wait until `attachment-report` shows `due now` at 0.

## Disk

### A disk is filling up

Find what is big first:

```sh
docker system df -v
df -h /var/lib/docker /var/lib/containerd
```

| Grows | Cause and fix |
|---|---|
| `blobs` | Mail itself. Trashed mail is deleted after `MAILROOM_TRASH_RETENTION` (30 days). Lowering it is destructive. `blob-report` shows files without database rows. Images kept by prefetch are capped at 16 GiB per user (`MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES`). |
| `pgdata` | Mail metadata and change history. `compact-changelog` reports history older than `MAILROOM_CHANGE_LOG_RETENTION`. `jobs-report` shows job rows the daily sweep would delete. |
| `search` | The index, a disposable plaintext cache. It can be rebuilt, at the cost of incomplete search while it does. |
| `clamav-db` | Signature databases. |
| Container logs | Rotated at 10 MB times 5 per container, so bounded. |
| Host backup staging | `scripts/backup.sh` stages an unencrypted copy under its checkout while it runs and removes it on exit. |
| Old images | `docker image ls` after upgrades. Remove only your own old Mailroom image tags. |

## Related

- [Monitoring](monitoring.md)
- [Configuration](configuration.md)
- [Security](security.md)
- [Backup runbook](../backup-restore-runbook.md)
