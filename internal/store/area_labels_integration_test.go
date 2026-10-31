// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for #442 increment 3's one write path (area_labels.go):
// a message resolved BY TAB gets its area's own label with applied_by =
// LabelByDesk; a message resolved BY LABEL, or unresolved, is left alone; and
// the ingest entry (ApplyAreaLabelForNewMessage) appends no change row of its
// own while the backfill entry (MaterializeAreaLabel) does, exactly once.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// seedAreaLabelMsg inserts a bare message with an optional classified_tab —
// the same minimal shape areas_resolve_integration_test.go's seedMsg uses,
// repeated here rather than shared across _test.go files in different
// packages of the same suite (Go does not let a non-exported helper cross a
// build-tag file boundary any more cleanly than copying six lines).
func seedAreaLabelMsg(t *testing.T, h *harness, userID string, tag string, classified tabs.Tab) ulid.ULID {
	t.Helper()
	hash := sha256.Sum256([]byte("arealabel-" + userID + "-" + tag))
	mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1,$2,10,20,$3)`, userID, hash[:], NewULID().String())
	id := NewULID()
	var classifiedArg any
	if classified != "" {
		classifiedArg = string(classified)
	}
	mustExec(t, h, `
		INSERT INTO app.messages (id, user_id, content_hash, subject, received_at,
		    size_bytes, classified_tab, classifier_version)
		VALUES ($1,$2,$3,$4,now(),10,$5::app.category_tab,$6)`,
		id.String(), userID, hash[:], tag, classifiedArg, tabs.ClassifierVersion)
	return id
}

// attachAreaLabel attaches label to msg with the DEFAULT applied_by ('user') —
// the schema's own default (db/schema.sql), so a bare INSERT with no
// applied_by column is exactly "the user filed this by hand".
func attachAreaLabelAsUser(t *testing.T, h *harness, userID string, msg, label ulid.ULID) {
	t.Helper()
	mustExec(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
		userID, msg.String(), label.String())
}

func appliedByOf(t *testing.T, h *harness, userID string, msg, label ulid.ULID) (string, bool) {
	t.Helper()
	var by string
	err := h.super.QueryRow(context.Background(),
		`SELECT applied_by FROM app.message_labels WHERE user_id=$1 AND message_id=$2 AND label_id=$3`,
		userID, msg.String(), label.String()).Scan(&by)
	if err != nil {
		if isNoRows(err) {
			return "", false
		}
		t.Fatalf("read applied_by: %v", err)
	}
	return by, true
}

// TestApplyAreaLabelOnlyForTabDecidedMail pins the whole of applyAreaLabel's
// contract in one place: only a message that resolves BY TAB gets its area's
// own label, with applied_by = 'desk'; a message the labels already decided,
// one nobody has looked at, and one whose own label a HUMAN already applied
// are all left exactly as they were.
func TestApplyAreaLabelOnlyForTabDecidedMail(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "aal-"+NewULID().String())
	userID := mustULID(t, u.ID)

	// Family: fed by an EXTRA label (Vip) beyond its own — a message carrying
	// Vip resolves ByLabel without ever touching Family's own label.
	// Updates: fed by tabs.Updates — a message with that classified tab and NO
	// labels resolves ByTab, which is the one case applyAreaLabel acts on.
	var familyID, updatesID, vipLabelID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		vipLabelID, e = tx.CreateLabel(ctx, LabelRow{Name: "Vip-" + u.Tag})
		if e != nil {
			return e
		}
		familyID, e = tx.CreateArea(ctx, AreaRow{Name: "Family-" + u.Tag, Position: 0,
			Feeds: []AreaFeed{{LabelID: &vipLabelID}}})
		if e != nil {
			return e
		}
		updatesID, e = tx.CreateArea(ctx, AreaRow{Name: "Updates-" + u.Tag, Position: 1,
			Feeds: []AreaFeed{{Tab: tabs.Updates}}})
		return e
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	var familyOwnLabel, updatesOwnLabel ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetAreas(ctx, []ulid.ULID{familyID, updatesID})
		if e != nil {
			return e
		}
		for _, v := range views {
			switch v.ID {
			case familyID:
				familyOwnLabel = v.LabelID
			case updatesID:
				updatesOwnLabel = v.LabelID
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read own labels: %v", err)
	}

	tabDecided := seedAreaLabelMsg(t, h, u.ID, "tab-decided", tabs.Updates)
	labelDecided := seedAreaLabelMsg(t, h, u.ID, "label-decided", "")
	attachAreaLabelAsUser(t, h, u.ID, labelDecided, vipLabelID)
	unsorted := seedAreaLabelMsg(t, h, u.ID, "unsorted", "")
	alreadyOwn := seedAreaLabelMsg(t, h, u.ID, "already-own", tabs.Updates)
	attachAreaLabelAsUser(t, h, u.ID, alreadyOwn, updatesOwnLabel)

	emailChanges := func() int64 {
		var n int64
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			var e error
			n, e = tx.ChangeState(ctx, ObjectEmail)
			return e
		}); err != nil {
			t.Fatalf("read Email change state: %v", err)
		}
		return n
	}

	// 1. tab-decided: gets Updates' own label, applied_by = 'desk', and exactly
	// one Email `updated` change row.
	before := emailChanges()
	var areaID *ulid.ULID
	var applied bool
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		areaID, applied, e = tx.MaterializeAreaLabel(ctx, tabDecided)
		return e
	}); err != nil {
		t.Fatalf("materialize tab-decided: %v", err)
	}
	if !applied || areaID == nil || *areaID != updatesID {
		t.Fatalf("tab-decided: applied=%v areaID=%v, want applied=true area=%s", applied, areaID, updatesID)
	}
	if by, ok := appliedByOf(t, h, u.ID, tabDecided, updatesOwnLabel); !ok || by != LabelByDesk {
		t.Errorf("tab-decided: applied_by = %q (present=%v), want %q", by, ok, LabelByDesk)
	}
	if got := emailChanges() - before; got != 1 {
		t.Errorf("tab-decided: Email change rows = %d, want 1", got)
	}

	// A second call is a true no-op: the row exists, so ON CONFLICT DO NOTHING
	// fires, applied is false, and NOTHING is appended.
	before2 := emailChanges()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, applied, e := tx.MaterializeAreaLabel(ctx, tabDecided)
		if e != nil {
			return e
		}
		if applied {
			t.Error("second MaterializeAreaLabel on the same message reported applied=true")
		}
		return nil
	}); err != nil {
		t.Fatalf("materialize tab-decided (second): %v", err)
	}
	if got := emailChanges() - before2; got != 0 {
		t.Errorf("second call on tab-decided appended %d Email change row(s), want 0", got)
	}

	// 2. label-decided: Family's EXTRA label already decided it (ByLabel);
	// Family's own label must never appear on this message, and nothing is
	// appended.
	before = emailChanges()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		areaID, applied, e = tx.MaterializeAreaLabel(ctx, labelDecided)
		return e
	}); err != nil {
		t.Fatalf("materialize label-decided: %v", err)
	}
	if applied || areaID == nil || *areaID != familyID {
		t.Errorf("label-decided: applied=%v areaID=%v, want applied=false area=%s", applied, areaID, familyID)
	}
	if _, ok := appliedByOf(t, h, u.ID, labelDecided, familyOwnLabel); ok {
		t.Error("label-decided message was given Family's own label, but it resolved ByLabel via Vip already")
	}
	if got := emailChanges() - before; got != 0 {
		t.Errorf("label-decided: Email change rows = %d, want 0", got)
	}

	// 3. unsorted: no labels, no verdict — Needs sorting, untouched.
	before = emailChanges()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		areaID, applied, e = tx.MaterializeAreaLabel(ctx, unsorted)
		return e
	}); err != nil {
		t.Fatalf("materialize unsorted: %v", err)
	}
	if applied || areaID != nil {
		t.Errorf("unsorted: applied=%v areaID=%v, want applied=false area=nil", applied, areaID)
	}
	if got := emailChanges() - before; got != 0 {
		t.Errorf("unsorted: Email change rows = %d, want 0", got)
	}

	// 4. already-own: the message already carries Updates' OWN label, applied
	// by the USER — which means it resolves ByLabel (the own label is itself a
	// feed), so this is untouched too, and the user's attribution survives.
	before = emailChanges()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		areaID, applied, e = tx.MaterializeAreaLabel(ctx, alreadyOwn)
		return e
	}); err != nil {
		t.Fatalf("materialize already-own: %v", err)
	}
	if applied || areaID == nil || *areaID != updatesID {
		t.Errorf("already-own: applied=%v areaID=%v, want applied=false area=%s", applied, areaID, updatesID)
	}
	if by, ok := appliedByOf(t, h, u.ID, alreadyOwn, updatesOwnLabel); !ok || by != LabelByUser {
		t.Errorf("already-own: applied_by = %q (present=%v), want %q (unchanged)", by, ok, LabelByUser)
	}
	if got := emailChanges() - before; got != 0 {
		t.Errorf("already-own: Email change rows = %d, want 0", got)
	}
}

// TestApplyAreaLabelForNewMessageAppendsNoChangeRow is the ingest half's own
// contract: the identical write as MaterializeAreaLabel, but NEVER a
// change_log row of its own — the caller's own Email `created` row is
// documented to cover it.
func TestApplyAreaLabelForNewMessageAppendsNoChangeRow(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "aani-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var areaID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		areaID, e = tx.CreateArea(ctx, AreaRow{Name: "Updates-" + u.Tag,
			Feeds: []AreaFeed{{Tab: tabs.Updates}}})
		return e
	}); err != nil {
		t.Fatalf("seed area: %v", err)
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
		t.Fatalf("read own label: %v", err)
	}

	msg := seedAreaLabelMsg(t, h, u.ID, "ingest-new", tabs.Updates)

	var before, after int64
	var applied bool
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		before, e = tx.ChangeState(ctx, ObjectEmail)
		if e != nil {
			return e
		}
		applied, e = tx.ApplyAreaLabelForNewMessage(ctx, msg)
		if e != nil {
			return e
		}
		after, e = tx.ChangeState(ctx, ObjectEmail)
		return e
	}); err != nil {
		t.Fatalf("ApplyAreaLabelForNewMessage: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true (the message classifies to Updates)")
	}
	if by, ok := appliedByOf(t, h, u.ID, msg, ownLabel); !ok || by != LabelByDesk {
		t.Errorf("applied_by = %q (present=%v), want %q", by, ok, LabelByDesk)
	}
	if after != before {
		t.Errorf("ApplyAreaLabelForNewMessage appended %d Email change row(s), want 0 (the caller's own "+
			"Email `created` row is documented to cover this)", after-before)
	}
}

// TestAreaNameAndLabelCollisionsAreSentinels pins #442 increment 3's follow-up
// to increment 2: CreateArea's two overlapping unique constraints now report
// DISTINCT sentinels (by constraint name, not by string-matching Postgres'
// own message), so internal/api/area.go's areaStoreSetError can name the
// right JMAP property instead of both.
func TestAreaNameAndLabelCollisionsAreSentinels(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "anls-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var alphaID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		alphaID, e = tx.CreateArea(ctx, AreaRow{Name: "Alpha-" + u.Tag})
		return e
	}); err != nil {
		t.Fatalf("seed Alpha: %v", err)
	}
	var alphaLabel ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetAreas(ctx, []ulid.ULID{alphaID})
		if e != nil {
			return e
		}
		alphaLabel = views[0].LabelID
		return nil
	}); err != nil {
		t.Fatalf("read Alpha's own label: %v", err)
	}

	// A name collision, case-insensitive (areas_user_lower_name).
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.CreateArea(ctx, AreaRow{Name: "ALPHA-" + u.Tag})
		if !errors.Is(e, ErrAreaNameTaken) {
			t.Errorf("create area name collision: err = %v, want errors.Is(err, ErrAreaNameTaken)", e)
		}
		// The transaction must still be usable after the refused create.
		if _, e := tx.CreateArea(ctx, AreaRow{Name: "Beta-" + u.Tag}); e != nil {
			return fmt.Errorf("write after a refused CreateArea (name): %w", e)
		}
		return nil
	}); err != nil {
		t.Fatalf("name collision: %v", err)
	}

	// A label collision: reusing Alpha's own label as another area's own label
	// (UNIQUE (user_id, label_id), areas_user_id_label_id_key).
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.CreateArea(ctx, AreaRow{Name: "Gamma-" + u.Tag, LabelID: &alphaLabel})
		if !errors.Is(e, ErrAreaLabelTaken) {
			t.Errorf("create area label collision: err = %v, want errors.Is(err, ErrAreaLabelTaken)", e)
		}
		if _, e := tx.CreateArea(ctx, AreaRow{Name: "Delta-" + u.Tag}); e != nil {
			return fmt.Errorf("write after a refused CreateArea (label): %w", e)
		}
		return nil
	}); err != nil {
		t.Fatalf("label collision: %v", err)
	}

	// UpdateArea's own rename path reports the same sentinel.
	betaName := "Beta-" + u.Tag
	var betaID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.ListAreas(ctx)
		if e != nil {
			return e
		}
		for _, v := range views {
			if v.Name == betaName {
				betaID = v.ID
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("find Beta: %v", err)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		alphaName := "Alpha-" + u.Tag
		_, e := tx.UpdateArea(ctx, betaID, AreaPatch{Name: &alphaName})
		if !errors.Is(e, ErrAreaNameTaken) {
			t.Errorf("update area name collision: err = %v, want errors.Is(err, ErrAreaNameTaken)", e)
		}
		return nil
	}); err != nil {
		t.Fatalf("rename collision: %v", err)
	}
}
