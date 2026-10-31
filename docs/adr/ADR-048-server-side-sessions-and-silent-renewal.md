# ADR-048: Server-side sessions, silent renewal and signed-in devices

**Status:** Accepted (v1.98.0; §9 measured against a real
OIDC provider, results in §9). The decisions
are in §1. Implements spec §10's "short-lived access tokens +
rotating refresh", which the stateless session cookie stood in for since v0.x
(`internal/auth/doc.go`, `Config.SessionTTL`). Revised after an adversarial
design review; §4's locking, §4a's identity refresh and
§3's 503 rule are the substantive changes it caused.

## Context

A browser session today is a stateless HMAC-signed cookie carrying the user id
and an expiry 12 hours out (`auth.DefaultSessionTTL`; `cmd/mailroom` never sets
another value). When it expires, the browser goes back through the OIDC
provider. That has three costs.

- **Users sign in again and again.** It happens every 12 hours and on every
  device. Where the provider's own session is short, the bounce is a password
  prompt (Authentik's default is "until the browser closes"). Users
  reported doing this several times a day.
- **A session cannot be ended early.** There is no server-side record to
  revoke. Disabling a user or ending their provider sessions leaves any open
  Mailroom session working until its cookie expires
  (`internal/auth/logout.go`, "What this still does not do"). Signing out
  clears this browser only.
- **Identity is re-read only at sign-in.** `app.upsert_login` re-applies the
  user's role (the admin group) and email on every login. That makes the
  12-hour expiry, in effect, the role-refresh interval. Any longer session has
  to keep doing this, or it becomes a privilege-retention bug (§4a).

## Decision

### 1. The decisions

- **Idle limit: 30 days, owned by the provider.** It is the provider's
  refresh-token validity, so an operator changes it in one place.
  Mailroom adds no idle timer of its own.
- **Renewal: every 15 minutes of use.** A session is re-confirmed with the
  provider at most every 15 minutes, and only when it is used.
- **A signed-in devices list**, with Sign out per device and "Sign out all other
  devices".

### 2. The session record

`app.sessions` (new migration). One row per signed-in browser:

| column | meaning |
|---|---|
| `id` | ULID |
| `user_id` | owner; every index leads with it |
| `token_hash` | SHA-256 of the cookie's secret, `UNIQUE`. The secret itself is never stored, so a database read yields no usable cookie (ADR-042's rule for API keys) |
| `refresh_ct`, `refresh_dek` | the provider refresh token, sealed under the user's key (ADR-013) with the user id and session id as associated data. NULL when the provider issued none (§4) |
| `id_token_ct` | the latest ID token, sealed the same way, used only as logout's `id_token_hint` (§5) |
| `generation` | rotation counter (§4) |
| `created_at`, `last_seen_at`, `last_checked_at` | sign-in time; the last request (written at most once a minute); the last successful provider confirmation |
| `user_agent`, `last_addr` | what the devices list shows (§6); `user_agent` capped at 512 bytes |
| `revoked_at`, `revoked_reason` | `signed_out`, `signed_out_elsewhere`, `replaced` (§3), `refused` (the provider or the resolver said no), `expired` |

- **RLS.** FORCE ROW LEVEL SECURITY with the usual
  `user_id = app.current_user_id()` policy. The table joins the required
  negative suite: user B can neither list, read, revoke nor renew A's sessions.
- **Owner policies.** The lookup function (§3) and the sweep (§7) run as
  `mailroom_owner`, so each needs an explicit owner policy mirroring its own
  predicate, in the `api_keys_token_lookup` pattern. The sweep's DELETE needs a
  SELECT policy as well: without one, `DELETE … WHERE` succeeds silently on
  zero rows. Both policies are in the negative suite.
- **No change_log rows.** Sessions are not JMAP-visible objects, so writes
  append none. That puts them in the same class as `push_subscriptions` and
  `api_keys`. The devices list (§6) is a read on demand, not a synced object.

### 3. The cookie and the per-request check

- **The cookie** (`mailroom_session`, the same name) holds `s1.` plus 256
  random bits in base32, with HttpOnly, Secure, SameSite=Lax and Path=/. It
  carries no user id and no expiry of its own: the server decides. Its browser
  `Expires` is 30 days, re-sent on renewal so a closed browser keeps it.
- **The lookup** happens before Mailroom knows who is asking. It goes through
  one narrow `SECURITY DEFINER` function, `app.session_for_token(p_hash)`,
  modelled on `app.api_key_for_token`. It does an exact hash match only, and
  returns the session id, user id and `last_checked_at`. It returns nothing for
  a revoked row or for a user who is disabled or deleted. It cannot enumerate.
  **Disabling a user in Mailroom therefore ends their sessions on the next
  request**; today it only blocks the next sign-in.
- **What counts as unauthenticated.** No cookie, a malformed cookie, an unknown
  hash or a revoked row: 401 on the API, the sign-in page in the browser,
  exactly as now.
- **A database error is 503, not 401.** It is logged, and the cookie is not
  cleared. The web client reads a JMAP 401 as "session expired" and sends the
  browser to sign in (`web/src/app/auth.ts`). A database blip answered with
  401 would bounce every open tab, which is exactly what this ADR removes. This
  is `BearerGate`'s existing rule.
- **A legacy cookie** (the signed v0.x format) is no longer accepted. Everyone
  signs in once after the upgrade (§8).
- **Sign-in replaces, never reuses.** The callback always mints a fresh secret.
  The anti-fixation rule is unchanged, and
  `TestCallbackHandler_ReplacesAPreExistingSession` stays. If the request also
  carried a valid session cookie, that row is revoked as `replaced`. This
  covers a second tab's login, and a cookie planted before the login, which
  then stops working. Without it, a replaced session would linger as a ghost in
  the devices list.

### 4. Renewal

- **When.** A request whose session has `last_checked_at` older than 15
  minutes renews **before** it is served, bounded as below. A fresh session
  costs one indexed lookup and nothing else.
- **How.** A server-to-server `grant_type=refresh_token` call at the provider's
  token endpoint, with the confidential client's credentials. No browser is
  involved, so nothing redirects, and a JMAP call or the installed PWA never
  sees a bounce.
- **Serialised, not raced.**
  - **Why.** Authentik treats a refresh token as single-use: presenting a used
    one again returns `invalid_grant` and raises a suspicious-request event.
    This is the opposite of the Microsoft behaviour ADR-046 §5 relies on, and
    §9 measures it. Two renewals presenting the same token therefore must
    never happen.
  - **In-process:** singleflight per session, detached from any one request's
    context.
  - **Across processes:** the renewal takes a per-session advisory lock
    (`pg_advisory_xact_lock` on the session id) in its own transaction before
    the token call. After taking the lock it re-reads the row. If
    `last_checked_at` is now fresh, another process renewed while this one
    waited, so it uses that result and makes no call.
  - **Storing:** the rotated token, the new ID token, `last_checked_at` and the
    bumped `generation` are written in that same transaction. The lock is held
    across the HTTPS call, which is bounded below.
  - **As built (increment 2).** The lock key is
    `hashtextextended('session-renew:' || id, 0)`, namespaced like the other
    advisory locks in the store. The re-read is `SELECT ... FOR UPDATE`, which
    on its own also serialises two renewals of one row; measured by removing
    each in turn, the advisory lock alone is not what turns the two-process
    test red (the row lock keeps it green), and removing both does. The
    advisory lock is kept as the stated mechanism (it does not depend on the
    row existing or on how the re-read is written), and the `FOR UPDATE` as the
    check that cannot be dropped by accident. The token request is made once,
    by hand, with the client credentials as HTTP Basic (RFC 6749 2.3.1), or in
    the body when discovery advertises only `client_secret_post`:
    `oauth2.Config.TokenSource` is not used because, with an unset auth style,
    it retries a failed request in the other style and would present the
    single-use token twice.
  - **As built: three phases (the security review of increment 2).** The
    lock is held only for phase A, and the session is confirmed only at the end.
    - **A, under the lock.** Re-read; if fresh or revoked, stop. Otherwise the
      one grant. Refused: revoke `refused`. Server refused or transient: write
      nothing. Success: in the same transaction store the rotated refresh token
      (or keep the stored one if none came) and the new ID token **only if it
      verified** (signature, issuer, audience, expiry; its subject must be the
      user's, else refuse), and bump `generation`, **without** stamping
      `last_checked_at`. A success consumes the old token at a single-use
      provider, so the rotation must be durable whatever happens next; stamping
      here would also confirm a session whose identity was never re-checked. The
      transaction's context outlives the 5-second provider budget by a reserved
      3 seconds, so the deadline cannot fall between the answer and the write.
    - **B, no transaction held.** Identity (§4a). **C.** `UPDATE ... SET
      last_checked_at = now() WHERE id = $1 AND generation = G AND revoked_at IS
      NULL`; zero rows (another process renewed since) still counts as confirmed
      for this request.
    - **Two processes.** A second process queued behind the lock re-reads
      after the first commits phase A. If the first has already confirmed, the
      second serves without a call; if it is still in phase B the session still
      looks stale, so the second renews once more with the *rotated* token. That
      costs one extra grant in a narrow window and never presents a token twice;
      the integration test asserts "no token presented twice" and at most two
      uses, not "exactly one".
    - **Pool and concurrency.** The resolver runs in B, so a renewal holds one
      pooled connection (in A), never two. Renewals in flight per process are
      bounded by `RenewConcurrency` (default 2); a request over the limit is
      served under the allowance with no provider call and is *not* recorded as
      a failed attempt, so it tries again on its next request. `store.Open` now
      sets the pool size explicitly (16, or the DSN's `pool_max_conns`).
    - **Re-review hardening.**
      - **An ID token that does not verify** (a JWKS refetch failing during key
        rotation, say) still has its rotation stored in A. The renewal is
        transient, with no UserInfo-only fallback, because UserInfo alone could
        demote an admin whose groups only the ID token carries.
      - **C reports a revoked row distinctly.** A sign-out that lands during B
        wins, and the request is refused rather than confirmed.
      - **Renewal slots.** A slot is released only by the acquire that took
        it. A concurrency below 1 still admits one renewal.
      - **A ceiling on the wait.** A request waiting on a renewal stops after
        3 × `RenewTimeout` + 2 × the write bound, and is served within the
        allowance. A wedged attempt therefore cannot hold the session's
        requests.
- **A crash between the provider rotating the token and Mailroom storing it**
  leaves the old token stored. Its next use is `invalid_grant`, which revokes,
  and the user signs in once. This is accepted: it needs a crash inside a
  5-second window, and it fails closed.
- **Bounded.** The token call has its own 5-second timeout, not ADR-046's 30
  seconds. On timeout the request takes the transient path, so a stalled
  provider delays each device's request by at most 5 seconds, and at most once
  a minute (the retry spacing below).
- **Outcomes.**
  - **Success:** store the results above (§4a runs first), stamp
    `last_checked_at`, and send the cookie again with a fresh 30-day
    `Expires`.
  - **Refused:** `invalid_grant`, `interaction_required`, `login_required` or
    `consent_required` mean the provider refused this user. The session is
    revoked with `refused`, and the request is unauthenticated.
  - **Server refused:** `invalid_client` / `unauthorized_client` mean the
    provider refused **this server**. The user is not punished for an operator
    problem: the request is served within the outage allowance, and the error
    is logged at ERROR.
  - **Transient:** network, timeout, 5xx, 429 and similar. The session is left
    as it is.
  - **Outage allowance: 12 hours.** While renewal fails for the last two
    reasons, a session keeps working until 12 hours after its last successful
    check, and is refused after that. Twelve hours is today's session length,
    so an outage never gives longer unconfirmed access than a stolen cookie
    gets now. During it, renewal is retried at most once a minute per session.
    `Config.SessionTTL` is renamed `OutageAllowance` to say what it now means.
- **No refresh token.** If the provider issued none (no `offline_access`), the
  session cannot renew. It lasts 12 hours from sign-in, exactly as today, and
  the log says why once per sign-in. An instance whose provider is not yet
  configured therefore keeps working unchanged.
- **Tokens never leave the server.**
  - The refresh and ID tokens travel as `xoauth2.Secret` (ADR-046 §2's
    redaction rules).
  - The provider's `error_description` is scrubbed and capped.
  - The token-endpoint client never follows a redirect.
  - The access token is used only for §4a's UserInfo call, then dropped.

### 4a. Identity refresh on every renewal

A renewal re-establishes who the user is, as a login does.

- **The ID token.** If the refresh response carries an `id_token` (optional by
  OIDC Core §12.2), it is verified with the same verifier as at login
  (signature, `iss`, `aud`, expiry; no nonce on refresh). Its `sub` must equal
  the session user's `app.users.oidc_subject`, read under the user's own
  UserTx. A mismatch revokes the session (`refused`).
- **The claims.** They come from the verified ID token, with the same UserInfo
  backfill as at login. Without an ID token, they come from UserInfo alone. The
  claims then go through **the same `ResolveUser` path as login**: `upsert_login`
  re-applies email, display name and role. Removing someone from the admin group
  at the provider therefore takes effect at their next renewal (≤ 15 minutes of
  use), as it took effect at their next login before.
- **Refusal.** Only a deliberate refusal revokes (`refused`): the resolver
  wrapping `auth.ErrRefused` (no email to provision from, a subject disabled
  or deleted in Mailroom, an email that belongs to another account, which also
  carries `ErrCannotProvision`), a UserInfo `sub` that is not the session
  user's, a UserInfo answer that still has no email, or a resolver that
  returns a different user id. Any other resolver error (a database reset, a
  serialization failure, a deadline) is a failure to decide: **transient**.
- **No half-checked identity.** UserInfo is **required** when there is no
  verified ID token, or the verified one lacks `email` or `groups`. A required
  call that fails is **transient**: nothing more is stored, the session is not
  marked fresh, and the outage allowance applies. If it succeeds and groups
  are absent they are genuinely none, so the empty list goes to the resolver
  (and an admin is demoted, as at login). A renewal that could not re-check
  identity must not count as one that did, and must never hand the resolver a
  partial claim set. (A provider that advertises no UserInfo endpoint cannot
  be asked, so only its ID token is used.) UserInfo's `sub` must match the
  ID token's, or the stored subject when there is no ID token (OIDC Core
  5.3.2); the same check applies at login, where a mismatch fails the sign-in
  and any other UserInfo failure stays best-effort.

### 5. Sign out

- **This device.** The row is revoked with `signed_out`, and its push
  subscriptions are deleted (§6). The refresh token is revoked at the provider
  through RFC 7009 when discovery advertises a `revocation_endpoint`; a failure
  there is logged, never shown.
- **RP-initiated logout** (`end_session_endpoint`) is kept. Its
  `id_token_hint` now comes from the session row's `id_token_ct`, which every
  renewal updates. The path-scoped logout cookie is **retired**: it expired
  after 12 hours, and from then on logout would silently have fallen back to
  local-only for the rest of a 30-day session.

### 6. Signed-in devices

A Settings › Security page, under a new capability `urn:mailroom:devices`. Two
methods, named to avoid RFC 8620's Session object:

- **`SignedInDevice/get`** lists the user's live sessions, each with:
  - a description parsed from the user agent ("Firefox on Linux", "Safari on
    iPhone"), with the raw string kept server-side;
  - `createdAt` and `lastSeenAt`;
  - `lastAddr`, the address it last connected from;
  - `isCurrent`.
- **`SignedInDevice/set` with `destroy`** revokes. For each session it:
  - revokes the row (`signed_out_elsewhere`);
  - revokes its refresh token at the provider;
  - deletes the push subscriptions registered from it;
  - closes its open EventSource streams within a minute.

  `destroy: ["*others*"]` does this for every session except the current one.
- **Not for API keys.** Neither method has a line in ADR-042's scope table, so
  every key is refused (deny by default; the walking test covers new methods).
  Keys keep their own page and are not sessions.
- **Dev-auth** has no sessions: the list is empty, and `destroy` is a no-op
  that reports `notFound`.
- **Immediacy.** Every request runs the §3 lookup, so a signed-out device is
  refused on its very next request, not at its next renewal.
- **Push:**
  - `push_subscriptions` gains a nullable `session_id` (`ON DELETE SET NULL`).
    `PushSubscription/set` fills it from the request's session, and the
    `(user_id, endpoint)` upsert **overwrites** it, so a re-registration moves
    the subscription to the new session.
  - Subscriptions from before this release have none. A device sign-out does
    not delete them, and the devices page says so.
  - **The client reconciles on connect.** If this browser holds a push
    subscription that `WebPushSubscription/get` does not list (deleted by a
    sign-out, expired, or never stored), it registers it again. Today the
    toggle is read from the browser alone (`web/src/app/push.ts`), so after
    signing out and back in it would show "on" while nothing arrives. That
    "ran and changed nothing" failure is closed here.
- **Streams.** The EventSource handler re-runs the §3 lookup every 30 seconds
  and closes when the session is gone. It also closes once the session is
  outside the outage allowance (no successful check for 12 hours). A stream
  check **does not count as use**: it stamps nothing and never triggers a
  renewal. A forgotten tab therefore cannot keep a session renewing forever
  against the 30-day idle limit.
- **The honest sentence.** Under the list, the page says: "Signing out a
  device you don't recognise ends that sign-in, but whoever used it may still
  know your password, so change your password in your sign-in provider too.
  Ending your sessions there doesn't sign Mailroom devices out; signing them
  out here does." The first draft advised ending the provider's sessions; §9's
  live test showed that does not reach Mailroom (the refresh token outlives the
  provider's web sessions), so the sentence now says only what was measured.

- **As built (increment 3).**
  - **Detached revocations are bounded.** At most 4 provider revocations are in
    flight; the rest wait inside their own goroutine under the same 15 second
    bound and are dropped with an Info line when it passes (the session is
    already signed out here).
  - **Provider revocation shares renewal's client.** `RevokeRefreshToken` uses
    `renewHTTPClient` (no redirects, owned transport) and the same
    client-authentication choice (`tokenEndpointRequest`, Basic unless discovery
    advertises only `client_secret_post`), bounded at 5 seconds. Logout does it
    synchronously after the row is revoked; the devices destroy does it in a
    detached goroutine, because the user is not waiting on the provider for a
    remote device. A row that could not be revoked is not revoked at the
    provider either.
  - **`SignedInDevice/get` takes `ids: null` only.** Any other `ids` are
    `notFound`. The state is a digest of the listed ids; there is no `/changes`,
    so the client re-fetches.
  - **The stream re-check** is an injected `SessionLive` function (the store,
    with the 12 hour allowance), run every 30 seconds for a request that has a
    session. An error keeps the stream and is logged once per stream.
  - **Push subscriptions end with the session, for every reason.**
    `RevokeSession` deletes the session's subscriptions in the same transaction,
    whatever the reason: sign-out, `refused`, `expired` and `replaced` as well as
    the devices destroy. A subscription left behind would keep receiving pushes
    whose title and body are derived from mail. **`replaced` depends on the web
    client:** a second sign-in revokes the first session as replaced and takes
    its subscription with it, and the client's reconcile-on-connect (increment 4)
    re-registers the browser under the new session. Until that ships, a replaced
    sign-in loses push on that browser.
  - **The client address** (`MAILROOM_TRUSTED_PROXIES`) is display data only:
    it feeds the session's `last_addr` and the callback's `Addr`, not the
    bearer rate limiter.

### 7. Housekeeping

- **What it deletes.** A daily job deletes rows revoked more than 7 days ago,
  and rows unseen for 45 days (past the 30-day provider limit, with margin).
- **How.** It runs through a narrow `SECURITY DEFINER` sweep with its owner
  policies (§2), because no user identity applies.
- **What it reports:** per-reason counts (revoked by reason, unseen), never one
  total.
- The devices list never shows a revoked row.
- **As built (increment 3).**
  - **A ticker, not a durable job.** The sweep runs from `cmd/mailroom` once at
    start and then every 24 hours, like the other instance-wide sweeps (the OAuth
    flow sweep), not through the job dispatcher: the dispatcher's recurring kind
    is per user, this job has no user, and a missed run costs nothing because the
    next one deletes the same rows. Several processes running it at once is
    harmless; the delete is idempotent.
  - **Every reason is reported.** `app.sweep_sessions(p_now)` returns one row
    per reason, `unseen` included, zero counts included, and the log line lists
    all six.
  - **The sweep deletes the subscriptions of the sessions it deletes,** before
    the rows, inside `app.sweep_sessions` (not `ON DELETE SET NULL`, which would
    leave an unbound subscription that keeps receiving). It has its own owner
    SELECT and DELETE policies on `app.push_subscriptions`, confined to rows
    whose session the sweep may delete. The counts are their own reasons:
    `push_unseen` (the session was live and unseen for 45 days) and
    `push_revoked` (a leftover of a session revoked before this rule), so no
    total mixes sessions and subscriptions.
  - **Two ages, two places.** The owner SELECT and DELETE policies bound what the
    sweep may delete at the wall clock (revoked more than 7 days ago, live and
    unseen for 45); the function's own `WHERE` applies the same ages as of
    `p_now`. `p_now` is therefore a clock for tests that can only make the
    function stricter than the policies, never looser.

### 8. Upgrade and operator setup

- **Everyone signs in once more** after deploying the release. The release notes
  say so in their upgrading section.
- **The provider must issue refresh tokens.** For Authentik:
  - the Mailroom provider's scope mappings include `offline_access`, without
    which current Authentik issues no refresh token;
  - its "Refresh token validity" is set to the idle limit (30 days);
  - `DefaultScopes` gains `offline_access`.

  The operator guide says this in neutral terms (portability rule).
- **The release itself is unaffected by the order.** Deploying before the
  provider is configured is safe: sessions behave as today (§4,
  no refresh token).
- **Dev-auth mode** (no OIDC) is unchanged.

### 9. Unverified provider behaviour, measured before release

These are checked against a real provider before
release. §4's serialisation is designed for the worst
case, so no answer weakens it. The user-facing claims, however, depend on them:

1. **Reuse.** Is a refresh token single-use, so that a second use is refused
   (the assumption behind §4's locking)? Does the provider then also revoke
   the newer token (token-family revocation)? If it does, a crash inside the
   5-second window signs the user out, which is still fail-closed.
2. **Rotation and sliding.** Does refresh rotate the token, and does it slide
   the validity? If it does not slide, an active user signs in again 30 days
   after their first sign-in, not 30 days after their last use. The notes then
   say so, or the validity is raised.
3. **Revocation reach.** Does a refresh fail after the user is deactivated,
   after a password change, and after their sessions are ended at the
   provider? §6's sentence promises only what this measures.
4. **Endpoints and claims.** Does the provider advertise
   `revocation_endpoint`, and does revocation make the next refresh fail? Does
   the refresh response carry `id_token`, and does it carry `groups`, or do
   they come only from UserInfo?


**Results (Authentik as the test provider):**

- **Issued and silent.** Refresh tokens are issued with `offline_access`.
  Renewal is silent, and the role is re-read on every renewal.
- **(1) Reuse.** The provider as configured does **not** rotate. A refresh
  response carried no new refresh token, and an already-used token was accepted
  again. The serialisation of §4 is therefore stricter than this provider
  needs, which costs nothing.
- **(2) Sliding.** Without rotation, the validity does **not** slide. An active
  device's token expires at issue time + validity, so the idle limit is in
  effect "30 days after sign-in". Sliding needs the provider to issue a fresh
  refresh token on renewal (a provider setting); the release notes say so.
- **(3) Reach.**
  - Ending the user's sessions at the provider does **not** reach Mailroom: the
    next renewal succeeded.
  - Revoking the refresh token there **does**: `invalid_grant`, the session is
    revoked `refused`, and the user is signed out on that click.
  - A password change and a deactivated user were not tested.
- **(4) Endpoints and claims.**
  - `revocation_endpoint` is advertised, and a remote sign-out removed the
    device's token at the provider.
  - Both `client_secret_basic` and `client_secret_post` are accepted.
  - Each renewal's identity re-check succeeded, and the admin role was kept.
    Whether the groups came from the refreshed ID token or from UserInfo was
    not distinguished.

### 10. Code shape and tests

- **Code shape.** `internal/auth` stays free of the database. It gains an
  injected `SessionStore` interface: lookup by hash, create, touch, renew under
  the lock (re-read, then store), revoke, list and revoke for the devices
  methods. `internal/store` implements it. The package's unit tests run against
  a fake; the integration tests run against PostgreSQL.
- **Tests,** each probed red by removing the mechanism it covers (CLAUDE.md):
  - Renewal on schedule, with a fake token endpoint and a clock, and none while
    fresh.
  - Singleflight.
  - **A fake endpoint that refuses token reuse,** with two concurrent renewals
    across two stores: the session stays alive and exactly one call is made.
  - Refusal revokes.
  - Server refusal and transient failures serve within 12 hours and refuse
    after; the 5-second timeout takes the transient path.
  - A no-refresh-token session lasts 12 hours.
  - **Role re-applied on renewal:** removed from the admin group means role
    `user` after the next renewal.
  - Resolver refusal revokes.
  - A mismatched `sub` revokes.
  - No ID token and a failed UserInfo is transient and not stamped.
  - A legacy cookie is refused.
  - A disabled user is refused on the next request.
  - **A store error is 503, not 401.**
  - Sign-in revokes a replaced session.
  - Logout's `id_token_hint` comes from the row after 12 hours.
  - Device sign-out deletes exactly that session's push subscriptions, and the
    stream closes within a minute.
  - `*others*` spares the current session.
  - API keys are refused both methods.
  - Push reconciliation re-registers a deleted subscription.
  - A log-capture test with sentinel tokens shows that no token reaches a log
    line or error, proven able to fail.
  - RLS negative tests and owner-policy tests for `app.sessions`. The lookup
    function cannot enumerate. The sweep reports per reason.

## Consequences

- Users sign in once per device and stay signed in for as long as the provider
  agrees and they use the device at least every 30 days.
- Revocation becomes real:
  - **provider-side:** within 15 minutes of the next use;
  - **Mailroom-side** (device sign-out, a disabled user): on the next
    request;
  - **role changes:** at the next renewal, which is sooner than the 12 hours
    they take today.
- **Cost.** Every request costs one indexed lookup, and every 15 minutes of
  activity costs one token-endpoint call (plus UserInfo, if needed) per device.
  A stalled provider costs at most 5 seconds per device per minute.
- The provider becomes a dependency of continued use as well as of sign-in. The
  12-hour outage allowance bounds what an outage can do.
- **What a breach yields.** A database dump plus the master key yields provider
  refresh tokens. Those can only mint Mailroom sign-in tokens for this client,
  not mailbox access: no worse than ADR-046's mailbox tokens, and strictly
  narrower.
- #34 (OAuth for AI tools) builds its consent and revocation on this session
  model, not on the stateless cookie.

## Alternatives considered

- **A longer cookie lifetime (`MAILROOM_SESSION_TTL`).** Rejected as the
  answer, though trivial to build. It widens exactly the window that cannot be
  revoked, freezes role changes for as long, and still bounces at the end.
- **Sliding the stateless cookie on use.** Rejected. Mailroom would never ask
  the provider again, so a user disabled there could stay signed in forever.
- **Silent re-authentication through the browser** (a hidden iframe or a
  redirect with `prompt=none`). Rejected. Third-party-cookie blocking breaks
  the iframe, a redirect cannot happen inside a JMAP call or an EventSource
  stream, and the installed PWA would visibly leave the app.
- **Renewing in the background and serving immediately (stale-while-revalidate).**
  Considered and kept as a fallback if blocking costs are ever measured. With
  the 5-second bound, blocking costs one slow request per device every 15
  minutes at worst. In exchange, a refused user is refused on that same
  request, not one request later.
- **Storing sessions in memory.** Rejected. A restart would sign everyone out,
  and the devices list could not survive one.

## References

Spec §10. ADR-013 (sealing), ADR-016 (push), ADR-042 (token hashing, the lookup
function's pattern, the scope table), ADR-046 (failure classes, redaction).
RFC 6749 §6 (refresh), RFC 7009 (revocation), OpenID Connect Core §12. #34.
