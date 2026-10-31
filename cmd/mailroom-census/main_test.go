// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/census"
)

func writeAccountsFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

const testAccountsFile = `{
  "accounts": [
    {
      "name": "personal",
      "imap_host": "imap.example.com",
      "imap_port": 993,
      "smtp_host": "smtp.example.com",
      "smtp_port": 465,
      "username": "you@example.com",
      "password": "hunter2-the-census-must-never-print-this"
    },
    {
      "name": "work",
      "imap_host": "imap.work.example",
      "smtp_host": "smtp.work.example",
      "username": "you@work.example",
      "password": "s3cret"
    }
  ]
}`

func TestConnectionConfigFromAccountsFile(t *testing.T) {
	path := writeAccountsFile(t, testAccountsFile)

	cfg, err := connectionConfig(options{accountsFile: path, account: "personal"}, "")
	if err != nil {
		t.Fatalf("connectionConfig: %v", err)
	}
	want := census.Config{
		Host: "imap.example.com", Port: 993, TLS: census.TLSImplicit,
		Username: "you@example.com", Password: "hunter2-the-census-must-never-print-this",
	}
	if cfg != want {
		t.Errorf("connectionConfig = %+v, want %+v", cfg, want)
	}
}

func TestConnectionConfigFromAccountsFilePortOverride(t *testing.T) {
	path := writeAccountsFile(t, testAccountsFile)

	cfg, err := connectionConfig(options{
		accountsFile: path, account: "work",
		port: 1143, portSet: true,
	}, "")
	if err != nil {
		t.Fatalf("connectionConfig: %v", err)
	}
	if cfg.Port != 1143 {
		t.Errorf("Port = %d, want 1143 (explicit -port must override the file)", cfg.Port)
	}
	if cfg.TLS != census.TLSImplicit {
		t.Errorf("TLS = %v, want TLSImplicit (there is no -tls override; see the package doc)", cfg.TLS)
	}
	if cfg.Host != "imap.work.example" || cfg.Username != "you@work.example" {
		t.Errorf("Host/Username = %s/%s, want imap.work.example/you@work.example", cfg.Host, cfg.Username)
	}
}

func TestConnectionConfigAccountsFileAlwaysImplicitTLS(t *testing.T) {
	path := writeAccountsFile(t, testAccountsFile)

	cfg, err := connectionConfig(options{accountsFile: path, account: "work"}, "")
	if err != nil {
		t.Fatalf("connectionConfig: %v", err)
	}
	if cfg.TLS != census.TLSImplicit {
		t.Errorf("TLS = %v, want TLSImplicit (accountSpec's own default, cmd/mailroom/main.go)", cfg.TLS)
	}
	// DefaultIMAPPort (993) was applied by LoadAccountsFile itself.
	if cfg.Port != 993 {
		t.Errorf("Port = %d, want 993", cfg.Port)
	}
}

func TestConnectionConfigMissingAccountsFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	_, err := connectionConfig(options{accountsFile: missing, account: "personal"}, "")
	if err == nil {
		t.Fatal("connectionConfig: error = nil, want an error for a missing accounts file")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error text leaks a password: %v", err)
	}
}

func TestConnectionConfigEmptyAccountsFile(t *testing.T) {
	path := writeAccountsFile(t, `{"accounts": []}`)
	_, err := connectionConfig(options{accountsFile: path, account: "personal"}, "")
	if err == nil {
		t.Fatal("connectionConfig: error = nil, want an error for an empty accounts document")
	}
}

func TestConnectionConfigUnknownAccountName(t *testing.T) {
	path := writeAccountsFile(t, testAccountsFile)
	_, err := connectionConfig(options{accountsFile: path, account: "nonexistent"}, "")
	if err == nil {
		t.Fatal("connectionConfig: error = nil, want an error for an unknown account name")
	}
	if !strings.Contains(err.Error(), "personal") || !strings.Contains(err.Error(), "work") {
		t.Errorf("error = %v, want it to name the accounts actually present (personal, work)", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error text leaks a password: %v", err)
	}
}

func TestConnectionConfigMixingFlagStylesIsAnError(t *testing.T) {
	path := writeAccountsFile(t, testAccountsFile)

	cases := []options{
		{accountsFile: path, account: "personal", host: "imap.other.example"},
		{accountsFile: path, account: "personal", user: "someone@example.com"},
		{accountsFile: path, account: "personal", passwordFile: "/tmp/whatever"},
	}
	for _, o := range cases {
		if _, err := connectionConfig(o, ""); err == nil {
			t.Errorf("connectionConfig(%+v): error = nil, want an error (mixing accounts-file and standalone flags)", o)
		}
	}

	// The environment variable counts as "standalone mode" too.
	if _, err := connectionConfig(options{accountsFile: path, account: "personal"}, "env-password"); err == nil {
		t.Error("connectionConfig with both -accounts-file and MAILROOM_CENSUS_PASSWORD set: error = nil, want an error")
	}
}

func TestConnectionConfigStandaloneFlags(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(pwFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := connectionConfig(options{
		host: "imap.example.com", port: 993,
		user: "you@example.com", passwordFile: pwFile,
	}, "")
	if err != nil {
		t.Fatalf("connectionConfig: %v", err)
	}
	if cfg.Password != "s3cret" {
		t.Errorf("Password = %q, want %q (trailing newline trimmed)", cfg.Password, "s3cret")
	}
	if cfg.TLS != census.TLSImplicit {
		t.Errorf("TLS = %v, want TLSImplicit (there is no -tls flag; see the package doc)", cfg.TLS)
	}
}

func TestConnectionConfigStandaloneMissingHost(t *testing.T) {
	_, err := connectionConfig(options{user: "you@example.com"}, "env-password")
	if err == nil {
		t.Fatal("connectionConfig: error = nil, want -host required error")
	}
}

func TestConnectionConfigStandaloneMissingPassword(t *testing.T) {
	_, err := connectionConfig(options{host: "imap.example.com", user: "you@example.com"}, "")
	if err == nil {
		t.Fatal("connectionConfig: error = nil, want a password-required error")
	}
}

func TestResolvePassword(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(pwFile, []byte("hunter2\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePassword(pwFile, "")
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("resolvePassword() = %q, want %q (CRLF trimmed)", got, "hunter2")
	}

	got, err = resolvePassword("", "env-secret")
	if err != nil || got != "env-secret" {
		t.Errorf("resolvePassword(env) = %q, %v, want %q, nil", got, err, "env-secret")
	}

	if _, err := resolvePassword("", ""); err == nil {
		t.Error("resolvePassword() with neither source: error = nil, want an error")
	}
}

func TestMessagesCannotBeCombinedWithReportOptions(t *testing.T) {
	for _, extra := range [][]string{{"-json"}, {"-probe-connections", "3"}} {
		args := append([]string{"-messages", "-host", "imap.example.com", "-user", "you@example.com"}, extra...)
		err := run(args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "-messages cannot be combined") {
			t.Errorf("run(%v) error = %v, want the -messages combination refusal (before any dial)", args, err)
		}
	}
}
