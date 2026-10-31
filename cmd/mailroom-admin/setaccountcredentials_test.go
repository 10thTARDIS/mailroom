// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/config"
	"mailroom/internal/onboarding"
	"mailroom/internal/sync"
)

// ---- flag-shape validation (mailroom#571) ----------------------------------

func TestParseCredentialSourceExactlyOne(t *testing.T) {
	cases := []struct {
		name             string
		passwordFile     string
		fromAccountsFile bool
		accountName      string
		wantKind         credentialSourceKind
		wantErr          bool
	}{
		{"password file alone", "/run/secrets/pw", false, "", credentialSourcePasswordFile, false},
		{"accounts file with account name", "", true, "work", credentialSourceAccountsFile, false},
		{"neither given", "", false, "", credentialSourceNone, true},
		{"both given", "/run/secrets/pw", true, "work", credentialSourceNone, true},
		{"accounts file without account name", "", true, "", credentialSourceNone, true},
		{"account name without accounts file", "/run/secrets/pw", false, "work", credentialSourceNone, true},
		{"account name alone", "", false, "work", credentialSourceNone, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCredentialSource(tc.passwordFile, tc.fromAccountsFile, tc.accountName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCredentialSource(%q, %v, %q) = %+v, nil; want an error",
						tc.passwordFile, tc.fromAccountsFile, tc.accountName, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCredentialSource(%q, %v, %q): %v", tc.passwordFile, tc.fromAccountsFile, tc.accountName, err)
			}
			if got.kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", got.kind, tc.wantKind)
			}
		})
	}
}

// ---- findDeclaredAccount ----------------------------------------------------

func TestFindDeclaredAccount(t *testing.T) {
	declared := []config.Account{{Name: "personal"}, {Name: "work"}}
	if _, ok := findDeclaredAccount(declared, "work"); !ok {
		t.Error("expected to find \"work\"")
	}
	if _, ok := findDeclaredAccount(declared, "missing"); ok {
		t.Error("expected \"missing\" to be absent")
	}
}

// ---- resolvePendingCredential (accounts-file mode's SMTP fallback) --------

// writeAccountsFile writes a minimal, valid accounts document with one
// account, returning its path.
func writeAccountsFile(t *testing.T, name, username, password, smtpUsername, smtpPassword string) string {
	t.Helper()
	doc := fmt.Sprintf(`{"accounts":[{"name":%q,"imap_host":"imap.example.com","smtp_host":"smtp.example.com",`+
		`"username":%q,"password":%q,"smtp_username":%q,"smtp_password":%q}]}`,
		name, username, password, smtpUsername, smtpPassword)
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}
	return path
}

// TestResolvePendingCredentialFromAccountsFileFallsBackSMTPToIMAP drives the
// REAL resolvePendingCredential against a REAL accounts file: an account that
// leaves smtp_username/smtp_password blank must resolve to the IMAP login for
// both, exactly as compareOneAccount's own fallback treats it
// (accounts_report.go) — so a file that legitimately leaves them blank rotates
// to what accounts-report already expects a converged account to hold.
func TestResolvePendingCredentialFromAccountsFileFallsBackSMTPToIMAP(t *testing.T) {
	path := writeAccountsFile(t, "work", "you@example.com", "hunter2", "", "")
	source := credentialSource{kind: credentialSourceAccountsFile, accountName: "work"}
	got, err := resolvePendingCredential(source, "", path)
	if err != nil {
		t.Fatalf("resolvePendingCredential: %v", err)
	}
	if got.Username != "you@example.com" || got.Password != "hunter2" {
		t.Errorf("IMAP login = %q/%q, want you@example.com/hunter2", got.Username, got.Password)
	}
	if got.SMTPUsername != "you@example.com" || got.SMTPPassword != "hunter2" {
		t.Errorf("SMTP fallback = %q/%q, want the IMAP login", got.SMTPUsername, got.SMTPPassword)
	}
}

// TestResolvePendingCredentialFromAccountsFilePreservesSeparateSMTP proves the
// one thing --password-file cannot do: a file that DOES declare its own SMTP
// login must keep it, not collapse it to the IMAP one.
func TestResolvePendingCredentialFromAccountsFilePreservesSeparateSMTP(t *testing.T) {
	path := writeAccountsFile(t, "work", "you@example.com", "hunter2", "submission-user", "submission-pass")
	source := credentialSource{kind: credentialSourceAccountsFile, accountName: "work"}
	got, err := resolvePendingCredential(source, "", path)
	if err != nil {
		t.Fatalf("resolvePendingCredential: %v", err)
	}
	if got.SMTPUsername != "submission-user" || got.SMTPPassword != "submission-pass" {
		t.Errorf("SMTP credentials = %q/%q, want the file's own separate SMTP login", got.SMTPUsername, got.SMTPPassword)
	}
}

// TestResolvePendingCredentialUsernameOverride proves --username overrides the
// file's declared login in accounts-file mode.
func TestResolvePendingCredentialUsernameOverride(t *testing.T) {
	path := writeAccountsFile(t, "work", "you@example.com", "hunter2", "", "")
	source := credentialSource{kind: credentialSourceAccountsFile, accountName: "work"}
	got, err := resolvePendingCredential(source, "override@example.com", path)
	if err != nil {
		t.Fatalf("resolvePendingCredential: %v", err)
	}
	if got.Username != "override@example.com" {
		t.Errorf("Username = %q, want the override", got.Username)
	}
}

// TestResolvePendingCredentialUnknownAccountName fails clearly rather than
// silently resolving to the zero value.
func TestResolvePendingCredentialUnknownAccountName(t *testing.T) {
	path := writeAccountsFile(t, "work", "you@example.com", "hunter2", "", "")
	source := credentialSource{kind: credentialSourceAccountsFile, accountName: "missing"}
	if _, err := resolvePendingCredential(source, "", path); err == nil {
		t.Fatal("expected an error for an account name absent from the file")
	}
}

// TestResolvePendingCredentialFromPasswordFile proves the --password-file path
// reads and trims the file (like every other --password-file flag in
// mailroom-admin) and leaves SMTPUsername/SMTPPassword empty — mirroring them
// onto Username/Password happens in setAccountCredentials, matching
// Account/set's own single-login connection-change model exactly.
func TestResolvePendingCredentialFromPasswordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	source := credentialSource{kind: credentialSourcePasswordFile, passwordFile: path}
	got, err := resolvePendingCredential(source, "", "")
	if err != nil {
		t.Fatalf("resolvePendingCredential: %v", err)
	}
	if got.Password != "hunter2" {
		t.Errorf("Password = %q, want the trimmed file contents", got.Password)
	}
	if got.Username != "" {
		t.Errorf("Username = %q, want empty (keep the stored one)", got.Username)
	}
	if got.SMTPUsername != "" || got.SMTPPassword != "" {
		t.Errorf("SMTP fields should be left empty for setAccountCredentials to mirror, got %q/%q", got.SMTPUsername, got.SMTPPassword)
	}
}

// ---- credentialsEqual --------------------------------------------------------

func TestCredentialsEqual(t *testing.T) {
	a := onboarding.Credentials{IMAPUsername: "u", IMAPPassword: "p", SMTPUsername: "u", SMTPPassword: "p"}
	same := a
	if !credentialsEqual(a, same) {
		t.Error("identical credentials compared unequal")
	}
	diffPass := a
	diffPass.IMAPPassword = "different"
	if credentialsEqual(a, diffPass) {
		t.Error("differing IMAP password compared equal")
	}
	diffUser := a
	diffUser.IMAPUsername = "other"
	if credentialsEqual(a, diffUser) {
		t.Error("differing IMAP username compared equal")
	}
	diffSMTPPass := a
	diffSMTPPass.SMTPPassword = "different"
	if credentialsEqual(a, diffSMTPPass) {
		t.Error("differing SMTP password compared equal")
	}
}

// ---- the password never appears in output or errors (mailroom#571) --------

// TestAccountValidationErrorNeverLeaksPassword drives the REAL error-formatting
// function setAccountCredentials returns on a rejected credential, through the
// REAL onboarding.ValidateAccount, with no database and no real network: the
// IMAP host is a literal address (skips DNS resolution, internal/onboarding's
// egress guard) and the dial is faked to fail exactly like a wrong password
// would. If accountValidationError, onboarding.ValidateAccount, or the
// underlying sync.Error ever start interpolating the candidate secret, this
// goes red.
func TestAccountValidationErrorNeverLeaksPassword(t *testing.T) {
	const password = "s3cret-rotate-me"
	const username = "someone@example.com"
	dial := func(_ context.Context, cfg sync.Config) (sync.SyncSession, error) {
		if cfg.Username != username || cfg.Password != password {
			t.Fatalf("dial got unexpected credentials: %q/%q", cfg.Username, cfg.Password)
		}
		return nil, &sync.Error{Kind: sync.ErrorKindAuth, Op: "dial",
			Err: errors.New("NO [AUTHENTICATIONFAILED] Authentication failed")}
	}
	spec := onboarding.AccountSpec{
		Name: "work", IMAPHost: "203.0.113.10", IMAPPort: 993,
		SMTPHost: "203.0.113.10", SMTPPort: 465,
		Username: username, Password: password, TLS: onboarding.TLSImplicit,
	}
	_, verr := onboarding.ValidateAccount(context.Background(), dial, spec, onboarding.WithAllowPrivateHosts(true))
	if verr == nil {
		t.Fatal("expected a validation error")
	}
	err := accountValidationError(spec.IMAPHost, verr)
	if err == nil {
		t.Fatal("accountValidationError returned nil")
	}
	msg := err.Error()
	if strings.Contains(msg, password) {
		t.Errorf("validation-failure error leaked the password: %q", msg)
	}
	if strings.Contains(msg, username) {
		t.Errorf("validation-failure error leaked the username: %q", msg)
	}
	if !strings.Contains(msg, "203.0.113.10") {
		t.Errorf("validation-failure error should still name the host: %q", msg)
	}
}
