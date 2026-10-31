// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveDSNInjectsPasswordFile pins the #537 path: a plain owner URL with
// no password, plus MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE, ends up with the
// file's password injected into the returned DSN.
func TestResolveDSNInjectsPasswordFile(t *testing.T) {
	for _, k := range []string{
		"MAILROOM_MIGRATE_DATABASE_URL", "MAILROOM_MIGRATE_DATABASE_URL_FILE",
		"MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE",
	} {
		t.Setenv(k, "")
	}

	pwPath := filepath.Join(t.TempDir(), "owner_password")
	const want = "p@ss/wo:rd%'# x"
	if err := os.WriteFile(pwPath, []byte(want+"\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	t.Setenv("MAILROOM_MIGRATE_DATABASE_URL", "postgres://mailroom_owner@postgres:5432/mailroom?sslmode=disable")
	t.Setenv("MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE", pwPath)

	got, err := resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if !strings.Contains(got, "mailroom_owner:") {
		t.Fatalf("resolveDSN() = %q, does not look like it carries a password", got)
	}
}

// TestResolveDSNMissing pins the existing required-variable error, unaffected
// by the password-file addition.
func TestResolveDSNMissing(t *testing.T) {
	for _, k := range []string{
		"MAILROOM_MIGRATE_DATABASE_URL", "MAILROOM_MIGRATE_DATABASE_URL_FILE",
		"MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE",
	} {
		t.Setenv(k, "")
	}
	if _, err := resolveDSN(); err == nil {
		t.Fatal("resolveDSN() error = nil, want error for missing owner DSN")
	}
}

// highestVersion decides whether the drift warning fires at all, so both of its
// answers matter: reporting too low a version cries wolf on every deploy, and
// reporting too high stays silent in exactly the case the warning exists for.
func TestHighestVersion(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  int64
	}{
		{
			name:  "the real shape",
			names: []string{"00001_baseline.sql", "00017_label_rule_provenance.sql", "00002_x.sql"},
			want:  17,
		},
		{
			// The bug this guards: string ordering puts "00009" above "00010",
			// so a max taken over names rather than parsed numbers reports 9 and
			// the warning never fires.
			name:  "ordered numerically, not lexically",
			names: []string{"00009_nine.sql", "00010_ten.sql"},
			want:  10,
		},
		{
			name:  "ignores non-migrations",
			names: []string{"00003_three.sql", "README.md", "schema.sql.bak", ".keep"},
			want:  3,
		},
		{
			name:  "no parseable migrations disables the check",
			names: []string{"README.md"},
			want:  0,
		},
		{
			name:  "empty",
			names: nil,
			want:  0,
		},
		{
			// goose's own NumericComponent requires the "_" separator and
			// rejects a name without one, so this must be ignored rather than
			// read as version 42. Matching goose here is the point: a parser
			// that disagreed with it about which version a file is would warn
			// about drift that does not exist, or miss drift that does.
			name:  "a name with no separator is not a migration",
			names: []string{"00042.sql"},
			want:  0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := highestVersion(c.names); got != c.want {
				t.Errorf("highestVersion(%v) = %d, want %d", c.names, got, c.want)
			}
		})
	}
}

// The binary must know its own migration set — if the embedded FS were mis-wired
// the check would silently disable itself (highestVersion == 0), which is the
// failure mode this whole guard is meant to eliminate.
func TestEmbeddedMigrationsAreReadable(t *testing.T) {
	names, err := migrationNames()
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations found — the drift check would disable itself")
	}
	if v := highestVersion(names); v <= 0 {
		t.Fatalf("highestVersion over the embedded set = %d, want > 0", v)
	}
}

// Both answers matter. A warning that fires when it should not would appear on
// every ordinary deploy until people learned to ignore it — which is the same as
// having no warning at all, and is the failure mode this guard exists to fix.
func TestDriftWarning(t *testing.T) {
	t.Run("silent when the database matches the build", func(t *testing.T) {
		if got := driftWarning(17, 17); got != "" {
			t.Errorf("driftWarning(17, 17) = %q, want \"\"", got)
		}
	})
	t.Run("silent when the database is behind (migrations pending)", func(t *testing.T) {
		// Normal for a deploy that is about to apply new migrations: goose is
		// about to fix it, and saying anything here would be noise.
		if got := driftWarning(15, 17); got != "" {
			t.Errorf("driftWarning(15, 17) = %q, want \"\"", got)
		}
	})
	t.Run("silent on a fresh database", func(t *testing.T) {
		if got := driftWarning(0, 17); got != "" {
			t.Errorf("driftWarning(0, 17) = %q, want \"\"", got)
		}
	})
	t.Run("disabled rather than tripped when no migrations are readable", func(t *testing.T) {
		if got := driftWarning(17, 0); got != "" {
			t.Errorf("driftWarning(17, 0) = %q, want \"\" (an empty set must not make every database look ahead)", got)
		}
	})
	t.Run("fires when the database is ahead", func(t *testing.T) {
		got := driftWarning(18, 17)
		if got == "" {
			t.Fatal("driftWarning(18, 17) = \"\", want a warning — this is the real case")
		}
		// The numbers have to be right or the operator cannot act on it: the
		// version it IS, the version we carry, and the version goose will SKIP.
		for _, want := range []string{"version 18", "up to 17", "SKIP version 18"} {
			if !strings.Contains(got, want) {
				t.Errorf("warning does not mention %q:\n%s", want, got)
			}
		}
	})
}
