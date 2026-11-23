// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// WHY LEARNED PLACEMENT HELD BACK, PER DAY (migration 00100). The durable tally
// is checked against the REAL ingest path and the same arrival cases the
// counter and placement-explain are held to (lpCases): one row-count of one for
// the reason the live decision produced, nothing at all for a placement.

import (
	"context"
	"testing"
	"time"

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
