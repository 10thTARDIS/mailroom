// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"log/slog"
	"mime"
	"net/http"
)

// CSRFGuard is the second CSRF layer for COOKIE-authenticated requests (#860).
// The first is SameSite=Lax on the session cookie; this one does not depend on
// the browser honouring it. For any state-changing method (everything except
// GET, HEAD and OPTIONS):
//
//   - a request whose Sec-Fetch-Site header is present and is neither
//     "same-origin" nor "none" is refused with 403. An absent header (an older
//     browser, a non-browser client) passes: the header is a forbidden header
//     name, so a page cannot omit or forge it, and the SameSite layer still
//     covers the clients that do not send it. "same-site" (a sibling
//     subdomain) is refused deliberately: no first-party flow is served from
//     one.
//   - when jsonOnlyPath is non-empty, a POST to exactly that path must carry
//     Content-Type: application/json (parameters such as charset allowed), or
//     it is refused with 415. A cross-site HTML form or a no-cors fetch can
//     only send the three "simple" content types, none of which is JSON, so
//     requiring it turns every simple request into a refused one without a
//     preflight. 415 rather than 403 because it is a statement about the body
//     format, which is what a legitimate-but-misconfigured client needs to see.
//
// Wrap it INSIDE the session authenticator and BEHIND the bearer gate, so only
// session-authenticated requests reach it: an API-key request is not
// ambient-credential authenticated and cannot be forged by a third-party page.
// Refusals are logged at INFO with the route and the header values; no cookie
// or token is ever logged.
func CSRFGuard(next http.Handler, jsonOnlyPath string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			log.Info("csrf: refused cross-site session request",
				"method", r.Method, "route", r.URL.Path,
				"sec_fetch_site", site, "sec_fetch_mode", r.Header.Get("Sec-Fetch-Mode"))
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if jsonOnlyPath != "" && r.Method == http.MethodPost && r.URL.Path == jsonOnlyPath {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				log.Info("csrf: refused non-JSON session request",
					"method", r.Method, "route", r.URL.Path,
					"content_type", r.Header.Get("Content-Type"),
					"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
