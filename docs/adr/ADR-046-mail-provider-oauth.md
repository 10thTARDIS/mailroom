# ADR-046: Mail-provider OAuth — grants, flow, registry

**Status:** Accepted (Forgejo #52, v1.96.0; every decision below was
taken as the design spike recommended). Increment 2
(XOAUTH2, the token store, migration 00078) implements §2, §4, §5 and §7;
increment 3 implements the connect flow (§3), disconnect (§6) and the §9
obligations (§10 records how); increment 4 implements health and UX (§5's
effects; §11 records how); the Gmail sync model is ADR-047; the Microsoft
profile is increment 6 (§12 records how). Narrows the change_log invariant for one field group, in the style
of ADR-014 (§4).

## Context

Spec §18.4 left live Gmail as "add live Gmail-OAuth in 1.x only if the tail
matters", with the self-hosted answer that **each operator creates their own
OAuth client**. It now matters: the instance is meant for a
small group whose accounts are mostly at large providers such as Gmail or Outlook.com. Outlook.com no longer accepts
passwords over IMAP at all ("requires Modern Auth / OAuth2"), and Gmail
accepts only app passwords, which need 2-Step Verification and which Google
describes as "not recommended".

Today every account authenticates with a password: `sync.Dial` sends LOGIN,
`provider.SMTPSender` sends AUTH PLAIN, and the secret is
`onboarding.Credentials` sealed into `app.accounts.credentials_ct` under a
per-user HKDF key (ADR-013). `app.accounts.auth_kind` already allows
`'oauth2'`, and nothing writes it.

## Decision

### 1. Providers, operator setup and who may connect

- **Two providers, data in a registry** (`internal/oauthmail`): Google (Gmail,
  Workspace) and Microsoft (Outlook.com, Microsoft 365). Each entry fixes the
  authorization, token and revocation endpoints, the scopes, the scopes that
  must be granted, and the IMAP and SMTP host, port and TLS mode. A generic
  OIDC-discovery provider is one entry away and is not shipped: it could not
  be tested without a real provider.
- **The hosts are the registry's, never the user's.** A bearer token is only
  ever sent to the provider that issued it. For an OAuth account the sync
  resolver and compose take the IMAP host and port and the SMTP host, port and
  TLS mode from the registry (`Manager.HostsFor`), not from the account row;
  a row that names anything else is **refused** with a config error
  (`xoauth2.ErrHostMismatch`, logged with ids only), never silently corrected,
  so the inconsistency is seen. Connection fields are immutable on an OAuth
  account (increment 3 enforces this in `Account/set`).
- **Google: the project is published "In production", unverified**, under the
  personal-use exemption (fewer than 100 users). Users click through Google's
  "unverified app" screen; the project's lifetime cap of 100 new users is
  accepted. "Testing" mode is unusable: its refresh tokens expire after 7 days.
  `https://mail.google.com/` is the only scope that grants IMAP and SMTP, and
  it is restricted.
- **Microsoft: one app registration for "any organizational directory and
  personal accounts", authority `/common`.** The operator may narrow the
  tenant (`consumers`, a tenant id or domain). Friends on work tenants may
  need their administrator's consent; the UI says so.
- **Any user of the instance may connect** an account once the operator has
  configured the provider. A provider is enabled only when both its client id
  and secret are set.
- **One OAuth client per instance** (a test and a production instance separately): the
  100-refresh-tokens-per-account limit is per client, and the redirect URIs
  differ.
- **Gmail app passwords** stay possible through the existing password path
  and are mentioned in the operator guide only as a fallback for a Workspace
  whose administrator blocks unverified apps. They are not promoted.

### 2. The token model

- **The refresh token is sealed where passwords are sealed.**
  `onboarding.Credentials` gains `OAuth *OAuthSecret{Provider, RefreshToken}`;
  for an OAuth account `IMAPUsername` is the mailbox address and the password
  fields are empty. One sealed-secret location means backups, the restore
  drill and the admin report keep working unchanged. `omitempty` keeps a
  password account's sealed JSON byte-identical (pinned by a test).
- **The queryable half of the grant is a table**, `app.oauth_grants`
  (migration 00078), 1:1 with the account: provider, the provider's subject,
  address, granted scopes, `state` (`active` | `needs_reauth` |
  `operator_error`) with its reason and time, the rotation `generation`, and
  `last_refresh_at`. Columns, not JSONB: this is queryable state. One grant
  per (user, provider, subject), so a duplicate connect is structural.
- **Access tokens live only in process memory**, per (user, account), and are
  never persisted. They are replaced five minutes before they expire. After a
  restart the first connection pays one HTTPS call. A database dump plus the
  master key therefore yields refresh tokens, which is no worse than today's
  passwords.
- **Every token travels as `xoauth2.Secret`**, whose every rendering — `%v`,
  `%s`, `%q`, `%x`, `%#v`, slog, JSON, text — is `[redacted]` wherever fmt or
  slog can call its methods. They cannot for an **unexported struct field**:
  fmt renders those by reflection, so a Secret there prints in the clear when
  the enclosing struct is formatted, however deeply nested. The rule: a
  Secret is never stored in an unexported field; it is held as
  `xoauth2.Held`, whose only field is a pointer (reflection prints an
  address), and the structs that hold one redact through `String`,
  `GoString`, `Format` and `LogValue` as well. Text that came from a provider
  (an `error_description`) is scrubbed of the request's secrets before it
  reaches an error or a log line.
- **Pending connects are a table too**, `app.oauth_flows`: single-use (read
  by `DELETE … RETURNING`), bound to the user who started it, expiring after
  minutes, the OAuth `state` kept only as its SHA-256 and the PKCE verifier
  sealed under a per-user key with the user id and flow id as associated data.
  A server-side row rather than the login flow's signed cookie, because it is
  single-use, bound to the user and auditable.
- **Not done:** the design's optional hardening — OAuth rows at
  `envelope_ver = 2` with associated data binding the account id as well as
  the user id — is deferred. Today's associated data is the user id only, as
  for password accounts.

### 3. The connect flow (increment 3)

- **Authorization code with PKCE (S256)**, started by a session-only,
  CSRF-guarded, JSON `Account/oauthStart`; refused for API keys.
- **Per-provider callbacks**, `GET /oauth/callback/{google|microsoft}`, built
  from the public origin, behind the session gate (a SameSite=Lax cookie is
  sent on the cross-site top-level GET). Never `response_mode=form_post`: a
  cross-site POST would be refused by `CSRFGuard` and would carry no cookie.
- **The callback checks, in order:** the flow's user is the session user; the
  flow's provider is the path's (mix-up defence, plus `iss` when present,
  RFC 9207); the flow is unexpired; it is consumed. Then the code exchange,
  the ID token's nonce, the **granted scopes** (a partial grant is refused:
  "Mailroom needs mail access to sync"), the address (Google requires
  `email_verified`), and a successful XOAUTH2 IMAP dial as the final arbiter.
- **Re-auth must return the same subject**, or it is refused, so one mailbox
  can never be silently swapped for another. A duplicate connect is offered
  re-auth instead. The callback redirects to a fixed in-app route, never to a
  parameter.
- Google requests `access_type=offline` and `prompt=consent` (so a refresh
  token is always issued) and `login_hint` on re-auth; Microsoft requests
  `offline_access`.

### 4. What appends a change_log row, and what does not

The load-bearing invariant: every mutation of a JMAP-visible object appends a
`change_log` row in the same transaction. An OAuth account is an Account, and
its grant state will be the Account property `authState`. The rule:

- **A grant's creation, and every state TRANSITION, append exactly one
  `Account` 'updated' row** in the transaction that makes it
  (`store.CreateOAuthGrant`, `store.SetOAuthGrantState`). A transition also
  appends an `oauth.state_changed` audit row carrying provider, from, to and
  reason — never a token.
- **Setting the state the grant already has appends nothing**, so a refresh
  that fails the same way every hour does not flood `/changes` or the audit
  log.
- **Refresh-token ROTATION and the `last_refresh_at` stamp append NO row.**
  They change nothing a client can see — the token is never exposed — and they
  happen about hourly per account. A row for each would push an Account change
  to every client every hour, per OAuth account, for nothing, which is exactly
  the flood ADR-014 declined for contact re-sightings.
- **Flows** are not JMAP-visible objects: no rows.
- If a future feature exposes the refresh time or anything rotation touches,
  that feature owes the change row.

### 5. Refresh, concurrency and the three failure classes

- **In-process: singleflight per (user, account).** N callers needing a token
  at once cause one token-endpoint request, and all N get its result. The
  refresh is detached from any one caller's context, so a caller that gives up
  does not fail the others.
- **Across processes: compare-and-swap on `generation`.** A rotated refresh
  token is stored, re-sealed, only `WHERE generation = <the value read with the
  token that was used>`, bumping the generation in the same statement. The
  loser of a race keeps the access token it was issued (it is valid), stores
  nothing and adopts the winner's refresh token on its next read. Microsoft
  does not revoke the previous refresh token on rotation, and Google does not
  rotate (UNVERIFIED), so losing is never fatal: the CAS exists only so a newer
  token is never overwritten with an older one.
- **A cached token that IMAP or SMTP rejects is to be dropped**
  (`Manager.Invalidate`) and refreshed once more before the grant is judged.
  `Invalidate` exists; **nothing calls it yet** — the reject hook lands in
  increment 3 (obligation (a) below).
- **The failure classes — the core of honest re-authentication:**
  - `invalid_grant` → **`needs_reauth`**. The refresh token is dead: revoked,
    the Gmail password changed, six months idle, the 100-token eviction,
    Microsoft's 90 days. Only the user reconnecting fixes it. While in this
    state no refresh is attempted at all: the provider has answered.
  - `interaction_required`, `login_required` and `consent_required` (OIDC's
    "the user must sign in or consent again", which Microsoft returns on
    refresh after a policy change) → **`needs_reauth`** as well.
  - `invalid_client` or `unauthorized_client` → **`operator_error`**: the
    provider refused **this server** (an expired Entra secret, a deleted
    Google client). Every other OAuth error code (`invalid_request`,
    `invalid_scope`, `unsupported_grant_type`, anything unknown) is classified
    the same way, because no user action can fix a request the server built
    wrongly. **The user is not told to reconnect** — that would tell every
    friend to do something useless at once — but "server configuration
    problem, contact the person who runs this server". It is logged at ERROR
    for the operator. Refresh keeps being attempted on the normal backoff, and
    the first success returns the grant to `active` with nobody reconnecting.
  - Network, timeout, 5xx, 429, `temporarily_unavailable`, a 200 without a
    token, a 4xx whose body is not an OAuth error (most likely an
    intermediary), and any 3xx → **transient**: the existing backoff, state
    untouched. The token-endpoint client **never follows a redirect** (the
    next hop would receive the refresh token and the client secret) and never
    falls back to `http.DefaultClient`.
  - A stated `expires_in` is clamped to [30 s, 24 h] before the expiry is
    computed, so a hostile or broken value can neither overflow nor keep a
    token cached for ever.
  - The classes reach the protocol layers as three sentinels in
    `internal/xoauth2` (`ErrReauthRequired`, `ErrProviderMisconfigured`,
    `ErrTokenUnavailable`), which `internal/sync` maps to its auth, config and
    transient error kinds without importing the grant store.
- **Effects of `needs_reauth` (increment 4):** the runner skips the account
  without dialling; writeback intents are held, never dead-lettered while
  waiting; `EmailSubmission/set` refuses new sends and an in-flight one is
  held until the reconnect, not failed (a later decision
  replaced the design's "fails with 'reconnect this account'"; §11); the
  transition shows a banner and sends one
  Web Push, with no repeats; `Account/getHealth` gains
  `auth{kind, provider, state, reason, lastRefreshAt}`. A fresh token that
  IMAP still rejects is reported as "IMAP access is off or not permitted for
  this mailbox" (the Outlook.com IMAP toggle, or IMAP disabled for an M365
  mailbox).

### 6. Disconnect

Disconnect revokes at Google (best effort, its revocation endpoint), deletes
the secret, sets `sync_enabled = false` and **keeps the mail**. Microsoft has
no programmatic revocation for delegated tokens (UNVERIFIED); the UI links to
the account's app-permissions page. "Remove account" stays the separate,
existing action.

### 7. Protocol mechanics

- **XOAUTH2 behind our own interfaces** (spec §3): `sync.Config.Auth`
  (`sync.XOAuth2Auth(user, tokenFunc)`) for IMAP and `provider.SendRequest.Auth`
  (`provider.XOAuth2Auth` / `provider.PasswordAuth`) for SMTP. No go-imap,
  go-sasl or net/smtp type crosses either seam. `Config.Auth` nil is LOGIN with
  Username/Password exactly as before; compose's `Keys.SMTPAuth` returns the
  mechanism instead of a username and password.
- **The token is fetched at authentication time**, once per connection, so a
  Config held for a long pass or an IDLE watcher never carries a stale one.
- **The failure challenge is answered with an empty response.** On failure
  Gmail sends a base64 JSON challenge (`{"status":"401",…}`) and waits; a client
  that does not answer hangs. Only the challenge's `status` is ever read into an
  error. go-imap encodes an empty continuation response as `=`; a server that
  answers that with BAD instead of NO is still a credential verdict, because it
  sent the challenge first.
- **Classified by what it is, not by which step wrapped it.** A refusal of
  AUTHENTICATE (the challenge path, or Microsoft's bare `NO AUTHENTICATE
  failed.`) is an authentication error on every op — the bug #888 fixes for
  LOGIN cannot recur on this path. A server that does not advertise
  AUTH=XOAUTH2 is never sent the token and is a configuration error.
- **SMTP XOAUTH2 refuses a connection without TLS** unless the server is
  localhost, and refuses a server name other than the host the token was meant
  for: `net/smtp`'s `PlainAuth` rules, which a bearer token deserves as much as
  a password. On top of that the sender still refuses to authenticate over
  cleartext at all outside the in-process test servers.
- **`535 5.7.139` (SMTP AUTH disabled for the tenant, exact text UNVERIFIED)**
  keeps a Microsoft account receive-only with an explanation instead of failing
  the connect (increment 6). Graph `sendMail` is out of scope.

### 8. Threat model

- **Blast radius.** `https://mail.google.com/` is full mailbox control — read,
  send, permanent delete — and is the only scope that works for IMAP.
  Microsoft's scopes are narrower: IMAP and SMTP only, no Graph, files or
  calendar. A compromised instance (the process, or the database plus the
  master key) can read, send as and delete for every connected user until each
  revokes or the token dies — spec §9's "OAuth tokens" row, still accurate.
  The database alone yields ciphertext only. Compared with passwords OAuth is
  strictly better: revocable per app, never the account password, and on
  Microsoft limited to the mail protocols.
- **Consent honesty.** Before the redirect, a Mailroom interstitial says in
  plain words that whoever runs this server can technically read and send mail
  from the account, and that Google will warn the app is unverified. The
  consent screen's app name is the instance's, not "Mailroom" as a brand. The
  Google API Services User Data Policy applies even unverified; the operator
  guide says so.
- **Scope minimization.** Exactly the registry's scopes plus `openid email`;
  partial grants refused; no Gmail API or Graph scopes.
- **Logging.** Tokens are `xoauth2.Secret`; provider error bodies are reduced
  to the parsed `error` code and a scrubbed, capped `error_description`; the
  XOAUTH2 challenge is read for `status` only. A negative-control test runs the
  refresh, rotation, CAS and failure paths with sentinel tokens and asserts no
  sentinel reaches a captured log line or error string; it is proven able to
  fail (a deliberately logged token turns it red).
- **Isolation.** Both tables carry FORCE ROW LEVEL SECURITY with
  deny-by-default user policies and are in the required negative suite
  (data-model §5): user B can neither see, consume, rotate nor re-state A's
  flow or grant, nor attach either to A's account (the inserts select from
  `app.accounts`, because a foreign key alone is checked without RLS).
- **CSRF and login-CSRF.** Start is a same-origin JSON POST; the callback
  state is single-use and bound to the user, which defeats "the attacker
  connects their own Gmail into the victim's Mailroom" (the victim's sends
  would land in the attacker's Sent folder). No open redirect. Per-provider
  callbacks plus `iss` defend against mix-up. API keys cannot start a flow.
- **Exfiltration.** Hosts fixed per provider and pinned at dial time (a row
  naming another host is refused), connection fields immutable, the egress
  guard kept on every dial, and an OAuth IMAP session refuses `TLSInsecure`
  and `InsecureSkipVerify` outright. The token endpoint is the registry's own
  constant, so its request does not go through the account egress guard: there
  is no user-controlled host to rebind.
- **Residual risks.** Go strings holding tokens cannot be zeroed. The instance
  owner can always read the mail: admin blindness is policy, not cryptography
  (spec §9, §10).

### 9. Increment 3 obligations

Recorded by the security review of #894 so they are not lost; none is built
by increment 2.

- **(a) A reject hook.** Replace `xoauth2.TokenFunc` with a source offering
  `Token(ctx)` and `Reject(tok)`, and call `Reject` on an IMAP
  `authRefusedError` and on an SMTP 535 after XOAUTH2, so §5's "refresh once
  more before judging the grant" actually happens (it ends in
  `Manager.Invalidate`).
- **(b) `Manager.Forget(user, account)`** on disconnect and on account
  deletion, so no access token outlives its grant in memory.
- **(c) A reconnect that re-seals the refresh token bumps `generation` in the
  same statement** (or deletes and recreates the grant in one transaction), so
  a worker holding the old generation cannot overwrite the new token.
- **(d) An `UpdateOAuthGrant` for re-auth** (scopes, address, state back to
  active): `UNIQUE (user_id, provider, subject)` correctly blocks a second
  `CreateOAuthGrant` for the same provider account.
- **(e) `Account/set` refuses connection-field changes on
  `auth_kind = 'oauth2'` accounts.** Today a password re-seal there would
  replace the sealed OAuth secret, orphaning the grant while `auth_kind` stays
  `oauth2`; with the host pin above the account would then simply fail to
  connect, but the state is incoherent and must be impossible.

### 10. Increment 3 as built (the connect flow)

Recorded where the build made a choice the sections above leave open.

- **Configuration.** `MAILROOM_OAUTH_{GOOGLE,MICROSOFT}_CLIENT_ID` and
  `_CLIENT_SECRET` (every one honors `_FILE`), `MAILROOM_OAUTH_MICROSOFT_TENANT`
  (default `common`), `MAILROOM_PUBLIC_URL` (default: the origin of the OIDC
  redirect URL). A half-configured provider, a malformed tenant, a cleartext
  public URL (other than loopback) or a configured provider with no public URL
  at all refuse to boot. The exact redirect URIs are logged at boot. One token
  `Manager` serves the sync resolver, the send worker and the connect flow.
- **The ID token's signature is not verified**; its claims are (issuer per
  provider, audience and `azp`, expiry, issue time, the flow's nonce, a usable
  subject, Google's `email_verified`, Microsoft's `tid` against its issuer and
  the operator's tenant narrowing). OIDC Core §3.1.3.7 (6) permits TLS to the
  token endpoint in place of the signature for a token received directly from
  it, which is this flow: the endpoint is the registry's constant, no redirect
  is followed, and the request carried the client secret and the PKCE
  verifier. A party able to forge that response could forge the access token
  beside it. The XOAUTH2 IMAP sign-in is the arbiter of the address; the
  subject alone binds identity (`internal/oauthmail/idtoken.go`).
- **Granted scopes**: an omitted `scope` in the token response means "as
  requested" (RFC 6749 §5.1); comparison is case-insensitive. Whether
  Microsoft returns the IMAP and SMTP scopes in exactly the requested form is
  UNVERIFIED; increment 0b settles it.
- **The callback consumes the flow first**, before reading `error` or `code`,
  so a state is good for exactly one callback, refused or not. A signed-out
  browser reaching the callback is sent to sign-in with the PATH only:
  `auth.WithUnauthenticatedRedirect` drops a query carrying `code` or `state`
  so it never travels into `return_to`.
- **Result codes**, redirected to `/?oauth=<code>&provider=<p>[&account=<id>]#/settings`:
  `connected`, `reconnected`, `already_connected`, `denied`, `provider_error`,
  `invalid_request`, `invalid_state`, `expired`, `provider_mismatch`,
  `exchange_failed`, `identity_invalid`, `missing_scope`, `no_refresh_token`,
  `mailbox_refused`, `mailbox_unreachable`, `wrong_account`, `not_configured`,
  `server_error`.
- **A new account** is named by its address (then "address (Gmail)" and
  numbered variants on a name clash), takes the registry's hosts and the
  provider's profile (`gmail`, `microsoft`) and a fixed identity for the
  address. A Microsoft account syncs at once.
- **The Gmail sync hold** (decision). Until the Gmail
  sync model lands (ADR-047, increment 5a), syncing a Gmail account would walk
  every label folder with no download budget, which can trip Google's daily
  IMAP limit and suspend the person's IMAP everywhere, their phone included.
  So a connected Gmail OAuth account is created (and re-attached on
  reconnect) with **sync off** and ingest-only, and every door that could
  switch it on refuses: `Account/set syncEnabled:true` (invalidProperties,
  code `gmail_sync_pending`) and `mailroom-admin enable-account` (not even
  with `--force`). The reason is reported by `Account/getHealth` as
  `syncHold: {code: "gmail_sync_pending", message: "Gmail sync arrives in the
  next update; your account is connected and ready."}`. It is **computed**
  (`oauthmail.SyncHold`, from `auth_kind`, `provider_profile` and one code
  constant), not stored: no account-status column fits a reason that is a
  property of the server build rather than of the account, and a computed
  health signal owes no change_log row. Increment 5a flips
  `oauthmail.GmailSyncReady` to true, the one line that lifts the hold.
  Password (app-password) Gmail accounts are not held: they are the user's
  explicit choice through the ordinary form, as before.
- **Nothing is revoked on a refused connect.** Revoking a Google token revokes
  the whole grant for this client and user, which would kill an existing
  connection of the same Google account (the duplicate and wrong-account
  cases).
- **Disconnect** (`Account/oauthDisconnect`) deletes the grant, re-seals the
  credentials without the refresh token but WITH the provider and the subject
  (`OAuthSecret.Subject`), switches sync off and keeps the account and its
  mail; one Account change row and an `oauth.disconnect` audit row recording
  the revocation outcome (`revoked`, `failed`, `unsupported`,
  `not_configured`). Revoking at Google ends that Google account's grant for
  this client everywhere, including another Mailroom user's connection of the
  same Google account: accepted, and rare. A disconnected account is
  reconnected with `Account/oauthStart {id}` and only for the same subject.
- **Flows are swept** hourly per active user (a ticker in cmd/mailroom, the
  API-key approval sweep's shape), and each `Start` clears the starting user's
  expired flows.

**Security review of #897** (independent, at `5db215f`): no high findings.
- **M1, fixed:** deleting an OAuth account (`Account/set` destroy) now
  revokes its grant at the provider (Google). A restored backup would
  otherwise re-arm a still-live grant. What to revoke is read before the delete
  (`Connector.PrepareRevoke`), and the revocation runs after the commit, only
  for accounts actually destroyed, bounded at 15 seconds. It never fails the
  delete, and outcomes are logged with ids only. Password accounts revoke
  nothing.
- **M2, fixed:** the token-endpoint client owns its transport, a clone of
  `http.DefaultTransport` taken at init, with a TLS 1.2 floor and the proxy
  taken explicitly from the environment. Redirects are still refused. This
  makes the "TLS to the token endpoint stands in for the ID-token signature"
  argument above (OIDC Core §3.1.3.7(6)) a property of this code rather than
  of whatever another imported package does to the shared default transport.
- **L1, fixed:** the FINAL refusal of a token is rejected too, on SMTP and,
  for the same reason, on IMAP. A token refused twice no longer stays cached
  for the next send or connection.
- **L2, accepted:** a disconnect racing a re-authentication of the same
  account ends in a consistent state, whichever commits last, so neither is
  serialised further. A refresh token written by the re-auth in that window
  is overwritten without being revoked; on Google, revoking the old token
  revokes the whole client+user grant, so it dies anyway, and Microsoft has no
  per-token revoke.
- **L3, accepted:** an account disconnected before the provider subject was
  kept in the sealed envelope cannot be reconnected (`wrong_account`). It fails
  closed, and no such account can exist: connect did not exist before this
  increment.
- **L4, accepted:** when a token response omits `scope` (allowed by RFC 6749
  §5.1), the requested scopes are recorded as granted. IMAP access is proven
  independently by the XOAUTH2 sign-in during connect; `SMTP.Send` is not, so
  the audit row may overstate the grant. A missing send scope surfaces as a
  refused send, not as a silent failure.

**How §9's obligations are met.**
(a) `xoauth2.TokenSource{Token, Reject}` replaces `TokenFunc`;
`Manager.TokenSource` maps Reject to `Invalidate`. IMAP: an
`authRefusedError` rejects the token and retries AUTHENTICATE once on the same
connection (`internal/sync/auth.go`). SMTP: a 535 to XOAUTH2 rejects the token
and retries once on a fresh connection (`provider.SMTPSender.authenticate`); a
password 535 is never retried. A failure to obtain a token is permanent for a
send unless it is transient.
(b) `Manager.Forget` on disconnect and after an `Account/set` destroy; a refresh
in flight across a Forget does not refill the cache (a per-account forget
epoch).
(c) `UpdateOAuthGrant` writes the credentials in the statement that bumps the
generation; a recreated grant (reconnect after disconnect, or any connect)
starts at a random generation in [2^32, 2^62), so a worker holding a
pre-disconnect generation cannot win a compare-and-swap against it.
(d) `store.UpdateOAuthGrant`, refusing another provider or subject
(`ErrOAuthSubjectMismatch`).
(e) `Account/set` refuses connection fields on an `oauth2` account before
sealing or dialling, and `store.UpdateAccount` refuses a `Connection` patch on
one (`ErrOAuthConnectionImmutable`) for every caller, the admin CLI's
`set-account-credentials` included.

### 11. Increment 4 as built (health and UX)

Recorded where the build made a choice §5's "effects of `needs_reauth`" and
the design leave open.

- **`authState` and friends on Account.** Four read-only properties:
  `authKind`, `authState`, `authProvider` and `authAddress`. `authState` is null
  for a password account. For an OAuth account it is the grant's state, or
  **`disconnected`** when there is no grant: the fourth value lets the client
  offer Reconnect after `Account/oauthDisconnect`. `authProvider` comes from the
  provider profile when the grant is gone. Every change to these four is
  already a grant creation, re-auth, transition or deletion, and each of those
  appends the Account change row (§4). No new write path, so no new row.
  `store.Account.AuthState` is read by a scalar subquery in the shared account
  SELECT, so `UpdateAccount`'s `FOR UPDATE` still locks one row of one table
  (`internal/store/accounts.go`).
- **`Account/getHealth.auth`**: `{kind, provider, state, reason,
  stateChangedAt, lastRefreshAt}`. `reason` is the recorded OAuth error code,
  already reduced to `[a-z0-9_]`. The refresh time is reported here, in a
  computed read, and not on Account/get, so §4's "rotation and the refresh
  stamp append no row" still holds.
- **"Waiting for a reconnect" means `needs_reauth` OR disconnected.** An
  OAuth account with no grant (`Account.AwaitingReconnect()`, and its SQL twin
  in `store.ListSyncableAccounts`) is treated exactly like one whose grant is
  `needs_reauth` everywhere below: not dialled, intents held, new sends
  refused, accepted ones held. (Review of #903, MEDIUM-2: a disconnected
  account's sends otherwise claimed, failed on `ErrNoGrant` and requeued every
  pass, two change rows each time, for ever.)
- **The sync runner never dials an account waiting for a reconnect.** The
  rule is in the SQL behind `store.ListSyncableAccounts`, which every
  scheduled pass, one-shot pass, writeback nudge and IDLE reconcile lists from.
  A reconnect returns the grant to active and the next pass lists the account
  again, with nothing to switch back on. `CredentialResolver.Prepare` refuses
  the same accounts before opening anything (`ErrAwaitingReconnect`, an
  `ErrReauthRequired`). That covers the admin CLI too. A running IDLE watcher
  holds the account it was started with, so before every session it re-reads
  the account (`AccountReader`, one query) and ends without a dial if it now
  waits for a reconnect; without that it would redial until `reconcileIdle`
  next dropped it (review of #903). Each scheduled pass names the waiting
  accounts at WARN (`ReconnectLister`), as #435 does for disabled ones.
  `operator_error` is listed and dialled as before, on the normal backoff. The
  one connection still possible is the IMAP session of a pass that was already
  running when the transition happened.
- **Writeback intents are held** by a guard in `writeback.Runner.DrainAccount`,
  read inside its transaction: no dial, intents left `pending`, attempts
  untouched. The first drain after the reconnect applies them.
- **Sending: refuse new submissions on an account waiting for a reconnect,
  hold accepted ones; hold, never fail, on `operator_error`.**
  `EmailSubmission/set` refuses a create on an account waiting for a reconnect
  with the SetError type **`accountNeedsReconnect`**, before anything durable
  exists (the ingest-only gate's position). The person is at the composer, and
  the draft is kept. Accepting the send would queue mail behind a reconnect
  that might never happen. A submission accepted *before* is **held, never
  failed** (decision: "waiting outgoing mail rather than failures"):
  - The send worker checks before the claim. A held submission stays
    `scheduled`, its response says "waiting: reconnect this account in Settings
    and it will be sent" (written once, so a long wait appends one change row),
    and a fresh job looks again every 5 minutes without spending the retry
    budget.
  - **The hold's write is guarded** (`store.MarkSubmissionHeld`: the row is read
    `FOR UPDATE` under `state = 'scheduled'`). The worker's first read is not
    locked, and an Undo Send can commit after it; an unguarded write put the
    cancelled submission back to `scheduled` and sent it after the reconnect
    (review of #903, MEDIUM-1). Now that cancel stands and nothing is requeued;
    a cancel arriving after the lock waits for it and cancels the job the hold
    linked. A claimed submission returns to `scheduled` only from the
    `'sending'` state it claimed (`HoldClaimedSubmission`).
  - If the token refresh hears `invalid_grant` during the send, the error
    carries `ErrReauthRequired` (checked before `ErrPermanent`), and the claimed
    submission returns to `scheduled` the same way.
  - **`operator_error` holds instead of failing** (decision recorded on review
    of #903). The provider refusing this server's client — an expired or
    rotated secret — used to reach the worker as `ErrProviderMisconfigured`,
    which the SMTP layer wraps as `ErrPermanent`, so every user's queued mail on
    that provider failed for good: the opposite of "waiting outgoing mail
    rather than failures", and none of it the users' doing. Now new sends on
    such an account are ACCEPTED, and the worker holds them with the stable
    reason "Sign-in to <Provider> is temporarily unavailable on this server;
    your message will be sent when it is fixed", with the same write-once and
    fresh-job rules, both before the claim and when the refusal arrives
    mid-send. Before holding, it asks the token Manager for a token (the
    optional `ProbeToken`): that is what notices the operator's fix on an
    account no sync pass refreshes (a held Gmail account has sync off), and the
    next look sends.
  - `EmailSubmission.mailroomHeld` (`"reconnect"` | `"provider"` | null) says
    which hold a scheduled submission is in, so the Outbox shows "waiting"
    instead of "Sending…" without matching reason text. It is derived from
    the stored reason, which only the worker writes, from fixed constants
    (`store.SubmissionHeldKind`), and each change to it already appends the
    submission's change row.
  - Not done: neither a reconnect nor the operator's fix wakes held
    submissions early. They go out on their next look, within 5 minutes.
- **One Web Push per transition into `needs_reauth`: a transactional
  outbox.** `StoreGrants.SetState` queues an `oauth_reauth_notify` job in the
  same transaction as the transition. The idempotency key is the account plus
  the transition time, and the payload is the account id and that time. The
  guarantee is built in layers:
  - `SetOAuthGrantState`'s `FOR UPDATE` means only one worker, in one process or
    several, makes the transition.
  - The notice commits exactly when the transition does.
  - A restart cannot make a second one: a `needs_reauth` grant is never even
    refreshed.
  - The worker sends only if the grant is still in that transition, so a user
    who already reconnected is not told to.
  - Quiet hours defer the notice with a fresh job rather than dropping it. It is
    the only one the transition gets.
  - The notice goes to every device whatever its mail mode, because it is not
    about new mail.
  - Residual window: the dispatcher is at-least-once, so a process that dies
    after the push service accepted the notice, but before the job is marked
    done, sends it again on retry. The same holds when the job fails after a
    partial send — for example `MarkPushSubscriptionExpired` erroring for one
    gone device after the others were sent to: the retry sends again to those
    others. Accepted: a rare duplicate beats a missed notice.

  The banner is client-side, from `Account.authState`. It can be dismissed for
  the session, for that exact set of accounts.
- **`operator_error` is not pushed to users, and the operator channel is
  getHealth plus the log.** Mailroom has no delivery path to "the operator"
  other than the ERROR line the token Manager already logs. A per-admin push
  would need a new SECURITY DEFINER to list admin users across RLS, a
  migration out of proportion to the notice. So `Manager.OperatorIssues()`
  keeps, in process memory, each provider currently refusing this server's
  client: the code, first and last seen, and the number of accounts affected.
  It is cleared by that provider's next successful refresh, and re-established
  after a restart by the next refresh, because `operator_error` keeps
  refreshing. `Account/getHealth` returns it as `operatorNotices` only to a
  caller whose own `app.users.role` is `admin` (read through `users_self`, with
  no new definer). Settings › Accounts shows it to admins.
- **Web.**
  - The Connect buttons follow the `oauthProviders` capability. The honesty
    interstitial is a modal in the settings dialog shell, and Continue calls
    `Account/oauthStart` and follows only an `https://` URL. There is no host
    allowlist on that URL: it comes from an authenticated, same-origin JMAP
    response that the server built from its own registry, so a host check in
    the client would only restate the server's. Accepted.
  - The return query is read once and removed with `history.replaceState`. Only
    the eighteen codes are recognised, so a crafted link cannot choose the
    words.
  - An OAuth account's settings replace the server editor with a Sign-in group.
    `operator_error` offers no Reconnect.
  - Reconnect from the banner, or for a disconnected account, goes straight to
    the provider without the interstitial: the access was already explained and
    consented to when the account was connected.

### 12. Increment 6 as built (the Microsoft profile)

Recorded where the build made a choice the design (§3.5 of the spike) leaves
open. Everything marked UNVERIFIED is for a live test with a real
Outlook.com and a real Microsoft 365 mailbox.

- **Folder map, stated not guessed** (`internal/provider/profile.go`). A
  top-level folder listed WITHOUT a SPECIAL-USE attribute gets one from its
  English name: Sent Items `\Sent`, Drafts `\Drafts`, Deleted Items `\Trash`,
  Junk Email / Junk E-mail `\Junk`, Archive `\Archive`. SPECIAL-USE always
  wins. Whether Exchange sends SPECIAL-USE is UNVERIFIED; a mailbox whose
  language is not English has localised names this map does not cover. The
  map applies to any account whose `provider_profile` is `microsoft`.
  Recorded risk (review L8): in a mailbox that mixes languages, or where the
  server sends SPECIAL-USE on a localised folder AND the user also has an
  English-named one (say "Sent Items" next to a server-marked "Gesendete
  Elemente"), two folders can carry the same role; role lookups then pick one.
- **Non-mail folders need evidence, not just a name** (review M3 of #910).
  A folder whose top-level name is on the profile's non-mail list (Calendar,
  Contacts, Journal, Notes, Tasks, Outbox, Sync Issues, Conversation History,
  RSS Feeds/Subscriptions, and anything nested under them) is set
  `sync_scope='off'` only on the pass that CREATED its row
  (`StoredMailbox.Created`, from the upsert's `xmax = 0`) and only if the
  server marks it `\Noselect` or its first SELECT shows it EMPTY. Anything
  else keeps syncing. A folder the user later switches back on is never
  switched off again, cursor or not (review M-H2). The risk this guards
  against is real: a user's own mail folder called "Notes" or "Tasks", and
  every folder under it, would otherwise never sync. In a localised mailbox
  ("Kalender", "Notizen") the names do not match at all, so Exchange's
  non-mail stores there simply sync as (empty) folders. Whether Exchange
  lists them, and empty, is UNVERIFIED.
- **Send-as:** unchanged from §10. One fixed identity for the address.
  `5.2.252 SendAsDenied` (code UNVERIFIED) is its own permanent cause
  (`provider.ErrSendAsDenied`).
- **Outbound quota:** 100 submissions a day for a new Microsoft account
  (`provider.OutboundDailyQuota`). Microsoft's limits are UNVERIFIED and
  recipient-based. 100 is cautious against every published figure, and going
  over the real limit locks the mailbox's sending everywhere. It equals the
  generic schema default today; it is stated separately so it does not move
  if that default does.
- **`535 5.7.139` makes the account RECEIVE-ONLY**, recorded on
  `app.account_sync_status` (`send_blocked_code`, migration 00080). This is
  operational state, not an Account property, so it appends no change row.
  - Detection: the SMTP sender matches the enhanced code (exact text
    UNVERIFIED) and returns `ErrSMTPAuthDisabled` + `ErrPermanent` without
    handing the token back.
  - Set by: the connect/reconnect probe (`Connector.checkSending`, Microsoft
    only; the account still connects), or a send that hits it.
  - Cleared by: a successful probe on reconnect (the person's way to recheck
    after an administrator changes the setting), or a password account's
    connection change. A probe failure for any other reason changes nothing.
  - Effects: `EmailSubmission/set` refuses with `accountCannotSend` and the
    reason, writing nothing. A queued submission **fails** with the same
    reason without dialling. `Account/getHealth.sendBlocked` shows it.
    Recorded (review L8): the daily quota is reserved in the claim
    transaction BEFORE the receive-only check, so a queued submission on a
    blocked account whose quota is already spent is DEFERRED to the next
    day (the over-quota path) and only then fails with the receive-only
    reason. It is never sent; it just fails a day later.
  - **Failed, not held — a deliberate departure from §11's hold model.** A
    hold is right when the fix is near and certain: a Reconnect click, or the
    operator rotating a secret. 5.7.139 is an organisation's security policy.
    The fix is an administrator's decision that may never come, so a hold
    would show "Sending…" for days over mail the person needed to send today,
    and every recheck would be one more refused AUTH against the mailbox. The
    message itself is not lost: it stays in Mailroom as the failed
    submission's Email. Graph `sendMail` stays out of scope.
- **IMAP-off hint.** Microsoft answers both a stale token and IMAP switched
  off with a bare `NO AUTHENTICATE failed.`. The two are told apart by the
  retry (§5): a refusal of the token the provider issued in place of a refused
  one is `sync.ErrIMAPAccessRefused`, and the sync runner records the
  profile's hint in front of it (`provider.IMAPAccessHint`). That is what
  `getHealth.syncStatus.lastError` shows: "IMAP access is off for this
  mailbox. On Outlook.com, turn it on in Outlook settings (Mail, Forwarding and
  IMAP)…". It remains `ErrorKindAuth`. It does not set `needs_reauth`, since
  the grant is evidently fine.
- **The CONDSTORE-less flag sweep** (`internal/mailsync/flags.go`, generic,
  not Microsoft-only). Exchange's IMAP has no CONDSTORE (pre-auth probe;
  post-auth UNVERIFIED), and the engine reconciled existing messages' flags
  ONLY through CONDSTORE.
  - Scope: it runs only when the server advertises neither CONDSTORE nor
    QRESYNC, never on the Gmail model.
  - Schedule: every `MAILROOM_FLAG_SWEEP_INTERVAL` (5m) it re-reads FLAGS for
    the top `MAILROOM_FLAG_SWEEP_WINDOW` (500) UID numbers below the cursor.
    Every `MAILROOM_FLAG_SWEEP_FULL_INTERVAL` (6h) it walks the rest in
    2,000-UID steps per pass.
  - Applies only a reading that **differs** from the copy's recorded remote
    flags. This is what keeps it as safe as CONDSTORE: the flag refresh
    recomputes a message's flags from its copies, so applying an unchanged
    reading would undo a local edit whose writeback is still pending.
  - Applies nothing from a command that failed. The schedule is in memory: a
    restart only makes it run sooner.
  - Review of #910: it runs AFTER the new-message walk and its failure is
    recorded, never fatal (M5), so a server that refuses its FETCH cannot
    hold back new mail. On a mailbox not known to keep user keywords (no
    PERMANENTFLAGS `\*` from this SELECT or the recorded probe) only system
    flags are compared and applied, and the copy's recorded keywords are
    carried through (M4): a server's silence about a keyword it cannot store
    is not a removal. A copy whose message has a flag or keyword writeback
    still pending or inflight is skipped until it drains (L6).
- **#909 (absent versus empty FLAGS).** go-imap patch 2 makes `FLAGS ()`
  non-nil; every refresh path skips a message whose reply carried no FLAGS.
  A new message whose body arrived without FLAGS is still ingested, and its
  flags are re-read once the command has finished. A CONDSTORE pass that met
  a reply without FLAGS keeps the old HIGHESTMODSEQ so the message is asked
  about again (review L7). A new message whose re-read also lacks FLAGS
  stays flag-less until its MODSEQ moves (CONDSTORE) or the sweep reads it
  (no CONDSTORE); the counter reports how often.

## Consequences

- OAuth accounts sync and send through the same engine, writeback and outbound
  ledger as password accounts; only the authentication step differs.
- Until increment 3 configures providers, no OAuth account can exist. An
  OAuth account reaching a path without a token source (the sync resolver,
  compose) fails with a cause naming that, and is never dialled with LOGIN.
  The other credential openers (ManageSieve out-of-office, the admin CLI's
  credential tools, the census) are still password-only: for an OAuth account
  they would present an empty password and be refused, never send a token.
  Gmail and Outlook have no ManageSieve, so their profiles turn it off; the
  admin tools learn OAuth when they need to.
- The operator gains a calendar duty: the Entra client secret expires (within
  24 months, UNVERIFIED), and its expiry is an `operator_error` for every
  Microsoft user at once.
- Everything marked UNVERIFIED here and in the design spike is settled by a
  live test with real accounts (increment 0b) before the release.

## Alternatives considered

- **Refresh token in its own column or table.** Rejected: a second
  sealed-secret location for the drill, the backups and the admin report to
  learn about, for no gain.
- **Persist access tokens.** Rejected: a dump would then hold usable bearer
  tokens; the saving is one HTTPS call per account per restart.
- **A change row per refresh.** Rejected (§4).
- **Signed-cookie flow state, as login uses.** Rejected for the connect flow:
  not single-use, not bound to the session user, not auditable.
- **golang.org/x/oauth2 for refresh.** Not used for the refresh call: its
  `RetrieveError` carries the raw response body into `Error()`, and the
  classification needs the parsed code. It remains available for the code
  exchange in increment 3.

## References

- Forgejo #52; the v1.96 design spike (posted on #52).
- ADR-013 (HKDF-derived credentials key), ADR-014 (the change-row narrowing
  this follows), ADR-028 (ingest-only mode), ADR-047 (the Gmail sync model).
- Spec §3 (anti-corruption layers), §9 (threat table), §18.4.
- Code: `internal/xoauth2`, `internal/oauthmail`, `internal/sync/auth.go`,
  `internal/provider/smtpauth.go`, `internal/store/oauth_grants.go`,
  `db/migrations/00078_oauth_grants.sql`.
