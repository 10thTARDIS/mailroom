// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// contact-names-clean (#831): the dry run counts per kind and writes nothing;
// --apply cleans exactly the affected names with one Contact change row each;
// a rerun changes 0.
func TestContactNamesCleanDryRunApplyRerun(t *testing.T) {
	h := newUntangleHarness(t)
	tag := "cnclean-" + strings.ToLower(store.NewULID().String())
	s := newNudgeSeed(t, h, tag)
	other := newNudgeSeed(t, h, tag+"-2")

	ids := map[string]string{}
	var idList []string
	contact := func(seed *nudgeSeed, key, name string) {
		cid := store.NewULID().String()
		ids[key] = cid
		idList = append(idList, cid)
		seed.exec(`INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1,$2,$3)`, cid, seed.user, name)
	}
	contact(s, "bidi", "Pay\u202Epal")
	contact(s, "ctl", "Line\nBreak\u0007")
	contact(s, "zw", "Zero\u200Bwidth\u200D")
	contact(s, "all", "\u202E\u200B") // bidi + format only: becomes empty
	contact(s, "clean", "Alice Example")
	contact(s, "none", "")
	contact(other, "other", "Eve\u202Evil") // another user's: untouched

	ctx := context.Background()
	uid := ulid.MustParse(s.user)
	names := func() map[string]string {
		t.Helper()
		rows, err := h.super.Query(ctx, `SELECT id, display_name FROM app.contacts WHERE id = ANY($1)`, idList)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, n string
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatal(err)
			}
			out[id] = n
		}
		return out
	}
	changeRows := func() map[string]int {
		t.Helper()
		rows, err := h.super.Query(ctx, `SELECT object_id, count(*) FROM app.change_log WHERE object_type = 'Contact' AND user_id = ANY($1) GROUP BY object_id`, []string{s.user, other.user})
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatal(err)
			}
			out[id] = n
		}
		return out
	}
	run := func(apply bool) (*contactNamesPlan, string) {
		t.Helper()
		var out bytes.Buffer
		p, err := cleanContactNames(ctx, &out, h.store, uid, apply, 10, 2)
		if err != nil {
			t.Fatalf("cleanContactNames(apply=%v): %v", apply, err)
		}
		return p, out.String()
	}

	before, beforeRows := names(), changeRows()
	plan, out := run(false)
	if plan.total != 5 || len(plan.fixes) != 4 || plan.bidi != 2 || plan.control != 1 || plan.format != 2 || plan.toEmpty != 1 {
		t.Fatalf("plan total=%d fixes=%d bidi=%d control=%d format=%d empty=%d; want 5/4/2/1/2/1\n%s",
			plan.total, len(plan.fixes), plan.bidi, plan.control, plan.format, plan.toEmpty, out)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "\\u202E") {
		t.Errorf("dry-run output lacks its markers or the escaped example:\n%s", out)
	}
	if after := names(); !equalNames(after, before) {
		t.Fatalf("the DRY RUN changed names:\nbefore %q\nafter  %q", before, after)
	}
	if got := changeRows(); len(got) != len(beforeRows) {
		t.Fatalf("the dry run wrote change rows: %v", got)
	}

	_, out = run(true)
	got := names()
	want := map[string]string{"bidi": "Paypal", "ctl": "LineBreak", "zw": "Zerowidth", "all": "",
		"clean": "Alice Example", "none": "", "other": "Eve\u202Evil"}
	for k, w := range want {
		if got[ids[k]] != w {
			t.Errorf("%s = %q, want %q", k, got[ids[k]], w)
		}
	}
	after := changeRows()
	for k, id := range ids {
		wantRows := 0
		if k == "bidi" || k == "ctl" || k == "zw" || k == "all" {
			wantRows = 1
		}
		if d := after[id] - beforeRows[id]; d != wantRows {
			t.Errorf("%s got %d new change rows, want %d", k, d, wantRows)
		}
	}
	if !strings.Contains(out, "names cleaned:                    4") {
		t.Errorf("apply output:\n%s", out)
	}

	// Rerun: nothing to do, nothing written.
	rowsAfterApply := changeRows()
	plan, out = run(true)
	if len(plan.fixes) != 0 || plan.bidi+plan.control+plan.format != 0 {
		t.Errorf("rerun planned %d fixes\n%s", len(plan.fixes), out)
	}
	if !equalNames(names(), got) {
		t.Errorf("rerun changed names")
	}
	for id, n := range changeRows() {
		if n != rowsAfterApply[id] {
			t.Errorf("rerun wrote a change row for %s", id)
		}
	}
}

func equalNames(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
