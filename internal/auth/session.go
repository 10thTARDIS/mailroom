// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/xoauth2"
)

// contextKey is an unexported type for context.Context keys defined by this
// package, per convention (avoids collisions with keys from other
// packages).
type contextKey int

const (
	userIDContextKey contextKey = iota
	sessionContextKey
)

// Session cookie format (ADR-048 §3): "s1." followed by 256 random bits in
// lowercase base32 without padding. The cookie carries no user id and no
// expiry of its own; the server decides, by looking the hash up.
const (
	sessionCookiePrefix = "s1."
	sessionSecretBytes  = 32
	// sessionSecretChars is the length of 32 bytes in unpadded base32 (ceil(256/5)).
	sessionSecretChars = 52
	sessionCookieLen   = len(sessionCookiePrefix) + sessionSecretChars
)

// SessionCookieLifetime is the browser-side Expires of the session cookie. It is
// NOT the session's validity (the server decides that); it only keeps a closed
// browser from forgetting the cookie, and is re-sent whenever renewal lands.
const SessionCookieLifetime = 30 * 24 * time.Hour

var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// SessionRef identifies a live server-side session.
type SessionRef struct {
	ID     ulid.ULID
	UserID ulid.ULID
	// LastCheckedAt is the last time the session was confirmed with the
	// provider. At sign-in it is the sign-in time.
	LastCheckedAt time.Time
}

// NewSessionInput is what the callback hands the store to create a session.
// The tokens cross this boundary as plain strings and are sealed by the
// implementation; they are bearer credentials and must never reach a log line
// or an error string.
type NewSessionInput struct {
	UserID       ulid.ULID
	TokenHash    [32]byte
	RefreshToken string // "" when the provider issued none
	IDToken      string
	UserAgent    string
	Addr         string
}

// SessionStore is the server-side session record (ADR-048), injected so this
// package stays free of the database. internal/store implements it behind an
// adapter in cmd/mailroom that seals the tokens.
type SessionStore interface {
	// Lookup resolves a cookie-secret hash. ok is false when there is no live
	// session (unknown, revoked, or its user disabled or deleted); err is a
	// failure to find out, which is NOT the same thing.
	Lookup(ctx context.Context, hash [32]byte) (ref SessionRef, ok bool, err error)
	// Create records a new session and returns its id.
	Create(ctx context.Context, in NewSessionInput) (ulid.ULID, error)
	// Touch records use of the session; implementations write at most once a minute.
	Touch(ctx context.Context, ref SessionRef, addr string, at time.Time) error
	// Revoke ends the session with a reason from the store's closed set.
	Revoke(ctx context.Context, ref SessionRef, reason string) error
	// IDToken returns the session's latest ID token, "" when it has none.
	IDToken(ctx context.Context, ref SessionRef) (string, error)
	// RefreshToken returns the session's current provider refresh token, empty
	// when it has none. Logout reads it BEFORE revoking the row, to revoke it at
	// the provider afterwards (RFC 7009).
	RefreshToken(ctx context.Context, ref SessionRef) (xoauth2.Secret, error)
	// Renew runs one silent renewal of the session (ADR-048 section 4) under a
	// per-session lock that holds across processes. It re-reads the row inside
	// the lock and calls do ONLY if the session is still live and its
	// confirmation is still older than staleBefore: when another process
	// renewed while this one waited, or the session was revoked, do is not
	// called and the result says so. do does the provider work; the
	// implementation applies its decision in the same transaction (new tokens
	// sealed and the generation bumped, WITHOUT stamping last_checked_at; or
	// the session revoked as refused; or nothing). An error is a failure of the
	// store, not a verdict on the session: callers treat it as transient.
	Renew(ctx context.Context, ref SessionRef, staleBefore time.Time,
		do func(ctx context.Context, cur RenewCurrent) RenewDecision) (RenewResult, error)
	// Confirm stamps the session confirmed as of now, once a renewal has
	// re-checked identity, only if its generation is still the one Renew
	// returned. at is the stamp; ok is false (and err nil) when another process
	// renewed since, which the caller still counts as confirmed for this
	// request. revoked is true when the row has been revoked (or is gone): the
	// caller must then refuse, because a sign-out raced the renewal. It runs
	// outside the renewal lock.
	Confirm(ctx context.Context, ref SessionRef, generation int64) (at time.Time, ok, revoked bool, err error)
}

// RenewCurrent is what the locked re-read gives the renewal: the opened
// refresh token and the user's subject. The token travels as an
// xoauth2.Secret so it cannot be formatted into a log line by accident.
type RenewCurrent struct {
	// RefreshToken is empty when the provider issued none at sign-in.
	RefreshToken xoauth2.Secret
	// Subject is the user's stored OIDC subject (app.users.oidc_subject).
	Subject       string
	LastCheckedAt time.Time
}

// RenewKind is what the store should write for a renewal.
type RenewKind int

const (
	// RenewLeave writes nothing: a transient failure or no refresh token. The
	// session is NOT marked confirmed.
	RenewLeave RenewKind = iota
	// RenewRotate durably stores the new tokens (a refresh grant consumes the old
	// one) and bumps the generation. It does NOT confirm the session: that is
	// SessionStore.Confirm, after identity has been re-checked.
	RenewRotate
	// RenewRefuse revokes the session as refused.
	RenewRefuse
)

// RenewDecision is the renewal's verdict. RefreshToken and IDToken are the
// provider's new values; an empty one keeps what is stored (a provider that
// does not rotate refresh tokens, or sent no ID token).
type RenewDecision struct {
	Kind         RenewKind
	RefreshToken xoauth2.Secret
	IDToken      xoauth2.Secret
}

// RenewResult is how a Renew call ended.
type RenewResult struct {
	// Ran reports whether do was called.
	Ran bool
	// Revoked reports that the session is revoked: it was already, or the
	// decision revoked it.
	Revoked bool
	// LastCheckedAt is the session's confirmation time after the call.
	LastCheckedAt time.Time
	// Generation is the session's generation after a rotation: the value
	// Confirm must match.
	Generation int64
}

// Revocation reasons this package passes to SessionStore.Revoke.
const (
	revokeSignedOut = "signed_out"
	revokeReplaced  = "replaced"
	revokeExpired   = "expired"
	revokeRefused   = "refused"
)

// sessionOutcome is how a request's session check ended. The middleware must
// tell a store failure from an unauthenticated request: answering a database
// blip with 401 would make the web client treat it as "session expired" and
// bounce every open tab to sign-in (ADR-048 §3).
type sessionOutcome int

const (
	sessionOK sessionOutcome = iota
	sessionUnauthenticated
	sessionStoreError
)

// mintSessionSecret returns a fresh cookie value ("s1." + 256 random bits).
func mintSessionSecret() (string, error) {
	buf := make([]byte, sessionSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return sessionCookiePrefix + strings.ToLower(secretEncoding.EncodeToString(buf)), nil
}

// hashSessionCookie is the lookup key: the SHA-256 of the whole cookie value.
// The store never holds the cookie itself, so a database read yields nothing
// that authenticates.
func hashSessionCookie(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}

// wellFormedSessionCookie reports whether v has the exact shape mintSessionSecret
// produces. Anything else (a legacy signed cookie, garbage) is unauthenticated
// without touching the store.
func wellFormedSessionCookie(v string) bool {
	if len(v) != sessionCookieLen || !strings.HasPrefix(v, sessionCookiePrefix) {
		return false
	}
	for _, c := range v[len(sessionCookiePrefix):] {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// newSessionCookie builds the HttpOnly session cookie carrying value. See the
// package doc's "Sessions" section for the design (server-side record,
// SameSite=Lax rationale).
func (a *Authenticator) newSessionCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     a.cfg.SessionCookieName,
		Value:    value,
		Path:     "/",
		Domain:   a.cfg.CookieDomain,
		Expires:  a.clock().Add(SessionCookieLifetime),
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// clearedSessionCookie builds a cookie that immediately expires the session
// cookie in the browser, used by LogoutHandler.
func (a *Authenticator) clearedSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     a.cfg.SessionCookieName,
		Value:    "",
		Path:     "/",
		Domain:   a.cfg.CookieDomain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// clock returns the current time; tests may set Authenticator.now.
func (a *Authenticator) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// logger returns the configured logger or the process default.
func (a *Authenticator) logger() *slog.Logger {
	if a.log != nil {
		return a.log
	}
	return slog.Default()
}

// lookupSession reads the session cookie from r and resolves it in the store,
// with no side effects and no expiry judgment. unauthenticated covers a missing
// or malformed cookie, an unknown hash and a revoked row alike (callers must not
// distinguish them); storeError is a failure of the lookup itself.
func (a *Authenticator) lookupSession(r *http.Request) (SessionRef, sessionOutcome) {
	cookie, err := r.Cookie(a.cfg.SessionCookieName)
	if err != nil || !wellFormedSessionCookie(cookie.Value) {
		return SessionRef{}, sessionUnauthenticated
	}
	ref, ok, err := a.cfg.SessionStore.Lookup(r.Context(), hashSessionCookie(cookie.Value))
	if err != nil {
		a.logger().ErrorContext(r.Context(), "session lookup failed", "err", err)
		return SessionRef{}, sessionStoreError
	}
	if !ok {
		return SessionRef{}, sessionUnauthenticated
	}
	return ref, sessionOK
}

// session is the per-request check: lookup, silent renewal when the last
// confirmation is older than RenewInterval, the outage-allowance judgment, and
// a rate-limited touch. On sessionOK the session is live and was used; reissue
// is non-nil when a renewal confirmed it and the cookie should be sent again
// with a fresh Expires.
func (a *Authenticator) session(r *http.Request) (ref SessionRef, outcome sessionOutcome, reissue *http.Cookie) {
	ref, outcome = a.lookupSession(r)
	if outcome != sessionOK {
		return SessionRef{}, outcome, nil
	}
	now := a.clock()
	if now.Sub(ref.LastCheckedAt) > a.cfg.RenewInterval {
		// Renew BEFORE serving (ADR-048 section 4). What the attempt cannot
		// settle (a provider outage, no refresh token) falls through to the
		// allowance below.
		res := a.renew(r.Context(), ref)
		switch res.kind {
		case renewConfirmed:
			ref.LastCheckedAt = res.lastChecked
			if c, err := r.Cookie(a.cfg.SessionCookieName); err == nil {
				reissue = a.newSessionCookie(c.Value)
			}
		case renewRefused:
			return SessionRef{}, sessionUnauthenticated, nil
		}
	}
	if now.Sub(ref.LastCheckedAt) > a.cfg.OutageAllowance {
		// Unconfirmed for longer than the allowance: refused, and ended so it
		// cannot be revived. Best effort; the refusal does not depend on it.
		if err := a.cfg.SessionStore.Revoke(r.Context(), ref, revokeExpired); err != nil {
			a.logger().ErrorContext(r.Context(), "revoking an expired session failed", "err", err)
		}
		return SessionRef{}, sessionUnauthenticated, nil
	}
	// A client that gave up while a renewal ran has nothing left to record, and a
	// Touch on its cancelled context would only log an error per disconnect.
	if r.Context().Err() == nil {
		if err := a.cfg.SessionStore.Touch(r.Context(), ref, a.displayAddr(r), now); err != nil {
			a.logger().ErrorContext(r.Context(), "recording session use failed", "err", err)
		}
	}
	return ref, sessionOK, reissue
}

// contextWithUserID returns a copy of ctx carrying userID, retrievable with
// UserIDFromContext. Unexported: only Middleware sets it, so user-id
// injection is solely a function of a verified session.
func contextWithUserID(ctx context.Context, userID ulid.ULID) context.Context {
	return context.WithValue(ctx, userIDContextKey, userID)
}

// contextWithSession returns a copy of ctx carrying the session, retrievable
// with SessionFromContext.
func contextWithSession(ctx context.Context, ref SessionRef) context.Context {
	return context.WithValue(ctx, sessionContextKey, ref)
}

// ContextWithSessionForTest returns ctx carrying ref as Middleware would, so a
// package that sits behind the middleware can test what it does for a request
// that has a session (the devices list, push registration, event streams)
// without standing up an Authenticator. It sets NO user id (that stays
// Middleware's alone), and nothing outside tests calls it.
func ContextWithSessionForTest(ctx context.Context, ref SessionRef) context.Context {
	return contextWithSession(ctx, ref)
}

// UserIDFromContext returns the mailroom user id injected by Middleware, if
// any. Handlers mounted behind Middleware can rely on ok being true;
// anything else (a handler not behind Middleware, or a bug) reports
// ok == false rather than a zero-value ULID.
func UserIDFromContext(ctx context.Context) (ulid.ULID, bool) {
	userID, ok := ctx.Value(userIDContextKey).(ulid.ULID)
	return userID, ok
}

// SessionFromContext returns the browser session Middleware authenticated the
// request with. ok is false for dev-auth and API-key requests, which have no
// session.
func SessionFromContext(ctx context.Context) (SessionRef, bool) {
	ref, ok := ctx.Value(sessionContextKey).(SessionRef)
	return ref, ok
}
