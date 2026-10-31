// SPDX-License-Identifier: AGPL-3.0-only

// Package auth implements Mailroom's user-facing authentication: OpenID
// Connect Authorization Code flow with PKCE against a generic OIDC provider
// (Authentik in the reference deployment, per spec §10 — this package makes
// no Authentik-specific assumptions), server-side browser sessions, and an
// authentication middleware.
//
// # Store- and server-agnostic by design
//
// This package touches neither net/http routing beyond the handlers it
// exposes, nor the database: CLAUDE.md reserves all DB access for
// internal/store. Mapping a verified OIDC identity to a mailroom user id
// (provisioning or lookup in app.users) is therefore the caller's
// responsibility, injected as a ResolveUser function passed to New. Callers
// (internal/server, cmd/mailroom) mount LoginHandler, CallbackHandler, and
// LogoutHandler on their own mux and wrap protected routes with Middleware.
//
// # Flow
//
//  0. Page (page.go) renders the sign-in landing page — the one surface an
//     unauthenticated visitor may reach. Its "Sign in" action is what hits
//     LoginHandler, and a login that FAILS lands back on it with a reason
//     (WithFailureRedirect). Keeping the page and the flow on separate routes
//     is deliberate: a subject the caller's ResolveUser refuses would otherwise
//     bounce straight back to a provider that still holds a valid SSO session,
//     get the same identity re-issued, be refused again, and loop — never once
//     being told why.
//  1. LoginHandler generates a random state and nonce, a PKCE verifier
//     (golang.org/x/oauth2's GenerateVerifier/S256ChallengeOption — RFC
//     7636, S256 only), stores them in a signed, HttpOnly "login state"
//     cookie, and redirects to the provider's authorization endpoint with
//     the S256 code_challenge.
//  2. The provider authenticates the user (2FA etc. is entirely the
//     provider's concern) and redirects back to CallbackHandler with a
//     state and an authorization code.
//  3. CallbackHandler checks the returned state against the login-state
//     cookie (CSRF defense for the login flow — see below), exchanges the
//     code for tokens using the stored PKCE verifier, verifies the ID
//     token's signature/issuer/audience/expiry via
//     github.com/coreos/go-oidc, checks the nonce, and extracts Claims.
//  4. Claims are handed to the caller-supplied ResolveUser to obtain a
//     mailroom user id, a session record is created through the injected
//     SessionStore, and a random-secret session cookie is set.
//
// # Login-state storage: signed cookie, not a server-side map
//
// The state/nonce/PKCE-verifier tuple lives in a signed, HttpOnly cookie
// rather than server-side storage (an in-memory map or a DB row). This
// keeps Authenticator free of any storage/cleanup dependency — consistent
// with "server-agnostic" above — at the cost of a slightly larger cookie
// for the few minutes a login is in flight. CSRF protection is unaffected:
// the state value is still sent to the provider and returned as a query
// parameter on the callback exactly as the OAuth2 spec requires, and
// CallbackHandler cross-checks it against the cookie copy an attacker
// cannot forge (it's HMAC-signed) or read (HttpOnly) from a different
// origin.
//
// # Sessions
//
// A session is a server-side record (ADR-048, Config.SessionStore), one per
// signed-in browser. The cookie ("s1." plus 256 random bits in base32, HttpOnly,
// SameSite=Lax) carries nothing else: no user id, no expiry. The store holds its
// SHA-256, so a database read yields no usable cookie, and every request does one
// indexed lookup by that hash before anyone is known. SameSite=Lax (not Strict)
// is used because a Strict cookie is withheld on the first top-level GET from an
// external link (email, bookmark), which would break normal navigation into the
// app; Lax still withholds the cookie on cross-site POST/fetch/XHR, which
// combined with the login-flow's state-parameter check is this package's CSRF
// defense. Secure is controlled by Config.CookieSecure and must be true whenever
// the app is served over HTTPS (i.e. always in production).
//
// A session is valid while it is not revoked, its user is neither disabled nor
// deleted, and its last provider confirmation is within Config.OutageAllowance.
// Until silent renewal lands (a later ADR-048 increment) the only confirmation
// is sign-in, so a session lasts that long, as the stateless cookie did. The
// browser Expires is SessionCookieLifetime (30 days); the server decides.
//
// A failure to check the session at all (a database error) is a 503, never a
// 401: the web client reads a 401 as "session expired" and bounces every open
// tab to sign-in. The cookie is not cleared on 503.
//
// Sign-in always mints a fresh secret (anti-fixation, #310) and revokes the
// session the request already held as "replaced". Sign-out revokes the row
// before redirecting. The provider's refresh token, when it issues one, is
// stored sealed by the SessionStore implementation for that later renewal.
//
// # What this package does NOT do
//
// Stated here rather than only in a comment on the handler that does not do it,
// because the place a reader looks for "is single sign-out handled?" is the
// package doc, and a gap nobody can find is a gap nobody can weigh (Forgejo
// #310).
//
// NO BACK-CHANNEL LOGOUT, AND NO PROVIDER-SIDE REVOCATION REACH YET. Signing out
// of Mailroom ends the local session and, when the provider publishes an
// end_session_endpoint and the session row holds an ID token, the provider
// session too (RP-initiated logout, #314; it falls back to a local redirect
// without an id_token_hint, because most providers ignore a hint-less request).
// The other direction, signing out at the provider or deactivating the account
// there, does not end a Mailroom session until renewal (ADR-048 section 4)
// re-asks the provider; until then Config.OutageAllowance bounds it. Disabling
// the user in Mailroom does end their sessions on the next request.
//
// NO "DEVICES" LIST OR "SIGN OUT EVERYWHERE" YET. The records now exist to
// support them (ADR-048 section 6); the methods are a later increment.
//
// # Testing scope
//
// Unit tests in this package (no database, no live provider) cover state/
// nonce/PKCE generation, cookie format and flags, session lookup against an
// in-memory SessionStore fake, and Middleware's allow/deny/503 behavior. CallbackHandler's
// full happy-path (ID token verification against a real signature) is
// exercised against a locally generated RSA key and oidc.StaticKeySet
// rather than a live provider — see callback_test.go. New's OIDC discovery
// (oidc.NewProvider) makes a real network call and is exercised only by
// integration/manual testing against a live Authentik instance
// (${OIDC_ISSUER}), which is out of scope for this package's unit tests.
package auth
