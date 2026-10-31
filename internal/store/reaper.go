// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file holds the store surface for EXPUNGE/VANISHED reconciliation — the
// reaper (spec §4, mailroom#86). Sync is CONDSTORE-only (QRESYNC is not drivable
// through go-imap v2 beta.8 — internal/sync/NOTES.md), so nothing ever told
// Mailroom that a message had been expunged or moved away on the server:
// app.message_provenance only ever grew, and a message deleted on another client
// never left its Mailroom folder. The reaper closes that hole by asking the
// server, periodically, for a mailbox's whole live UID set and dropping the
// provenance rows the server no longer backs.
//
// CHANGE-LOG DISCIPLINE (the load-bearing invariant — CLAUDE.md). Provenance is
// not itself a JMAP-visible object, but a message's mailboxIds are DERIVED from
// it (jmap_email.go effectiveMembershipSQL), so dropping a provenance row can
// change a JMAP-visible property. ReapMailbox therefore appends exactly one
// Email change_log row, in the same transaction as the delete, for every message
// whose EFFECTIVE membership actually moved — and none for a message whose
// membership is unchanged (see reapMessage). That one stream is how both the
// client's /changes cursor and the search indexer learn a message left a folder.
//
// FAILING CLOSED. Everything in here is driven by a ReapObservation, which is a
// statement about what the server ACTUALLY SAID. There is no code path that
// reaps on the absence of information: a failed SELECT, a failed SEARCH, a
// timeout, an auth failure, or a UID set that cannot be trusted never reaches
// this file — the caller (internal/mailsync) refuses to build an observation at
// all. "The server told us this UID is gone" and "we failed to ask" are not the
// same sentence, and only the first one deletes anything.

// ReapObservation is one mailbox's live UID set as the SERVER reported it, plus
// the bounds that make it safe to diff against provenance. It is deliberately a
// record of an OBSERVATION, not a request: every field must come from a server
// response the caller actually received.
type ReapObservation struct {
	// MailboxID and UIDValidity name the coordinate space the observation was
	// made in. Provenance rows outside it (another mailbox, another generation,
	// or already retired to 'stale_uidvalidity') are never touched: a UID means
	// nothing outside the generation that issued it.
	MailboxID   ulid.ULID
	UIDValidity int64

	// ObservedUIDs is the COMPLETE set of UIDs the server reported present
	// (UID SEARCH ALL). Empty is a legitimate answer — an empty mailbox — but
	// only in combination with a UIDBound the caller could actually justify.
	ObservedUIDs []int64

	// UIDBound is the EXCLUSIVE upper bound of the UID space this observation
	// covers: the mailbox's UIDNEXT at the moment the server answered. It is what
	// makes the reap race-free against concurrent ingest. A message that lands in
	// the mailbox after the search gets a UID at or above UIDNEXT, so it is
	// outside the observation, so its provenance row — even one written between
	// the search and this transaction — cannot be concluded absent and is left
	// alone. Only UIDs strictly below the bound are reapable.
	//
	// A zero (or negative) bound means the caller could not establish what the
	// observation covered; ReapMailbox then reaps nothing.
	UIDBound int64
}

// ReapResult reports one mailbox's reconciliation.
type ReapResult struct {
	// ProvenanceDropped is the number of physical locations the server no longer
	// has, and which were therefore deleted.
	ProvenanceDropped int
	// MembershipChanged is the number of messages whose JMAP mailboxIds actually
	// moved as a result — exactly the number of Email change_log rows appended.
	MembershipChanged int
	// MessagesVanished is the number of messages that were left in no location at
	// all and were soft-deleted (trashed_at + purge_after + vanished_at, ADR-009).
	// It is a subset of MembershipChanged.
	MessagesVanished int
}

// ReapMailbox diffs a mailbox's provenance against what the server actually
// holds and reconciles the difference, in this transaction:
//
//  1. Every live provenance row in (MailboxID, UIDValidity) whose UID is below
//     UIDBound and is NOT in ObservedUIDs is DELETED — the server says that
//     physical copy is gone (expunged there, or moved away there).
//  2. For each affected message, its effective JMAP membership is recomputed. If
//     it changed, exactly one Email change_log row is appended (the invariant).
//  3. A message left with no live provenance anywhere, no canonical placement and
//     no local placement is gone from everywhere we sync, and is SOFT-deleted per
//     ADR-009 — trashed_at + purge_after, plus vanished_at so the inference stays
//     retractable (see RetractVanished). Never a hard delete.
//
// The whole thing is one transaction: a message either loses its location and
// gets its change_log row, or neither happens. There is no SAVEPOINT here on
// purpose — there is no "recoverable per-object failure" to isolate. Any error
// below is infrastructure, and the right response is to abandon the entire
// mailbox's reap rather than to persist half a reconciliation. Per-mailbox
// isolation lives one level up, where each mailbox is reaped in its own UserTx.
func (tx *UserTx) ReapMailbox(ctx context.Context, obs ReapObservation) (ReapResult, error) {
	var res ReapResult

	// Fail closed. Each of these means "we cannot justify what this observation
	// covers", and an unjustifiable observation reaps nothing at all.
	if obs.MailboxID == (ulid.ULID{}) {
		return res, fmt.Errorf("store: reap mailbox: mailbox id is required")
	}
	if obs.UIDValidity <= 0 {
		return res, fmt.Errorf("store: reap mailbox %s: refusing to reap against UIDVALIDITY %d",
			obs.MailboxID, obs.UIDValidity)
	}
	if obs.UIDBound <= 1 {
		// Nothing below the bound can exist (UIDs start at 1), so there is nothing
		// this observation could possibly prove absent. Not an error — just a no-op.
		return res, nil
	}

	observed := obs.ObservedUIDs
	if observed == nil {
		observed = []int64{}
	}

	// The anti-join is expressed against unnest() rather than `uid <> ALL($5)` so
	// Postgres can hash the observed set once instead of scanning it per row — the
	// difference between O(n) and O(n·m) on a six-figure mailbox.
	rows, err := tx.tx.Query(ctx, `
		DELETE FROM app.message_provenance p
		 WHERE p.user_id = $1
		   AND p.mailbox_id = $2
		   AND p.uidvalidity = $3
		   AND p.state = 'live'
		   AND p.uid < $4
		   AND NOT EXISTS (
		       SELECT 1 FROM unnest($5::bigint[]) AS observed(uid)
		        WHERE observed.uid = p.uid)
		RETURNING p.message_id`,
		tx.userID.String(), obs.MailboxID.String(), obs.UIDValidity, obs.UIDBound, observed)
	if err != nil {
		return ReapResult{}, fmt.Errorf("store: reap mailbox %s: %w", obs.MailboxID, err)
	}
	affected := map[ulid.ULID]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ReapResult{}, fmt.Errorf("store: reap mailbox: scan message id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			rows.Close()
			return ReapResult{}, fmt.Errorf("store: reap mailbox: parse message id %q: %w", id, err)
		}
		res.ProvenanceDropped++
		affected[u] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ReapResult{}, fmt.Errorf("store: reap mailbox %s: %w", obs.MailboxID, err)
	}

	for msgID := range affected {
		changed, vanished, err := tx.reapMessage(ctx, msgID, obs.MailboxID)
		if err != nil {
			return ReapResult{}, err
		}
		if changed {
			res.MembershipChanged++
		}
		if vanished {
			res.MessagesVanished++
		}
	}
	return res, nil
}

// reapMessage reconciles one message whose provenance in mailboxID was just
// dropped: it decides whether the message's JMAP membership actually moved, soft-
// deletes it if it is now nowhere at all, and appends the ONE Email change_log
// row that covers both (the invariant).
//
// It is the single definition of "what does it mean for a message to lose a
// physical location", shared by BOTH ways that can happen: the expunge reaper
// (ReapMailbox — the server no longer lists the UID) and the UIDVALIDITY roll
// (ReconcileRetiredGeneration — the generation that issued the UID is gone and
// the re-fetch did not put the message back). Two callers, one set of rules, so
// the two cannot drift into disagreeing about when mail is allowed to disappear.
//
// The membership rules mirror effectiveMembershipSQL exactly, which is why they
// look asymmetric:
//
//   - A message with a CANONICAL PLACEMENT (moved_mailbox_id) reads its
//     membership from that column alone. Provenance is invisible to it. Deleting
//     a provenance row therefore changes NOTHING a client can see: no change_log
//     row, and — crucially — no soft-delete either. The user moved this message;
//     the reaper's "it is nowhere" inference is simply not true of it, and the
//     placement is what keeps it visible while its writeback move lands (or, if
//     the destination folder is not synced, forever).
//   - Otherwise membership is the union of local_mailbox_id (a draft we have not
//     APPENDed) and the live provenance mailboxes. It moved iff this mailbox is
//     no longer among them — which needs a re-check, because a message can have
//     two copies in one folder (two UIDs, same content hash) and lose only one.
func (tx *UserTx) reapMessage(ctx context.Context, msgID, mailboxID ulid.ULID) (changed, vanished bool, err error) {
	var (
		hasPlacement bool
		hasLocal     bool
		trashed      bool
		stillHere    bool // any live provenance left in the mailbox we reaped
		liveAnywhere bool // any live provenance left at all
	)

	// Lock the message row FOR UPDATE before deciding anything about it, exactly
	// as ApplyEmailUpdate does. Both are deciding "where does this message live",
	// and without the lock they can interleave: the reaper reads moved_mailbox_id
	// as NULL, an Email/set archives the message and commits, and the reaper then
	// soft-deletes a message the user just filed. Taking the same lock the JMAP
	// write path takes makes the two serialize, and the provider wins — which is
	// this project's stated conflict policy (internal/writeback doc) and is right
	// here: the reaper only gets this far because the server said the bytes are
	// gone.
	//
	// The lock order is safe against every other writer of these two tables. The
	// reaper takes provenance row locks (its DELETE) and then this message lock;
	// ingest takes them in the same order (UpsertProvenance, then
	// UpdateMessageFlagsIfChanged); ApplyEmailUpdate takes only the message lock
	// and never locks provenance rows. No cycle exists.
	if err := tx.tx.QueryRow(ctx, `
		SELECT moved_mailbox_id IS NOT NULL,
		       local_mailbox_id IS NOT NULL,
		       trashed_at IS NOT NULL
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), msgID.String(),
	).Scan(&hasPlacement, &hasLocal, &trashed); err != nil {
		if isNoRows(err) {
			// The message row is gone (a concurrent purge). Its provenance went with
			// it via ON DELETE CASCADE; nothing to reconcile.
			return false, false, nil
		}
		return false, false, fmt.Errorf("store: reap message %s: %w", msgID, err)
	}

	// What survives, read AFTER the lock (and therefore after our own DELETE and
	// after any concurrent ingest that has committed).
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.message_provenance
		                WHERE user_id = $1 AND message_id = $2
		                  AND state = 'live' AND mailbox_id = $3),
		       EXISTS (SELECT 1 FROM app.message_provenance
		                WHERE user_id = $1 AND message_id = $2
		                  AND state = 'live')`,
		tx.userID.String(), msgID.String(), mailboxID.String(),
	).Scan(&stillHere, &liveAnywhere); err != nil {
		return false, false, fmt.Errorf("store: reap message %s (survivors): %w", msgID, err)
	}

	// A GMAIL LABEL-VIEW PLACEMENT DIES WITH THE GMAIL COPY (#52 review H3).
	// ApplyGmailObservation places a Gmail message in its account's INBOX, Sent
	// or Drafts row — label views where no provenance ever lives, so
	// RetireCanonicalPlacements can never retire it and the early return below
	// would keep it forever. When the account's last live copy is gone (a sent
	// or discarded draft, a delete-forever, a Trash purge), the placement Gmail
	// implied is gone too: it is cleared here, and the message is then judged
	// exactly as any other — membership elsewhere, or vanished.
	if hasPlacement {
		cleared, err := tx.clearGmailLabelViewPlacement(ctx, msgID, mailboxID)
		if err != nil {
			return false, false, err
		}
		hasPlacement = !cleared
	}

	// The canonical placement overrides provenance for membership, so nothing a
	// client can observe changed. Say nothing, delete nothing.
	if hasPlacement {
		return false, false, nil
	}

	// Another copy of this message is still in the same folder: membership is
	// unchanged. (Rare — it needs two UIDs with the same content hash in one
	// mailbox — but a change_log row here would be a lie.)
	if stillHere {
		return false, false, nil
	}

	// GONE FROM EVERYWHERE. No live provenance, no canonical placement, no local
	// placement: there is no folder, anywhere we sync, that holds this message.
	// Soft-delete it (ADR-009 — the purge job is the only thing that ever hard-
	// deletes), and record vanished_at so the inference can be WITHDRAWN if the
	// message turns out to have merely been moved somewhere we had not yet looked
	// (see RetractVanished). Already-trashed messages keep their original
	// timestamps: re-stamping purge_after would extend a 30-day window the user
	// already started.
	if !liveAnywhere && !hasLocal && !trashed {
		now := time.Now().UTC()
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.messages
			   SET trashed_at = $3, purge_after = $4, vanished_at = $3
			 WHERE user_id = $1 AND id = $2 AND trashed_at IS NULL`,
			tx.userID.String(), msgID.String(), now, now.Add(tx.trashRetention)); err != nil {
			return false, false, fmt.Errorf("store: reap message %s (soft-delete): %w", msgID, err)
		}
		vanished = true
	}

	// One Email change_log row, in this transaction, for the membership move (and
	// the soft-delete, when it happened — they are one update from JMAP's point of
	// view). This is the invariant, and it is what drives both the client's
	// /changes cursor and the search index.
	if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
		return false, false, err
	}
	return true, vanished, nil
}

// ---------------------------------------------------------------------------
// The UIDVALIDITY roll (mailroom#88)
// ---------------------------------------------------------------------------
//
// A UIDVALIDITY roll is the OTHER way a message can leave a folder, and until
// #88 it was the way that left no trace at all.
//
// When a mailbox's UIDVALIDITY changes, every UID we hold for it is a coordinate
// in a generation the server has thrown away. MarkMailboxProvenanceStale retires
// those rows to 'stale_uidvalidity' and the engine re-fetches the mailbox from
// UID 1 under the new generation, which attaches a NEW live row to every message
// that is still there (content-hash dedup, ADR D1, lands it on the same canonical
// message).
//
// A message that is NOT still there gets no new row. It is left holding nothing
// but stale rows — and stale rows are invisible: membership derivation
// (jmap_email.go effectiveMembershipSQL) and the flag merge both filter
// state='live'. So the message dropped out of every folder, out of Email/query,
// out of mailboxIds and out of the mailbox counts, while trashed_at stayed NULL
// and NOT ONE change_log row was ever written. No client was told anything. It
// just quietly stopped existing, and the #86 reaper could not fix it either —
// there were no LIVE rows left for it to drop.
//
// ReconcileRetiredGeneration is what closes that. It treats the roll as what it
// actually is — a membership change — and lands it in the same honest state the
// reaper lands an expunge in: exactly one Email change_log row per message whose
// effective membership really moved, and a soft-delete (trashed_at + purge_after
// + vanished_at, ADR-009, retractable) for a message left in no location at all.
// It shares reapMessage with the reaper, so there is ONE implementation of "what
// does it mean for a message to lose a location", not two.
//
// FAILING CLOSED, AND THE ONE QUESTION THAT MATTERS. "The message is gone" and
// "we have not re-fetched it yet" are the same thing in the database and opposite
// things in the world, and getting them confused destroys mail that is sitting
// safely on the server. A roll is FOLLOWED by a re-fetch; during that re-fetch
// every message in the mailbox transiently has nothing but stale rows. So this
// method refuses to conclude anything about a mailbox until the re-fetch is
// PROVED COMPLETE — and the proof is not "our fetch loop returned no error" (a
// truncated FETCH response returns no error), it is a fresh, independent
// statement from the server, corroborated against what we actually stored:
//
//	EVERY UID THE SERVER NOW REPORTS IN THIS MAILBOX, BELOW THE BOUND THE
//	RE-FETCH COVERED, HAS A LIVE PROVENANCE ROW IN THE NEW GENERATION.
//
// If even one does not, the re-fetch did not land everything the server holds,
// so nothing it failed to land can be concluded absent — and this method reaps
// nothing, deletes nothing, and returns an error. The caller then leaves the
// mailbox's cursors unadvanced, so the roll is still pending and the whole thing
// is retried on the next pass. That is the same sentence as #86's: "we failed to
// ask" is not "the server said it's gone".

// GenerationObservation is the proof, made of things the SERVER actually said,
// that a rolled mailbox has been fully re-fetched under its new generation — and
// therefore that the retired generation can be reconciled. Every field must come
// from a server response the caller really received.
type GenerationObservation struct {
	// MailboxID is the mailbox whose UIDVALIDITY rolled.
	MailboxID ulid.ULID

	// UIDValidity is the NEW generation the re-fetch ran under. Live provenance
	// rows are only accepted as proof of presence inside it: a UID means nothing
	// outside the generation that issued it.
	UIDValidity int64

	// ObservedUIDs is the COMPLETE live UID set of the mailbox under UIDValidity
	// (UID SEARCH ALL), read AFTER the re-fetch finished. Empty is a legitimate
	// answer — a mailbox recreated empty, which is exactly the #88 scenario — but
	// only when the caller could corroborate it (see mailsync.generationObservation:
	// an empty UID set from a mailbox whose SELECT just said EXISTS > 0 is refused).
	ObservedUIDs []int64

	// UIDBound is the EXCLUSIVE upper bound of the UID space the RE-FETCH covered:
	// the mailbox's UIDNEXT at the SELECT that drove it. It is what keeps the
	// completeness proof from being defeated by ordinary new mail. A message
	// delivered after that SELECT gets a UID at or above UIDNEXT; it will show up
	// in ObservedUIDs (which is read later) with no provenance row, through no
	// fault of the re-fetch. Bounding the proof by what the re-fetch actually
	// covered excludes it, instead of jamming the reconcile forever on a busy
	// mailbox.
	//
	// It must be at least 1 (UIDs start at 1, so a bound of 1 is the honest
	// "this generation has never held a message" of a freshly recreated mailbox).
	// A zero or negative bound means the caller could not establish what the
	// re-fetch covered, and nothing is reconciled.
	UIDBound int64
}

// GenerationResult reports one mailbox's post-roll reconciliation.
type GenerationResult struct {
	// ProvenanceDropped is the number of retired provenance rows deleted — the
	// coordinates of a generation the server has thrown away.
	ProvenanceDropped int
	// MembershipChanged is the number of messages whose JMAP mailboxIds actually
	// moved as a result — exactly the number of Email change_log rows appended.
	MembershipChanged int
	// MessagesVanished is the number of messages left in no location at all and
	// soft-deleted (trashed_at + purge_after + vanished_at, ADR-009). A subset of
	// MembershipChanged.
	MessagesVanished int
}

// ReconcileRetiredGeneration finishes a UIDVALIDITY roll, in this transaction:
//
//  1. It PROVES the re-fetch complete: every UID the server reports in
//     (MailboxID, UIDValidity) below UIDBound must have a live provenance row. If
//     any does not, the re-fetch was partial and the method returns an error
//     having changed nothing.
//  2. Every remaining 'stale_uidvalidity' row in the mailbox is DELETED. After
//     step 1, every location the server actually holds in this mailbox is a LIVE
//     row; anything still marked stale is therefore, by construction, a location
//     the server does not have. (Retired generations can never be served again
//     while UIDVALIDITY is what it is; and a coordinate in the CURRENT generation
//     that the server still holds was revived to 'live' by UpsertProvenance when
//     the re-fetch re-ingested it.)
//  3. Each affected message is reconciled by reapMessage — the same code the
//     expunge reaper uses. A message still in the mailbox under the new
//     generation has a live row there, so its membership is unchanged and it gets
//     NOTHING: no change_log row, no soft-delete. A message whose membership
//     really moved gets exactly one Email change_log row. A message left in no
//     location anywhere is soft-deleted, retractably.
//
// Deleting the retired rows rather than keeping them as history is deliberate and
// load-bearing in two ways. It makes 'stale_uidvalidity' a genuinely TRANSIENT
// state — "retired, awaiting reconciliation" — so the set of stale rows is the
// outstanding work rather than a graveyard that grows without bound on a mailbox
// that rolls repeatedly. And it is what makes a UIDVALIDITY REVERT (a mailbox
// restored from backup) come back cleanly: with the old rows gone, re-ingesting
// the old coordinates INSERTs fresh live rows, which is a new live location, which
// retracts the soft-delete (RetractVanished) and appends the change_log row.
// Nothing reads stale rows — membership, the flag merge and the writeback target
// query all filter state='live' — so nothing loses information by their going.
//
// There is no SAVEPOINT here, for the same reason ReapMailbox has none: there is
// no recoverable per-object failure to isolate. The completeness proof is checked
// before anything is written, and every error after it is infrastructure, where
// the right answer is to abandon the whole mailbox rather than persist half a
// reconciliation. Per-mailbox isolation lives one level up, in its own UserTx.
func (tx *UserTx) ReconcileRetiredGeneration(ctx context.Context, obs GenerationObservation) (GenerationResult, error) {
	var res GenerationResult

	// Fail closed. Each of these means "we cannot justify what the re-fetch
	// covered", and an unjustifiable re-fetch proves nothing absent.
	if obs.MailboxID == (ulid.ULID{}) {
		return res, fmt.Errorf("store: reconcile retired generation: mailbox id is required")
	}
	if obs.UIDValidity <= 0 {
		return res, fmt.Errorf("store: reconcile retired generation %s: refusing to reconcile against UIDVALIDITY %d",
			obs.MailboxID, obs.UIDValidity)
	}
	if obs.UIDBound < 1 {
		return res, fmt.Errorf(
			"store: reconcile retired generation %s: no UID bound for the re-fetch; refusing to conclude anything is gone",
			obs.MailboxID)
	}

	observed := obs.ObservedUIDs
	if observed == nil {
		observed = []int64{}
	}

	// THE COMPLETENESS PROOF. Count the UIDs the server says are in this mailbox,
	// within the space the re-fetch covered, that we did NOT end up holding a live
	// location for. That number must be zero. Anything else means the re-fetch did
	// not bring back everything the server has — a truncated FETCH, an interrupted
	// pass, a fetch that silently dropped a message — and in that state the
	// absence of a message from our provenance says nothing whatsoever about
	// whether it is on the server.
	var missing int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM unnest($4::bigint[]) AS observed(uid)
		 WHERE observed.uid < $5
		   AND NOT EXISTS (
		       SELECT 1 FROM app.message_provenance p
		        WHERE p.user_id = $1
		          AND p.mailbox_id = $2
		          AND p.uidvalidity = $3
		          AND p.uid = observed.uid
		          AND p.state = 'live')`,
		tx.userID.String(), obs.MailboxID.String(), obs.UIDValidity, observed, obs.UIDBound,
	).Scan(&missing); err != nil {
		return GenerationResult{}, fmt.Errorf("store: reconcile retired generation %s (completeness): %w",
			obs.MailboxID, err)
	}
	if missing > 0 {
		return GenerationResult{}, fmt.Errorf(
			"store: reconcile retired generation %s: the re-fetch is INCOMPLETE — %d UID(s) the server "+
				"reports under UIDVALIDITY %d have no ingested location; refusing to conclude that anything is gone",
			obs.MailboxID, missing, obs.UIDValidity)
	}

	// The re-fetch is proved complete, so every location the server holds here is
	// live. Whatever is still stale is a coordinate the server does not have.
	rows, err := tx.tx.Query(ctx, `
		DELETE FROM app.message_provenance p
		 WHERE p.user_id = $1
		   AND p.mailbox_id = $2
		   AND p.state = 'stale_uidvalidity'
		RETURNING p.message_id`,
		tx.userID.String(), obs.MailboxID.String())
	if err != nil {
		return GenerationResult{}, fmt.Errorf("store: reconcile retired generation %s: %w", obs.MailboxID, err)
	}
	affected := map[ulid.ULID]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return GenerationResult{}, fmt.Errorf("store: reconcile retired generation: scan message id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			rows.Close()
			return GenerationResult{}, fmt.Errorf("store: reconcile retired generation: parse message id %q: %w", id, err)
		}
		res.ProvenanceDropped++
		affected[u] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GenerationResult{}, fmt.Errorf("store: reconcile retired generation %s: %w", obs.MailboxID, err)
	}

	// Same reconciliation as the reaper's, message by message: one change_log row
	// iff effective membership actually moved, a retractable soft-delete iff the
	// message is now nowhere, and NOTHING at all for a message that still has a
	// live location in this mailbox (the survivor of the roll) or a canonical
	// placement, or that the user trashed.
	for msgID := range affected {
		changed, vanished, err := tx.reapMessage(ctx, msgID, obs.MailboxID)
		if err != nil {
			return GenerationResult{}, err
		}
		if changed {
			res.MembershipChanged++
		}
		if vanished {
			res.MessagesVanished++
		}
	}
	return res, nil
}

// RetractVanished withdraws a reaper soft-delete for a message that has turned
// out to be on the server after all, clearing trashed_at, purge_after and
// vanished_at. It reports whether anything was withdrawn.
//
// It is the safety net under the reaper's one unavoidable inference. The reaper
// concludes "no live provenance anywhere ⇒ gone everywhere", and that races with
// another client MOVING a message between folders: we can poll the destination
// before the move happens and reap the source after it, and in that window the
// message really is in no folder we know of. Left alone, it would stay invisible
// forever while sitting untouched on the server. So when ingest attaches a NEW
// live provenance row to a message carrying vanished_at, the message is
// demonstrably back, the inference is retracted, and the message reappears on the
// next pass.
//
// The guard `vanished_at IS NOT NULL` is what makes this safe: a message the USER
// trashed never carries vanished_at, so syncing the IMAP Trash folder — which
// duly creates a live provenance row for it — can never un-trash it. Only the
// reaper's own conclusions are retractable.
//
// The caller must append the Email change_log row for the message in the same
// transaction (ingest already does: a new physical location is an Email update).
func (tx *UserTx) RetractVanished(ctx context.Context, messageID ulid.ULID) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages
		   SET trashed_at = NULL, purge_after = NULL, vanished_at = NULL
		 WHERE user_id = $1 AND id = $2 AND vanished_at IS NOT NULL`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return false, fmt.Errorf("store: retract vanished %s: %w", messageID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// RetireCanonicalPlacements clears app.messages.moved_mailbox_id for every
// message whose placement the reaper has made redundant, and returns how many it
// retired.
//
// WHY THE OVERRIDE HAS TO GO. moved_mailbox_id exists (mailroom#84, migration
// 00006) because a user's move could not be expressed in provenance: the source
// folder's row had to survive for the writeback engine to find the message, and
// with no expunge reconciliation it survived forever after — so an archived
// message bounced back into the inbox on the next poll. But an override that is
// never cleared is itself a bug: it PINS the message to that mailbox for good, so
// a LATER move by another client (phone moves it Archive -> Receipts) can never
// be reflected. Membership would be frozen at whatever Mailroom last did.
//
// WHY IT IS SAFE TO CLEAR — the only question that matters, because clearing it
// one moment too early lets the stale source provenance row resurrect the message
// into the folder the user just emptied it from. The predicate below is the proof,
// not a heuristic:
//
//	the message's LIVE PROVENANCE MAILBOXES are EXACTLY {moved_mailbox_id}
//	  (the destination is confirmed — the bytes are really there — AND no other
//	   location survives, in particular not the source we moved away from),
//	AND its local placement, if any, is that same mailbox.
//
// Under that predicate, effective membership (effectiveMembershipSQL) is
// {moved_mailbox_id} BEFORE the update — read from the override — and
// {moved_mailbox_id} AFTER it — read from provenance. The two are the same set by
// construction. The message therefore cannot flicker: there is no intermediate
// state in which the source folder is derivable, because the source row is gone
// before the guard can hold. This is also exactly why the update appends NO
// change_log row: it is a mutation whose own WHERE clause is a proof that nothing
// JMAP-visible changed (compare UpdateMailboxCursors, which writes sync
// bookkeeping for the same reason).
//
// The reaper is what makes the guard reachable at all — before it existed, the
// source row never went away and the "no other location" clause could never hold.
// A message whose destination folder is not synced (sync_scope='off'), or whose
// writeback move has not landed, simply never satisfies it and keeps its override,
// which is the correct conservative outcome.
func (tx *UserTx) RetireCanonicalPlacements(ctx context.Context) (int64, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages m
		   SET moved_mailbox_id = NULL
		 WHERE m.user_id = $1
		   AND m.moved_mailbox_id IS NOT NULL
		   AND (m.local_mailbox_id IS NULL OR m.local_mailbox_id = m.moved_mailbox_id)
		   AND EXISTS (
		       SELECT 1 FROM app.message_provenance p
		        WHERE p.user_id = m.user_id AND p.message_id = m.id
		          AND p.state = 'live' AND p.mailbox_id = m.moved_mailbox_id)
		   AND NOT EXISTS (
		       SELECT 1 FROM app.message_provenance p
		        WHERE p.user_id = m.user_id AND p.message_id = m.id
		          AND p.state = 'live' AND p.mailbox_id <> m.moved_mailbox_id)`,
		tx.userID.String())
	if err != nil {
		return 0, fmt.Errorf("store: retire canonical placements: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// The gap detector (mailroom#93)
// ---------------------------------------------------------------------------
//
// The reaper and the roll reconciler above both answer "what did we ingest that
// the server no longer has?". #93 is the MIRROR question, and until now nothing
// asked it: "what does the server have that we never ingested?"
//
// The ordinary poll (internal/mailsync syncMailbox) fetches the open-ended UID
// range above the mailbox's last-seen UID and then advances that cursor to the
// highest UID it ingested. If the FETCH silently drops a UID in the MIDDLE of the
// range — a truncated response, a half-read connection, a library bug — the loop
// returns no error, the cursor still advances past the dropped UID, and the
// next poll asks for `cursor+1:*`. The dropped message is now BELOW the cursor.
// It is never asked for again. Not by the poll, and not by the reaper either: the
// reaper only ever DROPS provenance rows, so a message with no row at all is
// invisible to it. The message is on the server, and Mailroom will never learn it
// exists. That is silent, permanent mail loss, and it is the same trap #86 and #88
// each fell into once: "our fetch loop returned no error" is not proof that the
// fetch was complete.
//
// #88 already established the only proof that is: an INDEPENDENT statement from
// the server (UID SEARCH ALL), corroborated against what we actually stored —
// every UID the server reports below the bound we covered must have a live
// provenance row. MissingUIDs is that same proof applied to the ordinary poll, and
// the reaper is already holding the evidence: it fetches the whole live UID set
// every reap pass. This just reads the diff in the OTHER direction.
//
// TWO CONCLUSIONS, RIGOROUSLY SEPARATE. The reaper's set is
// {live rows we hold} \ {UIDs the server reports}: locations to REMOVE.
// The gap detector's set is
// {UIDs the server reports} \ {live rows we hold}: locations to ADD.
// They are disjoint by construction and they mean opposite things. A gap is
// evidence that OUR FETCH was incomplete — never that anything was deleted — so
// nothing in this file deletes, soft-deletes, or retires anything, and the caller
// never feeds a gap into ReapMailbox. Conflating the two is how mail gets
// destroyed.
//
// FAILING OPEN IS THE SAFE DIRECTION HERE, and that is the one asymmetry worth
// naming. MissingUIDs is a pure read whose only consequence is that the caller
// re-FETCHes a UID. Ingest is idempotent (content-hash dedup, ADR D1), so a
// spurious gap costs one wasted round-trip; a MISSED gap costs the user a message.
// The caller still refuses to run this on an untrustworthy observation, but for
// the opposite reason from the reaper's: not because acting on it is dangerous,
// but because a UID set from the wrong generation is not about this mailbox at all.

// GapObservation asks which UIDs the SERVER reports in a mailbox that we hold no
// live provenance row for — the messages our own fetch never landed.
type GapObservation struct {
	// MailboxID and UIDValidity name the coordinate space. A live provenance row
	// only counts as proof we hold a UID inside the generation that issued it.
	MailboxID   ulid.ULID
	UIDValidity int64

	// ObservedUIDs is the COMPLETE set of UIDs the server reported present in the
	// mailbox (UID SEARCH ALL), from the same observation the reaper is bounded by.
	ObservedUIDs []int64

	// UIDBound is the EXCLUSIVE upper bound of the UID space the POLL CLAIMS TO
	// HAVE COVERED: min(the server's UIDNEXT, last_seen_uid + 1). Both halves are
	// load-bearing.
	//
	// Bounding by last_seen_uid+1 is what makes a gap a gap. A UID at or ABOVE the
	// cursor is not lost — the next poll's `cursor+1:*` range will ask for it, so
	// its absence proves nothing and re-fetching it here would be a pointless
	// duplicate of work the poll is about to do. Only a UID the cursor has already
	// moved PAST is unreachable forever, and only those are the #93 bug.
	//
	// Bounding by UIDNEXT keeps mail delivered concurrently with the observation
	// out of it, exactly as ReapObservation.UIDBound does.
	//
	// A bound of 1 or less means the poll has covered nothing yet (a mailbox never
	// successfully polled), so no UID can be below the cursor and there is nothing
	// to detect.
	UIDBound int64

	// Limit caps how many missing UIDs are returned in one pass, so a mailbox that
	// somehow lost a great many messages re-fetches them in bounded batches rather
	// than issuing one enormous FETCH. The caller treats a full batch as "there is
	// more to do" and leaves the mailbox due, so the backfill still converges.
	Limit int
}

// MissingUIDs returns, in ascending order, the UIDs the server reports in the
// mailbox that have NO live provenance row in the observed generation and that lie
// below the bound the poll claims to have covered. Each one is a message the
// server holds and we never ingested — and, being below the last-seen cursor, one
// the poll will never ask for again.
//
// It is READ-ONLY. It deletes nothing, retires nothing and concludes nothing about
// absence; its entire output is a list of UIDs to go and FETCH. There is no
// SAVEPOINT because there is no recoverable per-object failure to isolate: any
// error here is infrastructure, and it aborts the caller's UserTx (which contains
// only this statement — see mailsync.StoreAdapter.MissingUIDs).
//
// THE PLAN. The predicate is an anti-join between the server's UID array and the
// mailbox's live provenance rows. Postgres folds the bound parameters into the
// custom plan, so unnest() is estimated at the array's real length rather than the
// 100-row default, and it hash-anti-joins the two sides: one index range scan of
// provenance_live_uq (mailbox_id, uidvalidity, uid) for the mailbox's rows, one
// hash probe per observed UID. That is O(n + m), NOT one index lookup per observed
// UID — a mailbox with 10^6 messages costs one scan of its own provenance, the
// same order as the reaper's DELETE anti-join immediately after it, and not 10^6
// point lookups. RLS appends `user_id = app.current_user_id()` to the plan, which
// is why the explicit p.user_id predicate leads: it matches the policy exactly and
// keeps the scan on the (mailbox_id, uidvalidity, uid) index rather than degrading
// to a filter.
func (tx *UserTx) MissingUIDs(ctx context.Context, obs GapObservation) ([]int64, error) {
	// Fail closed on anything we cannot justify. Each of these means "we do not
	// know what space the poll covered", and an unjustifiable bound detects nothing
	// rather than guessing.
	if obs.MailboxID == (ulid.ULID{}) {
		return nil, fmt.Errorf("store: missing uids: mailbox id is required")
	}
	if obs.UIDValidity <= 0 {
		return nil, fmt.Errorf("store: missing uids %s: refusing to diff against UIDVALIDITY %d",
			obs.MailboxID, obs.UIDValidity)
	}
	if obs.Limit <= 0 {
		return nil, fmt.Errorf("store: missing uids %s: a batch limit is required", obs.MailboxID)
	}
	if obs.UIDBound <= 1 {
		// UIDs start at 1, so the poll has covered nothing: no UID can be below the
		// cursor, and no gap can exist. Not an error — a no-op.
		return nil, nil
	}

	observed := obs.ObservedUIDs
	if observed == nil {
		observed = []int64{}
	}

	rows, err := tx.tx.Query(ctx, `
		SELECT observed.uid
		  FROM unnest($4::bigint[]) AS observed(uid)
		 WHERE observed.uid < $5
		   AND NOT EXISTS (
		       SELECT 1 FROM app.message_provenance p
		        WHERE p.user_id = $1
		          AND p.mailbox_id = $2
		          AND p.uidvalidity = $3
		          AND p.uid = observed.uid
		          AND p.state = 'live')
		 ORDER BY observed.uid
		 LIMIT $6`,
		tx.userID.String(), obs.MailboxID.String(), obs.UIDValidity,
		observed, obs.UIDBound, obs.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: missing uids %s: %w", obs.MailboxID, err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("store: missing uids %s: scan: %w", obs.MailboxID, err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: missing uids %s: %w", obs.MailboxID, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Folder deletion (mailroom#87)
// ---------------------------------------------------------------------------
//
// The reaper above is the answer to a message LEAVING a folder. #87 is the same
// class of bug ONE LEVEL UP: a whole FOLDER leaving the server. When a mailbox is
// deleted in another IMAP client it stops appearing in LIST, so nothing ever
// selects it again, nothing reaps it, and its provenance rows live forever —
// every message it held stays "in" a mailbox that no longer exists, in mailboxIds,
// in inMailbox queries, and in the mailbox counts.
//
// FAIL CLOSED, exactly as the reaper does. The evidence that a folder was deleted
// is a SUCCESSFUL LIST that no longer names a folder we hold — never a failed or
// partial LIST. That gate lives in the caller (mailsync): a LIST error is fatal
// before this is reached, and a successful LIST that does not even name INBOX (the
// one mailbox RFC 9051 guarantees always exists) is refused there as a broken
// answer. By the time ReconcileDeletedMailbox runs, "this folder is gone" is as
// well-corroborated as the reaper's "this UID is gone".
//
// THE ON DELETE SET NULL LANDMINE. app.messages.moved_mailbox_id and
// local_mailbox_id both reference app.mailboxes ON DELETE SET NULL (db/schema.sql).
// A bare DELETE of the app.mailboxes row would make Postgres SILENTLY null every
// canonical placement that named this folder — a JMAP-visible membership change
// with no change_log row, undoing #84 invisibly for every message the user moved
// there. So this method never lets the cascade decide anything: it removes every
// reference to the folder ITSELF, first, with the change_log rows each one owes,
// and only then deletes the row — by which point ON DELETE SET NULL has nothing
// left to act on. The ordering IS the mitigation.

// DeletedMailboxResult reports one deleted folder's reconciliation. Its shape
// mirrors ReapResult because it is the same reconciliation, applied to the whole
// folder at once rather than to a diffed UID set.
type DeletedMailboxResult struct {
	// ProvenanceDropped is the number of live physical locations in the folder
	// that were deleted — the folder is gone on the server, so all of them are.
	ProvenanceDropped int
	// MembershipChanged is the number of messages whose JMAP mailboxIds actually
	// moved — exactly the number of Email change_log rows appended.
	MembershipChanged int
	// MessagesVanished is the number of messages left in no location at all and
	// soft-deleted (trashed_at + purge_after + vanished_at, ADR-009, retractable).
	// A subset of MembershipChanged, and shared with the reaper through reapMessage.
	MessagesVanished int
}

// ReconcileDeletedMailbox reconciles a mailbox the server's LIST no longer
// reports, in this transaction, and then destroys it:
//
//  1. Every LIVE provenance row in the folder is deleted (the folder is gone, so
//     every UID it held is gone with it); the retired-generation rows go too, as
//     history that now indexes a mailbox that no longer exists. Live drops are the
//     ones that can move membership, so their messages are collected.
//  2. Every canonical placement (moved_mailbox_id) and local placement
//     (local_mailbox_id) that NAMES this folder is cleared explicitly — the
//     landmine mitigation above — and those messages are collected too.
//  3. Each collected message is reconciled by reapMessage, the SAME code the
//     expunge reaper and the UIDVALIDITY roll use: one Email change_log row iff
//     its effective membership actually moved, a retractable soft-delete iff it is
//     now in no location anywhere, and NOTHING for a message a surviving placement
//     or another live location still holds.
//  4. The Mailbox is a JMAP-visible object, so its destruction appends one Mailbox
//     `destroyed` change_log row.
//  5. The app.mailboxes row is deleted. After steps 1-2 nothing references it, so
//     no ON DELETE cascade or SET NULL fires — the delete is the honest recording
//     of a destruction we have already reconciled, not the thing that causes it.
//
// One transaction: either the folder's membership changes, their change_log rows,
// the soft-deletes, the Mailbox destroy and the row delete ALL land, or none do.
// There is no SAVEPOINT, for the same reason ReapMailbox has none — no recoverable
// per-object failure to isolate; per-mailbox isolation lives one level up, where
// each deleted folder is reconciled in its own UserTx.
func (tx *UserTx) ReconcileDeletedMailbox(ctx context.Context, mailboxID ulid.ULID) (DeletedMailboxResult, error) {
	var res DeletedMailboxResult
	if mailboxID == (ulid.ULID{}) {
		return res, fmt.Errorf("store: reconcile deleted mailbox: mailbox id is required")
	}

	affected := map[ulid.ULID]struct{}{}

	// 1a. Drop the LIVE locations — the ones membership is derived from. Collect
	// their messages for reconciliation.
	rows, err := tx.tx.Query(ctx, `
		DELETE FROM app.message_provenance p
		 WHERE p.user_id = $1 AND p.mailbox_id = $2 AND p.state = 'live'
		RETURNING p.message_id`,
		tx.userID.String(), mailboxID.String())
	if err != nil {
		return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox %s: %w", mailboxID, err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox: scan message id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			rows.Close()
			return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox: parse message id %q: %w", id, err)
		}
		res.ProvenanceDropped++
		affected[u] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox %s: %w", mailboxID, err)
	}

	// 1b. Drop the retired-generation rows too. They are already invisible to
	// membership (state <> 'live'), so they need no change_log row — but they must
	// go before the app.mailboxes row can be deleted, and keeping history that
	// points at a destroyed folder serves nothing.
	if _, err := tx.tx.Exec(ctx, `
		DELETE FROM app.message_provenance
		 WHERE user_id = $1 AND mailbox_id = $2`,
		tx.userID.String(), mailboxID.String()); err != nil {
		return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox %s (stale rows): %w", mailboxID, err)
	}

	// 2. Clear the canonical and local placements that NAME this folder, before the
	// row delete can null them silently. Each is a JMAP-visible membership change,
	// so its message is collected and reconciled by reapMessage below (which is
	// where the one change_log row per message, and any soft-delete, is decided).
	// A message archived INTO this now-deleted folder loses its placement here; its
	// membership then falls back to whatever provenance/local location survives, or
	// to nothing — the honest state, whereas the silent SET NULL would leave it
	// there with no notification.
	for _, col := range []string{"moved_mailbox_id", "local_mailbox_id"} {
		pr, err := tx.tx.Query(ctx, `
			UPDATE app.messages m
			   SET `+col+` = NULL
			 WHERE m.user_id = $1 AND m.`+col+` = $2
			RETURNING m.id`,
			tx.userID.String(), mailboxID.String())
		if err != nil {
			return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox %s (clear %s): %w", mailboxID, col, err)
		}
		for pr.Next() {
			var id string
			if err := pr.Scan(&id); err != nil {
				pr.Close()
				return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox: scan %s message id: %w", col, err)
			}
			u, err := ulid.Parse(id)
			if err != nil {
				pr.Close()
				return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox: parse message id %q: %w", id, err)
			}
			affected[u] = struct{}{}
		}
		pr.Close()
		if err := pr.Err(); err != nil {
			return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox %s (clear %s): %w", mailboxID, col, err)
		}
	}

	// 3. Reconcile each affected message once. reapMessage now sees the post-clear
	// world: a message whose placement pointed at this folder no longer has one, so
	// its membership is recomputed from what actually survives.
	for msgID := range affected {
		changed, vanished, err := tx.reapMessage(ctx, msgID, mailboxID)
		if err != nil {
			return DeletedMailboxResult{}, err
		}
		if changed {
			res.MembershipChanged++
		}
		if vanished {
			res.MessagesVanished++
		}
	}

	// 4. The Mailbox object is destroyed (the load-bearing invariant — a
	// JMAP-visible object's destruction is one change_log row).
	if _, err := tx.AppendChange(ctx, ObjectMailbox, mailboxID, OpDestroyed); err != nil {
		return DeletedMailboxResult{}, err
	}

	// 5. Delete the row itself. Nothing references it now, so ON DELETE SET NULL
	// and ON DELETE CASCADE have nothing to act on — this cannot silently undo a
	// canonical placement, because step 2 already handled every one of them.
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.mailboxes WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String())
	if err != nil {
		return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox %s (delete row): %w", mailboxID, err)
	}
	if tag.RowsAffected() == 0 {
		return DeletedMailboxResult{}, fmt.Errorf("store: reconcile deleted mailbox: mailbox %s not found", mailboxID)
	}
	return res, nil
}

// StampMailboxReaped records that a mailbox's live UID set was successfully
// OBSERVED and fully acted on, now. It is the cadence cursor for everything that
// question drives — the expunge reap (#86) and the gap backfill (#93), which read
// one LiveUIDs answer between them — and only a pass that actually completed
// advances it. A mailbox whose live-UID read failed, whose answer was refused, or
// whose backfill could not close a gap it found is NOT stamped, so it is retried
// on the next pass rather than being marked done. Like the other sync cursors it
// is internal bookkeeping and appends no change_log row (mailboxes.go file header).
//
// It is a REAL observation, so it also records what that observation saw (#620,
// migration 00058): last_observed_full_at, and seen as the baseline a later
// LIST-STATUS is compared against. A nil seen stores NULL — "this observation
// established no baseline" — and must overwrite any older one, so the next due
// observation runs for real. The caller passes nil when the observation's proof
// did not cover the whole UID space it saw (mailsync.verifyMailbox).
func (tx *UserTx) StampMailboxReaped(ctx context.Context, mailboxID ulid.ULID, seen *ObservedStatus) error {
	var validity, next, messages, modseq *int64
	if seen != nil {
		validity, next, messages, modseq = &seen.UIDValidity, &seen.UIDNext, &seen.Messages, &seen.HighestModSeq
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.mailboxes
		   SET last_reaped_at          = now(),
		       last_observed_full_at   = now(),
		       observed_uidvalidity    = $3,
		       observed_uidnext        = $4,
		       observed_messages       = $5,
		       observed_highest_modseq = $6
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String(), validity, next, messages, modseq)
	if err != nil {
		return fmt.Errorf("store: stamp mailbox reaped: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: stamp mailbox reaped: mailbox %s not found", mailboxID)
	}
	return nil
}

// StampMailboxUnchanged advances ONLY the observation cadence cursor
// (last_reaped_at), for a due mailbox whose observation was SKIPPED because
// LIST-STATUS showed nothing had changed since the last real one (#620).
//
// It deliberately leaves last_observed_full_at and the observed_* baseline
// alone: nothing was observed, so nothing about the baseline is newer, and
// re-stamping last_observed_full_at here would let a folder that reads as
// unchanged skip its UID SEARCH forever instead of at most 24 hours.
func (tx *UserTx) StampMailboxUnchanged(ctx context.Context, mailboxID ulid.ULID) error {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.mailboxes SET last_reaped_at = now()
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String())
	if err != nil {
		return fmt.Errorf("store: stamp mailbox unchanged: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: stamp mailbox unchanged: mailbox %s not found", mailboxID)
	}
	return nil
}
