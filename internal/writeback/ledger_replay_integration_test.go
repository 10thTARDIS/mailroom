// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package writeback

// Replaying the ingest-only ledger (ADR-028 amendments A2 and A4), end to end:
// ledger -> ReplaySuppressedIntents -> a real drain against a recording writer.
//
// THE ASSERTIONS ARE ABOUT WHAT REACHED THE SERVER, not about row states. A row
// that stays 'suppressed' proves a SQL predicate; the writer receiving nothing
// proves nothing was written to the mailbox, which is the harm being avoided.
// Each skip test therefore has a control in the same file where the identical
// scenario WITHOUT the finding does reach the writer, because "the writer was
// not called" is also what a test that never replayed anything observes.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func (f *liveFixture) replay(t *testing.T) store.ReplayResult {
	t.Helper()
	var res store.ReplayResult
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		var err error
		res, err = tx.ReplaySuppressedIntents(context.Background(), f.account)
		return err
	}); err != nil {
		t.Fatalf("ReplaySuppressedIntents: %v", err)
	}
	return res
}

// shadowedMoveFixture: a message in INBOX, a move to Trash enqueued while the
// account is shadowed (so it is a ledger row), then the mode lifted.
func shadowedMoveFixture(t *testing.T) (*liveFixture, ulid.ULID, ulid.ULID) {
	t.Helper()
	f := newLiveFixture(t)
	f.setWriteback(t, false)
	msg := f.seedMessage(t, "filed while shadowed")
	f.seedLiveCopy(t, msg, f.inbox, 11)
	id := f.enqueue(t, msg, store.IntentMove, `{"mailbox":"Trash"}`, "ledger-move")
	if got := f.intentState(t, id); got != "suppressed" {
		t.Fatalf("setup: the move is %q, want suppressed - the account is not shadowed, so this tests nothing", got)
	}
	f.setWriteback(t, true)
	return f, msg, id
}

// TestReplayDoesNotMoveAMessageThatWasFiledElsewhere is the delta-replay
// problem: the move was recorded when the message sat in INBOX, another client
// has since put a copy elsewhere, and applying the move would be applied to
// wherever the message is NOW.
//
// NEGATIVE CONTROL (in the code): remove `AND NOT l.stale` from
// ReplaySuppressedIntents and the writer receives the move and this test fails.
func TestReplayDoesNotMoveAMessageThatWasFiledElsewhere(t *testing.T) {
	f, msg, id := shadowedMoveFixture(t)
	f.seedLiveCopy(t, msg, f.trash, 77) // first seen AFTER the intent was created

	res := f.replay(t)
	if res.Replayed != 0 || res.SkippedStale != 1 {
		t.Fatalf("replay = %+v, want 0 replayed and 1 skipped-stale", res)
	}
	w := &fakeWriter{supportsMove: true}
	f.drain(t, w)
	if len(w.calls) != 0 {
		t.Fatalf("the writer received %+v for a stale move: filing done since was reversed", w.calls)
	}
	if got := f.intentState(t, id); got != "suppressed" {
		t.Errorf("stale intent is %q, want it left in the ledger as suppressed", got)
	}
}

// TestReplayStillMovesAMessageNothingElseTouched is the control for the test
// above: the same ledger, no new copy, and the move reaches the server. Without
// it the stale skip could be "replay never writes".
func TestReplayStillMovesAMessageNothingElseTouched(t *testing.T) {
	f, _, id := shadowedMoveFixture(t)

	res := f.replay(t)
	if res.Replayed != 1 || res.SkippedStale != 0 || res.SkippedSuperseded != 0 {
		t.Fatalf("replay = %+v, want exactly 1 replayed", res)
	}
	w := &fakeWriter{supportsMove: true}
	f.drain(t, w)
	if n := len(w.callsOfKind("move")); n != 1 {
		t.Fatalf("the writer received %d move(s), want 1 (calls %+v)", n, w.calls)
	}
	if got := f.intentState(t, id); got != "applied" {
		t.Errorf("intent is %q, want applied", got)
	}
}

// purgeExpungeLedger: a trashed, overdue message with its only copy in Trash and
// a purge EXPUNGE that was enqueued while the account was live, parked as a
// ledger row when the account was shadowed, and then the mode lifted and the
// ledger replayed.
func purgeExpungeLedger(t *testing.T, restoreBeforeReplay bool) (Result, *fakeWriter) {
	t.Helper()
	ctx := context.Background()
	f := newLiveFixture(t)
	msg := f.seedMessage(t, "purge in the ledger")
	f.seedLiveCopy(t, msg, f.trash, 77)
	f.markTrashedAndDue(t, msg)

	var enqueued int
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		accts, _, err := tx.ExpungeableAccountIDs(ctx, msg)
		if err != nil {
			return err
		}
		enqueued, err = tx.EnqueuePurgeExpungeIntents(ctx, msg, accts)
		return err
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("armed %d purge expunges, want 1 - nothing to replay, the test would prove nothing", enqueued)
	}
	// Shadow the account the way Account/set does: the pending backlog becomes
	// the ledger. Then lift the mode again.
	f.setWriteback(t, false)
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.SuppressAccountIntents(ctx, f.account)
		return err
	}); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if got := f.countByState(t); got["suppressed"] != 1 {
		t.Fatalf("setup: states %v, want one suppressed purge expunge", got)
	}
	f.setWriteback(t, true)

	if restoreBeforeReplay {
		// A restore through another client or a local restore whose MOVE has not
		// been written: the logical state no longer says "destroy this".
		if _, err := f.super.Exec(ctx,
			`UPDATE app.messages SET trashed_at = NULL, purge_after = NULL
			  WHERE user_id = $1 AND id = $2`, f.userID.String(), msg.String()); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	if res := f.replay(t); res.Replayed != 1 {
		t.Fatalf("replay = %+v, want the purge expunge replayed (it is neither stale nor superseded: "+
			"the engine's apply-time check is what must decide it)", res)
	}
	w := &fakeWriter{}
	return f.drain(t, w), w
}

// TestReplayedPurgeExpungeIsRecheckedAtApplyTime: the ledger row was decided at
// enqueue time, weeks ago. By the time the operator replays it the message may
// have been restored; the engine must ask again and withdraw it.
//
// NEGATIVE CONTROL (in the code): delete the `PurgeExpungeStillEligible` branch
// in Engine.applyIntent and the writer receives the EXPUNGE.
func TestReplayedPurgeExpungeIsRecheckedAtApplyTime(t *testing.T) {
	res, w := purgeExpungeLedger(t, true)
	if res.Expunged != 0 || len(w.callsOfKind("expunge")) != 0 {
		t.Fatalf("a replayed purge EXPUNGE for a restored message reached the server: result %+v calls %+v", res, w.calls)
	}
	if res.Withdrawn != 1 {
		t.Errorf("Withdrawn = %d, want 1: the guard must be observable", res.Withdrawn)
	}
}

// TestReplayedPurgeExpungeStillRunsWhenStillDue is the control: the same ledger
// row for a message that is still trashed and overdue DOES expunge.
func TestReplayedPurgeExpungeStillRunsWhenStillDue(t *testing.T) {
	res, w := purgeExpungeLedger(t, false)
	if res.Expunged != 1 || len(w.callsOfKind("expunge")) != 1 {
		t.Fatalf("a replayed purge EXPUNGE for a still-due message did not run: result %+v calls %+v", res, w.calls)
	}
}
