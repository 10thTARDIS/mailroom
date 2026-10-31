// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// DAY-ONE ARCHIVES, ON THE SERVER (#934).
//
// Day-one setup (#446, setup.go) takes mail older than its window off the desk
// by setting desk_archived_at and NOTHING ELSE: no writeback intent, so every
// other client — the provider's webmail, a phone — still shows that mail in
// INBOX. That was the promise ("nothing moves on the server"), and it stays the
// promise of the wizard. This file is the operator's explicit, after-the-fact
// opt-out of it: for mail day-one archived that is STILL in the server's INBOX,
// make the archive real, exactly as if the user had pressed `e` on it.
//
// "Exactly as if" is literal: the apply goes through ApplyEmailUpdate with
// MoveToRole "archive" — the path the drawer sweep (ArchiveByDrawer) and every
// client archive take — so the canonical placement, the per-account move
// intent and the one Email change_log row all come from the one implementation
// (the load-bearing invariant). The only difference is attribution: the intent
// carries source 'setup', because no person pressed anything, and learned
// placement does not read it as the user dealing with a placement (see the
// LabelBySetup arm beside keepLearnedOnArchive in ApplyEmailUpdate).
//
// ⚠ A MOVE INTENT MOVES EVERY LIVE COPY IN ITS ACCOUNT, not the INBOX copy
// (MessageProvenanceTargetsForAccount + applyToEveryCopy in the writeback
// engine), and ApplyEmailUpdate enqueues one for every account with a live
// copy. So "it has an INBOX copy" is not enough: a message that ALSO lives in
// a folder the user chose — filed there in webmail since day-one, or two
// copies from the start — would have that copy pulled into Archive too. A
// candidate therefore has live copies ONLY in INBOX, and only in accounts that
// can actually perform the move (dayOneMovableSQL). Everything else is
// counted, by reason, and left alone.
//
// ⚠ CONSEQUENCE, BY DESIGN: once a message has moved, undoing its setup run no
// longer returns it to other clients' inboxes. Setup undo clears
// desk_archived_at only; the canonical placement this sets (moved_mailbox_id)
// is the user-level archive, and it stays.

// dayOneStateSQL is the message-state half of the predicate. `m` is the
// message alias.
//
//   - desk_archived_at IS NOT NULL — day-one took it off the desk. Without this
//     every live INBOX message would qualify: the inbox the user has not dealt
//     with (the integration test's in-window messages).
//   - moved_mailbox_id IS NULL — nobody has placed it since. An answer the user
//     gave (archived, filed, brought back) is theirs.
//   - not trashed, vanished, a draft, junk or snoozed — each is a different
//     state with its own owner, and an archive would fight it.
//   - not flagged or pinned — a starred message in INBOX is very often the
//     user's to-do list in their other clients, and pinning is an explicit
//     "keep this in view". Day-one archived those without asking; moving them
//     out of the server's inbox would be a second, louder version of that.
func dayOneStateSQL() string {
	return `m.desk_archived_at IS NOT NULL
	          AND m.moved_mailbox_id IS NULL
	          AND m.trashed_at IS NULL AND m.vanished_at IS NULL
	          AND NOT m.is_draft AND NOT m.is_junk
	          AND m.snoozed_until IS NULL
	          AND NOT m.is_flagged AND m.pinned_at IS NULL`
}

// dayOneServerArchiveWhereSQL is the ONE definition of a candidate, shared by
// the report, the scan and the by-id re-check so the report's count is the
// apply's count (#155): the state above, a live INBOX copy, and NO live copy
// anywhere else.
func dayOneServerArchiveWhereSQL() string {
	return dayOneStateSQL() + `
	          AND ` + dayOneInInboxSQL() + `
	          AND NOT ` + dayOneOutsideInboxSQL()
}

// dayOneInInboxSQL: message m has a live copy in an INBOX mailbox.
func dayOneInInboxSQL() string {
	return `EXISTS (
	          SELECT 1 FROM app.message_provenance ip
	            JOIN app.mailboxes ib ON ib.id = ip.mailbox_id AND ib.user_id = ip.user_id
	           WHERE ip.user_id = m.user_id AND ip.message_id = m.id AND ip.state = 'live'
	             AND lower(ib.imap_name) = 'inbox')`
}

// dayOneOutsideInboxSQL: message m has a live copy in some mailbox that is
// not INBOX — the copy a move intent would also take.
func dayOneOutsideInboxSQL() string {
	return `EXISTS (
	          SELECT 1 FROM app.message_provenance op
	            JOIN app.mailboxes ob ON ob.id = op.mailbox_id AND ob.user_id = op.user_id
	           WHERE op.user_id = m.user_id AND op.message_id = m.id AND op.state = 'live'
	             AND lower(ob.imap_name) <> 'inbox')`
}

// dayOneMovableSQL: EVERY live copy of m is in an account in the text[]
// parameter `accounts` (the movable ones: an Archive folder AND writeback
// live). Every account, not any: ApplyEmailUpdate enqueues a move for each
// account with a live copy, and one that cannot perform it dead-letters.
//
// ⚠ The COALESCE is load-bearing. With an empty or NULL array,
// `account_id = ANY(NULL)` is NULL, and a bare NOT(NULL) would make the
// inner WHERE unknown, the NOT EXISTS true, and every message "movable" — the
// fail-open direction. COALESCE(…, false) makes "not in the list" true, so
// the inner row exists and the message is not movable.
func dayOneMovableSQL(accounts string) string {
	return `NOT EXISTS (
	          SELECT 1 FROM app.message_provenance vp
	           WHERE vp.user_id = m.user_id AND vp.message_id = m.id AND vp.state = 'live'
	             AND NOT COALESCE(vp.account_id = ANY(` + accounts + `::text[]), false))`
}

// DayOneServerArchiveAccount is one account's line of the report.
type DayOneServerArchiveAccount struct {
	AccountID ulid.ULID
	Name      string
	// ArchiveMailbox is the IMAP name a move would target ("" = none).
	ArchiveMailbox string
	// WritebackLive is whether a move enqueued now would be performed: the
	// account's writeback_enabled, the Gmail writeback hold and the instance
	// ingest-only switch, exactly as EnqueueIntent decides it. When false, an
	// intent is born 'suppressed', which is terminal and is NOT replayed by
	// turning writeback back on (ReplaySuppressedIntents is explicit), so such
	// an account is not movable at all. WritebackOff says why.
	WritebackLive bool
	WritebackOff  string
	// Gmail marks a Gmail OAuth account: its inbox is the \Inbox label on All
	// Mail (ADR-047), not an INBOX mailbox, so nothing in it can qualify here
	// and its day-one archives are NOT ASSESSED by this command.
	Gmail bool
	// Candidates are candidates with a live copy in THIS account.
	Candidates int
}

// Movable reports whether a move into this account's Archive would be both
// resolvable and performed.
func (a DayOneServerArchiveAccount) Movable() bool { return a.ArchiveMailbox != "" && a.WritebackLive }

// DayOneServerArchiveReport is the read-only report. Every count is its own
// signal, never a sum, so "nothing to do" and "the thing measured is always
// empty" stay distinguishable (CLAUDE.md, measurement discipline).
type DayOneServerArchiveReport struct {
	// DeskArchived is every message day-one took off the desk that is still
	// off it (desk_archived_at set), wherever it lives.
	DeskArchived int
	// InInbox is the subset with a live INBOX copy on the server. The rest
	// have no INBOX copy: their copies are in other folders, or they have no
	// live copy at all; either way this command never touches them.
	InInbox int
	// Of InInbox, why a message is NOT a candidate. The reasons overlap; each
	// is counted on its own.
	PlacedSince  int // moved_mailbox_id set: the user (or a sweep, or this command) placed it since
	Trashed      int
	Snoozed      int
	Flagged      int
	Pinned       int
	DraftOrJunk  int
	Vanished     int
	OutsideInbox int // also has a live copy in another folder, which a move would take too
	// Candidates is distinct messages that qualify.
	Candidates int
	// Movable is the subset whose every live copy is in a movable account —
	// exactly what --apply moves. Candidates - Movable is blocked.
	Movable  int
	Accounts []DayOneServerArchiveAccount
	// SetupMoves counts this user's move intents with source 'setup' by
	// state — the follow-up: "applied" is done, "pending"/"inflight" is in
	// progress, and "dead"/"suppressed" are moves Mailroom shows as archived
	// while the server still has the message in INBOX (a rerun cannot see
	// those: the message is "placed since" now).
	SetupMoves map[string]int
}

// DayOneServerArchiveReport counts, writes nothing.
func (tx *UserTx) DayOneServerArchiveReport(ctx context.Context) (DayOneServerArchiveReport, error) {
	r := DayOneServerArchiveReport{SetupMoves: map[string]int{}}
	in := dayOneInInboxSQL()
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE `+in+`),
		       count(*) FILTER (WHERE `+in+` AND m.moved_mailbox_id IS NOT NULL),
		       count(*) FILTER (WHERE `+in+` AND m.trashed_at IS NOT NULL),
		       count(*) FILTER (WHERE `+in+` AND m.snoozed_until IS NOT NULL),
		       count(*) FILTER (WHERE `+in+` AND m.is_flagged),
		       count(*) FILTER (WHERE `+in+` AND m.pinned_at IS NOT NULL),
		       count(*) FILTER (WHERE `+in+` AND (m.is_draft OR m.is_junk)),
		       count(*) FILTER (WHERE `+in+` AND m.vanished_at IS NOT NULL),
		       count(*) FILTER (WHERE `+in+` AND `+dayOneOutsideInboxSQL()+`),
		       count(*) FILTER (WHERE `+dayOneServerArchiveWhereSQL()+`)
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.desk_archived_at IS NOT NULL`,
		tx.userID.String()).Scan(&r.DeskArchived, &r.InInbox, &r.PlacedSince, &r.Trashed,
		&r.Snoozed, &r.Flagged, &r.Pinned, &r.DraftOrJunk, &r.Vanished, &r.OutsideInbox,
		&r.Candidates); err != nil {
		return r, fmt.Errorf("store: day-one server archive report: %w", err)
	}

	accts, err := tx.dayOneArchiveAccounts(ctx)
	if err != nil {
		return r, err
	}
	for i := range accts {
		a := &accts[i]
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM app.messages m
			 WHERE m.user_id = $1 AND `+dayOneServerArchiveWhereSQL()+`
			   AND EXISTS (SELECT 1 FROM app.message_provenance ap
			                WHERE ap.user_id = m.user_id AND ap.message_id = m.id
			                  AND ap.state = 'live' AND ap.account_id = $2)`,
			tx.userID.String(), a.AccountID.String()).Scan(&a.Candidates); err != nil {
			return r, fmt.Errorf("store: day-one server archive report (account): %w", err)
		}
	}
	r.Accounts = accts
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FROM app.messages m
		 WHERE m.user_id = $1 AND `+dayOneServerArchiveWhereSQL()+`
		   AND `+dayOneMovableSQL("$2"),
		tx.userID.String(), movableAccountIDs(accts)).Scan(&r.Movable); err != nil {
		return r, fmt.Errorf("store: day-one server archive report (movable): %w", err)
	}

	rows, err := tx.tx.Query(ctx, `
		SELECT state::text, count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'move' AND source = $2
		 GROUP BY state`, tx.userID.String(), LabelBySetup)
	if err != nil {
		return r, fmt.Errorf("store: day-one server archive report (intents): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return r, fmt.Errorf("store: scan day-one server archive intents: %w", err)
		}
		r.SetupMoves[state] = n
	}
	return r, rows.Err()
}

// movableAccountIDs is the text[] parameter dayOneMovableSQL takes.
func movableAccountIDs(accts []DayOneServerArchiveAccount) []string {
	out := []string{}
	for _, a := range accts {
		if a.Movable() {
			out = append(out, a.AccountID.String())
		}
	}
	return out
}

// dayOneArchiveAccounts lists the user's accounts with the Archive mailbox a
// move would resolve to — through MailboxForRole, the resolver
// ApplyEmailUpdate's move uses — and whether writeback is live there, by the
// same three terms EnqueueIntent ANDs (writeback.go).
func (tx *UserTx) dayOneArchiveAccounts(ctx context.Context) ([]DayOneServerArchiveAccount, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT a.id, a.name, a.writeback_enabled, `+gmailWritebackHeldSQL+`,
		       (a.auth_kind = 'oauth2' AND a.provider_profile = 'gmail')
		  FROM app.accounts a
		 WHERE a.user_id = $1 ORDER BY a.name, a.id`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: day-one server archive accounts: %w", err)
	}
	var out []DayOneServerArchiveAccount
	for rows.Next() {
		var idStr string
		var enabled, gmailHeld bool
		var a DayOneServerArchiveAccount
		if err := rows.Scan(&idStr, &a.Name, &enabled, &gmailHeld, &a.Gmail); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan day-one server archive account: %w", err)
		}
		if a.AccountID, err = ulid.Parse(idStr); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: parse account id %q: %w", idStr, err)
		}
		switch {
		case tx.writebackDisabled:
			a.WritebackOff = "instance is ingest-only"
		case !enabled:
			a.WritebackOff = "account writeback is off"
		case gmailHeld:
			a.WritebackOff = "Gmail writeback is not switched on yet"
		default:
			a.WritebackLive = true
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: day-one server archive accounts: %w", err)
	}
	rows.Close()
	for i := range out {
		mb, found, err := tx.MailboxForRole(ctx, out[i].AccountID, "archive")
		if err != nil {
			return nil, err
		}
		if found {
			out[i].ArchiveMailbox = mb.IMAPName
		}
	}
	return out, nil
}

// DayOneServerArchiveCandidates returns up to limit MOVABLE candidates, oldest
// received_at first. Filtering on movability HERE, not only in the apply, is
// what stops a batch of unmovable messages starving the movable ones behind
// them (the reasoning DrawerCandidates gives for the same clause).
func (tx *UserTx) DayOneServerArchiveCandidates(ctx context.Context, limit int) ([]ulid.ULID, error) {
	accts, err := tx.dayOneArchiveAccounts(ctx)
	if err != nil {
		return nil, err
	}
	movable := movableAccountIDs(accts)
	if len(movable) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id FROM app.messages m
		 WHERE m.user_id = $1 AND `+dayOneServerArchiveWhereSQL()+`
		   AND `+dayOneMovableSQL("$2")+`
		 ORDER BY m.received_at, m.id
		 LIMIT $3`,
		tx.userID.String(), movable, limit)
	if err != nil {
		return nil, fmt.Errorf("store: day-one server archive candidates: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			return nil, fmt.Errorf("store: scan day-one server archive candidate: %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse candidate id %q: %w", idStr, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ArchiveDayOneOnServer archives one candidate for real: the canonical
// placement, a 'setup'-sourced move intent per live account and one Email
// change row, through ApplyEmailUpdate. It re-checks the WHOLE predicate,
// movability included, BY ID under a row lock first, because the user may
// have acted on the message (or a sync may have seen a new copy of it)
// between the scan and this write, and an answer they gave must win. Returns
// false (and writes nothing) when the message no longer qualifies.
func (tx *UserTx) ArchiveDayOneOnServer(ctx context.Context, id ulid.ULID) (bool, error) {
	accts, err := tx.dayOneArchiveAccounts(ctx)
	if err != nil {
		return false, err
	}
	var locked string
	err = tx.tx.QueryRow(ctx, `
		SELECT m.id FROM app.messages m
		 WHERE m.user_id = $1 AND m.id = $2 AND `+dayOneServerArchiveWhereSQL()+`
		   AND `+dayOneMovableSQL("$3")+`
		   FOR UPDATE OF m`,
		tx.userID.String(), id.String(), movableAccountIDs(accts)).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: day-one server archive (re-check): %w", err)
	}
	found, _, err := tx.ApplyEmailUpdate(ctx, id, EmailUpdate{MoveToRole: "archive", LabelSource: SetupLabelSource()})
	if err != nil {
		return false, err
	}
	return found, nil
}

// ArchiveDayOneOnServerRun drives the apply: transactions of up to batch
// candidates until none are left or limit (0 = no limit) have moved. It
// returns how many moved, and how many the scan offered that then failed
// their re-check. A transaction that moved nothing ends the run — either
// nothing was left, or every offered message stopped qualifying and the next
// scan would offer the same ones. An error rolls back only the current
// transaction; `moved` still counts the ones already committed.
func (s *Store) ArchiveDayOneOnServerRun(ctx context.Context, userID ulid.ULID, limit, batch int) (moved, skipped int, err error) {
	if batch <= 0 {
		return 0, 0, fmt.Errorf("store: day-one server archive: batch must be positive")
	}
	for limit <= 0 || moved < limit {
		want := batch
		if limit > 0 && limit-moved < want {
			want = limit - moved
		}
		var chunkMoved, chunkSeen int
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			ids, e := tx.DayOneServerArchiveCandidates(ctx, want)
			if e != nil {
				return e
			}
			chunkSeen = len(ids)
			for _, id := range ids {
				ok, e := tx.ArchiveDayOneOnServer(ctx, id)
				if e != nil {
					return fmt.Errorf("message %s: %w", id, e)
				}
				if ok {
					chunkMoved++
				}
			}
			return nil
		}); err != nil {
			return moved, skipped, err
		}
		moved += chunkMoved
		skipped += chunkSeen - chunkMoved
		if chunkSeen == 0 || chunkMoved == 0 {
			break
		}
	}
	return moved, skipped, nil
}
