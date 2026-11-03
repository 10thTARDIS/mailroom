// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// LEARNED PLACEMENT'S STATUS (store.LearningStatus) end to end, over the real
// ingest path and the real Re-file write, for the same reason the rule
// suggestion is tested here: "learned placement would act" is a fact about
// ingest, never about hand-written rows.
//
// Every "not listed" assertion is paired with a "listed" control built from the
// same fixture, so a test cannot pass because the list is always empty.

import (
	"context"
	"fmt"
	"testing"

	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

func (f *fixture) lsAsk(t *testing.T, trusted ...string) store.LearningStatus {
	t.Helper()
	ctx := context.Background()
	var out store.LearningStatus
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		out, err = tx.LearningStatus(ctx, f.rsSenderFunc(trusted...))
		return err
	}); err != nil {
		t.Fatalf("LearningStatus: %v", err)
	}
	return out
}

// lsWantSenders asserts the listed senders are exactly (address, area, reason)
// triples, in order.
func lsWantSenders(t *testing.T, got store.LearningStatus, want ...[3]string) {
	t.Helper()
	var have [][3]string
	for _, s := range got.Senders {
		have = append(have, [3]string{s.Address, s.AreaID.String(), s.Reason})
	}
	if fmt.Sprint(have) != fmt.Sprint(want) {
		t.Fatalf("listed senders = %v, want %v", have, want)
	}
}

// TestLearningStatusCounts: arrival placements decided in the last 30 days,
// by outcome. Backfilled placements and older ones are left out.
func TestLearningStatusCounts(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, rsSender, 3, areas, "Updates")
	f.lpSwitch(t, true)

	var ids []string
	for i := 0; i < 6; i++ {
		id := f.lpIngest(t, rsSender)
		if n := f.lpLabelBy(t, id, areas["Updates"], store.LabelByLearned); n != 1 {
			t.Fatalf("fixture: learned placement did not place message %d", i)
		}
		ids = append(ids, id.String())
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.learned_placements WHERE user_id = $1`, f.userID.String()); n != 6 {
		t.Fatalf("fixture: %d placement rows, want 6", n)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.super.Exec(context.Background(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 0 corrected, 1 kept, 2 and 5 stay open, 3 is a backfill placement, 4 is
	// older than the window.
	exec(`UPDATE app.learned_placements SET outcome='corrected', corrected_at=now() WHERE message_id = $1`, ids[0])
	exec(`UPDATE app.learned_placements SET outcome='kept', kept_at=now(), kept_by='archive' WHERE message_id = $1`, ids[1])
	exec(`INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
	      VALUES ($1, 'system', 'placement.backfill.move', 'PlacementBackfill', 'batch', jsonb_build_object('message', $2::text))`,
		f.userID.String(), ids[3])
	exec(`UPDATE app.learned_placements SET decided_at = now() - interval '31 days' WHERE message_id = $1`, ids[4])

	got := f.lsAsk(t)
	if !got.Enabled || got.Placed != 4 || got.Corrected != 1 || got.Kept != 1 || got.Open != 2 {
		t.Fatalf("status = %+v, want enabled placed=4 corrected=1 kept=1 open=2 (arrival, last 30 days)", got)
	}

	// Window edge: just inside still counts.
	exec(`UPDATE app.learned_placements SET decided_at = now() - interval '29 days' WHERE message_id = $1`, ids[4])
	if got := f.lsAsk(t); got.Placed != 5 {
		t.Errorf("placed with a 29-day-old placement = %d, want 5", got.Placed)
	}

	// Switched off: no counts at all, just the fact that it is off.
	f.lpSwitch(t, false)
	if got := f.lsAsk(t); got.Enabled || got.Placed != 0 || len(got.Senders) != 0 {
		t.Errorf("status with the switch off = %+v, want enabled=false and nothing else", got)
	}
}

// TestLearningStatusListsSendersItIsNotLearning: a mixed sender is listed with
// the placement reason; each condition that stops the rule suggestion keeps it
// off the list.
func TestLearningStatusListsSendersItIsNotLearning(t *testing.T) {
	f := newFixture(t)
	areas := f.rsMixedSeed(t, 3)
	ctx := context.Background()
	upd := areas["Updates"].ID.String()
	mixed := [3]string{rsSender, upd, string(placement.Mixed)}

	got := f.lsAsk(t)
	lsWantSenders(t, got, mixed)
	if got.Senders[0].Filings != 3 || got.MoreSenders != 0 || got.SendersTruncated {
		t.Errorf("sender line = %+v more=%d truncated=%v, want 3 filings and nothing more", got.Senders[0], got.MoreSenders, got.SendersTruncated)
	}

	// A covering enabled rule takes it off the list.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.CreateRule(ctx, store.RuleRow{
			Name: "covers", Priority: 100, Enabled: true,
			Conditions: rules.Condition{All: []rules.Condition{{Field: rules.FieldFrom, Op: rules.OpEquals, Value: rsSender}}},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: areas["Updates"].LabelID.String()}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	lsWantSenders(t, f.lsAsk(t))
	if _, err := f.super.Exec(ctx, `UPDATE app.rules SET enabled = false WHERE user_id = $1`, f.userID.String()); err != nil {
		t.Fatal(err)
	}
	lsWantSenders(t, f.lsAsk(t), mixed)

	// A recent "Not now" takes it off; an old one does not.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.DismissRuleSuggestion(ctx, areas["Updates"].ID, rsSender)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	lsWantSenders(t, f.lsAsk(t))
	if _, err := f.super.Exec(ctx, `UPDATE app.rule_suggestion_dismissals SET dismissed_at = now() - interval '31 days' WHERE user_id = $1`, f.userID.String()); err != nil {
		t.Fatal(err)
	}
	lsWantSenders(t, f.lsAsk(t), mixed)

	// Under three filings in the window: not a candidate.
	f.rsBackdateFilings(t, areas["Updates"], 31)
	lsWantSenders(t, f.lsAsk(t))
}

// TestLearningStatusNotListedWhenLearnedWillAct: consistent evidence and an
// authentic sender mean learned placement files the next message, so the
// sender is not listed; once the sender fails authentication it is, with that
// reason.
func TestLearningStatusNotListedWhenLearnedWillAct(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, "mx.trusted.example")
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, rsSender, 3, areas, "Updates")
	f.lpSwitch(t, true)
	f.lpWantFilings(t, rsSender, areas["Updates"], 3)

	// Control: the filed mail carries no trusted pass, so it is unauthenticated
	// and listed...
	lsWantSenders(t, f.lsAsk(t, "mx.trusted.example"),
		[3]string{rsSender, areas["Updates"].ID.String(), string(placement.Unauthenticated)})

	// ...and a newer authenticated message makes learned placement act: gone.
	f.lpIngest(t, rsSender,
		"Authentication-Results: mx.trusted.example; spf=pass; dkim=pass header.d=example.com; dmarc=pass header.from=example.com")
	lsWantSenders(t, f.lsAsk(t, "mx.trusted.example"))
}

// TestLearningStatusCandidateCap: only the first 25 candidates are evaluated
// and the answer says it was cut; five are returned and the rest are counted.
func TestLearningStatusCandidateCap(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	for i := 0; i < store.LearningStatusMaxCandidates+2; i++ {
		who := fmt.Sprintf("sender%02d@example.com", i)
		f.lpFile(t, who, 1, areas, "Promotions")
		f.lpFile(t, who, 3, areas, "Updates")
	}
	f.lpSwitch(t, true)

	got := f.lsAsk(t)
	if len(got.Senders) != store.LearningStatusMaxSenders {
		t.Fatalf("returned %d senders, want %d", len(got.Senders), store.LearningStatusMaxSenders)
	}
	if want := store.LearningStatusMaxCandidates - store.LearningStatusMaxSenders; got.MoreSenders != want {
		t.Errorf("moreSenders = %d, want %d (25 evaluated, 5 shown)", got.MoreSenders, want)
	}
	if !got.SendersTruncated {
		t.Error("27 candidates were cut to 25 but the answer does not say so")
	}
	if got.Senders[0].Address != "sender00@example.com" {
		t.Errorf("first sender = %s, want sender00 (ties by address)", got.Senders[0].Address)
	}
}

// TestLearningStatusIsPerUser: another user's placements and filings never
// reach this user's status.
func TestLearningStatusIsPerUser(t *testing.T) {
	f := newFixture(t)
	g := newFixture(t)
	gAreas := g.rsMixedSeed(t, 3)
	lsWantSenders(t, g.lsAsk(t), [3]string{rsSender, gAreas["Updates"].ID.String(), string(placement.Mixed)})

	got := f.lsAsk(t)
	if !got.Enabled || got.Placed != 0 || len(got.Senders) != 0 {
		t.Fatalf("user F sees %+v from user G's data, want an empty status", got)
	}
}
