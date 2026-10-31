// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the deletion lifecycle's terminal step — the purge
// (purge.go, ADR-009) — against a real PostgreSQL with RLS enforced, connecting
// as mailroom_app. These are the destructive-path proofs: a due message is hard-
// deleted and emits `destroyed` + a tombstone; its blob is reclaimed ONLY when
// unreferenced; a message whose copies still sit on a server is NOT hard-deleted;
// restore lands a message back in Archive. The reaper fixture (reaper_integration_
// test.go) supplies the users, account and mailboxes.

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func superExec(t *testing.T, f *reapFixture, sql string, args ...any) {
	t.Helper()
	if _, err := f.h.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// trash marks a message trashed with purge_after set to `after` (past-dated to
// make it due, future-dated to make it not-yet-due).
func trash(t *testing.T, f *reapFixture, msg ulid.ULID, after time.Time) {
	t.Helper()
	superExec(t, f, `UPDATE app.messages SET trashed_at = now(), purge_after = $3
	                  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String(), after.UTC())
}

func contentHashOf(t *testing.T, f *reapFixture, msg ulid.ULID) []byte {
	t.Helper()
	var h []byte
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&h); err != nil {
		t.Fatalf("content hash: %v", err)
	}
	return h
}

func messageExists(t *testing.T, f *reapFixture, msg ulid.ULID) bool {
	t.Helper()
	var one int
	err := f.h.super.QueryRow(context.Background(),
		`SELECT 1 FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&one)
	if err != nil {
		return false
	}
	return one == 1
}

func blobExists(t *testing.T, f *reapFixture, hash []byte) bool {
	t.Helper()
	var one int
	err := f.h.super.QueryRow(context.Background(),
		`SELECT 1 FROM app.blobs WHERE user_id = $1 AND content_hash = $2`,
		f.userID.String(), hash).Scan(&one)
	if err != nil {
		return false
	}
	return one == 1
}

// tombstones returns the audit tombstones recorded for a message: the count and,
// for the first, the recorded content_hash_prefix.
func tombstones(t *testing.T, f *reapFixture, msg ulid.ULID) (int, string) {
	t.Helper()
	var (
		n      int
		prefix *string
	)
	if err := f.h.super.QueryRow(context.Background(), `
		SELECT count(*), max(detail ->> 'content_hash_prefix')
		  FROM app.audit_log
		 WHERE user_id = $1 AND action = $2 AND object_type = 'Email' AND object_id = $3`,
		f.userID.String(), PurgeAction, msg.String()).Scan(&n, &prefix); err != nil {
		t.Fatalf("tombstones: %v", err)
	}
	if prefix == nil {
		return n, ""
	}
	return n, *prefix
}

func hardDelete(t *testing.T, f *reapFixture, msg ulid.ULID, now time.Time) (hash []byte, reclaim, ok bool) {
	t.Helper()
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var err error
		hash, reclaim, ok, err = tx.HardDeleteMessage(context.Background(), msg, now)
		return err
	}); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	return hash, reclaim, ok
}

// THE HEADLINE. A message past its window with no server copy is hard-deleted:
// its row is gone, it emits exactly one Email `destroyed`, it leaves a tombstone
// carrying only a hash prefix, and its (unshared) blob is reclaimed.
func TestHardDeleteMessageDestroysReclaimsAndTombstones(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "purge me")
	hash := contentHashOf(t, f, msg)
	trash(t, f, msg, time.Now().Add(-time.Hour)) // due: purge_after in the past
	before := f.emailChanges(t, msg)

	gotHash, reclaim, ok := hardDelete(t, f, msg, time.Now())
	if !ok {
		t.Fatal("a due message with no provenance was not hard-deleted")
	}
	if !reclaim {
		t.Fatal("an unshared blob was not reclaimed")
	}
	if hex.EncodeToString(gotHash) != hex.EncodeToString(hash) {
		t.Fatalf("returned hash %x != message hash %x", gotHash, hash)
	}
	if messageExists(t, f, msg) {
		t.Fatal("the message row survived a hard delete")
	}
	if blobExists(t, f, hash) {
		t.Fatal("the blob metadata row was not reclaimed")
	}
	// Exactly one Email `destroyed` row, in the same transaction (the invariant).
	if got := f.emailChanges(t, msg) - before; got != 1 {
		t.Fatalf("Email change_log rows appended = %d, want exactly 1 (destroyed)", got)
	}
	// The tombstone: exactly one, carrying only the hash prefix.
	n, prefix := tombstones(t, f, msg)
	if n != 1 {
		t.Fatalf("audit tombstones = %d, want exactly 1", n)
	}
	wantPrefix := hex.EncodeToString(hash[:tombstonePrefixBytes])
	if prefix != wantPrefix {
		t.Fatalf("tombstone content_hash_prefix = %q, want %q", prefix, wantPrefix)
	}
}

// THE DANGEROUS STEP. A blob whose bytes are ALSO an attachment on another
// message is content-addressed dedup at work: reclaiming it would delete data out
// from under the survivor. The message is destroyed; the blob is KEPT.
func TestHardDeleteMessageKeepsBlobSharedWithAnAttachment(t *testing.T) {
	f := newReapFixture(t)
	shared := f.seedMessage(t, "a forwarded .eml lives here too")
	hash := contentHashOf(t, f, shared)

	// A DIFFERENT message carries an attachment whose extracted bytes hash to the
	// SAME content — the exact dedup the reclaim guard must respect.
	other := f.seedMessage(t, "the message that forwarded it")
	superExec(t, f, `
		INSERT INTO app.attachments (id, user_id, message_id, part_path, size_bytes, content_hash)
		VALUES ($1, $2, $3, '2', 10, $4)`,
		NewULID().String(), f.userID.String(), other.String(), hash)

	trash(t, f, shared, time.Now().Add(-time.Hour))
	_, reclaim, ok := hardDelete(t, f, shared, time.Now())
	if !ok {
		t.Fatal("the due message was not hard-deleted")
	}
	if reclaim {
		t.Fatal("RECLAIMED A BLOB STILL REFERENCED BY AN ATTACHMENT — data loss for the survivor")
	}
	if messageExists(t, f, shared) {
		t.Fatal("the message row survived")
	}
	if !blobExists(t, f, hash) {
		t.Fatal("the shared blob was deleted out from under the attachment that still needs it")
	}
}

// CANONICAL-LAST. A due message whose copies still sit on a server we sync is NOT
// hard-deleted — deleting it would drop the record and the audit trail while a
// copy survives. It refuses; the row and the change stream are untouched.
func TestHardDeleteMessageRefusesWhileProvenanceLives(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "still on the server")
	f.seedProvenance(t, msg, f.trash, 1, 42, "live")
	trash(t, f, msg, time.Now().Add(-time.Hour))
	before := f.emailChanges(t, msg)

	_, _, ok := hardDelete(t, f, msg, time.Now())
	if ok {
		t.Fatal("hard-deleted a message whose copy still sits on the server (failed EXPUNGE would strand it)")
	}
	if !messageExists(t, f, msg) {
		t.Fatal("the canonical row was destroyed while a server copy survives")
	}
	if got := f.emailChanges(t, msg) - before; got != 0 {
		t.Fatalf("appended %d change rows for a message it refused to purge", got)
	}
	if n, _ := tombstones(t, f, msg); n != 0 {
		t.Fatalf("wrote %d tombstones for a message it refused to purge", n)
	}
}

// FAIL CLOSED. A message still inside its window, or one that was restored, is not
// eligible and must not be touched.
func TestHardDeleteMessageRefusesIneligible(t *testing.T) {
	f := newReapFixture(t)

	future := f.seedMessage(t, "window still open")
	trash(t, f, future, time.Now().Add(24*time.Hour)) // purge_after in the future

	restored := f.seedMessage(t, "restored before the sweep")
	// trashed then untrashed: trashed_at NULL, so not eligible.

	for name, msg := range map[string]ulid.ULID{"not yet due": future, "not trashed": restored} {
		t.Run(name, func(t *testing.T) {
			_, _, ok := hardDelete(t, f, msg, time.Now())
			if ok {
				t.Fatal("hard-deleted an ineligible message")
			}
			if !messageExists(t, f, msg) {
				t.Fatal("destroyed an ineligible message")
			}
		})
	}
}

// The due-scan finds exactly the trashed-and-past messages and classifies each by
// whether it still has a live server copy.
func TestDueForPurgeClassifies(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()

	dueNoProv := f.seedMessage(t, "due, gone from the server")
	trash(t, f, dueNoProv, time.Now().Add(-time.Hour))

	dueWithProv := f.seedMessage(t, "due, still on the server")
	f.seedProvenance(t, dueWithProv, f.trash, 2, 42, "live")
	trash(t, f, dueWithProv, time.Now().Add(-time.Hour))

	notYet := f.seedMessage(t, "trashed but window open")
	trash(t, f, notYet, time.Now().Add(24*time.Hour))

	live := f.seedMessage(t, "not trashed at all")
	f.seedProvenance(t, live, f.inbox, 3, 42, "live")

	var due []PurgeCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		due, err = tx.DueForPurge(ctx, time.Now(), 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	got := map[ulid.ULID]bool{}
	for _, c := range due {
		got[c.ID] = c.HasLiveProvenance
	}
	if len(got) != 2 {
		t.Fatalf("DueForPurge returned %d candidates, want 2 (the due ones)", len(got))
	}
	if prov, ok := got[dueNoProv]; !ok || prov {
		t.Fatalf("dueNoProv: present=%v hasProv=%v, want present with no provenance", ok, prov)
	}
	if prov, ok := got[dueWithProv]; !ok || !prov {
		t.Fatalf("dueWithProv: present=%v hasProv=%v, want present with provenance", ok, prov)
	}
	if _, ok := got[notYet]; ok {
		t.Fatal("a message still inside its window was returned as due")
	}
	if _, ok := got[live]; ok {
		t.Fatal("a message that was never trashed was returned as due")
	}
}

// The IMAP half: a due message with a server copy gets a per-account EXPUNGE
// intent, message-id-bound, and re-enqueuing is idempotent (no expunge storm).
func TestEnqueuePurgeExpungeIntents(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "expunge me")
	f.seedProvenance(t, msg, f.trash, 4, 42, "live")

	enqueued := func() int {
		var n int
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			// Resolve through the real predicate rather than hand-picking the
			// account: ExpungeableAccountIDs is the single definition of "may I
			// destroy this copy", and a test that bypassed it would keep passing
			// if that definition broke.
			accts, _, err := tx.ExpungeableAccountIDs(ctx, msg)
			if err != nil {
				return err
			}
			n, err = tx.EnqueuePurgeExpungeIntents(ctx, msg, accts)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := enqueued(); n != 1 {
		t.Fatalf("enqueued %d expunge intents, want 1 (one account)", n)
	}
	// Idempotent: a second sweep does not create a second intent.
	_ = enqueued()

	var (
		count int
		op    string
	)
	if err := f.h.super.QueryRow(ctx, `
		SELECT count(*), max(op::text) FROM app.writeback_intents
		 WHERE user_id = $1 AND message_id = $2`,
		f.userID.String(), msg.String()).Scan(&count, &op); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("writeback intents for the message = %d, want exactly 1 (idempotent)", count)
	}
	if op != string(IntentExpunge) {
		t.Fatalf("intent op = %q, want %q", op, IntentExpunge)
	}
}

// ⚠ THE REGRESSION TEST FOR #296's SECOND DEFECT.
//
// The purge key is deterministic per (message, account) so repeat sweeps do not
// storm — but it was enqueued WITHOUT IntentRow.Revive, and EnqueueIntent's
// ON CONFLICT arm is `WHERE $8::boolean AND …`, constant false without it. So a
// row that had reached a terminal state swallowed every later re-enqueue,
// returned its old id, and the sweep counted an ExpungeEnqueued that could never
// do anything again. A message the user destroyed kept its server copies
// forever, HardDeleteMessage kept (correctly) refusing while live provenance
// remained, the Trash entry never cleared, and nothing dead-lettered.
//
// The two arms are asserted together on purpose: reviving a FINISHED row is the
// fix, and NOT disturbing an unfinished one is what stops the fix from becoming
// an expunge storm.
func TestEnqueuePurgeExpungeIntentsRearmsAFinishedIntent(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "expunge me again")
	f.seedProvenance(t, msg, f.trash, 4, 42, "live")

	sweep := func() {
		t.Helper()
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			accts, _, err := tx.ExpungeableAccountIDs(ctx, msg)
			if err != nil {
				return err
			}
			_, err = tx.EnqueuePurgeExpungeIntents(ctx, msg, accts)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	stateOf := func(id ulid.ULID) string {
		t.Helper()
		var s string
		if err := f.h.super.QueryRow(ctx,
			`SELECT state::text FROM app.writeback_intents WHERE id = $1`, id.String()).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	rowCount := func() int {
		t.Helper()
		var n int
		if err := f.h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND message_id = $2`,
			f.userID.String(), msg.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	sweep()
	var id ulid.ULID
	if err := f.h.super.QueryRow(ctx,
		`SELECT id FROM app.writeback_intents WHERE user_id = $1 AND message_id = $2`,
		f.userID.String(), msg.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// ARM 1 — a request still PENDING is left alone: a second copy of a question
	// nobody has answered yet is just noise.
	sweep()
	if got := stateOf(id); got != string(IntentPending) {
		t.Errorf("state after re-sweeping a pending intent = %q, want pending", got)
	}
	if n := rowCount(); n != 1 {
		t.Fatalf("intents = %d, want 1 — a re-sweep must not duplicate", n)
	}

	// ARM 2 — a FINISHED request is re-armed. Run the intent to 'applied' the
	// real way (claim, then mark), then sweep again.
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		if _, err := tx.ClaimPendingIntents(ctx, f.accountID, 10); err != nil {
			return err
		}
		_, err := tx.MarkIntentApplied(ctx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(id); got != string(IntentApplied) {
		t.Fatalf("state = %q, want applied before the re-sweep", got)
	}
	sweep()
	if got := stateOf(id); got != string(IntentPending) {
		t.Fatalf("state after re-sweeping an APPLIED intent = %q, want pending — "+
			"a terminal row must not silently swallow a later purge", got)
	}
	if n := rowCount(); n != 1 {
		t.Fatalf("intents = %d, want 1 — reviving re-arms the row, it does not add another", n)
	}

	// ARM 3 — a DEAD request is re-armed too. The message is still in the user's
	// Trash asking to be destroyed; refusing to try again is the failure mode,
	// not the safety.
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		if _, err := tx.ClaimPendingIntents(ctx, f.accountID, 10); err != nil {
			return err
		}
		_, err := tx.MarkIntentFailed(ctx, id, "connection reset", "gave up")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(id); got != string(IntentDead) {
		t.Fatalf("state = %q, want dead before the re-sweep", got)
	}
	sweep()
	if got := stateOf(id); got != string(IntentPending) {
		t.Fatalf("state after re-sweeping a DEAD intent = %q, want pending", got)
	}
}

// RLS. A purge run as one user cannot see or destroy another user's Trash.
func TestPurgeIsUserScoped(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	mine := f.seedMessage(t, "mine to purge")
	trash(t, f, mine, time.Now().Add(-time.Hour))

	var due []PurgeCandidate
	var ok bool
	if err := f.st.UserTx(ctx, f.otherID, func(tx *UserTx) error {
		var err error
		if due, err = tx.DueForPurge(ctx, time.Now(), 100); err != nil {
			return err
		}
		_, _, ok, err = tx.HardDeleteMessage(ctx, mine, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("another user saw %d of my due messages", len(due))
	}
	if ok {
		t.Fatal("another user hard-deleted my message")
	}
	if !messageExists(t, f, mine) {
		t.Fatal("RLS did not protect my message from another user's purge")
	}
}

// RESTORE. Clearing the Trash window puts the message back — to Archive when its
// origin folder is gone (ADR-009). It leaves Trash, lands in Archive, its
// timestamps are cleared, and it emits an Email `update` (never `destroyed`).
func TestRestoreToArchiveWhenOriginGone(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "bring me back")
	// It currently sits in the account's Trash folder; its original folder is gone.
	f.seedProvenance(t, msg, f.trash, 5, 42, "live")
	trash(t, f, msg, time.Now().Add(-time.Hour))
	before := f.emailChanges(t, msg)

	var found bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		found, _, err = tx.ApplyEmailUpdate(ctx, msg, EmailUpdate{Restore: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("restore did not find the message")
	}
	st := f.msgState(t, msg)
	if st.trashed || st.purgeAfter != nil {
		t.Fatalf("restore left trash state behind: %+v", st)
	}
	if st.moved != f.archive.String() {
		t.Fatalf("restored to %q, want Archive %q", st.moved, f.archive.String())
	}
	if got := f.emailChanges(t, msg) - before; got != 1 {
		t.Fatalf("restore appended %d change rows, want exactly 1 (an update)", got)
	}
}

// A MOVE OUT OF TRASH IS A RESTORE. The web client's Archive is the patch
// `mailboxIds/<archive>: true` and nothing more, and pressing it in the Trash
// view used to file the message in Archive while leaving it trashed: hidden
// from Archive by the trash predicate, gone from Trash because it was no longer
// there, and destroyed by the purge when its 30 days ran out. The purge
// EXPUNGE armed below is the part that destroys the server copy, so its
// withdrawal is asserted along with the timestamps.
func TestMoveOutOfTrashRestores(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "archived from the trash")
	f.seedProvenance(t, msg, f.trash, 5, 42, "live")
	trash(t, f, msg, time.Now().Add(-time.Hour))

	// Arm the purge through the real path, as the sweep would have: the message
	// is overdue and its only copy sits in Trash.
	armed := 0
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		accts, _, err := tx.ExpungeableAccountIDs(ctx, msg)
		if err != nil {
			return err
		}
		armed, err = tx.EnqueuePurgeExpungeIntents(ctx, msg, accts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if armed != 1 {
		t.Fatalf("armed %d purge EXPUNGEs, want 1 — nothing was queued, so this test proves "+
			"nothing about the purge", armed)
	}
	before := f.emailChanges(t, msg)

	archive := f.archive
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msg, EmailUpdate{MoveToRole: "archive", MoveToMailboxID: &archive})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	st := f.msgState(t, msg)
	if st.trashed || st.purgeAfter != nil {
		t.Fatalf("a move out of Trash left the message trashed (%+v): it is hidden in Archive "+
			"and still due for purge", st)
	}
	if st.moved != f.archive.String() {
		t.Fatalf("moved to %q, want Archive %q", st.moved, f.archive.String())
	}
	var expunges int
	if err := f.h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND message_id = $2 AND op = 'expunge'`,
		f.userID.String(), msg.String()).Scan(&expunges); err != nil {
		t.Fatal(err)
	}
	if expunges != 0 {
		t.Fatalf("%d purge EXPUNGE(s) still queued after the move: the next drain destroys the "+
			"server copy of a message the user just filed", expunges)
	}
	if got := f.emailChanges(t, msg) - before; got != 1 {
		t.Fatalf("the move appended %d change rows, want exactly 1 (an update)", got)
	}
}

// The control for the rule above: a patch that names Trash AND another folder
// is a trash, not a restore. Trash wins the placement, so it has to win the
// trashed state too, or the message would sit in Trash without being deleted.
func TestMoveThatAlsoNamesTrashStaysTrashed(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "both at once")
	f.seedProvenance(t, msg, f.inbox, 6, 42, "live")

	archive := f.archive
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msg,
			EmailUpdate{Trash: true, MoveToRole: "archive", MoveToMailboxID: &archive})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st := f.msgState(t, msg)
	if !st.trashed || st.purgeAfter == nil {
		t.Fatalf("a patch naming Trash did not trash the message: %+v", st)
	}
	if st.moved != f.trash.String() {
		t.Fatalf("placed in %q, want Trash %q", st.moved, f.trash.String())
	}
}

// TestHardDeleteCascadesAPendingAppendIntent is #418 item 2.
//
// ⚠ AN APPEND INTENT MUST DIE WITH ITS MESSAGE. Its payload names the bytes by
// content hash and nothing else, so an intent that outlives a hard delete can
// never complete — its blob is gone — and it dead-letters as "could not file
// the draft" long after the user asked for that mail to be destroyed. Worse, if
// it COULD still run, it would file the deleted message onto an IMAP server,
// which is the one outcome hard delete exists to prevent.
//
// The mechanism is already in the schema: writeback_intents.message_id has a
// foreign key with ON DELETE CASCADE, and HardDeleteMessage deletes the message
// row BEFORE reclaiming the blob. The append path simply never set the column.
//
// Observed before the fix, on a real instance: every append intent
// and expunge intent had a NULL message_id, while the flag/keyword/
// move intents all set it.
func TestHardDeleteCascadesAPendingAppendIntent(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "append me then delete me")
	hash := contentHashOf(t, f, msg)

	// A pending APPEND naming this message — the state a signed send leaves
	// between saving the draft and the writeback pass that files it.
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.EnqueueAppendIntent(ctx, msg, f.accountID, "Sent",
			[]string{`\Seen`}, nil, hash, "<append-cascade@example.com>")
	}); err != nil {
		t.Fatalf("EnqueueAppendIntent: %v", err)
	}
	if n := intentsFor(t, f, msg); n != 1 {
		t.Fatalf("intents naming the message = %d, want 1 — the intent was not tied to it", n)
	}

	trash(t, f, msg, time.Now().Add(-time.Hour))
	if _, _, ok := hardDelete(t, f, msg, time.Now()); !ok {
		t.Fatal("the message was not hard-deleted")
	}

	if n := intentsFor(t, f, msg); n != 0 {
		t.Fatalf("%d writeback intent(s) survived the hard delete of their own message", n)
	}
	// And nothing is left pointing at bytes that are gone.
	if n := pendingIntentsAnywhere(t, f); n != 0 {
		t.Fatalf("%d intent(s) still pending after the message they name was destroyed", n)
	}
}

// intentsFor counts writeback intents naming this message.
func intentsFor(t *testing.T, f *reapFixture, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents WHERE user_id=$1 AND message_id=$2`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	return n
}

// pendingIntentsAnywhere is the control: it counts intents for THIS user
// regardless of message, so a cascade that deleted nothing (or everything)
// is distinguishable from one that deleted the right row.
func pendingIntentsAnywhere(t *testing.T, f *reapFixture) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents WHERE user_id=$1 AND state IN ('pending','inflight')`,
		f.userID.String()).Scan(&n); err != nil {
		t.Fatalf("count pending intents: %v", err)
	}
	return n
}
