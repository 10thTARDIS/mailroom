<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Authentication and users

How people sign in to Mailroom, who is allowed in, and how to disable a user or recover from a wrong
operator subject.

**On this page:** [Set up OIDC](#set-up-oidc) · [Who may log in](#who-may-log-in) ·
[Sessions and signing out](#sessions-and-signing-out) · [What an anonymous visitor reaches](#what-an-anonymous-visitor-reaches) ·
[The operator subject](#the-operator-subject) · [Recover from a wrong operator subject](#recover-from-a-wrong-operator-subject) ·
[Disable and re-enable a user](#disable-and-re-enable-a-user) · [Dev-auth](#dev-auth-insecure-local-only)

The server refuses to start unless exactly one sign-in path is usable. **Use OIDC for any instance
holding real mail.**

## Set up OIDC

1. Register Mailroom at your provider as a client. The redirect URI is
   `MAILROOM_OIDC_REDIRECT_URL`, for example `https://mail.example.com/auth/callback`.
   Mailroom serves the **path of that URL** as its callback, so the two cannot disagree. Any
   path works except `/auth/login` and `/auth/logout`.
2. Put the non-secret values in `deploy/.env`:

   ```sh
   MAILROOM_OIDC_ISSUER=https://auth.example.com/application/o/mailroom/
   MAILROOM_OIDC_CLIENT_ID=mailroom
   MAILROOM_OIDC_REDIRECT_URL=https://mail.example.com/auth/callback
   ```

3. Put the two secrets in files:

   ```sh
   cd deploy/secrets
   printf '%s' 'the-secret-your-provider-issued' > mailroom_oidc_client_secret.txt
   openssl rand -hex 32 | tr -d '\r\n' > mailroom_session_key.txt   # at least 32 bytes
   ```

4. Register the sign-out return address too (below), then restart: `docker compose -f deploy/compose.yml up -d`.

A partly configured OIDC block (one typo) is a hard error at boot. It never falls back to dev-auth.
A configured block with an empty client secret or session key fails with "OIDC is partially configured".

**The redirect URL's scheme matters.** `https://` marks session cookies `Secure` and enables HSTS.
`http://` does neither, even behind a working TLS proxy. If the instance is really reached over
plain http while the URL says `https://`, the browser never returns the login-state cookie and
sign-in ends with `?error=expired`. There is no `MAILROOM_COOKIE_SECURE` setting.

The default scopes are `openid profile email offline_access groups`.

**Register the sign-out return address.** Signing out also ends the provider session when the
provider publishes an `end_session_endpoint`. Most providers reject an unregistered return
address, so add this exact URL to the client (in Authentik, the provider's **Redirect URIs** list):

```
https://mail.example.com/auth/login?notice=signed_out
```

It is the origin of `MAILROOM_OIDC_REDIRECT_URL` plus `/auth/login?notice=signed_out`. Skip it and
the provider shows an error page on sign-out. The Mailroom session is already gone either way,
because the local cookies are cleared first. A provider with no `end_session_endpoint`, or a session
with no stored ID token, gives a purely local sign-out; there is nothing to configure.

## Who may log in

- **The provider is the gate.** Anyone who completes the OIDC flow for this application is
  authorised and auto-provisioned on first login from their token's `email` and
  `name`/`preferred_username`. Bind the application to the right users or groups at your provider.
  Each user gets an isolated mailbox with their own keys, blobs and search index. A new user starts
  syncing within about a minute.
- **Admins come from a group.** Members of the `mailroom-admin` group (the `groups` claim) get the
  admin role, re-evaluated on every login. The provider must emit a `groups` claim (in Authentik,
  add a groups scope mapping). With no groups claim there are no admins.
- **A login is refused** (no session cookie; the sign-in page shows the reason) when the token has
  no email, when the user is disabled or deleted, or when the email already belongs to a different
  user. The last happens with a wrong operator subject, a provider that re-issued a subject, or an
  email changed to another user's. The log names the subjects, never the email.

## Sessions and signing out

Sessions are server-side records; the cookie holds only a random secret. Signing out revokes the
record. Disabling a user ends their sessions on the next request.

- **With refresh tokens** (the provider honours `offline_access`), a session renews quietly with
  the provider every 15 minutes of use. The idle limit is the provider's refresh-token validity
  (in Authentik, the provider's Refresh Token validity, for example `days=30`). Role and email
  are refreshed at each renewal.
- **Without refresh tokens**, a session is refused 12 hours after sign-in. This is also the most a
  provider outage can cost.
- A database error while checking a session answers 503, not 401, so open tabs are not bounced.
- The session key now only signs the login-state cookie. Rotating it invalidates sign-ins in
  flight, not live sessions.
- Settings > Security lists signed-in devices. Behind a proxy, set `MAILROOM_TRUSTED_PROXIES`
  so it shows real addresses.

## What an anonymous visitor reaches

| Route | Anonymous request gets |
|---|---|
| `/auth/login` | 200, the sign-in page |
| `/auth/start` | 302 to your provider |
| the callback path | 302 to `/auth/login`, with the reason on failure |
| `/auth/logout` | 302 to `/auth/login?notice=signed_out` |
| `/healthz`, `/readyz`, `/metrics` | 200 |
| `/.well-known/openpgpkey/*`, `/key/*` | Published PGP keys only |
| `/jmap`, `/jmap/session`, `/.well-known/jmap`, `/jmap/download/…`, `/jmap/upload/…`, `/jmap/eventsource/…`, `/mcp` | 401 |
| `/`, `/assets/…`, anything else | 302 to `/auth/login` |

`/metrics` carries user and account ids as labels, so it must not be public. See
[monitoring](monitoring.md#metrics). The table is asserted route by route in
`cmd/mailroom/gate_test.go`.

## The operator subject

`MAILROOM_OPERATOR_SUBJECT` and `MAILROOM_OPERATOR_EMAIL` are optional under OIDC. They provision a
boot-time user who owns the [accounts file](accounts.md). Leave them unset for a pure multi-user
instance.

> **Important:** if you set the subject, it must equal your provider's `sub` claim byte for byte.
> A wrong one is worse than none: your declared accounts sync into a user you cannot sign in as,
> and your real login is refused with "the account could not be set up".

Providers often use an opaque `sub` (Authentik uses a UUID, not your username). To read it without
guessing, leave both variables unset, sign in once, then:

```sh
docker compose -f deploy/compose.yml logs mailroom | grep 'login provisioned'
# msg="login provisioned" subject=<the value you want> user_id=... role=...
```

Set the variables only after that, if you need the accounts file.

## Recover from a wrong operator subject

Do not just fix the variable and restart: boot would provision a second user whose email the first
still holds, and the server refuses to start. Move the user that owns the mail first. It keeps its
id, and with it every account and message.

1. Read both subjects from the refused-login log:

   ```sh
   docker compose -f deploy/compose.yml logs mailroom | grep -E 'login refused|login created a NEW user'
   ```

   `operator_subject` is the wrong value you configured. `token_subject` is what the provider
   issued. If your provider's email differs from `MAILROOM_OPERATOR_EMAIL`, the login instead
   succeeds into a second, empty user and the log says `login created a NEW user`.
2. **Confirm `token_subject` is your account** in the provider's own admin view. Anyone who can put
   your email on a provider account can produce that log line, and the next step hands everything
   the operator owns to the holder of `token_subject`.
3. Stop the server and move the user with [`rebind-operator-subject.sql`](../../deploy/rebind-operator-subject.sql):

   ```sh
   docker compose -f deploy/compose.yml stop mailroom
   docker compose -f deploy/compose.yml exec -T postgres \
     psql -U postgres -d mailroom -v ON_ERROR_STOP=1 \
       -v old='<operator_subject>' -v new='<token_subject>' \
     < deploy/rebind-operator-subject.sql
   ```

   It prints `NOTICE: moved user <id> …` and `COMMIT`. On any refusal it prints an `ERROR` and
   changes nothing. A second, empty user from your real login is retired first (disabled, its
   subject and email renamed). The script refuses if that user owns accounts or messages, because
   then there are two real users and choosing is your decision.
4. Set `MAILROOM_OPERATOR_SUBJECT` to `token_subject` in `.env`, then
   `docker compose -f deploy/compose.yml up -d mailroom`. The order matters in this direction too.
5. Sign out and in again. The `operator ready` and `login provisioned` log lines now show the same
   `user_id`.

## Disable and re-enable a user

```sh
mradmin user disable <user-id-or-email>
mradmin user enable  <user-id-or-email>
```

Disabling is soft and reversible; nothing is deleted. A disabled user cannot sign in. Their
sessions, API keys and OAuth grants are refused on every request (and work again on enable), open
event streams end, and background work stops: sync, writeback, push, search indexing, scheduled
sends, snoozes and AI-tool approval expiry. Sends and snoozes that fall due wait and run after
enable. Stored mail is untouched. Both commands are safe to run twice. `disable` refuses to disable
the last enabled user. The provider still authenticates a disabled user; Mailroom refuses them.

It does not unpublish the user's PGP key (a public key is public by design). To remove a user and
their data for good, use `delete-user` ([admin CLI](admin-cli.md#users)).

## Dev-auth (insecure, local only)

> **Important:** setting `MAILROOM_DEV_AUTH_SUBJECT` turns authentication **off**. Every request that
> reaches the port is the operator. Never put real mail on such an instance or expose it to an
> untrusted network.

It is honoured only when the OIDC block is entirely empty, and only with all three variables:

```sh
MAILROOM_DEV_AUTH_SUBJECT=operator
MAILROOM_DEV_AUTH_EMAIL=operator@example.com
MAILROOM_DEV_AUTH_I_UNDERSTAND=this-instance-has-no-authentication
```

The subject alone is refused. The server warns on every boot, and `/auth/login` says the instance
has no authentication. Disabling that user does not refuse the dev identity (only its event streams
end). Switch to OIDC before any real mail arrives.

## Related

- [Accounts](accounts.md)
- [Security](security.md)
- [Troubleshooting](troubleshooting.md#sign-in-loops-or-expired-behind-a-proxy)
- [Configuration](configuration.md#sign-in)
