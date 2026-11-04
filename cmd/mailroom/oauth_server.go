// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	"mailroom/internal/oauthserver"
	"mailroom/internal/store"
)

// The OAuth authorization server for /mcp (ADR-050): wiring. It is on exactly
// when cfg.OAuthIssuer() is non-empty (a public https URL is known and
// MAILROOM_OAUTH is not off); otherwise
// every route below answers 404 and /mcp behaves exactly as it did, keys only.

// buildOAuthServer returns nil when the OAuth surface is off.
func buildOAuthServer(st *store.Store, cfg *config.Config, svc *api.API, logger *slog.Logger) (*oauthserver.Server, error) {
	issuer := cfg.OAuthIssuer()
	if st == nil {
		return nil, nil // no database: nothing to issue tokens from
	}
	if cfg.OAuthDisabled {
		logger.Info("OAuth for AI tools is off: "+config.EnvOAuth+" is set to off; /mcp accepts API keys only, "+
			"and connected apps work again when it is turned back on", "env", config.EnvOAuth)
		return nil, nil
	}
	if issuer == "" {
		if why := cfg.OAuthIssuerError(); why != "" {
			logger.Warn("OAuth for AI tools is off: "+why, "env", config.EnvPublicURL)
		} else {
			logger.Info("OAuth for AI tools is off: no public URL is configured; /mcp accepts API keys only", "env", config.EnvPublicURL)
		}
		return nil, nil
	}
	s, err := oauthserver.New(oauthserver.Config{
		Issuer: issuer, Store: st, Policy: api.OAuthPolicy(), Approve: api.ApproveOAuthGrant,
		Alert:                   svc.AlertOAuthRefreshReuse,
		AllowPrivateClientHosts: cfg.OAuthAllowPrivateClientHosts, DCR: cfg.OAuthDCR, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	logger.Info("OAuth for AI tools is on", "issuer", issuer, "resource", s.Resource())
	return s, nil
}

// oauthAccessResolver adapts the store's access-token lookup to the bearer
// gate. With no store every token is unknown: fail closed.
func oauthAccessResolver(st *store.Store) auth.ResolveAPIKey {
	return func(ctx context.Context, hash [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
		if st == nil {
			return ulid.ULID{}, ulid.ULID{}, false, nil
		}
		return st.ResolveOAuthAccessToken(ctx, hash)
	}
}

// oauthRoutes is the OAuth surface's part of the route inventory. It is
// registered in every build (with 404 handlers when the surface is off), so the
// gate tests always cover it.
func oauthRoutes(s *oauthserver.Server) []route {
	nf := oauthserver.NotFound()
	disc, prm, authz, consent, token, revoke := nf, nf, nf, nf, nf, nf
	if s != nil {
		disc, prm, authz = s.AuthorizationServer(), s.ProtectedResource(), s.Authorize()
		// The decision carries its own CSRF guard (a cross-site POST is refused
		// before the handler); the request id (unguessable, single use, the
		// caller's own) is the second layer.
		consent = s.Consent()
		token, revoke = s.Token(), s.Revoke()
	}
	return []route{
		{pattern: "GET /.well-known/oauth-authorization-server", gate: gatePublic, handler: disc,
			probes: []probe{get("/.well-known/oauth-authorization-server")}},
		{pattern: "GET /.well-known/oauth-protected-resource/mcp", gate: gatePublic, handler: prm,
			probes: []probe{get("/.well-known/oauth-protected-resource/mcp")}},
		{pattern: "GET /.well-known/oauth-protected-resource", gate: gatePublic, handler: prm,
			probes: []probe{get("/.well-known/oauth-protected-resource")}},
		// A BROWSER route: a signed-out visitor is sent through sign-in and back
		// to this URL; the consent screen is shown only to a signed-in user.
		{pattern: "GET " + auth.OAuthAuthorizePath, gate: gateBrowser, handler: authz,
			probes: []probe{get(auth.OAuthAuthorizePath + "?response_type=code&client_id=mc_0&state=s")}},
		{pattern: "POST /oauth/consent", gate: gateBrowser, handler: consent,
			probes: []probe{post("/oauth/consent")}},
		// Machine endpoints for public clients: reached with no session, and
		// answering JSON errors rather than redirects.
		{pattern: "POST /oauth/token", gate: gatePublic, handler: token, probes: []probe{post("/oauth/token")}},
		{pattern: "POST /oauth/revoke", gate: gatePublic, handler: revoke, probes: []probe{post("/oauth/revoke")}},
	}
}

// runOAuthSweep deletes expired OAuth tokens, codes, requests and cached client
// documents once now and then every interval (migration 00087): a ticker, like
// the job and session sweeps, because a missed run costs nothing.
func runOAuthSweep(ctx context.Context, st *store.Store, interval time.Duration, log *slog.Logger) {
	sweepOAuthOnce(ctx, st, time.Now(), log)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sweepOAuthOnce(ctx, st, now, log)
		}
	}
}

// sweepOAuthOnce logs the count PER KIND, every kind listed even at zero.
func sweepOAuthOnce(ctx context.Context, st *store.Store, now time.Time, log *slog.Logger) {
	counts, err := st.SweepOAuth(ctx, now, store.OAuthSweepGrace)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("oauth sweep failed", "err", err)
		}
		return
	}
	attrs := make([]any, 0, 2*len(store.OAuthSweepKinds)+2)
	for _, k := range store.OAuthSweepKinds {
		attrs = append(attrs, k, counts[k])
	}
	attrs = append(attrs, "grace", store.OAuthSweepGrace.String())
	log.Info("oauth sweep", attrs...)
}
