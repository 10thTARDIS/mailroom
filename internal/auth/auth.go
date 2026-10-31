// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/oklog/ulid/v2"
	"golang.org/x/oauth2"
)

// ErrCannotProvision is the sentinel a ResolveUser implementation wraps into
// the error it returns when an identity was VERIFIED but the server cannot
// create or match an account for it because of the SERVER's OWN
// records — not anything wrong with the user's credentials or account
// (Forgejo #574; cmd/mailroom's provisioningResolver wraps it around
// store.ErrLoginEmailTaken). CallbackHandler tests every ResolveUser failure
// against it with errors.Is and gives a match its own closed-set code
// (page.go's ErrorUnprovisionable) and status (409) instead of the generic
// ErrorDenied/403 every other refusal gets — the user cannot fix this one by
// retrying or signing in with a different account, so it must not read like
// one they can.
var ErrCannotProvision = errors.New("auth: cannot provision an account for this identity")

// ErrRefused is the sentinel a ResolveUser implementation wraps into the error
// it returns when it deliberately REFUSES the identity: a token with no email
// to provision from, a subject disabled or deleted here, an email that belongs
// to another account. Silent renewal (ADR-048 section 4a) revokes a session only
// on this sentinel (or ErrCannotProvision); any other resolver error, such as a
// database connection reset, is a failure to decide, not a refusal, and leaves
// the session alone.
var ErrRefused = errors.New("auth: identity refused")

// Claims is the subset of OIDC ID token claims this package extracts and
// hands to ResolveUser. It is intentionally minimal — a ResolveUser
// implementation that needs more can be written against the provider's
// documented claim set directly, but Subject/Email/PreferredUsername cover
// the common provisioning/lookup cases.
type Claims struct {
	// Subject is the provider's stable, unique identifier for the end
	// user (the "sub" claim). This — not Email — is the correct primary
	// key for provider-account identity, since a user's email can change.
	Subject string

	// Email is the "email" claim, if the provider returned one.
	Email string

	// EmailVerified is the "email_verified" claim.
	EmailVerified bool

	// PreferredUsername is the "preferred_username" claim, if present.
	PreferredUsername string

	// Name is the "name" claim (the user's display name), if present. A
	// ResolveUser that auto-provisions uses it, falling back to
	// PreferredUsername, for app.users.display_name.
	Name string

	// Groups is the "groups" claim: the provider's group memberships for this
	// user, if the provider emits them (requested via the "groups" scope; see
	// DefaultScopes). It is what an app-binding-is-the-gate deployment reads to
	// decide a role — e.g. membership of an admin group grants admin. It is
	// nil/empty when the provider returns no groups, which a caller must treat
	// as "no privileged membership", never as an error.
	Groups []string
}

// ResolveUser maps verified OIDC claims to a mailroom user id. Implementations
// belong in the store layer (CLAUDE.md: internal/store is the only package
// that touches the database) — this package only calls the function it is
// given. A non-nil error aborts the login: CallbackHandler responds with an
// error status and does not set a session cookie.
type ResolveUser func(ctx context.Context, claims Claims) (ulid.ULID, error)

// Authenticator implements the OIDC Authorization Code + PKCE login flow,
// server-side sessions, and the request-authentication middleware
// described in the package doc. Construct with New.
type Authenticator struct {
	cfg          Config
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	resolveUser  ResolveUser

	// provider is the discovered OIDC provider, kept so CallbackHandler can hit
	// the UserInfo endpoint to backfill claims (groups/name/email) that a
	// provider chose to expose there rather than in the ID token. It is nil in
	// the package's own unit tests (which construct Authenticator directly and
	// never call New), and the UserInfo backfill is skipped when it is nil.
	provider *oidc.Provider

	// userInfoFn overrides the UserInfo call (a test seam; nil means the
	// discovered provider's endpoint).
	userInfoFn func(ctx context.Context, token *oauth2.Token) (userInfoResult, error)

	// tokenAuthPost sends the client credentials in the token request's body
	// (client_secret_post) instead of HTTP Basic, when the provider's discovery
	// document advertises only that method. Renewal builds its own token
	// request so that it can never be retried (see renew.go).
	tokenAuthPost bool

	// renewal is the in-process state of silent renewal: the singleflight and
	// the retry spacing (renew.go). The zero value is ready.
	renewal renewState

	// failureRedirect, when set, is where CallbackHandler sends a browser whose
	// login failed — with ?error=<closed-set code> — instead of writing a bare
	// error body. See WithFailureRedirect.
	failureRedirect string

	// postLogoutRedirect is where LogoutHandler sends the browser when the
	// request carries no return_to. Defaults to "/".
	postLogoutRedirect string

	// log receives session diagnostics; nil means slog.Default(). Nothing
	// logged may contain a token (they are bearer credentials).
	log *slog.Logger

	// now overrides time.Now for tests.
	now func() time.Time

	// endSessionEndpoint is the provider's RP-initiated logout endpoint, read
	// off the discovery document (#314). It is "" when the provider does not
	// publish one — RP-initiated logout is an optional part of OIDC, so its
	// absence is a normal deployment, not an error — and "" in this package's
	// own unit tests unless one is set explicitly. LogoutHandler falls back to
	// a local redirect whenever it is empty.
	endSessionEndpoint string

	// revocationEndpoint is the provider's RFC 7009 token revocation endpoint,
	// read off the discovery document like endSessionEndpoint. "" when the
	// provider publishes none, which makes RevokeRefreshToken a no-op.
	revocationEndpoint string
}

// Option customizes an Authenticator built by New.
type Option func(*Authenticator)

// WithFailureRedirect makes CallbackHandler respond to a FAILED login with a
// 302 to path carrying an "error" query parameter from the closed set
// {ErrorDenied, ErrorExpired, ErrorFailed} — instead of the default, which is a
// plain-text error body with a 4xx/5xx status.
//
// Point it at the sign-in page (Page). This is what turns "your subject is not
// the operator" from a bare 403 — or, worse, an automatic bounce back to a
// provider that will happily re-issue the same identity forever — into a
// sentence the user can read and act on. The login still fails exactly as
// hard: no session cookie is set on any path through CallbackHandler that does
// not resolve a user.
//
// The default (unset) behavior is kept for callers that mount CallbackHandler
// without a sign-in page: a redirect to a page that does not exist would be a
// worse error than the error.
func WithFailureRedirect(path string) Option {
	return func(a *Authenticator) { a.failureRedirect = path }
}

// WithLogger sets the logger for session diagnostics. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(a *Authenticator) { a.log = l }
}

// WithPostLogoutRedirect sets where LogoutHandler sends a browser that supplied
// no return_to. Defaults to "/". Point it at the sign-in page (with
// ?notice=signed_out) so signing out lands on a page that says so, rather than
// on "/" — which, being behind the gate, would only bounce there anyway.
func WithPostLogoutRedirect(path string) Option {
	return func(a *Authenticator) { a.postLogoutRedirect = path }
}

// New performs OIDC discovery against cfg.IssuerURL (oidc.NewProvider),
// builds the oauth2.Config used for the Authorization Code flow, and
// constructs an ID token verifier bound to cfg.ClientID. resolveUser maps a
// verified identity to a mailroom user id (see ResolveUser) and must be
// non-nil.
//
// New makes a network call (provider discovery) and is therefore not
// exercised by this package's unit tests, which construct Authenticator
// values directly against fakes instead — see the package doc's "Testing
// scope" section.
func New(ctx context.Context, cfg Config, resolveUser ResolveUser, opts ...Option) (*Authenticator, error) {
	if resolveUser == nil {
		return nil, fmt.Errorf("auth: New: resolveUser must not be nil")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("auth: OIDC discovery against %s: %w", cfg.IssuerURL, err)
	}

	oauth2Config := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       cfg.Scopes,
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})

	// end_session_endpoint is not part of oidc.Provider's typed surface, so
	// read it off the raw discovery document. A provider that does not publish
	// one simply leaves this empty and logout stays local (#314) — that is an
	// ordinary configuration, not a failure, so a decode error is swallowed
	// rather than aborting startup over an optional field.
	var discovery struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	_ = provider.Claims(&discovery)

	// Which client-authentication method the token endpoint takes. RFC 6749
	// requires HTTP Basic, so it is the default; a provider that advertises
	// only client_secret_post gets the credentials in the body.
	var methods struct {
		TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	_ = provider.Claims(&methods)

	a := &Authenticator{
		cfg:                cfg,
		oauth2Config:       oauth2Config,
		verifier:           verifier,
		resolveUser:        resolveUser,
		provider:           provider,
		postLogoutRedirect: "/",
		endSessionEndpoint: discovery.EndSessionEndpoint,
		revocationEndpoint: discovery.RevocationEndpoint,
		tokenAuthPost:      prefersClientSecretPost(methods.TokenEndpointAuthMethods),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}
