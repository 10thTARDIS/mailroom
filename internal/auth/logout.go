// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/url"
	"time"

	"mailroom/internal/xoauth2"
)

// legacyLogoutCookieName is the retired path-scoped cookie that earlier releases
// used to carry the raw ID token to LogoutHandler (#314). Since ADR-048 the
// token lives in the session row and the cookie is no longer set or read; it is
// only CLEARED on logout, for one release, so a browser that still holds one
// sheds it. Its Path must match what was set (Config.LogoutPath) or the browser
// keeps the original.
const legacyLogoutCookieName = "mailroom_auth_logout"

// clearedLegacyLogoutCookie expires the retired logout cookie in the browser.
func (a *Authenticator) clearedLegacyLogoutCookie() *http.Cookie {
	return &http.Cookie{
		Name:     legacyLogoutCookieName,
		Value:    "",
		Path:     a.cfg.LogoutPath,
		Domain:   a.cfg.CookieDomain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// LogoutHandler ends the session. It always clears Mailroom's own cookies
// first, then — when it can do so meaningfully — hands the browser to the
// provider's RP-initiated logout endpoint so the SSO session ends there too
// (#314). Otherwise it redirects locally, to the post-logout destination:
// "/" by default, or whatever WithPostLogoutRedirect set (point it at the
// sign-in page: "/" is behind the gate and would only bounce there anyway,
// and landing on a page that says "you are signed out" beats landing on one
// that says nothing). A same-origin "return_to" query parameter, sanitized
// exactly as LoginHandler's is, overrides the local destination.
//
// ⚠ "MEANINGFULLY" MEANS WITH AN id_token_hint. A logout redirect that
// reaches end_session_endpoint without one is, at most providers, either
// ignored outright or turned into an "are you sure?" interstitial — so it
// would look like RP-initiated logout while ending nothing. Rather than send
// the user on a provider round trip that may accomplish nothing, this falls
// back to the local redirect when the hint is unavailable, which is at least
// honest about what it did: the local session is gone either way.
//
// The session is REVOKED SERVER-SIDE before the redirect (ADR-048 section 5): the
// row is ended with reason signed_out, so the cookie is dead even if a copy
// survives. The id_token_hint comes from that row. A store failure never blocks
// the sign-out: the browser's cookie is still cleared and the redirect is local.
//
// After the row is revoked the session's refresh token is revoked at the provider
// (RFC 7009, RevokeRefreshToken) when discovery advertises a revocation_endpoint,
// synchronously and bounded by revokeTimeout. A failure there is logged and never
// changes the outcome: the sign-out here has already happened.
//
// ⚠ WHAT THIS DOES NOT DO: signing out AT THE PROVIDER ends a session HERE only
// at its next renewal (silent renewal, renew.go, within RenewInterval of use);
// until then the session ends when it is revoked or passes Config.OutageAllowance.
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	// Clear ours first and unconditionally: whatever happens with the
	// provider or the store, this browser is signed out of Mailroom. Doing it
	// before the branch below means no path can return early leaving the
	// cookie in place.
	http.SetCookie(w, a.clearedSessionCookie())
	http.SetCookie(w, a.clearedLegacyLogoutCookie())

	hint, refresh := a.revokeForLogout(r)
	// AFTER the row is revoked (the cookie is dead whatever happens here), and
	// synchronously: the call is bounded by revokeTimeout, and a failure is
	// logged and never blocks the sign-out (RFC 7009, ADR-048 section 5).
	if !refresh.Empty() {
		if err := a.RevokeRefreshToken(r.Context(), refresh); err != nil {
			a.logger().WarnContext(r.Context(), "revoking the refresh token at the provider failed; the session is signed out here regardless", "err", err)
		}
	}

	if to, ok := a.endSessionURL(hint); ok {
		http.Redirect(w, r, to, http.StatusFound)
		return
	}

	redirectTo := a.postLogoutRedirect
	if redirectTo == "" {
		redirectTo = "/"
	}
	if v := sanitizeReturnTo(r.URL.Query().Get("return_to")); v != "" {
		redirectTo = v
	}
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// revokeForLogout ends the request's session (if it has a live one) and returns
// the id_token_hint stored on it, "" when there is none, and its refresh token
// (empty when none), which the caller revokes at the provider. Both are read
// BEFORE the revoke. Store errors are logged and swallowed: a sign-out must
// complete.
func (a *Authenticator) revokeForLogout(r *http.Request) (hint string, refresh xoauth2.Secret) {
	ref, outcome := a.lookupSession(r)
	if outcome != sessionOK {
		return "", ""
	}
	ctx := r.Context()
	hint, err := a.cfg.SessionStore.IDToken(ctx, ref)
	if err != nil {
		a.logger().ErrorContext(ctx, "reading the session's ID token failed", "err", err)
		hint = ""
	}
	refresh, err = a.cfg.SessionStore.RefreshToken(ctx, ref)
	if err != nil {
		a.logger().ErrorContext(ctx, "reading the session's refresh token failed", "err", err)
		refresh = ""
	}
	if err := a.cfg.SessionStore.Revoke(ctx, ref, revokeSignedOut); err != nil {
		a.logger().ErrorContext(ctx, "revoking the session on sign-out failed", "err", err)
		// Not revoked here, so do not revoke the provider's grant either: the
		// session would stay usable here with a dead token.
		refresh = ""
	}
	return hint, refresh
}

// endSessionURL builds the provider's RP-initiated logout URL for this
// request, per OpenID Connect RP-Initiated Logout 1.0 §2. ok is false when
// either half is missing — the provider published no end_session_endpoint,
// or the session holds no usable id_token_hint — in which case the caller
// must fall back to a local redirect rather than send the user to the
// provider empty-handed.
func (a *Authenticator) endSessionURL(hint string) (string, bool) {
	if a.endSessionEndpoint == "" {
		return "", false
	}
	if hint == "" {
		return "", false
	}
	u, err := url.Parse(a.endSessionEndpoint)
	if err != nil {
		// Discovery handed us something unparseable. Fail to the local
		// redirect rather than emitting a broken Location header.
		return "", false
	}
	q := u.Query()
	q.Set("id_token_hint", hint)
	// client_id is RECOMMENDED alongside the hint and required by some
	// providers when post_logout_redirect_uri is present.
	q.Set("client_id", a.cfg.ClientID)
	if a.cfg.PostLogoutRedirectURI != "" {
		q.Set("post_logout_redirect_uri", a.cfg.PostLogoutRedirectURI)
	}
	u.RawQuery = q.Encode()
	return u.String(), true
}
