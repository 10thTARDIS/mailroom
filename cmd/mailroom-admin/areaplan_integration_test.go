// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// Integration coverage for applyAreaPlanDiff's "remove before add" ordering
// (#442 increment 3, §5): a feed a plan MOVES from one stored area to another
// must be freed from its old owner before it is claimed by the new one,
// against a real Postgres where store.ErrFeedTaken is the thing that would
// fire if the order were wrong.

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// TestAreaPlanMoveOrdersRemovalsBeforeAdditions is a negative control AND the
// positive proof in one test: claiming tabs.Updates on Movies before it is
// freed from News fails with ErrFeedTaken (proving the conflict is real, not
// vacuous — CLAUDE.md's "negative-control every... assertion"), and
// applyAreaPlanDiff's own two-phase apply then performs the identical move
// successfully.
func TestAreaPlanMoveOrdersRemovalsBeforeAdditions(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	tag := "moveorder-" + store.NewULID().String()
	userID := seedUntangleUser(t, h.super, tag)

	var newsID, moviesID ulid.ULID
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		newsID, e = tx.CreateArea(ctx, store.AreaRow{Name: "News", Feeds: []store.AreaFeed{{Tab: tabs.Updates}}})
		if e != nil {
			return e
		}
		moviesID, e = tx.CreateArea(ctx, store.AreaRow{Name: "Movies"})
		return e
	}); err != nil {
		t.Fatalf("seed areas: %v", err)
	}

	// THE NEGATIVE CONTROL: claiming the tab on Movies BEFORE freeing it from
	// News is refused. Without this, a green "the move succeeded" below could
	// mean the conflict was never real in the first place.
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, e := tx.UpdateArea(ctx, moviesID, store.AreaPatch{SetFeeds: true, Feeds: []store.AreaFeed{{Tab: tabs.Updates}}})
		return e
	}); !errors.Is(err, store.ErrFeedTaken) {
		t.Fatalf("claiming a feed before freeing it: err = %v, want ErrFeedTaken", err)
	}

	// The plan: News gives up the tab, Movies gains it.
	plan := areas.Config{Areas: []areas.Area{
		{Name: "News", Labels: []string{"News"}},
		{Name: "Movies", Tabs: []tabs.Tab{tabs.Updates}, Labels: []string{"Movies"}},
	}}
	var (
		stored []store.AreaView
		labels []store.LabelView
	)
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if stored, e = tx.ListAreas(ctx); e != nil {
			return e
		}
		labels, e = tx.ListLabels(ctx)
		return e
	}); err != nil {
		t.Fatalf("read stored plan: %v", err)
	}
	labelNameByID := make(map[ulid.ULID]string, len(labels))
	for _, l := range labels {
		labelNameByID[l.ID] = l.Name
	}

	diff := diffAreaPlan(stored, labelNameByID, plan)
	if err := applyAreaPlanDiff(ctx, h.store, userID, diff); err != nil {
		t.Fatalf("applyAreaPlanDiff (move): %v", err)
	}

	var views []store.AreaView
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		views, e = tx.GetAreas(ctx, []ulid.ULID{newsID, moviesID})
		return e
	}); err != nil {
		t.Fatalf("read areas after move: %v", err)
	}
	byID := make(map[ulid.ULID]store.AreaView, len(views))
	for _, v := range views {
		byID[v.ID] = v
	}
	for _, f := range byID[newsID].Feeds {
		if f.Tab == tabs.Updates {
			t.Error("News still holds tabs.Updates after the move")
		}
	}
	moved := false
	for _, f := range byID[moviesID].Feeds {
		if f.Tab == tabs.Updates {
			moved = true
		}
	}
	if !moved {
		t.Error("Movies did not gain tabs.Updates")
	}
}
