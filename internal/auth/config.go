// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Default values applied by New/Config.withDefaults when the corresponding
// Config field is left zero.
const (
	DefaultSessionCookieName = "mailroom_session"
	DefaultStateCookieName   = "mailroom_auth_state"
	DefaultLogoutPath        = "/auth/logout"
	DefaultOutageAllowance   = 12 * time.Hour
	DefaultStateTTL          = 10 * time.Minute
	// DefaultRenewInterval is how long a session may go without a provider
	// confirmation before the next request renews it (ADR-048 section 4).
	DefaultRenewInterval = 15 * time.Minute
	// DefaultRenewTimeout is the total budget of one renewal: the token call and
	// the UserInfo call share it.
	DefaultRenewTimeout = 5 * time.Second
	// DefaultRenewRetrySpacing is the minimum gap between two FAILED renewal
	// attempts for one session; requests inside it are served under the outage
	// allowance without a provider call.
	DefaultRenewRetrySpacing = time.Minute
	// DefaultRenewConcurrency bounds how many renewals run at once in one
	// process; each holds a database connection across the provider call.
	DefaultRenewConcurrency = 2
)

// DefaultScopes is used when Config.Scopes is empty: the standard OIDC scope,
// the two profile/email claim-bearing scopes ResolveUser needs to provision a
// user, "offline_access" (without which current providers issue no refresh
// token, which server-side sessions store for later silent renewal, ADR-048),
// and "groups" so a group-driven role (e.g. an admin group) is decidable
// at login. A provider that does not know "groups" simply omits it from the
// grant (OAuth2 authorization servers ignore unsupported scopes); a provider
// that does — Authentik, with a groups scope mapping — then emits the "groups"
// claim in the ID token or on UserInfo, both of which CallbackHandler reads.
var DefaultScopes = []string{"openid", "profile", "email", "offline_access", "groups"}

// MinSigningKeyLen is the minimum accepted length, in bytes, of
// Config.SigningKey. HMAC-SHA256 keys shorter than this undermine the
// unforgeability of session and login-state cookies, so New rejects them.
const MinSigningKeyLen = 32

// Config configures an Authenticator. It is this package's own config,
// populated by the caller (cmd/mailroom, typically sourced through its own
// internal/config) — auth deliberately does not read the environment or
// depend on internal/config, keeping it usable outside this project's
// wiring.
type Config struct {
	// IssuerURL is the OIDC provider's issuer, used for discovery
	// (oidc.NewProvider). E.g. "${OIDC_ISSUER}", which for an Authentik
	// deployment looks like "https://auth.example.com/application/o/mailroom/".
	// Required.
	IssuerURL string

	// ClientID and ClientSecret are the confidential-client credentials
	// registered with the provider. Required.
	ClientID     string
	ClientSecret string

	// RedirectURL is this app's callback URL, registered with the
	// provider (e.g. "https://mail.example.com/auth/callback"). Required.
	RedirectURL string

	// Scopes requested from the provider. Defaults to DefaultScopes if
	// empty.
	Scopes []string

	// SigningKey is the HMAC-SHA256 key used to sign the login-state cookie.
	// Required, at least MinSigningKeyLen bytes. Rotate by deploying a new
	// key; in-flight logins simply fail verification and the user starts
	// again. Sessions are NOT signed (they are server-side records, ADR-048),
	// so rotating the key no longer ends them.
	SigningKey []byte

	// CookieSecure sets the Secure flag on every cookie this package
	// issues. Must be true whenever the app is served over HTTPS (i.e.
	// always in production); false is only for local plain-HTTP
	// development.
	CookieSecure bool

	// CookieDomain optionally scopes cookies to a specific domain. Empty
	// (the default) leaves cookies host-only, which is usually what you
	// want.
	CookieDomain string

	// SessionCookieName / StateCookieName override the cookie names. Default
	// to DefaultSessionCookieName / DefaultStateCookieName.
	SessionCookieName string
	StateCookieName   string

	// SessionStore holds the server-side session records (ADR-048). Required
	// by New. It is an interface so this package stays free of the database.
	SessionStore SessionStore

	// LogoutPath is the path LogoutHandler is mounted at. Since ADR-048 it is
	// used for one thing only: clearing the retired path-scoped logout cookie
	// (#314) that earlier releases set, which the browser keeps unless the
	// clearing cookie names the same Path. It can be dropped one release
	// after the logout cookie's last possible lifetime has passed.
	// Defaults to DefaultLogoutPath.
	LogoutPath string

	// PostLogoutRedirectURI is the ABSOLUTE url the provider sends the
	// browser back to after ending its session, passed as
	// post_logout_redirect_uri. Most providers require it to be registered
	// against the client and reject an unregistered value, so it is optional:
	// left empty, the parameter is omitted and the provider falls back to its
	// own configured destination.
	//
	// Note this is a different thing from WithPostLogoutRedirect, which is a
	// LOCAL path used when there is no provider round trip to make.
	PostLogoutRedirectURI string

	// OutageAllowance bounds how long a session keeps working without a
	// successful confirmation (its last_checked_at): a session older than this
	// is refused and revoked as expired. Defaults to DefaultOutageAllowance.
	//
	// Silent renewal (ADR-048 section 4) re-confirms a session every
	// RenewInterval, so this is what an outage of the provider, or a session
	// with no refresh token, can cost: how long a session may go unconfirmed.
	OutageAllowance time.Duration

	// RenewInterval is how stale a session's last confirmation may be before a
	// request renews it, before serving. Defaults to DefaultRenewInterval.
	RenewInterval time.Duration

	// RenewTimeout is the total budget of one renewal (token call plus UserInfo
	// call); expiry is a transient failure. Defaults to DefaultRenewTimeout.
	RenewTimeout time.Duration

	// RenewRetrySpacing is the minimum gap between failed renewal attempts for
	// one session. Defaults to DefaultRenewRetrySpacing.
	RenewRetrySpacing time.Duration

	// RenewConcurrency bounds the renewals running at once in this process
	// (minimum 1; defaults to DefaultRenewConcurrency). Each holds a pooled
	// database connection while it talks to the provider, so an unbounded burst
	// of stale sessions could take the whole pool. A request over the limit is
	// served under the outage allowance and tries again on its next request; the
	// overflow is not counted as a failed attempt.
	RenewConcurrency int

	// TrustedProxies are the reverse proxies whose X-Forwarded-For is believed
	// when recording the address a session last connected from (see
	// displayAddr). Empty trusts none: the connection's own address is used.
	// Display data only; the bearer rate limiter does not read it.
	TrustedProxies []netip.Prefix

	// StateTTL bounds how long a login attempt (the state/nonce/PKCE
	// login-state cookie set by LoginHandler) remains valid before
	// CallbackHandler rejects it. Defaults to DefaultStateTTL.
	StateTTL time.Duration
}

// validate checks the fields New requires to be set explicitly (those
// without a meaningful default).
func (c Config) validate() error {
	var problems []string
	if strings.TrimSpace(c.IssuerURL) == "" {
		problems = append(problems, "IssuerURL is required")
	}
	if c.ClientID == "" {
		problems = append(problems, "ClientID is required")
	}
	if c.ClientSecret == "" {
		problems = append(problems, "ClientSecret is required")
	}
	if c.RedirectURL == "" {
		problems = append(problems, "RedirectURL is required")
	}
	if c.SessionStore == nil {
		problems = append(problems, "SessionStore is required")
	}
	if len(c.SigningKey) < MinSigningKeyLen {
		problems = append(problems, fmt.Sprintf("SigningKey must be at least %d bytes, got %d", MinSigningKeyLen, len(c.SigningKey)))
	}
	if len(problems) > 0 {
		return fmt.Errorf("auth: invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// withDefaults returns a copy of c with zero-valued optional fields filled
// in from the package defaults.
func (c Config) withDefaults() Config {
	if len(c.Scopes) == 0 {
		c.Scopes = DefaultScopes
	}
	if c.SessionCookieName == "" {
		c.SessionCookieName = DefaultSessionCookieName
	}
	if c.StateCookieName == "" {
		c.StateCookieName = DefaultStateCookieName
	}
	if c.LogoutPath == "" {
		c.LogoutPath = DefaultLogoutPath
	}
	if c.OutageAllowance <= 0 {
		c.OutageAllowance = DefaultOutageAllowance
	}
	if c.StateTTL <= 0 {
		c.StateTTL = DefaultStateTTL
	}
	if c.RenewInterval <= 0 {
		c.RenewInterval = DefaultRenewInterval
	}
	if c.RenewTimeout <= 0 {
		c.RenewTimeout = DefaultRenewTimeout
	}
	if c.RenewRetrySpacing <= 0 {
		c.RenewRetrySpacing = DefaultRenewRetrySpacing
	}
	if c.RenewConcurrency < 1 {
		c.RenewConcurrency = DefaultRenewConcurrency
	}
	return c
}
