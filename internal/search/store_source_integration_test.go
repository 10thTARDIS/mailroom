// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package search

// End-to-end proof that the REAL StoreSource (internal/search/source.go),
// backed by a real Postgres, reacts to change_log compaction the same way
// index_test.go's fakeStore already proves the pure indexer logic does
// (mailroom#526 test 6): a cursor that falls below the compaction floor
// triggers a full reindex, and the rebuilt index holds every current
// document. Uses the CGO-free MemoryFactory backend, exactly like every other
// test in this package — nothing here needs tantivy.

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// sourceHarness mirrors internal/store's and internal/api's own harnesses:
// a superuser pool for fixture seeding (bypasses RLS), and a real
// *store.Store on the mailroom_app pool for the StoreSource under test.
// Skips when the throwaway integration database is not configured.
type sourceHarness struct {
	super *pgxpool.Pool
	store *store.Store
}

func newSourceHarness(t *testing.T) *sourceHarness {
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
	return &sourceHarness{super: super, store: store.NewFromPool(app)}
}

// seedIndexUser creates a minimal user (no account/mailbox — IndexMessages
// does not require one; attachIndexProvenance simply finds none) with the
// change machinery at seq 0/floor 0, then inserts the given subjects as
// blob-backed messages with their own change_log 'created' rows starting at
// seq 1.
func seedIndexUser(t *testing.T, super *pgxpool.Pool, tag string, subjects []string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", tag, err)
		}
	}

	userID := store.NewULID()
	exec(`INSERT INTO app.users (id, oidc_subject, email, display_name)
	      VALUES ($1,$2,$3,$4)`, userID.String(), "oidc-"+tag, tag+"@example.com", "User "+tag)
	exec(`INSERT INTO app.change_seq (user_id, object_type, last_seq) VALUES ($1, 'Email', 0)`,
		userID.String())
	exec(`INSERT INTO app.change_floor (user_id, object_type, floor_seq) VALUES ($1, 'Email', 0)`,
		userID.String())

	insertIndexMessages(t, super, userID, tag, 1, subjects)
	return userID
}

// insertIndexMessages inserts len(subjects) new blob-backed messages for
// userID, each with a change_log 'created' row continuing the Email sequence
// from startSeq, and advances change_seq.last_seq to match — the same shape
// AppendChange produces, done via raw SQL because a test fixture in a
// different package cannot reach UserTx's unexported *pgx.Tx.
func insertIndexMessages(t *testing.T, super *pgxpool.Pool, userID ulid.ULID, tag string, startSeq int64, subjects []string) []ulid.ULID {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", tag, err)
		}
	}

	var ids []ulid.ULID
	seq := startSeq
	for _, subj := range subjects {
		msgID := store.NewULID()
		ids = append(ids, msgID)
		hash := sha256.Sum256([]byte(tag + subj + msgID.String()))
		exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		      VALUES ($1,$2,100,132,$3)`, userID.String(), hash[:], store.NewULID().String())
		exec(`INSERT INTO app.messages (id, user_id, content_hash, subject, snippet,
		        from_name, from_address, received_at, size_bytes, message_id_hint)
		      VALUES ($1,$2,$3,$4,'a preview','Friend','friend@example.com', now(), 100, $5)`,
			msgID.String(), userID.String(), hash[:], subj, "<"+msgID.String()+"@example.com>")
		exec(`INSERT INTO app.change_log (user_id, object_type, seq, object_id, op)
		      VALUES ($1,'Email',$2,$3,'created')`, userID.String(), seq, msgID.String())
		seq++
	}
	exec(`UPDATE app.change_seq SET last_seq = $2 WHERE user_id = $1 AND object_type = 'Email'`,
		userID.String(), seq-1)
	return ids
}

// TestStoreSourceEndToEndReindexAfterCompaction is mailroom#526 test 6: index
// a user's mail through the real StoreSource, record the cursor, append more
// mail, compact past the recorded cursor, then Sync — it must fall back to a
// full Reindex (the search.ErrCursorTooOld path StoreSource.EmailChanges maps
// store.ErrCannotCalculateChanges to), and the rebuilt index must hold every
// CURRENT document, not just the ones indexed before compaction.
func TestStoreSourceEndToEndReindexAfterCompaction(t *testing.T) {
	h := newSourceHarness(t)
	tag := store.NewULID().String()

	userID := seedIndexUser(t, h.super, tag, []string{
		"first message of the compaction batch",
		"second message of the compaction batch",
	})

	src := NewStoreSource(h.store)
	root := t.TempDir()
	ix, err := NewIndexer(root, MemoryFactory, src, nil)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	defer func() { _ = ix.Close() }()

	ctx := context.Background()
	prog, err := ix.Sync(ctx, userID)
	if err != nil {
		t.Fatalf("initial Sync: %v", err)
	}
	if prog.Indexed != 2 {
		t.Fatalf("initial Sync indexed %d, want 2", prog.Indexed)
	}
	cursorBefore := prog.Cursor

	// More mail arrives after the cursor was recorded.
	insertIndexMessages(t, h.super, userID, tag, cursorBefore+1, []string{
		"third message of the compaction batch",
		"fourth message of the compaction batch",
	})

	// Compact past the recorded cursor: with a cutoff in the future, every
	// row that exists right now (seq 1..4) is eligible, so the floor lands at
	// 4 — strictly above cursorBefore (2).
	cutoff := time.Now().UTC().Add(time.Hour)
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		results, cerr := tx.CompactChangeLog(ctx, cutoff, 0, true)
		if cerr != nil {
			return cerr
		}
		for _, r := range results {
			if r.ObjectType == store.ObjectEmail && r.NewFloor <= cursorBefore {
				t.Fatalf("Email floor after compaction = %d, want > cursor %d", r.NewFloor, cursorBefore)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("compact: %v", err)
	}

	prog, err = ix.Sync(ctx, userID)
	if err != nil {
		t.Fatalf("Sync after compaction: %v", err)
	}
	if !prog.Rebuilt {
		t.Fatal("a cursor below the compaction floor must trigger a full reindex, not an error and not a silent partial sync")
	}

	res, err := ix.Search(ctx, userID, Parse("batch", ParseOptions{}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) != 4 {
		t.Fatalf("hits = %d, want 4 (every current document, via the rebuild — including the two "+
			"inserted AFTER the cursor compaction invalidated)", len(res.Hits))
	}
}
