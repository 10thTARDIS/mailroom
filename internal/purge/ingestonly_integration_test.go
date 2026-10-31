// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package purge

// Purge under ingest-only mode (ADR-028 §7/§7a).
//
// ⚠ THIS FILE GUARDS A PATH THAT DESTROYS REAL MAIL. It is not a tidiness test.
//
// Trashing stamps purge_after in the same statement as trashed_at, so the
// retention clock starts even though the move to Trash is only a SUPPRESSED
// ledger entry that never reached the server. The first draft of ADR-028
// decided to skip phase-1 enqueue for a shadowed account and leave purge_after
// alone. The consequence, found in review rather than in production:
//
//	the moment writeback is enabled, every message trashed longer ago than the
//	window is instantly due; its EXPUNGE is enqueued 'pending' because writeback
//	is now live; and it is applied to EVERY LIVE COPY — which is still INBOX,
//	because the move never landed.
//
// A batch per sweep, no --apply, no review: the most destructive op class would
// have been the one thing that escaped §4's promise, and it would have escaped
// on a timer rather than by anyone clicking anything.
//
// ⚠ AND THE FIRST FIX FOR IT WAS ALSO WRONG — a second review caught that too.
// Deferring purge_after only reaches a message DueForPurge already returned,
// and that query selects `purge_after <= now`. Anything trashed DURING the run
// is not due yet, so its clock was never touched; it just came due later,
// possibly after the lift. On a six-week run with a 30-day window only the
// first fortnight's deletions were ever deferred.
//
// So there are three properties here, and the THIRD is the one that actually
// makes the mode safe:
//  1. while shadowed, no expunge is enqueued;
//  2. a due message's purge_after is deferred (this stops a large backlog
//     monopolising every DueForPurge batch, and gives a sane window once the
//     move lands) — necessary, not sufficient;
//  3. an EXPUNGE is enqueued for an account only when every live copy that
//     account holds is physically in that account's \Trash mailbox
//     (store.ExpungeableAccountIDs), not by any ledger state — a ledger check
//     on suppressed MOVE intents was tried and rejected because it could not
//     distinguish the latest trash move from any historical one and treated a
//     dead-lettered move as permission to destroy. Evaluated per message at
//     sweep time, so it holds regardless of WHEN the message became due.
//
// Runs against the throwaway Postgres from scripts/integration-test.sh.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// setWriteback flips the account's per-account switch, as Account/set does.
func (h *purgeHarness) setWriteback(t *testing.T, enabled bool) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.accounts SET writeback_enabled = $3 WHERE user_id = $1 AND id = $2`,
		h.userID.String(), h.account.String(), enabled); err != nil {
		t.Fatalf("set writeback_enabled=%v: %v", enabled, err)
	}
}

// purgeAfter reads the message's retention deadline as the superuser.
func (h *purgeHarness) purgeAfter(t *testing.T, msg ulid.ULID) time.Time {
	t.Helper()
	var ts *time.Time
	if err := h.super.QueryRow(context.Background(),
		`SELECT purge_after FROM app.messages WHERE user_id = $1 AND id = $2`,
		h.userID.String(), msg.String()).Scan(&ts); err != nil {
		t.Fatalf("read purge_after: %v", err)
	}
	if ts == nil {
		t.Fatalf("purge_after is NULL for %s", msg)
	}
	return *ts
}

// intentCount counts the account's writeback intents in the given state.
func (h *purgeHarness) intentCount(t *testing.T, state string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents
		  WHERE user_id = $1 AND account_id = $2 AND state::text = $3`,
		h.userID.String(), h.account.String(), state).Scan(&n); err != nil {
		t.Fatalf("count intents in %s: %v", state, err)
	}
	return n
}

// TestShadowedPurgeEnqueuesNothingAndDefersTheDeadline is the regression test
// for the destruction path described at the top of this file.
func TestShadowedPurgeEnqueuesNothingAndDefersTheDeadline(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, false)

	msg, _ := h.seedDueMessage(t, "trashed while shadowed")
	h.seedProvenance(t, msg) // a live server copy: this is a phase-1 message
	before := h.purgeAfter(t, msg)

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// It was a candidate — otherwise the rest of this test proves nothing about
	// shadowing, only that the message was invisible to the sweep.
	if res.Candidates != 1 {
		t.Fatalf("candidates = %d, want 1 — the sweep never considered the message", res.Candidates)
	}
	if res.ShadowedSkipped != 1 {
		t.Errorf("shadowedSkipped = %d, want 1 (result %+v)", res.ShadowedSkipped, res)
	}
	// The other side of the pair with TestACopyOutsideTrashInTheSameAccount
	// BlocksTheExpunge: both scenarios withhold the EXPUNGE, and the counters
	// must tell them apart. Here the copy IS in Trash and the mode is the only
	// reason — so `unlanded` would be the false report.
	if res.UnlandedSkipped != 0 {
		t.Errorf("unlandedSkipped = %d, want 0 — the copy is in Trash; the mode is "+
			"the whole reason (%+v)", res.UnlandedSkipped, res)
	}
	if res.ExpungeEnqueued != 0 {
		t.Errorf("expungeEnqueued = %d, want 0 — an EXPUNGE was queued for a shadowed account", res.ExpungeEnqueued)
	}
	if n := h.intentCount(t, "pending"); n != 0 {
		t.Errorf("pending intents = %d, want 0", n)
	}

	// THE HALF THAT ACTUALLY PREVENTS THE DESTRUCTION: the deadline moved out,
	// so the message is no longer due and lifting the mode does not make it due.
	after := h.purgeAfter(t, msg)
	if !after.After(before) {
		t.Fatalf("purge_after was not deferred (before %s, after %s): "+
			"the message is still due, so enabling writeback would expunge it "+
			"from wherever it sits — which is INBOX, because the trash move is "+
			"suppressed in the ledger", before, after)
	}
	if !after.After(time.Now().UTC()) {
		t.Errorf("purge_after %s is still in the past", after)
	}

	// And the message survives — nothing was hard-deleted either.
	if !h.exists(t, msg) {
		t.Error("message was hard-deleted despite having a live server copy")
	}
}

// TestLiftingTheModeDoesNotImmediatelyExpunge is the end-to-end statement of
// the bug: shadow, sweep, lift, sweep again, and require that the second sweep
// does NOT enqueue an expunge, because the deferral bought a full window.
//
// ⚠ This is the test that would have caught the original defect. The one above
// asserts the mechanism; this one asserts the OUTCOME the operator cares about
// — that flipping the switch is not itself a destructive act.
func TestLiftingTheModeDoesNotImmediatelyExpunge(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, false)

	msg, _ := h.seedDueMessage(t, "trashed while shadowed, then lifted")
	h.seedProvenance(t, msg)

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	if _, err := w.Sweep(context.Background(), h.userID); err != nil {
		t.Fatalf("shadowed Sweep: %v", err)
	}

	// The operator lifts the mode. Nothing else.
	h.setWriteback(t, true)

	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("post-lift Sweep: %v", err)
	}
	if res.Candidates != 0 {
		t.Errorf("candidates immediately after lift = %d, want 0 — the message is "+
			"still due and its EXPUNGE is about to fire unreviewed", res.Candidates)
	}
	if res.ExpungeEnqueued != 0 {
		// ⚠ Wording matters here: this fixture's copy is in TRASH
		// (harness seedProvenance), so what this test proves is "an EXPUNGE was
		// enqueued the instant the mode was lifted", not "against a copy in
		// INBOX". The INBOX case is TestTrashedDuringTheRunIsNotExpungedAfterThe
		// Lift, which seeds it explicitly. An earlier version of this message
		// claimed INBOX and was describing a scenario its own fixture did not
		// model.
		t.Fatalf("expungeEnqueued = %d immediately after lifting ingest-only mode, want 0 "+
			"— a deletion reached the server with no --apply and no review", res.ExpungeEnqueued)
	}
	if n := h.intentCount(t, "pending"); n != 0 {
		t.Errorf("pending intents after lift = %d, want 0", n)
	}
}

// TestUnshadowedPurgeStillEnqueuesAndDoesNotDefer is the NEGATIVE CONTROL for
// both tests above.
//
// ⚠ REQUIRED, not decorative. Every assertion above is satisfied by a purge
// worker that has simply stopped enqueueing anything, or by a `DueForPurge`
// that returns nothing — both far worse bugs, and both invisible without this.
func TestUnshadowedPurgeStillEnqueuesAndDoesNotDefer(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, true)

	msg, _ := h.seedDueMessage(t, "trashed on a live account")
	h.seedProvenance(t, msg)
	before := h.purgeAfter(t, msg)

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ExpungeEnqueued != 1 {
		t.Errorf("expungeEnqueued = %d, want 1 — a live account stopped purging (result %+v)",
			res.ExpungeEnqueued, res)
	}
	if res.ShadowedSkipped != 0 {
		t.Errorf("shadowedSkipped = %d, want 0 on a writeback-enabled account", res.ShadowedSkipped)
	}
	if n := h.intentCount(t, "pending"); n != 1 {
		t.Errorf("pending intents = %d, want 1", n)
	}
	// The deadline must NOT move for a live account: deferring there would
	// postpone a hard delete the user actually asked for.
	if got := h.purgeAfter(t, msg); !got.Equal(before) {
		t.Errorf("purge_after moved on a live account (before %s, after %s)", before, got)
	}
}

// TestInstanceFlagAloneShadowsPurge covers the switch the per-account column
// cannot express: the account's own writeback_enabled is TRUE, and only
// MAILROOM_WRITEBACK_DISABLED is holding it back.
//
// That is the production cutover configuration, so it is the one that must not
// be left untested — and it is a distinct code path, since ExpungeableAccountIDs
// short-circuits on the instance flag (tx.writebackDisabled) before it ever
// queries the per-account writeback_enabled column.
func TestInstanceFlagAloneShadowsPurge(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, true) // the COLUMN says yes...
	h.st.SetWritebackDisabled(true)
	t.Cleanup(func() { h.st.SetWritebackDisabled(false) })

	msg, _ := h.seedDueMessage(t, "shadowed by the instance flag only")
	h.seedProvenance(t, msg)
	before := h.purgeAfter(t, msg)

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ShadowedSkipped != 1 || res.ExpungeEnqueued != 0 {
		t.Errorf("instance flag did not shadow the purge: %+v", res)
	}
	if got := h.purgeAfter(t, msg); !got.After(before) {
		t.Errorf("purge_after was not deferred under the instance flag (before %s, after %s)",
			before, got)
	}
}

// --- the timing gap the first fix missed -------------------------------------

// seedInbox creates an INBOX mailbox for the harness account and returns its id.
//
// ⚠ The shared harness only seeds Trash, and seedProvenance puts the copy
// THERE. That models a message whose trash move already landed, which is not
// the shadow case at all: in shadow mode the move is suppressed, so the copy is
// still wherever the user left it. Tests about this mode must seed INBOX or
// they are describing a scenario that cannot arise.
func (h *purgeHarness) seedInbox(t *testing.T) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		 VALUES ($1, $2, $3, 'INBOX', 'Inbox')`,
		id.String(), h.userID.String(), h.account.String()); err != nil {
		t.Fatalf("seed inbox: %v", err)
	}
	return id
}

// seedNotYetDueTrashed inserts a message the user trashed but whose retention
// window has NOT expired — the state of everything trashed during a shadow run.
func (h *purgeHarness) seedNotYetDueTrashed(t *testing.T, subject string) ulid.ULID {
	t.Helper()
	id, _ := h.seedDueMessage(t, subject)
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET purge_after = now() + interval '20 days'
		  WHERE user_id = $1 AND id = $2`,
		h.userID.String(), id.String()); err != nil {
		t.Fatalf("push purge_after into the future: %v", err)
	}
	return id
}

// seedProvenanceIn attaches a live copy at a specific mailbox.
func (h *purgeHarness) seedProvenanceIn(t *testing.T, msg, mailbox ulid.ULID, uid int64) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, $6, 42, 'live')`,
		store.NewULID().String(), h.userID.String(), msg.String(), h.account.String(),
		mailbox.String(), uid); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
}

// seedMoveIntent inserts the trash MOVE writeback intent in the given state —
// 'suppressed' for the shadow case, 'applied' for the control.
func (h *purgeHarness) seedMoveIntent(t *testing.T, msg ulid.ULID, state string) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.writeback_intents
		        (id, user_id, message_id, account_id, op, payload, idempotency_key, state)
		 VALUES ($1, $2, $3, $4, 'move', '{"mailbox":"Trash"}', $5, $6::app.intent_state)`,
		store.NewULID().String(), h.userID.String(), msg.String(), h.account.String(),
		"move-"+msg.String(), state); err != nil {
		t.Fatalf("seed move intent (%s): %v", state, err)
	}
}

// makeDue brings a not-yet-due message's deadline into the past, standing in
// for the passage of time so the test needs no clock injection.
func (h *purgeHarness) makeDue(t *testing.T, msg ulid.ULID) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET purge_after = now() - interval '1 hour'
		  WHERE user_id = $1 AND id = $2`,
		h.userID.String(), msg.String()); err != nil {
		t.Fatalf("make due: %v", err)
	}
}

// TestTrashedDuringTheRunIsNotExpungedAfterTheLift is the regression test for
// the gap the FIRST fix left, found by a second review and reproduced before
// being fixed.
//
// ⚠ WHY THE FIRST FIX WAS NOT ENOUGH. DeferPurgeWhileShadowed is reachable only
// for a message DueForPurge already returned, and that query selects
// `purge_after <= now`. A message trashed DURING the run is not due yet, so its
// clock is never touched — it simply comes due later, by which time the mode
// may have been lifted. On a six-week run with a 30-day window, only the first
// fortnight's deletions were ever deferred. Everything trashed after that kept
// its original deadline and would have been expunged from INBOX.
//
// The real invariant is not about deadlines at all: never EXPUNGE a copy whose
// trash MOVE has not landed.
func TestTrashedDuringTheRunIsNotExpungedAfterTheLift(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, false)
	inbox := h.seedInbox(t)

	// Trashed in Mailroom during the shadow run: the move is suppressed and the
	// copy is still in INBOX. Not due yet.
	msg := h.seedNotYetDueTrashed(t, "trashed mid-run")
	h.seedProvenanceIn(t, msg, inbox, 1)
	h.seedMoveIntent(t, msg, "suppressed")

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("shadowed Sweep: %v", err)
	}
	if res.Candidates != 0 {
		t.Fatalf("setup: candidates = %d, want 0 — the message should not be due yet, "+
			"so this test is not exercising the timing gap", res.Candidates)
	}

	// The operator lifts the mode, and time passes.
	h.setWriteback(t, true)
	h.makeDue(t, msg)

	res, err = w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("post-lift Sweep: %v", err)
	}
	if res.Candidates != 1 {
		t.Fatalf("setup: candidates = %d, want 1 — the message should now be due", res.Candidates)
	}
	if res.ExpungeEnqueued != 0 {
		t.Fatalf("expungeEnqueued = %d, want 0. The trash MOVE is still suppressed in "+
			"the ledger, so the only copy is in INBOX and this EXPUNGE would destroy "+
			"inbox mail the server never saw deleted.", res.ExpungeEnqueued)
	}
	if n := h.intentCount(t, "pending"); n != 0 {
		t.Errorf("pending intents = %d, want 0", n)
	}
}

// TestAppliedTrashMoveStillExpunges is the NEGATIVE CONTROL for the test above.
//
// ⚠ REQUIRED. Every assertion above is equally satisfied by a purge worker that
// has stopped enqueueing anything at all — which would silently break Trash for
// every normal account. The only difference here is that the MOVE landed.
func TestAppliedTrashMoveStillExpunges(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, true)

	msg := h.seedNotYetDueTrashed(t, "trash move landed")
	h.seedProvenanceIn(t, msg, h.trash, 2)
	h.seedMoveIntent(t, msg, "applied")
	h.makeDue(t, msg)

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ExpungeEnqueued != 1 {
		t.Errorf("expungeEnqueued = %d, want 1 — a landed trash move stopped purging (%+v)",
			res.ExpungeEnqueued, res)
	}
	if res.ShadowedSkipped != 0 {
		t.Errorf("shadowedSkipped = %d, want 0", res.ShadowedSkipped)
	}
}

// seedSecondAccount adds another account to the harness and returns its id
// plus an INBOX and a \Trash mailbox inside it.
//
// Both mailboxes, because the predicate under test is PHYSICAL: an account
// qualifies for a phase-1 EXPUNGE only when every live copy it holds is in that
// account's Trash. A fixture with only an INBOX can express "must not expunge"
// but not its control, and a rule that always says no is not a rule.
func (h *purgeHarness) seedSecondAccount(t *testing.T) (acct, inbox, trash ulid.ULID) {
	t.Helper()
	acct, inbox, trash = store.NewULID(), store.NewULID(), store.NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(context.Background(), sql, args...); err != nil {
			t.Fatalf("seed second account: %v", err)
		}
	}
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'secondary', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		acct.String(), h.userID.String(), store.NewULID().String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	      VALUES ($1, $2, $3, 'INBOX', 'Inbox')`,
		inbox.String(), h.userID.String(), acct.String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1, $2, $3, 'Trash', 'Trash', '\Trash')`,
		trash.String(), h.userID.String(), acct.String())
	return acct, inbox, trash
}

// TestMultiAccountUnlandedMoveIsNotExpunged is the regression test for the
// defect a third review found and reproduced: the safety check lived in the
// WORKER while the enqueue resolved its own account set in the STORE, with a
// different predicate.
//
// ⚠ THE SHAPE: account A lifted but its trash move never landed (copy still in
// INBOX); account B fine (copy genuinely in Trash). The worker skipped A and
// then asked the store to enqueue — and the store, seeing B had made the
// message a candidate, enqueued for BOTH. A's EXPUNGE went `pending` because A
// is live now, and the engine applied it to A's INBOX copy.
//
// A single-account test cannot see this, and ExpungeEnqueued is a per-MESSAGE
// count, so the assertion has to be per-account on the intent rows themselves.
func TestMultiAccountUnlandedMoveIsNotExpunged(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, true) // A is live again after a shadow run
	inboxA := h.seedInbox(t)
	acctB, _, trashB := h.seedSecondAccount(t)

	msg, _ := h.seedDueMessage(t, "A still in inbox, B in trash")
	h.seedProvenanceIn(t, msg, inboxA, 1) // A: the move never landed
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, 9, 42, 'live')`,
		store.NewULID().String(), h.userID.String(), msg.String(), acctB.String(),
		trashB.String()); err != nil {
		t.Fatalf("seed B trash copy: %v", err)
	}

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	if _, err := w.Sweep(context.Background(), h.userID); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Per-account, because a per-message count cannot distinguish "enqueued for
	// B" from "enqueued for A and B".
	countFor := func(acct ulid.ULID) int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(context.Background(),
			`SELECT count(*) FROM app.writeback_intents
			  WHERE user_id = $1 AND message_id = $2 AND account_id = $3 AND op = 'expunge'`,
			h.userID.String(), msg.String(), acct.String()).Scan(&n); err != nil {
			t.Fatalf("count intents: %v", err)
		}
		return n
	}
	if got := countFor(h.account); got != 0 {
		t.Errorf("account A got %d EXPUNGE intent(s), want 0 — its copy is in INBOX, "+
			"so this would destroy inbox mail", got)
	}
	if got := countFor(acctB); got != 1 {
		t.Errorf("account B got %d EXPUNGE intent(s), want 1 — its copy IS in Trash, "+
			"so purge must still work there", got)
	}
}

// TestStaleProvenanceIsNotAWritebackTarget pins the other half of the same
// defect.
//
// MessageProvenanceTargetsForAccount deliberately falls back to returning STALE
// (stale_uidvalidity) coordinates when the account holds no live copy, so that
// a caller never receives a mixed slice. `len(targets) > 0` therefore counted a
// stale-only account as a live phase-1 target: the sweep reported an
// ExpungeEnqueued it had not enqueued, skipped the deferral, and left the
// message due on every following sweep.
//
// ⚠ THE FIXTURE NEEDS TWO ACCOUNTS, AND THE FIRST VERSION OF THIS TEST DID NOT
// HAVE THEM — it seeded a single stale row and passed with the fix removed.
// With no live row anywhere, DueForPurge reports HasLiveProvenance=false and
// the worker goes to phase 2 (hard delete) without ever calling
// ExpungeableAccountIDs, so the assertion held for a reason unrelated to the
// defect. Caught by probing, which is the only thing that finds this shape.
//
// The real scenario needs the message to be a phase-1 candidate (a live copy
// SOMEWHERE) while the account under test has only a stale coordinate: a live
// copy in the shadowed account, a stale one in the writeback-enabled account.
func TestStaleProvenanceIsNotAWritebackTarget(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, false) // account A: shadowed, holds the LIVE copy
	inboxA := h.seedInbox(t)
	acctB, inboxB, _ := h.seedSecondAccount(t) // account B: writeback ON, STALE only

	msg, _ := h.seedDueMessage(t, "live in A, stale in B")
	h.seedProvenanceIn(t, msg, inboxA, 1)
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, 7, 41, 'stale_uidvalidity')`,
		store.NewULID().String(), h.userID.String(), msg.String(), acctB.String(),
		inboxB.String()); err != nil {
		t.Fatalf("seed stale provenance: %v", err)
	}

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	// It must have reached phase 1 — otherwise ExpungeableAccountIDs was never
	// called and this test proves nothing, which is exactly how its first
	// version passed against the bug.
	if res.Candidates != 1 {
		t.Fatalf("setup: candidates = %d, want 1", res.Candidates)
	}
	if res.Purged != 0 {
		t.Fatalf("setup: purged = %d, want 0 — the message went down the phase-2 "+
			"hard-delete path, so ExpungeableAccountIDs was never consulted", res.Purged)
	}
	if res.ExpungeEnqueued != 0 {
		t.Errorf("expungeEnqueued = %d, want 0 — a stale coordinate in account B was "+
			"counted as a live writeback target (%+v)", res.ExpungeEnqueued, res)
	}
	if res.ShadowedSkipped != 1 {
		t.Errorf("shadowedSkipped = %d, want 1 (%+v)", res.ShadowedSkipped, res)
	}
}

// TestLiveProvenanceInASecondAccountIsAWritebackTarget is the NEGATIVE CONTROL
// for the test above: same two-account shape, but B's coordinate is LIVE.
//
// Without it, the stale test passes just as well against a worker that treats
// no second account as a target ever.
// ⚠ B's copy must be in B's TRASH, not its INBOX. An earlier version of this
// test seeded INBOX and went red under the physical predicate — correctly, and
// that is the point: a live copy is not licence to destroy it, only a live copy
// IN TRASH is. The control has to model a message whose deletion actually
// reached the server.
func TestLiveProvenanceInASecondAccountIsAWritebackTarget(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, false)
	inboxA := h.seedInbox(t)
	acctB, _, trashB := h.seedSecondAccount(t)

	msg, _ := h.seedDueMessage(t, "live in A's inbox, trashed in B")
	h.seedProvenanceIn(t, msg, inboxA, 1)
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, 7, 42, 'live')`,
		store.NewULID().String(), h.userID.String(), msg.String(), acctB.String(),
		trashB.String()); err != nil {
		t.Fatalf("seed live provenance in B's Trash: %v", err)
	}

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ExpungeEnqueued != 1 {
		t.Errorf("expungeEnqueued = %d, want 1 — a live copy in a writeback-enabled "+
			"second account must still be expunged (%+v)", res.ExpungeEnqueued, res)
	}
}

// --- the NULL the fourth attempt missed --------------------------------------

// TestACopyOutsideTrashInTheSameAccountBlocksTheExpunge is the regression test
// for a defect in the fix the three tests above pin — the FOURTH attempt at this
// one safety decision, undone by NULL.
//
// ⚠ THE SHAPE: ONE account, writeback on, a due message with TWO live copies —
// one genuinely in Trash (its move landed) and one still in an ordinary folder.
// That is not exotic: it is re-delivered mail (#206) and a copy filed in two
// places, which is why the engine applies an op to EVERY live copy in the
// account (applyToEveryCopy, #296) rather than to one.
//
// ExpungeableAccountIDs asked `HAVING bool_and(m.special_use = '\Trash')`, and
// app.mailboxes.special_use is NULLABLE — an ordinary folder carries NULL, INBOX
// included (store/drafts.go has to match inbox by NAME for exactly this reason).
// So the comparison yields NULL for that copy, and bool_and SKIPS NULL inputs
// like every other SQL aggregate.
//
// MEASURED on PostgreSQL 18 rather than reasoned about, because the whole defect
// is one aggregate's NULL rule:
//
//	bool_and over {true, NULL} -> true      <- the hole: HAVING passes
//	bool_and over {NULL}       -> NULL      <- HAVING drops it (correct, by luck)
//	bool_and over {COALESCE..} -> false     <- the fix: stated, not inferred
//
// So the account passed the HAVING on the strength of its Trash copy alone, the
// EXPUNGE was enqueued, and the engine issued it against every live copy —
// destroying the one that was never trashed. That is the IDENTICAL destruction
// TestMultiAccountUnlandedMoveIsNotExpunged exists to prevent, reached through
// NULL instead of through two predicates disagreeing.
//
// ⚠ WHY NO EXISTING TEST COULD SEE IT. Every other test here puts an account's
// copies ALL in Trash or ALL outside it, and all-outside is excluded because
// bool_and over {NULL} is NULL — the right answer for the wrong reason. Only a
// MIXED set inside one account distinguishes "every copy is in Trash" from "some
// copy is in Trash", and nothing mixed one until now.
func TestACopyOutsideTrashInTheSameAccountBlocksTheExpunge(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, true) // writeback is ON: this is not about the mode
	inbox := h.seedInbox(t)

	msg, _ := h.seedDueMessage(t, "one copy trashed, one still in inbox")
	h.seedProvenanceIn(t, msg, h.trash, 1) // this copy's trash move landed
	h.seedProvenanceIn(t, msg, inbox, 2)   // ...this one was never trashed

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ExpungeEnqueued != 0 {
		t.Errorf("expungeEnqueued = %d, want 0. A live copy of this message sits in "+
			"INBOX, and an expunge intent is applied to EVERY live copy in the "+
			"account — so this EXPUNGE would destroy mail the user never trashed. "+
			"(%+v)", res.ExpungeEnqueued, res)
	}
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents
		  WHERE user_id = $1 AND message_id = $2 AND op = 'expunge'`,
		h.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if n != 0 {
		t.Errorf("%d EXPUNGE intent(s) queued, want 0 — the count above can be "+
			"read as bookkeeping, but a queued row is the thing that destroys mail", n)
	}

	// AND THE SKIP MUST NAME THE RIGHT CAUSE. Withholding the EXPUNGE is only
	// half the job: this account's writeback is ON, so reporting the skip as
	// `shadowed` would file a stalled trash move under the one cause an operator
	// is expecting to see thousands of during a shadow run, and it would never be
	// looked at. TestShadowedPurgeEnqueuesNothingAndDefersTheDeadline is the
	// other side of this pair — same skip, different cause.
	if res.UnlandedSkipped != 1 {
		t.Errorf("unlandedSkipped = %d, want 1 — the copy in INBOX means the trash "+
			"move has not landed, which is the actionable cause (%+v)", res.UnlandedSkipped, res)
	}
	if res.ShadowedSkipped != 0 {
		t.Errorf("shadowedSkipped = %d, want 0 — writeback is ENABLED on this "+
			"account, so attributing the skip to ingest-only mode is a false "+
			"report, not a rounding error (%+v)", res.ShadowedSkipped, res)
	}
}

// TestEveryCopyInTrashStillExpunges is the NEGATIVE CONTROL for the test above,
// and it is not a formality: the cheap way to make that test pass is to refuse
// any message with more than one live copy, which would silently stop purging
// re-delivered mail (#206) forever — the exact "ran without erroring while doing
// nothing" failure this project keeps paying for.
//
// TWO copies, BOTH in Trash, one account. Every copy IS in that account's Trash,
// so destroying them is precisely what the user asked for, and the EXPUNGE must
// still be enqueued.
func TestEveryCopyInTrashStillExpunges(t *testing.T) {
	h := newPurgeHarness(t)
	h.setWriteback(t, true)

	msg, _ := h.seedDueMessage(t, "re-delivered, both copies trashed")
	h.seedProvenanceIn(t, msg, h.trash, 1)
	h.seedProvenanceIn(t, msg, h.trash, 2)

	w := NewWorker(h.st, newFakeBlobs(), time.Hour, nil)
	res, err := w.Sweep(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ExpungeEnqueued != 1 {
		t.Errorf("expungeEnqueued = %d, want 1 — both copies are in Trash, so purge "+
			"must still work; a fix that keys on copy COUNT rather than on where the "+
			"copies ARE would break here (%+v)", res.ExpungeEnqueued, res)
	}
	if res.ShadowedSkipped != 0 {
		t.Errorf("shadowedSkipped = %d, want 0 (%+v)", res.ShadowedSkipped, res)
	}
}
