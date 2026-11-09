// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// No database: the SQL's shape. Per-branch cursor conditions must stay index
// conditions built from the branch's own constant kind (ADR-053).
var userWhere = regexp.MustCompile(`\n\s*WHERE [a-z]+\.user_id = \$1`)

func TestActivityBranchCursorShape(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	f := ActivityFilter{After: &ActivityCursor{At: at, Kind: ActivityKindPurge, ID: "42"}}
	cases := map[string]string{
		ActivityKindAITool:     "e.at <=",                // sorts before purge: ties on the timestamp still follow
		ActivityKindSignIn:     "s.created_at <",         // sorts after purge
		ActivityKindPurge:      "(a.created_at, a.id) <", // the cursor's own kind: full tuple
		ActivityKindDecryption: "d.decrypted_at <=",
	}
	for kind, want := range cases {
		b := &actBuilder{args: []any{"u"}}
		sql := b.activityBranch(kind, f, "51")
		if !strings.Contains(sql, want) {
			t.Errorf("%s: branch lacks %q:\n%s", kind, want, sql)
		}
		// The branch's own WHERE (at a line start: the EXISTS inside a select list
		// has its own user predicate and must not stand in for it).
		if !userWhere.MatchString(sql) {
			t.Errorf("%s: branch has no user predicate of its own:\n%s", kind, sql)
		}
		if !strings.Contains(sql, "LIMIT 51") {
			t.Errorf("%s: branch is not bounded:\n%s", kind, sql)
		}
	}
}

func TestActivityKindsFor(t *testing.T) {
	all, err := kindsFor(nil)
	if err != nil || len(all) != len(kindSource) {
		t.Fatalf("no filter: %v, %v", all, err)
	}
	two, _ := kindsFor([]string{ActivitySourceSession})
	if len(two) != 2 || two[0] != ActivityKindSignIn || two[1] != ActivityKindSignOut {
		t.Errorf("session kinds = %v", two)
	}
	if _, err := kindsFor([]string{"x"}); err == nil {
		t.Error("an unknown source was accepted")
	}
	// Every kind has a branch: a kind that returns "" would silently vanish.
	for k := range kindSource {
		b := &actBuilder{args: []any{"u"}}
		if b.activityBranch(k, ActivityFilter{}, "2") == "" {
			t.Errorf("kind %s has no branch", k)
		}
	}
}

// A cursor is client input: a forged one is refused as bad arguments, never
// passed to Postgres (a year-300000 time was a serverFail with the SQLSTATE).
func TestParseActivityCursorBounds(t *testing.T) {
	ok := ActivityCursor{At: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Kind: ActivityKindPurge, ID: "00000000000000000042"}
	if _, err := ParseActivityCursor(ok.Encode()); err != nil {
		t.Fatalf("a real cursor was refused: %v", err)
	}
	for name, c := range map[string]ActivityCursor{
		"year 300000": {At: time.Date(300000, 1, 1, 0, 0, 0, 0, time.UTC), Kind: ActivityKindPurge, ID: "1"},
		"before 1970": {At: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), Kind: ActivityKindPurge, ID: "1"},
		"long id":     {At: ok.At, Kind: ActivityKindAITool, ID: strings.Repeat("A", 1<<20)},
	} {
		if _, err := ParseActivityCursor(c.Encode()); !errors.Is(err, ErrActivityArgs) {
			t.Errorf("%s: %v, want ErrActivityArgs", name, err)
		}
	}
}
