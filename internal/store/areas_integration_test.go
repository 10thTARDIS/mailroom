// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the areas store layer (#442 increment 1, migration
// 00046): default materialisation, the "a feed belongs to at most one area"
// invariant, and change-log discipline on create/update/delete. The resolver
// agreement test and AreaSummary live in areas_resolve_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// TestAreasMaterialiseDefaultPlanExactlyOnce pins the shape
// materialiseCategoryTabs already established, extended with the one thing
// unique to areas: an existing label is REUSED rather than shadowed by a
// second one of the same name.
func TestAreasMaterialiseDefaultPlanExactlyOnce(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "am-"+NewULID().String())
	userID := mustULID(t, u.ID)

	// The fixture seeds one area directly (so the RLS inventory has one);
	// delete it (cascading to its feed) so materialiseDefaultAreas' OWN "zero
	// areas" guard — not the fixture — is what this test actually exercises.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.areas WHERE user_id = $1`, u.ID); err != nil {
		t.Fatalf("clear fixture area: %v", err)
	}

	// A pre-existing label named "promotions" (lower-case), to prove
	// case-insensitive reuse: the Promotions area must own THIS id, not mint a
	// second label that would collide with (or shadow) the operator's own.
	var promoLabelID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		id, e := tx.CreateLabel(ctx, LabelRow{Name: "promotions", MirrorToIMAP: true})
		promoLabelID = id
		return e
	}); err != nil {
		t.Fatalf("seed promotions label: %v", err)
	}

	var (
		first            []AreaView
		before, afterOne int64
		afterTwo         int64
	)
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if before, e = tx.ChangeState(ctx, ObjectArea); e != nil {
			return e
		}
		if first, e = tx.ListAreas(ctx); e != nil {
			return e
		}
		afterOne, e = tx.ChangeState(ctx, ObjectArea)
		return e
	}); err != nil {
		t.Fatalf("first list: %v", err)
	}

	if len(first) != len(tabs.All) {
		t.Fatalf("first list returned %d areas, want %d", len(first), len(tabs.All))
	}
	if got := afterOne - before; got != int64(len(tabs.All)) {
		t.Errorf("first read appended %d change_log rows, want %d (one per area)", got, len(tabs.All))
	}

	var otherLabelIDs []ulid.ULID
	for i, want := range tabs.All {
		tabName := string(want)
		wantName := strings.ToUpper(tabName[:1]) + tabName[1:]
		if first[i].Name != wantName {
			t.Errorf("area %d name = %q, want %q (tabs.All order)", i, first[i].Name, wantName)
		}
		hasTab := false
		for _, f := range first[i].Feeds {
			if f.Tab == want {
				hasTab = true
			}
		}
		if !hasTab {
			t.Errorf("area %q does not feed from tab %q", first[i].Name, want)
		}
		if want == tabs.Promotions {
			if first[i].LabelID != promoLabelID {
				t.Errorf("promotions area owns label %s, want the pre-existing %s", first[i].LabelID, promoLabelID)
			}
		} else {
			if first[i].LabelID == promoLabelID {
				t.Errorf("area %q unexpectedly owns the pre-existing promotions label", first[i].Name)
			}
			otherLabelIDs = append(otherLabelIDs, first[i].LabelID)
		}
	}

	// The four minted labels are MirrorToIMAP false and carry the matching tab
	// mapping; the reused "promotions" label is left exactly as the operator
	// made it (MirrorToIMAP true here), which materialisation must not touch.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetLabels(ctx, otherLabelIDs)
		if e != nil {
			return e
		}
		if len(views) != len(otherLabelIDs) {
			t.Fatalf("got %d labels, want %d", len(views), len(otherLabelIDs))
		}
		for _, v := range views {
			if v.MirrorToIMAP {
				t.Errorf("materialised label %q mirrors to IMAP, want false", v.Name)
			}
			if v.Tab == "" {
				t.Errorf("materialised label %q has no tab mapping", v.Name)
			}
		}
		promo, e := tx.GetLabels(ctx, []ulid.ULID{promoLabelID})
		if e != nil {
			return e
		}
		if len(promo) != 1 || !promo[0].MirrorToIMAP {
			t.Errorf("materialisation touched the pre-existing promotions label: %+v", promo)
		}
		return nil
	}); err != nil {
		t.Fatalf("check materialised labels: %v", err)
	}

	// Idempotent: a second ListAreas creates nothing more.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.ListAreas(ctx)
		if e != nil {
			return e
		}
		afterTwo, e = tx.ChangeState(ctx, ObjectArea)
		return e
	}); err != nil {
		t.Fatalf("second list: %v", err)
	}
	if afterTwo != afterOne {
		t.Errorf("a repeat ListAreas moved the state %d -> %d; materialisation must be idempotent", afterOne, afterTwo)
	}
}

// TestAreaFeedsAtMostOneArea pins #442's "a tab or label feeds at most one
// area": a legitimate reassignment (free it from the source, then claim it in
// the destination) succeeds, while claiming a feed still held by another area
// is refused WITHOUT poisoning the transaction.
func TestAreaFeedsAtMostOneArea(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "afo-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var areaA, areaB ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		areaA, e = tx.CreateArea(ctx, AreaRow{Name: "Area A", Feeds: []AreaFeed{{Tab: tabs.Updates}}})
		if e != nil {
			return e
		}
		areaB, e = tx.CreateArea(ctx, AreaRow{Name: "Area B"})
		return e
	}); err != nil {
		t.Fatalf("create areas: %v", err)
	}

	// Moving tab:updates to another area: free it from A, then claim it in
	// B — the way a client actually reassigns a feed.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.UpdateArea(ctx, areaA, AreaPatch{SetFeeds: true, Feeds: nil})
		if e != nil {
			return e
		}
		if !found {
			t.Error("area A not found")
		}
		return nil
	}); err != nil {
		t.Fatalf("free tab from A: %v", err)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.UpdateArea(ctx, areaB, AreaPatch{SetFeeds: true, Feeds: []AreaFeed{{Tab: tabs.Updates}}})
		if e != nil {
			return e
		}
		if !found {
			t.Error("area B not found")
		}
		return nil
	}); err != nil {
		t.Fatalf("claim tab in B: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetAreas(ctx, []ulid.ULID{areaA, areaB})
		if e != nil {
			return e
		}
		byID := map[ulid.ULID]AreaView{}
		for _, v := range views {
			byID[v.ID] = v
		}
		for _, f := range byID[areaA].Feeds {
			if f.Tab == tabs.Updates {
				t.Error("area A still feeds from tab:updates after moving it away")
			}
		}
		moved := false
		for _, f := range byID[areaB].Feeds {
			if f.Tab == tabs.Updates {
				moved = true
			}
		}
		if !moved {
			t.Error("area B does not feed from tab:updates after claiming it")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify move: %v", err)
	}

	var ownLabelOfA ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetAreas(ctx, []ulid.ULID{areaA})
		if e != nil {
			return e
		}
		ownLabelOfA = views[0].LabelID
		return nil
	}); err != nil {
		t.Fatalf("read area A: %v", err)
	}

	// The genuine conflict: claiming a label another area still owns, in a
	// patch that ALSO drops area B's tab:updates feed (SetFeeds replaces the
	// whole set). The transaction must survive it — proved by a second write
	// in the SAME tx, not a fresh one — and area B's feed set must come out
	// UNCHANGED: a partial apply (the drop taking effect while the claim
	// fails) would be a feed change with no Area change_log row to show for
	// it, the exact failure mode a shared outer SAVEPOINT exists to prevent.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.UpdateArea(ctx, areaB, AreaPatch{SetFeeds: true, Feeds: []AreaFeed{{LabelID: &ownLabelOfA}}})
		if !errors.Is(e, ErrFeedTaken) {
			return fmt.Errorf("claiming another area's own label: %v, want ErrFeedTaken", e)
		}

		views, e := tx.GetAreas(ctx, []ulid.ULID{areaB})
		if e != nil {
			return e
		}
		stillHasTab := false
		for _, f := range views[0].Feeds {
			if f.Tab == tabs.Updates {
				stillHasTab = true
			}
			if f.LabelID != nil && *f.LabelID == ownLabelOfA {
				return fmt.Errorf("area B ended up feeding from A's own label despite the conflict")
			}
		}
		if !stillHasTab {
			return fmt.Errorf("area B lost its tab:updates feed even though the conflicting update failed — " +
				"the partial DELETE was not rolled back with the failed INSERT")
		}

		if _, e := tx.CreateArea(ctx, AreaRow{Name: "Area C"}); e != nil {
			return fmt.Errorf("write after a feed conflict in the same tx: %w", e)
		}
		return nil
	}); err != nil {
		t.Fatalf("feed conflict recovery: %v", err)
	}
}

// TestAreaCreateUpdateDeleteChangeLog pins the load-bearing invariant for
// areas: exactly one change row per actual mutation, none for a no-op, and a
// delete that keeps the area's own label (and everything it labelled) intact.
func TestAreaCreateUpdateDeleteChangeLog(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "acud-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var areaID ulid.ULID
	var before, afterCreate int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if before, e = tx.ChangeState(ctx, ObjectArea); e != nil {
			return e
		}
		areaID, e = tx.CreateArea(ctx, AreaRow{Name: "Focus"})
		if e != nil {
			return e
		}
		afterCreate, e = tx.ChangeState(ctx, ObjectArea)
		return e
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if afterCreate-before != 1 {
		t.Errorf("create appended %d change rows, want 1", afterCreate-before)
	}

	var afterUpdate int64
	newName := "Focus (renamed)"
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.UpdateArea(ctx, areaID, AreaPatch{Name: &newName})
		if e != nil {
			return e
		}
		if !found {
			t.Error("area not found")
		}
		afterUpdate, e = tx.ChangeState(ctx, ObjectArea)
		return e
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if afterUpdate-afterCreate != 1 {
		t.Errorf("update appended %d change rows, want 1", afterUpdate-afterCreate)
	}

	var afterNoop int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.UpdateArea(ctx, areaID, AreaPatch{Name: &newName})
		if e != nil {
			return e
		}
		if !found {
			t.Error("area not found")
		}
		afterNoop, e = tx.ChangeState(ctx, ObjectArea)
		return e
	}); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if afterNoop != afterUpdate {
		t.Errorf("a no-op patch appended %d change rows, want 0", afterNoop-afterUpdate)
	}

	var ownLabel ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetAreas(ctx, []ulid.ULID{areaID})
		if e != nil {
			return e
		}
		ownLabel = views[0].LabelID
		return nil
	}); err != nil {
		t.Fatalf("read area: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
		u.ID, u.MessageID, ownLabel.String()); err != nil {
		t.Fatalf("attach label: %v", err)
	}

	var afterDelete int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.DeleteArea(ctx, areaID)
		if e != nil {
			return e
		}
		if !found {
			t.Error("area not found on delete")
		}
		afterDelete, e = tx.ChangeState(ctx, ObjectArea)
		return e
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if afterDelete-afterNoop != 1 {
		t.Errorf("delete appended %d change rows, want 1", afterDelete-afterNoop)
	}

	var labelStillThere, messageStillLabelled int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.labels WHERE user_id = $1 AND id = $2`,
		u.ID, ownLabel.String()).Scan(&labelStillThere); err != nil {
		t.Fatalf("check label: %v", err)
	}
	if labelStillThere != 1 {
		t.Error("deleting the area deleted its own label")
	}
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.message_labels WHERE user_id = $1 AND message_id = $2 AND label_id = $3`,
		u.ID, u.MessageID, ownLabel.String()).Scan(&messageStillLabelled); err != nil {
		t.Fatalf("check message label: %v", err)
	}
	if messageStillLabelled != 1 {
		t.Error("deleting the area detached its own label from the message")
	}
}

// TestCreateAreaFailureLeavesNothingBehind: CreateArea mints the area's own
// label before inserting the area row. If the area row is refused (here: a
// name that differs from an existing area only by case), the label must go
// with it — otherwise the user is told "could not create Receipts" and finds
// a stray Receipts label, announced by a Label change row, in every client.
func TestCreateAreaFailureLeavesNothingBehind(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "acf-"+NewULID().String())
	userID := mustULID(t, u.ID)

	name := "Receipts-" + u.Tag
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.CreateArea(ctx, AreaRow{Name: name})
		return e
	}); err != nil {
		t.Fatalf("create first area: %v", err)
	}

	count := func(sql string, args ...any) int {
		var n int
		if err := h.super.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	labelsBefore := count(`SELECT count(*) FROM app.labels WHERE user_id = $1`, u.ID)
	changesBefore := count(`SELECT count(*) FROM app.change_log WHERE user_id = $1`, u.ID)

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.CreateArea(ctx, AreaRow{Name: strings.ToUpper(name)})
		if e == nil {
			return fmt.Errorf("creating an area whose name differs only by case succeeded")
		}
		// The transaction must still be usable after the refused create.
		if _, e := tx.CreateLabel(ctx, LabelRow{Name: "after-failure-" + u.Tag}); e != nil {
			return fmt.Errorf("write after a refused CreateArea in the same tx: %w", e)
		}
		return nil
	}); err != nil {
		t.Fatalf("refused create: %v", err)
	}

	labelsAfter := count(`SELECT count(*) FROM app.labels WHERE user_id = $1`, u.ID)
	changesAfter := count(`SELECT count(*) FROM app.change_log WHERE user_id = $1`, u.ID)
	// Exactly the one label (and its one change row) written AFTER the failure.
	if labelsAfter-labelsBefore != 1 {
		t.Errorf("labels grew by %d across a refused CreateArea, want 1 (only the follow-up label); "+
			"the minted own label of the refused area leaked", labelsAfter-labelsBefore)
	}
	if changesAfter-changesBefore != 1 {
		t.Errorf("change_log grew by %d across a refused CreateArea, want 1 (only the follow-up label)",
			changesAfter-changesBefore)
	}
	if strays := count(`SELECT count(*) FROM app.labels WHERE user_id = $1 AND name = $2`,
		u.ID, strings.ToUpper(name)); strays != 0 {
		t.Errorf("the refused area's own label %q survived the failure", strings.ToUpper(name))
	}
}
