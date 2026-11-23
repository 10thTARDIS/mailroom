// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// WHY LEARNED PLACEMENT HELD BACK, PER DAY (migration 00100). The durable tally
// is checked against the REAL ingest path and the same arrival cases the
// counter and placement-explain are held to (lpCases): one row-count of one for
// the reason the live decision produced, nothing at all for a placement.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// lpTally reads the whole tally for the fixture's user as reason -> n.
func (f *fixture) lpTally(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.super.Query(context.Background(),
		`SELECT reason, n FROM app.learned_placement_abstentions WHERE user_id = $1`, f.userID.String())
	if err != nil {
		t.Fatalf("read tally: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var r string
		var n int
		if err := rows.Scan(&r, &n); err != nil {
			t.Fatal(err)
		}
		out[r] = n
	}
	return out
}

// TestLearnedAbstentionRecordedPerReason: for every reason a real arrival can
// have, the tally gains exactly one count under that reason, and a placement
// gains none.
//
// NEGATIVE CONTROL: with one reason excluded from store.LearnedAbstentionReason
// (see the PR: "unauthenticated" taken out) that case, and only that case, goes
// red with "tally = map[]".
func TestLearnedAbstentionRecordedPerReason(t *testing.T) {
	for _, c := range lpCases() {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			rec := &lpRecorder{}
			f.lpObserve(t, rec, c.trusted...)
			areas := f.lpAreas(t)
			if c.setup != nil {
				c.setup(t, f, areas)
			}
			for _, w := range c.filed {
				f.lpWantFilings(t, w.from, areas[w.area], w.n)
			}
			// Only THE arrival is counted below: the setup's own ingests were
			// arrivals too.
			if _, err := f.super.Exec(context.Background(),
				`DELETE FROM app.learned_placement_abstentions WHERE user_id = $1`, f.userID.String()); err != nil {
				t.Fatal(err)
			}
			rec.reset()
			c.arrive(t, f)
			if live := rec.all(); len(live) != 1 || live[0] != c.want {
				t.Fatalf("fixture: the live arrival reported %v, want [%s]", live, c.want)
			}
			got := f.lpTally(t)
			if c.want == "placed" {
				if len(got) != 0 {
					t.Errorf("a placement is not an abstention, but the tally = %v", got)
				}
				return
			}
			if len(got) != 1 || got[c.want] != 1 {
				t.Errorf("tally = %v, want exactly {%s: 1}", got, c.want)
			}
		})
	}
}

// TestLearnedAbstentionAccumulatesAndReadsBack: repeated arrivals add to the
// day's row (an upsert, not a row each), and AbstentionTally reads them back
// inside the window with the day the recording starts.
func TestLearnedAbstentionAccumulatesAndReadsBack(t *testing.T) {
	f := newFixture(t)
	f.lpObserve(t, &lpRecorder{})
	for i := 0; i < 3; i++ {
		f.lpIngest(t, "stranger@example.com") // no evidence: "none"
	}
	if got := f.lpTally(t)["none"]; got != 3 {
		t.Fatalf("tally[none] = %d after three arrivals, want 3", got)
	}
	if rows := f.scalar(t, `SELECT count(*) FROM app.learned_placement_abstentions WHERE user_id = $1`, f.userID.String()); rows != 1 {
		t.Errorf("rows = %d, want ONE row for the day and reason", rows)
	}
	// An older day, outside a 30-day window, must not be counted in it.
	if _, err := f.super.Exec(context.Background(), `
		INSERT INTO app.learned_placement_abstentions (user_id, day, reason, n)
		VALUES ($1, current_date - 40, 'none', 7), ($1, current_date - 5, 'thin', 2)`, f.userID.String()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var tally store.AbstentionTally
	if err := f.st.UserReadTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		tally, e = tx.AbstentionTally(ctx, 30, time.Now())
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if tally.ByReason["none"] != 3 || tally.ByReason["thin"] != 2 || tally.Total() != 5 {
		t.Errorf("30-day tally = %v, want none 3 + thin 2 (the 40-day-old 7 is outside)", tally.ByReason)
	}
	if want := time.Now().UTC().AddDate(0, 0, -40).Format("2006-01-02"); tally.FirstRecorded.Format("2006-01-02") != want {
		t.Errorf("first recorded = %s, want %s", tally.FirstRecorded.Format("2006-01-02"), want)
	}
	if tally.DaysCovered != 30 {
		t.Errorf("days covered = %d, want the whole 30-day window", tally.DaysCovered)
	}
}

// Mail synced from a Sent or Drafts mailbox goes through learned placement (it
// has no role gate) but is not an ARRIVAL: it is the user's own mail, held back
// for want of evidence about their own address, and it must not be tallied as
// "learned placement could not file this for you". The same message from the
// inbox is tallied (positive control).
//
// NEGATIVE CONTROL: with the role check removed, the Sent and Drafts rows appear
// in the tally and this test goes red.
func TestLearnedAbstentionSkipsSentAndDrafts(t *testing.T) {
	f := newFixture(t)
	f.lpObserve(t, &lpRecorder{})
	ctx := context.Background()
	mailbox := func(name, role string) ulid.ULID {
		id := ulid.Make()
		if _, err := f.super.Exec(ctx, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
			VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''))`, id.String(), f.userID.String(), f.accountID.String(), name, role); err != nil {
			t.Fatal(err)
		}
		return id
	}
	sent, drafts := mailbox("Sent", `\Sent`), mailbox("Drafts", `\Drafts`)
	ingestTo := func(box ulid.ULID, tag string) {
		uid := lpUID.Add(1)
		if _, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
			rawFrom("me@example.com", tag, fmt.Sprintf("%s-%d@example.com", tag, uid)), f.prov(box, uid, nil)); err != nil {
			t.Fatal(err)
		}
	}
	ingestTo(sent, "sent-copy")
	ingestTo(drafts, "a-draft")
	if got := f.lpTally(t); len(got) != 0 {
		t.Errorf("Sent and Drafts mail was tallied as held back: %v", got)
	}
	ingestTo(f.mailboxA, "inbox-copy")
	if got := f.lpTally(t); got["none"] != 1 {
		t.Errorf("the same mail in the INBOX is an arrival and must be tallied: %v", got)
	}
}

// PlacedRecent is the placements of the same window, so "held back" reads
// against what learned placement did place; a backfill's are left out.
func TestPlacementMeasurePlacedCount(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpIngest(t, "alice@example.com") // placed by learned placement
	f.lpIngest(t, "alice@example.com") // and again
	ctx := context.Background()
	var m store.PlacementMeasure
	if err := f.st.UserReadTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		m, e = tx.PlacementMeasure(ctx, store.PlacementMeasureOptions{Now: time.Now()})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if m.PlacedRecent != 2 {
		t.Errorf("placed in the last 30 days = %d, want 2", m.PlacedRecent)
	}
	// The setup's own arrivals (alice's first two, 'none' and 'thin') were held back.
	if m.Abstentions.ByReason["none"] != 1 || m.Abstentions.ByReason["thin"] != 1 {
		t.Errorf("held back = %v, want none 1 and thin 1 (alice's first two arrivals)", m.Abstentions.ByReason)
	}
}
