// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// #625 §4: delete-user --apply used to delete the search index and the blob
// prefix, then run the database transaction — on a database missing
// app.users' owner-delete policy that transaction removes ZERO rows (the same
// silent no-op internal/store/delete_policy_integration_test.go measures), so
// the user's files were already gone from disk by the time anything noticed
// the row was still there. checkDeletePolicy already computed this; it just
// was never consulted before the destructive half ran.
//
// This drops the REAL users_owner_delete policy (migration 00040) on the
// shared integration database, exactly as internal/store's delete-policy
// tests do, and restores it via t.Cleanup — safe because
// scripts/integration-test.sh runs `go test -p 1` (one package at a time) and
// nothing in this package runs its own tests in parallel (no t.Parallel), so
// no other test observes the dropped policy.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/store"
)

// quietDeleteUserLogger discards logs so test output stays focused on
// assertion failures.
func quietDeleteUserLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// deleteUserTestMaster must be exactly crypto.KeySize (32) bytes: DeriveKey
// (the blob path key derivation openBlobStore uses) rejects anything else.
var deleteUserTestMaster = []byte("mailroom-admin-delete-user-key!!")

// deleteUserHarness mirrors accountsReportHarness: a superuser pool for
// fixture seeding and schema surgery (dropping/restoring the delete policy),
// and a Store backed by the mailroom_app pool — the exact role deleteUserCore
// runs as in production.
type deleteUserHarness struct {
	super *pgxpool.Pool
	store *store.Store
}

func newDeleteUserHarness(t *testing.T) *deleteUserHarness {
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
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)
	return &deleteUserHarness{super: super, store: store.NewFromPool(app)}
}

func seedDeleteUserFixture(t *testing.T, super *pgxpool.Pool, tag string) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email, display_name) VALUES ($1,$2,$3,$4)`,
		id.String(), "oidc-"+tag, tag+"@example.com", "User "+tag); err != nil {
		t.Fatalf("seed user %s: %v", tag, err)
	}
	return id
}

// dropDeletePolicy removes the REAL users_owner_delete policy (migration
// 00040) — committed, not rolled back, because deleteUserCore runs the
// mailroom_app-pool store on its OWN connections, which would never see an
// uncommitted change on the super pool's transaction. Always restored.
func dropDeletePolicy(t *testing.T, super *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := super.Exec(ctx, `DROP POLICY IF EXISTS users_owner_delete ON app.users`); err != nil {
		t.Fatalf("drop users_owner_delete: %v", err)
	}
	t.Cleanup(func() {
		if _, err := super.Exec(context.Background(),
			`CREATE POLICY users_owner_delete ON app.users FOR DELETE TO mailroom_owner USING (true)`); err != nil {
			t.Fatalf("restore users_owner_delete: %v", err)
		}
	})
}

// seedBlobAndIndex writes one blob file under userID's derived prefix (via the
// SAME openBlobStore this package's delete-user code path uses, so the path
// derivation cannot drift between fixture and code under test) and one file
// under the search index directory, and returns the search index directory so
// the caller can check it afterwards.
func seedBlobAndIndex(t *testing.T, ctx context.Context, d *deps, userID ulid.ULID, blobRoot, searchRoot string) (blobPrefix, indexDir string) {
	t.Helper()
	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	dek := bytes.Repeat([]byte{0x11}, 32)
	if _, err := blobs.Put(ctx, userID, dek, strings.NewReader("fixture blob bytes")); err != nil {
		t.Fatalf("put fixture blob: %v", err)
	}

	indexDir = filepath.Join(searchRoot, userID.String())
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture index dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, "fixture.seg"), []byte("fixture index bytes"), 0o644); err != nil {
		t.Fatalf("write fixture index file: %v", err)
	}

	enum, ok := blobs.(*blob.FSBlobStore)
	if !ok {
		t.Fatalf("blob store %T is not *blob.FSBlobStore", blobs)
	}
	// ⚠ UserPrefix returns the bare derived HMAC hex string, NOT joined with
	// root (fs.go: `UserPrefix(userID) string { return s.userPrefix(userID) }`)
	// — the same thing deleteuser.go's own report prints verbatim. Joining it
	// here is what makes os.Stat below check the real on-disk location instead
	// of a relative path under the test binary's working directory.
	return filepath.Join(blobRoot, enum.UserPrefix(userID)), indexDir
}

// silenceReports redirects the report package's print helpers away from
// os.Stdout for the duration of a test — these tests assert on return values
// and the filesystem, not on report text.
func silenceReports(t *testing.T) {
	t.Helper()
	prev := reportOut
	reportOut = io.Discard
	t.Cleanup(func() { reportOut = prev })
}

// TestDeleteUserApplyRefusesWhenDeletePolicyIsAbsent is #625 §4's pin: --apply
// must check the delete policy FIRST and refuse — non-zero error, nothing
// deleted — rather than removing the search index and blob prefix and only
// then running a database transaction that silently deletes zero rows.
func TestDeleteUserApplyRefusesWhenDeletePolicyIsAbsent(t *testing.T) {
	silenceReports(t)
	h := newDeleteUserHarness(t)
	ctx := context.Background()
	tag := "delete-user-refuse-" + store.NewULID().String()
	userID := seedDeleteUserFixture(t, h.super, tag)

	blobRoot := t.TempDir()
	searchRoot := t.TempDir()
	t.Setenv("MAILROOM_BLOB_ROOT", blobRoot)
	t.Setenv("MAILROOM_SEARCH_ROOT", searchRoot)

	d := &deps{
		log:   quietDeleteUserLogger(),
		keys:  fakeKeyProvider{key: deleteUserTestMaster},
		store: h.store,
		close: func() {},
	}
	blobPrefix, indexDir := seedBlobAndIndex(t, ctx, d, userID, blobRoot, searchRoot)

	dropDeletePolicy(t, h.super)

	err := deleteUserCore(ctx, d, userID, true, userID.String(), "test-actor", false)
	if err == nil {
		t.Fatal("deleteUserCore(apply=true) with no delete policy = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "refusing --apply") {
		t.Errorf("error = %v, want it to say it is refusing --apply", err)
	}

	// The load-bearing assertions: NOTHING was touched. A refusal that still
	// deleted the files first would look identical from the error message
	// alone, which is exactly the bug (files deleted, then the no-op DB
	// transaction "succeeded").
	if _, err := os.Stat(blobPrefix); err != nil {
		t.Errorf("blob prefix %s: %v — the blob file must still exist after a refused --apply", blobPrefix, err)
	}
	if _, err := os.Stat(indexDir); err != nil {
		t.Errorf("search index dir %s: %v — it must still exist after a refused --apply", indexDir, err)
	}
	var exists bool
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		exists, e = tx.SelfExists(ctx)
		return e
	}); err != nil {
		t.Fatalf("check user row: %v", err)
	}
	if !exists {
		t.Error("app.users row is gone after a refused --apply — the database was touched despite the refusal")
	}
}

// TestDeleteUserApplyProceedsWhenDeletePolicyIsPresent is the positive
// control: without it, the refusal test above would pass even if
// deleteUserCore refused UNCONDITIONALLY, which would be a --apply that never
// applies anything rather than one that gates correctly on the real policy
// state.
func TestDeleteUserApplyProceedsWhenDeletePolicyIsPresent(t *testing.T) {
	silenceReports(t)
	h := newDeleteUserHarness(t)
	ctx := context.Background()
	tag := "delete-user-proceed-" + store.NewULID().String()
	userID := seedDeleteUserFixture(t, h.super, tag)

	blobRoot := t.TempDir()
	searchRoot := t.TempDir()
	t.Setenv("MAILROOM_BLOB_ROOT", blobRoot)
	t.Setenv("MAILROOM_SEARCH_ROOT", searchRoot)

	d := &deps{
		log:   quietDeleteUserLogger(),
		keys:  fakeKeyProvider{key: deleteUserTestMaster},
		store: h.store,
		close: func() {},
	}
	blobPrefix, indexDir := seedBlobAndIndex(t, ctx, d, userID, blobRoot, searchRoot)

	// users_owner_delete is present (this test does not drop it) — the
	// database's normal, deployed state.
	if err := deleteUserCore(ctx, d, userID, true, userID.String(), "test-actor", false); err != nil {
		t.Fatalf("deleteUserCore(apply=true) with the delete policy present: %v", err)
	}

	if _, err := os.Stat(blobPrefix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob prefix %s: stat err = %v, want ErrNotExist — a genuine apply must remove it", blobPrefix, err)
	}
	if _, err := os.Stat(indexDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("search index dir %s: stat err = %v, want ErrNotExist — a genuine apply must remove it", indexDir, err)
	}
}
