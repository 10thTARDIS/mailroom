// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/config"
	"mailroom/internal/oauthserver"
	"mailroom/internal/store"
)

// gateOAuthServer builds the OAuth server the gate tests mount: on when the
// config has a usable public origin, nil (every route 404s) otherwise. Its
// store is never reached: every probe is anonymous, and the handlers that need
// the database are behind the session gate.
func gateOAuthServer(t *testing.T, cfg *config.Config) *oauthserver.Server {
	t.Helper()
	issuer := cfg.OAuthIssuer()
	if issuer == "" {
		return nil
	}
	s, err := oauthserver.New(oauthserver.Config{Issuer: issuer, Store: new(store.Store), Policy: api.OAuthPolicy(),
		Approve: api.ApproveOAuthGrant})
	if err != nil {
		t.Fatalf("oauthserver.New: %v", err)
	}
	return s
}
