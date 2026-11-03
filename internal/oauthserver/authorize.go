// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"net/http"
	"net/url"
	"strings"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// Authorize is GET /oauth/authorize (ADR-050 section 5). It is a BROWSER route:
// the session gate in front of it sends a signed-out user through sign-in and
// back to this same URL (auth.WithUnauthenticatedRedirect keeps this query), so
// by the time this runs there is a signed-in user, and the request row is
// created for that user only, after the client and redirect URI were validated.
//
// Until the client and redirect URI are valid, errors are shown on Mailroom's
// own page and never redirected (OAuth 2.1 4.1.2.1). After that, errors
// redirect with error, state and iss.
func (s *Server) Authorize() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			// The session gate guarantees a user; fail closed if it did not.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The limiter is keyed by the signed-in user and charged ONLY on failure
		// (and on a request that causes an outbound metadata fetch), never on a
		// successful validation: a limiter charged on success would let one
		// caller's burst lock others out behind a shared proxy address (ADR-042
		// section 2, amended). The connection address is not used here: the one
		// trusted-proxy-aware address helper (auth's forwardedClientAddr) is
		// display-only by its own contract and unexported.
		key := "u:" + userID.String()
		refuse := func(status int, msg string) {
			if blocked, _ := s.authLimiter.Blocked(key); blocked {
				errorPage(w, http.StatusTooManyRequests, "Too many failed attempts. Wait a moment and start again from the app.")
				return
			}
			s.authLimiter.Fail(key)
			errorPage(w, status, msg)
		}
		allowFetch := func() bool {
			if blocked, _ := s.authLimiter.Blocked(key); blocked {
				return false
			}
			s.authLimiter.Fail(key)
			return true
		}

		q := r.URL.Query()
		for _, k := range []string{"client_id", "redirect_uri", "response_type", "code_challenge",
			"code_challenge_method", "resource", "scope", "state"} {
			if len(q[k]) > 1 {
				refuse(http.StatusBadRequest, "The request repeats a parameter ("+k+"), so it cannot be trusted.")
				return
			}
		}
		client, err := s.resolveClient(r.Context(), q.Get("client_id"), allowFetch)
		if err != nil {
			var ec errClient
			if asClientErr(err, &ec) {
				refuse(http.StatusBadRequest, ec.msg)
				return
			}
			s.log.ErrorContext(r.Context(), "oauth: resolve client", "err", err)
			errorPage(w, http.StatusServiceUnavailable, "Something went wrong on our side. Try again in a moment.")
			return
		}
		if client.Kind == store.OAuthClientManual && (client.OwnerID == nil || *client.OwnerID != userID) {
			refuse(http.StatusBadRequest, "This application is registered to a different account.")
			return
		}
		redirectURI := q.Get("redirect_uri")
		if redirectURI == "" || !MatchRedirectURI(client.RedirectURIs, redirectURI) {
			refuse(http.StatusBadRequest, "The address this application asked to be sent back to is not one it registered, so the request was refused.")
			return
		}

		state := q.Get("state")
		fail := func(code, desc string) {
			s.authLimiter.Fail(key)
			s.redirectError(w, r, redirectURI, state, code, desc)
		}
		if len(state) > 2048 {
			fail("invalid_request", "state is too long")
			return
		}
		if q.Get("response_type") != "code" {
			fail("unsupported_response_type", "only response_type=code is supported")
			return
		}
		challenge := q.Get("code_challenge")
		if challenge == "" || !validChallenge(challenge) {
			fail("invalid_request", "a PKCE code_challenge is required")
			return
		}
		if q.Get("code_challenge_method") != "S256" {
			fail("invalid_request", "code_challenge_method must be S256")
			return
		}
		if !s.resourceMatches(q.Get("resource")) {
			fail("invalid_target", "the resource must be "+s.resource)
			return
		}
		requested := strings.Fields(q.Get("scope"))
		if len(requested) == 0 {
			requested = append([]string(nil), s.policy.DefaultScopes...)
		}
		if len(requested) > 20 {
			fail("invalid_scope", "too many scopes")
			return
		}
		for _, sc := range requested {
			if len(sc) > 100 {
				fail("invalid_scope", "a scope is too long")
				return
			}
		}

		raw, hash, err := randomSecret("mrq_")
		if err != nil {
			errorPage(w, http.StatusInternalServerError, "Something went wrong on our side.")
			return
		}
		req := store.NewOAuthRequest{ID: store.NewULID(), Hash: hash, ClientID: client.ID, ClientName: client.Name,
			ClientKind: client.Kind, RedirectURI: redirectURI, RedirectHost: RedirectHost(redirectURI),
			AllLoopback: AllLoopback(client.RedirectURIs), CodeChallenge: challenge, Resource: s.resource,
			Scopes: requested, State: state}
		var view consentView
		err = s.st.UserTx(r.Context(), userID, func(tx *store.UserTx) error {
			if err := tx.CreateOAuthRequest(r.Context(), req); err != nil {
				return err
			}
			var verr error
			view, verr = s.buildConsent(r.Context(), tx, req, raw, "")
			return verr
		})
		if err != nil {
			s.log.ErrorContext(r.Context(), "oauth: authorize", "err", err)
			errorPage(w, http.StatusServiceUnavailable, "Something went wrong on our side. Try again in a moment.")
			return
		}
		renderConsent(w, http.StatusOK, view)
	})
}

// validChallenge: a PKCE S256 challenge is 43 base64url characters.
func validChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// redirectError sends the browser back to the client with an error (303).
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	s.redirectBack(w, r, redirectURI, state, map[string]string{"error": code, "error_description": desc})
}

// redirectBack redirects to redirectURI (already validated against the client)
// with the parameters, state and iss added.
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, redirectURI, state string, params map[string]string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		errorPage(w, http.StatusBadRequest, "The return address is not valid.")
		return
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.issuer)
	u.RawQuery = q.Encode()
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func asClientErr(err error, out *errClient) bool {
	e, ok := err.(errClient)
	if ok {
		*out = e
	}
	return ok
}
