// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// `mailroom-admin placement-measure` (#28, #169) against REAL ingested mail.
//
// Messages come from the real ingest path (so From, List-Id and the desk's own
// label are what ingest writes), then their labels, flags, feedback events and
// dates are set to a hand-drawn history, because a measurement over time needs a
// history and a test cannot wait for one. Each test builds its own user, so
// the numbers asserted are exactly the ones drawn in its fixture, worked out by
// hand in the comments beside it.
//
// Learned placement is switched OFF while a fixture is built: a placement made
// by the feature under measurement would be a label the fixture did not draw.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

type mmWorld struct {
	f     *fixture
	areas map[string]store.AreaView
	now   time.Time
}

func newMMWorld(t *testing.T, trusted ...string) *mmWorld {
	t.Helper()
	f := newFixture(t)
	f.lpObserve(t, &lpRecorder{}, trusted...)
	w := &mmWorld{f: f, areas: f.lpAreas(t), now: time.Now().UTC().Truncate(time.Second)}
	f.lpSwitch(t, false)
	return w
}

// ago is a time d days before the world's now.
func (w *mmWorld) ago(d float64) time.Time {
	return w.now.Add(-time.Duration(d * 24 * float64(time.Hour)))
}

func (w *mmWorld) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := w.f.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture: %v\n%s", err, sql)
	}
}

// ingest arrives a real message from `from` and dates it created days ago.
func (w *mmWorld) ingest(t *testing.T, from string, createdAgo float64, extra ...string) ulid.ULID {
	t.Helper()
	id := w.f.lpIngest(t, from, extra...)
	at := w.ago(createdAgo)
	w.exec(t, `UPDATE app.messages SET created_at = $2, received_at = $2 WHERE id = $1`, id.String(), at)
	w.exec(t, `UPDATE app.message_labels SET created_at = $2 WHERE message_id = $1`, id.String(), at)
	return id
}

// place makes the message carry exactly one area own-label: area, applied by
// `by`, dated at. It deletes whatever area own-labels the message arrived with
// (the desk's, from real ingest) and writes the one asked for; it asserts
// nothing about the label it replaced.
func (w *mmWorld) place(t *testing.T, id ulid.ULID, area, by string, at time.Time) {
	t.Helper()
	var all []string
	for _, a := range w.areas {
		all = append(all, a.LabelID.String())
	}
	w.exec(t, `DELETE FROM app.message_labels WHERE message_id = $1 AND label_id::text = ANY($2)`, id.String(), all)
	w.exec(t, `INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, created_at)
	           VALUES ($1, $2, $3, $4, $5)`, w.f.userID.String(), id.String(), w.areas[area].LabelID.String(), by, at)
}

func (w *mmWorld) unlabel(t *testing.T, id ulid.ULID) {
	t.Helper()
	var all []string
	for _, a := range w.areas {
		all = append(all, a.LabelID.String())
	}
	w.exec(t, `DELETE FROM app.message_labels WHERE message_id = $1 AND label_id::text = ANY($2)`, id.String(), all)
}

// desk arrives a message and leaves it with the desk's own placement into area
// at its arrival, as ingest would have put it there.
func (w *mmWorld) desk(t *testing.T, from, area string, createdAgo float64, extra ...string) ulid.ULID {
	t.Helper()
	id := w.ingest(t, from, createdAgo, extra...)
	w.place(t, id, area, store.LabelByDesk, w.ago(createdAgo))
	return id
}

// file arrives a message and records the user's own filing of it into area, at
// the given time (the message arrives an hour earlier unless stated).
func (w *mmWorld) file(t *testing.T, from, area string, filedAgo float64, extra ...string) ulid.ULID {
	t.Helper()
	id := w.ingest(t, from, filedAgo+1.0/24, extra...)
	w.place(t, id, area, store.LabelByUser, w.ago(filedAgo))
	return id
}

func (w *mmWorld) event(t *testing.T, id ulid.ULID, action string, at time.Time) {
	t.Helper()
	w.exec(t, `INSERT INTO app.feedback_events (id, user_id, message_id, action, created_at) VALUES ($1, $2, $3, $4, $5)`,
		ulid.Make().String(), w.f.userID.String(), id.String(), action, at)
}

// opened records an `open` event an hour after at.
func (w *mmWorld) opened(t *testing.T, id ulid.ULID, at time.Time) {
	t.Helper()
	w.event(t, id, "open", at.Add(time.Hour))
}

func (w *mmWorld) measure(t *testing.T, mutate func(*store.PlacementMeasureOptions)) store.PlacementMeasure {
	t.Helper()
	ctx := context.Background()
	opts := store.PlacementMeasureOptions{Now: w.now, SplitDays: 45, WindowDays: 365, RecentDays: 30}
	if mutate != nil {
		mutate(&opts)
	}
	var m store.PlacementMeasure
	if err := w.f.st.UserReadTx(ctx, w.f.userID, func(tx *store.UserTx) error {
		var e error
		m, e = tx.PlacementMeasure(ctx, opts)
		return e
	}); err != nil {
		t.Fatalf("placement measure: %v", err)
	}
	return m
}

func mmNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > 0.0006 {
		t.Errorf("%s = %.4f, want %.4f", name, got, want)
	}
}

func (w *mmWorld) wantFilings(t *testing.T, from, area string, n int) {
	t.Helper()
	w.f.lpWantFilings(t, from, w.areas[area], n)
}

// ── section 4 ────────────────────────────────────────────────────────────────

// TestPlacementMeasureLeftAlone draws a mailbox in which the user opens and
// leaves some of the desk's placements, files others by hand, and ignores the
// rest, and checks every number of section 4 against the hand count.
//
// THE HISTORY (D = 45 days ago; "U" Updates, "R" Promotions, "P" Primary):
//
//	evidence, before D
//	  a@shop    3 desk U (-70,-69,-68)   opened: first two only
//	  b@news    2 desk R (-66,-65)       both opened
//	  c@thin    1 desk U (-64)           opened
//	  d@mixed   2 desk U + 1 desk R      all opened
//	  e@known   2 desk U (-60,-59)       opened; plus two filings by the user
//	                                     into U (-85,-84): the user already
//	                                     covers this sender
//	  j@quiet   2 desk U (-66,-65)       NEVER opened
//	  g@late    1 message (-60) the user filed into U at -40: moved after the
//	            7 days, so the desk label that named its area is gone
//	  h@early   1 message (-60) filed into U at -59: moved at once
//	  i@recent  1 desk U (-48) opened: placed < 7 days before D, so not judged
//	  k@mid     1 message (-60) filed into U at -50: moved late but BEFORE D
//	  lrn, rule 1 message each, placed by LEARNED placement and by a RULE (-69,
//	            -68), both opened: not desk placements, so never events
//	  m@bulk    1 message received 3 days before it was created (an import)
//
//	outcomes, on or after D, 7+ days old
//	  o1  a desk U            o2  a user-filed R      o3  a user-filed U
//	  o4  b desk R            o5  b user-filed P
//	  o6  j desk U            o7  j user-filed R
//	  o8  e desk U            o9  z desk P            o10 z user-filed U
//	  o11 a rule's label      o12 a learned placement's label
//	  o13 a, 3 days old       o14 a, trashed          (neither is an outcome)
//	  o15 a, filed into R by an API KEY (agent)       a filing
//	  o16 a, no area label at all                    label removed
//	  o17 a, desk label R (the desk put it elsewhere)
//	  o18 day-one setup's label                       excluded
//
// Hand count, "opened and not moved": the keys taught are a (U), b (R) and e (U,
// but the user already covers e); c is thin, d mixed. New reach is a and b.
// Later mail from a (taught U): o1 AGREE (desk U, untouched), o3 KEPT (filed
// into the TAUGHT area), o2 and o15 CORRECTED (filed elsewhere, one by a key),
// o16 CORRECTED by label removal, o17 UNRESOLVED (desk put it in R). So U: kept
// 1, corrected 3, agree 1, unresolved 1, precision 1/4.
// From b (taught R): o4 AGREE, o5 CORRECTED. R: kept 0, corrected 1, agree 1.
// j is untaught (never opened), so o6 and o7 are base rate, as are z's o9 and
// o10; o8 is covered by the user's evidence. Totals: kept 1, corrected 4 (1 label
// removed), agree 2, unresolved 1, precision 1/5.
//
// NEGATIVE CONTROLS, run and recorded in the PR: with the split made to leak
// (outcomes read from before D) the precision rows go red; with the "opened"
// condition dropped the opened variant's key and event counts go red.
func TestPlacementMeasureLeftAlone(t *testing.T) {
	w := newMMWorld(t)
	t.Cleanup(func() { w.f.super.Close() })
	deskOpened := func(from, area string, ago float64) ulid.ULID {
		id := w.desk(t, from, area, ago)
		w.opened(t, id, w.ago(ago))
		return id
	}
	deskOpened("a@shop.example", "Updates", 70)
	deskOpened("a@shop.example", "Updates", 69)
	w.desk(t, "a@shop.example", "Updates", 68)
	deskOpened("b@news.example", "Promotions", 66)
	deskOpened("b@news.example", "Promotions", 65)
	deskOpened("c@thin.example", "Updates", 64)
	deskOpened("d@mixed.example", "Updates", 63)
	deskOpened("d@mixed.example", "Updates", 62)
	deskOpened("d@mixed.example", "Promotions", 61)
	deskOpened("e@known.example", "Updates", 60)
	deskOpened("e@known.example", "Updates", 59)
	w.file(t, "e@known.example", "Updates", 85)
	w.file(t, "e@known.example", "Updates", 84)
	w.desk(t, "j@quiet.example", "Updates", 66)
	w.desk(t, "j@quiet.example", "Updates", 65)
	g := w.ingest(t, "g@late.example", 60)
	w.place(t, g, "Updates", store.LabelByUser, w.ago(40))
	h := w.ingest(t, "h@early.example", 60)
	w.place(t, h, "Updates", store.LabelByUser, w.ago(59))
	deskOpened("i@recent.example", "Updates", 48)
	k := w.ingest(t, "k@mid.example", 60)
	w.place(t, k, "Updates", store.LabelByUser, w.ago(50))
	// Not desk placements, whatever else is true of them: a learned placement and
	// a rule's label, both opened, before D. They must never become events.
	lrn := w.ingest(t, "lrn@learned.example", 69)
	w.place(t, lrn, "Updates", store.LabelByLearned, w.ago(69))
	w.opened(t, lrn, w.ago(69))
	rl := w.ingest(t, "rl@rule.example", 68)
	w.place(t, rl, "Forums", store.LabelByRule, w.ago(68))
	w.opened(t, rl, w.ago(68))
	bulk := w.ingest(t, "m@bulk.example", 100)
	w.place(t, bulk, "Forums", store.LabelByRule, w.ago(100))
	w.exec(t, `UPDATE app.messages SET received_at = created_at - interval '3 days' WHERE id = $1`, bulk.String())

	w.desk(t, "a@shop.example", "Updates", 31)                   // o1
	o2 := w.ingest(t, "a@shop.example", 29)                      // o2
	w.place(t, o2, "Promotions", store.LabelByUser, w.ago(28))   //
	o3 := w.ingest(t, "a@shop.example", 28)                      // o3
	w.place(t, o3, "Updates", store.LabelByUser, w.ago(27))      //
	w.desk(t, "b@news.example", "Promotions", 25)                // o4
	o5 := w.ingest(t, "b@news.example", 24)                      // o5
	w.place(t, o5, "Primary", store.LabelByUser, w.ago(23))      //
	w.desk(t, "j@quiet.example", "Updates", 20)                  // o6
	o7 := w.ingest(t, "j@quiet.example", 19)                     // o7
	w.place(t, o7, "Promotions", store.LabelByUser, w.ago(18))   //
	w.desk(t, "e@known.example", "Updates", 18)                  // o8
	w.desk(t, "z@other.example", "Primary", 17)                  // o9
	o10 := w.ingest(t, "z@other.example", 16)                    // o10
	w.place(t, o10, "Updates", store.LabelByUser, w.ago(15))     //
	o11 := w.ingest(t, "r@rule.example", 15)                     // o11
	w.place(t, o11, "Forums", store.LabelByRule, w.ago(15))      //
	o12 := w.ingest(t, "l@learned.example", 14)                  // o12
	w.place(t, o12, "Updates", store.LabelByLearned, w.ago(14))  //
	o15 := w.ingest(t, "a@shop.example", 21)                     // o15
	w.place(t, o15, "Promotions", store.LabelByAgent, w.ago(20)) //
	o16 := w.ingest(t, "a@shop.example", 19)                     // o16
	w.unlabel(t, o16)                                            //
	w.desk(t, "a@shop.example", "Promotions", 18)                // o17
	o18 := w.ingest(t, "s@setup.example", 17)                    // o18
	w.place(t, o18, "Updates", store.LabelBySetup, w.ago(17))    //
	w.desk(t, "a@shop.example", "Updates", 3)                    // o13
	o14 := w.desk(t, "a@shop.example", "Updates", 22)            // o14
	w.exec(t, `UPDATE app.messages SET trashed_at = $2::timestamptz, purge_after = $2::timestamptz + interval '30 days' WHERE id = $1`, o14.String(), w.ago(21))

	// The fixture's own filings, through the evidence query the decision uses.
	w.wantFilings(t, "e@known.example", "Updates", 2)
	w.wantFilings(t, "a@shop.example", "Promotions", 1)
	w.wantFilings(t, "a@shop.example", "Updates", 1)

	m := w.measure(t, nil)
	u, r := w.areas["Updates"].ID, w.areas["Promotions"].ID
	if want := w.now.AddDate(0, 0, -45); !m.Split.Equal(want) {
		t.Errorf("split day D = %s, want %s", m.Split, want)
	}
	c := m.LeftAloneCover
	// 13 desk placements, 10 opened. The learned-placed and rule-placed messages
	// are in neither: a learned or rule label is not the desk's own verdict.
	if c.DeskPlaced != 13 || c.Opened != 10 {
		t.Errorf("desk-placed/opened = %d/%d, want 13/10 (a3 and j1, j2 unopened; i1 too young; the learned and rule messages are not desk placements)", c.DeskPlaced, c.Opened)
	}
	if c.UnobservableMoved != 2 || c.UnobservableAfterD != 1 {
		t.Errorf("unobservable moved/after D = %d/%d, want 2/1 (g filed at -40, after D; k at -50, before it; h was filed at once)", c.UnobservableMoved, c.UnobservableAfterD)
	}
	if c.Outcomes != 13 || c.ExcludedRule != 1 || c.ExcludedLearned != 1 || c.ExcludedOther != 1 {
		t.Errorf("outcomes/rule/learned/other = %d/%d/%d/%d, want 13/1/1/1 (o13 is too young, o14 is trashed)", c.Outcomes, c.ExcludedRule, c.ExcludedLearned, c.ExcludedOther)
	}
	if m.UserEvidence != 4 {
		t.Errorf("user evidence before D = %d, want 4 (e's two, h's one, k's one; g's was after D)", m.UserEvidence)
	}
	if want := w.ago(70).Add(time.Hour); !c.EarliestOpen.Equal(want) {
		t.Errorf("earliest open = %s, want %s", c.EarliestOpen, want)
	}
	if total := w.f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id = $1`, w.f.userID.String()); m.WindowMessages != total || m.BulkImported != 1 {
		t.Errorf("window messages/bulk imported = %d/%d, want %d/1 (m@bulk was received 3 days before it was created)", m.WindowMessages, m.BulkImported, total)
	}

	op := m.LeftAlone
	if op.Events != 10 || op.EventsByArea[u] != 7 || op.EventsByArea[r] != 3 {
		t.Errorf("opened: events %d (U %d, R %d), want 10 (U 7, R 3)", op.Events, op.EventsByArea[u], op.EventsByArea[r])
	}
	// A single filing acts for an address now (ADR-032 amendment), so the one
	// thin address (j's one event) is confident too.
	if got := op.Keys.Confident[u]; got != (placement.KeyCounts{Addresses: 3}) {
		t.Errorf("opened: confident U keys = %+v, want addresses a, e and j", got)
	}
	if got := op.Keys.New[u]; got != (placement.KeyCounts{Addresses: 2}) || op.Keys.New[r] != (placement.KeyCounts{Addresses: 1}) {
		t.Errorf("opened: new keys U %+v R %+v, want U a and j, R one (e is covered by the user)", got, op.Keys.New[r])
	}
	if v := op.Keys.Reasons[placement.KeyAddress]; v[placement.Confident] != 4 || v[placement.Thin] != 0 || v[placement.Mixed] != 1 {
		t.Errorf("opened: address verdicts = %v, want confident 4 thin 0 mixed 1", v)
	}
	if got := op.Areas[u]; got.Kept != 1 || got.Corrected != 3 || got.LabelRemoved != 1 || got.Agreement != 1 || got.Unresolved != 1 {
		t.Errorf("opened: Updates = %+v, want kept 1, corrected 3 (1 label removed), agree 1, unresolved 1", got)
	}
	if got := op.Areas[r]; got.Kept != 0 || got.Corrected != 1 || got.Agreement != 1 || got.Unresolved != 0 {
		t.Errorf("opened: Promotions = %+v, want kept 0, corrected 1, agree 1", got)
	}
	mmNear(t, "opened: Updates LCB95", op.Areas[u].Gate.LCB, 0.0456)
	mmNear(t, "opened: Promotions LCB95", op.Areas[r].Gate.LCB, 0)
	mmNear(t, "opened: total precision", op.Total.Gate.Precision, 0.2)
	mmNear(t, "opened: total LCB95", op.Total.Gate.LCB, 0.0362)
	if op.Total.WouldChange() != 1 {
		t.Errorf("opened: would change = %d, want 1 (o17: taught U, the desk put it in R)", op.Total.WouldChange())
	}
	if op.Served != 1 || op.Untaught != 4 || op.MovedAnyTaught != 4 || op.MovedAnyUntaught() != 2 {
		t.Errorf("opened: served/untaught/moved taught/moved untaught = %d/%d/%d/%d, want 1/4/4/2",
			op.Served, op.Untaught, op.MovedAnyTaught, op.MovedAnyUntaught())
	}

	nm := m.LeftAloneAny
	if nm.Events != 13 || nm.EventsByArea[u] != 10 {
		t.Errorf("not moved: events %d (U %d), want 13 (U 10): the opened condition removed 3", nm.Events, nm.EventsByArea[u])
	}
	if got := nm.Keys.New[u]; got != (placement.KeyCounts{Addresses: 3}) {
		t.Errorf("not moved: new U keys = %+v, want 3 (a, j and the address that was thin before a single filing acted)", got)
	}
	// j is taught here: o6 AGREE (desk U), o7 CORRECTED (filed into R).
	if got := nm.Areas[u]; got.Kept != 1 || got.Corrected != 4 || got.LabelRemoved != 1 || got.Agreement != 2 || got.Unresolved != 1 {
		t.Errorf("not moved: Updates = %+v, want kept 1, corrected 4, agree 2, unresolved 1 (j's o6 agrees, o7 is corrected)", got)
	}
	mmNear(t, "not moved: total LCB95", nm.Total.Gate.LCB, 0.0301)
	if nm.Served != 1 || nm.Untaught != 2 || nm.MovedAnyTaught != 5 || nm.MovedAnyUntaught() != 1 {
		t.Errorf("not moved: served/untaught/moved taught/moved untaught = %d/%d/%d/%d, want 1/2/5/1",
			nm.Served, nm.Untaught, nm.MovedAnyTaught, nm.MovedAnyUntaught())
	}
}

// ── section 3 ────────────────────────────────────────────────────────────────

// TestPlacementMeasureRecency: the user's filings, in the order they were made.
//
//	k@flip   U -100, U -99, then R -98 -97 -96 -95 -94   (mixed; ends in a run of 5 R)
//	m@flop   R -100, U -99, U -98                        (mixed; run of 2 U)
//	n@tie    U -95, then R and U at the same instant -90 (mixed; no tail area: run 0)
//	p1@multi U -80 and p2@multi R -79                    (each address pure; the DOMAIN is mixed, run 1)
//	a@shop   R -28, U -27                                (mixed; run 1)
//
// Mail received in the last 30 days: a@shop 3 (-29, -28, -3), k@flip 1 (-2).
func TestPlacementMeasureRecency(t *testing.T) {
	w := newMMWorld(t)
	for i, area := range []string{"Updates", "Updates", "Promotions", "Promotions", "Promotions", "Promotions", "Promotions"} {
		w.file(t, "k@flip.example", area, 100-float64(i))
	}
	w.file(t, "m@flop.example", "Promotions", 100)
	w.file(t, "m@flop.example", "Updates", 99)
	w.file(t, "m@flop.example", "Updates", 98)
	w.file(t, "n@tie.example", "Updates", 95)
	w.file(t, "n@tie.example", "Promotions", 90)
	w.file(t, "n@tie.example", "Updates", 90)
	w.file(t, "p1@multi.example", "Updates", 80)
	w.file(t, "p2@multi.example", "Promotions", 79)
	w.file(t, "a@shop.example", "Promotions", 28)
	w.file(t, "a@shop.example", "Updates", 27)
	w.desk(t, "a@shop.example", "Updates", 3)
	w.desk(t, "k@flip.example", "Updates", 2)
	// Mail from addresses with NO filings of their own: the only mail a DOMAIN key
	// decides (the address key decides the rest).
	w.desk(t, "new@flip.example", "Updates", 4)
	w.desk(t, "new@flip.example", "Updates", 5)
	w.desk(t, "new@shop.example", "Updates", 6)
	w.wantFilings(t, "k@flip.example", "Promotions", 5)

	m := w.measure(t, nil)
	check := func(kind string, r placement.Recency, keys, mixed, mixedRecent int, rows []placement.RecencyRow) {
		if r.Keys != keys || r.Mixed != mixed || r.MixedRecent != mixedRecent {
			t.Errorf("%s: keys/mixed/mixed recent = %d/%d/%d, want %d/%d/%d", kind, r.Keys, r.Mixed, r.MixedRecent, keys, mixed, mixedRecent)
		}
		for i, want := range rows {
			if r.Rows[i] != want {
				t.Errorf("%s: run %d = %+v, want %+v", kind, want.N, r.Rows[i], want)
			}
		}
	}
	// Addresses: k m n p1 p2 a = 6 with filings; mixed k, m, n, a; their recent
	// mail a 3 + k 1 = 4. Runs: k 5, m 2, n 0, a 1.
	check("address", m.RecencyAddress, 6, 4, 4, []placement.RecencyRow{{N: 2, Keys: 2, Recent: 1}, {N: 3, Keys: 1, Recent: 1}, {N: 5, Keys: 1, Recent: 1}})
	// Domains: flip flop tie multi shop = 5, every one mixed (multi's two
	// addresses disagree); runs flip 5, flop 2, tie 0, multi 1, shop 1. Recent
	// mail counts only addresses with no filings of their own: new@flip 2 and
	// new@shop 1 (k@flip's and a@shop's mail belongs to their address keys).
	check("domain", m.RecencyDomain, 5, 5, 3, []placement.RecencyRow{{N: 2, Keys: 2, Recent: 2}, {N: 3, Keys: 1, Recent: 2}, {N: 5, Keys: 1, Recent: 2}})
}

// ── section 5 ────────────────────────────────────────────────────────────────

// TestPlacementMeasureArchivedUnread: senders whose mail the user archives
// unopened, scored on later mail in Primary.
//
//	events, before D (each archived unread)
//	  x@promo 2 in R · y@news2 1 in R + 1 in U (mixed origin) · w@bank 2 in P
//	  v@one 1 in R (thin) · u@nolabel 1 with no area label (dropped, counted)
//	later mail in Primary (7+ days old)
//	  from x: archived unread, opened (event), opened (seen flag), kept, archived with no sign of reading
//	  from w: opened (event)
//	  from y, v and an unknown q: untaught
//	  from x in Updates and from x 3 days old: not outcomes
func TestPlacementMeasureArchivedUnread(t *testing.T) {
	w := newMMWorld(t)
	archivedUnread := func(from, area string, ago float64) {
		id := w.desk(t, from, area, ago)
		w.event(t, id, "archive_unread", w.ago(ago).Add(time.Hour))
		w.exec(t, `UPDATE app.messages SET desk_archived_at = $2 WHERE id = $1`, id.String(), w.ago(ago))
	}
	archivedUnread("x@promo.example", "Promotions", 70)
	archivedUnread("x@promo.example", "Promotions", 69)
	archivedUnread("y@news2.example", "Promotions", 68)
	archivedUnread("y@news2.example", "Updates", 67)
	archivedUnread("w@bank.example", "Primary", 66)
	archivedUnread("w@bank.example", "Primary", 65)
	archivedUnread("v@one.example", "Promotions", 64)
	u := w.desk(t, "u@nolabel.example", "Primary", 63)
	w.event(t, u, "archive_unread", w.ago(63).Add(time.Hour))
	w.unlabel(t, u)

	s1 := w.desk(t, "x@promo.example", "Primary", 30)
	w.event(t, s1, "archive_unread", w.ago(29))
	s2 := w.desk(t, "x@promo.example", "Primary", 29)
	w.opened(t, s2, w.ago(29))
	s3 := w.desk(t, "x@promo.example", "Primary", 28)
	w.exec(t, `UPDATE app.messages SET is_seen = true WHERE id = $1`, s3.String())
	w.desk(t, "x@promo.example", "Primary", 27)
	s5 := w.desk(t, "x@promo.example", "Primary", 26)
	w.exec(t, `UPDATE app.messages SET desk_archived_at = $2 WHERE id = $1`, s5.String(), w.ago(25))
	s6 := w.desk(t, "w@bank.example", "Primary", 20)
	w.opened(t, s6, w.ago(20))
	w.desk(t, "y@news2.example", "Primary", 19)
	w.desk(t, "v@one.example", "Primary", 18)
	w.desk(t, "q@q.example", "Primary", 17)
	w.desk(t, "x@promo.example", "Updates", 16) // desk-placed outside Primary: not an outcome at all
	w.desk(t, "x@promo.example", "Primary", 3)  // too young to judge
	s10 := w.ingest(t, "x@promo.example", 15)   // the user filed it out of Primary
	w.place(t, s10, "Updates", store.LabelByUser, w.ago(14))
	s12 := w.desk(t, "y@news2.example", "Primary", 12) // base rate: an untaught sender's mail, opened
	w.opened(t, s12, w.ago(12))
	q2 := w.ingest(t, "q2@q.example", 13) // base rate: an untaught sender's mail, filed out
	w.place(t, q2, "Updates", store.LabelByUser, w.ago(12))

	m := w.measure(t, nil)
	r, p := w.areas["Promotions"].ID, w.areas["Primary"].ID
	if m.PrimaryName != "Primary" || m.Primary != p {
		t.Fatalf("primary = %s %v, want the area named Primary", m.PrimaryName, m.Primary)
	}
	ar := m.Archived
	if ar.Events != 7 || m.ArchivedCover.EventsNoArea != 1 || m.ArchivedCover.PrimaryOutcomes != 12 {
		t.Errorf("events/no-area/outcomes = %d/%d/%d, want 7/1/12", ar.Events, m.ArchivedCover.EventsNoArea, m.ArchivedCover.PrimaryOutcomes)
	}
	// The BASE-RATE row: y (mixed origin) and q are untaught (v's single filing
	// teaches now, ADR-032 amendment): s7, s9 kept, s12 opened, q2 filed out of
	// Primary = 4 messages (v's s8 moved to the taught side).
	if b := ar.Untaught; b.Messages != 4 || b.KeptUnopened != 2 || b.Opened != 1 || b.RefiledOut != 1 {
		t.Errorf("base-rate row = %+v, want 4: kept 2, opened 1, refiled 1", b)
	}
	mmNear(t, "base-rate strict precision", ar.Untaught.Strict.Precision, 0.25)
	if got := ar.Keys.Reasons[placement.KeyAddress]; got[placement.Confident] != 3 || got[placement.Mixed] != 1 || got[placement.Thin] != 0 {
		t.Errorf("address verdicts = %v, want confident 3 (x, w, v), mixed 1 (y), thin 0", got)
	}
	if ar.Keys.Confident[r] != (placement.KeyCounts{Addresses: 2}) || ar.Keys.Confident[p] != (placement.KeyCounts{Addresses: 1}) {
		t.Errorf("confident keys = %+v, want two addresses of origin R (x, v) and one of origin P", ar.Keys.Confident)
	}
	// x's later mail: s1 archived unread, s2 opened (event), s3 seen with NO event,
	// s4 kept, s5 archived with no sign of reading, s10 re-filed out of Primary.
	pr := ar.Areas[r]
	// v's s8 (kept) is in now: 7 later.
	if pr.Messages != 7 || pr.ArchivedUnread != 1 || pr.Opened != 1 || pr.SeenNoOpen != 1 || pr.KeptUnopened != 2 || pr.ArchivedOther != 1 || pr.RefiledOut != 1 {
		t.Errorf("origin Promotions = %+v, want 7 later: 1 archived unread, 1 opened, 1 seen, 2 kept, 1 other, 1 refiled", pr)
	}
	mmNear(t, "Promotions strict precision", pr.Strict.Precision, 2.0/7) // (1 unread + 1 refiled) of 7
	mmNear(t, "Promotions strict LCB95", pr.Strict.LCB, 0.0822)
	mmNear(t, "Promotions engaged precision", pr.Engaged.Precision, 2.0/3) // 2 of (2 + 1 opened); seen-without-open is not a reading
	mmNear(t, "Promotions engaged LCB95", pr.Engaged.LCB, 0.2077)
	pp := ar.Areas[p]
	if pp.Messages != 1 || pp.Opened != 1 || pp.ArchivedUnread != 0 {
		t.Errorf("origin Primary = %+v, want 1 later, opened", pp)
	}
	mmNear(t, "Primary strict LCB95", pp.Strict.LCB, 0)
}

// ── section 6 ────────────────────────────────────────────────────────────────

// TestPlacementMeasureListID: real ingested mail with real List-Id headers
// (so list_id_key is what ingest stores), the user's filings by hand.
//
//	s1 list a -> U x2, list b -> R x2          a List-Id key makes it clean
//	s2 list a -> U, R; list b -> R x2          cleans b only
//	s3 list a -> U, R (list b exists, unfiled)  helps nothing
//	s4 no List-Id: U, R                        mixed, but no list to separate by
//	s5 list a -> U x2 (list b unfiled)          multi-list, not mixed
//	s6 list a -> U x2, plus a filing whose list_id_key is NULL (not backfilled) -> R
func TestPlacementMeasureListID(t *testing.T) {
	w := newMMWorld(t)
	list := func(id string) string { return "List-Id: Weekly Digest <" + id + ">" }
	fileList := func(from, area, id string, n int) {
		for i := 0; i < n; i++ {
			msg := w.file(t, from, area, 10, list(id))
			var got *string
			if err := w.f.super.QueryRow(context.Background(), `SELECT list_id_key FROM app.messages WHERE id = $1`, msg.String()).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got == nil || *got != id {
				t.Fatalf("fixture: ingest stored list_id_key %v for %q, want it (the real header projection)", got, id)
			}
		}
	}
	fileList("s1@news.example", "Updates", "a.news.example", 2)
	fileList("s1@news.example", "Promotions", "b.news.example", 2)
	fileList("s2@news.example", "Updates", "a.news.example", 1)
	fileList("s2@news.example", "Promotions", "a.news.example", 1)
	fileList("s2@news.example", "Promotions", "b.news.example", 2)
	fileList("s3@news.example", "Updates", "a.news.example", 1)
	fileList("s3@news.example", "Promotions", "a.news.example", 1)
	w.ingest(t, "s3@news.example", 9, list("b.news.example"))
	w.file(t, "s4@plain.example", "Updates", 10)
	w.file(t, "s4@plain.example", "Promotions", 10)
	fileList("s5@news.example", "Updates", "a.news.example", 2)
	w.ingest(t, "s5@news.example", 9, list("b.news.example"))
	fileList("s6@news.example", "Updates", "a.news.example", 2)
	w.ingest(t, "s6@news.example", 9, list("b.news.example"))
	nul := w.file(t, "s6@news.example", "Promotions", 10, list("c.news.example"))
	w.exec(t, `UPDATE app.messages SET list_id_key = NULL WHERE id = $1`, nul.String())
	fileList("s7@news.example", "Updates", "a.news.example", 2)
	w.file(t, "s7@news.example", "Promotions", 10) // filed, and carries no List-Id at all
	w.ingest(t, "s7@news.example", 9, list("b.news.example"))

	m := w.measure(t, nil)
	// s1 separated; s2 partial; s3 not helped; s6 UNDETERMINED (one filing's
	// list_id_key is NULL, so it is in none of the others, and its two a-filings
	// are not counted as a sub-key); s7 partial (a list-versus-no-list split is
	// not counted as separating). Sub-keys: s1 two, s2's b, s7's a.
	want := placement.ListIDCeiling{Filings: 20, Unfilled: 1, MixedAddresses: 6, MultiList: 5, Undetermined: 1,
		Separated: 1, Partial: 2, NotHelped: 1, SubKeysConfident: 4}
	if m.ListID != want {
		t.Errorf("ceiling = %+v\nwant      %+v", m.ListID, want)
	}
	if m.ListIDAddresses != 6 {
		t.Errorf("addresses carrying more than one List-Id = %d, want 6 (s1, s2, s3, s5, s6, s7; s4 has none)", m.ListIDAddresses)
	}
}

// ── section 2 ────────────────────────────────────────────────────────────────

// TestPlacementMeasureAuthRefusals: filed senders whose mail is refused for
// authentication alone, read from the raw headers.
//
// A trusted authserv-id is configured, so a message needs a trusted DMARC pass
// bound to its From domain.
//
//	alerts@bank   filed U x2 (confident). Unfiled mail:
//	  pass for bank.example ........................ passes
//	  no Authentication-Results .................... AUTH ONLY
//	  dmarc=fail ................................... AUTH ONLY
//	  a second From header, with a valid pass ...... OTHER ONLY (ambiguous)
//	  a second From header, no authentication ...... AUTH + OTHER
//	promo@shop2   filed R x2 (confident); an enabled rule filing it into R is
//	              made AFTER the mail arrived, so the mail is not already placed:
//	  no Authentication-Results x2 ................. AUTH ONLY (rule-covered)
//	  pass ......................................... passes
//	thin@wide     filed Forums x1 (thin): one without auth (AUTH ONLY), one pass
func TestPlacementMeasureAuthRefusals(t *testing.T) {
	w := newMMWorld(t, lpTrusted)
	t.Cleanup(func() { w.f.super.Close() })
	for _, from := range []string{lpBank, "promo@shop2.example"} {
		area := "Updates"
		if from != lpBank {
			area = "Promotions"
		}
		w.file(t, from, area, 40)
		w.file(t, from, area, 39)
	}
	w.file(t, "thin@wide.example", "Forums", 38)
	w.wantFilings(t, lpBank, "Updates", 2)
	w.wantFilings(t, "promo@shop2.example", "Promotions", 2)
	w.wantFilings(t, "thin@wide.example", "Forums", 1)

	w.ingest(t, lpBank, 30, lpPass("bank.example"))
	w.ingest(t, lpBank, 29)
	w.ingest(t, lpBank, 28, "Authentication-Results: "+lpTrusted+"; dmarc=fail header.from=bank.example")
	w.ingest(t, lpBank, 27, "From: x@attacker.example", lpPass("bank.example"))
	w.ingest(t, lpBank, 26, "From: x@attacker.example")
	w.ingest(t, "promo@shop2.example", 25)
	w.ingest(t, "promo@shop2.example", 24)
	w.ingest(t, "promo@shop2.example", 23, lpPass("shop2.example"))
	w.ingest(t, "thin@wide.example", 22)
	w.ingest(t, "thin@wide.example", 21, lpPass("wide.example"))

	ctx := context.Background()
	if err := w.f.st.UserTx(ctx, w.f.userID, func(tx *store.UserTx) error {
		_, err := tx.CreateRule(ctx, store.RuleRow{
			Name: "shop2-to-promotions", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "promo@shop2.example"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: w.areas["Promotions"].LabelID.String()}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	exact := w.measure(t, func(o *store.PlacementMeasureOptions) {
		o.Senders = BackfillSenders(w.f.ing.blobs, w.f.userID, w.f.dek, w.f.ing.trustedAuthservIDs)
	})
	a := exact.Auth
	if !a.Exact || a.Population != 10 || a.Read != 10 || a.Unreadable != 0 || a.Truncated {
		t.Errorf("exact: %+v, want 10 messages, all read", a)
	}
	// The thin population (thin@wide, one filing) is confident now that a single
	// filing acts: its 2 messages join the confident bucket (8+2, OK 2+1,
	// AuthOnly 4+1, keys 2+1).
	if want := (store.AuthBucket{Messages: 10, OK: 3, AuthOnly: 5, AuthAlso: 1, OtherOnly: 1, AuthOnlyKeys: 3, CoveredKeys: 1, CoveredMessages: 2}); a.Confident != want {
		t.Errorf("exact confident = %+v\nwant           %+v", a.Confident, want)
	}
	if want := (store.AuthBucket{}); a.Thin != want {
		t.Errorf("exact thin = %+v\nwant       %+v", a.Thin, want)
	}

	// Without the headers only the stored failure flag is visible: a LOWER
	// bound. Of the four authentication-only messages exactly one (the
	// dmarc=fail) is a clear failure.
	lower := w.measure(t, nil).Auth
	if lower.Exact || lower.Confident.AuthOnly != 1 || lower.Thin.AuthOnly != 0 {
		t.Errorf("lower bound = exact %v, confident auth-only %d, thin %d; want false/1/0", lower.Exact, lower.Confident.AuthOnly, lower.Thin.AuthOnly)
	}

	// The cap reports its bound.
	capped := w.measure(t, func(o *store.PlacementMeasureOptions) {
		o.AuthScanMax = 4
		o.Senders = BackfillSenders(w.f.ing.blobs, w.f.userID, w.f.dek, w.f.ing.trustedAuthservIDs)
	}).Auth
	if !capped.Truncated || capped.Read != 4 || capped.Population != 10 {
		t.Errorf("capped scan: %+v, want truncated, 4 of 10 read", capped)
	}
}
