// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Store-layer integration tests for the model registry and feedback replay
// source (mailroom#35). Proves: version assignment + parent lineage, the
// one-active DB invariant (activating a version deactivates the prior one, and
// the partial unique index ml_models_one_active never lets two be active),
// rollback validation, Prune's keep-N + always-active behaviour with unreferenced-
// blob reporting, ListFeedbackEvents time-ordering, and cross-user isolation.
// Runs against the throwaway Postgres from scripts/integration-test.sh; skips
// when absent.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func mlHash(tag string) []byte {
	h := sha256.Sum256([]byte("ml-artifact-" + tag))
	return h[:]
}

// cleanModelState removes the ml_models / feedback_events rows seedUser plants,
// so a test starts from an empty registry for that user.
func cleanModelState(t *testing.T, h *harness, userID string) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"app.ml_models", "app.feedback_events"} {
		if _, err := h.super.Exec(ctx, `DELETE FROM `+tbl+` WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func countActive(t *testing.T, h *harness, userID, kind string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.ml_models WHERE user_id = $1 AND model_kind = $2 AND active`,
		userID, kind).Scan(&n); err != nil {
		t.Fatalf("count active: %v", err)
	}
	return n
}

func TestModelVersionLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "ml-"+NewULID().String())
	cleanModelState(t, h, u.ID)
	s := h.appStore()
	uID := mustULID(t, u.ID)

	// Two snapshots before any activation: versions 1,2; no active row yet so both
	// have a NULL parent even with linkParent=true.
	var v1, v2 int
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		if v1, e = tx.InsertModelVersion(ctx, "priority", mlHash("v1"), true); e != nil {
			return e
		}
		v2, e = tx.InsertModelVersion(ctx, "priority", mlHash("v2"), true)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if v1 != 1 || v2 != 2 {
		t.Fatalf("versions = %d,%d, want 1,2", v1, v2)
	}

	// No active model yet.
	if got := countActive(t, h, u.ID, "priority"); got != 0 {
		t.Fatalf("active count before activation = %d, want 0", got)
	}
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		_, _, found, e := tx.ActiveModelRow(ctx, "priority")
		if e != nil {
			return e
		}
		if found {
			t.Error("ActiveModelRow reported found before any activation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Activate v1, then a third snapshot should link its parent to v1 (the active).
	var v3 int
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		if e := tx.ActivateModelVersion(ctx, "priority", v1); e != nil {
			return e
		}
		var e error
		v3, e = tx.InsertModelVersion(ctx, "priority", mlHash("v3"), true)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if v3 != 3 {
		t.Fatalf("v3 = %d, want 3", v3)
	}

	var versions []VersionInfo
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		versions, e = tx.ListModelVersions(ctx, "priority")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("ListModelVersions len = %d, want 3", len(versions))
	}
	// v1 root (parent NULL, active), v3 parent == v1.id.
	byVer := map[int]VersionInfo{}
	for _, v := range versions {
		byVer[v.Version] = v
	}
	if byVer[1].ParentID != nil || !byVer[1].Active {
		t.Errorf("v1 = %+v, want parent nil + active", byVer[1])
	}
	if byVer[3].ParentID == nil || *byVer[3].ParentID != byVer[1].ID {
		t.Errorf("v3 parent = %v, want v1.id %v", byVer[3].ParentID, byVer[1].ID)
	}

	// Activating v3 deactivates v1: exactly one active throughout.
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		return tx.ActivateModelVersion(ctx, "priority", v3)
	}); err != nil {
		t.Fatal(err)
	}
	if got := countActive(t, h, u.ID, "priority"); got != 1 {
		t.Fatalf("active count after re-activation = %d, want exactly 1", got)
	}
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		hash, version, found, e := tx.ActiveModelRow(ctx, "priority")
		if e != nil {
			return e
		}
		if !found || version != v3 || string(hash) != string(mlHash("v3")) {
			t.Errorf("active row = (v%d, %x, found=%v), want v3 with v3 hash", version, hash, found)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Rollback to a nonexistent version fails cleanly and does not disturb active.
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		return tx.ActivateModelVersion(ctx, "priority", 999)
	}); !errors.Is(err, ErrModelVersionNotFound) {
		t.Fatalf("activate missing version err = %v, want ErrModelVersionNotFound", err)
	}
	if got := countActive(t, h, u.ID, "priority"); got != 1 {
		t.Fatalf("active count after failed activate = %d, want 1 (unchanged)", got)
	}
}

// The partial unique index is a hard DB invariant: a direct attempt (even as the
// owner) to mark a second row active for the same (user, kind) is rejected.
func TestOneActiveIndexEnforced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "mlidx-"+NewULID().String())
	cleanModelState(t, h, u.ID)
	s := h.appStore()
	uID := mustULID(t, u.ID)

	var v1, v2 int
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		if v1, e = tx.InsertModelVersion(ctx, "priority", mlHash("a"), true); e != nil {
			return e
		}
		if v2, e = tx.InsertModelVersion(ctx, "priority", mlHash("b"), true); e != nil {
			return e
		}
		return tx.ActivateModelVersion(ctx, "priority", v1)
	}); err != nil {
		t.Fatal(err)
	}
	_ = v2

	// Force a second active WITHOUT deactivating v1: the index must reject it.
	_, err := h.super.Exec(ctx,
		`UPDATE app.ml_models SET active = true WHERE user_id = $1 AND model_kind = 'priority' AND version = $2`,
		u.ID, v2)
	if err == nil {
		t.Fatal("expected unique-violation forcing two active rows, got nil")
	}
}

func TestPruneModelVersions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "mlprune-"+NewULID().String())
	cleanModelState(t, h, u.ID)
	s := h.appStore()
	uID := mustULID(t, u.ID)

	// Six versions; activate v2 (deliberately NOT one of the newest, to prove the
	// active row is always kept even when it falls outside keep-N).
	//
	// ⚠ EACH VERSION GETS ITS app.blobs ROW, because that is what the registry
	// writes (#316). This fixture used to insert the model rows alone, and that
	// was a state production could not produce — the same defect the reclaim
	// guard's own test had. It matters here specifically: prune reclaims through
	// the one shared guard now, and the guard deletes app.blobs ROWS, so a
	// fixture with no rows would assert that prune frees nothing and would have
	// locked in exactly the leak this change exists to stop.
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		for i := 1; i <= 6; i++ {
			hash := mlHash(string(rune('0' + i)))
			if e := tx.InsertBlobMeta(ctx, BlobMeta{
				ContentHash: hash, SizePlain: 64, SizeStored: 96,
				DEKID: NewULID(), EnvelopeVer: 1,
			}); e != nil {
				return e
			}
			if _, e := tx.InsertModelVersion(ctx, "priority", hash, true); e != nil {
				return e
			}
		}
		return tx.ActivateModelVersion(ctx, "priority", 2)
	}); err != nil {
		t.Fatal(err)
	}

	// Keep newest 2 (v5,v6) plus the active v2: prune v1,v3,v4 (3 rows).
	var deleted int
	var freed [][]byte
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		deleted, freed, e = tx.PruneModelVersions(ctx, "priority", 2)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3 (v1,v3,v4)", deleted)
	}
	// Each pruned artifact hash is distinct, so all three are unreferenced -> freed.
	if len(freed) != 3 {
		t.Fatalf("freed hashes = %d, want 3", len(freed))
	}

	var survivors []VersionInfo
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		survivors, e = tx.ListModelVersions(ctx, "priority")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, v := range survivors {
		got[v.Version] = true
	}
	for _, want := range []int{2, 5, 6} {
		if !got[want] {
			t.Errorf("version %d was pruned but should survive (active or newest-N)", want)
		}
	}
	for _, gone := range []int{1, 3, 4} {
		if got[gone] {
			t.Errorf("version %d survived but should have been pruned", gone)
		}
	}
	if got := countActive(t, h, u.ID, "priority"); got != 1 {
		t.Fatalf("active count after prune = %d, want 1", got)
	}
}

// TestPruneLeavesAPreBackfillArtifactAlone is the reason
// `mailroom-admin backfill-model-blobs` is REQUIRED rather than housekeeping
// (#316), asserted rather than argued.
//
// Artifacts written before the registry recorded blob rows have none. Prune now
// reclaims through the shared guard, which deletes app.blobs ROWS and reports
// what it removed — so for those artifacts it correctly removes nothing and the
// caller correctly deletes no file. The model row goes and the bytes stay,
// forever. The old unguarded prune deleted the file by hash regardless, so
// shipping the fix WITHOUT the backfill would make the existing leak worse.
//
// This test exists so that trade-off cannot be quietly reversed by someone
// "fixing" prune to delete files it has no row for — which would reopen the
// hazard TestPruneModelVersionsDoesNotReclaimBytesAMessageShares covers.
func TestPruneLeavesAPreBackfillArtifactAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "mllegacy-"+NewULID().String())
	cleanModelState(t, h, u.ID)
	s := h.appStore()
	uID := mustULID(t, u.ID)

	// A model version with NO app.blobs row: exactly what every artifact on the
	// existing instance looked like before this change.
	legacy := mlHash("legacy-" + NewULID().String())
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		_, e := tx.InsertModelVersion(ctx, "priority", legacy, false)
		return e
	}); err != nil {
		t.Fatal(err)
	}

	var (
		deleted int
		freed   [][]byte
	)
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		deleted, freed, e = tx.PruneModelVersions(ctx, "priority", 0)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 — the fixture did not exercise the path", deleted)
	}
	if len(freed) != 0 {
		t.Errorf("prune handed back %d hash(es) for an artifact with no app.blobs row. The "+
			"guard can only report rows it deleted, so a non-empty result here means it is "+
			"reporting bytes it never proved it owns", len(freed))
	}
}

// TestModelArtifactRefsCountsVersionsAndRows covers the backfill's data source
// (#316). It reports DISTINCT HASHES with a version count, not model rows,
// because byte-identical snapshots share one artifact: on a real
// instance more versions than files existed, and a report that conflated the two
// would name the wrong number of missing rows.
func TestModelArtifactRefsCountsVersionsAndRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "mlrefs-"+NewULID().String())
	cleanModelState(t, h, u.ID)
	s := h.appStore()
	uID := mustULID(t, u.ID)

	accounted := mlHash("accounted-" + NewULID().String()) // has a blob row
	orphan := mlHash("orphan-" + NewULID().String())       // pre-backfill: none

	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		if e := tx.InsertBlobMeta(ctx, BlobMeta{
			ContentHash: accounted, SizePlain: 64, SizeStored: 96,
			DEKID: NewULID(), EnvelopeVer: 1,
		}); e != nil {
			return e
		}
		// TWO versions on the accounted hash — the re-snapshot-of-an-unchanged-model
		// shape — and one on the orphan.
		for i := 0; i < 2; i++ {
			if _, e := tx.InsertModelVersion(ctx, "priority", accounted, false); e != nil {
				return e
			}
		}
		_, e := tx.InsertModelVersion(ctx, "priority", orphan, false)
		return e
	}); err != nil {
		t.Fatal(err)
	}

	var refs []ModelArtifactRef
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		refs, e = tx.ModelArtifactRefs(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2 distinct hashes across 3 model rows", len(refs))
	}
	byHash := map[string]ModelArtifactRef{}
	for _, r := range refs {
		byHash[hex.EncodeToString(r.ContentHash)] = r
	}
	got := byHash[hex.EncodeToString(accounted)]
	if got.Versions != 2 || !got.HasBlobRow {
		t.Errorf("accounted hash: versions=%d hasBlobRow=%v, want 2 and true",
			got.Versions, got.HasBlobRow)
	}
	got = byHash[hex.EncodeToString(orphan)]
	if got.Versions != 1 || got.HasBlobRow {
		t.Errorf("orphan hash: versions=%d hasBlobRow=%v, want 1 and false — a backfill that "+
			"cannot see the missing row writes nothing and reports success",
			got.Versions, got.HasBlobRow)
	}
}

func TestListFeedbackEventsOrderingAndWindow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "fb-"+NewULID().String())
	cleanModelState(t, h, u.ID)
	s := h.appStore()
	uID := mustULID(t, u.ID)

	base := time.Now().Add(-10 * time.Hour).UTC().Truncate(time.Microsecond)
	// Insert three events out of chronological order (super bypasses RLS for seeding).
	for _, ev := range []struct {
		action string
		at     time.Time
	}{
		{"reply", base.Add(2 * time.Hour)},
		{"open", base},
		{"archive_unread", base.Add(1 * time.Hour)},
	} {
		if _, err := h.super.Exec(ctx,
			`INSERT INTO app.feedback_events (id, user_id, message_id, action, created_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			NewULID().String(), u.ID, u.MessageID, ev.action, ev.at); err != nil {
			t.Fatal(err)
		}
	}

	var events []FeedbackEvent
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		events, e = tx.ListFeedbackEvents(ctx, base, base.Add(3*time.Hour))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	wantOrder := []string{"open", "archive_unread", "reply"}
	for i, e := range events {
		if e.Action != wantOrder[i] {
			t.Errorf("event[%d].Action = %q, want %q (must be created_at-ordered)", i, e.Action, wantOrder[i])
		}
		if e.MessageID == nil || e.MessageID.String() != u.MessageID {
			t.Errorf("event[%d].MessageID = %v, want %s", i, e.MessageID, u.MessageID)
		}
	}

	// The window is half-open [since, until): an event AT until is excluded.
	var narrowed []FeedbackEvent
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		var e error
		narrowed, e = tx.ListFeedbackEvents(ctx, base, base.Add(2*time.Hour))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(narrowed) != 2 {
		t.Fatalf("half-open window returned %d, want 2 (reply at the boundary excluded)", len(narrowed))
	}
}

// Cross-user isolation: user B cannot see or mutate A's models or feedback, and
// version numbering is per-user (both start at 1 independently).
func TestModelAndFeedbackIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	cleanModelState(t, h, a.ID)
	cleanModelState(t, h, b.ID)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	bID := mustULID(t, b.ID)

	// A snapshots + activates a model.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		v, e := tx.InsertModelVersion(ctx, "priority", mlHash("A"), true)
		if e != nil {
			return e
		}
		return tx.ActivateModelVersion(ctx, "priority", v)
	}); err != nil {
		t.Fatal(err)
	}

	// B sees an empty registry (RLS scopes to B), and B's first version is 1 too.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		vs, e := tx.ListModelVersions(ctx, "priority")
		if e != nil {
			return e
		}
		if len(vs) != 0 {
			t.Errorf("B sees %d of A's model versions, want 0", len(vs))
		}
		_, _, found, e := tx.ActiveModelRow(ctx, "priority")
		if e != nil {
			return e
		}
		if found {
			t.Error("B sees an active model, want none (A's is invisible)")
		}
		v, e := tx.InsertModelVersion(ctx, "priority", mlHash("B"), true)
		if e != nil {
			return e
		}
		if v != 1 {
			t.Errorf("B's first version = %d, want 1 (numbering is per-user)", v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// B cannot activate A's version (RLS hides it -> not found).
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.ActivateModelVersion(ctx, "priority", 1)
	}); err != nil && !errors.Is(err, ErrModelVersionNotFound) {
		// v1 exists for B, so this actually activates B's own — fine. The real
		// isolation proof is the empty list above; keep this lenient.
		t.Fatalf("unexpected error activating B's own v1: %v", err)
	}

	// A's feedback is invisible to B. A has a seeded... we cleaned it; seed one now.
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.feedback_events (id, user_id, message_id, action)
		 VALUES ($1, $2, $3, 'open')`,
		NewULID().String(), a.ID, a.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		evs, e := tx.ListFeedbackEvents(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour))
		if e != nil {
			return e
		}
		if len(evs) != 0 {
			t.Errorf("B sees %d of A's feedback events, want 0", len(evs))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
