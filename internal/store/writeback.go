// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// This file holds the store surface for the writeback engine (spec §4a): the
// queue of intent DELTAS that mirror local mutations back to the IMAP server.
//
// writeback_intents is internal machinery, NOT a JMAP-visible object, so these
// methods deliberately do NOT append a change_log row (contrast ingest.go).
// The load-bearing change_log invariant is about JMAP objects (messages,
// mailboxes, labels, threads, identities, contacts, submissions); the intent
// queue is a private write-path detail. Every method still runs on a UserTx,
// so RLS scopes each statement to the owning user (fail closed).

// IntentOp mirrors the op strings stored in app.writeback_intents.op. Each op
// is a DELTA, never a target state, so replay is safe (spec §4a).
type IntentOp string

const (
	IntentAddFlag       IntentOp = "+flag"
	IntentRemoveFlag    IntentOp = "-flag"
	IntentAddKeyword    IntentOp = "+kw"
	IntentRemoveKeyword IntentOp = "-kw"
	IntentMove          IntentOp = "move"
	IntentAppend        IntentOp = "append"
	IntentExpunge       IntentOp = "expunge"
)

// IntentState mirrors the app.intent_state enum.
type IntentState string

const (
	IntentPending  IntentState = "pending"
	IntentInflight IntentState = "inflight"
	IntentApplied  IntentState = "applied"
	IntentDead     IntentState = "dead"
)

// What asked for an intent (app.writeback_intents.source, migration 00057).
//
// ⚠ THE STRINGS ARE DELIBERATELY THE SAME as app.message_labels.applied_by's
// (internal/store/label_source.go) wherever they mean the same thing, so 'rule'
// denotes the same actor in both tables and a reader learns one vocabulary.
// Two subsystem values are added for the paths no person initiates. They are
// new strings rather than a stretched reading of an existing one, because
// neither is an actor in the applied_by sense.
//
// ⚠ THERE IS NO CONSTANT FOR "UNKNOWN", ON PURPOSE. The column is nullable and
// NULL means "enqueued before 00057, actor unrecoverable" — a fact about
// history, not a value a caller may choose. A caller that does not know what it
// is doing must not be able to say so in a way that reads like an answer; see
// the migration for why backfilling those rows as 'user' would have repeated
// the exact falsehood label_source.go was written to end.
const (
	IntentByUser  = LabelByUser
	IntentByRule  = LabelByRule
	IntentByML    = LabelByML
	IntentByAgent = LabelByAgent
	IntentByDesk  = LabelByDesk
	IntentBySetup = LabelBySetup
	// IntentByPurge: the Trash purge sweep's phase-1 expunge (internal/purge).
	// ⚠ #555 TURNS ON THIS VALUE. It is the only thing that lets the engine tell
	// a purge EXPUNGE — whose precondition ("still trashed, still due") can go
	// stale between enqueue and apply — from a draft-retirement EXPUNGE, whose
	// precondition has nothing to do with Trash. Re-checking trashed state
	// against the latter would refuse to retire drafts.
	IntentByPurge = "purge"
	// IntentByDraft: the draft machinery (internal/store/drafts.go) — an
	// autosaved revision's APPEND, and the header-addressed EXPUNGE that
	// retires a superseded one. Not a user action even though a person's typing
	// triggers it: the user asked for a draft to exist, not for these specific
	// IMAP round trips, and #554 item 3 is precisely that the ledger must stop
	// counting them as though they were deliberate.
	IntentByDraft = "draft"
)

// IntentRow is the input to EnqueueIntent. MessageID is nil for ops that do
// not target an existing message (e.g. an APPEND of freshly built bytes).
// Payload is raw JSONB holding the op arguments (flag name, target mailbox,
// expected uidvalidity, append body, ...); the store treats it opaquely.
type IntentRow struct {
	MessageID      *ulid.ULID
	AccountID      ulid.ULID
	Op             IntentOp
	Payload        []byte
	IdempotencyKey string
	// Revive asks EnqueueIntent to bring a TERMINAL row with this key (applied
	// or dead) back to pending, instead of collapsing into it.
	//
	// ⚠ THIS IS WHAT BREAKS THE STALE-DRAFT LOOP (#268). Plain idempotency is
	// keyed on (user_id, idempotency_key) for the row's whole lifetime, so an
	// expunge that ran an hour ago silently swallows the expunge a user asks for
	// now: the operator deleted a leftover draft inside Mailroom, no intent was
	// enqueued because an APPLIED one already held the key, the next sync
	// re-ingested the surviving IMAP copy, and the message came back.
	//
	// The distinction is between the two things idempotency is protecting
	// against. A duplicate of a request still in flight is noise and must
	// collapse — so a pending or inflight row is left exactly as it is. A
	// request made AFTER the previous one finished is a new fact about the
	// server, and refusing to act on it is not idempotency, it is amnesia.
	//
	// The EXPUNGES set this: the header-addressed ones (store/drafts.go) and,
	// since #296, the purge sweep's message-addressed ones
	// (EnqueuePurgeExpungeIntents — see its doc for why a terminal row silently
	// swallowing the re-enqueue left destroyed mail on the server forever).
	// Reviving an APPEND would re-send bytes the server already has, which is a
	// different question with a different answer.
	Revive bool
	// Source is what asked for this intent: one of the IntentBy* constants.
	//
	// ⚠ EMPTY IS WRITTEN AS SQL NULL, NOT AS A DEFAULT. There is deliberately no
	// "assume the user did it" fallback here, which is the opposite of
	// LabelSource.columns()' zero value. That default is right for labels,
	// where the JMAP path really is a person acting through a client; it is
	// wrong here, because four of the seven enqueue paths are machinery and a
	// silent 'user' would be a claim nobody made. An unattributed row says
	// "unknown" and stays legible as such.
	Source string
}

// Intent is a claimed writeback intent, as returned by ClaimPendingIntents.
type Intent struct {
	ID             ulid.ULID
	MessageID      *ulid.ULID
	AccountID      ulid.ULID
	Op             IntentOp
	Payload        []byte
	IdempotencyKey string
	State          IntentState
	Attempts       int
	LastError      string
	DeadReason     string
	// Source is what asked for this intent: one of the IntentBy* constants, or
	// EMPTY for a row enqueued before migration 00057, where the actor is
	// unrecoverable.
	//
	// ⚠ EMPTY MEANS UNKNOWN AND MUST NEVER BE TREATED AS A VALUE. In particular
	// a caller deciding whether this is a purge expunge must test
	// `Source == IntentByPurge`, never `Source != IntentByDraft` — the latter
	// silently sweeps every pre-00057 row into the purge branch, which is the
	// branch that destroys mail.
	Source string
}

// ProvenanceTarget is where a message physically lives in an account right
// now — the coordinates the writeback engine applies an op against, and the
// baseline it compares to detect a conflict (message moved/expunged remotely,
// UIDVALIDITY bumped) before mutating the server (spec §4).
type ProvenanceTarget struct {
	ProvenanceID ulid.ULID
	MailboxID    ulid.ULID
	IMAPName     string // app.mailboxes.imap_name — the SELECT target
	UID          int64
	UIDValidity  int64
	State        string // app.provenance_state: one of the constants below
	// GmMsgID is the copy's Gmail X-GM-MSGID (#52, ADR-047 §3), 0 when the
	// copy has none (every non-Gmail copy, or a Gmail copy ingested without
	// a reading). The Gmail writeback path re-resolves the UID by it.
	GmMsgID uint64
}

// The two values of app.provenance_state, named so that the one comparison the
// writeback engine must never get wrong — "is this coordinate still valid?" —
// is not spelled as a bare string literal in three packages (#296).
const (
	// ProvenanceLive: the (mailbox, uidvalidity, uid) coordinate is current and
	// writable.
	ProvenanceLive = "live"
	// ProvenanceStaleUIDValidity: the mailbox was recreated on the server and
	// this row belongs to a generation the server has thrown away. Readable as
	// history; NEVER a write target (the UID may now name a different message).
	ProvenanceStaleUIDValidity = "stale_uidvalidity"
)

// EnqueueIntent inserts a pending intent. Enqueue is idempotent on
// (user_id, idempotency_key): a duplicate key is a no-op that returns the id
// of the existing row rather than inserting a second one or erroring. Callers
// therefore get replay-safety for free — the same logical mutation enqueued
// twice produces exactly one intent.
//
// IntentRow.Revive narrows that: see its doc for why an ALREADY-FINISHED row
// must not silence a request made later (#268).
func (tx *UserTx) EnqueueIntent(ctx context.Context, in IntentRow) (ulid.ULID, error) {
	payload := in.Payload
	if payload == nil {
		payload = []byte("{}")
	}
	var messageID any
	if in.MessageID != nil {
		messageID = in.MessageID.String()
	}
	// Empty source becomes SQL NULL rather than '', which the
	// writeback_intents_source_known CHECK would reject outright. NULL is the
	// column's documented "unrecoverable" value; '' would be a new third thing
	// meaning the same, which is how a two-state column becomes a three-state
	// one nobody planned.
	var source any
	if in.Source != "" {
		source = in.Source
	}

	id := NewULID()
	var returned string
	// The ON CONFLICT arm is DO NOTHING unless the caller asked to revive (see
	// IntentRow.Revive): with revive false the WHERE is constant-false, no row is
	// updated, and this behaves exactly as the plain DO NOTHING form did. With
	// revive true, a row that has already FINISHED — applied or dead — is reset
	// to pending and re-dated, so it is claimed again by the next pass; a row
	// still pending or inflight is left alone, because that request has not been
	// answered yet and a second copy of it would be a duplicate.
	// INGEST-ONLY MODE (ADR-028). The state this intent is born in is decided
	// here, in the same statement, from the account's writeback_enabled column
	// ANDed with the instance-level switch. A 'suppressed' intent is TERMINAL
	// and is never claimed by ClaimPendingIntents — it is a ledger entry
	// recording a change Mailroom would have made to a mailbox it must not
	// touch, not work waiting to happen.
	//
	// ⚠ FAIL CLOSED on a missing account row. The FK plus RLS make that
	// impossible in practice (same user, same transaction), so the COALESCE is
	// reached only if something is already wrong — and the safe answer to "I
	// cannot tell whether I may write to this mailbox" is not to write.
	//
	// ⚠ THE SECOND ON CONFLICT ARM IS NOT OPTIONAL. Without it, a suppressed row
	// would silently swallow the same logical mutation forever: the re-request
	// hits the existing idempotency key, DO NOTHING fires, and the change never
	// reaches IMAP — a brand-new silent divergence created by the mechanism
	// meant to prevent one. So a suppressed row is UPGRADED to pending when the
	// same key is enqueued again while writeback is LIVE. While still shadowed
	// the new row is itself 'suppressed', EXCLUDED.state is not 'pending', and
	// the arm does not fire — the ledger cannot promote itself.
	//
	// ⚠ WHICH CALLERS CAN ACTUALLY REACH IT, because this was documented wrong
	// once. NOT Email/set: ApplyEmailUpdate mints a fresh action id per call
	// (jmap_email.go) and folds it into every key, so a user repeating a
	// mutation inserts a NEW row and never conflicts. The arm is reachable only
	// from the deterministic-key callers — the purge expunge
	// (`purge-expunge:<msg>:<acct>`, with Revive), and the draft header-expunge
	// and draft APPEND in drafts.go.
	//
	// The purge one is a TIMER, not a user action, so on its own it would be a
	// second, unreviewed way out of the ledger after a lift. What stops that is
	// not this arm but DeferPurgeDeadline (store/purge.go): a shadowed
	// message's purge_after is pushed out a full window, so it is not due at the
	// moment of lift and the operator has that window to report and --apply.
	//
	// Conversely `w.state IN ('applied','dead')` deliberately does NOT include
	// 'suppressed', so Revive (the purge worker's re-enqueue) cannot resurrect
	// the ledger either.
	//
	// ⚠ `source = EXCLUDED.source` ON THE UPDATE ARM, deliberately. Both arms
	// re-date the row and reset its state — they describe a request made NOW,
	// not the one it collapsed into — so the antecedent has to move with it.
	// Keeping the original would leave a row whose state says "pending, asked
	// for just now" beside a source naming whatever asked months ago, and #555's
	// apply-time check reads that source to decide whether it may destroy mail.
	// A stale source there is not untidiness, it is the check consulting the
	// wrong fact.
	err := tx.tx.QueryRow(ctx, `
		WITH wb AS (
			SELECT COALESCE((SELECT a.writeback_enabled AND NOT `+gmailWritebackHeldSQL+`
			                   FROM app.accounts a
			                  WHERE a.id = $4 AND a.user_id = $2), false)
			       AND NOT $9::boolean AS live
		)
		INSERT INTO app.writeback_intents AS w
			(id, user_id, message_id, account_id, op, payload, idempotency_key, state, source)
		SELECT $1, $2, $3, $4, $5, $6, $7,
		       CASE WHEN wb.live THEN 'pending' ELSE 'suppressed' END::app.intent_state,
		       $10
		  FROM wb
		ON CONFLICT (user_id, idempotency_key) DO UPDATE
		   SET state       = EXCLUDED.state,
		       attempts    = 0,
		       payload     = EXCLUDED.payload,
		       last_error  = NULL,
		       dead_reason = NULL,
		       applied_at  = NULL,
		       created_at  = now(),
		       source      = EXCLUDED.source
		 WHERE ($8::boolean AND w.state IN ('applied', 'dead'))
		    OR (w.state = 'suppressed' AND EXCLUDED.state = 'pending')
		RETURNING id`,
		id.String(), tx.userID.String(), messageID, in.AccountID.String(),
		string(in.Op), payload, in.IdempotencyKey, in.Revive, tx.writebackDisabled,
		source,
	).Scan(&returned)
	if errors.Is(err, pgx.ErrNoRows) {
		// Duplicate idempotency_key and nothing was revived: the row already
		// exists and stays as it is. Return its id so enqueue is a true no-op
		// rather than an error.
		if err := tx.tx.QueryRow(ctx, `
			SELECT id FROM app.writeback_intents
			 WHERE user_id = $1 AND idempotency_key = $2`,
			tx.userID.String(), in.IdempotencyKey,
		).Scan(&returned); err != nil {
			return ulid.ULID{}, fmt.Errorf("writeback: read existing intent: %w", err)
		}
	} else if err != nil {
		return ulid.ULID{}, fmt.Errorf("writeback: enqueue intent: %w", err)
	}

	u, err := ulid.Parse(returned)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("writeback: parse intent id %q: %w", returned, err)
	}
	return u, nil
}

// ClaimPendingIntents atomically claims up to limit intents for an account: it
// moves them 'pending' -> 'inflight', stamps claimed_at, increments attempts,
// and returns them. FOR UPDATE SKIP LOCKED lets multiple workers drain the same
// account's queue without blocking or double-claiming (the same pattern as the
// job dispatcher). The claim is the first half of the pending->inflight->applied
// state machine that makes application idempotent.
//
// ⚠ IT ALSO RECLAIMS INTENTS STRANDED IN 'inflight' (#520). A process that dies
// between claiming an intent and completing it leaves the row inflight, and
// MarkIntentApplied/MarkIntentFailed are both guarded on state='inflight' with
// no other reader — so without this arm the row is never looked at again.
// Nothing re-fires, which is fail-SAFE, but the user's flag change or move
// silently never reaches IMAP and only 'dead' is surfaced in account health.
//
// The lease mirrors app.dequeue_due_jobs (migration 00041) at 15 minutes, and it
// cannot race a live pass: syncGate guarantees at most one pass per account at
// any instant, so a reclaim only ever runs in a pass that started after the
// previous one ended or died. See db/migrations/00053 for the per-op safety
// argument and the measurement that says this is insurance, not a repair.
//
// ⚠ THE ORDER IS ENQUEUE ORDER, AND `created_at` ALONE DOES NOT GIVE IT (#576).
// created_at defaults to now(), which is the TRANSACTION's start time, so every
// intent one Email/set enqueues carries the same value and ORDER BY created_at
// left their relative order to the plan: heap order on a good day, anything
// after a sort or a page reuse. That order is not cosmetic. A keyword STORE
// addresses the copy by its UID in the SOURCE folder; once a MOVE in the same
// action has run, that UID is gone, a UID STORE against it succeeds doing
// nothing, and the move has already carried the old keywords to the
// destination. The id breaks the tie: ids are ULIDs minted app-side from a
// monotonic source (NewULID), so within one transaction they increase in the
// order EnqueueIntent was called. COLLATE "C" because app.ulid is text and the
// ULID alphabet only sorts by time under byte order.
//
// ⚠ AND THE ORDER BY HAS TO BE ON THE OUTSIDE. The inner one only chooses
// WHICH rows the LIMIT claims; an UPDATE ... RETURNING hands its rows back in
// whatever order it happened to update them, which is not the subquery's.
// Measured while fixing #576: one Not-junk action's intents came back as
// [+kw, -kw, move] with the ORDER BY inside only. The engine applies a batch in
// the order this returns, so before this the claim's ORDER BY had never
// governed application order at all.
func (tx *UserTx) ClaimPendingIntents(ctx context.Context, accountID ulid.ULID, limit int) ([]Intent, error) {
	rows, err := tx.tx.Query(ctx, `
		WITH claimed AS (
			UPDATE app.writeback_intents w
			   SET state = 'inflight', attempts = attempts + 1, claimed_at = now()
			 WHERE w.id IN (
				SELECT id FROM app.writeback_intents
				 WHERE user_id = $1 AND account_id = $2
				   AND (state = 'pending'
				        OR (state = 'inflight' AND claimed_at IS NOT NULL
				            AND claimed_at < now() - interval '15 minutes'))
				 ORDER BY created_at, id COLLATE "C"
				   FOR UPDATE SKIP LOCKED
				 LIMIT $3)
			RETURNING w.id, w.message_id, w.account_id, w.op, w.payload,
			          w.idempotency_key, w.state, w.attempts,
			          COALESCE(w.last_error, '') AS last_error,
			          COALESCE(w.dead_reason, '') AS dead_reason,
			          COALESCE(w.source, '') AS source, w.created_at
		)
		SELECT id, message_id, account_id, op, payload, idempotency_key, state,
		       attempts, last_error, dead_reason, source
		  FROM claimed
		 ORDER BY created_at, id COLLATE "C"`,
		tx.userID.String(), accountID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("writeback: claim pending intents: %w", err)
	}
	defer rows.Close()

	var out []Intent
	for rows.Next() {
		var (
			in        Intent
			id        string
			messageID *string
			accountID string
			op        string
			state     string
		)
		if err := rows.Scan(&id, &messageID, &accountID, &op, &in.Payload,
			&in.IdempotencyKey, &state, &in.Attempts, &in.LastError, &in.DeadReason,
			&in.Source); err != nil {
			return nil, fmt.Errorf("writeback: scan intent: %w", err)
		}
		if in.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("writeback: intent id %q: %w", id, err)
		}
		if messageID != nil {
			m, err := ulid.Parse(*messageID)
			if err != nil {
				return nil, fmt.Errorf("writeback: intent message id %q: %w", *messageID, err)
			}
			in.MessageID = &m
		}
		if in.AccountID, err = ulid.Parse(accountID); err != nil {
			return nil, fmt.Errorf("writeback: intent account id %q: %w", accountID, err)
		}
		in.Op = IntentOp(op)
		in.State = IntentState(state)
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("writeback: claim pending intents: %w", err)
	}
	return out, nil
}

// CountOutstandingIntents returns how many intents for an account are still
// owed to the server — pending, plus inflight (claimed by a pass that has not
// finished, or stranded by one that died and awaiting the lease reclaim).
//
// It backs mailroom_action_queue_depth (#524). Terminal states are excluded:
// 'applied' is done and 'dead' is surfaced separately in account health, so
// counting either would make the gauge a running total that never falls.
//
// ⚠ INFLIGHT IS INCLUDED DELIBERATELY. Those rows are work the user has asked
// for and the server has not yet been told about, which is exactly what "queue
// depth" should mean. It also means a strand shows up here as a floor the gauge
// never returns below, which is a second, independent way to notice the failure
// #520 fixes — the reclaim empties it, a leak would not.
func (tx *UserTx) CountOutstandingIntents(ctx context.Context, accountID ulid.ULID) (int64, error) {
	var n int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2
		   AND state IN ('pending', 'inflight')`,
		tx.userID.String(), accountID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("writeback: count outstanding intents: %w", err)
	}
	return n, nil
}

// SuppressAccountIntents moves every not-yet-finished intent for one account to
// the terminal 'suppressed' state, returning how many it moved. It is what the
// API layer calls, in the SAME transaction as the flag change, when an account
// is switched into ingest-only mode (ADR-028 §4).
//
// ⚠ WITHOUT THIS CALL THE MODE HAS A HOLE AT ITS FRONT EDGE. Suppression at
// enqueue only governs intents created AFTER the switch; anything queued before
// it stays 'pending' and fires on the very next pass — the account is shadowed
// and its backlog drains anyway. An enqueue guard and an apply guard are
// different guarantees (#521 question 2), and this is what closes the gap
// between them at the moment of transition.
//
// 'inflight' rows are included deliberately: at most one pass per account runs
// at a time (syncGate), so an inflight row here belongs either to a pass that
// has ended or to a dead process, and in both cases the 15-minute reclaim would
// otherwise pick it up and apply it to a mailbox we must not touch.
func (tx *UserTx) SuppressAccountIntents(ctx context.Context, accountID ulid.ULID) (int64, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.writeback_intents
		   SET state = 'suppressed', claimed_at = NULL
		 WHERE user_id = $1 AND account_id = $2
		   AND state IN ('pending', 'inflight')`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return 0, fmt.Errorf("writeback: suppress account intents: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SuppressedIntentCount returns how many suppressed intents an account has
// accumulated — the size of the ingest-only ledger, surfaced by
// Account/getHealth so a shadow run is legible without a SQL prompt.
func (tx *UserTx) SuppressedIntentCount(ctx context.Context, accountID ulid.ULID) (int64, error) {
	var n int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND state = 'suppressed'`,
		tx.userID.String(), accountID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("writeback: count suppressed intents: %w", err)
	}
	return n, nil
}

// SuppressedIntentsByOp returns the ingest-only ledger broken down by operation,
// which is the report form that actually answers "what would Mailroom have done
// to my mailbox?".
//
// ⚠ PER-OP, NEVER ONE TOTAL. A single number cannot distinguish "the shadow run
// changed nothing" from "the one thing I measured is always empty" (CLAUDE.md);
// "1,200 +flag, 400 move, 247 +kw" is a finding and "1,847" is not.
func (tx *UserTx) SuppressedIntentsByOp(ctx context.Context, accountID ulid.ULID) (map[string]int64, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT op, count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND state = 'suppressed'
		 GROUP BY op ORDER BY op`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return nil, fmt.Errorf("writeback: suppressed intents by op: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var op string
		var n int64
		if err := rows.Scan(&op, &n); err != nil {
			return nil, fmt.Errorf("writeback: scan suppressed intent count: %w", err)
		}
		out[op] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("writeback: suppressed intents by op: %w", err)
	}
	return out, nil
}

// ReplaySuppressedIntents moves an account's suppressed ledger back to
// 'pending' so the next pass applies it, returning how many it revived. It
// exists solely behind the admin CLI's explicit --apply (ADR-028 §4).
//
// ⚠ It is the only path that moves the ledger IN BULK, but it is not the only
// path out of 'suppressed' — EnqueueIntent's second ON CONFLICT arm upgrades a
// single row when that exact idempotency key is enqueued again while writeback
// is live. This comment said "the ONLY path" until a review pointed out the
// arm; do not restore that wording, because it is the kind of false
// reassurance that stops the next person looking.
//
// ⚠ NOTHING ELSE MAY CALL THIS, and in particular lifting ingest-only mode must
// not. A fortnight of queued moves firing as a side effect of flipping a flag
// is a mass mutation nobody reviewed, arriving at the worst possible moment —
// which is precisely the hazard sync_enabled already has and this mode refuses
// to reproduce. The report comes first; applying it is a separate decision.
func (tx *UserTx) ReplaySuppressedIntents(ctx context.Context, accountID ulid.ULID) (int64, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.writeback_intents
		   SET state = 'pending', attempts = 0, claimed_at = NULL,
		       last_error = NULL, dead_reason = NULL
		 WHERE user_id = $1 AND account_id = $2 AND state = 'suppressed'`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return 0, fmt.Errorf("writeback: replay suppressed intents: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkIntentApplied transitions a claimed intent 'inflight' -> 'applied' and
// stamps applied_at. The state guard makes it a no-op if the row is no longer
// inflight, so a duplicate call is harmless — part of the idempotency story.
//
// ⚠ THE GUARD IS ON STATE, NOT ON WHO CLAIMED IT, and unlike app.jobs there is
// no locked_by to compare. So it cannot distinguish "I applied this" from
// "someone else's claim of the same row". That is sound here only because
// syncGate admits at most one pass per account at any instant, so two live
// claims of one intent cannot coexist: a reclaim (ClaimPendingIntents) only
// runs once the previous pass ended or its process died, and a dead process has
// no goroutine left to call this. It would NOT hold for two server processes
// sharing one database — that deployment would need a locked_by column.
//
// It reports whether a row actually moved (#691). false means the intent was no
// longer inflight — most often because it is gone: deleting a message cascades
// its intents away, and that can land between the claim and this mark. The
// caller must not count a false as an applied intent, because there is no row
// behind the count.
func (tx *UserTx) MarkIntentApplied(ctx context.Context, id ulid.ULID) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.writeback_intents
		   SET state = 'applied', applied_at = now()
		 WHERE user_id = $1 AND id = $2 AND state = 'inflight'`,
		tx.userID.String(), id.String())
	if err != nil {
		return false, fmt.Errorf("writeback: mark intent applied: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkIntentFailed records a failed application of a claimed intent. When
// deadReason is non-empty the intent moves 'inflight' -> 'dead' with
// dead_reason set — the SURFACED dead-letter state the account-health UI reads
// (spec §12); it is never retried and never silently dropped. Otherwise the
// intent returns to 'pending' with last_error recorded, so the next claim
// retries it. The caller (the engine) owns the bounded-retry policy and passes
// a deadReason once attempts are exhausted or a conflict is unrecoverable.
//
// Like MarkIntentApplied it reports whether a row moved (#691): a pass once
// logged dead_lettered=1 with no dead row anywhere, because the intent had been
// cascaded away while the pass was still working on it.
func (tx *UserTx) MarkIntentFailed(ctx context.Context, id ulid.ULID, errMsg, deadReason string) (bool, error) {
	if deadReason != "" {
		tag, err := tx.tx.Exec(ctx, `
			UPDATE app.writeback_intents
			   SET state = 'dead', last_error = $3, dead_reason = $4
			 WHERE user_id = $1 AND id = $2 AND state = 'inflight'`,
			tx.userID.String(), id.String(), nullStr(errMsg), deadReason)
		if err != nil {
			return false, fmt.Errorf("writeback: dead-letter intent: %w", err)
		}
		return tag.RowsAffected() == 1, nil
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.writeback_intents
		   SET state = 'pending', last_error = $3
		 WHERE user_id = $1 AND id = $2 AND state = 'inflight'`,
		tx.userID.String(), id.String(), nullStr(errMsg))
	if err != nil {
		return false, fmt.Errorf("writeback: retry intent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MessageProvenanceTargetsForAccount returns EVERY place a message physically
// lives in an account — the complete set of writeback targets, and the baseline
// each one's conflict check compares against (spec §4).
//
// ⚠ IT RETURNS A SET BECAUSE ONE MESSAGE CAN HAVE SEVERAL SERVER COPIES INSIDE
// ONE ACCOUNT (#296). Until v1.34.4 this read ended in
// `ORDER BY (state='live') DESC, first_seen_at DESC LIMIT 1` and the engine
// applied each intent to that single row. Meanwhile ingest merges the flags of
// every live copy with OR (internal/ingest.mergeFlags — "a positive flag set
// anywhere wins; keywords are unioned"). Those two halves disagree, and the
// disagreement is invisible for ADDS and destructive for REMOVALS: clearing
// \Seen on one copy leaves the others carrying it, the next CONDSTORE pass
// re-fetches the touched copy, ingest re-merges all of them, and is_seen comes
// straight back. The message marks itself read again on the next poll; the same
// chain un-removes a label and re-stars a message.
//
// Observed on a real instance before the fix: some messages had
// 2–4 live copies in one account, and several were in exactly the state where "mark
// unread" silently reverts. The multi-copy rows are correct — they are what
// #206's re-delivered-mail work deliberately records — so the fix is to widen
// this side, never to collapse them.
//
// The returned slice is HOMOGENEOUS, which is what lets the engine apply to
// every element without re-deciding safety row by row:
//
//   - empty: no provenance row of ANY state exists for this (message,
//     account) — the message was moved or expunged on the server since the
//     intent was created (or never had provenance in this account at all).
//   - every element 'live': the normal case — apply the op to ALL of them.
//   - every element 'stale_uidvalidity': no live row exists anywhere for this
//     (message, account). The mailbox was recreated on the server (UIDVALIDITY
//     rolled, mailroom#88) and the re-fetch under the new generation has not
//     (yet, or ever) re-attached a live row. That is a materially different
//     fact than "gone", and writeback.conflictReason reports it as such rather
//     than as "moved or expunged remotely" (mailroom#91). Those rows are
//     returned so the caller can SAY that — never so it can write to them: a
//     UID means nothing outside its UIDVALIDITY generation, so writing to a
//     stale coordinate can mutate or destroy whichever message the server has
//     since given that number to.
//
// A live row is therefore always preferred over a stale one regardless of which
// is newer, and the two are never mixed. Within the set the order is
// first_seen_at DESC — most recently observed copy first, the same row the old
// LIMIT 1 picked — so "the copy the engine used to reach" is still the first
// one it reaches, and the widening only ever ADDS work after that point.
//
// There is deliberately NO singular variant. The one-target read WAS the bug;
// keeping a `MessageProvenanceForAccount` beside this one would leave an
// obligingly-named way to reintroduce it, and no caller genuinely wants one
// copy — reaching all of them is the entire job of the writeback engine.
func (tx *UserTx) MessageProvenanceTargetsForAccount(ctx context.Context, messageID, accountID ulid.ULID) ([]ProvenanceTarget, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT p.id, p.mailbox_id, m.imap_name, p.uid, p.uidvalidity, p.state,
		       COALESCE(p.gm_msgid, '')
		  FROM app.message_provenance p
		  JOIN app.mailboxes m
		    ON m.id = p.mailbox_id AND m.user_id = p.user_id
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.account_id = $3
		 ORDER BY (p.state = 'live') DESC, p.first_seen_at DESC`,
		tx.userID.String(), messageID.String(), accountID.String())
	if err != nil {
		return nil, fmt.Errorf("writeback: provenance for account: %w", err)
	}
	defer rows.Close()

	var live, stale []ProvenanceTarget
	for rows.Next() {
		var (
			t            ProvenanceTarget
			provID, mbID string
			gmMsgID      string
		)
		if err := rows.Scan(&provID, &mbID, &t.IMAPName, &t.UID, &t.UIDValidity, &t.State, &gmMsgID); err != nil {
			return nil, fmt.Errorf("writeback: scan provenance target: %w", err)
		}
		if gmMsgID != "" {
			// A malformed id is treated as absent: the Gmail path then falls
			// back to the stored UID, never to a guess.
			t.GmMsgID, _ = strconv.ParseUint(gmMsgID, 10, 64)
		}
		if t.ProvenanceID, err = ulid.Parse(provID); err != nil {
			return nil, fmt.Errorf("writeback: provenance id %q: %w", provID, err)
		}
		if t.MailboxID, err = ulid.Parse(mbID); err != nil {
			return nil, fmt.Errorf("writeback: mailbox id %q: %w", mbID, err)
		}
		if t.State == ProvenanceLive {
			live = append(live, t)
		} else {
			stale = append(stale, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("writeback: provenance for account: %w", err)
	}
	// Partitioning here rather than in the caller is what makes the set
	// homogeneous, and homogeneity is a safety property: a mixed slice would put
	// an unwritable stale coordinate in a loop whose whole purpose is to write to
	// every element, one `if` away from destroying somebody else's mail.
	if len(live) > 0 {
		return live, nil
	}
	return stale, nil
}

// ListDeadIntents returns the dead-lettered intents for an account, so the
// account-health surface can show the user which mirror-backs gave up and why
// (dead_reason). Dead intents are surfaced, never swallowed (CLAUDE.md).
func (tx *UserTx) ListDeadIntents(ctx context.Context, accountID ulid.ULID) ([]Intent, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, message_id, account_id, op, payload, idempotency_key,
		       state, attempts, COALESCE(last_error, ''), COALESCE(dead_reason, '')
		  FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND state = 'dead'
		 ORDER BY created_at`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return nil, fmt.Errorf("writeback: list dead intents: %w", err)
	}
	defer rows.Close()

	var out []Intent
	for rows.Next() {
		var (
			in        Intent
			id        string
			messageID *string
			acct      string
			op        string
			state     string
		)
		if err := rows.Scan(&id, &messageID, &acct, &op, &in.Payload,
			&in.IdempotencyKey, &state, &in.Attempts, &in.LastError, &in.DeadReason); err != nil {
			return nil, fmt.Errorf("writeback: scan dead intent: %w", err)
		}
		if in.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("writeback: dead intent id %q: %w", id, err)
		}
		if messageID != nil {
			m, err := ulid.Parse(*messageID)
			if err != nil {
				return nil, fmt.Errorf("writeback: dead intent message id %q: %w", *messageID, err)
			}
			in.MessageID = &m
		}
		if in.AccountID, err = ulid.Parse(acct); err != nil {
			return nil, fmt.Errorf("writeback: dead intent account id %q: %w", acct, err)
		}
		in.Op = IntentOp(op)
		in.State = IntentState(state)
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("writeback: list dead intents: %w", err)
	}
	return out, nil
}

// WithdrawIntent deletes one intent outright. The caller must already HOLD the
// claim on it (state 'inflight', taken by ClaimPendingIntents in this pass),
// which is what makes deleting safe: nobody else can be acting on it.
//
// ⚠ THIS IS NOT MarkIntentFailed, AND THE DIFFERENCE IS THE POINT (#555). A
// withdrawn intent did not fail — it was correctly not performed, because its
// precondition stopped holding between enqueue and apply. Routing it through
// the failure path would put a dead_reason on it, and dead_reason is SURFACED
// to the user as a writeback problem: the guard that just saved a message from
// being destroyed would report itself as an error, and a shadow run's health
// signal would fill with them. Nor is it MarkIntentApplied, which would claim
// an IMAP operation happened that never did.
//
// The visible record of a withdrawal is the pass Result's own counter and the
// log line the engine emits, not a row: there is no 'withdrawn' state in
// app.intent_state, and adding one to describe work that must never happen
// would be a heavier change than the fact deserves.
func (tx *UserTx) WithdrawIntent(ctx context.Context, id ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx,
		`DELETE FROM app.writeback_intents WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()); err != nil {
		return fmt.Errorf("writeback: withdraw intent: %w", err)
	}
	return nil
}

// SuppressedIntentRow is one (op, source) bucket of an account's shadow ledger.
type SuppressedIntentRow struct {
	Op string
	// Source is what asked for these intents, or EMPTY for rows enqueued before
	// migration 00057, where the actor is unrecoverable. Empty is rendered as
	// "unknown" by the report rather than folded into any named source.
	Source string
	Count  int64
}

// SuppressedIntentsByOpAndSource breaks an account's shadow ledger down by BOTH
// the operation and what asked for it (#554 item 3).
//
// ⚠ WHY THE SECOND DIMENSION IS THE POINT. The op-only breakdown counts a draft
// autosave and a deliberate user action identically, so `--apply`'s headline
// number cannot be read as "changes the user asked for". That is not a
// cosmetic complaint: ingest-time rule labelling produces '+kw' in bulk, and on
// a real corpus most intents are '+kw'. An operator deciding whether
// to replay a shadow run's ledger onto a real mailbox is looking at a number
// dominated by machinery, with no way to tell from the report.
//
// It is deliberately a SLICE of explicit rows rather than a nested map: the
// report prints every bucket it is given, and a map-of-maps invites a printer
// that iterates the sources it knows about and silently drops the rest — which
// is the failure the op-only version already guards against by name.
func (tx *UserTx) SuppressedIntentsByOpAndSource(ctx context.Context, accountID ulid.ULID) ([]SuppressedIntentRow, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT op, COALESCE(source, ''), count(*)
		  FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND state = 'suppressed'
		 GROUP BY op, source
		 ORDER BY op, source`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return nil, fmt.Errorf("writeback: suppressed intents by op and source: %w", err)
	}
	defer rows.Close()
	var out []SuppressedIntentRow
	for rows.Next() {
		var r SuppressedIntentRow
		if err := rows.Scan(&r.Op, &r.Source, &r.Count); err != nil {
			return nil, fmt.Errorf("writeback: scan suppressed intent bucket: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("writeback: suppressed intents by op and source: %w", err)
	}
	return out, nil
}
