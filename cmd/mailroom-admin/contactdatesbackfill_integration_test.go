// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// contact-dates-backfill (#816): the dry run writes nothing; --apply changes
// exactly what the dry run listed (and nothing else), with one Contact change
// row per changed contact; a rerun plans and changes 0. The fixture is the
// report's (contactdatesreport_integration_test.go), plus outbound counts:
//
//	alice  first -10d -> -400d, last -5d -> -100d
//	bob    first/last now -> -20d
//	carol  first/last now -> -200d (sent-to only); outbound 1, NOT saved -> saved
//	dan    within 1 d on both; outbound 1, already saved -> left alone
//	zed    first -100d -> -10d (stored EARLIER); last within
//	erin, frank: no mail found -> left alone
func TestContactDatesBackfillDryRunApplyRerun(t *testing.T) {
	h := newUntangleHarness(t)
	tag := "cdback-" + strings.ToLower(store.NewULID().String())
	s := newNudgeSeed(t, h, tag)
	me := "me-" + tag + "@example.com"
	other := newNudgeSeed(t, h, tag+"-2")

	days := func(n float64) time.Time { return s.now.Add(-time.Duration(n * float64(24*time.Hour))) }
	ids := map[string]string{}
	contact := func(addr string, first, last time.Time, outbound int, saved bool) {
		cid := store.NewULID().String()
		ids[addr] = cid
		s.exec(`INSERT INTO app.contacts (id, user_id, display_name, saved_at) VALUES ($1,$2,'', CASE WHEN $3 THEN now() END)`, cid, s.user, saved)
		s.exec(`INSERT INTO app.contact_addresses (id, user_id, contact_id, address, first_seen_at, last_seen_at, outbound_count)
		        VALUES ($1,$2,$3,$4,$5,$6,$7)`, store.NewULID().String(), s.user, cid, addr, first, last, outbound)
	}
	contact("alice@example.org", days(10), days(5), 0, false)
	contact("bob@example.org", s.now, s.now, 0, false)
	contact("carol@example.org", s.now, s.now, 1, false)
	contact("dan@example.org", days(5), days(5), 1, true)
	// zed's stored last is 2.4 h off the true one: inside the 1 d bucket, so it must
	// be left alone, and a write would be visible.
	contact("zed@example.org", days(100), days(9.9), 0, false)
	contact("erin@example.org", s.now, s.now, 0, false)
	contact("frank@example.org", s.now, s.now, 0, false)

	th := s.thread()
	s.add(nmsg{thread: th, daysAgo: 400, from: "alice@example.org", to: me, subject: "one", body: "a", msgID: "<a1@x>"})
	s.add(nmsg{thread: th, daysAgo: 100, from: "alice@example.org", to: me, subject: "two", body: "b", msgID: "<a2@x>"})
	s.add(nmsg{thread: th, daysAgo: 20, from: "bob@example.org", to: me, subject: "three", body: "c", msgID: "<b1@x>"})
	s.add(nmsg{thread: th, daysAgo: 200, from: me, to: "someone@example.net", cc: "carol@example.org", subject: "four", body: "d", msgID: "<c1@x>"})
	s.add(nmsg{thread: th, daysAgo: 5, from: me, to: "someone@example.net", extraHeaders: "Bcc: dan@example.org\r\n", subject: "five", body: "e", msgID: "<d1@x>"})
	s.add(nmsg{thread: th, daysAgo: 10, from: "zed@example.org", to: me, subject: "six", body: "f", msgID: "<z1@x>"})
	s.add(nmsg{thread: th, daysAgo: 3, from: "frank@example.org", to: me, subject: "seven", body: "g", msgID: "<f1@x>", draft: true})
	other.add(nmsg{thread: other.thread(), daysAgo: 1, from: "alice@example.org", to: "x@example.org", subject: "x", body: "x", msgID: "<o1@x>"})

	ctx := context.Background()
	uid := ulid.MustParse(s.user)
	snapshot := func() string {
		t.Helper()
		rows, err := h.super.Query(ctx, `
			SELECT ca.address::text, ca.first_seen_at, ca.last_seen_at, c.saved_at IS NOT NULL
			  FROM app.contact_addresses ca JOIN app.contacts c ON c.id = ca.contact_id
			 WHERE ca.user_id = $1 ORDER BY ca.address`, s.user)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var (
				a           string
				first, last time.Time
				saved       bool
			)
			if err := rows.Scan(&a, &first, &last, &saved); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %s %s %v\n", a, first.UTC().Format(time.RFC3339Nano), last.UTC().Format(time.RFC3339Nano), saved)
		}
		return b.String()
	}
	changeRows := func() map[string]int {
		t.Helper()
		rows, err := h.super.Query(ctx, `SELECT object_id, count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Contact' GROUP BY object_id`, s.user)
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
	run := func(apply bool) (*contactDatesPlan, string) {
		t.Helper()
		var out bytes.Buffer
		plan, err := backfillContactDates(ctx, &out, h.store, uid, s.reader(), backfillOptions{
			apply: apply, list: true, sample: 3, batch: 2, perTx: 2, now: s.now})
		if err != nil {
			t.Fatalf("backfill(apply=%v): %v", apply, err)
		}
		return plan, out.String()
	}

	before, beforeRows := snapshot(), changeRows()
	plan, out := run(false)
	if after := snapshot(); after != before {
		t.Fatalf("the DRY RUN changed rows:\nbefore\n%s\nafter\n%s", before, after)
	}
	if got := changeRows(); len(got) != len(beforeRows) {
		t.Fatalf("the dry run wrote change rows: %v", got)
	}
	if plan.firstFixes != 4 || plan.lastFixes != 3 || len(plan.save) != 1 || len(plan.contacts) != 4 || plan.noMail != 2 || plan.withinBoth != 1 {
		t.Fatalf("plan first=%d last=%d save=%d contacts=%d noMail=%d within=%d; want 4/3/1/4/2/1\n%s",
			plan.firstFixes, plan.lastFixes, len(plan.save), len(plan.contacts), plan.noMail, plan.withinBoth, out)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "would save 1 contacts") || !strings.Contains(out, "within 1 d:") {
		t.Errorf("dry-run output lacks its markers:\n%s", out)
	}
	// The plan's buckets are the report's non-"within" buckets.
	if plan.first.later[bucketOverYear] != 1 || plan.first.later[bucketDay30] != 1 || plan.first.later[bucketDay365] != 1 || plan.first.earlier[bucketDay365] != 1 {
		t.Errorf("first buckets = %+v", plan.first)
	}

	// --apply: exactly the planned addresses move, to the true dates.
	_, out = run(true)
	want := map[string][2]float64{ // address -> true first/last days ago (-1: unchanged)
		"alice@example.org": {400, 100},
		"bob@example.org":   {20, 20},
		"carol@example.org": {200, 200},
		"zed@example.org":   {10, -1},
	}
	beforeLines := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(before), "\n") {
		beforeLines[strings.Fields(l)[0]] = l
	}
	for _, l := range strings.Split(strings.TrimSpace(snapshot()), "\n") {
		f := strings.Fields(l)
		addr := f[0]
		w, planned := want[addr]
		if !planned {
			if addr == "carol@example.org" {
				continue
			}
			if l != beforeLines[addr] {
				t.Errorf("%s changed though not planned:\n  %s\n  %s", addr, beforeLines[addr], l)
			}
			continue
		}
		first, _ := time.Parse(time.RFC3339Nano, f[1])
		last, _ := time.Parse(time.RFC3339Nano, f[2])
		if d := first.Sub(days(w[0])); d > time.Second || d < -time.Second {
			t.Errorf("%s first = %s, want %v days ago", addr, first, w[0])
		}
		if w[1] >= 0 {
			if d := last.Sub(days(w[1])); d > time.Second || d < -time.Second {
				t.Errorf("%s last = %s, want %v days ago", addr, last, w[1])
			}
		} else if f[2] != strings.Fields(beforeLines[addr])[2] {
			t.Errorf("%s last moved though within 1 d", addr)
		}
		if addr == "carol@example.org" && f[3] != "true" {
			t.Errorf("carol was not saved")
		}
	}
	if !strings.Contains(snapshot(), "carol@example.org") || !strings.Contains(out, "saved:                          1 contacts") {
		t.Errorf("apply output:\n%s", out)
	}
	afterRows := changeRows()
	var changed []string
	for id, n := range afterRows {
		if n != beforeRows[id] {
			changed = append(changed, id)
			if n-beforeRows[id] != 1 {
				t.Errorf("contact %s got %d change rows, want 1", id, n-beforeRows[id])
			}
		}
	}
	sort.Strings(changed)
	wantChanged := []string{ids["alice@example.org"], ids["bob@example.org"], ids["carol@example.org"], ids["zed@example.org"]}
	sort.Strings(wantChanged)
	if strings.Join(changed, ",") != strings.Join(wantChanged, ",") {
		t.Errorf("change rows for %v, want exactly %v", changed, wantChanged)
	}

	// Rerun: nothing planned, nothing written.
	snap := snapshot()
	plan, out = run(true)
	if plan.firstFixes != 0 || plan.lastFixes != 0 || len(plan.save) != 0 {
		t.Errorf("rerun planned changes:\n%s", out)
	}
	if snapshot() != snap || len(changeRows()) != len(afterRows) {
		t.Error("the rerun wrote something")
	}
	for id, n := range changeRows() {
		if n != afterRows[id] {
			t.Errorf("rerun wrote a change row for %s", id)
		}
	}
}

// A fix computed against dates that moved before the write is skipped and
// counted, never forced.
func TestContactDatesBackfillSkipsStale(t *testing.T) {
	h := newUntangleHarness(t)
	tag := "cdstale-" + strings.ToLower(store.NewULID().String())
	s := newNudgeSeed(t, h, tag)
	me := "me-" + tag + "@example.com"
	cid := store.NewULID().String()
	s.exec(`INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1,$2,'')`, cid, s.user)
	s.exec(`INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1,$2,$3,'old@example.org')`,
		store.NewULID().String(), s.user, cid)
	s.add(nmsg{thread: s.thread(), daysAgo: 50, from: "old@example.org", to: me, subject: "s", body: "b", msgID: "<s1@x>"})
	ctx := context.Background()
	uid := ulid.MustParse(s.user)
	in, err := gatherContactDates(ctx, h.store, uid, s.reader(), 10)
	if err != nil {
		t.Fatal(err)
	}
	plan := planContactDates(in, nil)
	if len(plan.fixes) != 1 {
		t.Fatalf("plan = %+v", plan.fixes)
	}
	// Ingest re-sights the address between the read and the write.
	s.exec(`UPDATE app.contact_addresses SET last_seen_at = now() + interval '1 second', seen_count = seen_count + 1 WHERE contact_id = $1`, cid)
	res, err := applyContactDatesPlan(ctx, h.store, uid, plan, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stale != 1 || res.FirstFixed != 0 || res.Contacts != 0 {
		t.Errorf("stale fix: %+v, want 1 stale and nothing written", res)
	}
}
