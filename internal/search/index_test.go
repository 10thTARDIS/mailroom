// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// The indexer's contract, tested with a fake Source: no database, no engine, no
// CGO. What is being pinned here is the crash-safety argument itself — at-least-
// once delivery, idempotent replay, a cursor that never runs ahead of the index,
// and a reindex that can always rebuild the cache from the store.

// fakeStore is an in-memory stand-in for Postgres: a message table plus an
// ordered change log, driven exactly like the real one.
//
// EVERYTHING in it is keyed by user id, and every read filters on it. That is not
// decoration: it models the one property the real Source gets from RLS
// (store.UserTx scopes every statement to the transaction's user), and it is what
// makes TestSearchIsPerUser a test of the indexer rather than a test of the fake.
type fakeStore struct {
	mu       sync.Mutex
	messages map[ulid.ULID]map[ulid.ULID]Message // user -> id -> message
	log      map[ulid.ULID][]logRow              // user -> ordered log, seq = index+1
	raw      map[string][]byte

	failChanges error // injectable failure
}

type logRow struct {
	id ulid.ULID
	op string // created | updated | destroyed
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		messages: map[ulid.ULID]map[ulid.ULID]Message{},
		log:      map[ulid.ULID][]logRow{},
		raw:      map[string][]byte{},
	}
}

// put inserts or updates a message and appends the change row the store layer
// would have written in the same transaction (the load-bearing invariant).
func (f *fakeStore) put(user ulid.ULID, m Message, op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.messages[user] == nil {
		f.messages[user] = map[ulid.ULID]Message{}
	}
	if op == "destroyed" {
		delete(f.messages[user], m.ID)
	} else {
		f.messages[user][m.ID] = m
	}
	f.log[user] = append(f.log[user], logRow{id: m.ID, op: op})
}

func (f *fakeStore) EmailState(_ context.Context, user ulid.ULID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.log[user])), nil
}

// EmailChanges mirrors store.UserTx.Changes: scan seq > since for THIS user,
// collapse per object, bucket by (firstOp, lastOp), honor the max at an object
// boundary.
func (f *fakeStore) EmailChanges(_ context.Context, user ulid.ULID, since int64, max int) (Changes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failChanges != nil {
		return Changes{}, f.failChanges
	}
	log := f.log[user]
	if since > int64(len(log)) {
		return Changes{}, ErrCursorTooOld
	}

	type collapsed struct{ first, last string }
	seen := map[ulid.ULID]*collapsed{}
	var order []ulid.ULID
	capped := false
	newState := since

	for i := since; i < int64(len(log)); i++ {
		row := log[i]
		c, ok := seen[row.id]
		if !ok {
			if max > 0 && len(order) >= max {
				capped = true
				break
			}
			c = &collapsed{first: row.op, last: row.op}
			seen[row.id] = c
			order = append(order, row.id)
		} else {
			c.last = row.op
		}
		newState = i + 1
	}
	if !capped {
		newState = int64(len(log))
	}

	out := Changes{NewState: newState, HasMore: capped}
	for _, id := range order {
		c := seen[id]
		switch {
		case c.first == "created" && c.last == "destroyed":
			// never existed as far as this consumer is concerned
		case c.last == "destroyed":
			out.Delete = append(out.Delete, id)
		default:
			out.Upsert = append(out.Upsert, id)
		}
	}
	return out, nil
}

func (f *fakeStore) Messages(_ context.Context, user ulid.ULID, ids []ulid.ULID) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		if m, ok := f.messages[user][id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) MessageIDsAfter(_ context.Context, user ulid.ULID, after ulid.ULID, limit int) ([]ulid.ULID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []ulid.ULID
	for id := range f.messages[user] {
		if id.Compare(after) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// Raw makes fakeStore a Content too.
func (f *fakeStore) Raw(_ context.Context, _ ulid.ULID, hash []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.raw[string(hash)]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

var _ Source = (*fakeStore)(nil)
var _ Content = (*fakeStore)(nil)

func testUser(t *testing.T) ulid.ULID {
	t.Helper()
	return ulid.MustNew(1, strings.NewReader("USER0123456789abcdefghijk"))
}

func msg(t *testing.T, ms int64, subject, body string) Message {
	t.Helper()
	return Message{
		ID:         mkID(t, ms),
		Subject:    subject,
		Snippet:    body,
		ReceivedAt: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
	}
}

func newTestIndexer(t *testing.T, src Source, content Content) (*Indexer, string) {
	t.Helper()
	root := t.TempDir()
	ix, err := NewIndexer(root, MemoryFactory, src, content, WithBatch(2))
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix, root
}

// The cursor advances only after the documents land, and a second Sync with
// nothing new is a no-op.
func TestSyncConsumesTheChangeLog(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice 42", "payment due"), "created")
	fs.put(user, msg(t, 2, "Lunch", "tomorrow"), "created")

	ix, _ := newTestIndexer(t, fs, nil)

	prog, err := ix.Sync(ctx, user)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if prog.Indexed != 2 {
		t.Fatalf("indexed %d, want 2", prog.Indexed)
	}
	if prog.Cursor != 2 {
		t.Fatalf("cursor %d, want 2", prog.Cursor)
	}

	res, err := ix.Search(ctx, user, Parse("invoice", ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(res.Hits))
	}

	// Nothing new: a second pass indexes nothing and does not move the cursor.
	prog2, err := ix.Sync(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if prog2.Indexed != 0 || prog2.Cursor != 2 {
		t.Fatalf("idle sync did work: %+v", prog2)
	}
}

// The batch size must not be a correctness boundary: HasMore drives the loop
// until the index is at the head.
func TestSyncPaginatesToHead(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	for i := int64(1); i <= 7; i++ {
		fs.put(user, msg(t, i, "Message", "body"), "created")
	}

	ix, _ := newTestIndexer(t, fs, nil) // batch = 2
	prog, err := ix.Sync(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if prog.Indexed != 7 || prog.Cursor != 7 {
		t.Fatalf("got %+v, want 7 indexed at cursor 7", prog)
	}
}

// At-least-once delivery: replaying the same changes must be a no-op, because
// the crash-recovery path does exactly this.
func TestReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice 42", "payment due"), "created")

	ix, root := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}

	// Simulate the crash window: the documents landed, the cursor write did not.
	// Rewinding the cursor is exactly the state a crash leaves behind.
	dir := filepath.Join(root, user.String(), "live")
	if err := writeMeta(dir, meta{SchemaVersion: schemaVersion, Cursor: 0}); err != nil {
		t.Fatal(err)
	}
	ix2, err := NewIndexer(root, MemoryFactory, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix2.Close() }()

	if _, err := ix2.Sync(ctx, user); err != nil {
		t.Fatalf("replay: %v", err)
	}
	res, err := ix2.Search(ctx, user, Parse("invoice", ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("replay duplicated the document: %d hits", len(res.Hits))
	}
}

// A destroy in the stream removes the document.
func TestSyncHandlesDestroy(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	m := msg(t, 1, "Invoice 42", "payment due")
	fs.put(user, m, "created")

	ix, _ := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}

	fs.put(user, m, "destroyed")
	prog, err := ix.Sync(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if prog.Deleted != 1 {
		t.Fatalf("deleted %d, want 1", prog.Deleted)
	}
	res, _ := ix.Search(ctx, user, Parse("invoice", ParseOptions{}))
	if len(res.Hits) != 0 {
		t.Fatal("a destroyed message is still searchable")
	}
}

// A cursor below the compaction floor cannot be caught up with deltas — the
// indexer must fall back to a full rebuild rather than skip the gap.
func TestCursorTooOldTriggersReindex(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice 42", "payment due"), "created")

	ix, root := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}

	// Forge a cursor past the head: the fake store answers ErrCursorTooOld, the
	// same signal the real store gives below the floor.
	dir := filepath.Join(root, user.String(), "live")
	if err := writeMeta(dir, meta{SchemaVersion: schemaVersion, Cursor: 9999}); err != nil {
		t.Fatal(err)
	}
	ix2, err := NewIndexer(root, MemoryFactory, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix2.Close() }()

	prog, err := ix2.Sync(ctx, user)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !prog.Rebuilt {
		t.Fatal("a too-old cursor must trigger a full reindex")
	}
	res, _ := ix2.Search(ctx, user, Parse("invoice", ParseOptions{}))
	if len(res.Hits) != 1 {
		t.Fatalf("the rebuilt index does not serve the message: %d hits", len(res.Hits))
	}
}

// A schema-version bump rebuilds and swaps (spec §7).
func TestSchemaBumpRebuilds(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice 42", "payment due"), "created")

	ix, root := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()

	// An index written by an older schema.
	dir := filepath.Join(root, user.String(), "live")
	if err := writeMeta(dir, meta{SchemaVersion: schemaVersion - 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}

	ix2, err := NewIndexer(root, MemoryFactory, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix2.Close() }()

	prog, err := ix2.Sync(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if !prog.Rebuilt {
		t.Fatal("a schema bump must rebuild")
	}
	m, err := readMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != schemaVersion {
		t.Fatalf("meta not updated: %+v", m)
	}
	if m.Rebuilding {
		t.Fatal("the rebuilding flag must be cleared after a successful swap")
	}
}

// THE doctrine test (spec §7): the index is a disposable cache. Delete it
// entirely and a reindex must restore it from the store, with nothing lost.
func TestIndexIsADisposableCache(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice 42", "payment due"), "created")
	fs.put(user, msg(t, 2, "Lunch tomorrow", "usual place"), "created")

	ix, root := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()

	// Nuke the whole index root — the "someone deleted the volume" scenario.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	ix2, err := NewIndexer(root, MemoryFactory, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix2.Close() }()

	// Sync alone must heal it: no metadata means no trustworthy cursor, which
	// means a rebuild — NOT a resume from a cursor that would skip everything.
	prog, err := ix2.Sync(ctx, user)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !prog.Rebuilt {
		t.Fatal("a discarded index must rebuild, not resume")
	}
	for _, q := range []string{"invoice", "lunch"} {
		res, _ := ix2.Search(ctx, user, Parse(q, ParseOptions{}))
		if len(res.Hits) != 1 {
			t.Fatalf("after rebuild, %q returns %d hits", q, len(res.Hits))
		}
	}
}

// A reindex must not lose changes committed WHILE it runs: the head is read
// before the walk, so the catch-up replays anything newer.
func TestReindexCatchesConcurrentChanges(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice 42", "payment due"), "created")

	ix, _ := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}

	// A message arrives; then a reindex runs. The rebuild's scan sees it (it is
	// already in the store) AND the catch-up replays its change row. Both paths
	// upsert the same id, and idempotency makes that a no-op — which is the whole
	// point of the ordering.
	fs.put(user, msg(t, 2, "Wire transfer", "urgent"), "created")

	prog, err := ix.Reindex(ctx, user)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if !prog.Rebuilt {
		t.Fatal("Reindex must report a rebuild")
	}
	res, _ := ix.Search(ctx, user, Parse("wire", ParseOptions{}))
	if len(res.Hits) != 1 {
		t.Fatalf("the concurrent message is missing after reindex: %d hits", len(res.Hits))
	}
	st, err := ix.Stats(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lag != 0 {
		t.Fatalf("after a reindex + catch-up the index must be at the head, lag=%d", st.Lag)
	}
	if st.Documents != 2 {
		t.Fatalf("documents=%d, want 2 (a rebuild must not duplicate)", st.Documents)
	}
}

// Stats surfaces the staleness spec §7 asks us to report.
func TestStatsReportsLag(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice", "x"), "created")

	ix, _ := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}
	fs.put(user, msg(t, 2, "Later", "y"), "created")
	fs.put(user, msg(t, 3, "Later still", "z"), "created")

	st, err := ix.Stats(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lag != 2 {
		t.Fatalf("lag=%d, want 2", st.Lag)
	}
}

// A body the indexer cannot read must degrade the ENTRY, never wedge the cursor:
// the headers from Postgres are still indexed and the pass still completes.
func TestUnreadableBodyDegradesGracefully(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	m := msg(t, 1, "Invoice 42", "")
	m.ContentHash = []byte("this-hash-has-no-blob-behind-it!") // 32 bytes, absent
	fs.put(user, m, "created")

	ix, _ := newTestIndexer(t, fs, fs) // fs.Raw will fail
	prog, err := ix.Sync(ctx, user)
	if err != nil {
		t.Fatalf("a missing blob must not fail the pass: %v", err)
	}
	if prog.Indexed != 1 || prog.Cursor != 1 {
		t.Fatalf("got %+v, want the message indexed and the cursor advanced", prog)
	}
	res, _ := ix.Search(ctx, user, Parse("subject:invoice", ParseOptions{}))
	if len(res.Hits) != 1 {
		t.Fatal("the headers from Postgres should still be searchable")
	}
}

// The body and recipients come from the raw message, which lives only in the
// blob. Prove the indexer actually reads it.
func TestBodyAndRecipientsComeFromTheBlob(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()

	raw := "From: Acme Billing <billing@example.com>\r\n" +
		"To: You <you@example.com>, Carol <carol@example.com>\r\n" +
		"Cc: Dave <dave@example.com>\r\n" +
		"Subject: Invoice 42\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"The zorbex payment is overdue.\r\n"
	hash := "0123456789abcdef0123456789abcdef"
	fs.raw[hash] = []byte(raw)

	m := msg(t, 1, "Invoice 42", "")
	m.ContentHash = []byte(hash)
	m.FromAddress = "billing@example.com"
	fs.put(user, m, "created")

	ix, _ := newTestIndexer(t, fs, fs)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"body:zorbex", "to:carol@example.com", "to:dave@example.com"} {
		res, err := ix.Search(ctx, user, Parse(q, ParseOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) != 1 {
			t.Errorf("%q: %d hits, want 1 (the blob was not parsed)", q, len(res.Hits))
		}
	}
}

// --- Isolation ---------------------------------------------------------------

// The NEGATIVE test: one user's query can never reach another user's mail. The
// indexes are separate directories and the caller's id picks the index — there
// is no query that names another one.
func TestSearchIsPerUser(t *testing.T) {
	ctx := context.Background()
	alice := ulid.MustNew(1, strings.NewReader("ALICE0123456789abcdefghij"))
	mallory := ulid.MustNew(2, strings.NewReader("MALLORY0123456789abcdefg"))

	fs := newFakeStore()
	secret := msg(t, 1, "Alice's secret merger plans", "project zorbex")
	fs.put(alice, secret, "created")

	ix, root := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, alice); err != nil {
		t.Fatal(err)
	}

	// Mallory's index is empty (her change stream, in the real store, is RLS-
	// scoped to her). Every query she can express returns nothing.
	if _, err := ix.Sync(ctx, mallory); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"zorbex", "merger", "subject:secret", "*",
		"id:" + secret.ID.String(), secret.ID.String(),
	} {
		res, err := ix.Search(ctx, mallory, Parse(q, ParseOptions{}))
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(res.Hits) != 0 {
			t.Fatalf("ISOLATION BREACH: mallory's %q returned %d of alice's messages", q, len(res.Hits))
		}
	}
	// Even asking for everything.
	res, err := ix.Search(ctx, mallory, Query{Root: All{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("ISOLATION BREACH: a match-everything query returned %d of alice's messages", len(res.Hits))
	}

	// And alice still finds her own.
	own, err := ix.Search(ctx, alice, Parse("zorbex", ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Hits) != 1 {
		t.Fatal("alice cannot find her own message")
	}

	// The directories are distinct.
	if filepath.Join(root, alice.String()) == filepath.Join(root, mallory.String()) {
		t.Fatal("two users share an index directory")
	}
}

// Fail closed, exactly as store.UserTx does: no identity, no index.
func TestZeroUserIDIsRejected(t *testing.T) {
	ctx := context.Background()
	ix, _ := newTestIndexer(t, newFakeStore(), nil)

	if _, err := ix.Search(ctx, ulid.ULID{}, Query{Root: All{}}); err == nil {
		t.Fatal("a zero user id must be rejected, not searched")
	}
	if _, err := ix.Sync(ctx, ulid.ULID{}); err == nil {
		t.Fatal("a zero user id must be rejected, not synced")
	}
	if err := ix.Run(ctx, ulid.ULID{}); err == nil {
		t.Fatal("a zero user id must be rejected, not run")
	}
}

// Per-user index directories are 0700 (spec §7: a security control, not
// organization). A user's index must never be readable by another user's code
// path, and the index holds plaintext.
func TestIndexDirectoriesAre0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not meaningful on Windows")
	}
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	fs.put(user, msg(t, 1, "Invoice", "x"), "created")

	ix, root := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{
		root,
		filepath.Join(root, user.String()),
		filepath.Join(root, user.String(), "live"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if mode := info.Mode().Perm(); mode != dirMode {
			t.Errorf("%s has mode %04o, want %04o", dir, mode, dirMode)
		}
	}
}

// A pre-existing, world-readable index root must be TIGHTENED, not accepted: an
// operator who created the volume with a loose umask should not silently ship a
// world-readable copy of their mail.
func TestExistingLooseRootIsTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not meaningful on Windows")
	}
	root := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ix, err := NewIndexer(root, MemoryFactory, newFakeStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()

	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != dirMode {
		t.Fatalf("index root left at %04o, want %04o", mode, dirMode)
	}
}

// The cursor lives with the index, in one atomically written file.
func TestMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := meta{SchemaVersion: 3, Cursor: 42, UpdatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := writeMeta(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := readMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != want.SchemaVersion || got.Cursor != want.Cursor {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// No temp file left behind.
	if _, err := os.Stat(filepath.Join(dir, metaFile+".tmp")); !os.IsNotExist(err) {
		t.Error("the temp file was not cleaned up")
	}
}

// Corrupt metadata must force a rebuild, not a resume from a garbage cursor.
func TestCorruptMetaForcesRebuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, metaFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readMeta(dir)
	if err != nil {
		t.Fatalf("corrupt metadata must not be a hard error: %v", err)
	}
	if m.SchemaVersion != 0 || m.Cursor != 0 {
		t.Fatalf("corrupt metadata must read back as 'rebuild me', got %+v", m)
	}
}

// A meta.json that claims a completed state must actually be JSON we wrote.
func TestMetaIsValidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := writeMeta(dir, meta{SchemaVersion: 1, Cursor: 7}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("meta.json is not valid JSON: %v", err)
	}
	if raw["cursor"] != float64(7) {
		t.Fatalf("cursor not persisted: %v", raw)
	}
}
