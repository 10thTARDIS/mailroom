// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/egress"
	"mailroom/internal/oauthmail"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
	"mailroom/internal/webpush"
	"mailroom/internal/xoauth2"
)

// Mail-provider OAuth wiring (#52, ADR-046 increment 3).
//
// ONE token Manager per process, shared by the sync resolver (IMAP), the send
// worker (SMTP) and the connect flow, so they share one access-token cache and
// one in-flight refresh per account. Nothing is built when no provider is
// configured: tokens stays a NIL INTERFACE (never a typed nil *Manager, which
// would compare non-nil), and an OAuth account then fails with
// syncrunner.ErrOAuthNotConfigured exactly as before this increment.

// oauthWiring is what the rest of main needs from the OAuth setup.
type oauthWiring struct {
	// tokens is the Manager as the resolver and compose see it; nil when no
	// provider is configured.
	tokens syncrunner.AccessTokens
	// connector runs the connect flow; nil when no provider is configured.
	connector *oauthmail.Connector
	// ops reports providers refusing this server's client registration, for
	// admins in Account/getHealth (#52); nil when no provider is configured.
	ops api.OAuthOperatorStatus
}

// oauthFlowSweepInterval is how often expired pending connects are deleted.
// Each start also clears the starting user's own; this catches users who
// never come back.
const oauthFlowSweepInterval = time.Hour

// buildOAuth builds the registry, Manager and Connector from cfg. A
// misconfiguration (a bad tenant, no public URL to build the callback from)
// is a startup error: the operator asked for OAuth and would otherwise get a
// server that silently offers no Connect button.
func buildOAuth(st *store.Store, keys crypto.KeyProvider, cfg *config.Config, logger *slog.Logger) (oauthWiring, error) {
	ms, err := oauthmail.Microsoft(oauthmail.Client{ID: cfg.OAuthMicrosoftClientID,
		Secret: xoauth2.Secret(cfg.OAuthMicrosoftClientSecret)}, cfg.OAuthMicrosoftTenant)
	if err != nil {
		return oauthWiring{}, fmt.Errorf("main: %s: %w", config.EnvOAuthMicrosoftTenant, err)
	}
	reg, err := oauthmail.NewRegistry(
		oauthmail.Google(oauthmail.Client{ID: cfg.OAuthGoogleClientID, Secret: xoauth2.Secret(cfg.OAuthGoogleClientSecret)}),
		ms,
	)
	if err != nil {
		return oauthWiring{}, fmt.Errorf("main: oauth registry: %w", err)
	}
	if len(reg.Enabled()) == 0 {
		logger.Info("mail-provider sign-in (OAuth) is off: no provider is configured",
			"env", config.EnvOAuthGoogleClientID+", "+config.EnvOAuthMicrosoftClientID)
		return oauthWiring{}, nil
	}

	public := cfg.PublicURL
	if public == "" {
		public = cfg.OIDCRedirectURL
	}
	if public == "" {
		return oauthWiring{}, fmt.Errorf("main: an OAuth provider is configured but there is no public URL to build its "+
			"callback from: set %s (required under dev-auth)", config.EnvPublicURL)
	}
	mgr := oauthmail.NewManager(reg, oauthmail.StoreGrants{Store: st, Keys: keys}, oauthmail.WithLogger(logger))
	conn, err := oauthmail.NewConnector(oauthmail.ConnectorConfig{
		Providers: reg, Store: st, Keys: keys, Manager: mgr, PublicURL: public, Logger: logger,
		AllowPrivateHosts: cfg.AllowPrivateAccountHosts,
		// A Microsoft account's SMTP AUTH is probed at connect and reconnect;
		// 5.7.139 leaves it connected but receive-only (ADR-046 §12).
		CheckSMTP: oauthmail.SMTPAuthCheck(cfg.AllowPrivateAccountHosts),
	})
	if err != nil {
		return oauthWiring{}, fmt.Errorf("main: %s: %w", config.EnvPublicURL, err)
	}
	for _, name := range reg.Enabled() {
		// The redirect URI is not a secret, and it is the one value the
		// operator must copy into the provider's console exactly.
		attrs := []any{"provider", name, "redirect_uri", conn.RedirectURI(name)}
		if name == oauthmail.ProviderMicrosoft {
			attrs = append(attrs, "tenant", cfg.OAuthMicrosoftTenant)
		}
		logger.Info("mail-provider sign-in (OAuth) enabled; register this exact redirect URI with the provider", attrs...)
	}
	if _, ok := reg.Lookup(oauthmail.ProviderGoogle); ok && !oauthmail.GmailSyncReady {
		// Said once at boot so "why does my Gmail not sync?" is answerable
		// from the log (oauthmail/synchold.go, ADR-047).
		logger.Info("Google sign-in is enabled; connected Gmail accounts are held with sync off until "+
			"Mailroom's Gmail sync model lands (they connect, but cannot be switched on yet)",
			"provider", oauthmail.ProviderGoogle, "sync_hold", oauthmail.SyncHoldGmailPending)
	}
	return oauthWiring{tokens: mgr, connector: conn, ops: mgr}, nil
}

// oauthCallbackHandler serves GET /oauth/callback/{provider} behind the
// browser session gate. With no provider configured it still answers — with
// the fixed "not configured" result — so the route exists in every build and
// the gate tests always cover it.
func oauthCallbackHandler(c *oauthmail.Connector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if c == nil {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			http.Redirect(w, r, oauthmail.ResultLocation(oauthmail.ResultNotConfigured, ""), http.StatusSeeOther)
			return
		}
		c.Callback(w, r, userID, ok)
	})
}

// reauthNotifyHandler is the job handler for the one Web Push a grant owes
// when it starts needing its user to reconnect (#52, oauthmail/notify.go). It
// is registered whether or not a provider is configured right now, so a notice
// queued before a configuration change still finds its handler. Without a
// VAPID key there is no push channel at all: the notice is dropped with a
// warning, and the banner in the app is what tells the user.
func reauthNotifyHandler(ctx context.Context, st *store.Store, keys crypto.KeyProvider, logger *slog.Logger) func(context.Context, store.Job) error {
	vk, err := deriveVAPID(ctx, keys)
	if err != nil {
		logger.Warn("oauth reconnect notices will not be pushed: could not derive the VAPID key", "err", err)
		return func(_ context.Context, job store.Job) error {
			logger.Warn("oauth reconnect notice dropped: Web Push is unavailable on this server", "job_id", job.ID.String())
			return nil
		}
	}
	sender := webpush.NewSender(vk, egress.New(egress.Policy{Timeout: 30 * time.Second}), "")
	return oauthmail.ReauthNotifyHandler(st, syncrunner.NewStorePushNotifier(st, sender, logger), logger, nil)
}

// runOAuthFlowSweep starts the periodic expired-flow sweep when the connect
// flow is configured.
func runOAuthFlowSweep(ctx context.Context, st *store.Store, oa oauthWiring) {
	if oa.connector == nil {
		return
	}
	go oa.connector.RunFlowSweep(ctx, oauthFlowSweepInterval, st.ListActiveUserIDs)
}
