// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ml

// Integration coverage for the model registry against a real database + encrypted
// blob store (mailroom#35): Snapshot -> Activate -> ActiveModel round-trips the
// exact model bytes, Rollback re-activates an older version, Reset produces a
// fresh baseline with a broken lineage and makes it active, and Prune keeps N +
// the active one while reclaiming orphaned artifacts. Skips when the integration
// database is not configured (run via scripts/integration-test.sh).

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/store"
)

type fakeKeys struct{ master []byte }

func (f fakeKeys) MasterKey(context.Context) ([]byte, error) {
	return append([]byte(nil), f.master...), nil
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, crypto.KeySize)
	if _, err := crand.Read(k); err != nil {
		t.Fatalf("random key: %v", err)
	}
	return k
}

type mlFixture struct {
	super  *pgxpool.Pool
	reg    *Registry
	userID ulid.ULID
}

func newMLFixture(t *testing.T) *mlFixture {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()

	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)

	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	blobs, err := blob.NewFSBlobStore(t.TempDir(), randomKey(t))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	keys := fakeKeys{master: randomKey(t)}

	f := &mlFixture{super: super, reg: NewRegistry(st, blobs, keys), userID: ulid.Make()}
	if _, err := super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return f
}

func marshal(t *testing.T, m Model) []byte {
	t.Helper()
	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	return b
}

func TestRegistrySnapshotActivateRoundTrip(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	// A distinctively-trained model so a wrong round-trip is detectable.
	m := NewBaseline("priority")
	for i := 0; i < 50; i++ {
		m.Update(FeatureVector{"x": 1}, 1.0)
	}
	want := marshal(t, m)

	version, err := f.reg.Snapshot(ctx, f.userID, m)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
	if err := f.reg.Activate(ctx, f.userID, "priority", version); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	got, gotVer, err := f.reg.ActiveModel(ctx, f.userID, "priority")
	if err != nil {
		t.Fatalf("ActiveModel: %v", err)
	}
	if gotVer != 1 {
		t.Errorf("active version = %d, want 1", gotVer)
	}
	if !bytes.Equal(marshal(t, got), want) {
		t.Error("round-tripped model bytes differ from the snapshot (encrypted blob did not preserve the model)")
	}
}

func TestRegistryPriorityFallbackIsColdStart(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	// No active PRIORITY model: ActiveModel hands back the cold-start heuristic
	// (#36) at version 0 — NOT the feature-blind baseline. Its score varies with
	// the message's features (a known+direct message outranks list mail), which is
	// the whole point of replacing the flat-0.5 baseline fallback.
	m, version, err := f.reg.ActiveModel(ctx, f.userID, "priority")
	if err != nil {
		t.Fatalf("ActiveModel: %v", err)
	}
	if version != 0 {
		t.Errorf("version = %d, want 0 (no persisted model)", version)
	}
	if _, ok := m.(coldStartModel); !ok {
		t.Errorf("priority fallback is %T, want coldStartModel", m)
	}
	knownDirect := m.Score(featuresFromFacts(MessageFacts{
		HasCorresponded: true, OutboundCount: 8, ToCcCount: 1,
	}))
	listMail := m.Score(featuresFromFacts(MessageFacts{
		IsListMail: true, HasListUnsubscribe: true, ToCcCount: 50,
	}))
	if knownDirect <= listMail {
		t.Errorf("cold-start did not vary with features: known+direct=%v list=%v", knownDirect, listMail)
	}

	// A non-priority kind still falls back to the feature-blind baseline (0.5).
	lb, _, err := f.reg.ActiveModel(ctx, f.userID, "labeler")
	if err != nil {
		t.Fatalf("ActiveModel(labeler): %v", err)
	}
	if got := lb.Score(nil); got != 0.5 {
		t.Errorf("labeler fallback Score = %v, want 0.5 (fresh baseline)", got)
	}
}

// With a trained priority model ACTIVE, ActiveModel returns THAT model, not the
// cold-start fallback — the fallback only ever fills the no-model gap.
func TestRegistryActiveModelSupersedesColdStart(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	trained := NewBaseline("priority") // stands in for #33's trained model
	for i := 0; i < 50; i++ {
		trained.Update(nil, 1.0) // drive its score well away from any cold-start value
	}
	v, err := f.reg.Snapshot(ctx, f.userID, trained)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := f.reg.Activate(ctx, f.userID, "priority", v); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	got, gotVer, err := f.reg.ActiveModel(ctx, f.userID, "priority")
	if err != nil {
		t.Fatalf("ActiveModel: %v", err)
	}
	if gotVer != v {
		t.Errorf("active version = %d, want %d (trained model, not fallback)", gotVer, v)
	}
	if _, isColdStart := got.(coldStartModel); isColdStart {
		t.Error("ActiveModel returned the cold-start fallback despite an active trained model")
	}
	if !bytes.Equal(marshal(t, got), marshal(t, trained)) {
		t.Error("ActiveModel did not return the activated trained model")
	}
}

func TestRegistryRollback(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	mv1 := NewBaseline("priority")
	for i := 0; i < 10; i++ {
		mv1.Update(nil, 1.0)
	}
	v1Bytes := marshal(t, mv1)
	v1, err := f.reg.Snapshot(ctx, f.userID, mv1)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Activate(ctx, f.userID, "priority", v1); err != nil {
		t.Fatal(err)
	}

	mv2 := NewBaseline("priority")
	for i := 0; i < 10; i++ {
		mv2.Update(nil, 0.0)
	}
	v2, err := f.reg.Snapshot(ctx, f.userID, mv2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Activate(ctx, f.userID, "priority", v2); err != nil {
		t.Fatal(err)
	}

	// Rollback to v1: ActiveModel must return v1's model bytes again.
	if err := f.reg.Rollback(ctx, f.userID, "priority", v1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	got, gotVer, err := f.reg.ActiveModel(ctx, f.userID, "priority")
	if err != nil {
		t.Fatal(err)
	}
	if gotVer != v1 {
		t.Errorf("after rollback version = %d, want %d", gotVer, v1)
	}
	if !bytes.Equal(marshal(t, got), v1Bytes) {
		t.Error("rollback did not restore v1's model")
	}
}

func TestRegistryReset(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	trained := NewBaseline("priority")
	for i := 0; i < 100; i++ {
		trained.Update(nil, 1.0)
	}
	v1, err := f.reg.Snapshot(ctx, f.userID, trained)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Activate(ctx, f.userID, "priority", v1); err != nil {
		t.Fatal(err)
	}

	resetVer, err := f.reg.Reset(ctx, f.userID, "priority")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// The active model is now a fresh baseline (Score 0.5), and the reset row has a
	// broken lineage (parent NULL) even though a prior active version existed.
	got, gotVer, err := f.reg.ActiveModel(ctx, f.userID, "priority")
	if err != nil {
		t.Fatal(err)
	}
	if gotVer != resetVer {
		t.Errorf("active version = %d, want reset version %d", gotVer, resetVer)
	}
	if s := got.Score(nil); s != 0.5 {
		t.Errorf("post-reset Score = %v, want 0.5", s)
	}

	versions, err := f.reg.ListVersions(ctx, f.userID, "priority")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if v.Version == resetVer {
			if v.ParentID != nil {
				t.Errorf("reset version parent = %v, want nil (lineage broken)", v.ParentID)
			}
			if !v.Active {
				t.Error("reset version is not active")
			}
		}
	}
	// The old trained version still exists (Reset never deletes).
	if len(versions) != 2 {
		t.Errorf("versions = %d, want 2 (trained + reset both on record)", len(versions))
	}
}

func TestRegistryPrune(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		m := NewBaseline("priority")
		for j := 0; j <= i; j++ {
			m.Update(nil, 1.0) // distinct training -> distinct bytes -> distinct hashes
		}
		v, err := f.reg.Snapshot(ctx, f.userID, m)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := f.reg.Activate(ctx, f.userID, "priority", v); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Keep newest 1 (v5) + active (v2): prune v1,v3,v4.
	deleted, err := f.reg.Prune(ctx, f.userID, "priority", 1)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	versions, err := f.reg.ListVersions(ctx, f.userID, "priority")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("surviving versions = %d, want 2 (active v2 + newest v5)", len(versions))
	}

	// The active model's artifact survived and still loads.
	if _, _, err := f.reg.ActiveModel(ctx, f.userID, "priority"); err != nil {
		t.Errorf("ActiveModel after prune: %v (active artifact must never be reclaimed)", err)
	}
}

// TestSnapshotRecordsTheBlobRow is #316: the row the registry never wrote.
//
// ⚠ WHAT WAS BROKEN IS NOT WHAT THIS LOOKS LIKE IT TESTS. Nothing user-facing
// failed without the row — ActiveModel reads through blobs.Get, which is
// content-addressed on disk and never consults app.blobs, so the registry worked
// perfectly while every artifact it produced was a file no query could reach.
// The absence of an error proved nothing. What the missing row actually cost:
// the bytes leaked one file per distinct version forever, the reclaim guard
// #300 added to protect them could not fire because it protects a row that did
// not exist, and a hard-delete sweep driven off app.blobs (#54) would walk past
// every model a user owns.
//
// So this asserts on the ROW, and on each field of it, rather than on a
// round-trip that was green throughout the defect.
func TestSnapshotRecordsTheBlobRow(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	m := NewBaseline("priority")
	m.Update(nil, 1.0) // distinctive bytes, so a wrong hash is detectable
	wantPlain := int64(len(marshal(t, m)))

	version, err := f.reg.Snapshot(ctx, f.userID, m)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	var artifact []byte
	if err := f.super.QueryRow(ctx,
		`SELECT artifact_hash FROM app.ml_models
		  WHERE user_id = $1 AND model_kind = 'priority' AND version = $2`,
		f.userID.String(), version).Scan(&artifact); err != nil {
		t.Fatalf("read ml_models row: %v", err)
	}

	var (
		plain, stored int64
		dekID         string
		envelopeVer   int
	)
	// Read as the superuser: RLS must not be what makes a missing row look
	// present or absent.
	err = f.super.QueryRow(ctx,
		`SELECT size_plain, size_stored, dek_id, envelope_ver FROM app.blobs
		  WHERE user_id = $1 AND content_hash = $2`,
		f.userID.String(), artifact).Scan(&plain, &stored, &dekID, &envelopeVer)
	if err != nil {
		t.Fatalf("the model artifact has NO app.blobs row (%v) — it is a file nothing can "+
			"account for, reclaim or delete: the #316 defect", err)
	}

	if plain != wantPlain {
		t.Errorf("size_plain = %d, want %d (the marshalled model). A size_stored-shaped "+
			"number here means the ciphertext length was recorded as the plaintext length",
			plain, wantPlain)
	}
	if stored <= plain {
		t.Errorf("size_stored = %d, want > size_plain %d (ciphertext carries an AEAD envelope)",
			stored, plain)
	}
	if dekID != ModelBlobSchemeV1.String() {
		t.Errorf("dek_id = %q, want the model scheme sentinel %q. Mail blobs and model "+
			"artifacts share one content-addressed namespace and are sealed under DIFFERENT "+
			"HKDF labels, so this column is what says which key opens these bytes",
			dekID, ModelBlobSchemeV1.String())
	}
	if envelopeVer != 1 {
		t.Errorf("envelope_ver = %d, want 1", envelopeVer)
	}
}

// TestReSnapshotOfAnUnchangedModelKeepsOneBlobRow covers the shape a real
// instance has: more model versions than distinct hashes, because an
// unchanged model re-snapshots to byte-identical bytes. The blob row is
// content-addressed, so those versions must SHARE one row rather than collide on
// its primary key — which is what InsertBlobMeta's ON CONFLICT DO NOTHING buys.
func TestReSnapshotOfAnUnchangedModelKeepsOneBlobRow(t *testing.T) {
	f := newMLFixture(t)
	ctx := context.Background()

	m := NewBaseline("priority")
	for i := 0; i < 3; i++ {
		if _, err := f.reg.Snapshot(ctx, f.userID, m); err != nil {
			t.Fatalf("Snapshot %d: %v", i+1, err)
		}
	}

	var models, blobs int
	if err := f.super.QueryRow(ctx,
		`SELECT count(*) FROM app.ml_models WHERE user_id = $1 AND model_kind = 'priority'`,
		f.userID.String()).Scan(&models); err != nil {
		t.Fatalf("count models: %v", err)
	}
	if err := f.super.QueryRow(ctx,
		`SELECT count(*) FROM app.blobs WHERE user_id = $1`,
		f.userID.String()).Scan(&blobs); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if models != 3 {
		t.Errorf("ml_models rows = %d, want 3 (every snapshot is on record)", models)
	}
	if blobs != 1 {
		t.Errorf("app.blobs rows = %d, want 1 (identical bytes are one content-addressed blob)", blobs)
	}
}
