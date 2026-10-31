// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"mailroom/internal/config"
)

// No provider configured: empty wiring, and tokens is a NIL INTERFACE (a
// typed-nil *Manager would compare non-nil and make every OAuth account look
// configured). A configured provider with no public URL to build its callback
// from refuses to boot, as does a malformed tenant.
func TestBuildOAuth(t *testing.T) {
	oa, err := buildOAuth(nil, nil, &config.Config{OAuthMicrosoftTenant: "common"}, quietLogger())
	if err != nil || oa.tokens != nil || oa.connector != nil {
		t.Fatalf("unconfigured = %+v, %v; want empty wiring", oa, err)
	}
	_, err = buildOAuth(nil, nil, &config.Config{OAuthGoogleClientID: "id", OAuthGoogleClientSecret: "s",
		OAuthMicrosoftTenant: "common"}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), config.EnvPublicURL) {
		t.Errorf("no public URL = %v, want a startup error naming %s", err, config.EnvPublicURL)
	}
	_, err = buildOAuth(nil, nil, &config.Config{OAuthMicrosoftTenant: "bad/tenant"}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), config.EnvOAuthMicrosoftTenant) {
		t.Errorf("bad tenant = %v", err)
	}
	_, err = buildOAuth(nil, nil, &config.Config{OAuthGoogleClientID: "id", OAuthGoogleClientSecret: "s",
		OAuthMicrosoftTenant: "common", PublicURL: "http://mail.example.com"}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "must be https") {
		t.Errorf("a cleartext public URL = %v, want the https refusal", err)
	}
}
