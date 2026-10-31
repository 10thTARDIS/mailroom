// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"testing"
)

// setEnv sets an env var for the duration of the test (t.Setenv restores it).
// Load also needs the required MAILROOM_DATABASE_URL and an auth path to succeed.
func loadWithRateEnv(t *testing.T, rps, burst string) *Config {
	t.Helper()
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	if rps != "" {
		t.Setenv(EnvRateLimitPerSecond, rps)
	}
	if burst != "" {
		t.Setenv(EnvRateLimitBurst, burst)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestRateLimitDefaults(t *testing.T) {
	cfg := loadWithRateEnv(t, "", "")
	if cfg.RateLimitPerSecond != DefaultRateLimitPerSecond {
		t.Errorf("RateLimitPerSecond = %v, want default %v", cfg.RateLimitPerSecond, DefaultRateLimitPerSecond)
	}
	if cfg.RateLimitBurst != DefaultRateLimitBurst {
		t.Errorf("RateLimitBurst = %v, want default %v", cfg.RateLimitBurst, DefaultRateLimitBurst)
	}
}

func TestRateLimitOverrideAndDisable(t *testing.T) {
	cfg := loadWithRateEnv(t, "5", "12")
	if cfg.RateLimitPerSecond != 5 || cfg.RateLimitBurst != 12 {
		t.Errorf("override = %v/%v, want 5/12", cfg.RateLimitPerSecond, cfg.RateLimitBurst)
	}

	// Zero is the explicit opt-out — accepted, not an error.
	cfg = loadWithRateEnv(t, "0", "0")
	if cfg.RateLimitPerSecond != 0 || cfg.RateLimitBurst != 0 {
		t.Errorf("disable = %v/%v, want 0/0", cfg.RateLimitPerSecond, cfg.RateLimitBurst)
	}
}

func TestRateLimitRejectsNegativeAndGarbage(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	for _, bad := range []struct{ rps, burst string }{
		{"-1", ""},
		{"", "-5"},
		{"abc", ""},
		{"", "notanint"},
	} {
		t.Setenv(EnvRateLimitPerSecond, bad.rps)
		t.Setenv(EnvRateLimitBurst, bad.burst)
		if _, err := Load(); err == nil {
			t.Errorf("Load with rps=%q burst=%q should have failed", bad.rps, bad.burst)
		}
	}
}
