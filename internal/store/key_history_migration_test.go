// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"mailroom/db"
)

// TestKeyHistoryMigrationInventsNoDates pins #815's backfill rule: migration
// 00071 adds retired_at, replaced_by, removed_at and checked_at as plain
// nullable columns and writes NO row. NULL there means "not recorded"; a
// backfill (an UPDATE, or a DEFAULT that fills existing rows) would turn a key
// retired years ago into one "retired on the day we upgraded".
func TestKeyHistoryMigrationInventsNoDates(t *testing.T) {
	raw, err := fs.ReadFile(db.Migrations, "migrations/00071_key_history.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := string(raw)
	if i := strings.Index(up, "-- +goose Up"); i >= 0 {
		up = up[i:]
	}
	if i := strings.Index(up, "-- +goose Down"); i >= 0 {
		up = up[:i]
	}
	var code []string
	for _, line := range strings.Split(up, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "--") {
			code = append(code, l)
		}
	}
	body := strings.Join(code, "\n")
	if len(code) < 5 {
		t.Fatalf("read only %d code lines of 00071's Up section; the test is not looking at the migration", len(code))
	}
	// A statement that writes rows (not the GRANT naming the privileges).
	if regexp.MustCompile(`(?im)^(UPDATE\s+\S+\s+SET|INSERT\s+INTO|COPY)\b`).MatchString(body) {
		t.Errorf("00071 writes rows; the history columns must start NULL (not recorded):\n%s", body)
	}
	for _, col := range []string{"retired_at", "replaced_by", "removed_at", "checked_at"} {
		re := regexp.MustCompile(`(?i)ADD COLUMN ` + col + `\b[^\n]*`)
		decl := re.FindString(body)
		if decl == "" {
			t.Errorf("00071 does not add %s", col)
			continue
		}
		if regexp.MustCompile(`(?i)\b(DEFAULT|NOT NULL)\b`).MatchString(decl) {
			t.Errorf("%s is declared %q: a DEFAULT or NOT NULL fills existing rows with an invented value", col, decl)
		}
	}
}
