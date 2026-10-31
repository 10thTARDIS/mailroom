// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clearAuthEnv wipes every auth-related variable so a stray value in the host
// environment can't perturb the either/or auth validation under test.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{
		EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecret, EnvOIDCClientSecret + fileSuffix,
		EnvOIDCRedirectURL, EnvSessionKey, EnvSessionKey + fileSuffix,
		EnvOperatorSubject, EnvOperatorEmail, EnvDevAuthSubject, EnvDevAuthEmail, EnvDevAuthAck,
	} {
		t.Setenv(e, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SyncInterval != DefaultSyncInterval {
		t.Errorf("SyncInterval = %v, want %v", cfg.SyncInterval, DefaultSyncInterval)
	}
	if cfg.SyncMaxBackoff != DefaultSyncMaxBackoff {
		t.Errorf("SyncMaxBackoff = %v, want %v", cfg.SyncMaxBackoff, DefaultSyncMaxBackoff)
	}
	if !cfg.IMAPIdle {
		t.Errorf("IMAPIdle = %v, want true (default on)", cfg.IMAPIdle)
	}
	if cfg.IMAPIdleTimeout != DefaultIMAPIdleTimeout {
		t.Errorf("IMAPIdleTimeout = %v, want %v", cfg.IMAPIdleTimeout, DefaultIMAPIdleTimeout)
	}
	if !cfg.IMAPPool {
		t.Errorf("IMAPPool = %v, want true (default on)", cfg.IMAPPool)
	}
	if cfg.IMAPPoolIdleTimeout != DefaultIMAPPoolIdleTimeout {
		t.Errorf("IMAPPoolIdleTimeout = %v, want %v", cfg.IMAPPoolIdleTimeout, DefaultIMAPPoolIdleTimeout)
	}
	if cfg.ReapInterval != DefaultReapInterval {
		t.Errorf("ReapInterval = %v, want %v", cfg.ReapInterval, DefaultReapInterval)
	}
	if cfg.TrashRetention != DefaultTrashRetention {
		t.Errorf("TrashRetention = %v, want %v", cfg.TrashRetention, DefaultTrashRetention)
	}
	if cfg.PurgeInterval != DefaultPurgeInterval {
		t.Errorf("PurgeInterval = %v, want %v", cfg.PurgeInterval, DefaultPurgeInterval)
	}
	if cfg.ChangeLogRetention != DefaultChangeLogRetention {
		t.Errorf("ChangeLogRetention = %v, want %v", cfg.ChangeLogRetention, DefaultChangeLogRetention)
	}
	if cfg.BlobRoot != DefaultBlobRoot {
		t.Errorf("BlobRoot = %q, want %q", cfg.BlobRoot, DefaultBlobRoot)
	}
	if !cfg.DevAuthConfigured() {
		t.Error("DevAuthConfigured() = false, want true")
	}
	if cfg.OIDCConfigured() {
		t.Error("OIDCConfigured() = true, want false")
	}
	if cfg.DatabaseURL != "postgres://mailroom_app@localhost/mailroom" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.MasterKeyFile != DefaultMasterKeyFile {
		t.Errorf("MasterKeyFile = %q, want %q", cfg.MasterKeyFile, DefaultMasterKeyFile)
	}
	if cfg.LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, DefaultLogLevel)
	}
	if cfg.LogFormat != DefaultLogFormat {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, DefaultLogFormat)
	}
}

func TestLoadIMAPIdle(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}

	t.Run("explicit off", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPIdle, "0")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.IMAPIdle {
			t.Error("IMAPIdle = true, want false (MAILROOM_IMAP_IDLE=0)")
		}
	})

	t.Run("explicit on", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPIdle, "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !cfg.IMAPIdle {
			t.Error("IMAPIdle = false, want true (MAILROOM_IMAP_IDLE=true)")
		}
	})

	t.Run("custom timeout", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPIdleTimeout, "10m")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.IMAPIdleTimeout != 10*time.Minute {
			t.Errorf("IMAPIdleTimeout = %v, want 10m", cfg.IMAPIdleTimeout)
		}
	})

	t.Run("non-positive timeout rejected", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPIdleTimeout, "0s")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvIMAPIdleTimeout) {
			t.Fatalf("Load() error = %v, want an error mentioning %s", err, EnvIMAPIdleTimeout)
		}
	})

	t.Run("unparseable timeout rejected", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPIdleTimeout, "later")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvIMAPIdleTimeout) {
			t.Fatalf("Load() error = %v, want an error mentioning %s", err, EnvIMAPIdleTimeout)
		}
	})
}

func TestLoadIMAPPool(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}

	t.Run("explicit off", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPPool, "0")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.IMAPPool {
			t.Error("IMAPPool = true, want false (MAILROOM_IMAP_POOL=0)")
		}
	})

	t.Run("explicit on", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPPool, "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !cfg.IMAPPool {
			t.Error("IMAPPool = false, want true (MAILROOM_IMAP_POOL=true)")
		}
	})

	t.Run("custom idle timeout", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPPoolIdleTimeout, "90s")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.IMAPPoolIdleTimeout != 90*time.Second {
			t.Errorf("IMAPPoolIdleTimeout = %v, want 90s", cfg.IMAPPoolIdleTimeout)
		}
	})

	t.Run("non-positive idle timeout rejected", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPPoolIdleTimeout, "0s")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvIMAPPoolIdleTimeout) {
			t.Fatalf("Load() error = %v, want an error mentioning %s", err, EnvIMAPPoolIdleTimeout)
		}
	})

	t.Run("unparseable idle timeout rejected", func(t *testing.T) {
		base(t)
		t.Setenv(EnvIMAPPoolIdleTimeout, "soon")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvIMAPPoolIdleTimeout) {
			t.Fatalf("Load() error = %v, want an error mentioning %s", err, EnvIMAPPoolIdleTimeout)
		}
	})
}

func TestLoadMissingRequired(t *testing.T) {
	// Ensure a clean slate: no MAILROOM_DATABASE_URL(_FILE) from the host env.
	t.Setenv(EnvDatabaseURL, "")
	t.Setenv(EnvDatabaseURL+fileSuffix, "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error for missing database URL")
	}
	if !strings.Contains(err.Error(), EnvDatabaseURL) {
		t.Errorf("error %q does not mention %s", err.Error(), EnvDatabaseURL)
	}
}

func TestLoadDatabaseURLFromFile(t *testing.T) {
	// An auth path is mandatory for a Config to load at all (validateAuth), so
	// select the cheapest one; this test is about the _FILE indirection.
	clearAuthEnv(t)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	dir := t.TempDir()
	secretPath := filepath.Join(dir, "database_url")
	const want = "postgres://mailroom_app@db/mailroom?sslmode=verify-full"
	if err := os.WriteFile(secretPath, []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	// A plaintext value is present too; the _FILE form must win.
	t.Setenv(EnvDatabaseURL, "postgres://should-not-be-used")
	t.Setenv(EnvDatabaseURL+fileSuffix, secretPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DatabaseURL != want {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, want)
	}
}

func TestLoadDatabaseURLFileMissing(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "")
	t.Setenv(EnvDatabaseURL+fileSuffix, filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error for unreadable secret file")
	}
}

// TestLoadDatabaseURLWithPasswordFile pins the #537 path: a plain URL with no
// password, plus MAILROOM_DATABASE_PASSWORD_FILE, ends up with the file's
// password injected into DatabaseURL.
func TestLoadDatabaseURLWithPasswordFile(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	pwPath := filepath.Join(t.TempDir(), "app_password")
	if err := os.WriteFile(pwPath, []byte("p@ss/wo:rd%'# x\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom?sslmode=disable")
	t.Setenv(EnvDatabasePasswordFile, pwPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	u, perr := url.Parse(cfg.DatabaseURL)
	if perr != nil {
		t.Fatalf("url.Parse(%q): %v", cfg.DatabaseURL, perr)
	}
	pw, ok := u.User.Password()
	if !ok || pw != "p@ss/wo:rd%'# x" {
		t.Errorf("password = %q, ok = %v, want %q, true", pw, ok, "p@ss/wo:rd%'# x")
	}
	if u.User.Username() != "mailroom_app" {
		t.Errorf("username = %q, want mailroom_app", u.User.Username())
	}
}

// TestLoadDatabaseURLFileWithPasswordFile checks the same injection works when
// the URL itself also arrives via its own _FILE indirection (both are
// legitimately separate secrets in the deployment: one composes the URL,
// the other supplies the role's password).
func TestLoadDatabaseURLFileWithPasswordFile(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	urlPath := filepath.Join(t.TempDir(), "database_url")
	if err := os.WriteFile(urlPath, []byte("postgres://mailroom_app@db/mailroom?sslmode=disable\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	pwPath := filepath.Join(t.TempDir(), "app_password")
	if err := os.WriteFile(pwPath, []byte("s3cret\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	t.Setenv(EnvDatabaseURL, "")
	t.Setenv(EnvDatabaseURL+fileSuffix, urlPath)
	t.Setenv(EnvDatabasePasswordFile, pwPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	u, perr := url.Parse(cfg.DatabaseURL)
	if perr != nil {
		t.Fatalf("url.Parse(%q): %v", cfg.DatabaseURL, perr)
	}
	pw, ok := u.User.Password()
	if !ok || pw != "s3cret" {
		t.Errorf("password = %q, ok = %v, want \"s3cret\", true", pw, ok)
	}
}

// TestLoadDatabasePasswordFileRejectsExistingPassword confirms Load surfaces
// pgdsn's "set it in exactly one place" error rather than silently ignoring
// the password file or silently overwriting the URL's own password.
func TestLoadDatabasePasswordFileRejectsExistingPassword(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	pwPath := filepath.Join(t.TempDir(), "app_password")
	if err := os.WriteFile(pwPath, []byte("s3cret\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app:already-set@localhost/mailroom")
	t.Setenv(EnvDatabasePasswordFile, pwPath)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error for a URL that already carries a password")
	}
	if !strings.Contains(err.Error(), "one place") {
		t.Errorf("error %q does not explain the one-place rule", err.Error())
	}
}

func TestLoadInvalidLogFormat(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
	t.Setenv(EnvLogFormat, "yaml")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error for invalid log format")
	}
	if !strings.Contains(err.Error(), EnvLogFormat) {
		t.Errorf("error %q does not mention %s", err.Error(), EnvLogFormat)
	}
}

func TestLoadInvalidLogLevel(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
	t.Setenv(EnvLogLevel, "verbose")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error for invalid log level")
	}
	if !strings.Contains(err.Error(), EnvLogLevel) {
		t.Errorf("error %q does not mention %s", err.Error(), EnvLogLevel)
	}
}

func TestLoadOverridesAndTrimming(t *testing.T) {
	// As above: some auth path must be selected for Load to succeed.
	clearAuthEnv(t)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	t.Setenv(EnvDatabaseURL, "  postgres://mailroom_app@localhost/mailroom  ")
	t.Setenv(EnvListenAddr, " :9090 ")
	t.Setenv(EnvLogLevel, " DEBUG ")
	t.Setenv(EnvLogFormat, " TEXT ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DatabaseURL != "postgres://mailroom_app@localhost/mailroom" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q", cfg.LogFormat)
	}
}

// The either/or auth contract (validateAuth), which is the rule that stops a
// TYPO from opening the instance: a partially configured OIDC block must be a
// hard config error, and must NEVER fall through to the wide-open dev-auth
// escape hatch — not even when MAILROOM_DEV_AUTH_SUBJECT is sitting right there,
// which is exactly the situation an operator midway through switching to OIDC is
// in. The gate (cmd/mailroom) can only be as strong as this function.
func TestLoadAuthPathIsEitherOr(t *testing.T) {
	const (
		issuer   = "https://auth.example.com/application/o/mailroom/"
		clientID = "mailroom"
		secret   = "client-secret"
		redirect = "https://mail.example.com/auth/callback"
	)
	longKey := strings.Repeat("k", MinSessionKeyLen)

	// full is the complete OIDC block; each case removes or corrupts one part.
	full := map[string]string{
		EnvOIDCIssuer:       issuer,
		EnvOIDCClientID:     clientID,
		EnvOIDCClientSecret: secret,
		EnvOIDCRedirectURL:  redirect,
		EnvSessionKey:       longKey,
	}

	tests := []struct {
		name string
		// env overrides applied on top of full ("" removes the variable).
		env map[string]string
		// devAuth also sets the escape hatch — the tempting fall-through.
		devAuth  bool
		wantErr  string // "" means Load must succeed
		wantOIDC bool
	}{
		{name: "complete OIDC block", wantOIDC: true},
		{
			name:    "complete OIDC block wins over a dev-auth subject",
			devAuth: true, wantOIDC: true,
		},
		{
			name: "missing issuer is an error, not a dev-auth fallback",
			env:  map[string]string{EnvOIDCIssuer: ""}, devAuth: true,
			wantErr: EnvOIDCIssuer,
		},
		{
			name: "missing client id is an error, not a dev-auth fallback",
			env:  map[string]string{EnvOIDCClientID: ""}, devAuth: true,
			wantErr: EnvOIDCClientID,
		},
		{
			name: "missing client secret is an error, not a dev-auth fallback",
			env:  map[string]string{EnvOIDCClientSecret: ""}, devAuth: true,
			wantErr: EnvOIDCClientSecret,
		},
		{
			name: "missing redirect url is an error, not a dev-auth fallback",
			env:  map[string]string{EnvOIDCRedirectURL: ""}, devAuth: true,
			wantErr: EnvOIDCRedirectURL,
		},
		{
			name: "missing session key is an error, not a dev-auth fallback",
			env:  map[string]string{EnvSessionKey: ""}, devAuth: true,
			wantErr: EnvSessionKey,
		},
		{
			// The subtlest typo of the lot: every variable is present, but the
			// signing key is too short to be worth anything. Silently accepting
			// it would leave session cookies forgeable.
			name: "a too-short session key is an error, not a dev-auth fallback",
			env:  map[string]string{EnvSessionKey: "short"}, devAuth: true,
			wantErr: EnvSessionKey,
		},
		{
			name: "no OIDC and no dev-auth: the server must not come up",
			env: map[string]string{
				EnvOIDCIssuer: "", EnvOIDCClientID: "", EnvOIDCClientSecret: "",
				EnvOIDCRedirectURL: "", EnvSessionKey: "",
			},
			wantErr: "no auth path configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
			for k, v := range full {
				t.Setenv(k, v)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if tt.devAuth {
				t.Setenv(EnvDevAuthSubject, "dev-operator")
				t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
				t.Setenv(EnvDevAuthEmail, "dev@example.com")
			}

			cfg, err := Load()

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() = nil error, want a config error naming %s — "+
						"a broken OIDC block MUST NOT fall through to open access", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to name %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.OIDCConfigured() != tt.wantOIDC {
				t.Errorf("OIDCConfigured() = %v, want %v", cfg.OIDCConfigured(), tt.wantOIDC)
			}
			if tt.wantOIDC && cfg.DevAuthConfigured() {
				t.Error("DevAuthConfigured() = true alongside a complete OIDC block — " +
					"OIDC must win outright, or a stale dev-auth variable would disable auth")
			}
		})
	}
}

// The expunge reaper's cadence (#86). Zero is a legal, meaningful value here —
// the operator's explicit "never reap" switch — so it gets its own coverage:
// a knob whose OFF state leaves deleted mail visible forever must not be
// reachable by a typo.
func TestLoadReapInterval(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}

	t.Run("explicit", func(t *testing.T) {
		base(t)
		t.Setenv(EnvReapInterval, " 15m ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.ReapInterval != 15*time.Minute {
			t.Errorf("ReapInterval = %v, want 15m", cfg.ReapInterval)
		}
	})

	t.Run("zero disables", func(t *testing.T) {
		base(t)
		t.Setenv(EnvReapInterval, "0")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.ReapInterval != 0 {
			t.Errorf("ReapInterval = %v, want 0 (reaper disabled)", cfg.ReapInterval)
		}
	})

	for _, bad := range []string{"-1h", "hourly", "60"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			base(t)
			t.Setenv(EnvReapInterval, bad)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=%q", EnvReapInterval, bad)
			} else if !strings.Contains(err.Error(), EnvReapInterval) {
				t.Errorf("error %q does not name %s", err, EnvReapInterval)
			}
		})
	}
}

// The per-account sync-backoff cap (mailroom#103). It must be a positive
// duration — a non-positive value has no meaning as a ceiling — so a bad value is
// a hard config error rather than a silent default.
func TestLoadSyncMaxBackoff(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}

	t.Run("explicit", func(t *testing.T) {
		base(t)
		t.Setenv(EnvSyncMaxBackoff, " 45m ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.SyncMaxBackoff != 45*time.Minute {
			t.Errorf("SyncMaxBackoff = %v, want 45m", cfg.SyncMaxBackoff)
		}
	})

	for _, bad := range []string{"0", "-5m", "soon"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			base(t)
			t.Setenv(EnvSyncMaxBackoff, bad)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=%q", EnvSyncMaxBackoff, bad)
			} else if !strings.Contains(err.Error(), EnvSyncMaxBackoff) {
				t.Errorf("error %q does not name %s", err, EnvSyncMaxBackoff)
			}
		})
	}
}

// TestLoadImageProxyPrefetch: the arrival prefetch (#922) is on by default
// with a 7-day window, can be turned off, and refuses a window it cannot read
// rather than guessing — a zero or negative window would read as "prefetch
// nothing" or "prefetch everything", and neither is what anyone typed.
func TestLoadImageProxyPrefetch(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
		t.Setenv(EnvImageProxyPrefetch, "")
		t.Setenv(EnvImageProxyPrefetchMaxAge, "")
		t.Setenv(EnvImageProxyPrefetchMaxBytes, "")
	}

	t.Run("defaults", func(t *testing.T) {
		base(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !cfg.ImageProxyPrefetch || cfg.ImageProxyPrefetchMaxAge != 7*24*time.Hour {
			t.Errorf("prefetch = %v, max age = %v; want on, 168h", cfg.ImageProxyPrefetch, cfg.ImageProxyPrefetchMaxAge)
		}
	})
	t.Run("explicit", func(t *testing.T) {
		base(t)
		t.Setenv(EnvImageProxyPrefetch, "off")
		t.Setenv(EnvImageProxyPrefetchMaxAge, " 48h ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.ImageProxyPrefetch || cfg.ImageProxyPrefetchMaxAge != 48*time.Hour {
			t.Errorf("prefetch = %v, max age = %v; want off, 48h", cfg.ImageProxyPrefetch, cfg.ImageProxyPrefetchMaxAge)
		}
	})
	t.Run("referenced bytes ceiling", func(t *testing.T) {
		for raw, want := range map[string]int64{"": 0, "0": 0, " 1073741824 ": 1 << 30, "-1": -1} {
			base(t)
			t.Setenv(EnvImageProxyPrefetchMaxBytes, raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() with %q: %v", raw, err)
			}
			if cfg.ImageProxyPrefetchMaxBytes != want {
				t.Errorf("%s=%q read as %d, want %d", EnvImageProxyPrefetchMaxBytes, raw, cfg.ImageProxyPrefetchMaxBytes, want)
			}
		}
		base(t)
		t.Setenv(EnvImageProxyPrefetchMaxBytes, "16GiB")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvImageProxyPrefetchMaxBytes) {
			t.Errorf("Load() accepted %s=16GiB (err %v); want a refusal naming it", EnvImageProxyPrefetchMaxBytes, err)
		}
	})
	for _, bad := range []string{"0", "-1h", "a week"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			base(t)
			t.Setenv(EnvImageProxyPrefetchMaxAge, bad)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=%q", EnvImageProxyPrefetchMaxAge, bad)
			} else if !strings.Contains(err.Error(), EnvImageProxyPrefetchMaxAge) {
				t.Errorf("error %q does not name %s", err, EnvImageProxyPrefetchMaxAge)
			}
		})
	}
}

func TestLoadTrashRetentionAndPurgeInterval(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}

	t.Run("explicit", func(t *testing.T) {
		base(t)
		t.Setenv(EnvTrashRetention, " 168h ")
		t.Setenv(EnvPurgeInterval, " 30m ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.TrashRetention != 168*time.Hour {
			t.Errorf("TrashRetention = %v, want 168h", cfg.TrashRetention)
		}
		if cfg.PurgeInterval != 30*time.Minute {
			t.Errorf("PurgeInterval = %v, want 30m", cfg.PurgeInterval)
		}
	})

	// Neither has a meaningful "off": unlike the reaper, a non-positive value is a
	// hazard (purge-on-trash, or a sweep that never runs), so it is rejected.
	for _, tc := range []struct {
		env, val string
	}{
		{EnvTrashRetention, "0"},
		{EnvTrashRetention, "-720h"},
		{EnvTrashRetention, "forever"},
		{EnvPurgeInterval, "0"},
		{EnvPurgeInterval, "-1h"},
		{EnvPurgeInterval, "often"},
	} {
		t.Run("rejects "+tc.env+"="+tc.val, func(t *testing.T) {
			base(t)
			t.Setenv(tc.env, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.env, tc.val)
			} else if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

// TestLoadChangeLogRetention pins the 7-day floor (mailroom#526): unlike
// TrashRetention/PurgeInterval, "positive" is not enough here — anything
// short of MinChangeLogRetention would force every client (not just one
// genuinely offline for a while) through cannotCalculateChanges on a
// recurring basis, so it is refused outright.
func TestLoadChangeLogRetention(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}

	t.Run("valid override parses", func(t *testing.T) {
		base(t)
		t.Setenv(EnvChangeLogRetention, " 2160h ") // 90 days, spelled explicitly
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.ChangeLogRetention != 2160*time.Hour {
			t.Errorf("ChangeLogRetention = %v, want 2160h", cfg.ChangeLogRetention)
		}
	})

	t.Run("exactly the minimum is accepted", func(t *testing.T) {
		base(t)
		t.Setenv(EnvChangeLogRetention, MinChangeLogRetention.String())
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.ChangeLogRetention != MinChangeLogRetention {
			t.Errorf("ChangeLogRetention = %v, want %v", cfg.ChangeLogRetention, MinChangeLogRetention)
		}
	})

	for _, tc := range []struct{ env, val string }{
		{EnvChangeLogRetention, "0"},
		{EnvChangeLogRetention, "-2160h"},
		{EnvChangeLogRetention, "forever"},
		{EnvChangeLogRetention, "24h"},        // positive, but below the 7-day floor
		{EnvChangeLogRetention, "167h59m59s"}, // one second short of 7 days
	} {
		t.Run("rejects "+tc.env+"="+tc.val, func(t *testing.T) {
			base(t)
			t.Setenv(tc.env, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.env, tc.val)
			} else if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

// TestLoadRejectsUnreadableBoolToggle covers #556: a misspelled boolean used to
// boot with the toggle silently off, which for MAILROOM_WRITEBACK_DISABLED
// meant an instance-wide safety lock that was not there. Measured on the code
// before the fix: `enabled`, `disabled`, `ture` and `y` each booted with
// WritebackDisabled=false.
//
// ⚠ THE ACCEPTED-SPELLINGS SUBTEST IS WHAT KEEPS THIS HONEST. A validator that
// rejected everything would satisfy every rejection case below and break every
// real deployment — including the ordinary one, because deploy/compose.yml
// writes each of these as `${VAR:-}` and so passes an EMPTY string for a
// toggle nobody set.
func TestLoadRejectsUnreadableBoolToggle(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
		// Hermetic: a toggle exported in the host environment must not decide
		// another toggle's case.
		for _, n := range boolToggles {
			t.Setenv(n, "")
		}
	}

	// The reproduction the issue asked for, kept as its own case because it is
	// the one seen on a real mailbox: `enabled` reads as an intent to
	// disable writeback, and used to enable it.
	t.Run("the writeback kill switch refuses a value it cannot read", func(t *testing.T) {
		base(t)
		t.Setenv(EnvWritebackDisabled, "enabled")
		cfg, err := Load()
		if err == nil {
			t.Fatalf("Load() succeeded with %s=enabled and WritebackDisabled=%v; want a refusal, "+
				"because a lock that is silently off is worse than no lock at all",
				EnvWritebackDisabled, cfg.WritebackDisabled)
		}
		if !strings.Contains(err.Error(), EnvWritebackDisabled) {
			t.Errorf("Load() error = %v, want it to name %s", err, EnvWritebackDisabled)
		}
	})

	for _, name := range boolToggles {
		for _, raw := range []string{"ture", "enabled", "disabled", "y", "n", "2", "off ten"} {
			t.Run("rejected/"+name+"="+raw, func(t *testing.T) {
				base(t)
				t.Setenv(name, raw)
				_, err := Load()
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("Load() error = %v, want an error naming %s", err, name)
				}
			})
		}
	}

	for _, name := range boolToggles {
		for _, raw := range []string{
			"", "  ", // compose.yml's ${VAR:-} for a toggle nobody set
			"1", "true", "yes", "on", "TRUE", "On", " yes ",
			"0", "false", "no", "off", "FALSE", "Off", " no ",
		} {
			t.Run("accepted/"+name+"="+strconv.Quote(raw), func(t *testing.T) {
				base(t)
				t.Setenv(name, raw)
				if _, err := Load(); err != nil {
					t.Fatalf("Load() error = %v, want nil for %s=%q", err, name, raw)
				}
			})
		}
	}
}

// #766: a malformed authserv-id bound is a startup error, never a silent drop.
func TestValidateAuthservIDs(t *testing.T) {
	good := []string{"", "mx.provider.example", "mx.provider.example, old.provider.example<2026-07-01", " , ,"}
	for _, v := range good {
		t.Setenv(EnvAuthservIDs, v)
		if err := ValidateAuthservIDs(); err != nil {
			t.Errorf("%q: unexpected error %v", v, err)
		}
	}
	bad := []string{
		"old.provider.example<2026-13-45",
		"old.provider.example<",
		"old.provider.example<July",
		"<2026-07-01",
		"old.provider.example<2026-07-01<2026-08-01",
		"mx.provider.example,old.provider.example<nope",
	}
	for _, v := range bad {
		t.Setenv(EnvAuthservIDs, v)
		err := ValidateAuthservIDs()
		if err == nil {
			t.Errorf("%q: want an error, got none", v)
			continue
		}
		if !strings.Contains(err.Error(), EnvAuthservIDs) {
			t.Errorf("%q: the error does not name the variable: %v", v, err)
		}
	}
}

// Load must refuse to boot on one (the server's startup path).
func TestLoadRefusesAMalformedAuthservBound(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	// CONTROL: the same environment with a well-formed bound boots, so the
	// failure below is the bound and nothing else.
	t.Setenv(EnvAuthservIDs, "old.provider.example<2026-07-01")
	if _, err := Load(); err != nil {
		t.Fatalf("CONTROL FAILED: a well-formed bound does not load: %v", err)
	}
	t.Setenv(EnvAuthservIDs, "old.provider.example<not-a-date")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvAuthservIDs) {
		t.Fatalf("Load = %v, want an error naming %s", err, EnvAuthservIDs)
	}
}
