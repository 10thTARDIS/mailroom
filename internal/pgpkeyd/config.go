// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"fmt"
	"os"
	"strings"

	"mailroom/internal/pgdsn"
)

// Environment variables read by the crypto process.
//
// This is a SEPARATE, deliberately tiny configuration surface rather than a
// reuse of internal/config. internal/config knows how to find the master key,
// the database, the mail accounts and the OIDC client secret; the crypto
// process must be unable to reach any of them, and the cleanest way to
// guarantee that is for the binary not to link the code that reads them.
const (
	// EnvSocketPath is where the daemon listens.
	EnvSocketPath = "MAILROOM_PGP_SOCKET"
	// EnvRootKeyFile is the crypto process's OWN root secret — not the
	// application's master key, and never mounted into the application's
	// container (ADR-024).
	EnvRootKeyFile = "MAILROOM_PGP_ROOT_KEY_FILE"
	// EnvDatabaseURL is the mailroom_app DSN, honouring the _FILE indirection
	// the rest of the stack uses. Keys live in app.user_keys under RLS, so the
	// crypto process needs a database — see the note on Config.DatabaseURL for
	// what that costs.
	EnvDatabaseURL = "MAILROOM_DATABASE_URL"
	// EnvDatabasePasswordFile names a file whose trimmed contents are
	// injected into EnvDatabaseURL's userinfo as the password, via
	// internal/pgdsn (#537). The same variable name internal/config defines
	// as config.EnvDatabasePasswordFile — kept as this package's OWN
	// constant, deliberately, for the same reason EnvDatabaseURL above is:
	// this package must not import internal/config (see the package doc).
	EnvDatabasePasswordFile = "MAILROOM_DATABASE_PASSWORD_FILE"
	// EnvLogLevel / EnvLogFormat mirror the server's names so an operator sets
	// one pair of values for the whole stack.
	EnvLogLevel  = "MAILROOM_LOG_LEVEL"
	EnvLogFormat = "MAILROOM_LOG_FORMAT"
)

// Defaults.
const (
	DefaultSocketPath  = "/run/mailroom-crypto/crypto.sock"
	DefaultRootKeyFile = "/run/secrets/mailroom_pgp_root_key" //nolint:gosec // a path, not a credential
	DefaultLogLevel    = "info"
	DefaultLogFormat   = "json"
)

// Config is the crypto process's whole configuration.
type Config struct {
	SocketPath  string
	RootKeyFile string
	// DatabaseURL is the mailroom_app DSN.
	//
	// ⚠ This is a REQUIRED field and there is deliberately no fallback. An
	// unset DSN could plausibly degrade to the in-memory vault, and that would
	// be the worst behaviour available: keys would appear to be created, would
	// vanish on restart, and — because MemoryVault does not filter by owner —
	// would lose the database's half of the cross-user isolation at the same
	// time. All silently, on a process that reports itself healthy. Fail closed.
	//
	// ⚠ It also means the crypto process now depends on PostgreSQL. ADR-024
	// already says signing and decryption fail rather than degrade when this
	// process is down; they now also fail when its database is. Mail delivery
	// and reading are unaffected.
	DatabaseURL string
	LogLevel    string
	LogFormat   string
}

// FromEnv reads the configuration, applying defaults.
//
// It does NOT read the root secret — that happens once, at startup, in the
// binary, so that the bytes have exactly one lifetime and one owner.
func FromEnv() (Config, error) {
	dsn, err := envFileOr(EnvDatabaseURL, "")
	if err != nil {
		return Config{}, err
	}
	// Inject the password from EnvDatabasePasswordFile, if set (#537). Only
	// attempted when a DSN was actually resolved — with none at all, the
	// "DatabaseURL required" check below is the one error that matters.
	if dsn != "" {
		dsn, err = pgdsn.WithPasswordFile(dsn, strings.TrimSpace(os.Getenv(EnvDatabasePasswordFile)))
		if err != nil {
			return Config{}, err
		}
	}
	cfg := Config{
		SocketPath:  envOr(EnvSocketPath, DefaultSocketPath),
		RootKeyFile: envOr(EnvRootKeyFile, DefaultRootKeyFile),
		DatabaseURL: dsn,
		LogLevel:    envOr(EnvLogLevel, DefaultLogLevel),
		LogFormat:   envOr(EnvLogFormat, DefaultLogFormat),
	}
	if cfg.SocketPath == "" {
		return Config{}, fmt.Errorf("%s must not be empty", EnvSocketPath)
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf(
			"%s (or %s_FILE) is required: keys are stored in app.user_keys under RLS, and there is "+
				"no in-memory fallback on purpose — it would lose keys on restart and drop the "+
				"database half of cross-user isolation, silently", EnvDatabaseURL, EnvDatabaseURL)
	}
	if cfg.RootKeyFile == "" {
		return Config{}, fmt.Errorf("%s must not be empty", EnvRootKeyFile)
	}
	switch cfg.LogFormat {
	case "json", "text":
	default:
		return Config{}, fmt.Errorf("%s must be \"json\" or \"text\", got %q", EnvLogFormat, cfg.LogFormat)
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("%s must be debug|info|warn|error, got %q", EnvLogLevel, cfg.LogLevel)
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// envFileOr reads NAME, honouring the NAME_FILE Docker-secret indirection the
// rest of the stack uses. As of #537, deploy/compose.yml no longer composes a
// password-bearing DSN onto a shared volume: EnvDatabaseURL carries no
// password of its own, and FromEnv injects one separately from the file named
// by EnvDatabasePasswordFile (internal/pgdsn.WithPasswordFile), right before
// this Config is built. NAME_FILE still exists for the cases that need it
// (e.g. a URL assembled by another process, or local dev), independent of the
// password-file mechanism.
func envFileOr(name, fallback string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE (%s): %w", name, path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return envOr(name, fallback), nil
}
