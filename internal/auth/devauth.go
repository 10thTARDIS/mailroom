// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"

	"github.com/oklog/ulid/v2"
)

// DevAuth is an INSECURE escape hatch: it performs NO AUTHENTICATION AT ALL.
//
// It replaces Middleware with a middleware that injects userID into every
// request's context — the same context key Middleware uses, so
// UserIDFromContext (and therefore the whole API layer) works unchanged. Every
// request, from anyone, from anywhere, is treated as that user. There is no
// cookie, no token, no session, and no way for a request to fail
// authentication.
//
// It exists so a test instance is exercisable end to end without standing up an
// OIDC provider. NEVER enable it on an internet-reachable instance: anyone who
// can reach the port reads and writes the users' mail.
//
// Enabling it is deliberately awkward and loud: cmd/mailroom selects it only
// when the OIDC block is entirely absent (a partially configured OIDC block is a
// config error, not a silent fallback to open access — see
// config.Config.validateAuth) AND MAILROOM_DEV_AUTH_SUBJECT is explicitly set,
// and it logs a banner warning at startup for as long as it runs.
//
// A zero userID is a wiring bug (nothing was provisioned), and the returned
// handler fails closed on it: every request gets 503 rather than a zero-value
// identity that could be mistaken for a real user downstream.
func DevAuth(next http.Handler, userID ulid.ULID) http.Handler {
	if userID == (ulid.ULID{}) {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "dev-auth: no user id configured", http.StatusServiceUnavailable)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(contextWithUserID(r.Context(), userID)))
	})
}

// DevAuthWarning is the banner cmd/mailroom logs at startup (and the text this
// package's own tests assert on) whenever DevAuth is in play. It is exported so
// the warning is a single, greppable string rather than something a caller might
// paraphrase into harmlessness.
const DevAuthWarning = "INSECURE: dev-auth is enabled — every request is treated as the operator, " +
	"with NO authentication. This instance MUST NOT be reachable from an untrusted network. " +
	"Configure the OIDC block to disable it."
