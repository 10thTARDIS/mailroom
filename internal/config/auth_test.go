// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The either/or auth contract (validateAuth): the server must come up with
// EXACTLY one usable way to authenticate, never zero, and a half-configured OIDC
// block must be a loud error rather than a silent fall-through to the wide-open
// dev-auth hatch. These tests pin every edge of that decision.

const (
	testIssuer   = "https://auth.example.com/application/o/mailroom/"
	testClientID = "mailroom"
	testSecret   = "client-secret"
	testRedirect = "https://mail.example.com/auth/callback"
	testDSN      = "postgres://mailroom_app@localhost/mailroom"
)

// testSessionKey is exactly MinSessionKeyLen bytes.
var testSessionKey = strings.Repeat("k", MinSessionKeyLen)

// setOIDC sets the full OIDC block.
func setOIDC(t *testing.T) {
	t.Helper()
	t.Setenv(EnvOIDCIssuer, testIssuer)
	t.Setenv(EnvOIDCClientID, testClientID)
	t.Setenv(EnvOIDCClientSecret, testSecret)
	t.Setenv(EnvOIDCRedirectURL, testRedirect)
	t.Setenv(EnvSessionKey, testSessionKey)
}

func TestAuthOIDCPathSelected(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	setOIDC(t)
	// Dev-auth is set too: OIDC must win, and dev-auth must NOT be honored.
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.OIDCConfigured() {
		t.Error("OIDCConfigured() = false, want true")
	}
	if cfg.DevAuthConfigured() {
		t.Error("DevAuthConfigured() = true — dev-auth must never be honored alongside OIDC")
	}
	if string(cfg.SessionKey) != testSessionKey {
		t.Errorf("SessionKey = %q, want the configured key", cfg.SessionKey)
	}
}

func TestAuthNoPathIsRejected(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error: no auth path configured")
	}
	if !strings.Contains(err.Error(), "no auth path configured") {
		t.Errorf("error = %q, want it to say no auth path is configured", err)
	}
}

func TestAuthPartialOIDCIsRejectedNotFallback(t *testing.T) {
	// The dangerous case: a typo'd/omitted OIDC field must never degrade into
	// the unauthenticated dev-auth path just because a dev-auth subject is set.
	for _, missing := range []string{EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecret, EnvOIDCRedirectURL} {
		t.Run(missing, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvDatabaseURL, testDSN)
			setOIDC(t)
			t.Setenv(missing, "")
			t.Setenv(EnvDevAuthSubject, "dev-operator")
			t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want an error: OIDC partially configured (%s missing)", missing)
			}
			if !strings.Contains(err.Error(), "partially configured") {
				t.Errorf("error = %q, want it to report a partially configured OIDC block", err)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error = %q, want it to name the missing variable %s", err, missing)
			}
		})
	}
}

func TestAuthShortSessionKeyIsRejected(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	setOIDC(t)
	t.Setenv(EnvSessionKey, "too-short")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error: session key below MinSessionKeyLen")
	}
	if !strings.Contains(err.Error(), EnvSessionKey) {
		t.Errorf("error = %q, want it to name %s", err, EnvSessionKey)
	}
}

func TestAuthSessionKeyOnlyIsRejected(t *testing.T) {
	// Only the session key set: OIDC is "partially configured" (a key alone is
	// not an auth path), so this must fail rather than silently do nothing.
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	t.Setenv(EnvSessionKey, testSessionKey)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error: session key without the OIDC block")
	}
	if !strings.Contains(err.Error(), "partially configured") {
		t.Errorf("error = %q, want it to report a partially configured OIDC block", err)
	}
}

func TestAuthDevAuthPathSelected(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	t.Setenv(EnvDevAuthEmail, "operator@example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.DevAuthConfigured() {
		t.Fatal("DevAuthConfigured() = false, want true")
	}
	if cfg.DevAuthSubject != "dev-operator" || cfg.DevAuthEmail != "operator@example.com" {
		t.Errorf("dev-auth identity = %q/%q, want dev-operator/operator@example.com",
			cfg.DevAuthSubject, cfg.DevAuthEmail)
	}
}

func TestAuthSecretsHonorFileIndirection(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)

	dir := t.TempDir()
	secretFile := writeFile(t, dir, "client_secret", testSecret+"\n")
	keyFile := writeFile(t, dir, "session_key", testSessionKey+"\n")

	t.Setenv(EnvOIDCIssuer, testIssuer)
	t.Setenv(EnvOIDCClientID, testClientID)
	t.Setenv(EnvOIDCRedirectURL, testRedirect)
	t.Setenv(EnvOIDCClientSecret+fileSuffix, secretFile)
	t.Setenv(EnvSessionKey+fileSuffix, keyFile)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.OIDCConfigured() {
		t.Fatal("OIDCConfigured() = false, want true (secrets came from _FILE)")
	}
	if cfg.OIDCClientSecret != testSecret {
		t.Errorf("OIDCClientSecret = %q, want the file's contents", cfg.OIDCClientSecret)
	}
	if string(cfg.SessionKey) != testSessionKey {
		t.Errorf("SessionKey = %q, want the file's contents", cfg.SessionKey)
	}
}

// ⚠ THE GATE THESE TWO TESTS GUARD IS THE ONE THAT WAS MISSING (#351).
//
// Dev-auth used to be selected by the ABSENCE of OIDC plus a non-empty subject.
// That is not a statement of intent — it is the default state of an unedited
// config — and `deploy/.env.example` shipped the subject UNCOMMENTED beneath a
// comment reading "Leave both commented to force the OIDC path". So the
// documented first step of a deployment produced a reachable mailbox with no
// authentication at all, and nothing anywhere said no.
//
// The pair matters more than either half: the refusal proves the gate exists,
// and the acceptance proves the gate is not simply "dev-auth is broken now" —
// a test suite where only the refusal existed would pass just as happily if
// dev-auth had been deleted outright.
func TestDevAuthIsRefusedWithoutTheAcknowledgement(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthEmail, "operator@example.com")
	// Deliberately NOT setting EnvDevAuthAck.

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want a refusal: dev-auth disables authentication " +
			"entirely and must not be selectable by omission")
	}
	// The message has to be actionable, not merely present: an operator who hits
	// this at 3am needs the variable to set and the phrase to set it to.
	for _, want := range []string{EnvDevAuthSubject, EnvDevAuthAck, DevAuthAckPhrase} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestDevAuthIsHonoredWithTheAcknowledgement(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthEmail, "operator@example.com")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want the acknowledged dev-auth path to load", err)
	}
	if !cfg.DevAuthConfigured() {
		t.Error("DevAuthConfigured() = false, want true once acknowledged")
	}
}

// A near-miss must not pass. Anything other than the exact phrase is refused, so
// a half-remembered value ("yes", "true", the variable name) cannot switch
// authentication off.
func TestDevAuthAcknowledgementMustBeExact(t *testing.T) {
	for _, wrong := range []string{"yes", "true", "1", "I_UNDERSTAND", DevAuthAckPhrase + "!", strings.ToUpper(DevAuthAckPhrase)} {
		t.Run(wrong, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvDatabaseURL, testDSN)
			t.Setenv(EnvDevAuthSubject, "dev-operator")
			t.Setenv(EnvDevAuthAck, wrong)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() error = nil for %s=%q, want a refusal: only the exact phrase may disable authentication",
					EnvDevAuthAck, wrong)
			}
		})
	}
}

// A _FILE pointing at a DIRECTORY must fail loudly, and this is not a
// hypothetical shape (#351). Compose's `file:` secrets are bind mounts, and a
// secret whose source file does not exist is NOT refused: measured on Docker
// 29.5.3 / Compose 5.1.4, `docker compose config` exits 0 and the daemon
// CREATES the absent path as a directory — on the host and at
// /run/secrets/<name>. So "the operator forgot to create
// secrets/mailroom_session_key.txt" arrives here as a _FILE pointing at a
// directory, not as an unset variable.
//
// The requirement is that it can never be mistaken for an EMPTY secret, because
// empty is a legitimate configuration (it is how a dev-auth instance leaves the
// OIDC pair) and would send this down a different, quieter path. os.ReadFile
// returns "is a directory" and resolveSecret propagates it, so Load fails.
func TestAuthSecretFileThatIsADirectoryIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"session key", EnvSessionKey},
		{"OIDC client secret", EnvOIDCClientSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvDatabaseURL, testDSN)

			dir := t.TempDir()
			asDir := filepath.Join(dir, "not-a-file")
			if err := os.Mkdir(asDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			// Everything else is valid, so the ONLY reason Load can fail is the
			// directory — a test that left the rest empty would pass on "no auth
			// path configured" and prove nothing.
			t.Setenv(EnvOIDCIssuer, testIssuer)
			t.Setenv(EnvOIDCClientID, testClientID)
			t.Setenv(EnvOIDCRedirectURL, testRedirect)
			t.Setenv(EnvOIDCClientSecret, testSecret)
			t.Setenv(EnvSessionKey, testSessionKey)
			t.Setenv(tc.env+fileSuffix, asDir)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil; %s%s pointed at a directory and was accepted — "+
					"a forgotten secret file must not resolve to an empty secret", tc.env, fileSuffix)
			}
			// ⚠ THE PATH, NOT JUST THE VARIABLE NAME, AND THE PROBE IS WHY.
			// Deleting resolveSecret's error check leaves BOTH cases still
			// failing Load — the secret just resolves to "" and validateAuth
			// rejects that instead. For the session key it even fails with a
			// message naming MAILROOM_SESSION_KEY_FILE, so an assertion on the
			// variable name alone passes with the mechanism removed. Only the
			// path is unique to the read error, and it is also the thing the
			// operator needs: which file to go and create.
			if !strings.Contains(err.Error(), asDir) {
				t.Errorf("Load() error = %q; want the unreadable PATH %q, so the failure reads as "+
					"\"that file is wrong\" rather than \"this secret is empty\" — an empty secret is "+
					"a legitimate configuration and must not be how a forgotten file presents",
					err, asDir)
			}
			if !strings.Contains(err.Error(), tc.env+fileSuffix) {
				t.Errorf("Load() error = %q; want it to name %s%s too", err, tc.env, fileSuffix)
			}
		})
	}
}
