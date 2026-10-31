// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes name under dir with the given contents and returns its path.
func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("os.WriteFile %s: %v", p, err)
	}
	return p
}

const goodAccountsFile = `{
  "accounts": [
    {
      "name": "personal",
      "provider_profile": "migadu",
      "imap_host": "imap.example.com",
      "imap_port": 993,
      "smtp_host": "smtp.example.com",
      "smtp_port": 465,
      "username": "you@example.com",
      "password": "hunter2"
    },
    {
      "name": "work",
      "imap_host": "imap.work.example",
      "smtp_host": "smtp.work.example",
      "username": "you@work.example",
      "password": "s3cret",
      "smtp_username": "submission@work.example",
      "smtp_password": "other"
    }
  ]
}`

func TestLoadAccountsFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "accounts.json", goodAccountsFile)

	accts, err := LoadAccountsFile(path)
	if err != nil {
		t.Fatalf("LoadAccountsFile: %v", err)
	}
	if len(accts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accts))
	}

	a := accts[0]
	if a.Name != "personal" || a.ProviderProfile != "migadu" {
		t.Errorf("accounts[0] = %+v, want name=personal profile=migadu", a)
	}
	if a.IMAPHost != "imap.example.com" || a.IMAPPort != 993 {
		t.Errorf("accounts[0] imap = %s:%d", a.IMAPHost, a.IMAPPort)
	}
	if a.Username != "you@example.com" || a.Password.Reveal() != "hunter2" {
		t.Errorf("accounts[0] credentials did not round-trip")
	}
	// writeback_enabled is absent from goodAccountsFile: nil, so CreateAccount
	// takes the schema default (true) rather than this field silently
	// shadowing the account.
	if a.WritebackEnabled != nil {
		t.Errorf("accounts[0].WritebackEnabled = %v, want nil (field absent)", *a.WritebackEnabled)
	}

	// Ports default to implicit TLS when omitted.
	b := accts[1]
	if b.IMAPPort != DefaultIMAPPort || b.SMTPPort != DefaultSMTPPort {
		t.Errorf("accounts[1] ports = %d/%d, want defaults %d/%d",
			b.IMAPPort, b.SMTPPort, DefaultIMAPPort, DefaultSMTPPort)
	}
	if b.SMTPUsername != "submission@work.example" || b.SMTPPassword.Reveal() != "other" {
		t.Errorf("accounts[1] separate SMTP credentials did not round-trip")
	}
}

func TestLoadAccountsFileAbsentIsNotAnError(t *testing.T) {
	// No path configured at all.
	accts, err := LoadAccountsFile("")
	if err != nil || accts != nil {
		t.Fatalf(`LoadAccountsFile("") = %v, %v; want nil, nil`, accts, err)
	}
	// A configured path that does not exist: still "no accounts", not a failure
	// — an instance may legitimately have none (mailroom-admin can add them).
	accts, err = LoadAccountsFile(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || accts != nil {
		t.Fatalf("LoadAccountsFile(missing) = %v, %v; want nil, nil", accts, err)
	}
}

func TestLoadAccountsFileMalformed(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"truncated JSON":    `{"accounts": [`,
		"not an object":     `["personal"]`,
		"wrong field type":  `{"accounts": [{"name": "a", "imap_port": "993"}]}`,
		"trailing content":  `{"accounts": []}{"accounts": []}`,
		"unknown key":       `{"accounts": [{"name": "a", "imap_hostname": "imap.example.com"}]}`,
		"unknown top key":   `{"acounts": []}`,
		"missing name":      `{"accounts": [{"imap_host": "i", "smtp_host": "s", "username": "u", "password": "p"}]}`,
		"missing imap_host": `{"accounts": [{"name": "a", "smtp_host": "s", "username": "u", "password": "p"}]}`,
		"missing smtp_host": `{"accounts": [{"name": "a", "imap_host": "i", "username": "u", "password": "p"}]}`,
		"missing username":  `{"accounts": [{"name": "a", "imap_host": "i", "smtp_host": "s", "password": "p"}]}`,
		"missing password":  `{"accounts": [{"name": "a", "imap_host": "i", "smtp_host": "s", "username": "u"}]}`,
		"port out of range": `{"accounts": [{"name": "a", "imap_host": "i", "imap_port": 70000, "smtp_host": "s", "username": "u", "password": "p"}]}`,
		"writeback_enabled not a bool": `{"accounts": [{"name": "a", "imap_host": "i", "smtp_host": "s",
			"username": "u", "password": "p", "writeback_enabled": "yes"}]}`,
		"duplicate name": `{"accounts": [
			{"name": "a", "imap_host": "i", "smtp_host": "s", "username": "u", "password": "p"},
			{"name": "a", "imap_host": "i2", "smtp_host": "s2", "username": "u2", "password": "p2"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, dir, "accounts.json", body)
			accts, err := LoadAccountsFile(path)
			if err == nil {
				t.Fatalf("LoadAccountsFile = %v, nil; want an error for %s", accts, name)
			}
			// A file the operator meant something by must never be silently
			// reduced to "no accounts".
			if accts != nil {
				t.Errorf("accounts = %v, want nil alongside the error", accts)
			}
		})
	}
}

// TestLoadAccountsFileWritebackEnabled pins the pointer contract an operator
// depends on to attach a real account already shadowed (ADR-028): the
// field absent must be nil (schema default true), and an explicit value must
// round-trip exactly, never silently coerced to the default.
func TestLoadAccountsFileWritebackEnabled(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *bool // nil means "absent -> nil"
	}{
		{
			name: "explicitly true",
			body: `{"accounts": [{"name": "a", "imap_host": "i", "smtp_host": "s",
				"username": "u", "password": "p", "writeback_enabled": true}]}`,
			want: boolPtr(true),
		},
		{
			name: "explicitly false",
			body: `{"accounts": [{"name": "a", "imap_host": "i", "smtp_host": "s",
				"username": "u", "password": "p", "writeback_enabled": false}]}`,
			want: boolPtr(false),
		},
		{
			name: "absent",
			body: `{"accounts": [{"name": "a", "imap_host": "i", "smtp_host": "s",
				"username": "u", "password": "p"}]}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "accounts.json", tc.body)
			accts, err := LoadAccountsFile(path)
			if err != nil {
				t.Fatalf("LoadAccountsFile: %v", err)
			}
			if len(accts) != 1 {
				t.Fatalf("len(accounts) = %d, want 1", len(accts))
			}
			got := accts[0].WritebackEnabled
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("WritebackEnabled = %v, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("WritebackEnabled = nil, want %v", *tc.want)
			case tc.want != nil && got != nil && *tc.want != *got:
				t.Errorf("WritebackEnabled = %v, want %v", *got, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestLoadAccountsFileEmptyList(t *testing.T) {
	path := writeFile(t, t.TempDir(), "accounts.json", `{"accounts": []}`)
	accts, err := LoadAccountsFile(path)
	if err != nil {
		t.Fatalf("LoadAccountsFile: %v", err)
	}
	if accts != nil {
		t.Errorf("accounts = %v, want nil for an empty list", accts)
	}
}

func TestLoadAccountsFileErrorsNeverQuoteSecrets(t *testing.T) {
	// The password sits in a field whose *value* is fine but whose sibling key
	// is bogus. The resulting error must not carry the password with it.
	const body = `{"accounts": [{"name": "a", "imap_host": "i", "smtp_host": "s",
	  "username": "u", "password": "top-secret-password", "bogus": 1}]}`
	path := writeFile(t, t.TempDir(), "accounts.json", body)

	_, err := LoadAccountsFile(path)
	if err == nil {
		t.Fatal("LoadAccountsFile: error = nil, want an unknown-field error")
	}
	if strings.Contains(err.Error(), "top-secret-password") {
		t.Fatalf("error leaks the password: %q", err)
	}
}

// TestSecretNeverRenders pins the redaction guarantee: the obvious accidents —
// printing, formatting the containing struct, structured logging, and
// re-marshaling — must all produce the placeholder, never the password.
func TestSecretNeverRenders(t *testing.T) {
	const pw = "top-secret-password"
	s := Secret(pw)

	// %s of a Secret is covered by the String() case (staticcheck rightly
	// objects to spelling it as fmt.Sprintf("%s", s)); the rest exercise the
	// formatting paths that do NOT go through Stringer.
	for _, got := range []string{
		s.String(),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%+v", Account{Name: "a", Password: s, SMTPPassword: s}),
		fmt.Sprint(slog.Any("password", s).Value.Resolve()),
	} {
		if strings.Contains(got, pw) {
			t.Errorf("rendered form leaks the password: %q", got)
		}
	}

	b, err := json.Marshal(Account{Name: "a", Password: s})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), pw) {
		t.Errorf("JSON form leaks the password: %s", b)
	}

	// ...and Reveal still returns it, or nothing would work.
	if s.Reveal() != pw {
		t.Errorf("Reveal() = %q, want the plaintext", s.Reveal())
	}
}

func TestLoadReadsAccountsFilePath(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvDatabaseURL, testDSN)
	t.Setenv(EnvDevAuthSubject, "dev-operator")
	t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	t.Setenv(EnvAccountsFile, "  /run/secrets/mailroom_accounts  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AccountsFile != "/run/secrets/mailroom_accounts" {
		t.Errorf("AccountsFile = %q, want the trimmed path", cfg.AccountsFile)
	}
}
