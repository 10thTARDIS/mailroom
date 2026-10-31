// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file holds the store surface for the DELETION lifecycle's terminal step:
// the purge (ADR-009, data-model §6). Trash and Restore live in jmap_email.go
// (soft-delete: trashed_at/purge_after set/cleared, an Email `updated`). Purge is
// the terminal step of the USER-VISIBLE lifecycle — the one a Trash entry ages
// into — and a hard delete with full propagation.
//
// ⚠ IT IS NOT THE ONLY WAY A ROW LEAVES app.messages, and this comment claimed
// it was until #300 counted (there are four `DELETE FROM app.messages` sites).
// The claim mattered because the other three were written on the strength of it
// and reclaimed no blobs; two of the four orphaned app.blobs rows on the live
// instance came from that. The full list, so the next reader does not have to
// grep for it:
//
//	purge.go:HardDeleteMessage   the Trash window closed             (this file)
//	collapse.go:CollapseDuplicateGroup  duplicate rows merged onto a keeper (#206)
//	drafts.go:RetireDraft        a superseded draft revision         (spec §5)
//	accounts.go:DeleteAccount    messages that lived only in that account
//
// All four now reclaim through the same guard — see blobreclaim.go, which also
// explains the third referent (app.ml_models.artifact_hash) that neither this
// path nor collapse checked before #300. What IS true, and is the property
// ADR-009 actually needs, is that purge is the only path REACHABLE FROM AN
// EMAIL/SET: the deepest delete a client (or an agent driving one) can ask for
// is the Trash transition. The other three are not user-requested deletes at
// all — a draft revision the client itself replaced, duplicate rows of one
// message, and an account the user asked to remove.
//
// PROPAGATE-FIRST, CANONICAL-LAST. A message with live IMAP provenance still has
// copies sitting on servers we sync. Deleting its canonical row while those
// copies exist would (a) throw away the Message-ID/coordinates needed to ever
// find them again and (b) drop the audit trail while the message still exists
// somewhere — exactly the trap ADR-009's consequences warn against ("a delete is
// never silently dropped"). So the canonical row is deleted LAST, only once every
// provenance copy is gone:
//
//   - A due message WITH live provenance is not hard-deleted. Its per-account
//     EXPUNGE is enqueued through the writeback path (EnqueuePurgeExpungeIntents),
//     which the engine applies and dead-letters — SURFACED, never swallowed — on
//     failure. The server drops the copy; the reaper (#86) later observes the UID
//     gone and drops the provenance row.
//   - A due message with NO live provenance (its copies already expunged, or a
//     reaper-vanished / local-only message that never had any) is hard-deleted now
//     (HardDeleteMessage).
//
// The reaper is thus the phase-1→phase-2 transition, reusing machinery that
// already fails closed. A message whose EXPUNGE keeps failing keeps its
// provenance, stays in Trash, and its dead-letter is visible in account health —
// which is the correct conservative outcome, not a bug.

// tombstonePrefixBytes is how many leading bytes of the content hash the audit
// tombstone records (ADR-009's content_hash_prefix). Enough to correlate a purge
// with an external record of the bytes, far too few to be the address itself.
const tombstonePrefixBytes = 8

// PurgeAction is the audit-log action string for a hard delete (ADR-009's
// tombstone: {action, object_id, content_hash_prefix}).
const PurgeAction = "message.purge"

// PurgeCandidate is one message the purge sweep must act on: it is trashed and
// past its purge_after. HasLiveProvenance decides the phase — expunge-first
// (true) versus hard-delete-now (false).
type PurgeCandidate struct {
	ID                ulid.ULID
	HasLiveProvenance bool
}

// DueForPurge returns the caller's messages that are trashed and whose 30-day
// (configurable) window has closed at `now`, oldest first, capped at limit. The
// scan rides the messages_user_trash partial index (user_id, purge_after) WHERE
// trashed_at IS NOT NULL. Each candidate carries whether it still has live IMAP
// provenance, so the sweep can route it without a second round trip.
//
// It is a pure READ: no lock, no mutation. HasLiveProvenance is a hint that the
// hard-delete path re-checks under a row lock (a concurrent ingest can attach a
// live location between this read and the delete), so a stale hint here is never
// unsafe — only, at worst, a wasted pass.
func (tx *UserTx) DueForPurge(ctx context.Context, now time.Time, limit int) ([]PurgeCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id,
		       EXISTS (SELECT 1 FROM app.message_provenance p
		                WHERE p.user_id = m.user_id AND p.message_id = m.id
		                  AND p.state = 'live')
		  FROM app.messages m
		 WHERE m.user_id = $1
		   AND m.trashed_at IS NOT NULL
		   AND m.purge_after IS NOT NULL
		   AND m.purge_after <= $2
		 ORDER BY m.purge_after
		 LIMIT $3`,
		tx.userID.String(), now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: due for purge: %w", err)
	}
	defer rows.Close()

	var out []PurgeCandidate
	for rows.Next() {
		var (
			id       string
			liveProv bool
		)
		if err := rows.Scan(&id, &liveProv); err != nil {
			return nil, fmt.Errorf("store: scan purge candidate: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse purge candidate id %q: %w", id, err)
		}
		out = append(out, PurgeCandidate{ID: u, HasLiveProvenance: liveProv})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: due for purge: %w", err)
	}
	return out, nil
}

// DeferPurgeDeadline pushes a due message's purge_after out by one full
// retention window and returns the new deadline. The purge worker calls it for
// any message it declined to expunge — whichever of the three ExpungeSkip causes
// applied, not only ingest-only mode (ADR-028 §7).
//
// ⚠ THIS IS NOT A TIDY-UP. WITHOUT IT, LIFTING INGEST-ONLY MODE DESTROYS MAIL
// FROM THE USER'S INBOX.
//
// Trashing stamps purge_after in the same statement as trashed_at
// (jmap_email.go), so the retention clock starts even though the move to Trash
// is only a suppressed ledger entry and never reached the server. Leave it
// alone and, the moment writeback is enabled, every message trashed longer ago
// than the window is instantly due: the EXPUNGE is enqueued 'pending' because
// writeback is now live, and it is applied to EVERY LIVE COPY — which is still
// INBOX, because the move never landed. Up to one batch per sweep, with no
// operator review and without the explicit --apply that ADR-028 §4 promises is
// the only way a shadow-era intent reaches the server. The most destructive op
// class would be the one that escapes, on a timer.
//
// Deferring instead means a shadow-trashed message is never due while shadowed,
// and gets a FULL window after the mode is lifted — room for the operator to
// run the ledger report, --apply the moves so the copies actually reach Trash,
// and only then let retention expire. It also stops a large shadow backlog
// monopolising every DueForPurge batch (which is ORDER BY purge_after LIMIT n)
// and starving a live account on the same user.
//
// ⚠ It knowingly departs from the reaper's rule that "re-stamping purge_after
// would extend a 30-day window the user already started" (reaper.go). That rule
// guards a window that was counting down against a real Trash placement. This
// one has been counting down against a placement that does not exist anywhere
// but locally, and the asymmetry decides it: the cost of extending is that a
// hard delete the user asked for happens later, and the cost of not extending
// is destroying mail from INBOX that the server never saw deleted.
//
// No change_log row: purge_after is internal retention state and is not part of
// any JMAP projection (grep confirms no Email property reads it), so nothing
// JMAP-visible changed and the load-bearing invariant is not engaged.
func (tx *UserTx) DeferPurgeDeadline(ctx context.Context, messageID ulid.ULID) (time.Time, error) {
	until := time.Now().UTC().Add(tx.trashRetention)
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages
		   SET purge_after = $3
		 WHERE user_id = $1 AND id = $2
		   AND trashed_at IS NOT NULL`,
		tx.userID.String(), messageID.String(), until)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: defer purge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Restored or hard-deleted between the due scan and here. Not an error —
		// it simply is not a purge candidate any more.
		return time.Time{}, nil
	}
	return until, nil
}

// ExpungeSkip says WHY no account may destroy its copy of a message, for the
// cases where none may.
//
// ⚠ IT EXISTS BECAUSE ONE COUNTER FOR THREE CAUSES IS NOT A MEASUREMENT. The
// caller used to tally every skip as `ShadowedSkipped`, which asserts a cause —
// ingest-only mode — that is only one of three, and the other two mean something
// an operator would want to act on. On the shadow run the mode's count is
// expected to be large, so a stuck trash move hiding inside it would read as the
// mode working correctly (CLAUDE.md: report per-case counts, never one total).
//
// When several causes apply at once across accounts, the most actionable one
// wins: an unlanded copy in a live account is a thing to fix, a shadowed account
// is a thing that is deliberately true.
type ExpungeSkip string

const (
	// ExpungeSkipNone means at least one account may expunge.
	ExpungeSkipNone ExpungeSkip = ""
	// ExpungeSkipShadowed: no account holding a live copy has writeback on —
	// either its own column or the instance kill switch (ADR-028 §2/§7). Expected
	// and benign during a shadow run; the message waits.
	ExpungeSkipShadowed ExpungeSkip = "shadowed"
	// ExpungeSkipUnlanded: an account with writeback ON holds a live copy OUTSIDE
	// its Trash, so the trash move has not reached the server. NOT benign — it is
	// a suppressed, queued, dead-lettered or never-made move, and a persistent
	// count here means retention has stalled for a reason worth finding.
	ExpungeSkipUnlanded ExpungeSkip = "unlanded"
	// ExpungeSkipVanished: no live copy anywhere, seen only as a race against the
	// due scan. Like the other two causes, purge_after is deferred a full
	// retention window rather than left due (purge.Sweep applies the defer to
	// all three causes unconditionally) — so phase 2 picks it up one window
	// later, not on the next sweep.
	ExpungeSkipVanished ExpungeSkip = "vanished"
	// ExpungeSkipRestored: the message is no longer trashed, or its retention
	// deadline is no longer past — it left the Trash lifecycle after the EXPUNGE
	// was already queued (#555). Produced ONLY by PurgeExpungeStillEligible, the
	// apply-time re-check; ExpungeableAccountIDs cannot return it, so the purge
	// sweep's switch over skip reasons never sees it and needs no new arm.
	ExpungeSkipRestored ExpungeSkip = "restored"
)

// ExpungeableAccountIDs returns the accounts in which phase-1 purge may safely
// EXPUNGE this message: writeback is effectively on, and **every live copy the
// account holds is in that account's Trash**. When it returns none, the
// ExpungeSkip says which of the three reasons applied.
//
// ⚠ THIS IS THE THIRD ANSWER TO ONE QUESTION, AND THE FIRST PHYSICAL ONE. The
// question phase 1 asks is "may I destroy this copy?", and the two previous
// answers were bookkeeping proxies for it:
//
//  1. a deadline (defer purge_after while shadowed) — missed every message
//     trashed DURING the run, because the defer only reaches rows DueForPurge
//     already returned, and that query selects purge_after <= now;
//  2. a ledger state (no suppressed 'move' intent for the account) — missed
//     the multi-account case, because the check lived in the WORKER while the
//     enqueue resolved its own account set in the STORE, with a different
//     predicate. It also could not distinguish the latest trash move from any
//     historical move, treated a dead-lettered move as permission to destroy,
//     and self-cleared at 'pending' rather than 'applied'.
//
// Both were patches on a proxy. The fact that actually licenses an EXPUNGE is
// not in the intent queue at all — it is where the copy physically is. If the
// live provenance says Trash, expunging it empties Trash. If it says INBOX,
// expunging it destroys inbox mail, and that is equally wrong whether the move
// is suppressed, dead-lettered, still queued, or was never made.
//
// So this also fixes a defect that predates ingest-only mode: a trash move that
// dead-letters used to leave the message due, and phase 1 would then expunge it
// from wherever it still sat.
//
// bool_and, not bool_or: re-delivered mail can have several live copies in ONE
// account (#206) and the engine applies an intent to every one of them, so a
// single copy still in INBOX disqualifies the account.
//
// ⚠ COALESCE IS LOAD-BEARING, AND ITS ABSENCE WAS A FOURTH WRONG ANSWER. The
// paragraph above states the rule correctly; the SQL below did not implement it.
// app.mailboxes.special_use is NULLABLE and an ordinary folder carries NULL
// (INBOX included — store/drafts.go has to match inbox by NAME for exactly this
// reason), so `m.special_use = '\Trash'` yields NULL for such a copy, and
// bool_and SKIPS NULL inputs like every other SQL aggregate. MEASURED on
// PostgreSQL 18 rather than reasoned about, the whole defect being one
// aggregate's NULL rule:
//
//	bool_and over {true, NULL} -> true   <- the hole: one Trash copy carried it
//	bool_and over {NULL}       -> NULL   <- dropped by HAVING: right, by luck
//	bool_and over {COALESCE…}  -> false  <- stated rather than inferred
//
// So an account holding one copy in Trash and one in INBOX passed, and the
// engine destroyed both — the identical destruction answer 2 was rejected for,
// reached through NULL instead of through two disagreeing predicates. Pinned by
// TestACopyOutsideTrashInTheSameAccountBlocksTheExpunge, with
// TestEveryCopyInTrashStillExpunges as the control that a fix keying on copy
// COUNT rather than copy LOCATION would break.
//
// An account with no \Trash mailbox can never satisfy this. That is correct
// rather than a gap — its trash MOVE already dead-letters for want of a
// destination — and it reports as ExpungeSkipUnlanded, which is exactly what it
// is: a trash move that never reached the server.
func (tx *UserTx) ExpungeableAccountIDs(ctx context.Context, messageID ulid.ULID) ([]ulid.ULID, ExpungeSkip, error) {
	if tx.writebackDisabled {
		return nil, ExpungeSkipShadowed, nil
	}
	// ONE QUERY, ONE EVALUATION — the decision and its explanation come from the
	// SAME rows. Classifying in a second query would recreate this function's
	// original defect in miniature: two predicates over state that can differ
	// between them, disagreeing about one message.
	//
	// So `a.writeback_enabled` moved out of the WHERE and into the SELECT: a
	// shadowed account's rows must still be VISIBLE here, or "no account may
	// expunge" cannot be told apart from "no account holds a copy".
	rows, err := tx.tx.Query(ctx, `
		SELECT p.account_id,
		       -- The Gmail writeback hold counts as writeback off here too
		       -- (#911 review): purge never promotes an expunge for a held
		       -- Gmail account, whatever its column says.
		       (a.writeback_enabled AND NOT `+gmailWritebackHeldSQL+`) AS writeback_enabled,
		       bool_and(COALESCE(m.special_use, '') = '\Trash') AS all_in_trash
		  FROM app.message_provenance p
		  JOIN app.accounts  a ON a.id = p.account_id  AND a.user_id = p.user_id
		  JOIN app.mailboxes m ON m.id = p.mailbox_id  AND m.user_id = p.user_id
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'
		 GROUP BY p.account_id, a.id
		 ORDER BY p.account_id`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return nil, ExpungeSkipNone, fmt.Errorf("store: expungeable account ids: %w", err)
	}
	defer rows.Close()
	var (
		out       []ulid.ULID
		anyCopy   bool // any live copy at all, in any account
		anyLiveWB bool // ...held by an account whose writeback is on
	)
	for rows.Next() {
		var (
			id         string
			writeback  bool
			allInTrash bool
		)
		if err := rows.Scan(&id, &writeback, &allInTrash); err != nil {
			return nil, ExpungeSkipNone, fmt.Errorf("store: scan account id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, ExpungeSkipNone, fmt.Errorf("store: parse account id %q: %w", id, err)
		}
		anyCopy = true
		if !writeback {
			continue
		}
		anyLiveWB = true
		if allInTrash {
			out = append(out, u)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, ExpungeSkipNone, fmt.Errorf("store: expungeable account ids: %w", err)
	}
	switch {
	case len(out) > 0:
		return out, ExpungeSkipNone, nil
	case !anyCopy:
		// DueForPurge's hint and this query share the `state = 'live'` predicate
		// and neither JOIN can drop a row (both FKs are ON DELETE CASCADE), so
		// this is reachable only as a race: the last live copy went away between
		// the due scan's transaction and this one. Counted rather than acted on —
		// purge.Sweep defers purge_after a full retention window like the other
		// two causes, so phase 2 sees no live provenance and hard-deletes one
		// window later, not on the next sweep.
		return nil, ExpungeSkipVanished, nil
	case !anyLiveWB:
		return nil, ExpungeSkipShadowed, nil
	default:
		return nil, ExpungeSkipUnlanded, nil
	}
}

// EnqueuePurgeExpungeIntents enqueues the phase-1 EXPUNGE for every live
// account that may actually be written to.
//
// The intents are MESSAGE-ID-BOUND (in.MessageID set), because the message row
// still exists at this point: the engine resolves every live copy the message
// has in that account (MessageProvenanceTargetsForAccount) and does conflict
// detection against the UIDVALIDITY before firing, which is safer than a header
// SEARCH. One intent therefore expunges N copies, not one. Contrast
// RetireDraft, which deletes the row in the SAME transaction and therefore must
// name its target by header to survive the ON DELETE CASCADE — here we never
// delete the row while an expunge intent references it (that is a LATER pass,
// once provenance is gone), so the cascade never bites.
//
// The idempotency key is deterministic per (message, account), so re-enqueuing
// every sweep while the expunge is still pending or inflight is a no-op
// (EnqueueIntent's ON CONFLICT), never a storm of duplicate expunges.
//
// ⚠ IT TAKES THE ACCOUNT SET RATHER THAN RESOLVING ITS OWN, and that signature
// is the fix for a whole class of bug rather than a style preference.
//
// It used to call its own resolver while the purge worker applied a DIFFERENT
// safety predicate before deciding to call it. Two predicates for one question
// is how the ingest-only guard came to be applied in the worker and not at the
// enqueue: the worker correctly skipped an account whose trash move had not
// landed, then this function happily enqueued for it anyway because any OTHER
// account had made the message a candidate. Passing the set in means there is
// exactly one definition of "may I expunge here" (ExpungeableAccountIDs) and
// this function cannot disagree with it.
//
// ⚠ Revive IS LOAD-BEARING (#296), and it is the second half of a fix whose
// first half lives in the engine. Both halves are needed, for different reasons:
//
//   - The WIDENING (MessageProvenanceTargetsForAccount) is what makes one pass
//     enough. One intent per ACCOUNT used to expunge exactly one COPY, so a
//     message with three copies in one account needed three successful sweeps to
//     clear — and could never get them, because of the second point.
//   - Revive is what makes a LATER pass possible at all. Without it,
//     EnqueueIntent's ON CONFLICT arm is `WHERE $8::boolean AND …` — constant
//     false — so a terminal row (applied or dead) holding this key swallows the
//     re-enqueue, returns the old id, and the sweep counts an ExpungeEnqueued
//     that will never do anything. The message the user destroyed keeps its
//     server copies forever, HardDeleteMessage keeps (correctly) refusing while
//     live provenance remains, the Trash entry never clears, and nothing
//     dead-letters. Belt and braces: with the widening the normal case is one
//     pass, but a partial failure — one copy's mailbox unavailable, say — must
//     still be retryable by the next sweep rather than sealed shut.
//
// Reviving from 'dead' does mean a dead-lettered purge expunge is re-armed on
// the next sweep instead of staying dead until an operator intervenes. That is
// deliberate: the message is still sitting in the user's Trash asking to be
// destroyed, an hourly retry is the behaviour they would expect, and the
// alternative failure mode — never trying again — is the bug this comment is
// about. Each failed re-attempt re-enters 'dead' with its reason, so the
// account-health surface still sees it.
//
// Enqueueing for an account that must not be written also is not inert: the
// row lands `suppressed`, the sweep re-enqueues with Revive every pass, and
// EnqueueIntent's second ON CONFLICT arm promotes it to pending the moment
// writeback is live again — drainable with no --apply and no review, which is
// exactly what ADR-028 §4 exists to prevent.
func (tx *UserTx) EnqueuePurgeExpungeIntents(ctx context.Context, messageID ulid.ULID, accounts []ulid.ULID) (int, error) {
	mid := messageID
	n := 0
	for _, acct := range accounts {
		key := fmt.Sprintf("purge-expunge:%s:%s", messageID, acct)
		if _, err := tx.EnqueueIntent(ctx, IntentRow{
			MessageID:      &mid,
			AccountID:      acct,
			Op:             IntentExpunge,
			Payload:        []byte("{}"),
			IdempotencyKey: key,
			Revive:         true,
			// ⚠ #555 DEPENDS ON THIS BEING SET. It is what lets the engine
			// re-check "still trashed, still due" before destroying a copy,
			// without that check also firing on a draft-retirement EXPUNGE,
			// which is the same op with the same shape and no relation to
			// Trash. An unattributed row here reads as unknown and the engine
			// declines to treat it as a purge — safe, but it means the guard
			// silently stops guarding, so this field is not optional.
			Source: IntentByPurge,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// HardDeleteMessage performs the terminal, destructive step of the deletion
// lifecycle for one message, in this UserTx (ADR-009, data-model §6):
//
//   - locks the message FOR UPDATE and RE-VERIFIES it is still eligible — trashed,
//     past purge_after at `now`, and (the load-bearing recheck) has NO live
//     provenance. A message that gained a live location since DueForPurge, or that
//     a Restore un-trashed, is NOT deleted: ok is false and nothing changes;
//   - deletes the app.messages row, which CASCADES provenance, labels, triage
//     scores, feedback events, attachments and any writeback intents (schema FKs);
//   - reclaims the content-addressed blob METADATA row — but ONLY when nothing
//     else references those bytes (blobreclaim.go). reclaim reports whether
//     the caller must now delete the blob FILE;
//   - appends the Email `destroyed` change_log row (the invariant), which is
//     what drives the client's /changes cursor and the search indexer's delete,
//     AND the Thread `updated` row for the conversation it just left (#299);
//   - writes the audit tombstone {action: message.purge, object_id,
//     content_hash_prefix} — the only trace ADR-009 lets survive.
//
// BLOB RECLAIM SAFETY (the dangerous step). The guard itself — no message, no
// attachment, no model artifact — lives in blobreclaim.go with the reasoning for
// each referent. What is specific to THIS caller is the concurrency proof:
// having just deleted the one possible message row above, the message guard
// holds unless a concurrent ingest inserted a new one — which it cannot, because
// that ingest is serialized behind the FK key-share lock our FOR UPDATE / row
// delete holds until this transaction commits (ingest of byte-identical mail
// dedups onto THIS row and blocks; after we commit it sees the row gone, aborts
// on the dangling FK, and retries as a fresh ingest that re-Puts the bytes). The
// caller deletes the blob FILE inside this same transaction, before commit — so
// the not-found window a re-ingest needs opens only AFTER the file is already
// gone, never during. See internal/purge.
//
// ⚠ THE EmailSubmission CASCADE IS A DOCUMENTED DEFERRAL, NOT AN OVERSIGHT
// (#299). app.submissions.message_id is ON DELETE CASCADE (migration 00014,
// mailroom#148), so purging a message that was sent through Mailroom destroys
// its EmailSubmission with it — a JMAP-visible object, with no `destroyed` row
// of its own. It is deferred rather than emitted because an EmailSubmission is
// meaningless without the Email it sent: a client is being told in the very same
// transaction that the Email is gone, and every surface that renders a
// submission renders it against that Email. What a second client observes is
// therefore bounded and self-correcting — a submission id in its cache that
// resolves to nothing, until the Email `destroyed` it received alongside makes
// it drop the row anyway. Emitting the pair would be more correct in the
// abstract; it is not emitted because the message being purged has sat in Trash
// for thirty days and any submission on it is at least that old, so no client is
// still rendering it as live outbound state.
func (tx *UserTx) HardDeleteMessage(ctx context.Context, messageID ulid.ULID, now time.Time) (contentHash []byte, reclaim bool, ok bool, err error) {
	var (
		trashed    bool
		purgeAfter *time.Time
		hash       []byte
		liveProv   bool
		threadID   *string
	)
	err = tx.tx.QueryRow(ctx, `
		SELECT m.trashed_at IS NOT NULL, m.purge_after, m.content_hash,
		       EXISTS (SELECT 1 FROM app.message_provenance p
		                WHERE p.user_id = m.user_id AND p.message_id = m.id
		                  AND p.state = 'live'),
		       m.thread_id
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id = $2
		 FOR UPDATE`,
		tx.userID.String(), messageID.String()).
		Scan(&trashed, &purgeAfter, &hash, &liveProv, &threadID)
	if err != nil {
		if isNoRows(err) {
			// Already gone (a concurrent purge, or account removal). Nothing to do.
			return nil, false, false, nil
		}
		return nil, false, false, fmt.Errorf("store: hard delete message (read): %w", err)
	}

	// FAIL CLOSED on any ambiguity. Every one of these means "this message is not,
	// right now, a message we may destroy", and an unjustifiable delete does
	// nothing rather than guessing — the same discipline as the reaper.
	switch {
	case !trashed || purgeAfter == nil:
		return nil, false, false, nil // restored, or never trashed
	case purgeAfter.After(now):
		return nil, false, false, nil // window has not closed yet
	case liveProv:
		// Still has copies on a server we sync. This is a phase-1 message
		// (expunge-first); hard-deleting it now would drop the record while a copy
		// survives. Refuse — the caller enqueues the expunge instead.
		return nil, false, false, nil
	}

	if _, err := tx.tx.Exec(ctx,
		`DELETE FROM app.messages WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), messageID.String()); err != nil {
		return nil, false, false, fmt.Errorf("store: hard delete message: %w", err)
	}

	// Reclaim the blob metadata row ONLY when nothing references the bytes — the
	// one shared guard (blobreclaim.go). The message guard is belt-and-suspenders
	// (we just deleted the unique row and the FK would block the delete anyway);
	// the attachment and model-artifact guards are the ones that actually matter,
	// because both share a blob with no FK to enforce it.
	reclaimedHashes, rerr := tx.reclaimUnreferencedBlobs(ctx, [][]byte{hash})
	if rerr != nil {
		return nil, false, false, rerr
	}
	reclaim = len(reclaimedHashes) > 0

	// The invariant: exactly one Email `destroyed` row, in this transaction. It is
	// what tells /changes clients and the search indexer the message is gone.
	if _, err := tx.AppendChange(ctx, ObjectEmail, messageID, OpDestroyed); err != nil {
		return nil, false, false, err
	}

	// ⚠ AND THE THREAD IT LEFT (#299). A Thread's JMAP identity is its list of
	// emailIds, so removing a message from one MUTATES a JMAP-visible object —
	// CollapseDuplicateGroup already emits Thread rows for exactly this reason and
	// this path did not, so a client holding the conversation kept rendering a
	// message that no longer exists and Thread/changes never said otherwise.
	//
	// `updated`, never `destroyed`, even when that was the thread's last message:
	// purge does not delete app.threads rows, so claiming the Thread was destroyed
	// would be a lie a client could check. An emptied thread survives as an empty
	// thread — pre-existing behaviour, deliberately not widened here (a purge that
	// started deleting threads would be a new delete, and mail safety says no).
	//
	// thread_id is nullable (ON DELETE SET NULL from app.threads), so a message
	// with no thread simply owes no row.
	if threadID != nil {
		tid, perr := ulid.Parse(*threadID)
		if perr != nil {
			return nil, false, false, fmt.Errorf("store: hard delete message: parse thread id %q: %w", *threadID, perr)
		}
		if _, err := tx.AppendChange(ctx, ObjectThread, tid, OpUpdated); err != nil {
			return nil, false, false, err
		}
	}

	// The tombstone — the only trace ADR-009 lets survive.
	if err := tx.writePurgeTombstone(ctx, messageID, hash); err != nil {
		return nil, false, false, err
	}

	return hash, reclaim, true, nil
}

// writePurgeTombstone appends the ADR-009 audit tombstone for a purged message:
// {action, object_id, content_hash_prefix} and NOTHING ELSE. The actor is the
// system (the purge job), and the detail carries only a short hash prefix — never
// the subject, the sender, or the bytes.
func (tx *UserTx) writePurgeTombstone(ctx context.Context, messageID ulid.ULID, contentHash []byte) error {
	prefix := contentHash
	if len(prefix) > tombstonePrefixBytes {
		prefix = prefix[:tombstonePrefixBytes]
	}
	detail, err := json.Marshal(map[string]string{
		"content_hash_prefix": hex.EncodeToString(prefix),
	})
	if err != nil {
		return fmt.Errorf("store: marshal tombstone detail: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'system', $2, 'Email', $3, $4)`,
		tx.userID.String(), PurgeAction, messageID.String(), detail); err != nil {
		return fmt.Errorf("store: write purge tombstone: %w", err)
	}
	return nil
}

// PurgeExpungeStillEligible re-asks, at APPLY time, the question
// ExpungeableAccountIDs answered at ENQUEUE time: may this account still
// destroy its copies of this message?
//
// ⚠ THIS EXISTS BECAUSE A SAFETY DECISION WAS BEING MADE ONCE AND THEN ASSUMED
// (#555). Phase 1 enqueues an expunge when every live copy in an account sits
// in that account's Trash and the retention deadline has passed. Both facts can
// stop being true before the drain arrives — the user restores the message, or
// another client moves it out of Trash and Mailroom learns at the next sync —
// and nothing re-checked. The engine then issued STORE \Deleted + UID EXPUNGE
// against a copy the user had just asked to keep, and the only server copy was
// destroyed. Reproduced, not theorised; see the probe in internal/writeback.
//
// It deliberately calls ExpungeableAccountIDs rather than re-implementing the
// predicate. A second copy of "every live copy is in Trash" would be a second
// thing to get wrong, and this one has already been wrong once in a way that
// destroyed mail: `bool_and(col = 'x')` over a NULLABLE column skips NULLs, so
// "every copy in Trash" silently meant "some copy" until #299. One predicate,
// asked twice.
func (tx *UserTx) PurgeExpungeStillEligible(ctx context.Context, messageID, accountID ulid.ULID) (bool, ExpungeSkip, error) {
	// ⚠ THE LOGICAL STATE FIRST, AND IT IS NOT WHAT ExpungeableAccountIDs
	// ANSWERS. That query is about PHYSICAL location — is every live copy
	// sitting in a \Trash mailbox — plus the writeback switch. It says nothing
	// about whether the USER still wants the message destroyed.
	//
	// Those two come apart in exactly the case this function exists for. A
	// restore clears trashed_at/purge_after immediately, but the copy stays
	// physically in Trash until the restore's MOVE is written back, so
	// ExpungeableAccountIDs still answers "eligible" for a message the user just
	// rescued. Asking only that question is a re-check that cannot see the thing
	// it was added to catch: MEASURED, by writing the test first — the engine
	// destroyed the restored copy with Withdrawn = 0, the guard never firing.
	//
	// This is the "still trashed, still due" half, and it is the same predicate
	// DueForPurge uses to decide a message is a candidate at all.
	var due bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT m.trashed_at IS NOT NULL
		   AND m.purge_after IS NOT NULL
		   AND m.purge_after <= now()
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id = $2`,
		tx.userID.String(), messageID.String()).Scan(&due); err != nil {
		if isNoRows(err) {
			// The message row is gone entirely (hard-deleted, or collapsed onto
			// a duplicate keeper). Nothing to destroy on its behalf.
			return false, ExpungeSkipVanished, nil
		}
		return false, "", fmt.Errorf("store: re-check purge due state: %w", err)
	}
	if !due {
		return false, ExpungeSkipRestored, nil
	}

	accounts, skip, err := tx.ExpungeableAccountIDs(ctx, messageID)
	if err != nil {
		return false, skip, err
	}
	for _, a := range accounts {
		if a == accountID {
			return true, ExpungeSkipNone, nil
		}
	}
	// The message may still be expungeable in some OTHER account while this one
	// has gone stale (a copy restored in one account only), so the skip reason
	// can be ExpungeSkipNone here. That is not a contradiction: the reason
	// describes the message, the bool describes this account.
	return false, skip, nil
}

// WithdrawPurgeExpunges removes not-yet-applied purge EXPUNGE intents for a
// message and reports how many it removed, so a caller can log a number rather
// than assert an effect it did not measure.
//
// ⚠ SCOPED BY source = 'purge' (migration 00057), AND THAT IS LOAD-BEARING. The
// draft machinery also enqueues EXPUNGE intents, for a completely unrelated
// reason — retiring a superseded revision from the IMAP Drafts folder — and
// those must survive untouched. Before there was a source column the two were
// indistinguishable, which is why #555's durable fix was blocked on #554 item 3
// rather than being a one-line DELETE.
//
// ⚠ NULL source IS NOT MATCHED, deliberately. A pre-00057 row is 'unknown', not
// 'purge', and sweeping unknown rows into a DELETE would make this quietly
// destroy draft-retirement intents from before the migration. Unknown means
// unknown; the terminal-state audit in 00057's note records that there are no
// such rows awaiting application anyway.
//
// 'inflight' is excluded for the same reason cancelPendingAppend excludes it:
// the op has (or may have) already reached the server, and withdrawing the
// record of something that already happened does not un-happen it.
func (tx *UserTx) WithdrawPurgeExpunges(ctx context.Context, messageID ulid.ULID) (int, error) {
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.writeback_intents
		 WHERE user_id = $1 AND message_id = $2
		   AND op = 'expunge' AND source = 'purge'
		   AND state IN ('pending', 'suppressed')`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return 0, fmt.Errorf("store: withdraw purge expunges: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
