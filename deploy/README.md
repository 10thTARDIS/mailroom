# Install Mailroom

This directory is a self-contained Docker Compose stack: Postgres, schema migrations, the crypto
process, malware scanning and the server, each running non-root with least privilege. The same
stack serves a trial and a real instance. Placeholders such as `mail.example.com` are neutral;
substitute your own.

**On this page:** [1. Prerequisites](#1-check-the-prerequisites) · [2. Image](#2-get-the-image) ·
[3. Secrets](#3-generate-the-secrets) · [4. Accounts](#4-declare-your-accounts) ·
[5. Sign-in](#5-set-up-sign-in-oidc) · [6. Start](#6-bring-it-up) · [7. Verify](#7-verify-it-is-healthy) ·
[8. First sign-in](#8-sign-in-for-the-first-time)

Running a server day to day? Go to the [operator guide](../docs/admin/README.md). Attaching a real
mailbox? Read the [first-mailbox runbook](../docs/production-cutover-runbook.md) first.

## 1. Check the prerequisites

- **Docker Engine with Compose v2**, and a checkout of this repository at the release tag you
  deploy (for `deploy/`). Do not build from source on this host.
- **Encrypted storage.** The `search` and `pgdata` volumes hold plaintext mail, and Mailroom cannot
  encrypt them for you. Put Docker's data root on LUKS or equivalent, and run without swap or with
  encrypted swap. Check it: [encrypt the volumes](../docs/admin/security.md#encrypt-the-volumes).
- **TLS in front.** The stack terminates no TLS and publishes one port on `127.0.0.1`. Run a reverse
  proxy for `mail.example.com`, and keep `/metrics` off the public internet.
  [Details and the check](../docs/admin/security.md#terminate-tls-in-front).
- **An OIDC provider** where you can register a client, and DNS for your hostname.
- **Outbound access** from the host to your mail provider's IMAP and SMTP, and to the provider.

## 2. Get the image

Pushing a `vX.Y.Z` tag publishes `ghcr.io/10thtardis/mailroom:X.Y.Z` to the GitHub Container
Registry (public, no login). It is the same bytes CI tested, built only from a commit that had a
green run, with its version stamp checked. Pin it by digest. **Never use `:latest` in production.**

```sh
docker pull ghcr.io/10thtardis/mailroom:X.Y.Z        # prints "Digest: sha256:..."
cp deploy/.env.example deploy/.env
echo 'MAILROOM_IMAGE=ghcr.io/10thtardis/mailroom:X.Y.Z@sha256:<digest>' >> deploy/.env
```

For throwaway testing only, build locally with `docker build --build-arg MAILROOM_VERSION=X.Y.Z -t mailroom:test .`.
Without the build argument the binary reports `0.0.0-dev`. The one image holds `/mailroom` (server),
`/mailroom-migrate`, `/mailroom-admin`, `/mailroom-crypto` and `/mailroom-census`.

## 3. Generate the secrets

> **Important:** restoring after a disaster? **Skip this step.** It mints new crypto roots. A wrong
> master key shows as `blob: not found`; a wrong PGP root stays silent until someone signs or
> decrypts, and then every stored private key is unopenable. Install both roots from escrow
> instead: [restore runbook](../docs/backup-restore-runbook.md) step 5 and [key escrow](../docs/key-escrow.md).

Secrets are files under `deploy/secrets/` (gitignored). Never commit real values.

```sh
cd deploy/secrets
openssl rand -hex 24 | tr -d '\r\n' > postgres_password.txt
openssl rand -hex 24 | tr -d '\r\n' > mailroom_owner_password.txt
openssl rand -hex 24 | tr -d '\r\n' > mailroom_app_password.txt
openssl rand -hex 32 | tr -d '\r\n' > mailroom_master_key.txt     # master key
openssl rand -hex 32 | tr -d '\r\n' > mailroom_pgp_root_key.txt   # a SECOND, independent root
openssl rand -hex 32 | tr -d '\r\n' > mailroom_session_key.txt    # signs sign-in state
printf '{"accounts": []}' > accounts.json                          # step 4
: > mailroom_oidc_client_secret.txt                                # step 5
wc -c *.txt     # 48 bytes for the three passwords, 64 for each key, no more
```

- `tr -d '\r\n'` matters on Windows, where openssl emits CRLF and a stray `\r` breaks password auth.
- **Every file must exist before the first `up`**, even unused ones. Empty is fine. A missing one
  becomes a directory ([fix](../docs/admin/troubleshooting.md#a-secrets-path-became-a-directory)).
- **Escrow both roots separately, off this host, before relying on the instance.** Losing the
  master key loses the mailbox; losing the PGP root silently loses encrypted mail. Never point both
  at one file. See [key escrow](../docs/key-escrow.md) and [security](../docs/admin/security.md#root-keys-and-escrow).

## 4. Declare your accounts

Optional: users can also add accounts in the web client. To have one present at first boot, edit
`deploy/secrets/accounts.json`:

```json
{ "accounts": [ {
    "name": "personal", "imap_host": "imap.example.com", "smtp_host": "smtp.example.com",
    "username": "you@example.com", "password": "<app password>"
} ] }
```

The file is **additive**: boot onboards new names only and never changes or removes an existing
account. Declared accounts need the operator variables in step 5. Fields, the edit-effect table,
aliases and ingest-only mode are in [accounts](../docs/admin/accounts.md). Users can connect Gmail
and Outlook by OAuth: [setup](../docs/oauth-providers.md).

## 5. Set up sign-in (OIDC)

The server refuses to start unless exactly one sign-in path is usable. Use OIDC for real mail.
Register `https://mail.example.com/auth/callback` as the client's redirect URI, and also
`https://mail.example.com/auth/login?notice=signed_out` as the post-sign-out address. Then:

```sh
# deploy/.env
MAILROOM_OIDC_ISSUER=https://auth.example.com/application/o/mailroom/
MAILROOM_OIDC_CLIENT_ID=mailroom
MAILROOM_OIDC_REDIRECT_URL=https://mail.example.com/auth/callback

# deploy/secrets
printf '%s' 'the-secret-your-provider-issued' > mailroom_oidc_client_secret.txt
```

- The redirect URL's **scheme** sets `Secure` cookies and HSTS. Use `https://` behind your proxy.
- Anyone who completes the flow is provisioned on first login, so bind the application to the right
  users at your provider. Members of the `mailroom-admin` group (a `groups` claim) become admins.
- Leave `MAILROOM_OPERATOR_SUBJECT` and `MAILROOM_OPERATOR_EMAIL` unset unless you declared accounts
  in step 4. If you set the subject it must equal the provider's `sub` exactly; read it from the
  first login ([how](../docs/admin/authentication.md#the-operator-subject)).
- No provider yet? Dev-auth turns authentication **off**: local, empty instances only
  ([dev-auth](../docs/admin/authentication.md#dev-auth-insecure-local-only)).

More: [authentication](../docs/admin/authentication.md). Every setting: [configuration](../docs/admin/configuration.md).

## 6. Bring it up

```sh
docker compose -f deploy/compose.yml up -d
docker compose -f deploy/compose.yml ps
docker compose -f deploy/compose.yml logs -f mailroom
```

In order, Compose starts `postgres` (empty database), `role-bootstrap-owner`, `migrate` (applies every
migration, the schema's source of truth), `role-bootstrap-app`, then `mailroom`, `crypto` and the two
scanner containers. The default project name is `mailroom-deploy`, so containers are named
`mailroom-deploy-<service>-1`; `ps` shows the real names. Every command here uses service names.

The app is published on `127.0.0.1:${MAILROOM_HTTP_PORT:-8080}` (`.env.example` sets 8090). To reach
it from another machine set `MAILROOM_HTTP_BIND=0.0.0.0` and firewall it.

## 7. Verify it is healthy

```sh
PORT="${MAILROOM_HTTP_PORT:-8080}"
curl -s -o /dev/null -w '%{http_code}\n' "http://localhost:$PORT/healthz"    # 200
curl -s -o /dev/null -w '%{http_code}\n' "http://localhost:$PORT/readyz"     # 200: a real database round trip
docker compose -f deploy/compose.yml exec crypto /mailroom-crypto -healthcheck   # ok
docker compose -f deploy/compose.yml exec clamd clamdscan --ping 1                # PONG (~70 s from cold)
docker compose -f deploy/compose.yml logs crypto | grep -o '"outcome":"[a-z]*"'
docker compose -f deploy/compose.yml logs mailroom | grep "attachment scanning"   # "...is ON; ..."
```

- `crypto` outcome is `recorded` on first boot or `verified` later. `mismatch` means the wrong PGP
  root: it restarts and never turns healthy ([fix](../docs/admin/troubleshooting.md#crypto-never-turns-healthy-pgp-root-mismatch)).
- All services should be `healthy` in `ps`. `/healthz`, `/readyz` and `/metrics` need no session.
- The server must not hold the PGP root, and `crypto` must not hold the master key:
  `exec mailroom ls /run/secrets/` has no `mailroom_pgp_root_key`; `exec crypto ls /run/secrets/` has
  only `mailroom_pgp_root_key` and `mailroom_app_password`.
- **Prove the proxy is the only way in** with a negative control from another machine: the app port
  must refuse `/healthz` and `/metrics`, while `https://<public-name>/auth/login` sends HSTS.
  [Commands](../docs/admin/security.md#terminate-tls-in-front).
- A first scan of old attachments can take hours; sync pauses if `clamd` is down.
  [ClamAV](../docs/admin/security.md#attachment-scanning-with-clamav).

## 8. Sign in for the first time

Open `https://mail.example.com/` (it sends you to `/auth/login`) and sign in. Your first login
provisions you. The log line shows your subject and user id:

```sh
docker compose -f deploy/compose.yml logs mailroom | grep 'login provisioned'
# msg="login provisioned" subject=<sub> user_id=<id> role=<role>
```

If you are not an admin, add yourself to the `mailroom-admin` group at the provider and sign in again.
If sign-in loops or is refused, see [troubleshooting](../docs/admin/troubleshooting.md#sign-in).

Before real mail arrives: escrow the two roots, confirm encrypted volumes, take a backup and run the
restore drill ([backup runbook](../docs/backup-restore-runbook.md)), and read the
[first-mailbox runbook](../docs/production-cutover-runbook.md).

## Tear down

```sh
docker compose -f deploy/compose.yml down       # keep data (pgdata, blobs, search, clamav-db)
docker compose -f deploy/compose.yml down -v    # also wipe all of it
```

## Related

- [Operator guide](../docs/admin/README.md)
- [Upgrading](../docs/admin/upgrading.md)
- [Configuration reference](../docs/admin/configuration.md)
- [Troubleshooting](../docs/admin/troubleshooting.md)
