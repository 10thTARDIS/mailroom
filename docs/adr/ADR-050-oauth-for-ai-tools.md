# ADR-050: OAuth for AI tools — Mailroom as the authorization server for `/mcp`, and a consent that is an API key

**Status:** Proposed (v1.100; #34). Follows the MCP authorization specification,
revision 2026-07-28. Builds on ADR-042 (agent access) and ADR-044 (MCP tools) and
widens neither: everything an approved client may do is decided by the gate
ADR-042 already enforces.

## Context

Since v1.95 an AI client reaches `/mcp` with an API key the user creates in
Settings and pastes into the client (ADR-044 §2). Clients whose connectors expect
OAuth, which is what the MCP specification defines for HTTP transports, can only
connect through a local stdio bridge with the key in a header (`docs/mcp.md`).

The MCP authorization specification (2026-07-28) makes the MCP server an OAuth
2.1 resource server and requires:

- Protected Resource Metadata (RFC 9728) naming at least one authorization
  server, discoverable from the 401's `WWW-Authenticate` and at a well-known URI;
- an authorization server with RFC 8414 (or OIDC) discovery, OAuth 2.1, PKCE
  with `S256` advertised in `code_challenge_methods_supported`, exact redirect
  URI matching, HTTPS endpoints, redirect URIs that are HTTPS or loopback;
- access tokens validated as issued for this server (RFC 8707 audience), never
  accepted for or passed to anything else; short-lived access tokens; refresh
  tokens **rotated** for public clients;
- client registration by Client ID Metadata Documents (CIMD, **SHOULD**),
  pre-registration, or Dynamic Client Registration (RFC 7591, **MAY**,
  deprecated); `iss` in the authorization response (RFC 9207, **SHOULD**);
- for CIMD: SSRF protection on the fetch, the redirect URI hostname shown at
  consent, and a warning for localhost-only redirect URIs.

Two facts about Mailroom shape the design:

1. **Sign-in is already delegated** to an external OpenID provider (ADR-048), or
   to dev-auth. That provider knows who the user is. It does not know Mailroom's
   scopes, account restriction, limits or confirm mode, and cannot show them at
   consent or revoke a grant from Mailroom's Settings.
2. **Every rule an agent obeys is already one gate keyed by an `api_keys` row**
   (ADR-042 §2–7): scope table, account restriction, rate and budget, confirm-mode
   holds, audit, anomaly rules and suspension. ADR-044 binds MCP to be a thin
   client of it.

## Decision

### 1. Mailroom is its own authorization server for `/mcp`

Mailroom issues the tokens `/mcp` accepts. It does **not** use the sign-in
provider as the authorization server: that provider cannot present ADR-042's
choices at consent, cannot be revoked from Settings, and many providers support
neither CIMD, DCR nor resource indicators. The sign-in provider still answers
*who is approving*: the consent screen requires an ordinary browser session.

Mailroom is an OAuth authorization server only. It issues no ID tokens and is not
an OpenID provider.

**Issuer and resource** come from `MAILROOM_PUBLIC_URL` (which already defaults to
the origin of `MAILROOM_OIDC_REDIRECT_URL`): issuer `https://mail.example.com`,
resource `https://mail.example.com/mcp`. Under dev-auth, `MAILROOM_PUBLIC_URL` is
required for OAuth, as it already is for mail-provider OAuth. If it is unset, the
OAuth surface is off and `/mcp` behaves exactly as today (keys only).

### 2. Discovery

- `GET /.well-known/oauth-protected-resource/mcp` (and the root form
  `/.well-known/oauth-protected-resource`): `resource`,
  `authorization_servers: [issuer]`, `bearer_methods_supported: ["header"]`,
  `scopes_supported: ["mail:read", "search:read"]` (the minimal useful set; the
  rest come by step-up, §8), `resource_name: "Mailroom"`.
- `GET /.well-known/oauth-authorization-server`: `issuer`,
  `authorization_endpoint` (`/oauth/authorize`), `token_endpoint`
  (`/oauth/token`), `revocation_endpoint` (`/oauth/revoke`, RFC 7009),
  `response_types_supported: ["code"]`,
  `grant_types_supported: ["authorization_code", "refresh_token"]`,
  `code_challenge_methods_supported: ["S256"]`,
  `token_endpoint_auth_methods_supported: ["none"]`,
  `scopes_supported` (ADR-042's closed set),
  `client_id_metadata_document_supported: true`,
  `authorization_response_iss_parameter_supported: true`, and
  `registration_endpoint` only when DCR is enabled (§4).
- `/mcp`'s 401 becomes
  `WWW-Authenticate: Bearer realm="mailroom", resource_metadata="<…/oauth-protected-resource/mcp>", scope="mail:read search:read"`.
  API-key clients are unaffected: the challenge only gains parameters.

All of these are unauthenticated GETs serving the same document to everyone.

### 3. A grant is an API key; its tokens resolve to it

- **Consent creates (or updates) a row in `app.api_keys` with `kind = 'oauth'`.**
  Scopes, `accountIds`, `confirmScopes`, `ratePerMinute`, `dailyBudget` and
  `suspendOnAnomaly` are the same columns with the same CHECKs, chosen on the
  consent screen exactly as on the key form (§5). So the scope table, the account
  restriction, limits, holds, audit (`api_key_events`), anomalies and suspension
  apply to an OAuth client **with no new enforcement path**, and Settings lists,
  edits, suspends and revokes it like a key (§7).
- Expand migration (ADR-008): `kind text NOT NULL DEFAULT 'key'`
  (`'key' | 'oauth'`), `token_hash` becomes nullable with
  `CHECK ((kind = 'key') = (token_hash IS NOT NULL))`, and new columns
  `oauth_client_id`, `oauth_client_name`, `oauth_client_kind`
  (`'cimd' | 'registered' | 'manual'`), `oauth_redirect_host`.
  `UNIQUE (user_id, kind, oauth_client_id)` WHERE kind = 'oauth': **one grant per
  user and client**. A client that authorizes again updates that grant (the
  consent screen starts from its current settings) instead of piling up rows. The
  key name defaults to the client's name, de-duplicated with a suffix.
- **Tokens:** new table `app.oauth_tokens` (user_id-led, FORCE RLS): `id`,
  `user_id`, `api_key_id`, `kind` (`access | refresh`), `token_hash` (SHA-256,
  unique), `family_id` (one per authorization code), `resource`, `scopes` (a
  snapshot, informational: the gate reads the key's current scopes),
  `expires_at`, `used_at` (refresh), `revoked_at`, `created_at`. Tokens are opaque
  random 256-bit values with a scanner-recognisable prefix, `mra_` (access) and
  `mrr_` (refresh), distinct from keys' `mr_`. Hash only, shown once, never logged,
  as for keys (ADR-042 §1).
- **Resolution:** the bearer gate tells the token kind by prefix. An `mra_` token
  resolves through a new SECURITY DEFINER `app.oauth_access_for_token(hash)`,
  returning `(user_id, key_id)` only for an unexpired, unrevoked access token
  whose grant is `kind = 'oauth'`, neither revoked nor suspended, and whose user
  can sign in: the predicate `api_key_for_token` uses, plus the token's own. From
  there the request is a key request: `keyPolicyGate` loads the grant's policy
  and the dispatcher's key gate runs unchanged. Every refusal stays the bearer
  gate's uniform 401, with the same timing rule.
- **Audience:** an OAuth access token is accepted **only on `/mcp`**, the resource
  it was issued for (RFC 8707). On `/jmap`, `/jmap/session` and every other route
  it is the uniform 401. API keys keep every route they have today. Refresh tokens
  are accepted only at the token endpoint.
- **Lifetimes:** access tokens 1 hour. Refresh tokens rotate on every use
  (OAuth 2.1 §4.3.1). Each one expires 30 days after it was issued, so a client
  that keeps refreshing stays connected and one left unused for 30 days must
  re-authorize. There is no absolute cap: like a key, the grant lasts until it is
  revoked.
- **Reuse detection:** presenting a refresh token that was already used revokes
  its whole family (every access and refresh token from that authorization) and
  records an `api_key_events` anomaly `refresh_reuse` with a push alert. A
  stolen-and-raced refresh token ends both copies' sessions; the grant itself
  stays, so the legitimate client re-authorizes. It does not suspend the grant: a
  client bug that retries a refresh would otherwise lock the user out.

### 4. Clients

Public clients only (`token_endpoint_auth_method: none`, PKCE carries the proof).
Confidential clients and `private_key_jwt` are not supported in this release.

- **Client ID Metadata Documents (preferred).** A `client_id` that is an `https`
  URL with a path is fetched through `internal/egress` (the guarded client: no
  private, loopback, link-local or metadata addresses at dial time, redirects
  refused, body capped). The fetch has a 5 s timeout and a 16 KiB cap. The
  document must be JSON whose `client_id` equals the URL exactly, with a
  `client_name` and `redirect_uris`; `token_endpoint_auth_method`, if present, must be
  `none`. It is cached per HTTP cache headers, clamped to 5 minutes to 24 hours.
  A fetch failure is `invalid_client` on an error page (never a redirect). An
  operator whose clients publish their documents on a private network can opt in
  with `MAILROOM_OAUTH_ALLOW_PRIVATE_CLIENT_HOSTS`, modelled on
  `MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS` and off by default.
- **Dynamic Client Registration (compatibility).** `POST /oauth/register` accepts
  RFC 7591 metadata for public clients and returns a random `client_id`. It is
  unauthenticated by nature, so: a per-address rate limit, at most 50
  registrations a day per address, size limits on every field, and a
  registration not used by a completed consent within 24 hours is deleted. The
  client name a registration declares is **self-asserted** and the consent screen
  says so (§5). Enabled by default, `MAILROOM_OAUTH_DCR=off` turns it off, and
  the metadata omits `registration_endpoint` when it is off. (Open question Q64.)
- **Manual registration.** A user can create a client in Settings (a name and its
  redirect URIs) and paste the `client_id` into a client that supports
  pre-registration. It is the fallback for a client that supports neither of the
  above.

**Redirect URIs** are matched exactly against the client's registered or
documented list, except that a loopback URI (`http://127.0.0.1`, `http://[::1]`,
`http://localhost`) matches on any port (RFC 8252 §7.3, OAuth 2.1). Anything that
is not `https` or loopback is refused, including private-use schemes, as the MCP
specification requires.

### 5. The authorization endpoint and the consent screen

`GET /oauth/authorize` checks, before anything else: `response_type=code`, a
known or resolvable client, a redirect URI that matches, `code_challenge` with
`code_challenge_method=S256` (plain is refused), and `resource` equal to the
canonical `/mcp` URI. Scheme and host case are folded, a trailing slash is
ignored, and a missing `resource` is taken to mean `/mcp`, the only resource. A
different resource is `invalid_target`. Until the client and redirect URI are
valid, errors are shown on Mailroom's own page and never redirected (OAuth 2.1
§4.1.2.1). After that, errors redirect with `error`, `state` and `iss`.

The validated request is stored server-side (`app.oauth_requests`: a random id,
the user, client, redirect URI, PKCE challenge, resource, requested scopes, state,
expiry 10 minutes). The consent page refers to it by id only. So the form cannot
change the client, redirect URI or challenge, and what the user approves is what
was checked. A signed-out user is sent through the normal sign-in and back to the
same request id (`auth.WithUnauthenticatedRedirect`, which already drops a
`code`/`state` query it redirects, ADR-046 §10).

The consent screen:

- names the client with what is known about it. For CIMD, the name plus "from
  **app.example.com**" (the `client_id` host, which is verified by the fetch). For
  DCR or manual clients, the name marked "name given by the app, not verified".
- **shows the redirect host** (required by the spec). When every redirect URI is
  loopback it warns: "This app receives the approval on this computer. Approve
  only if you started this from an app you trust."
- is the API key form. Scopes start from the requested scopes intersected with
  ADR-042's closed set (unknown scopes dropped, listed as "not available"), and
  the user can narrow them. It also covers the account restriction, confirm mode
  (destructive scopes start as "ask me first", ADR-042 §1), rate and daily budget,
  and the typed confirmation when send and delete are granted together. On
  re-authorization it starts from the grant's current settings, with newly
  requested scopes added and highlighted.
- **Approve** is a same-origin POST guarded by the existing `CSRFGuard`, from a
  page served with `frame-ancestors 'none'`. The request id is single-use and
  bound to the signed-in user who opened it, so a forged or replayed approval
  fails. **Deny** redirects with `access_denied`.

Approval writes the grant (§3) and its audit (`api_key.create`/`update` with
`kind: oauth` and the client), then issues an **authorization code**: random,
stored as a hash in `app.oauth_codes`, bound to the client, redirect URI, PKCE
challenge, resource, grant and user, valid 60 seconds, single use. It redirects
with `code`, `state` and `iss`.

### 6. The token endpoint

`POST /oauth/token`, form-encoded, no client authentication (public clients),
CORS-free.

- `authorization_code`: code, `redirect_uri` and `client_id` must match the
  code's, `code_verifier` must hash to the challenge, and `resource` (if sent)
  must be `/mcp`. It issues an access token and a refresh token in a new family.
  Presenting a code a second time is refused **and revokes the family it already
  produced** (OAuth 2.1 §4.1.3).
- `refresh_token`: rotation and reuse detection as in §3. The scope may be
  narrowed, never widened. Widening is a new consent (§8).
- Errors per RFC 6749 §5.2 (`invalid_grant`, `invalid_client`,
  `invalid_request`, `unsupported_grant_type`, `invalid_target`). Responses carry
  `Cache-Control: no-store`.
- Failed attempts share the bearer gate's per-address failure limiter, charged
  only on failure, as ADR-042 §2 amended.

`POST /oauth/revoke` (RFC 7009) revokes a refresh token's family or one access
token and always answers 200.

### 7. Settings, revocation and audit

- Settings → API keys lists OAuth grants beside keys as **Connected apps**: the
  client and how it is identified, the redirect host, scopes, limits,
  created and last used, and the token families (one per device that connected)
  with when each was last used.
- Every key action applies: edit settings, suspend and reactivate, **revoke**.
  Revoking the grant (or a single family) takes effect on the next request,
  because every access token is checked against the grant's state on every
  request. There is no self-contained token to outlive a revocation.
- Audit: grant creation and updates, revocation, family revocation, code replay
  and refresh reuse go to `app.audit_log` and `api_key_events`. Token issuance
  and refresh are not audited per event (they are as frequent as a session
  renewal). `last_used_at` on the grant and the family records use.
- Grants are not JMAP-synced objects (as for keys, ADR-042 §1): no change rows.

### 8. Scope challenges and step-up

A tool call outside the grant's scopes is already a tool error from the gate
(ADR-044 §3), and `tools/list` hides tools the grant cannot use. For OAuth tokens
only, the bridge also answers a `tools/call` refused **only for scope** with HTTP
403 and
`WWW-Authenticate: Bearer error="insufficient_scope", scope="<the scopes that tool needs>", resource_metadata="…"`.
The scopes come from `KeyMayCall`'s view of the tool's declared uses, all of them
in one challenge, as the spec asks. A client that supports step-up sends the user
back to consent, which shows the grant with the new scopes added. A client that
does not support it sees the tool error it sees today. API keys keep today's
behaviour exactly: a key cannot be re-consented.

### 9. Threat model and negative controls

Each line is a test that must be shown to fail without its control (CLAUDE.md:
probe your tests, negative-control every safety assertion):

- **Audience:** an OAuth access token on `/jmap` and every non-`/mcp` route is 401.
  An API key on `/mcp` still works. A refresh token as a bearer is 401.
- **Revocation is immediate:** revoking the grant, or suspending it, makes the
  next `/mcp` request with a live access token 401. The same for one family.
- **Cross-user:** user B's session cannot consent to, see, revoke or refresh A's
  grant, request, code or tokens (RLS, FORCE). A's code redeemed in a request
  signed by B's PKCE verifier fails.
- **PKCE:** a missing verifier, a wrong verifier, `plain` and a missing challenge
  are each refused.
- **Codes:** an expired code, a second redemption (which also revokes the family),
  a code redeemed with another client_id, redirect URI or resource, are each refused.
- **Redirects:** an unregistered redirect URI, a private-use scheme, `http` to a
  non-loopback host, and a near miss (case, trailing path, extra query) are each
  refused on Mailroom's page without redirecting.
- **Consent forgery:** a cross-site POST to approve is refused (CSRF), the page
  cannot be framed, a request id opened by A cannot be approved by B, and an
  approved or expired request id cannot be approved again.
- **Refresh reuse:** a reused refresh token revokes its family and raises the
  anomaly.
- **CIMD fetch:** a `client_id` resolving to a private, loopback, link-local or
  metadata address is refused at dial time (including through DNS that changes
  between check and dial). So are a redirect, an oversized body, a slow response,
  a mismatched `client_id` and a missing `redirect_uris`.
- **Scope ceiling:** whatever scopes a client requests, the grant holds only what
  the user approved, and the gate refuses beyond it. A refresh cannot widen it.
- **No new enforcement path:** the existing ADR-042 and ADR-044 suites, run with an
  OAuth token in place of a key, pass unchanged (the same table drives both).

Residual risks, accepted and stated: a DCR client's name is whatever it says (the
screen says so). A loopback redirect can be received by any program on that
computer that wins the port (the warning, and PKCE, which makes an intercepted
code useless without the verifier). A user who approves a malicious client
grants what they approved, which is bounded by ADR-042's limits, holds and soft
deletes like any key.

### 10. Increments

1. **Server core:** discovery documents, the migration, `app.oauth_*`
   tables and definers, CIMD and manual clients, authorize (with a minimal consent
   page), token, revoke, bearer resolution and audience. All §9 controls except
   step-up. Shipped as a beta; `/mcp` with keys unchanged.
2. **Consent and Settings UI:** the full consent screen (§5) and Connected apps
   (§7), with accessibility checks.
3. **DCR and step-up** (§4, §8), then `docs/mcp.md` and a live test with at least
   one real client that uses CIMD and one that uses DCR. The operator does the
   live test.

## Consequences

- An AI client that speaks MCP OAuth connects by URL alone: the user approves it
  once in Mailroom and can revoke it there.
- Nothing about API keys changes. Keys remain the path for clients without OAuth
  and for anything outside `/mcp`.
- An OAuth client is exactly as capable as a key with the same settings, and no
  more. Opening anything new to agents is still ADR-042's one-table change.
- Mailroom gains an authorization server: about five new endpoints and five
  tables. The grant's tokens, requests and codes are user_id-led with FORCE RLS
  and isolation policies. The registered-client and CIMD-cache tables hold no
  user data; they carry FORCE RLS with no policy for `mailroom_app` (deny all)
  and are reached only through narrow SECURITY DEFINER functions, so the
  fail-closed rule holds for them too. The RLS inventory and negative suites
  cover all five.
- `oauth_tokens` grows with use. Expired tokens are deleted by the existing job
  sweep after a grace period.

## Alternatives considered

- **The sign-in provider as the authorization server** (Mailroom validates its
  JWTs). Rejected: the provider cannot present scopes, accounts, limits or confirm
  mode, revocation would live outside Mailroom, and support for CIMD, DCR and
  RFC 8707 varies by provider.
- **Self-contained (JWT) access tokens.** Rejected: revocation would wait for
  expiry, and the gate needs the grant's row on every request anyway.
- **A separate grants table instead of `api_keys` rows.** Rejected: it would need
  a second policy loader, a second Settings list and a second set of tests for
  the same rules, which is the second enforcement path ADR-042 forbids.
- **DCR only, or CIMD only.** CIMD is the specification's preferred mechanism and
  needs no unauthenticated writes. DCR is deprecated but still the only mechanism
  many deployed clients use. Supporting both, with DCR bounded and switchable,
  serves the clients people actually have.

## References

MCP specification 2026-07-28, Authorization (and its Client Registration,
Authorization Server Discovery and Security Considerations pages) · OAuth 2.1
(draft-ietf-oauth-v2-1-13) · RFC 6749, 6750, 7009, 7591, 8252, 8414, 8707, 9207,
9728 · draft-ietf-oauth-client-id-metadata-document-00 · ADR-008, ADR-042, ADR-044,
ADR-046, ADR-048 · `internal/auth/bearer.go`, `internal/api/mcp_bridge.go`,
`internal/egress`
