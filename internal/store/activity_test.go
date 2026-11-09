// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
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
