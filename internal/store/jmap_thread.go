// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// This file holds the read surface for JMAP Thread/get (RFC 8621 §3). A Thread
// is just an ordered list of its Email ids; membership lives on
// messages.thread_id and is recomputable (spec §4). Reads run on a UserTx, so
// RLS scopes every row to the caller.

// ThreadView is a JMAP Thread projection: the thread id and its member Email
// ids in conversation order (received_at, then id as a stable tiebreak).
type ThreadView struct {
	ID       ulid.ULID
	EmailIDs []ulid.ULID
	// Muted reports the thread's mute state (spec §6, #51) — "stop surfacing
	// this conversation". Thread-scoped because that is what the user means:
	// muting one message of a conversation is not a thing anyone wants. What
	// it DOES happens at ingest, not here: DivertMutedArrival (rules.go) keeps a
	// new reply out of the inbox and the sync engine leaves it out of the push.
	Muted bool
}

// GetThreads returns Thread projections for the requested ids. A thread that
// exists (a row in app.threads) but has no live messages returns with an empty
// EmailIDs slice; an id with no thread row is simply absent from the result
// (the caller reports it as notFound). RLS makes another user's thread ids
// indistinguishable from non-existent ones — the isolation property.
func (tx *UserTx) GetThreads(ctx context.Context, ids []ulid.ULID) ([]ThreadView, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	idStrs := ulidStrings(ids)

	// Which requested threads exist at all (so an existing-but-empty thread is
	// distinguished from a not-found id).
	existing := make(map[ulid.ULID]bool)
	muted := make(map[ulid.ULID]bool)
	erows, err := tx.tx.Query(ctx,
		`SELECT id, muted_at IS NOT NULL FROM app.threads
		  WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), idStrs)
	if err != nil {
		return nil, fmt.Errorf("store: get threads (existence): %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var id string
		var isMuted bool
		if err := erows.Scan(&id, &isMuted); err != nil {
			return nil, fmt.Errorf("store: scan thread id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse thread id %q: %w", id, err)
		}
		existing[u] = true
		muted[u] = isMuted
	}
	if err := erows.Err(); err != nil {
		return nil, fmt.Errorf("store: get threads (existence): %w", err)
	}
	erows.Close()

	// Thread membership excludes deleted messages, by the SAME definition
	// Email/query's default uses (jmap_email.go, #631): trashed_at set, or
	// effectively in a trash-role mailbox. Email/set already reported Mailroom's
	// own deletes to the client as `destroyed` (soft-delete, ADR-009), and the
	// list view never lists mail another client moved to Trash, so handing
	// either id back here would resurrect it — a reading pane renders the
	// thread's emailIds, and the client would display a message its own list
	// view excludes. A second, narrower definition here is exactly how #631's
	// folder-trashed mail would have come back: out of every list, into the pane.
	//
	// The membership test is mailboxMembershipSQL itself, not a copy, so a
	// message restored in Mailroom (moved out of Trash, provider copy still in
	// Trash) stays in its thread just as it stays in the list.
	//
	// This is a deliberate deviation from strict RFC 8621, where a Thread may
	// contain messages in Trash. The Trash view is unaffected: it reaches those
	// messages through Email/query on the trash-role mailbox and Email/get by id,
	// neither of which is filtered here.
	trash, err := tx.trashRoleMailboxIDs(ctx)
	if err != nil {
		return nil, err
	}
	args := []any{tx.userID.String(), idStrs}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	folderTrashed := ""
	if len(trash) > 0 {
		folderTrashed = "\n\t\t   AND NOT " + inAnyMailboxSQL(bind, tx.userID, trash)
	}
	members := make(map[ulid.ULID][]ulid.ULID, len(existing))
	mrows, err := tx.tx.Query(ctx, `
		SELECT m.thread_id, m.id
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.thread_id = ANY($2)
		   AND m.trashed_at IS NULL`+folderTrashed+`
		 ORDER BY m.thread_id, m.received_at, m.id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: get threads (members): %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var tid, mid string
		if err := mrows.Scan(&tid, &mid); err != nil {
			return nil, fmt.Errorf("store: scan thread member: %w", err)
		}
		tu, err := ulid.Parse(tid)
		if err != nil {
			return nil, fmt.Errorf("store: parse thread id %q: %w", tid, err)
		}
		mu, err := ulid.Parse(mid)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", mid, err)
		}
		members[tu] = append(members[tu], mu)
		existing[tu] = true // a message's thread implicitly exists
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("store: get threads (members): %w", err)
	}

	// A thread with no VISIBLE messages is not reported at all — it lands in the
	// caller's notFound. Every message in it is deleted (either way, above), so
	// no list view shows any of them; handing back an existing-but-empty Thread would
	// be incoherent (RFC 8621 threads are defined by their emailIds) and would
	// leave a client rendering an empty reading pane.
	out := make([]ThreadView, 0, len(existing))
	for tid := range existing {
		if len(members[tid]) == 0 {
			continue
		}
		out = append(out, ThreadView{ID: tid, EmailIDs: members[tid], Muted: muted[tid]})
	}
	return out, nil
}

// SetThreadMuted sets or clears a thread's mute (spec §6, #51) and reports
// whether anything actually changed.
//
// The effect is FORWARD-ONLY in both directions: muting archives replies that
// arrive from now on (DivertMutedArrival) and moves nothing already here, and
// unmuting lets the next reply surface while leaving what mute already archived
// in Archive. Neither is an undo of a placement.
//
// Mute is JMAP-visible state, so a real change appends a Thread change_log row
// IN THE SAME TRANSACTION — the load-bearing invariant (CLAUDE.md). It is the
// Thread object type, not Email: the mute belongs to the conversation, and a
// client syncing it wants one Thread update, not one Email update per member.
//
// A no-op write appends NOTHING. Re-muting an already-muted thread must not
// churn the change log: every appended row is a wake-up for the indexer and
// every client cursor, and a client that repeats a set (retry, double-click,
// two devices agreeing) would otherwise generate sync traffic that carries no
// information. The `IS DISTINCT FROM` guard is what makes the write idempotent,
// matching UpdateMessageFlagsIfChanged.
//
// A thread id that does not exist — or belongs to another user, which RLS makes
// indistinguishable — changes nothing and returns false, so the caller reports
// it as notFound rather than silently succeeding.
func (tx *UserTx) SetThreadMuted(ctx context.Context, threadID ulid.ULID, muted bool) (bool, error) {
	var at *time.Time
	if muted {
		now := time.Now().UTC()
		at = &now
	}
	var id string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.threads
		   SET muted_at = $3
		 WHERE user_id = $1 AND id = $2
		   AND (muted_at IS NOT NULL) IS DISTINCT FROM $4
		RETURNING id`,
		tx.userID.String(), threadID.String(), at, muted).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: set thread muted: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectThread, threadID, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}
