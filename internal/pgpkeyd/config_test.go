// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/crypto"
)

const testDSN = "postgres://mailroom_app:pw@127.0.0.1:5432/mailroom?sslmode=disable"

func TestFromEnvDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.SocketPath != DefaultSocketPath {
		t.Errorf("SocketPath = %q, want %q", cfg.SocketPath, DefaultSocketPath)
	}
	if cfg.RootKeyFile != DefaultRootKeyFile {
		t.Errorf("RootKeyFile = %q, want %q", cfg.RootKeyFile, DefaultRootKeyFile)
	}
}

// TestConfigCannotNameTheMasterKey is the configuration half of ADR-024's
// custody decision. The crypto process must not be configurable to read the
// application's master key — if it could, an operator who pointed both secrets
// at the same file would collapse the two roots into one and lose the property
// the whole boundary exists for, silently and with everything still working.
func TestConfigCannotNameTheMasterKey(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	t.Setenv("MAILROOM_MASTER_KEY_FILE", "/run/secrets/mailroom_master_key")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.RootKeyFile != DefaultRootKeyFile {
		t.Errorf("the master-key variable influenced the PGP root: RootKeyFile = %q", cfg.RootKeyFile)
	}
	if strings.Contains(cfg.RootKeyFile, "master") {
		t.Errorf("PGP root defaults to a master-key path: %q", cfg.RootKeyFile)
	}
}

func TestFromEnvRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"log format", EnvLogFormat, "yaml"},
		{"log level", EnvLogLevel, "chatty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvDatabaseURL, testDSN)
			t.Setenv(tc.key, tc.value)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("accepted %s=%q", tc.key, tc.value)
			}
		})
	}
}

// TestRootKeyFileAcceptsTheEscrowEncoding pins the format an operator will
// actually produce, and the one docs/key-escrow.md tells them to escrow: 64 hex
// characters with a trailing newline, as `... > secret` leaves behind.
func TestRootKeyFileAcceptsTheEscrowEncoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pgp_root")
	const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(path, []byte(hexKey+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := crypto.ReadKeyFile(path, "PGP root key")
	if err != nil {
		t.Fatalf("ReadKeyFile: %v", err)
	}
	if len(got) != crypto.KeySize {
		t.Fatalf("key length = %d, want %d", len(got), crypto.KeySize)
	}

	// A malformed secret must fail loudly at startup rather than yield a
	// short key nobody notices until the mail is unreadable.
	bad := filepath.Join(dir, "bad_root")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = crypto.ReadKeyFile(bad, "PGP root key")
	if err == nil {
		t.Fatal("a malformed root key was accepted")
	}
	if !strings.Contains(err.Error(), "PGP root key") {
		t.Errorf("the error does not say WHICH secret is malformed — there are two now: %v", err)
	}
}

// TestDatabaseURLIsRequired pins the fail-closed rule, which is the kind of
// thing a later "make local dev easier" change removes without noticing what it
// costs.
//
// There is no in-memory fallback ON PURPOSE. A crypto process that quietly ran
// without a database would report itself healthy, create keys that vanish on
// restart, and — because MemoryVault does not filter by owner — simultaneously
// drop the database half of cross-user isolation. Every one of those failures
// is silent. Starting is the wrong behaviour.
func TestDatabaseURLIsRequired(t *testing.T) {
	clearEnv(t)

	_, err := FromEnv()
	if err == nil {
		t.Fatal("the crypto process configured itself with no database")
	}
	if !strings.Contains(err.Error(), EnvDatabaseURL) {
		t.Errorf("the error does not name the missing variable: %v", err)
	}
}

// TestDatabaseURLFileIndirection covers how the DSN actually arrives in the
// deployment: compose cannot assemble a secret at parse time, so the bootstrap
// steps write the composed DSN to a file on a shared volume.
func TestDatabaseURLFileIndirection(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "app_dsn")
	if err := os.WriteFile(path, []byte(testDSN+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(EnvDatabaseURL+"_FILE", path)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.DatabaseURL != testDSN {
		t.Errorf("DatabaseURL = %q, want %q (trailing newline must be trimmed)", cfg.DatabaseURL, testDSN)
	}
}

// TestFromEnvDatabasePasswordFile pins the #537 path: a DSN with no password,
// plus MAILROOM_DATABASE_PASSWORD_FILE, ends up with the file's password
// injected into Config.DatabaseURL.
func TestFromEnvDatabasePasswordFile(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@127.0.0.1:5432/mailroom?sslmode=disable")

	pwPath := filepath.Join(t.TempDir(), "app_password")
	const want = "p@ss/wo:rd%'# x"
	if err := os.WriteFile(pwPath, []byte(want+"\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	t.Setenv(EnvDatabasePasswordFile, pwPath)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !strings.Contains(cfg.DatabaseURL, "mailroom_app:") {
		t.Fatalf("DatabaseURL = %q, does not look like it carries a password", cfg.DatabaseURL)
	}
	if strings.Contains(cfg.DatabaseURL, want) {
		// Not itself wrong (percent-encoding may legitimately reorder bytes),
		// but this DSN's special characters should have been percent-encoded
		// rather than left raw, so a literal, unencoded match here would be
		// surprising for THIS specific password.
		t.Errorf("DatabaseURL embeds the raw, unencoded password: %q", cfg.DatabaseURL)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvSocketPath, EnvRootKeyFile, EnvDatabaseURL, EnvDatabaseURL + "_FILE",
		EnvDatabasePasswordFile, EnvLogLevel, EnvLogFormat, "MAILROOM_MASTER_KEY_FILE",
	} {
		t.Setenv(k, "")
	}
}
