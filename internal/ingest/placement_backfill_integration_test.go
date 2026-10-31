// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// THE PLACEMENT BACKFILL end to end (#714 increment 2a, ADR-032 addendum):
// existing mail is ingested through the real ingest path with learned
// placement switched OFF (so nothing is placed on arrival), the user's
// evidence is written by the real Re-file write, and the plan reads each
// candidate's bytes back out of the real encrypted blob store through
// BackfillSenders, the function mailroom-admin placement-backfill uses.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/placement"
	"mailroom/internal/store"
)

const bfTrusted = "mx.trusted.example"

// bfPass is a trusted DMARC pass bound to domain.
func bfPass(domain string) string {
	return "Authentication-Results: " + bfTrusted + "; spf=pass smtp.mailfrom=" + domain +
		"; dkim=pass header.d=" + domain + "; dmarc=pass header.from=" + domain
}

// bfPlan runs the planner in EXACT mode over the fixture's own blob store.
func (f *fixture) bfPlan(t *testing.T, excludes ...string) store.PlacementBackfillPlan {
	t.Helper()
	ctx := context.Background()
	var plan store.PlacementBackfillPlan
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		plan, e = tx.PlanPlacementBackfill(ctx, store.PlacementBackfillOptions{
			Exclude: excludes,
			Senders: BackfillSenders(f.ing.blobs, f.userID, f.dek, f.ing.trustedAuthservIDs),
		})
		return e
	}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Accounted() != plan.Scanned {
		t.Fatalf("plan partition accounts for %d of %d scanned: %+v", plan.Accounted(), plan.Scanned, plan)
	}
	return plan
}

func (f *fixture) bfApply(t *testing.T, plan store.PlacementBackfillPlan) store.PlacementBackfillApplied {
	t.Helper()
	res, err := f.st.ApplyPlacementBackfill(context.Background(), f.userID, plan.Moves)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return res
}

// bfIngestWith ingests through another Ingestor (another blob store).
func (f *fixture) bfIngestWith(t *testing.T, ing *Ingestor, from string, extra ...string) ulid.ULID {
	t.Helper()
	uid := lpUID.Add(1)
	res, err := ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		rawFrom(from, fmt.Sprintf("Note %d", uid), fmt.Sprintf("bf-%d-%s@example.com", uid, f.userID), extra...),
		f.prov(f.mailboxA, uid, nil))
	if err != nil || !res.Created {
		t.Fatalf("ingest from %s: %v (created %v)", from, err, res.Created)
	}
	return res.MessageID
}

// bfLabels snapshots every label on msgID: label id -> "applied_by@created_at".
func (f *fixture) bfLabels(t *testing.T, msgID ulid.ULID) map[string]string {
	t.Helper()
	rows, err := f.super.Query(context.Background(), `
		SELECT label_id, applied_by, created_at FROM app.message_labels WHERE message_id = $1`, msgID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, by string
		var at time.Time
		if err := rows.Scan(&id, &by, &at); err != nil {
			t.Fatal(err)
		}
		out[id] = by + "@" + at.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func sameLabels(a, b map[string]string) bool {
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

func (f *fixture) bfKey(plan store.PlacementBackfillPlan, value string) (store.PlacementBackfillKey, bool) {
	for _, k := range plan.Keys {
		if k.Key.Value == value {
			return k, true
		}
	}
	return store.PlacementBackfillKey{}, false
}

func (f *fixture) bfReport(t *testing.T) store.PlacementReport {
	t.Helper()
	ctx := context.Background()
	var rep store.PlacementReport
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		rep, e = tx.PlacementReport(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestPlacementBackfillMovesExactlyWhatItReports is the feature, and the
// report-equals-apply promise: the plan's per-key counts are what is written,
// each move is the swap (desk label off, learned label on), one Email
// change_log row per message, no writeback, a placement-log row marked as a
// backfill, and user-, rule- and learned-labelled mail is not touched.
func TestPlacementBackfillMovesExactlyWhatItReports(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, bfTrusted)
	ctx := context.Background()
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpFile(t, "news@shop.example", 2, areas, "Promotions")

	a1 := f.lpIngest(t, "alice@example.com", bfPass("example.com"))
	a2 := f.lpIngest(t, "alice@example.com", bfPass("example.com"))
	s1 := f.lpIngest(t, "news@shop.example", bfPass("shop.example"))
	// HELD by a rule: the desk's Primary label re-attributed to a rule.
	r1 := f.lpIngest(t, "alice@example.com", bfPass("example.com"))
	if _, err := f.super.Exec(ctx, `UPDATE app.message_labels SET applied_by = 'rule' WHERE message_id = $1 AND label_id = $2`,
		r1.String(), areas["Primary"].LabelID.String()); err != nil {
		t.Fatal(err)
	}
	// HELD by the user: a topic label of theirs that feeds Primary.
	u1 := f.lpIngest(t, "alice@example.com", bfPass("example.com"))
	receipts := ulid.Make()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, 'Receipts')`, []any{receipts.String(), f.userID.String()}},
		{`INSERT INTO app.area_feeds (id, user_id, area_id, label_id) VALUES ($1, $2, $3, $4)`,
			[]any{ulid.Make().String(), f.userID.String(), areas["Primary"].ID.String(), receipts.String()}},
		{`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by) VALUES ($1, $2, $3, 'user')`,
			[]any{f.userID.String(), u1.String(), receipts.String()}},
	} {
		if _, err := f.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	// HELD by learned placement: placed on arrival.
	f.lpSwitch(t, true)
	l1 := f.lpIngest(t, "alice@example.com", bfPass("example.com"))
	if f.lpLabelBy(t, l1, areas["Updates"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: the arrival placement must have filed l1")
	}
	held := map[ulid.ULID]map[string]string{r1: f.bfLabels(t, r1), u1: f.bfLabels(t, u1), l1: f.bfLabels(t, l1)}

	countAll := func() [3]int {
		return [3]int{
			f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE user_id=$1`, f.userID.String()),
			f.scalar(t, `SELECT count(*) FROM app.learned_placements WHERE user_id=$1`, f.userID.String()),
			f.scalar(t, `SELECT count(*) FROM app.change_log WHERE user_id=$1`, f.userID.String()),
		}
	}
	before := countAll()
	plan := f.bfPlan(t)
	if after := countAll(); after != before {
		t.Fatalf("the report wrote: labels/placements/changes %v -> %v", before, after)
	}

	if !plan.Exact || !plan.Enabled {
		t.Fatalf("plan exact=%v enabled=%v, want both", plan.Exact, plan.Enabled)
	}
	if len(plan.Moves) != 3 {
		t.Fatalf("moves = %d, want 3 (a1, a2, s1): %+v", len(plan.Moves), plan.Moves)
	}
	if n := plan.Pairs[store.PlacementMove{From: areas["Primary"].ID, To: areas["Updates"].ID}]; n != 2 {
		t.Errorf("Primary -> Updates = %d, want 2", n)
	}
	if n := plan.Pairs[store.PlacementMove{From: areas["Primary"].ID, To: areas["Promotions"].ID}]; n != 1 {
		t.Errorf("Primary -> Promotions = %d, want 1", n)
	}
	// 4 evidence messages + r1, u1, l1.
	if plan.Held != 7 {
		t.Errorf("held = %d, want 7", plan.Held)
	}
	if k, ok := f.bfKey(plan, "alice@example.com"); !ok || k.Moves != 2 || k.NowIn[areas["Primary"].ID] != 2 ||
		k.AreaID != areas["Updates"].ID || k.Key.Kind != placement.KeyAddress {
		t.Errorf("alice's key line = %+v (found %v), want 2 moves from Primary into Updates", k, ok)
	}

	beforeUpdated := f.changeCount(t, "Email", "updated")
	res := f.bfApply(t, plan)
	if res.Moved != 3 || len(res.Skipped) != 0 {
		t.Fatalf("applied = %+v, want 3 moved, none skipped", res)
	}
	// REPORT EQUALS APPLY, per key, from what is in the database.
	for _, k := range plan.Keys {
		got := f.scalar(t, `SELECT count(*) FROM app.learned_placements p
		                     JOIN app.audit_log a ON a.user_id = p.user_id AND a.object_type = 'PlacementBackfill'
		                      AND a.object_id = $3 AND a.action = 'placement.backfill.move' AND a.detail->>'message' = p.message_id
		                    WHERE p.user_id = $1 AND p.key_value = $2`, f.userID.String(), k.Key.Value, res.BatchID.String())
		if got != k.Moves || res.PerKey[k.Key] != k.Moves {
			t.Errorf("key %s: planned %d, written %d (apply says %d)", k.Key.Value, k.Moves, got, res.PerKey[k.Key])
		}
	}
	for _, m := range []struct {
		id   ulid.ULID
		area string
	}{{a1, "Updates"}, {a2, "Updates"}, {s1, "Promotions"}} {
		if n := f.lpLabelBy(t, m.id, areas[m.area], store.LabelByLearned); n != 1 {
			t.Errorf("%s: %s learned label = %d, want 1", m.id, m.area, n)
		}
		if n := f.lpDeskLabels(t, m.id); n != 0 {
			t.Errorf("%s: desk labels left = %d, want 0 (the swap takes Primary's off)", m.id, n)
		}
		if n := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Email' AND op='updated' AND object_id=$2`,
			f.userID.String(), m.id.String()); n != 1 {
			t.Errorf("%s: Email updated rows = %d, want 1", m.id, n)
		}
		if f.lpOutcome(t, m.id) != "open//false" {
			t.Errorf("%s: placement = %q, want open", m.id, f.lpOutcome(t, m.id))
		}
	}
	if got := f.changeCount(t, "Email", "updated") - beforeUpdated; got != 3 {
		t.Errorf("Email updated rows = %d, want 3", got)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1 AND message_id = ANY($2)`,
		f.userID.String(), idStrings([]ulid.ULID{a1, a2, s1})); n != 0 {
		t.Errorf("writeback intents for moved mail = %d, want 0 (area labels are local-only)", n)
	}
	for id, snap := range held {
		if got := f.bfLabels(t, id); !sameLabels(got, snap) {
			t.Errorf("held message %s changed: %v -> %v", id, snap, got)
		}
	}
	// The placement log tells the backfill from arrival.
	rep := f.bfReport(t)
	if rep.Origins.Backfill[store.PlacementOpen] != 3 || rep.Origins.Arrival[store.PlacementOpen] != 1 {
		t.Errorf("origins = %+v, want backfill open 3, arrival open 1", rep.Origins)
	}
	if len(rep.Batches) != 1 || rep.Batches[0].BatchID != res.BatchID.String() || rep.Batches[0].Moved != 3 {
		t.Errorf("batches = %+v, want one with 3 moves", rep.Batches)
	}
	// Applied once, there is nothing left to do.
	if again := f.bfPlan(t); len(again.Moves) != 0 || again.Held != 10 {
		t.Errorf("re-plan after apply: moves %d held %d, want 0 and 10", len(again.Moves), again.Held)
	}
}

// TestPlacementBackfillEnforcesArrivalChecks: each arrival-time check, run on
// each candidate's own stored headers, skips with its own reason, and only
// the authenticated, unambiguous, unwarned message moves (the positive
// control). placement-report's simulation, which reads no headers, counts
// the same mail as an upper bound.
func TestPlacementBackfillEnforcesArrivalChecks(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, bfTrusted)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	const bank = "alerts@bank.example"
	f.lpFile(t, bank, 2, areas, "Updates")
	f.lpFile(t, "shop@xn--pple-43d.com", 2, areas, "Updates")

	pos := f.lpIngest(t, bank, bfPass("bank.example"))
	f.lpIngest(t, bank, "Authentication-Results: "+bfTrusted+"; spf=pass smtp.mailfrom=evil.example; dmarc=fail header.from=bank.example")
	f.lpIngest(t, bank, bfPass("attacker.example"))
	f.lpIngest(t, bank)
	f.lpIngest(t, bank+", x@attacker.example", bfPass("bank.example"))
	f.lpIngest(t, "shop@xn--pple-43d.com", bfPass("xn--pple-43d.com"))
	otherBlobs, err := blob.NewFSBlobStore(t.TempDir(), randomKey(t))
	if err != nil {
		t.Fatal(err)
	}
	f.bfIngestWith(t, New(f.st, otherBlobs, WithTrustedAuthservIDs([]string{bfTrusted})), bank, bfPass("bank.example"))
	f.lpSwitch(t, true)

	plan := f.bfPlan(t)
	if len(plan.Moves) != 1 || plan.Moves[0].MessageID != pos {
		t.Fatalf("moves = %+v, want exactly the authenticated message %s", plan.Moves, pos)
	}
	want := map[placement.Reason]int{
		placement.Unauthenticated: 3, // trusted fail, a pass for another domain, no header
		placement.AmbiguousFrom:   1,
		placement.SenderWarned:    1,
		store.BackfillUnreadable:  1,
	}
	for r, n := range want {
		if plan.Abstain[r] != n {
			t.Errorf("abstain[%s] = %d, want %d (all: %v)", r, plan.Abstain[r], n, plan.Abstain)
		}
	}
	if k, ok := f.bfKey(plan, bank); !ok || k.Moves != 1 || k.Skipped[placement.Unauthenticated] != 3 ||
		k.Skipped[placement.AmbiguousFrom] != 1 || k.Skipped[store.BackfillUnreadable] != 1 {
		t.Errorf("bank key line = %+v, want 1 move and its skips", k)
	}
	// The upper bound reads only the stored auth_fail: it would move the
	// other-domain pass, the header-less, the ambiguous and the unreadable
	// message too.
	rep := f.bfReport(t)
	if n := rep.Moves[store.PlacementMove{From: areas["Primary"].ID, To: areas["Updates"].ID}]; n != 5 {
		t.Errorf("upper-bound simulation moves = %d, want 5 (exact: 1)", n)
	}

	res := f.bfApply(t, plan)
	if res.Moved != 1 {
		t.Fatalf("applied %+v, want 1", res)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE user_id=$1 AND applied_by='learned'`, f.userID.String()); n != 1 {
		t.Errorf("learned labels = %d, want 1", n)
	}
}

// TestPlacementBackfillExclude: --exclude takes an address or a domain,
// case-insensitively; a domain covers address keys under it too; an entry
// that matched nothing reports 0.
func TestPlacementBackfillExclude(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@corp.example", 2, areas, "Updates")
	f.lpFile(t, "carol@other.example", 1, areas, "Promotions")
	f.lpFile(t, "dave@other.example", 1, areas, "Promotions")
	f.lpIngest(t, "alice@corp.example")
	f.lpIngest(t, "alice@corp.example")
	erin := f.lpIngest(t, "erin@other.example") // decided by the other.example domain
	f.lpSwitch(t, true)

	if all := f.bfPlan(t); len(all.Moves) != 3 {
		t.Fatalf("control: moves without --exclude = %d, want 3", len(all.Moves))
	}
	plan := f.bfPlan(t, "Alice@Corp.example", "@other.example", "nobody.example")
	if len(plan.Moves) != 0 || plan.Abstain[store.BackfillExcluded] != 3 {
		t.Errorf("moves %d excluded %d, want 0 and 3", len(plan.Moves), plan.Abstain[store.BackfillExcluded])
	}
	for e, n := range map[string]int{"alice@corp.example": 2, "other.example": 1, "nobody.example": 0} {
		if got, ok := plan.Excluded[e]; !ok || got != n {
			t.Errorf("excluded[%s] = %d (present %v), want %d", e, got, ok, n)
		}
	}
	if byDomain := f.bfPlan(t, "corp.example"); len(byDomain.Moves) != 1 || byDomain.Moves[0].MessageID != erin {
		t.Errorf("--exclude corp.example: moves %+v, want only erin's", byDomain.Moves)
	}
}

// TestPlacementBackfillRefusesWhenOff: with learned placement off the report
// still runs, and the apply refuses and writes nothing.
func TestPlacementBackfillRefusesWhenOff(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@corp.example", 2, areas, "Updates")
	msg := f.lpIngest(t, "alice@corp.example")

	plan := f.bfPlan(t)
	if plan.Enabled || len(plan.Moves) != 1 {
		t.Fatalf("plan with the switch off: enabled %v, moves %d; want false, 1", plan.Enabled, len(plan.Moves))
	}
	if _, err := f.st.ApplyPlacementBackfill(context.Background(), f.userID, plan.Moves); !errors.Is(err, store.ErrLearnedPlacementOff) {
		t.Fatalf("apply with the switch off: err = %v, want ErrLearnedPlacementOff", err)
	}
	if f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned) != 0 || f.lpPlacements(t, msg) != 0 {
		t.Fatal("the refused apply wrote")
	}
	f.lpSwitch(t, true)
	if res := f.bfApply(t, plan); res.Moved != 1 {
		t.Errorf("positive control: with the switch on, moved %d, want 1", res.Moved)
	}
}

// TestPlacementBackfillUndo: undo restores exactly, attribution and
// timestamp included, records no correction, and leaves a message the user
// re-filed in between where they put it.
func TestPlacementBackfillUndo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@corp.example", 2, areas, "Updates")
	a1 := f.lpIngest(t, "alice@corp.example")
	a2 := f.lpIngest(t, "alice@corp.example")
	a3 := f.lpIngest(t, "alice@corp.example")
	// a2's placement came from day-one setup, a week ago.
	if _, err := f.super.Exec(ctx, `
		UPDATE app.message_labels SET applied_by = 'setup', created_at = now() - interval '7 days'
		 WHERE message_id = $1 AND label_id = $2`, a2.String(), areas["Primary"].LabelID.String()); err != nil {
		t.Fatal(err)
	}
	f.lpSwitch(t, true)
	snap := map[ulid.ULID]map[string]string{a1: f.bfLabels(t, a1), a2: f.bfLabels(t, a2)}

	res := f.bfApply(t, f.bfPlan(t))
	if res.Moved != 3 {
		t.Fatalf("moved %d, want 3", res.Moved)
	}
	if f.bfLabels(t, a2)[areas["Primary"].LabelID.String()] != "" {
		t.Fatal("fixture: the apply must have taken a2's setup label off")
	}
	// The user re-files a3 into Promotions after the batch.
	f.lpRefile(t, a3, areas, "Promotions")

	beforeUpdated := f.changeCount(t, "Email", "updated")
	undo, err := f.st.UndoPlacementBackfill(ctx, f.userID, res.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Journal != 3 || undo.Outcomes[store.BackfillUndone] != 2 || undo.Outcomes[store.BackfillRefiled] != 1 {
		t.Fatalf("undo = %+v, want 3 journalled: 2 undone, 1 refiled", undo)
	}
	for id, want := range snap {
		if got := f.bfLabels(t, id); !sameLabels(got, want) {
			t.Errorf("%s not restored exactly:\n got  %v\n want %v", id, got, want)
		}
		if f.lpPlacements(t, id) != 0 {
			t.Errorf("%s: placement log row survived the undo", id)
		}
	}
	if got := f.changeCount(t, "Email", "updated") - beforeUpdated; got != 2 {
		t.Errorf("undo Email updated rows = %d, want 2", got)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1 AND message_id = ANY($2)`,
		f.userID.String(), idStrings([]ulid.ULID{a1, a2})); n != 0 {
		t.Errorf("writeback intents from the undo = %d, want 0", n)
	}
	// a3 stays where the user put it, and their correction stands.
	if f.lpLabelBy(t, a3, areas["Promotions"], store.LabelByUser) != 1 || f.lpDeskLabels(t, a3) != 0 {
		t.Errorf("a3 was clobbered: %v", f.bfLabels(t, a3))
	}
	if got := f.lpOutcome(t, a3); got != "corrected//true" {
		t.Errorf("a3 placement = %q, want corrected", got)
	}
	// Again: nothing more to do.
	again, err := f.st.UndoPlacementBackfill(ctx, f.userID, res.BatchID)
	if err != nil || again.Outcomes[store.BackfillAlreadyUndone] != 2 || again.Outcomes[store.BackfillRefiled] != 1 {
		t.Errorf("second undo = %+v (%v), want 2 already undone, 1 refiled", again, err)
	}
	if _, err := f.st.UndoPlacementBackfill(ctx, f.userID, ulid.Make()); !errors.Is(err, store.ErrNoSuchBackfillBatch) {
		t.Errorf("unknown batch: err = %v, want ErrNoSuchBackfillBatch", err)
	}
}

// TestPlacementBackfillIsPerUser: user B's mail and evidence never reach user
// A's plan, and a plan naming B's messages applied as A writes nothing.
func TestPlacementBackfillIsPerUser(t *testing.T) {
	a := newFixture(t)
	b := newFixture(t)
	areasA := a.lpAreas(t)
	areasB := b.lpAreas(t)
	b.lpSwitch(t, false)
	b.lpFile(t, "alice@corp.example", 2, areasB, "Updates")
	b1 := b.lpIngest(t, "alice@corp.example")
	b.lpIngest(t, "alice@corp.example")
	b.lpSwitch(t, true)
	a.lpIngest(t, "alice@corp.example")
	_ = areasA

	planB := b.bfPlan(t)
	if len(planB.Moves) != 2 {
		t.Fatalf("control: B's plan moves %d, want 2", len(planB.Moves))
	}
	planA := a.bfPlan(t)
	if want := a.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, a.userID.String()); planA.Scanned != want {
		t.Errorf("A's plan scanned %d, want A's own %d messages", planA.Scanned, want)
	}
	if planA.Filings != 0 || len(planA.Moves) != 0 {
		t.Errorf("A's plan: filings %d moves %d, want 0 and 0 (B's evidence is not A's)", planA.Filings, len(planA.Moves))
	}
	res, err := a.st.ApplyPlacementBackfill(context.Background(), a.userID, planB.Moves)
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 0 || res.Skipped[store.BackfillGone] != 2 {
		t.Errorf("A applying B's moves: %+v, want 0 moved, 2 gone", res)
	}
	if b.lpPlacements(t, b1) != 0 || b.lpLabelBy(t, b1, areasB["Updates"], store.LabelByLearned) != 0 {
		t.Error("A's apply wrote to B's message")
	}
}

// bfArea finds an area's line in the report.
func bfArea(t *testing.T, rep store.PlacementReport, id ulid.ULID) store.PlacementReportArea {
	t.Helper()
	for _, a := range rep.Areas {
		if a.AreaID == id {
			return a
		}
	}
	t.Fatalf("area %s not in the report", id)
	return store.PlacementReportArea{}
}

// TestPlacementBackfillIsNotGated: the correction gate judges ARRIVAL
// placements only. Thirty REAL backfilled placements, aged into kept, sit
// beside twenty arrival placements whose Wilson bound is below the floor
// (19 kept, 1 corrected: 0.764). Counted together they would be 49/50 and
// lift the bound to about 0.89; the area must stay STOPPED, ingest must hold
// new mail back, and the report must show the backfill outcomes on their own
// row.
func TestPlacementBackfillIsNotGated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	areas := f.lpAreas(t)
	upd := areas["Updates"]
	f.lpSwitch(t, false)
	f.lpFile(t, "bob@shop.example", 2, areas, "Updates")
	for i := 0; i < 30; i++ {
		f.lpIngest(t, "bob@shop.example")
	}
	f.lpSwitch(t, true)
	res := f.bfApply(t, f.bfPlan(t))
	if res.Moved != 30 {
		t.Fatalf("backfill moved %d, want 30", res.Moved)
	}
	// Age the batch: untouched for nine days, so every one is KEPT by age.
	if _, err := f.super.Exec(ctx, `UPDATE app.learned_placements SET decided_at = now() - interval '9 days' WHERE user_id = $1`,
		f.userID.String()); err != nil {
		t.Fatal(err)
	}
	// Twenty arrival placements in Updates, 19 kept and 1 corrected.
	for i := 0; i < placement.GateMinResolved; i++ {
		m := f.lpIngest(t, fmt.Sprintf("seed%d@other.example", i))
		outcome, keptAt, keptBy, correctedAt := "kept", "now()", "'age'", "NULL"
		if i == 0 {
			outcome, keptAt, keptBy, correctedAt = "corrected", "NULL", "NULL", "now()"
		}
		if _, err := f.super.Exec(ctx, `
			INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id, decided_at, outcome, kept_at, kept_by, corrected_at)
			VALUES ($1, $2, 'address', $3, $4, now() - interval '9 days', $5, `+keptAt+`, `+keptBy+`, `+correctedAt+`)`,
			f.userID.String(), m.String(), fmt.Sprintf("seed%d@other.example", i), upd.ID.String(), outcome); err != nil {
			t.Fatal(err)
		}
	}

	a := bfArea(t, f.bfReport(t), upd.ID)
	if !a.Gate.Stopped || a.Gate.Resolved != 20 {
		t.Errorf("Updates gate = %+v, want STOPPED on 20 resolved arrival placements", a.Gate)
	}
	if a.Counts.Kept != 19 || a.Counts.Corrected != 1 || a.Counts.BackfillKept != 30 ||
		a.Counts.BackfillOpen != 0 || a.Counts.BackfillCorrected != 0 {
		t.Errorf("Updates counts = %+v, want arrival 19/1 and backfill kept 30", a.Counts)
	}
	// Ingest agrees: a confident sender's new mail is held back by the gate.
	f.assertDeskPlaced(t, f.lpIngest(t, "bob@shop.example"), areas, "gated area with kept backfills")
	// And so does the backfill planner.
	f.lpSwitch(t, false)
	f.lpIngest(t, "bob@shop.example")
	f.lpSwitch(t, true)
	if plan := f.bfPlan(t); plan.Abstain[store.LearnedGated] != 2 || len(plan.Moves) != 0 {
		t.Errorf("plan in a stopped area: gated %d, moves %d; want 2 and 0", plan.Abstain[store.LearnedGated], len(plan.Moves))
	}
}

// TestPlacementBackfillCorrectionContradictsItsKey: the gate ignores
// backfilled placements, but a correction of one is still the user saying
// "not there" about that sender: the key stops acting for new mail.
func TestPlacementBackfillCorrectionContradictsItsKey(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "carol@corp.example", 2, areas, "Updates")
	old := f.lpIngest(t, "carol@corp.example")
	f.lpSwitch(t, true)
	if res := f.bfApply(t, f.bfPlan(t)); res.Moved != 1 {
		t.Fatalf("backfill moved %d, want 1", res.Moved)
	}
	if r := f.bfReport(t).KeyReasons[placement.KeyAddress]; r[placement.Confident] != 1 {
		t.Fatalf("control: carol must be confident before the correction, got %v", r)
	}
	// The user takes the learned label off by hand, filing it nowhere.
	f.lpUpdate(t, old, store.EmailUpdate{RemoveLabelIDs: []ulid.ULID{areas["Updates"].LabelID}})
	if got := f.lpOutcome(t, old); got != "corrected//true" {
		t.Fatalf("backfilled placement = %q, want corrected", got)
	}
	rep := f.bfReport(t)
	if r := rep.KeyReasons[placement.KeyAddress]; r[placement.Contradicted] != 1 || r[placement.Confident] != 0 {
		t.Errorf("carol's key after the correction = %v, want contradicted", r)
	}
	if rep.Origins.Backfill[store.PlacementCorrected] != 1 || bfArea(t, rep, areas["Updates"].ID).Counts.BackfillCorrected != 1 {
		t.Errorf("the correction must show as a backfill outcome: %+v", rep.Origins)
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "carol@corp.example"), areas, "key contradicted by a backfill correction")
}
