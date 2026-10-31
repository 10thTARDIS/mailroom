// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Collapse keeps a copy's learned-placement and day-one undo records (#882).
//
// app.learned_placements and app.setup_journal reference the copy ON DELETE
// CASCADE, so a collapse used to take them with it: a correction on the kept copy
// found no placement to correct, and day-one undo found no journal row, so the
// label the run wrote was never taken back. Each test asserts a thing a bare
// DELETE would have destroyed, and reads it back through the code that needs it
// (recordLearnedCorrection, SetupUndoChunk), not just by counting rows.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

type recordsFixture struct {
	h        *harness
	u        fixtureUser
	keeper   ulid.ULID
	loser    ulid.ULID
	labelA   string
	areaA    string
	labelB   string
	areaB    string
	runID    string
	runIDTwo string
}

func seedRecords(t *testing.T, tag string) recordsFixture {
	t.Helper()
	h := newHarness(t)
	u := seedUser(t, h.super, "rec-"+NewULID().String())
	keeper, loser := dupPair(t, h, u, tag)
	f := recordsFixture{h: h, u: u, keeper: keeper, loser: loser}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(context.Background(), sql, args...); err != nil {
			t.Fatalf("seed %s: %v", tag, err)
		}
	}
	mk := func(name string) (label, area string) {
		label, area = NewULID().String(), NewULID().String()
		exec(`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`, label, u.ID, name+"-"+tag)
		exec(`INSERT INTO app.areas (id, user_id, name, position, label_id) VALUES ($1,$2,$3,0,$4)`,
			area, u.ID, "Area-"+name+"-"+tag, label)
		return label, area
	}
	f.labelA, f.areaA = mk("a")
	f.labelB, f.areaB = mk("b")
	run := func() string {
		id := NewULID().String()
		exec(`INSERT INTO app.setup_runs (id, user_id, kind, status, window_days, plan)
		      VALUES ($1,$2,'import','done',30,'{"windowDays":30,"folders":[]}'::jsonb)`, id, u.ID)
		return id
	}
	f.runID, f.runIDTwo = run(), run()
	return f
}

func (f recordsFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.h.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func (f recordsFixture) placement(t *testing.T, msg ulid.ULID, areaID, outcome string) {
	t.Helper()
	if outcome == "corrected" {
		f.exec(t, `INSERT INTO app.learned_placements
		             (user_id, message_id, key_kind, key_value, area_id, outcome, corrected_at)
		           VALUES ($1,$2,'address','s@example.com',$3,'corrected',now())`,
			f.u.ID, msg.String(), areaID)
		return
	}
	f.exec(t, `INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id)
	           VALUES ($1,$2,'address','s@example.com',$3)`, f.u.ID, msg.String(), areaID)
}

func (f recordsFixture) journal(t *testing.T, msg ulid.ULID, run string, label *string, archived bool) {
	t.Helper()
	f.exec(t, `INSERT INTO app.setup_journal (id, user_id, run_id, message_id, label_id, desk_archived)
	           VALUES ($1,$2,$3,$4,$5,$6)`,
		NewULID().String(), f.u.ID, run, msg.String(), label, archived)
}

func (f recordsFixture) collapse(t *testing.T) (CollapseResult, bool) {
	t.Helper()
	var res CollapseResult
	var ok bool
	if err := f.h.appStore().UserTx(context.Background(), mustULID(t, f.u.ID), func(tx *UserTx) error {
		var e error
		res, ok, e = tx.CollapseDuplicateGroup(context.Background(), f.keeper, []ulid.ULID{f.loser})
		return e
	}); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	return res, ok
}

func (f recordsFixture) outcomeOnKeeper(t *testing.T) (area, outcome string) {
	t.Helper()
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT area_id, outcome FROM app.learned_placements WHERE message_id = $1`,
		f.keeper.String()).Scan(&area, &outcome); err != nil {
		t.Fatalf("no learned placement on the keeper: %v", err)
	}
	return area, outcome
}

// The headline: a copy carrying a placement and a setup-journal row. After the
// collapse a correction on the KEPT copy finds the placement, and day-one undo
// takes the carried label off. Also reads the report first, per CLAUDE.md.
func TestCollapseCarriesPlacementAndSetupJournalToTheKeeper(t *testing.T) {
	f := seedRecords(t, "rec-main")
	ctx := context.Background()
	s := f.h.appStore()
	userID := mustULID(t, f.u.ID)

	// The copy has the area label as the day-one run's own ('setup') filing.
	f.exec(t, `INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
	           VALUES ($1,$2,$3,'setup')`, f.u.ID, f.loser.String(), f.labelA)
	f.placement(t, f.loser, f.areaA, "open")
	f.journal(t, f.loser, f.runID, &f.labelA, false)

	// REPORT BEFORE FIX: per-signal counts, read-only.
	var cands []CollapseCandidate
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		cands, e = tx.DuplicateCollapseCandidates(ctx, 50)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var l CollapseLoser
	for _, c := range cands {
		if c.KeeperID == f.keeper {
			l = c.Losers[0]
		}
	}
	if l.LearnedPlacements != 1 || l.LearnedPlacementConflicts != 0 || l.LearnedCorrectionsWin != 0 ||
		l.SetupJournalRows != 1 || l.SetupJournalConflicts != 0 || l.SetupJournalLabelClashes != 0 {
		t.Errorf("report counts wrong: %+v", l)
	}
	if got := scalar(t, f.h, `SELECT count(*) FROM app.learned_placements`+
		` WHERE message_id = $1`, f.loser.String()); got != 1 {
		t.Fatal("the report changed something")
	}

	res, ok := f.collapse(t)
	if !ok {
		t.Fatal("collapse refused")
	}
	if res.PlacementsMoved != 1 || res.PlacementsReplaced != 0 || res.PlacementsDropped != 0 ||
		res.JournalMoved != 1 || res.JournalMerged != 0 {
		t.Errorf("result = %+v", res)
	}

	// A correction on the keeper finds its placement.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.recordLearnedCorrection(ctx, f.keeper)
	}); err != nil {
		t.Fatal(err)
	}
	if area, outcome := f.outcomeOnKeeper(t); outcome != "corrected" || area != f.areaA {
		t.Errorf("keeper placement = (%s, %s), want (%s, corrected)", area, outcome, f.areaA)
	}

	// Day-one undo removes the carried label.
	if got := scalar(t, f.h, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		f.keeper.String(), f.labelA); got != 1 {
		t.Fatal("the carried label is not on the keeper")
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.SetupUndoChunk(ctx, mustULID(t, f.runID), 50)
		return e
	}); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := scalar(t, f.h, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		f.keeper.String(), f.labelA); got != 0 {
		t.Error("day-one undo left the carried label: its journal row was lost in the collapse")
	}
}

// Both copies have a placement. A correction outranks the keeper's open row; the
// keeper's own row survives a copy's non-correction; a correction on the keeper is
// never displaced.
func TestCollapsePlacementConflictRule(t *testing.T) {
	t.Run("copy's correction replaces the keeper's open row", func(t *testing.T) {
		f := seedRecords(t, "pc1")
		f.placement(t, f.keeper, f.areaA, "open")
		f.placement(t, f.loser, f.areaB, "corrected")
		res, ok := f.collapse(t)
		if !ok {
			t.Fatal("refused")
		}
		if area, outcome := f.outcomeOnKeeper(t); area != f.areaB || outcome != "corrected" {
			t.Errorf("keeper placement = (%s, %s), want the copy's correction", area, outcome)
		}
		if res.PlacementsMoved != 1 || res.PlacementsReplaced != 1 || res.PlacementsDropped != 0 {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("keeper's row survives a copy's open row", func(t *testing.T) {
		f := seedRecords(t, "pc2")
		f.placement(t, f.keeper, f.areaA, "open")
		f.placement(t, f.loser, f.areaB, "open")
		res, _ := f.collapse(t)
		if area, outcome := f.outcomeOnKeeper(t); area != f.areaA || outcome != "open" {
			t.Errorf("keeper placement = (%s, %s), want its own", area, outcome)
		}
		if res.PlacementsMoved != 0 || res.PlacementsDropped != 1 {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("keeper's correction is never displaced", func(t *testing.T) {
		f := seedRecords(t, "pc3")
		f.placement(t, f.keeper, f.areaA, "corrected")
		f.placement(t, f.loser, f.areaB, "corrected")
		f.collapse(t)
		if area, outcome := f.outcomeOnKeeper(t); area != f.areaA || outcome != "corrected" {
			t.Errorf("keeper placement = (%s, %s), want its own correction", area, outcome)
		}
	})
	t.Run("report counts the conflict and the winning correction", func(t *testing.T) {
		f := seedRecords(t, "pc4")
		f.placement(t, f.keeper, f.areaA, "open")
		f.placement(t, f.loser, f.areaB, "corrected")
		var l CollapseLoser
		if err := f.h.appStore().UserTx(context.Background(), mustULID(t, f.u.ID), func(tx *UserTx) error {
			cands, e := tx.DuplicateCollapseCandidates(context.Background(), 50)
			for _, c := range cands {
				if c.KeeperID == f.keeper {
					l = c.Losers[0]
				}
			}
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if l.LearnedPlacements != 1 || l.LearnedPlacementConflicts != 1 || l.LearnedCorrectionsWin != 1 {
			t.Errorf("report counts wrong: %+v", l)
		}
	})
}

// Journal rows for the same run on both copies merge into the keeper's row, keeping
// what undo needs; different labels in one run block the group and change nothing.
func TestCollapseSetupJournalConflictRule(t *testing.T) {
	t.Run("same run merges into the keeper's row", func(t *testing.T) {
		f := seedRecords(t, "jc1")
		// Keeper's row: the run found the label already there (NULL) and did nothing.
		f.journal(t, f.keeper, f.runID, nil, false)
		// The copy's row: the run wrote the label and archived it.
		f.journal(t, f.loser, f.runID, &f.labelA, true)
		// And a second run touched only the copy: its row moves whole.
		f.journal(t, f.loser, f.runIDTwo, &f.labelB, false)
		res, ok := f.collapse(t)
		if !ok {
			t.Fatal("refused")
		}
		if res.JournalMoved != 1 || res.JournalMerged != 1 {
			t.Errorf("result = %+v", res)
		}
		var label *string
		var archived bool
		if err := f.h.super.QueryRow(context.Background(),
			`SELECT label_id, desk_archived FROM app.setup_journal WHERE message_id=$1 AND run_id=$2`,
			f.keeper.String(), f.runID).Scan(&label, &archived); err != nil {
			t.Fatal(err)
		}
		if label == nil || *label != f.labelA || !archived {
			t.Errorf("merged row = (%v, %v), want (%s, true)", label, archived, f.labelA)
		}
		if got := scalar(t, f.h, `SELECT count(*) FROM app.setup_journal WHERE message_id=$1`,
			f.keeper.String()); got != 2 {
			t.Errorf("keeper journal rows = %d, want 2 (one per run)", got)
		}
	})
	t.Run("different labels in one run block the group", func(t *testing.T) {
		f := seedRecords(t, "jc2")
		f.journal(t, f.keeper, f.runID, &f.labelA, false)
		f.journal(t, f.loser, f.runID, &f.labelB, false)

		// Blocked in the report...
		var l CollapseLoser
		var blockers []string
		if err := f.h.appStore().UserTx(context.Background(), mustULID(t, f.u.ID), func(tx *UserTx) error {
			cands, e := tx.DuplicateCollapseCandidates(context.Background(), 50)
			for _, c := range cands {
				if c.KeeperID == f.keeper {
					l, blockers = c.Losers[0], c.Blockers()
				}
			}
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if l.SetupJournalLabelClashes != 1 || len(blockers) != 1 {
			t.Errorf("report: clashes=%d blockers=%v", l.SetupJournalLabelClashes, blockers)
		}
		// ... and again under the lock, changing nothing.
		if _, ok := f.collapse(t); ok {
			t.Fatal("collapse went ahead over a label clash")
		}
		if got := scalar(t, f.h, `SELECT count(*) FROM app.messages WHERE id=$1`, f.loser.String()); got != 1 {
			t.Error("the blocked collapse deleted the copy")
		}
		if got := scalar(t, f.h, `SELECT count(*) FROM app.setup_journal WHERE message_id=$1`,
			f.loser.String()); got != 1 {
			t.Error("the blocked collapse touched the copy's journal row")
		}
	})
}

// A copy with neither record: behaviour unchanged, nothing invented on the keeper.
func TestCollapseWithoutRecordsIsUnchanged(t *testing.T) {
	f := seedRecords(t, "rec-none")
	res, ok := f.collapse(t)
	if !ok {
		t.Fatal("refused")
	}
	if res.PlacementsMoved+res.PlacementsReplaced+res.PlacementsDropped+res.JournalMoved+res.JournalMerged != 0 {
		t.Errorf("result = %+v, want no placement or journal activity", res)
	}
	if got := scalar(t, f.h, `SELECT count(*) FROM app.learned_placements WHERE message_id=$1`,
		f.keeper.String()); got != 0 {
		t.Error("a placement appeared on the keeper")
	}
	if got := scalar(t, f.h, `SELECT count(*) FROM app.setup_journal WHERE message_id=$1`,
		f.keeper.String()); got != 0 {
		t.Error("a journal row appeared on the keeper")
	}
	if got := scalar(t, f.h, `SELECT count(*) FROM app.messages WHERE id=$1`, f.loser.String()); got != 0 {
		t.Error("the copy is still there")
	}
}
