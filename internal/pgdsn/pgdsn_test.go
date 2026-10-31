// SPDX-License-Identifier: AGPL-3.0-only

package pgdsn

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// writeSecret writes contents to a fresh file under t.TempDir() and returns
// its path.
func writeSecret(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return path
}

func TestWithPasswordFileEmptyPathIsNoop(t *testing.T) {
	const raw = "postgres://mailroom_app@postgres:5432/mailroom?sslmode=disable"
	got, err := WithPasswordFile(raw, "")
	if err != nil {
		t.Fatalf("WithPasswordFile: %v", err)
	}
	if got != raw {
		t.Errorf("got %q, want unchanged %q", got, raw)
	}
}

func TestWithPasswordFileEmptyPasswordIsError(t *testing.T) {
	path := writeSecret(t, "\n\n")
	_, err := WithPasswordFile("postgres://mailroom_app@postgres:5432/mailroom", path)
	if err == nil {
		t.Fatal("empty password file was accepted")
	}
}

func TestWithPasswordFileMissingFile(t *testing.T) {
	_, err := WithPasswordFile("postgres://mailroom_app@postgres:5432/mailroom", filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("missing password file was accepted")
	}
}

func TestWithPasswordFileRejectsKeywordValueDSN(t *testing.T) {
	path := writeSecret(t, "s3cret")
	_, err := WithPasswordFile("host=postgres user=mailroom_app dbname=mailroom", path)
	if err == nil {
		t.Fatal("a keyword/value DSN was accepted")
	}
	if !strings.Contains(err.Error(), "URL") {
		t.Errorf("error does not explain the URL-form requirement: %v", err)
	}
}

func TestWithPasswordFileRejectsWrongScheme(t *testing.T) {
	path := writeSecret(t, "s3cret")
	_, err := WithPasswordFile("mysql://mailroom_app@postgres:5432/mailroom", path)
	if err == nil {
		t.Fatal("a non-postgres scheme was accepted")
	}
}

func TestWithPasswordFileRequiresUsername(t *testing.T) {
	path := writeSecret(t, "s3cret")
	_, err := WithPasswordFile("postgres://postgres:5432/mailroom", path)
	if err == nil {
		t.Fatal("a URL with no username was accepted")
	}
}

func TestWithPasswordFileRejectsExistingPassword(t *testing.T) {
	path := writeSecret(t, "s3cret")
	_, err := WithPasswordFile("postgres://mailroom_app:already-set@postgres:5432/mailroom", path)
	if err == nil {
		t.Fatal("a URL that already carries a password was accepted")
	}
	if !strings.Contains(err.Error(), "one place") {
		t.Errorf("error does not explain the one-place rule: %v", err)
	}
}

// TestWithPasswordFileTrimsTrailingNewlinesOnly pins the exact stripping rule
// (mirrors POSIX $(cat file), not strings.TrimSpace): trailing "\n\n" is
// stripped entirely, a trailing space survives, and a CRLF-saved file keeps
// its bare trailing '\r'.
func TestWithPasswordFileTrimsTrailingNewlinesOnly(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"single trailing newline stripped", "p@ss\n", "p@ss"},
		{"double trailing newline stripped", "p@ss\n\n", "p@ss"},
		{"trailing space kept", "p@ss ", "p@ss "},
		{"trailing CRLF leaves bare CR", "p@ss\r\n", "p@ss\r"},
		{"no trailing newline", "p@ss", "p@ss"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSecret(t, tc.content)
			got, err := WithPasswordFile("postgres://mailroom_app@postgres:5432/mailroom", path)
			if err != nil {
				t.Fatalf("WithPasswordFile: %v", err)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", got, err)
			}
			pw, ok := u.User.Password()
			if !ok {
				t.Fatalf("returned URL has no password: %q", got)
			}
			if pw != tc.want {
				t.Errorf("password = %q, want %q", pw, tc.want)
			}
		})
	}
}

// TestWithPasswordFileSpecialCharactersRoundTrip is the core contract: a
// password containing every character the compose-generated secrets could
// plausibly contain must survive both a plain net/url re-parse AND
// pgconn.ParseConfig, which is what the actual binaries use to connect.
func TestWithPasswordFileSpecialCharactersRoundTrip(t *testing.T) {
	cases := []string{
		"@", "/", ":", "%", "'", "#", "?", " ",
		"p@ss/wo:rd%'# x",
		"a@b:c/d%e'f#g?h i",
	}
	for _, want := range cases {
		t.Run(want, func(t *testing.T) {
			path := writeSecret(t, want+"\n")
			got, err := WithPasswordFile("postgres://mailroom_app@postgres:5432/mailroom?sslmode=disable", path)
			if err != nil {
				t.Fatalf("WithPasswordFile: %v", err)
			}

			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", got, err)
			}
			pw, ok := u.User.Password()
			if !ok {
				t.Fatalf("returned URL has no password: %q", got)
			}
			if pw != want {
				t.Errorf("net/url: password = %q, want %q", pw, want)
			}
			if u.User.Username() != "mailroom_app" {
				t.Errorf("net/url: username = %q, want mailroom_app", u.User.Username())
			}

			cfg, err := pgconn.ParseConfig(got)
			if err != nil {
				t.Fatalf("pgconn.ParseConfig(%q): %v", got, err)
			}
			if cfg.Password != want {
				t.Errorf("pgconn: Password = %q, want %q", cfg.Password, want)
			}
			if cfg.User != "mailroom_app" {
				t.Errorf("pgconn: User = %q, want mailroom_app", cfg.User)
			}
		})
	}
}

// TestWithPasswordFileErrorsNeverLeakThePasswordOrURL feeds a distinctive
// password through every error path that reads the file, and asserts the
// distinctive value never appears in the returned error string. It also
// checks that a distinctive substring of the URL never leaks — errors must
// name the problem, not echo the value.
func TestWithPasswordFileErrorsNeverLeakThePasswordOrURL(t *testing.T) {
	const distinctivePassword = "sUpEr-sEcReT-p@55w0rd!437"
	const urlMarker = "do-not-print-this-host.invalid"

	cases := []struct {
		name string
		url  string
	}{
		{"malformed URL", "://" + urlMarker + "/mailroom"},
		{"wrong scheme", "mysql://mailroom_app@" + urlMarker + "/mailroom"},
		{"keyword/value form", "host=" + urlMarker + " user=mailroom_app dbname=mailroom"},
		{"no username", "postgres://" + urlMarker + "/mailroom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSecret(t, distinctivePassword+"\n")
			_, err := WithPasswordFile(tc.url, path)
			if err == nil {
				t.Fatalf("case %q unexpectedly succeeded", tc.name)
			}
			if strings.Contains(err.Error(), distinctivePassword) {
				t.Errorf("error leaked the password: %v", err)
			}
			if strings.Contains(err.Error(), urlMarker) {
				t.Errorf("error leaked the URL: %v", err)
			}
		})
	}

	// The "already has a password" path: the file's password has been read
	// successfully by the time this check runs, so it is the one most at
	// risk of appearing in the error text.
	t.Run("already has a password", func(t *testing.T) {
		path := writeSecret(t, distinctivePassword+"\n")
		existing := "postgres://mailroom_app:already-set-" + urlMarker + "@postgres/mailroom"
		_, err := WithPasswordFile(existing, path)
		if err == nil {
			t.Fatal("unexpectedly succeeded")
		}
		if strings.Contains(err.Error(), distinctivePassword) {
			t.Errorf("error leaked the file's password: %v", err)
		}
		if strings.Contains(err.Error(), urlMarker) {
			t.Errorf("error leaked the URL (or its existing password): %v", err)
		}
	})
}
