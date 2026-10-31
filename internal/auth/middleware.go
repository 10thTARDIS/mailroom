// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/url"
)

// middlewareOptions configures Middleware's behavior on an unauthenticated
// request.
type middlewareOptions struct {
	onUnauthenticated http.HandlerFunc
}

// MiddlewareOption customizes Middleware.
type MiddlewareOption func(*middlewareOptions)

// WithUnauthenticatedRedirect makes Middleware redirect unauthenticated
// requests to loginPath (with a "return_to" query parameter carrying the
// original request path) instead of the default 401 response. Use this for
// browser-facing routes; leave the default (401) for API/JMAP routes, where
// a redirect would be meaningless to a programmatic client and the user id
// must come from a verified token rather than any request-driven flow
// (spec §10).
func WithUnauthenticatedRedirect(loginPath string) MiddlewareOption {
	return func(o *middlewareOptions) {
		o.onUnauthenticated = func(w http.ResponseWriter, r *http.Request) {
			dest := loginPath
			rt := r.URL.RequestURI()
			if q := r.URL.Query(); q.Has("code") || q.Has("state") {
				// An OAuth authorization response (the mail-provider connect
				// callback, #52) reaching a signed-out browser: its code and
				// state must not be carried on into return_to, where they would
				// travel through the sign-in round trip. Keep the path only;
				// the connect is simply started again after signing in.
				rt = r.URL.EscapedPath()
			}
			if rt != "" && rt != "/" {
				dest += "?return_to=" + url.QueryEscape(rt)
			}
			http.Redirect(w, r, dest, http.StatusFound)
		}
	}
}

// Middleware requires a valid session cookie, injecting the mailroom user
// id (UserIDFromContext) and the session (SessionFromContext) into the request
// context before calling next. A failure to check the session at all (a
// database error) answers 503, not 401. By default, a missing/invalid/expired session results in
// 401 Unauthorized (suited to a JMAP/API mux); pass
// WithUnauthenticatedRedirect to redirect to a login page instead (suited
// to browser-facing routes).
func (a *Authenticator) Middleware(next http.Handler, opts ...MiddlewareOption) http.Handler {
	var o middlewareOptions
	for _, opt := range opts {
		opt(&o)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, outcome, reissue := a.session(r)
		if outcome == sessionStoreError {
			// A failure to CHECK the session is not an unauthenticated request:
			// 401 would make the web client read it as "session expired" and
			// bounce every open tab to sign-in. The cookie is left alone.
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if outcome != sessionOK {
			if o.onUnauthenticated != nil {
				o.onUnauthenticated(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if reissue != nil {
			// A renewal just confirmed the session: send the cookie again so a
			// closed browser keeps it for another 30 days.
			http.SetCookie(w, reissue)
		}
		ctx := contextWithSession(contextWithUserID(r.Context(), ref.UserID), ref)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
