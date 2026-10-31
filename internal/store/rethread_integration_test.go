// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the #210 re-thread repair: messages that share an
// RFC 5322 Message-ID but were ingested into different threads, before
// FindThreadByHints learned to match on the Message-ID itself.
//
// The ingest fix is forward-only, so this is the half that changes an EXISTING
// mailbox. It moves messages between threads and deletes threads, which makes
// it the kind of code where "it ran and changed nothing" and "it ran and
// changed too much" are both plausible — so every test here asserts a specific
// per-category count rather than a single total.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"
)

type rethreadFixture struct {
	h      *harness
	st     *Store
	userID ulid.ULID
}

func newRethreadFixture(t *testing.T) *rethreadFixture {
	t.Helper()
	h := newHarness(t)
	f := &rethreadFixture{h: h, st: h.appStore(), userID: NewULID()}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return f
}

// seedThread creates a thread and returns its id.
func (f *rethreadFixture) seedThread(t *testing.T, subject string) ulid.ULID {
	t.Helper()
	id := NewULID()
	if _, err := f.h.super.Exec(context.Background(),
		`INSERT INTO app.threads (id, user_id, subject, message_count) VALUES ($1, $2, $3, 0)`,
		id.String(), f.userID.String(), subject); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	return id
}

// seedMessage inserts a message into threadID carrying messageIDHint.
// receivedOffset orders messages within the test ('now() + N seconds').
func (f *rethreadFixture) seedMessage(t *testing.T, threadID ulid.ULID, subject, hint string, receivedOffset int) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), hash[:], NewULID().String())
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject,
	        received_at, size_bytes, message_id_hint)
	      VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6), 10, $7)`,
		id.String(), f.userID.String(), hash[:], threadID.String(), subject, receivedOffset, hint)
	exec(`UPDATE app.threads SET message_count = message_count + 1,
	        last_received_at = GREATEST(last_received_at, now() + make_interval(secs => $3))
	      WHERE user_id = $1 AND id = $2`,
		f.userID.String(), threadID.String(), receivedOffset)
	return id
}

func (f *rethreadFixture) scalar(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("scalar: %v", err)
	}
	return n
}

// TestRethreadMergesSplitMessageIDGroup is the whole point: two threads holding
// messages that share a Message-ID become one, and the survivors' rollups tell
// the truth afterwards.
func TestRethreadMergesSplitMessageIDGroup(t *testing.T) {
	f := newRethreadFixture(t)
	ctx := context.Background()

	// The split: one Message-ID, two threads. This is the shape a real
	// mailbox held — a message re-delivered through a second catchall
	// alias, differing only in headers, so no References tie them together.
	early := f.seedThread(t, "Newsletter 12")
	late := f.seedThread(t, "Newsletter 12")
	mEarly := f.seedMessage(t, early, "Newsletter 12", "<dup@example.com>", 0)
	mLate := f.seedMessage(t, late, "Newsletter 12 (alias)", "<dup@example.com>", 10)

	// An unrelated conversation that must be left completely alone. Without it
	// a merge that swept every thread into one would still pass.
	other := f.seedThread(t, "Unrelated")
	mOther := f.seedMessage(t, other, "Unrelated", "<other@example.com>", 5)

	var groups []SplitThreadGroup
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		groups, e = tx.SplitMessageIDGroups(ctx)
		return e
	}); err != nil {
		t.Fatalf("SplitMessageIDGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d split group(s), want exactly 1 (the unrelated thread must not be reported)", len(groups))
	}
	// The keeper must be the EARLIEST thread — the same one FindThreadByHints
	// picks with `ORDER BY received_at LIMIT 1`, so a repaired mailbox and a
	// freshly ingested one agree about where a duplicate lands.
	if groups[0].Keeper != early {
		t.Fatalf("keeper = %s, want the earliest thread %s", groups[0].Keeper, early)
	}

	var res MergeResult
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		res, e = tx.MergeSplitThreadGroups(ctx, groups)
		return e
	}); err != nil {
		t.Fatalf("MergeSplitThreadGroups: %v", err)
	}
	if res.Groups != 1 || res.MessagesMoved != 1 || res.ThreadsDeleted != 1 {
		t.Fatalf("MergeResult = %+v, want 1 group / 1 message moved / 1 thread deleted", res)
	}

	// Both messages now sit in the keeper.
	for _, id := range []ulid.ULID{mEarly, mLate} {
		if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND thread_id=$2`,
			id.String(), early.String()); got != 1 {
			t.Errorf("message %s is not in the keeper thread", id)
		}
	}
	// The emptied thread is gone, not left behind as an empty conversation.
	if got := f.scalar(t, `SELECT count(*) FROM app.threads WHERE id=$1`, late.String()); got != 0 {
		t.Errorf("the emptied loser thread still exists")
	}
	// Rollups are RECOMPUTED, not incremented — an increment would be wrong on
	// a re-run and this is exactly where a stale count would hide.
	if got := f.scalar(t, `SELECT message_count FROM app.threads WHERE id=$1`, early.String()); got != 2 {
		t.Errorf("keeper message_count = %d, want 2", got)
	}

	// ⚠ The unrelated conversation is untouched. This is the control.
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND thread_id=$2`,
		mOther.String(), other.String()); got != 1 {
		t.Errorf("the unrelated message was re-threaded")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.threads WHERE id=$1`, other.String()); got != 1 {
		t.Errorf("the unrelated thread was deleted")
	}
}

// TestRethreadAppendsTheChangeRows pins the load-bearing invariant: every
// mutation of a JMAP-visible object appends a change_log row in the same
// transaction. Three kinds are due here, and a client that never hears about
// the move keeps showing two conversations forever.
func TestRethreadAppendsTheChangeRows(t *testing.T) {
	f := newRethreadFixture(t)
	ctx := context.Background()

	early := f.seedThread(t, "Topic")
	late := f.seedThread(t, "Topic")
	f.seedMessage(t, early, "Topic", "<dup@example.com>", 0)
	f.seedMessage(t, late, "Topic again", "<dup@example.com>", 10)

	before := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE user_id=$1`, f.userID.String())

	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		groups, e := tx.SplitMessageIDGroups(ctx)
		if e != nil {
			return e
		}
		_, e = tx.MergeSplitThreadGroups(ctx, groups)
		return e
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Counted per (object_type, op) rather than as one total: a single number
	// cannot tell "the Thread rows are missing" from "an extra Email row".
	emailUpdated := f.scalar(t,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Email' AND op='updated'`,
		f.userID.String())
	threadUpdated := f.scalar(t,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Thread' AND op='updated'`,
		f.userID.String())
	threadDestroyed := f.scalar(t,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Thread' AND op='destroyed'`,
		f.userID.String())

	if emailUpdated != 1 {
		t.Errorf("Email updated rows = %d, want 1 (the moved message)", emailUpdated)
	}
	if threadUpdated != 1 {
		t.Errorf("Thread updated rows = %d, want 1 (the keeper)", threadUpdated)
	}
	if threadDestroyed != 1 {
		t.Errorf("Thread destroyed rows = %d, want 1 (the emptied thread)", threadDestroyed)
	}
	if after := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE user_id=$1`, f.userID.String()); after == before {
		t.Errorf("no change_log rows were appended at all")
	}
}

// TestRethreadIsIdempotent matters because this is an operator-run repair that
// may be run twice — and because the rollup recomputation is the part that
// would silently drift if it were an increment.
func TestRethreadIsIdempotent(t *testing.T) {
	f := newRethreadFixture(t)
	ctx := context.Background()

	early := f.seedThread(t, "Topic")
	late := f.seedThread(t, "Topic")
	f.seedMessage(t, early, "Topic", "<dup@example.com>", 0)
	f.seedMessage(t, late, "Topic again", "<dup@example.com>", 10)

	run := func() MergeResult {
		t.Helper()
		var res MergeResult
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			groups, e := tx.SplitMessageIDGroups(ctx)
			if e != nil {
				return e
			}
			res, e = tx.MergeSplitThreadGroups(ctx, groups)
			return e
		}); err != nil {
			t.Fatalf("merge: %v", err)
		}
		return res
	}

	if got := run(); got.MessagesMoved != 1 {
		t.Fatalf("first run moved %d message(s), want 1", got.MessagesMoved)
	}
	// The second run must find NOTHING — the group no longer spans two threads.
	if got := run(); got.Groups != 0 || got.MessagesMoved != 0 || got.ThreadsDeleted != 0 {
		t.Fatalf("second run = %+v, want all zero (the repair must be idempotent)", got)
	}
	if got := f.scalar(t, `SELECT message_count FROM app.threads WHERE id=$1`, early.String()); got != 2 {
		t.Errorf("keeper message_count = %d after two runs, want 2 (a rollup drifted)", got)
	}
}

// TestRethreadIgnoresMessagesWithoutAMessageID guards the one way this could
// merge unrelated mail: grouping on an empty hint. Messages with no Message-ID
// share no identity, and treating "" as one would sweep them all together.
func TestRethreadIgnoresMessagesWithoutAMessageID(t *testing.T) {
	f := newRethreadFixture(t)
	ctx := context.Background()

	a := f.seedThread(t, "A")
	b := f.seedThread(t, "B")
	f.seedMessage(t, a, "A", "", 0)
	f.seedMessage(t, b, "B", "", 10)

	var groups []SplitThreadGroup
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		groups, e = tx.SplitMessageIDGroups(ctx)
		return e
	}); err != nil {
		t.Fatalf("SplitMessageIDGroups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("got %d group(s), want 0 — messages with no Message-ID must never be merged", len(groups))
	}
}
