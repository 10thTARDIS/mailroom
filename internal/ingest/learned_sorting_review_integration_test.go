// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// The independent review's findings on learned sorting (v1.104 beta 2): the
// backfill plan's breakdown by evidence and its opt-outs, a recency correction's
// stricter undo, and the three Forget probes adopted as tests.

import (
	"github.com/oklog/ulid/v2"
	"testing"
	"time"

	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// A backfill plan says which moves rest on the weaker rules, and --no-single and
// --no-recency leave them out (backfilled placements are not gated, so reading
// the plan is the safeguard).
func TestPlacementBackfillBreaksMovesDownByEvidence(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@corp.example", 2, areas, "Updates")    // ordinary
	f.lpFile(t, "bob@other.example", 1, areas, "Promotions")  // single filing
	f.lpFile(t, "dana@third.example", 2, areas, "Promotions") // recency: P P U U U
	f.lpFile(t, "dana@third.example", 3, areas, "Updates")
	for _, from := range []string{"alice@corp.example", "bob@other.example", "dana@third.example"} {
		f.lpIngest(t, from)
	}
	f.lpSwitch(t, true)

	all := f.bfPlan(t)
	if len(all.Moves) != 3 || all.MovesByBasis[placement.BasisEvidence] != 1 ||
		all.MovesByBasis[placement.BasisSingle] != 1 || all.MovesByBasis[placement.BasisRecency] != 1 {
		t.Fatalf("plan = %d moves by basis %v, want 3: one each", len(all.Moves), all.MovesByBasis)
	}
	if k, ok := f.bfKey(all, "bob@other.example"); !ok || k.ByBasis[placement.BasisSingle] != 1 {
		t.Errorf("bob's key line = %+v, want one single-filing move", k)
	}

	ns := f.bfPlanOpts(t, store.PlacementBackfillOptions{NoSingle: true})
	if len(ns.Moves) != 2 || ns.MovesByBasis[placement.BasisSingle] != 0 || ns.Abstain[store.BackfillNoSingle] != 1 {
		t.Errorf("--no-single: %d moves, by basis %v, no_single %d, want 2, none single, 1", len(ns.Moves), ns.MovesByBasis, ns.Abstain[store.BackfillNoSingle])
	}
	nr := f.bfPlanOpts(t, store.PlacementBackfillOptions{NoRecency: true})
	if len(nr.Moves) != 2 || nr.MovesByBasis[placement.BasisRecency] != 0 || nr.Abstain[store.BackfillNoRecency] != 1 {
		t.Errorf("--no-recency: %d moves, by basis %v, no_recency %d, want 2, none recency, 1", len(nr.Moves), nr.MovesByBasis, nr.Abstain[store.BackfillNoRecency])
	}
	both := f.bfPlanOpts(t, store.PlacementBackfillOptions{NoSingle: true, NoRecency: true})
	if len(both.Moves) != 1 || both.MovesByBasis[placement.BasisEvidence] != 1 {
		t.Errorf("both opt-outs: %d moves by basis %v, want only the ordinary one", len(both.Moves), both.MovesByBasis)
	}
}

// The plan honours Forget: what the user asked it to forget is not moved.
func TestPlacementBackfillHonoursForget(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@corp.example", 2, areas, "Updates")
	f.lpIngest(t, "alice@corp.example")
	f.lpSwitch(t, true)
	if p := f.bfPlan(t); len(p.Moves) != 1 {
		t.Fatalf("control: moves before Forget = %d, want 1", len(p.Moves))
	}
	time.Sleep(10 * time.Millisecond)
	f.lsForget(t, placement.KeyAddress, "alice@corp.example")
	if p := f.bfPlan(t); len(p.Moves) != 0 || p.Abstain[placement.NoEvidence] == 0 {
		t.Errorf("after Forget: %d moves, abstain %v, want 0 moves and 'none'", len(p.Moves), p.Abstain)
	}
}

// An ADDRESS Forget mark hides that address's filings from the DOMAIN key too,
// or the domain fallback would act on the very evidence the user forgot.
func TestAddressForgetHidesFilingsFromTheDomainKey(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "x1@shop.example", 2, areas, "Promotions")
	f.lpFile(t, "x2@shop.example", 2, areas, "Promotions")
	if n := f.lpLabelBy(t, f.lpIngest(t, "newcomer@shop.example"), areas["Promotions"], store.LabelByLearned); n != 1 {
		t.Fatal("control: the two-address domain must act before Forget")
	}
	time.Sleep(10 * time.Millisecond)
	f.lsForget(t, placement.KeyAddress, "x1@shop.example")
	// The domain now rests on x2 alone: one address is not a domain's pattern.
	f.assertDeskPlaced(t, f.lpIngest(t, "newcomer2@shop.example"), areas, "domain after an address Forget")
}

// Forget hides a single-filing correction: a filing made after the mark acts
// again at once, where the correction would otherwise have demanded two.
func TestForgetHidesASingleFilingCorrection(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "bob@example.com", 1, areas, "Updates")
	placed := f.lpIngest(t, "bob@example.com")
	if f.lsBasis(t, placed) != "single" {
		t.Fatal("fixture: a single-filing placement")
	}
	f.lpUpdate(t, placed, store.EmailUpdate{RemoveLabelIDs: []ulid.ULID{areas["Updates"].LabelID}})
	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "bob@example.com", 1, areas, "Updates")
	f.assertDeskPlaced(t, f.lpIngest(t, "bob@example.com"), areas, "control: one filing after a single-filing correction")
	time.Sleep(10 * time.Millisecond)
	f.lsForget(t, placement.KeyAddress, "bob@example.com")
	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "bob@example.com", 1, areas, "Updates")
	if n := f.lpLabelBy(t, f.lpIngest(t, "bob@example.com"), areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("one filing after Forget: learned = %d, want 1 (the correction is hidden)", n)
	}
}

// A correction of a RECENCY placement is undone as strictly as a single
// filing's: two newer filings, not one.
func TestRecencyCorrectionNeedsTwoMoreFilings(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Promotions")
	f.lpFile(t, "alice@example.com", 3, areas, "Updates")
	placed := f.lpIngest(t, "alice@example.com")
	if f.lsBasis(t, placed) != "recency" {
		t.Fatal("fixture: a recency placement")
	}
	f.lpUpdate(t, placed, store.EmailUpdate{RemoveLabelIDs: []ulid.ULID{areas["Updates"].LabelID}})
	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "alice@example.com", 1, areas, "Updates")
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "one filing after a recency correction")
	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "alice@example.com", 1, areas, "Updates")
	if n := f.lpLabelBy(t, f.lpIngest(t, "alice@example.com"), areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("two filings after a recency correction: learned = %d, want 1", n)
	}
}
