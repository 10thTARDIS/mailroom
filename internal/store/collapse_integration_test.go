// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Collapsing already-duplicated mail (#206 part 3).
//
// This deletes rows from the FK hub, so the tests are about WHAT SURVIVES, not about the
// happy path. Ten of the twelve foreign keys onto app.messages cascade, so every one of
// these assertions is a thing a naive DELETE would have destroyed.
//
// The most important test here is the one real data CANNOT exercise: a
// label that exists only on a redundant copy. In practice that count is
// 0, so a broken union would have collapsed the rows and lost nothing, passed every check
// I could run against a real mailbox, and then silently eaten labels on the next mailbox. The
// fixture below creates that case deliberately.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"
)

// dupPair seeds two messages sharing a dedup_hash, each with its own blob, thread and
// provenance — the shape #206 leaves behind. Returns (keeper, loser).
func dupPair(t *testing.T, h *harness, u fixtureUser, tag string) (ulid.ULID, ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	dedup := []byte("dedup-hash-" + tag + "-000000000000")

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", tag, err)
		}
	}

	var ids []ulid.ULID
	// The keeper must be the OLDEST, and the store picks it with ORDER BY id, so the
	// ULIDs have to be generated in order. NewULID is time-ordered, so sequential calls
	// give ascending ids.
	for i := 0; i < 2; i++ {
		id := NewULID()
		ids = append(ids, id)
		// A REAL hash, not a truncated string. Truncating `tag + id` to 32 bytes broke
		// as soon as the tag was long enough to push the ULID's varying tail past the
		// cut: two ULIDs created milliseconds apart share a long time-prefix, so the
		// two copies collided on blobs_pkey.
		sum := sha256.Sum256([]byte(tag + "|" + id.String()))
		hash := sum[:]
		exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		      VALUES ($1,$2,100,132,$3)`, u.ID, hash, NewULID().String())
		threadID := NewULID().String()
		exec(`INSERT INTO app.threads (id, user_id, subject, message_count, last_received_at)
		      VALUES ($1,$2,$3,1,now())`, threadID, u.ID, "dup "+tag)
		exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
		        from_name, from_address, received_at, size_bytes, message_id_hint, dedup_hash)
		      VALUES ($1,$2,$3,$4,$5,'p','Sender','s@example.com',now(),100,$6,$7)`,
			id.String(), u.ID, hash, threadID, "dup "+tag, "<"+tag+"@example.com>", dedup)
		// Each copy sits at its own IMAP coordinate — that is what makes them separate
		// rows in the first place, and what has to be preserved.
		exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		      VALUES ($1,$2,$3,$4,$5,$6,1)`,
			NewULID().String(), u.ID, id.String(), u.AccountID, u.MailboxID, 9000+i)
	}
	return ids[0], ids[1]
}

func TestCollapseMovesProvenanceAndDeletesTheRedundantRow(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, loser := dupPair(t, h, u, "basic")

	var res CollapseResult
	var ok bool
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		res, ok, e = tx.CollapseDuplicateGroup(ctx, keeper, []ulid.ULID{loser})
		return e
	}); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	if !ok {
		t.Fatal("collapse refused a group with nothing blocking it")
	}

	if res.MessagesDeleted != 1 {
		t.Errorf("MessagesDeleted = %d, want 1", res.MessagesDeleted)
	}
	// THE POINT OF THE WHOLE OPERATION: the keeper must end up knowing about both
	// physical copies, or writeback and the expunge reaper stop seeing the one still on
	// the server.
	if res.ProvenanceMoved != 1 {
		t.Errorf("ProvenanceMoved = %d, want 1", res.ProvenanceMoved)
	}
	if got := scalar(t, h, `SELECT count(*) FROM app.message_provenance WHERE message_id=$1`,
		keeper.String()); got != 2 {
		t.Errorf("keeper provenance = %d, want 2", got)
	}
	if got := scalar(t, h, `SELECT count(*) FROM app.messages WHERE id=$1`, loser.String()); got != 0 {
		t.Error("the redundant row is still there")
	}
	if got := scalar(t, h, `SELECT count(*) FROM app.messages WHERE id=$1`, keeper.String()); got != 1 {
		t.Fatal("the KEEPER was deleted — this would be data loss")
	}
	// The loser's blob is orphaned by the delete (its bytes differ from the keeper's,
	// which is why it was a separate row), so it must be reclaimed.
	if len(res.ReclaimBlobs) != 1 {
		t.Errorf("ReclaimBlobs = %d, want 1 — the loser's ciphertext would be orphaned", len(res.ReclaimBlobs))
	}
	// And its now-empty thread goes, or the UI shows a conversation with nothing in it.
	if res.ThreadsDeleted != 1 {
		t.Errorf("ThreadsDeleted = %d, want 1", res.ThreadsDeleted)
	}
}

// THE TEST THE REAL DATA CANNOT PROVIDE. On a real mailbox, no labels exist only on a
// redundant copy — so a broken union would have passed every real-data check and then
// eaten labels on the next mailbox.
func TestCollapsePreservesALabelOnlyOnTheRedundantCopy(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, loser := dupPair(t, h, u, "labels")

	// A label the user applied to the COPY and not to the keeper.
	onlyLabel := NewULID().String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`,
		onlyLabel, u.ID, "only-on-loser"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
		u.ID, loser.String(), onlyLabel); err != nil {
		t.Fatal(err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, _, e := tx.CollapseDuplicateGroup(ctx, keeper, []ulid.ULID{loser})
		return e
	}); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	if got := scalar(t, h, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		keeper.String(), onlyLabel); got != 1 {
		t.Error("a label that existed ONLY on the redundant copy was lost")
	}
}

// A user's triage correction is training data. Cascading it away would silently un-teach
// the model, and the user would have no way to know.
func TestCollapseMovesAFeedbackEvent(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, loser := dupPair(t, h, u, "feedback")

	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.feedback_events (id, user_id, message_id, action)
		 VALUES ($1,$2,$3,'mark_important')`,
		NewULID().String(), u.ID, loser.String()); err != nil {
		t.Fatal(err)
	}

	var res CollapseResult
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		res, _, e = tx.CollapseDuplicateGroup(ctx, keeper, []ulid.ULID{loser})
		return e
	}); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	if res.FeedbackMoved != 1 {
		t.Errorf("FeedbackMoved = %d, want 1", res.FeedbackMoved)
	}
	if got := scalar(t, h, `SELECT count(*) FROM app.feedback_events WHERE message_id=$1`,
		keeper.String()); got != 1 {
		t.Error("the correction was lost instead of moved to the keeper")
	}
}

// FAIL CLOSED. A pending writeback intent is queued IMAP work; cascading it away would
// silently cancel something the user asked for.
func TestCollapseRefusesWhenAWritebackIntentIsStillPending(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, loser := dupPair(t, h, u, "pending")

	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.writeback_intents (id, user_id, message_id, account_id, op, idempotency_key, state)
		VALUES ($1,$2,$3,$4,'add_keyword',$5,'pending')`,
		NewULID().String(), u.ID, loser.String(), u.AccountID,
		"collapse-test-"+loser.String()); err != nil {
		t.Fatal(err)
	}

	var ok bool
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		_, ok, e = tx.CollapseDuplicateGroup(ctx, keeper, []ulid.ULID{loser})
		return e
	}); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	if ok {
		t.Error("collapsed a copy with queued IMAP work — that work would have been cancelled")
	}
	// And NOTHING changed: a refusal must be total, not partial.
	if got := scalar(t, h, `SELECT count(*) FROM app.messages WHERE id=$1`, loser.String()); got != 1 {
		t.Error("the row was deleted despite the refusal")
	}
	if got := scalar(t, h, `SELECT count(*) FROM app.message_provenance WHERE message_id=$1`,
		loser.String()); got != 1 {
		t.Error("provenance moved despite the refusal — the group is now half-merged")
	}
}

// The change_log invariant. Without these rows a client that already synced the
// duplicates keeps showing them forever and no /changes call ever says otherwise.
func TestCollapseAppendsChangeRows(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, loser := dupPair(t, h, u, "changes")

	count := func(objType, op string) int {
		return scalar(t, h, `SELECT count(*) FROM app.change_log
		                      WHERE user_id=$1 AND object_type=$2 AND op=$3`,
			u.ID, objType, op)
	}
	beforeDestroyed := count("Email", "destroyed")
	beforeUpdated := count("Email", "updated")
	beforeThreads := count("Thread", "destroyed")

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, _, e := tx.CollapseDuplicateGroup(ctx, keeper, []ulid.ULID{loser})
		return e
	}); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	if got := count("Email", "destroyed") - beforeDestroyed; got != 1 {
		t.Errorf("Email destroyed rows = %d, want 1 — clients would never drop the duplicate", got)
	}
	if got := count("Email", "updated") - beforeUpdated; got != 1 {
		t.Errorf("Email updated rows = %d, want 1 — the keeper's membership changed", got)
	}
	if got := count("Thread", "destroyed") - beforeThreads; got != 1 {
		t.Errorf("Thread destroyed rows = %d, want 1", got)
	}
}

// A thread shared with a message that SURVIVES must not be deleted.
func TestCollapseKeepsAThreadThatStillHasMessages(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, loser := dupPair(t, h, u, "sharedthread")

	// Put the loser and the keeper in the SAME thread, and confirm it survives because
	// the keeper is still in it.
	var threadID string
	if err := h.super.QueryRow(ctx,
		`SELECT thread_id FROM app.messages WHERE id=$1`, keeper.String()).Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx,
		`UPDATE app.messages SET thread_id=$2 WHERE id=$1`, loser.String(), threadID); err != nil {
		t.Fatal(err)
	}

	var res CollapseResult
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		res, _, e = tx.CollapseDuplicateGroup(ctx, keeper, []ulid.ULID{loser})
		return e
	}); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	if res.ThreadsDeleted != 0 {
		t.Errorf("ThreadsDeleted = %d, want 0 — the keeper is still in that thread", res.ThreadsDeleted)
	}
	if got := scalar(t, h, `SELECT count(*) FROM app.threads WHERE id=$1`, threadID); got != 1 {
		t.Error("deleted a thread that still contains the keeper")
	}
}

// Being asked to fold a message into itself means the caller computed the group wrongly,
// and the next step would delete the keeper. Refuse loudly rather than skip quietly.
func TestCollapseRefusesTheKeeperAsItsOwnLoser(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, _ := dupPair(t, h, u, "self")

	err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, _, e := tx.CollapseDuplicateGroup(ctx, keeper, []ulid.ULID{keeper})
		return e
	})
	if err == nil {
		t.Fatal("collapsing the keeper into itself was accepted")
	}
	if got := scalar(t, h, `SELECT count(*) FROM app.messages WHERE id=$1`, keeper.String()); got != 1 {
		t.Fatal("the keeper was deleted")
	}
}

func TestDuplicateCollapseCandidatesReportsWhatWouldMove(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "col-"+NewULID().String())
	userID := mustULID(t, u.ID)
	keeper, loser := dupPair(t, h, u, "report")

	var got []CollapseCandidate
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		got, e = tx.DuplicateCollapseCandidates(ctx, 50)
		return e
	}); err != nil {
		t.Fatalf("candidates: %v", err)
	}

	var found *CollapseCandidate
	for i := range got {
		if got[i].KeeperID == keeper {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("the seeded group was not reported: %+v", got)
	}
	if len(found.Losers) != 1 || found.Losers[0].ID != loser {
		t.Fatalf("losers = %+v, want just %s", found.Losers, loser)
	}
	if found.Losers[0].ProvenanceRows != 1 {
		t.Errorf("ProvenanceRows = %d, want 1", found.Losers[0].ProvenanceRows)
	}
	if len(found.Blockers()) != 0 {
		t.Errorf("Blockers = %v, want none", found.Blockers())
	}
	// The report is READ-ONLY: nothing may have changed.
	if n := scalar(t, h, `SELECT count(*) FROM app.messages WHERE id=$1`, loser.String()); n != 1 {
		t.Error("the report deleted something")
	}
}

func scalar(t *testing.T, h *harness, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}
