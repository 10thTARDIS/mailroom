// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Mail-provider OAuth settings (#52): off by default, the tenant defaults to
// "common", secrets honor _FILE, and a half-configured provider refuses to
// boot rather than silently offering nothing.
func TestLoadOAuth(t *testing.T) {
	base := func(t *testing.T) {
		clearAuthEnv(t)
		for _, e := range []string{EnvOAuthGoogleClientID, EnvOAuthGoogleClientSecret, EnvOAuthMicrosoftClientID,
			EnvOAuthMicrosoftClientSecret, EnvOAuthMicrosoftTenant, EnvPublicURL} {
			t.Setenv(e, "")
			t.Setenv(e+fileSuffix, "")
		}
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}
	t.Run("defaults", func(t *testing.T) {
		base(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OAuthGoogleClientID != "" || cfg.OAuthMicrosoftClientID != "" || cfg.OAuthMicrosoftTenant != "common" || cfg.PublicURL != "" {
			t.Errorf("defaults = %+v", cfg)
		}
	})
	t.Run("configured, secret from a file", func(t *testing.T) {
		base(t)
		f := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(f, []byte("ms-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvOAuthMicrosoftClientID, "ms-id")
		t.Setenv(EnvOAuthMicrosoftClientSecret+fileSuffix, f)
		t.Setenv(EnvOAuthMicrosoftTenant, "consumers")
		t.Setenv(EnvPublicURL, "https://mail.example.com")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OAuthMicrosoftClientSecret != "ms-secret" || cfg.OAuthMicrosoftTenant != "consumers" || cfg.PublicURL != "https://mail.example.com" {
			t.Errorf("cfg = %+v", cfg)
		}
	})
	t.Run("half configured refuses", func(t *testing.T) {
		base(t)
		t.Setenv(EnvOAuthGoogleClientID, "g-id")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "Google OAuth is partially configured") {
			t.Errorf("Load = %v, want the partial-configuration error", err)
		}
	})
}
