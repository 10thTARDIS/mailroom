// SPDX-License-Identifier: AGPL-3.0-only

// Package pgdsn injects a password read from a Docker-secret file into a
// Postgres connection URL that otherwise carries none (Forgejo #537).
//
// Before #537, every process that needed the database composed its own DSN
// with the password spliced directly into the connection-string text
// (postgres://user:PASSWORD@host/db) and wrote that whole string to a shared
// runtime volume. That put the password in a place a bootstrap script had to
// build with string concatenation (no percent-encoding, so a password
// containing '@', ':' or '/' produced a DSN that parsed wrong), and kept a
// second plaintext copy of every database credential alive on disk for the
// life of the deployment.
//
// This package is the alternative: the connection URL in the environment (or
// its own _FILE) carries NO password, and each binary reads the password
// separately — from the SAME secret file the bootstrap step used to set the
// role's password in the first place — and assembles the two in memory, just
// before connecting. Nothing this package returns is ever written back to
// disk.
package pgdsn

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// WithPasswordFile returns rawURL with the password read from passwordFile
// set as the URL's userinfo password. passwordFile == "" returns rawURL
// unchanged — callers that never configured a password file keep working
// exactly as before (a URL that already carries its own password, or no
// password at all, e.g. peer/trust auth).
//
// rawURL must be in URL form (postgres:// or postgresql://) with a username
// and no password of its own; the file must exist and contain a non-empty
// password after trimming trailing newlines. Any violation is reported as an
// error naming the problem — never the URL or the password itself, so a
// caller that logs the error cannot leak either.
func WithPasswordFile(rawURL, passwordFile string) (string, error) {
	if passwordFile == "" {
		return rawURL, nil
	}

	raw, err := os.ReadFile(passwordFile) //nolint:gosec // the path is operator-supplied config
	if err != nil {
		return "", fmt.Errorf("pgdsn: read password file %s: %w", passwordFile, err)
	}

	// Strip trailing '\n' characters ONLY — never TrimSpace. This mirrors
	// POSIX `$(cat file)`, which is exactly how deploy/compose.yml's
	// role-bootstrap steps read this SAME file to SET the role's password
	// (`OWNER_PW="$(cat /run/secrets/mailroom_owner_password)"`, passed to
	// psql as a `-v` variable). Command substitution strips ALL trailing
	// newlines and nothing else, so matching that exactly — rather than the
	// broader whitespace trim TrimSpace would do — is what keeps the
	// password this process logs in with byte-identical to the password the
	// role was actually given. A trailing space or a bare '\r' left by a
	// CRLF-saved file is part of the password as far as the role is
	// concerned, and must survive here too.
	password := strings.TrimRight(string(raw), "\n")
	if password == "" {
		return "", fmt.Errorf("pgdsn: password file %s is empty", passwordFile)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf(
			"pgdsn: the connection setting could not be parsed as a URL; a password file (%s) "+
				"requires the URL form (postgres://user@host/db?...) — keyword/value DSNs are not supported",
			passwordFile)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
	default:
		return "", fmt.Errorf(
			"pgdsn: a password file (%s) requires the connection setting to be a postgres:// or "+
				"postgresql:// URL — keyword/value DSNs are not supported",
			passwordFile)
	}
	if u.User.Username() == "" {
		return "", fmt.Errorf("pgdsn: the connection URL has no username for the password file (%s) to attach to", passwordFile)
	}
	if _, has := u.User.Password(); has {
		return "", fmt.Errorf(
			"pgdsn: the connection URL already carries a password; set it in exactly one place — "+
				"either the URL or the password file (%s), not both", passwordFile)
	}

	u.User = url.UserPassword(u.User.Username(), password)
	return u.String(), nil
}
