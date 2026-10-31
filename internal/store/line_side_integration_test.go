// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE LINE OVERRIDE AND THE DRAWER SWEEP (#464) — the half of the feature the
// user cannot see until it goes wrong: what the sweep does after they move a
// message across the line.
//
// It lives here rather than in internal/api because the harness does: the
// drawer fixture (drawer_sweep_integration_test.go) is what can build a
// message old enough to sweep, in an auto-archiving area, in an account that
// actually has an Archive folder.
//
// Two directions, and the FIRST one is the reason this issue exists at all:
//
//   - A low-scored message the user pulled ABOVE the line must never be
//     archived, however long it sits there. Before #464 the sweep read the raw
//     score, so it would have archived exactly the mail somebody had just said
//     was important.
//   - An above-the-line message the user pushed INTO the drawer must be swept
//     once it is old enough, whatever its score — a placement is a verdict,
//     and the user's outranks the model's.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestLineSideOverridesTheDrawerSweep(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()

	area := f.createArea(t, "Sweeps", true, intPtr(30))

	below := score32(0.1) // desk_preferences.line_threshold defaults to 0.5
	above := score32(0.9)

	// The two controls, with no override at all: these are the shapes the sweep
	// already got right, and they are here so a failure below cannot be read as
	// "the sweep stopped working" (a probe that removes the override entirely
	// must leave these two green).
	plainCandidate := f.seedMessage(t, "low, untouched", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below,
	})
	plainAboveLine := f.seedMessage(t, "high, untouched", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: above,
	})

	// The two subjects.
	pulledUp := f.seedMessage(t, "low, corrected to important", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below,
	})
	pushedDown := f.seedMessage(t, "high, corrected to notImportant", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: above,
	})
	// And an unscored one placed in the drawer by hand: nothing scored it, so
	// without an override the sweep has no verdict about it and leaves it
	// alone — with one, the verdict is the user's.
	unscoredPushedDown := f.seedMessage(t, "unscored, corrected to notImportant", msgOpt{
		daysOld: 31, areaLabel: area.LabelID,
	})

	for id, side := range map[ulid.ULID]LineSide{
		pulledUp:           LineSideSurface,
		pushedDown:         LineSideDrawer,
		unscoredPushedDown: LineSideDrawer,
	} {
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			return tx.SetLineSide(ctx, id, side)
		}); err != nil {
			t.Fatalf("set line side %s on %s: %v", side, id, err)
		}
	}

	var candidates []DrawerCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		candidates, e = tx.DrawerCandidates(ctx, time.Now(), 100)
		return e
	}); err != nil {
		t.Fatalf("drawer candidates: %v", err)
	}
	got := candidateIDs(candidates)

	names := map[ulid.ULID]string{
		plainCandidate: "low/untouched", plainAboveLine: "high/untouched",
		pulledUp: "low/corrected-important", pushedDown: "high/corrected-notImportant",
		unscoredPushedDown: "unscored/corrected-notImportant",
	}
	render := func() []string {
		out := make([]string, 0, len(got))
		for id := range got {
			if n, ok := names[id]; ok {
				out = append(out, n)
			} else {
				out = append(out, id.String())
			}
		}
		return out
	}

	if _, ok := got[pulledUp]; ok {
		t.Errorf("a message the user PULLED ABOVE THE LINE is a sweep candidate — the sweep "+
			"would archive mail somebody just called important. candidates = %v", render())
	}
	if _, ok := got[pushedDown]; !ok {
		t.Errorf("a message the user PUSHED INTO THE DRAWER is not a sweep candidate, though it "+
			"is old enough — its 0.9 score is still deciding. candidates = %v", render())
	}
	if _, ok := got[unscoredPushedDown]; !ok {
		t.Errorf("an UNSCORED message the user pushed into the drawer is not a sweep candidate, "+
			"though it is old enough — a placement is a verdict. candidates = %v", render())
	}
	// The controls, unchanged by any of this.
	if _, ok := got[plainCandidate]; !ok {
		t.Errorf("the plain below-the-line candidate was excluded: %v", render())
	}
	if _, ok := got[plainAboveLine]; ok {
		t.Errorf("a plain above-the-line message became a candidate: %v", render())
	}

	// ArchiveByDrawer's re-check reads the SAME predicate (drawerCandidateWhereSQL),
	// so a message pulled above the line AFTER it was selected must be refused at
	// the write — the window between the scan and the move is exactly where a
	// user's correction lands.
	var archived bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		if err := tx.SetLineSide(ctx, plainCandidate, LineSideSurface); err != nil {
			return err
		}
		var e error
		archived, e = tx.ArchiveByDrawer(ctx, got[plainCandidate], time.Now())
		return e
	}); err != nil {
		t.Fatalf("archive by drawer after a late correction: %v", err)
	}
	if archived {
		t.Error("ArchiveByDrawer archived a message the user pulled above the line between the " +
			"scan and the write — the re-check is not reading the override")
	}
}
