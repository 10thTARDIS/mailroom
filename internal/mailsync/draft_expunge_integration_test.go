// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package mailsync

// #686 end to end, against a real Dovecot: a retired draft revision's copy on the
// server must never become a second local draft, and must not survive.
//
// The sequence is the one reconstructed from a real database:
//
//	rev1 composed             -> APPEND queued
//	writeback pass            -> rev1 is on the server (Drafts)
//	autosave: rev2 + retire 1 -> expunge {Drafts, Message-ID of rev1} queued
//	SYNC PASS                 -> reads Drafts BEFORE the expunge has run
//	send: rev3 + retire 2     -> rev2's APPEND never ran: cancelled, nothing owed
//	writeback pass            -> the expunge of rev1 runs; rev3 is appended
//	sync passes               -> poll + observe/reap
//
// It uses the real store (RLS on), the real ingest path, the real writeback
// engine over its own IMAP session (as writeback.Runner dials one), and the
// real mailsync engine. Run via scripts/mailsync-test.sh.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/writeback"
)

const liveDrafts = "Drafts"

// mapBlobs is the writeback engine's BlobReader over bytes this test composed.
type mapBlobs map[string][]byte

func (m mapBlobs) Raw(_ context.Context, _ ulid.ULID, hash []byte) ([]byte, error) {
	raw, ok := m[hex.EncodeToString(hash)]
	if !ok {
		return nil, fmt.Errorf("no blob %x", hash)
	}
	return raw, nil
}

type draftHarness struct {
	f      *liveFixture
	blobs  mapBlobs
	writer *sync.Writer
	drafts ulid.ULID
}

func newDraftHarness(t *testing.T) *draftHarness {
	t.Helper()
	ctx := context.Background()
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond)) // always due

	// Drafts starts empty on the server, like the other test folders.
	_, uids, err := f.sess.LiveUIDs(ctx, liveDrafts)
	if err != nil {
		t.Fatalf("Drafts: %v (is it missing? scripts/mailsync-test.sh creates it)", err)
	}
	for _, uid := range uids {
		f.expungeOnServer(t, liveDrafts, uid)
	}

	// The writeback runner dials its OWN session; so does this.
	ws, err := sync.Dial(ctx, f.cfg)
	if err != nil {
		t.Fatalf("dial writeback session: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	f.syncOnce(t) // learn the folders, Drafts' SPECIAL-USE included
	h := &draftHarness{f: f, blobs: mapBlobs{}, writer: sync.NewWriter(ws), drafts: f.mailboxID(t, liveDrafts)}

	var role string
	if err := f.super.QueryRow(ctx, `SELECT COALESCE(special_use, '') FROM app.mailboxes WHERE id = $1`,
		h.drafts.String()).Scan(&role); err != nil || role != `\Drafts` {
		t.Fatalf("premise: Drafts special_use = %q (%v), want \\Drafts", role, err)
	}
	return h
}

// compose records one revision and queues its Drafts APPEND, as
// compose.Composer.record does.
func (h *draftHarness) compose(t *testing.T, tx *store.UserTx, subject, msgID string) ulid.ULID {
	t.Helper()
	raw := []byte("From: me@example.com\r\n" +
		"To: you@example.com\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + msgID + ">\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"body of " + subject + "\r\n")
	sum := sha256.Sum256(raw)
	h.blobs[hex.EncodeToString(sum[:])] = raw
	id, err := tx.CreateLocalMessage(context.Background(), store.LocalMessage{
		ContentHash: sum[:], SizePlain: int64(len(raw)), SizeStored: int64(len(raw)),
		DEKID: h.f.dekID, Subject: subject, FromAddress: "me@example.com",
		SentAt: time.Now().UTC(), SizeBytes: int64(len(raw)),
		MessageIDHint: msgID, MailboxID: h.drafts, AccountID: h.f.accountID,
		Draft: true, Seen: true,
	})
	if err != nil {
		t.Fatalf("create %s: %v", subject, err)
	}
	if err := tx.EnqueueAppendIntent(context.Background(), id, h.f.accountID, liveDrafts,
		[]string{`\Draft`, `\Seen`}, raw, sum[:], msgID); err != nil {
		t.Fatalf("append intent %s: %v", subject, err)
	}
	return id
}

func (h *draftHarness) tx(t *testing.T, fn func(tx *store.UserTx) error) {
	t.Helper()
	if err := h.f.st.UserTx(context.Background(), h.f.userID, fn); err != nil {
		t.Fatal(err)
	}
}

func (h *draftHarness) writeback(t *testing.T) writeback.Result {
	t.Helper()
	eng := writeback.NewEngine(writeback.NewStoreIntents(h.f.st, h.f.userID),
		writeback.WithBlobReader(h.blobs, h.f.userID))
	res, err := eng.ApplyPending(context.Background(), h.f.accountID, h.writer)
	if err != nil {
		t.Fatalf("writeback: %v", err)
	}
	return res
}

// onServer counts the copies of msgID in Drafts, asked on a FRESHLY DIALED
// session.
//
// ⚠ NOT f.sess, and the reason is the measurement, not tidiness: a session that
// already has Drafts selected has not been told about an EXPUNGE another session
// performed until the server sends it an untagged EXPUNGE, and RFC 3501 §7.4.1
// forbids sending one while responding to a SEARCH. So a long-lived session's
// UID SEARCH can name a message that is already gone, and this assertion would
// then report a surviving copy that does not exist.
func (h *draftHarness) onServer(t *testing.T, msgID string) int {
	t.Helper()
	ctx := context.Background()
	s, err := sync.Dial(ctx, h.f.cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = s.Close() }()
	uids, err := s.SearchMessageID(ctx, liveDrafts, msgID)
	if err != nil {
		t.Fatalf("search %s: %v", msgID, err)
	}
	if len(uids) > 0 {
		t.Logf("server holds %s at Drafts uid(s) %v", msgID, uids)
	}
	return len(uids)
}

// logDrafts records the server's Drafts UID set, from a fresh session, so a
// failure shows what was actually there rather than what was inferred.
func (h *draftHarness) logDrafts(t *testing.T, when string) {
	t.Helper()
	ctx := context.Background()
	s, err := sync.Dial(ctx, h.f.cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = s.Close() }()
	_, uids, err := s.LiveUIDs(ctx, liveDrafts)
	if err != nil {
		t.Fatalf("live uids: %v", err)
	}
	t.Logf("Drafts %s: uids %v", when, uids)
}

func (h *draftHarness) rows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.f.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (h *draftHarness) localWithMessageID(t *testing.T, msgID string) int {
	t.Helper()
	return h.rows(t, `SELECT count(*) FROM app.messages WHERE user_id = $1 AND message_id_hint = $2`,
		h.f.userID.String(), msgID)
}

func TestLiveRetiredDraftRevisionIsNeitherIngestedNorLeftOnTheServer(t *testing.T) {
	h := newDraftHarness(t)
	f := h.f
	tag := ulid.Make().String()
	m1, m2, m3 := "rev1-"+tag+"@example.com", "rev2-"+tag+"@example.com", "rev3-"+tag+"@example.com"

	// rev1 is composed and reaches the server.
	var rev1, rev2, rev3 ulid.ULID
	h.tx(t, func(tx *store.UserTx) error { rev1 = h.compose(t, tx, "rev1", m1); return nil })
	if res := h.writeback(t); res.Applied != 1 {
		t.Fatalf("premise: rev1's APPEND did not apply: %+v", res)
	}
	if h.onServer(t, m1) != 1 {
		t.Fatal("premise: rev1 is not on the server")
	}
	h.logDrafts(t, "after rev1's APPEND")

	// Autosave: rev2 created and rev1 retired in ONE transaction (Email/set).
	h.tx(t, func(tx *store.UserTx) error {
		rev2 = h.compose(t, tx, "rev2", m2)
		_, err := tx.RetireDraft(context.Background(), rev1)
		return err
	})
	if got := h.rows(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND op = 'expunge'
	                      AND state = 'pending' AND payload->>'message_id_header' = $2`, f.userID.String(), m1); got != 1 {
		t.Fatalf("premise: %d pending expunges for rev1, want 1", got)
	}
	changesBefore := h.rows(t, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, f.userID.String())

	// THE RACE: a sync pass reads Drafts while that expunge is still queued.
	raced := f.syncOnce(t)
	t.Logf("racing pass: awaiting=%d gaps=%d backfilled=%d verified=%d errors=%v",
		raced.CopiesAwaitingExpunge, raced.GapsDetected, raced.GapsBackfilled, raced.MailboxesVerified, raced.Errors)
	// The copy was SEEN and deliberately not recorded — counted, so a pass that
	// silently fetched nothing cannot pass for one that deferred correctly — and
	// the Drafts observation that met it is NOT reported as a lossy fetch (#93's
	// WARN), because the cause is known and is ours.
	if raced.CopiesAwaitingExpunge == 0 {
		t.Error("the racing pass reports no copy awaiting its expunge — it never saw rev1's copy, so this run proves nothing")
	}
	if len(raced.Errors) != 0 {
		t.Errorf("the racing pass reported mailbox errors %v — a copy awaiting our own expunge is not a lossy fetch", raced.Errors)
	}
	if got := h.localWithMessageID(t, m1); got != 0 {
		t.Errorf("after the sync that raced the expunge: %d local message(s) carry rev1's Message-ID, want 0 — "+
			"the provider's copy of a RETIRED revision became a second local draft (#686)", got)
	}
	if got := h.rows(t, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, f.userID.String()); got != changesBefore {
		t.Errorf("that sync appended %d change_log row(s); nothing a client can see changed", got-changesBefore)
	}
	if got := f.inMailbox(t, h.drafts); len(got) != 1 || got[0] != rev2.String() {
		t.Errorf("Drafts holds %v, want only the live revision %s", got, rev2)
	}

	// Send: the send path creates its own revision (rev3) and the composer then
	// retires rev2, whose APPEND has NOT run. That APPEND is cancelled, so no
	// copy exists and no expunge is owed — anomaly (b) in the issue.
	h.tx(t, func(tx *store.UserTx) error { rev3 = h.compose(t, tx, "rev3", m3); return nil })
	h.tx(t, func(tx *store.UserTx) error { _, err := tx.RetireDraft(context.Background(), rev2); return err })
	if got := h.rows(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1
	                      AND payload->>'message_id_header' = $2`, f.userID.String(), m2); got != 0 {
		t.Errorf("rev2 left %d intent(s) behind; its APPEND never ran, so none is owed", got)
	}

	// The expunge of rev1 runs (and rev3's APPEND).
	h.logDrafts(t, "before the expunge pass")
	res := h.writeback(t)
	h.logDrafts(t, "after the expunge pass")
	if res.Expunged != 1 || res.DeadLettered != 0 || res.Retried != 0 {
		t.Errorf("writeback = %+v, want exactly one copy expunged and nothing retried or dead", res)
	}

	// NO SERVER COPY OF ANY SUPERSEDED REVISION SURVIVES.
	if n := h.onServer(t, m1); n != 0 {
		t.Errorf("rev1 still has %d copy(ies) on the server after its expunge applied", n)
	}
	if n := h.onServer(t, m2); n != 0 {
		t.Errorf("rev2 has %d copy(ies) on the server; its APPEND was cancelled", n)
	}
	if n := h.onServer(t, m3); n != 1 {
		t.Errorf("rev3 has %d copies on the server, want 1", n)
	}

	// Two more passes: the poll sees rev3's copy, the observation reconciles.
	f.syncOnce(t)
	if last := f.syncOnce(t); len(last.Errors) != 0 || last.CopiesAwaitingExpunge != 0 {
		t.Errorf("after the expunge ran the view should be whole again: errors=%v awaiting=%d",
			last.Errors, last.CopiesAwaitingExpunge)
	}
	t.Logf("rev1 Message-ID after convergence: %d live (not trashed) local row(s)",
		h.rows(t, `SELECT count(*) FROM app.messages WHERE user_id = $1 AND message_id_hint = $2 AND trashed_at IS NULL`,
			f.userID.String(), m1))
	if got := h.localWithMessageID(t, m1); got != 0 {
		t.Errorf("after convergence: %d local row(s) still carry rev1's Message-ID (live or soft-deleted), want 0", got)
	}
	if got := h.localWithMessageID(t, m3); got != 1 {
		t.Errorf("rev3: %d local rows, want 1", got)
	}
	// ONE ROW PER REAL CHANGE: every Email row in the log is about a revision
	// that really existed; a phantom draft would add rows for an object nobody
	// composed.
	if got := h.rows(t, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email'
	                      AND object_id NOT IN ($2, $3, $4)`,
		f.userID.String(), rev1.String(), rev2.String(), rev3.String()); got != 0 {
		t.Errorf("%d Email change_log row(s) name a message no one composed", got)
	}
	for _, c := range []struct {
		id      ulid.ULID
		op      string
		want    int
		meaning string
	}{
		{rev1, "created", 1, "rev1 composed once"},
		{rev1, "destroyed", 1, "rev1 retired once"},
		{rev2, "destroyed", 1, "rev2 retired once"},
	} {
		if got := h.rows(t, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email'
		                      AND object_id = $2 AND op = $3`, f.userID.String(), c.id.String(), c.op); got != c.want {
			t.Errorf("%s: %d %q rows, want %d", c.meaning, got, c.op, c.want)
		}
	}
}
