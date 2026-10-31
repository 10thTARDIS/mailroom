// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package purge

// Integration coverage for the purge worker against a real PostgreSQL (connecting
// as mailroom_app, RLS enforced) with a fake blob store standing in for the
// filesystem. It proves the two things the store-level tests cannot: that the
// sweep actually reclaims the blob FILE, and that one message's failure is
// isolated so the rest of the batch still purges.

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/store"
)

// --- fake blob store ---------------------------------------------------------

type fakeBlobs struct {
	mu      sync.Mutex
	deleted map[blob.ContentHash]bool
	fail    map[blob.ContentHash]bool // hashes whose Delete returns an error
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{deleted: map[blob.ContentHash]bool{}, fail: map[blob.ContentHash]bool{}}
}

func (b *fakeBlobs) Delete(_ context.Context, _ ulid.ULID, h blob.ContentHash) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail[h] {
		return errors.New("fake blob store: delete failed")
	}
	b.deleted[h] = true
	return nil
}

func (b *fakeBlobs) wasDeleted(h blob.ContentHash) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deleted[h]
}

// Unused by the purge path, present to satisfy blob.BlobStore.
func (b *fakeBlobs) Put(context.Context, ulid.ULID, []byte, io.Reader) (blob.ContentHash, error) {
	return blob.ContentHash{}, errors.New("not implemented")
}
func (b *fakeBlobs) Get(context.Context, ulid.ULID, []byte, blob.ContentHash) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (b *fakeBlobs) Exists(context.Context, ulid.ULID, blob.ContentHash) (bool, error) {
	return false, nil
}
func (b *fakeBlobs) StoredSize(context.Context, ulid.ULID, blob.ContentHash) (int64, error) {
	return 0, blob.ErrNotFound
}

var _ blob.BlobStore = (*fakeBlobs)(nil)

// --- harness -----------------------------------------------------------------

type purgeHarness struct {
	super   *pgxpool.Pool
	st      *store.Store
	userID  ulid.ULID
	account ulid.ULID
	trash   ulid.ULID
}

func newPurgeHarness(t *testing.T) *purgeHarness {
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

	h := &purgeHarness{
		super: super, st: store.NewFromPool(app),
		userID: store.NewULID(), account: store.NewULID(), trash: store.NewULID(),
	}
	exec := func(sql string, args ...any) {
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		h.userID.String(), "sub-"+h.userID.String(), h.userID.String()+"@example.com")
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		h.account.String(), h.userID.String(), store.NewULID().String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1, $2, $3, 'Trash', 'Trash', '\Trash')`,
		h.trash.String(), h.userID.String(), h.account.String())
	return h
}

// seedDueMessage inserts a trashed, past-due message (no provenance) and returns
// its id and content hash.
func (h *purgeHarness) seedDueMessage(t *testing.T, subject string) (ulid.ULID, blob.ContentHash) {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	sum := sha256.Sum256([]byte(subject + id.String()))
	threadID := store.NewULID()
	exec := func(sql string, args ...any) {
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, h.userID.String(), sum[:], store.NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), h.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes,
	                                trashed_at, purge_after)
	      VALUES ($1, $2, $3, $4, $5, now(), 10, now() - interval '1 hour', now() - interval '1 hour')`,
		id.String(), h.userID.String(), sum[:], threadID.String(), subject)
	return id, blob.ContentHash(sum)
}

func (h *purgeHarness) seedProvenance(t *testing.T, msg ulid.ULID) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, 1, 42, 'live')`,
		store.NewULID().String(), h.userID.String(), msg.String(), h.account.String(), h.trash.String()); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
}

func (h *purgeHarness) exists(t *testing.T, msg ulid.ULID) bool {
	t.Helper()
	var one int
	err := h.super.QueryRow(context.Background(),
		`SELECT 1 FROM app.messages WHERE user_id = $1 AND id = $2`,
		h.userID.String(), msg.String()).Scan(&one)
	return err == nil && one == 1
}

// The sweep hard-deletes due mail and reclaims each unshared blob FILE.
func TestSweepPurgesAndReclaims(t *testing.T) {
	h := newPurgeHarness(t)
	a, ah := h.seedDueMessage(t, "first")
	b, bh := h.seedDueMessage(t, "second")

	fb := newFakeBlobs()
	w := NewWorker(h.st, fb, time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Purged != 2 || res.BlobsReclaimed != 2 {
		t.Fatalf("result = %+v, want 2 purged / 2 reclaimed", res)
	}
	if h.exists(t, a) || h.exists(t, b) {
		t.Fatal("a purged message row survived")
	}
	if !fb.wasDeleted(ah) || !fb.wasDeleted(bh) {
		t.Fatal("a reclaimed blob file was not deleted from the blob store")
	}
}

// One message's blob-file delete failing does NOT poison the batch: it stays in
// Trash (retried next sweep), the others purge, and nothing is half-destroyed.
func TestSweepOneFailureDoesNotPoisonTheBatch(t *testing.T) {
	h := newPurgeHarness(t)
	a, ah := h.seedDueMessage(t, "ok one")
	bad, badHash := h.seedDueMessage(t, "blob delete will fail")
	c, ch := h.seedDueMessage(t, "ok two")

	fb := newFakeBlobs()
	fb.fail[badHash] = true

	w := NewWorker(h.st, fb, time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Purged != 2 || res.Errors != 1 {
		t.Fatalf("result = %+v, want 2 purged / 1 error", res)
	}
	if h.exists(t, a) || h.exists(t, c) {
		t.Fatal("a healthy message was not purged because a sibling failed")
	}
	if !h.exists(t, bad) {
		t.Fatal("the message whose blob delete failed was destroyed anyway — its row must survive to retry")
	}
	if !fb.wasDeleted(ah) || !fb.wasDeleted(ch) {
		t.Fatal("healthy blobs were not reclaimed")
	}
}

// A due message that still has a live server copy is NOT hard-deleted; its
// EXPUNGE is enqueued instead (propagate-first, canonical-last).
func TestSweepEnqueuesExpungeForServerCopies(t *testing.T) {
	h := newPurgeHarness(t)
	msg, hash := h.seedDueMessage(t, "still on the server")
	h.seedProvenance(t, msg)

	fb := newFakeBlobs()
	w := NewWorker(h.st, fb, time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.ExpungeEnqueued != 1 || res.Purged != 0 {
		t.Fatalf("result = %+v, want 1 expunge enqueued / 0 purged", res)
	}
	if !h.exists(t, msg) {
		t.Fatal("hard-deleted a message whose copy still sits on the server")
	}
	if fb.wasDeleted(hash) {
		t.Fatal("reclaimed the blob of a message that was not purged")
	}
	var intents int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND message_id = $2 AND op = 'expunge'`,
		h.userID.String(), msg.String()).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 1 {
		t.Fatalf("expunge intents = %d, want 1", intents)
	}
}

// Handle reschedules the next sweep so the timer keeps ticking.
func TestHandleReschedules(t *testing.T) {
	h := newPurgeHarness(t)
	job := store.Job{ID: store.NewULID(), UserID: &h.userID, Kind: JobKind}

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	if err := w.Handle(context.Background(), job); err != nil {
		t.Fatalf("handle: %v", err)
	}
	var pending int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = $2 AND state = 'pending'`,
		h.userID.String(), JobKind).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending purge jobs after Handle = %d, want exactly 1", pending)
	}
}
