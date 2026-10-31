// SPDX-License-Identifier: AGPL-3.0-only

package store

// TestMigrationDMLRespectsForceRLS is the static guard for mailroom#505:
// migrations are applied by mailroom_owner, which — like any role without
// BYPASSRLS — is subject to FORCE ROW LEVEL SECURITY on every app table
// (CLAUDE.md "Database access"). With FORCE on and no policy granted to
// mailroom_owner, a plain UPDATE/INSERT/DELETE against such a table silently
// matches zero rows: no error, goose prints OK, and the backfill never
// happened on any real database. 00050's desk_preferences backfill and
// 00047's messages backfill both shipped with exactly this defect (see
// db/migrations/00051_backfills_under_force_rls.sql, which fixes both).
//
// This test parses every db/migrations/*.sql file as TEXT — no database
// required — and requires that any top-level UPDATE/INSERT/DELETE against a
// table db/schema.sql declares FORCE ROW LEVEL SECURITY sits after a
// matching `ALTER TABLE app.X NO FORCE ROW LEVEL SECURITY` and before the
// closing `ALTER TABLE app.X FORCE ROW LEVEL SECURITY`, in the same file
// (docs/migrations.md "Backfilling a FORCE-RLS table from a migration").
//
// "Top-level" excludes anything inside a `-- +goose StatementBegin` /
// `-- +goose StatementEnd` block: those are function/DO bodies that run
// later, under whatever role calls them (often mailroom_app, or a
// SECURITY DEFINER function's own definer privileges) — not one-shot DML
// that mailroom_owner executes while applying the migration. app.jobs's
// dequeue/reclaim functions and the user-provisioning functions all contain
// UPDATE/INSERT/DELETE on FORCE-RLS tables for exactly this reason, and none
// of them are the shape this issue is about.
import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"mailroom/db"
)

// forceRLSExempt lists migration files whose top-level DML predates this
// rule (#505). Migrations are forward-only (ADR-008): rewriting history to
// retrofit the wrap would make the applied-migration record lie about what
// actually ran, which is worse than a named, reviewable exception. Every
// entry states why it is safe to leave as shipped.
var forceRLSExempt = map[string]string{
	"00026_sync_scope_constraint.sql": "predates the rule (#505). Its " +
		"`UPDATE app.mailboxes` has the identical defect — mailboxes is " +
		"FORCE RLS and mailroom_owner holds no policy on it either — but " +
		"fixing it is OUT OF SCOPE for #505's 00051 migration and it is not " +
		"re-run there. A known gap, not a silent one: recorded here rather " +
		"than rewritten into history.",
	"00047_flagged_at.sql": "predates the rule (#505). Its " +
		"`UPDATE app.messages` is re-applied idempotently (flagged_at IS " +
		"NULL guard) by 00051_backfills_under_force_rls.sql under NO " +
		"FORCE/FORCE, so the historical statement here is left exactly as " +
		"shipped.",
	"00050_day_one_setup.sql": "predates the rule (#505). Its " +
		"`UPDATE app.desk_preferences` is re-applied idempotently " +
		"(setup_completed_at IS NULL guard) by " +
		"00051_backfills_under_force_rls.sql under NO FORCE/FORCE, so the " +
		"historical statement here is left exactly as shipped.",
}

var (
	reSchemaForceTable = regexp.MustCompile(`(?m)^ALTER TABLE app\.(\w+) FORCE ROW LEVEL SECURITY\s*;`)
	reNoForceStmt      = regexp.MustCompile(`(?i)ALTER TABLE app\.(\w+) NO FORCE ROW LEVEL SECURITY`)
	reForceStmt        = regexp.MustCompile(`(?i)ALTER TABLE app\.(\w+) FORCE ROW LEVEL SECURITY`)
	reUpdateStmt       = regexp.MustCompile(`(?i)\bUPDATE\s+app\.(\w+)\b`)
	reInsertStmt       = regexp.MustCompile(`(?i)\bINSERT\s+INTO\s+app\.(\w+)\b`)
	reDeleteStmt       = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+app\.(\w+)\b`)
)

// stripNonMigrationText blanks (preserving byte offsets and line numbers,
// so error messages and ordering both stay accurate):
//   - everything from a `--` to end of line (SQL line comments), and
//   - everything strictly between a `-- +goose StatementBegin` line and its
//     matching `-- +goose StatementEnd` line (function/DO bodies — see the
//     package doc comment above for why those are out of scope).
func stripNonMigrationText(src string) string {
	lines := strings.Split(src, "\n")
	inBody := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "-- +goose StatementBegin":
			inBody = true
			continue // the annotation line itself has no DML to hide
		case "-- +goose StatementEnd":
			inBody = false
			continue
		}
		if inBody {
			lines[i] = ""
			continue
		}
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// forceRLSTablesFromSchema returns the lowercase names of every app.X table
// db/schema.sql declares FORCE ROW LEVEL SECURITY on.
func forceRLSTablesFromSchema(t *testing.T) map[string]bool {
	t.Helper()
	// internal/store -> repo root is two levels up, matching the convention
	// used elsewhere in this repo for reading tracked files from tests (e.g.
	// internal/api/openapi_capability_test.go's "../../docs/openapi.yaml").
	raw, err := os.ReadFile("../../db/schema.sql")
	if err != nil {
		t.Fatalf("read db/schema.sql: %v", err)
	}
	clean := stripNonMigrationText(string(raw))
	tables := map[string]bool{}
	for _, m := range reSchemaForceTable.FindAllStringSubmatch(clean, -1) {
		tables[strings.ToLower(m[1])] = true
	}
	if len(tables) == 0 {
		t.Fatal("forceRLSTablesFromSchema found zero FORCE RLS tables in db/schema.sql — " +
			"the regex or the path stopped matching; every assertion below would pass vacuously")
	}
	return tables
}

func lineAt(src string, pos int) int {
	return strings.Count(src[:pos], "\n") + 1
}

func TestMigrationDMLRespectsForceRLS(t *testing.T) {
	forceRLSTables := forceRLSTablesFromSchema(t)

	entries, err := fs.ReadDir(db.Migrations, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("found zero migration files under db/migrations — the embed or the path is broken")
	}

	// Every exemption must name a file that still exists — a stale entry
	// (the file renamed or removed) would otherwise silently stop meaning
	// anything.
	live := map[string]bool{}
	for _, n := range names {
		live[n] = true
	}
	for exempt := range forceRLSExempt {
		if !live[exempt] {
			t.Errorf("forceRLSExempt lists %q, which does not exist under db/migrations — stale exemption", exempt)
		}
	}

	checked := 0
	for _, name := range names {
		if _, exempt := forceRLSExempt[name]; exempt {
			continue
		}
		raw, err := fs.ReadFile(db.Migrations, "migrations/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		checkFileRespectsForceRLS(t, name, string(raw), forceRLSTables)
	}
	if checked == 0 {
		t.Fatal("every migration file was exempted — the non-exempt code path never ran")
	}
}

type forceRLSEvent struct {
	pos   int
	kind  int // 0 = NO FORCE (opens), 1 = FORCE (closes), 2 = DML statement
	table string
	stmt  string // human-readable statement description, kind == 2 only
}

func checkFileRespectsForceRLS(t *testing.T, name, content string, forceRLSTables map[string]bool) {
	t.Helper()
	clean := stripNonMigrationText(content)

	var events []forceRLSEvent
	for _, m := range reNoForceStmt.FindAllStringSubmatchIndex(clean, -1) {
		events = append(events, forceRLSEvent{pos: m[0], kind: 0, table: strings.ToLower(clean[m[2]:m[3]])})
	}
	for _, m := range reForceStmt.FindAllStringSubmatchIndex(clean, -1) {
		events = append(events, forceRLSEvent{pos: m[0], kind: 1, table: strings.ToLower(clean[m[2]:m[3]])})
	}
	dmlPatterns := []struct {
		re   *regexp.Regexp
		verb string
	}{
		{reUpdateStmt, "UPDATE"},
		{reInsertStmt, "INSERT INTO"},
		{reDeleteStmt, "DELETE FROM"},
	}
	for _, d := range dmlPatterns {
		for _, m := range d.re.FindAllStringSubmatchIndex(clean, -1) {
			table := strings.ToLower(clean[m[2]:m[3]])
			if !forceRLSTables[table] {
				continue
			}
			stmtEnd := strings.IndexByte(clean[m[0]:], ';')
			stmt := clean[m[0]:]
			if stmtEnd >= 0 {
				stmt = clean[m[0] : m[0]+stmtEnd+1]
			}
			stmt = strings.Join(strings.Fields(stmt), " ") // collapse whitespace/newlines for the message
			if len(stmt) > 160 {
				stmt = stmt[:160] + "..."
			}
			events = append(events, forceRLSEvent{
				pos: m[0], kind: 2, table: table,
				stmt: fmt.Sprintf("%s app.%s: %s", d.verb, table, stmt),
			})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	unforced := map[string]bool{}
	for _, e := range events {
		switch e.kind {
		case 0:
			unforced[e.table] = true
		case 1:
			unforced[e.table] = false
		case 2:
			if !unforced[e.table] {
				t.Errorf(
					"%s:%d: %s runs while app.%s still has FORCE ROW LEVEL SECURITY on. "+
						"Migrations run as mailroom_owner (no BYPASSRLS, no policy of its own): "+
						"under FORCE this statement matches ZERO rows on every real database, "+
						"silently (mailroom#505). Fix — wrap it in this same file:\n"+
						"  ALTER TABLE app.%s NO FORCE ROW LEVEL SECURITY;\n"+
						"  <the statement above>\n"+
						"  ALTER TABLE app.%s FORCE ROW LEVEL SECURITY;\n"+
						"(see db/migrations/00051_backfills_under_force_rls.sql and "+
						"docs/migrations.md \"Backfilling a FORCE-RLS table from a migration\").",
					name, lineAt(clean, e.pos), e.stmt, e.table, e.table, e.table)
			}
		}
	}
}
