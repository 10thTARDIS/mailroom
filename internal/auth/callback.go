// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

// idTokenClaims is the subset of standard OIDC claims this package reads
// off a verified ID token (and, as a backfill, the UserInfo endpoint) before
// mapping them into Claims for ResolveUser.
type idTokenClaims struct {
	Email             string   `json:"email"`
	EmailVerified     bool     `json:"email_verified"`
	PreferredUsername string   `json:"preferred_username"`
	Name              string   `json:"name"`
	Groups            []string `json:"groups"`
}

// CallbackHandler completes the Authorization Code + PKCE flow started by
// LoginHandler: it validates the returned state against the login-state
// cookie (CSRF defense), exchanges the authorization code for tokens using
// the stored PKCE verifier, verifies the ID token's signature/issuer/
// audience/expiry (github.com/coreos/go-oidc) and nonce, extracts Claims,
// resolves them to a mailroom user id via the configured ResolveUser, and
// establishes a session: a server-side record plus a random-secret cookie.
//
// On any failure it responds with a 4xx/5xx status and does not set a
// session cookie. It always clears the login-state cookie first (single
// use), so a failed or abandoned login can't be replayed.
// When WithFailureRedirect is set, every failure below instead becomes a 302 to
// the sign-in page carrying a closed-set error code — see failLogin. Either way
// NO SESSION COOKIE IS SET on any path that does not reach resolveUser
// successfully.
func (a *Authenticator) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	fs, err := a.readAndClearFlowState(w, r)
	if err != nil {
		a.failLogin(w, r, ErrorExpired, http.StatusBadRequest, "auth: "+err.Error())
		return
	}

	if providerErr := r.URL.Query().Get("error"); providerErr != "" {
		// The provider's error string is NOT reflected onto the sign-in page:
		// it is attacker-controllable text arriving in a query parameter, and
		// the page renders a fixed sentence per code instead (page.go).
		a.failLogin(w, r, ErrorFailed, http.StatusBadRequest,
			fmt.Sprintf("auth: provider returned error: %s", providerErr))
		return
	}

	gotState := r.URL.Query().Get("state")
	if gotState == "" || subtle.ConstantTimeCompare([]byte(gotState), []byte(fs.State)) != 1 {
		a.failLogin(w, r, ErrorFailed, http.StatusBadRequest, "auth: state mismatch")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		a.failLogin(w, r, ErrorFailed, http.StatusBadRequest, "auth: missing code")
		return
	}

	ctx := r.Context()
	token, err := a.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		a.failLogin(w, r, ErrorFailed, http.StatusBadGateway, "auth: code exchange failed")
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		a.failLogin(w, r, ErrorFailed, http.StatusBadGateway, "auth: token response missing id_token")
		return
	}

	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		a.failLogin(w, r, ErrorFailed, http.StatusUnauthorized, "auth: id token verification failed")
		return
	}

	// Verify does NOT check the nonce (its docs say so explicitly) — that
	// is this package's responsibility, and is what ties the returned ID
	// token back to the specific login attempt recorded in fs, defending
	// against ID token replay/injection.
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(fs.Nonce)) != 1 {
		a.failLogin(w, r, ErrorFailed, http.StatusUnauthorized, "auth: nonce mismatch")
		return
	}

	var extra idTokenClaims
	if err := idToken.Claims(&extra); err != nil {
		a.failLogin(w, r, ErrorFailed, http.StatusUnauthorized, "auth: malformed id token claims")
		return
	}

	// Some providers (Authentik among them, depending on how its scope mappings
	// are configured) expose group membership and profile fields on the UserInfo
	// endpoint rather than in the ID token. Backfill from there when the ID token
	// left something we need empty. A UserInfo failure is NOT fatal: it is a
	// best-effort enrichment of an already-verified identity, so the login still
	// proceeds with whatever the ID token carried (ResolveUser then fails closed
	// on its own if it lacks, e.g., an email to provision from).
	if _, err := a.backfillFromUserInfo(ctx, token, idToken.Subject, &extra); errors.Is(err, errUserInfoSubject) {
		// OIDC Core 5.3.2: UserInfo for a different subject than the ID token is
		// not an enrichment to ignore; it fails the login.
		a.failLogin(w, r, ErrorFailed, http.StatusUnauthorized, "auth: UserInfo subject mismatch")
		return
	}

	claims := Claims{
		Subject:           idToken.Subject,
		Email:             extra.Email,
		EmailVerified:     extra.EmailVerified,
		PreferredUsername: extra.PreferredUsername,
		Name:              extra.Name,
		Groups:            extra.Groups,
	}

	userID, err := a.resolveUser(ctx, claims)
	if err != nil {
		if errors.Is(err, ErrCannotProvision) {
			// The identity was VERIFIED, but the resolver could not set up an
			// account for it because of the SERVER's OWN records (Forgejo
			// #574) — not anything the user did. Retrying or picking a different
			// account cannot fix this, so it must not read like ErrorDenied,
			// which implies exactly that it can.
			a.failLogin(w, r, ErrorUnprovisionable, http.StatusConflict, "auth: cannot provision account for this identity")
			return
		}
		// The identity was VERIFIED and then REFUSED by the resolver
		// (cmd/mailroom's provisioningResolver) — e.g. a token with no email to
		// provision from, or a subject an admin has disabled. This is the one
		// failure a user can do something about, so it gets its own code and its
		// own sentence on the sign-in page instead of a generic "failed".
		a.failLogin(w, r, ErrorDenied, http.StatusForbidden, "auth: could not resolve user")
		return
	}

	// A FRESH SESSION, UNCONDITIONALLY: this is the anti-session-fixation step
	// (Forgejo #310). A new secret is minted and a new row created for the id the
	// ID token just resolved to; the request's own session cookie is never
	// adopted or carried forward, so a value planted in the victim's browser
	// before the login is REPLACED by one the planter does not know. Do not make
	// this conditional on "the user already has a session": that is precisely the
	// change that turns a planted cookie into an authenticated one.
	// TestCallbackHandler_ReplacesAPreExistingSession fails on both shapes of that
	// mistake.
	//
	// If the request ALSO carried a valid session, that row is revoked
	// ("replaced") once the new one exists: a second tab's login, or a cookie
	// planted before this login, then stops working instead of lingering as a
	// ghost (ADR-048 section 3). The lookup is read here, before Create.
	oldRef, oldOutcome := a.lookupSession(r)

	secret, err := mintSessionSecret()
	if err != nil {
		a.failLogin(w, r, ErrorFailed, http.StatusInternalServerError, "auth: internal error")
		return
	}
	if token.RefreshToken == "" {
		// Said once per sign-in, never with the token. Without a refresh token
		// the session cannot renew (ADR-048 section 4): it lasts the outage allowance.
		a.logger().InfoContext(ctx, fmt.Sprintf(
			"provider issued no refresh token; sessions will last %s", a.cfg.OutageAllowance))
	}
	if _, err := a.cfg.SessionStore.Create(ctx, NewSessionInput{
		UserID:       userID,
		TokenHash:    hashSessionCookie(secret),
		RefreshToken: token.RefreshToken,
		IDToken:      rawIDToken,
		UserAgent:    r.UserAgent(),
		Addr:         a.displayAddr(r),
	}); err != nil {
		// No session row, no cookie: a login that cannot be recorded fails.
		a.logger().ErrorContext(ctx, "creating the session failed", "err", err)
		a.failLogin(w, r, ErrorFailed, http.StatusInternalServerError, "auth: internal error")
		return
	}
	if oldOutcome == sessionOK {
		if err := a.cfg.SessionStore.Revoke(ctx, oldRef, revokeReplaced); err != nil {
			a.logger().ErrorContext(ctx, "revoking the replaced session failed", "err", err)
		}
	}
	http.SetCookie(w, a.newSessionCookie(secret))

	redirectTo := "/"
	if fs.ReturnTo != "" {
		redirectTo = fs.ReturnTo
	}
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// backfillFromUserInfo fills empty fields of extra from the provider's UserInfo
// endpoint. It only ever ADDS what the ID token omitted (email, name, groups)
// and never overwrites a value the ID token already asserted, so a provider that
// puts everything in the ID token pays nothing and behaves identically. It is a
// no-op when the provider is not wired (the package's unit tests) or the token
// source is unusable. It reports whether a call was attempted and how it
// ended, for the caller to judge: login treats every failure but a subject
// mismatch as best-effort (it enriches an already-verified identity and must not
// turn a good login into a failure), while renewal treats a call it needed and
// could not make as a failed renewal.
func (a *Authenticator) backfillFromUserInfo(ctx context.Context, token *oauth2.Token, subject string, extra *idTokenClaims) (attempted bool, err error) {
	if token == nil || !a.canFetchUserInfo() {
		return false, nil
	}
	// Skip the round trip entirely when the ID token already gave us everything.
	if extra.Email != "" && extra.Name != "" && len(extra.Groups) > 0 {
		return false, nil
	}
	return true, a.mergeFetchedUserInfo(ctx, token, subject, extra)
}

// errUserInfoSubject is a UserInfo answer whose subject is not the one the ID
// token (or the session) established. OIDC Core 5.3.2 requires them to match; a
// mismatch is a refusal, never a transient failure.
var errUserInfoSubject = errors.New("auth: UserInfo subject does not match")

// mergeFetchedUserInfo calls UserInfo, checks its subject against subject, and
// fills the empty fields of extra from it.
func (a *Authenticator) mergeFetchedUserInfo(ctx context.Context, token *oauth2.Token, subject string, extra *idTokenClaims) error {
	ui, err := a.fetchUserInfo(ctx, token)
	if err != nil {
		return err
	}
	if ui.Subject != subject {
		return errUserInfoSubject
	}
	mergeUserInfo(extra, ui.Claims)
	return nil
}

// userInfoResult is a UserInfo answer: the subject it asserts and its claims.
type userInfoResult struct {
	Subject string
	Claims  idTokenClaims
}

// canFetchUserInfo reports whether a UserInfo endpoint exists: the discovered
// provider advertises one, or a test seam is installed. It is false for a
// provider with no UserInfo endpoint (identity can then only come from the ID
// token) and in the package's own unit tests unless they install userInfoFn.
func (a *Authenticator) canFetchUserInfo() bool {
	return a.userInfoFn != nil || (a.provider != nil && a.provider.UserInfoEndpoint() != "")
}

// fetchUserInfo calls the provider's UserInfo endpoint with token. Shared by the
// login backfill and by renewal (ADR-048 section 4a), which also needs the
// subject UserInfo asserts.
func (a *Authenticator) fetchUserInfo(ctx context.Context, token *oauth2.Token) (userInfoResult, error) {
	if a.userInfoFn != nil {
		return a.userInfoFn(ctx, token)
	}
	if a.provider == nil {
		return userInfoResult{}, errors.New("auth: no UserInfo endpoint")
	}
	info, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return userInfoResult{}, err
	}
	var ui idTokenClaims
	if err := info.Claims(&ui); err != nil {
		return userInfoResult{}, err
	}
	return userInfoResult{Subject: info.Subject, Claims: ui}, nil
}

// mergeUserInfo fills the empty fields of extra from ui, never overwriting a
// value the ID token already asserted.
func mergeUserInfo(extra *idTokenClaims, ui idTokenClaims) {
	if extra.Email == "" {
		extra.Email = ui.Email
		// email_verified is only meaningful alongside the email it qualifies.
		extra.EmailVerified = ui.EmailVerified
	}
	if extra.Name == "" {
		extra.Name = ui.Name
	}
	if extra.PreferredUsername == "" {
		extra.PreferredUsername = ui.PreferredUsername
	}
	if len(extra.Groups) == 0 {
		extra.Groups = ui.Groups
	}
}

// failLogin ends a failed login. With WithFailureRedirect set it sends the
// browser to the sign-in page with ?error=<code> (from the closed set in
// page.go); otherwise it writes msg with status, as it always did.
//
// It never sets a session cookie, and it never puts request- or provider-
// supplied text into the redirect: code is one of a fixed set of constants.
func (a *Authenticator) failLogin(w http.ResponseWriter, r *http.Request, code string, status int, msg string) {
	if a.failureRedirect == "" {
		http.Error(w, msg, status)
		return
	}
	dest := a.failureRedirect + "?" + url.Values{pageErrorParam: []string{code}}.Encode()
	http.Redirect(w, r, dest, http.StatusFound)
}

// readAndClearFlowState reads and verifies the login-state cookie, clears
// it in the response (single use), and checks it hasn't expired.
func (a *Authenticator) readAndClearFlowState(w http.ResponseWriter, r *http.Request) (flowState, error) {
	cookie, err := r.Cookie(a.cfg.StateCookieName)
	if err != nil {
		return flowState{}, errors.New("missing login state (cookie expired, blocked, or login not started here)")
	}
	http.SetCookie(w, a.clearedStateCookie())

	var fs flowState
	if err := decodeSigned(a.cfg.SigningKey, statePurpose, cookie.Value, &fs); err != nil {
		return flowState{}, errors.New("invalid login state")
	}
	if time.Now().After(fs.Expiry) {
		return flowState{}, errors.New("login expired, please try again")
	}
	return fs, nil
}

// clearedStateCookie builds a cookie that immediately expires the
// login-state cookie in the browser.
func (a *Authenticator) clearedStateCookie() *http.Cookie {
	c := a.stateCookie("", time.Unix(0, 0))
	c.MaxAge = -1
	return c
}
