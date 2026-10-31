// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file holds the store surface for live single-account IMAP sync
// (spec §4/§16, issue #10): discovering mailboxes, tracking per-mailbox sync
// cursors, and retiring a stale UIDVALIDITY generation.
//
// Change-log discipline (the load-bearing invariant — CLAUDE.md): a Mailbox
// is a JMAP-visible object, so UpsertMailbox appends a Mailbox change_log row
// when it creates a mailbox or alters a JMAP-visible attribute. The sync
// cursors (uidvalidity, last_seen_uid, highest_modseq) and the PERMANENTFLAGS
// probe (accepts_keywords) are internal sync bookkeeping, NOT JMAP-visible
// Mailbox properties, so UpdateMailboxCursors writes them WITHOUT a change_log
// row — otherwise every poll would spam the client change stream.
//
// message_provenance is not itself a JMAP-visible object, but a message's
// mailboxIds are DERIVED from its LIVE provenance (jmap_email.go
// effectiveMembershipSQL, which filters state='live'). So retiring a live row
// to 'stale_uidvalidity' can change a JMAP-visible property the instant it
// commits — which is exactly the #94 bug MarkMailboxProvenanceStale used to
// carry: it dropped every message out of the rolled folder with no change_log
// row, so a client polling mid-roll saw a briefly-empty folder and /changes and
// the indexer were never told. It now appends one Email `updated` row per
// affected message, in the same transaction as the retirement (see the method).

// MailboxUpsert is the discoverable metadata for one IMAP mailbox, as learned
// from an IMAP LIST (spec §4). It carries only server-discovered attributes;
// sync_scope is an operator/policy setting UpsertMailbox never overwrites, and
// the sync cursors are owned by UpdateMailboxCursors.
type MailboxUpsert struct {
	AccountID   ulid.ULID
	IMAPName    string // raw IMAP mailbox name (UTF-7 decoded upstream)
	DisplayName string
	SpecialUse  string // RFC 6154 attribute (e.g. `\Sent`), or "" if none
}

// MailboxState is a mailbox's persisted state after an upsert: its id, the
// (preserved) sync scope, and the current sync cursors. On a freshly created
// mailbox the cursors are all zero.
type MailboxState struct {
	ID ulid.ULID
	// Created is true when THIS upsert inserted the row (the folder was seen
	// for the first time). The Microsoft profile decides whether a non-mail
	// folder is synced only then (ADR-046 §12), never on a later pass.
	Created       bool
	SyncScope     string // 'full' | 'headers' | 'off'
	UIDValidity   int64
	LastSeenUID   int64
	HighestModSeq int64
	// LastReapedAt is when the expunge reaper last diffed this mailbox's live UID
	// set against its provenance (reaper.go). nil = never. It is the reaper's
	// cadence cursor: persisted rather than held in memory so the (slow, hourly-
	// ish) cadence survives a restart and spreads itself across an account's
	// mailboxes instead of reaping all of them in the same pass.
	LastReapedAt *time.Time

	// PollUIDNext and PollMessages are UIDNEXT and MESSAGES as the last poll's
	// own SELECT reported them (migration 00058, #620); nil = unknown (never
	// recorded, or the SELECT carried no UIDNEXT). Together with UIDValidity and
	// HighestModSeq they are the status the poll last reconciled against.
	PollUIDNext  *int64
	PollMessages *int64

	// Observed is the status the last REAL live-UID observation saw, nil when
	// none is recorded; LastObservedFullAt is when that observation ran. Both
	// are written only by StampMailboxReaped — never by a skipped observation
	// (StampMailboxUnchanged) — see migration 00058 for why the observation,
	// not the poll, is the baseline.
	Observed           *ObservedStatus
	LastObservedFullAt *time.Time

	// BackfillFloorUID is the Gmail newest-first import's floor (#52,
	// migration 00079): every UID from it up to LastSeenUID has been fetched,
	// history below it has not. Nil = no floor (oldest-first; every non-Gmail
	// mailbox). LabelsSweptAt is when the X-GM-LABELS sweep last read the
	// mailbox; nil = never.
	BackfillFloorUID *int64
	LabelsSweptAt    *time.Time
	// LabelSweepNext is where an interrupted label sweep resumes; nil = none
	// in progress.
	LabelSweepNext *int64
}

// ObservedStatus is the (UIDVALIDITY, UIDNEXT, MESSAGES, HIGHESTMODSEQ) tuple a
// live-UID observation saw (#620). It is the baseline a later LIST-STATUS is
// compared against to decide that the next observation cannot find anything
// new and may be skipped.
type ObservedStatus struct {
	UIDValidity   int64
	UIDNext       int64
	Messages      int64
	HighestModSeq int64
}

// UpsertMailbox records a discovered mailbox and returns its persisted state,
// including the sync cursors the engine reconciles against. On first sight it
// inserts the row (cursors default to zero) and appends a Mailbox `created`
// change; on a repeat it refreshes only the JMAP-visible discovered attributes
// (display_name, special_use) — never sync_scope or the cursors — and appends
// a Mailbox `updated` change only when one of those attributes actually
// changed. The returned cursors are read from the pre-upsert row, so the
// caller need not issue a separate read.
func (tx *UserTx) UpsertMailbox(ctx context.Context, m MailboxUpsert) (MailboxState, error) {
	var (
		st                                           MailboxState
		id                                           string
		created, changed                             bool
		obsValidity, obsNext, obsMessages, obsModSeq *int64
	)
	// The `existing` CTE reads the pre-upsert snapshot (a data-modifying WITH
	// cannot see its own effects), so it yields the OLD display_name/
	// special_use for change detection; `upsert` performs the write.
	err := tx.tx.QueryRow(ctx, `
		WITH existing AS (
			SELECT id, display_name, special_use
			  FROM app.mailboxes
			 WHERE user_id = $2 AND account_id = $3 AND imap_name = $4
		),
		upsert AS (
			INSERT INTO app.mailboxes
				(id, user_id, account_id, imap_name, display_name, special_use)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (account_id, imap_name) DO UPDATE
				SET display_name = EXCLUDED.display_name,
				    special_use  = EXCLUDED.special_use
			RETURNING id, sync_scope, uidvalidity, last_seen_uid, highest_modseq,
			          last_reaped_at, (xmax = 0) AS created,
			          poll_uidnext, poll_messages,
			          observed_uidvalidity, observed_uidnext, observed_messages,
			          observed_highest_modseq, last_observed_full_at,
			          backfill_floor_uid, labels_swept_at, label_sweep_next_uid
		)
		SELECT u.id, u.sync_scope, u.uidvalidity, u.last_seen_uid, u.highest_modseq,
		       u.last_reaped_at, u.created,
		       (NOT u.created
		        AND ( e.display_name IS DISTINCT FROM $5
		           OR e.special_use  IS DISTINCT FROM $6::text )) AS changed,
		       u.poll_uidnext, u.poll_messages,
		       u.observed_uidvalidity, u.observed_uidnext, u.observed_messages,
		       u.observed_highest_modseq, u.last_observed_full_at,
		       u.backfill_floor_uid, u.labels_swept_at, u.label_sweep_next_uid
		  FROM upsert u
		  LEFT JOIN existing e ON e.id = u.id`,
		NewULID().String(), tx.userID.String(), m.AccountID.String(),
		m.IMAPName, m.DisplayName, nullStr(m.SpecialUse),
	).Scan(&id, &st.SyncScope, &st.UIDValidity, &st.LastSeenUID, &st.HighestModSeq,
		&st.LastReapedAt, &created, &changed,
		&st.PollUIDNext, &st.PollMessages,
		&obsValidity, &obsNext, &obsMessages, &obsModSeq, &st.LastObservedFullAt,
		&st.BackfillFloorUID, &st.LabelsSweptAt, &st.LabelSweepNext)
	if err != nil {
		return MailboxState{}, fmt.Errorf("store: upsert mailbox: %w", err)
	}
	// All four or nothing: a baseline missing any one number cannot be compared,
	// and "unknown" must read as unknown rather than as a zero that might match.
	if obsValidity != nil && obsNext != nil && obsMessages != nil && obsModSeq != nil {
		st.Observed = &ObservedStatus{
			UIDValidity: *obsValidity, UIDNext: *obsNext,
			Messages: *obsMessages, HighestModSeq: *obsModSeq,
		}
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return MailboxState{}, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
	}
	st.ID = u
	st.Created = created

	switch {
	case created:
		if _, err := tx.AppendChange(ctx, ObjectMailbox, u, OpCreated); err != nil {
			return MailboxState{}, err
		}
	case changed:
		if _, err := tx.AppendChange(ctx, ObjectMailbox, u, OpUpdated); err != nil {
			return MailboxState{}, err
		}
	}
	return st, nil
}

// SyncScope values (#62). The set is closed and the database enforces it
// (mailboxes_sync_scope_known, migration 00026): a value the engine does not
// implement must not be storable, because the engine tests for 'off' and treats
// everything else as a full sync — so an unrecognised value silently means its
// opposite.
const (
	// SyncScopeFull polls, verifies and reaps the mailbox: the default.
	SyncScopeFull = "full"
	// SyncScopeOff stops looking at the mailbox entirely. It is skipped before
	// SELECT, so it is never a reap candidate either and its already-synced mail
	// is FROZEN rather than deleted.
	SyncScopeOff = "off"
)

// ErrUnknownSyncScope is returned for a value outside the closed set above,
// before any write is attempted.
var ErrUnknownSyncScope = errors.New("store: unknown sync scope")

// ErrMailboxNotFound is returned when a mailbox id does not resolve for the
// caller. RLS makes another user's mailbox indistinguishable from a missing one,
// which is the intended behaviour: the API reports both as `notFound`.
var ErrMailboxNotFound = errors.New("store: mailbox not found")

// SetMailboxSyncScope turns a mailbox's syncing on or off (#62).
//
// It appends a Mailbox `updated` change row in the same transaction — the
// load-bearing invariant, and here it is also load-bearing for the FEATURE: the
// client renders a frozen folder differently from a live one, so a scope change
// that did not reach `/changes` would leave every other device showing a snapshot
// as though it were current mail.
//
// IT MOVES NO MESSAGES, deliberately. Turning a folder off is a preference, and a
// preference must not destroy mail; the messages stay exactly where they are and
// simply stop being updated. Nothing concludes anything about them either — an
// 'off' mailbox is skipped before SELECT (internal/mailsync/engine.go) and so
// never becomes a reap target, which is what keeps "we are not looking" from
// becoming "the mailbox is empty".
//
// changed is false when the mailbox already had that scope; no change row is
// appended in that case, so a client re-asserting a scope does not churn every
// other device's sync cursor.
func (tx *UserTx) SetMailboxSyncScope(ctx context.Context, id ulid.ULID, scope string) (changed bool, err error) {
	if scope != SyncScopeFull && scope != SyncScopeOff {
		return false, fmt.Errorf("%w: %q", ErrUnknownSyncScope, scope)
	}
	// The `existing` CTE reads the PRE-update value — a data-modifying WITH cannot
	// see its own effects — which is the same shape UpsertMailbox uses for change
	// detection, and the reason this is one round trip rather than a read-then-write
	// race.
	var before string
	e := tx.tx.QueryRow(ctx, `
		WITH existing AS (
			SELECT id, sync_scope FROM app.mailboxes
			 WHERE user_id = $1 AND id = $2
		),
		upd AS (
			UPDATE app.mailboxes SET sync_scope = $3
			 WHERE user_id = $1 AND id = $2
			 RETURNING id
		)
		SELECT e.sync_scope FROM upd u JOIN existing e ON e.id = u.id`,
		tx.userID.String(), id.String(), scope).Scan(&before)
	if e != nil {
		if isNoRows(e) {
			// RLS makes another user's mailbox indistinguishable from a missing one.
			return false, ErrMailboxNotFound
		}
		return false, fmt.Errorf("store: set mailbox sync scope: %w", e)
	}
	if before == scope {
		return false, nil
	}
	if _, err := tx.AppendChange(ctx, ObjectMailbox, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// SetMailboxFeedsDesk flags whether mail with live provenance in this mailbox
// counts as unactioned exactly like INBOX — "folders feed the desk" (#442,
// #446) — for AreaSummary's own live-mail predicate (areas_resolve.go). It
// mirrors SetMailboxSyncScope: a Mailbox property, so it appends the Mailbox
// `updated` change row in the same transaction. Without that row a client's
// desk counts would silently disagree with what the server is now counting
// until something else happened to touch the mailbox.
//
// changed is false when the mailbox already had that value; no change row is
// appended in that case, for the same reason SetMailboxSyncScope gives.
func (tx *UserTx) SetMailboxFeedsDesk(ctx context.Context, id ulid.ULID, feeds bool) (changed bool, err error) {
	var before bool
	e := tx.tx.QueryRow(ctx, `
		WITH existing AS (
			SELECT id, feeds_desk FROM app.mailboxes
			 WHERE user_id = $1 AND id = $2
		),
		upd AS (
			UPDATE app.mailboxes SET feeds_desk = $3
			 WHERE user_id = $1 AND id = $2
			 RETURNING id
		)
		SELECT e.feeds_desk FROM upd u JOIN existing e ON e.id = u.id`,
		tx.userID.String(), id.String(), feeds).Scan(&before)
	if e != nil {
		if isNoRows(e) {
			return false, ErrMailboxNotFound
		}
		return false, fmt.Errorf("store: set mailbox feeds desk: %w", e)
	}
	if before == feeds {
		return false, nil
	}
	if _, err := tx.AppendChange(ctx, ObjectMailbox, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// MailboxCursors is the sync-derived state persisted after selecting a mailbox
// and reconciling it. Every field is internal sync bookkeeping (not a
// JMAP-visible Mailbox property), so UpdateMailboxCursors writes no change_log
// row.
type MailboxCursors struct {
	UIDValidity   int64
	LastSeenUID   int64
	HighestModSeq int64
	// accepts_keywords is NOT here any more (#740): these cursors come from the
	// sync pass's EXAMINE, whose PERMANENTFLAGS is always empty, so carrying it
	// wrote false over every mailbox on every pass. RecordKeywordProbes is the
	// one writer now, fed from read-write SELECTs.

	// UIDNext and Messages are UIDNEXT and MESSAGES from the SAME SELECT the
	// cursors above were reconciled against (#620, migration 00058). A zero
	// UIDNext means the SELECT carried none and is stored as NULL (unknown), so
	// it can never compare equal to a LIST-STATUS UIDNEXT.
	UIDNext  int64
	Messages int64
}

// UpdateMailboxCursors advances a mailbox's sync cursors after a reconcile.
// It touches only sync bookkeeping columns and appends no change_log row (see
// the file header). A missing mailbox is an error: the caller upserts the
// mailbox before syncing it, so a zero-row update signals a bug or a mailbox
// that vanished mid-sync.
func (tx *UserTx) UpdateMailboxCursors(ctx context.Context, mailboxID ulid.ULID, c MailboxCursors) error {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.mailboxes
		   SET uidvalidity      = $3,
		       last_seen_uid    = $4,
		       highest_modseq   = $5,
		       poll_uidnext     = NULLIF($6::bigint, 0),
		       poll_messages    = $7
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String(),
		c.UIDValidity, c.LastSeenUID, c.HighestModSeq,
		c.UIDNext, c.Messages,
	)
	if err != nil {
		return fmt.Errorf("store: update mailbox cursors: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: update mailbox cursors: mailbox %s not found", mailboxID)
	}
	return nil
}

// MarkMailboxProvenanceStale retires a mailbox's live provenance generation
// after a UIDVALIDITY reset (spec §4): every live physical location in the
// mailbox becomes 'stale_uidvalidity', preserving history while the caller
// re-fetches the mailbox under the new UIDVALIDITY. It returns the number of
// rows retired.
//
// It appends exactly one Email `updated` change_log row per affected message
// whose JMAP membership this retirement actually changes — the #94 fix (see the
// file header). "Actually changes" is the effectiveMembershipSQL rule made
// precise: a message with a canonical placement (moved_mailbox_id) reads its
// mailboxIds from that column, not provenance, so retiring its provenance is
// invisible and gets NO row; every other affected message loses this folder the
// instant the retirement commits and so gets one. This is deliberately the safe
// direction — it can over-notify in a pathological case (a draft whose
// local_mailbox_id is this same folder) but it can never DROP a message from a
// folder without telling a client. The re-fetch that follows re-ingests the
// survivors, and UpsertProvenance's revive path appends the Email row that puts
// each of them back — the accepted 2N change_log burst on a large roll.
func (tx *UserTx) MarkMailboxProvenanceStale(ctx context.Context, mailboxID ulid.ULID) (int64, error) {
	// One statement retires the rows AND reports, per retired row, whether the
	// owning message derives membership from provenance (moved_mailbox_id IS NULL)
	// — i.e. whether a client can see the change.
	rows, err := tx.tx.Query(ctx, `
		WITH retired AS (
			UPDATE app.message_provenance p
			   SET state = 'stale_uidvalidity'
			 WHERE p.user_id = $1 AND p.mailbox_id = $2 AND p.state = 'live'
			RETURNING p.message_id
		)
		SELECT r.message_id, (m.moved_mailbox_id IS NULL) AS visible
		  FROM retired r
		  JOIN app.messages m ON m.user_id = $1 AND m.id = r.message_id`,
		tx.userID.String(), mailboxID.String())
	if err != nil {
		return 0, fmt.Errorf("store: mark mailbox provenance stale: %w", err)
	}
	var retired int64
	changed := map[ulid.ULID]struct{}{}
	for rows.Next() {
		var id string
		var visible bool
		if err := rows.Scan(&id, &visible); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: mark mailbox provenance stale: scan: %w", err)
		}
		retired++
		if !visible {
			continue
		}
		u, err := ulid.Parse(id)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: mark mailbox provenance stale: parse message id %q: %w", id, err)
		}
		changed[u] = struct{}{} // a set: one row per message, even with two UIDs in the folder
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: mark mailbox provenance stale: %w", err)
	}

	for msgID := range changed {
		if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
			return 0, err
		}
	}
	return retired, nil
}

// AccountMailbox is one stored mailbox's identity, as ListAccountMailboxes
// reports it: the id the JMAP object and provenance rows are keyed by, and the
// raw IMAP name the server's LIST is diffed against (folder-deletion
// reconciliation, #87).
type AccountMailbox struct {
	ID       ulid.ULID
	IMAPName string
}

// ListAccountMailboxes returns every mailbox Mailroom currently holds for an
// account — the set the folder-deletion reconciler (mailsync, #87) diffs against
// the server's live LIST. A name we hold that a SUCCESSFUL LIST no longer reports
// is evidence the folder was deleted on the server. It is RLS-scoped, so it can
// only ever see the caller's own mailboxes.
func (tx *UserTx) ListAccountMailboxes(ctx context.Context, accountID ulid.ULID) ([]AccountMailbox, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, imap_name FROM app.mailboxes
		 WHERE user_id = $1 AND account_id = $2`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list account mailboxes: %w", err)
	}
	defer rows.Close()
	var out []AccountMailbox
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("store: scan account mailbox: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
		}
		out = append(out, AccountMailbox{ID: u, IMAPName: name})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list account mailboxes: %w", err)
	}
	return out, nil
}

// MailboxRoleRow is one stored mailbox as MailboxRolesForAccount reports it:
// the raw IMAP name, the RFC 6154 SPECIAL-USE attribute the server advertised
// (empty when it advertised none), and the JMAP role Mailroom derived from
// them.
type MailboxRoleRow struct {
	IMAPName   string
	SpecialUse string
	Role       string // "" when the mailbox has no role
}

// MailboxRolesForAccount reports every mailbox of an account together with the
// role Mailroom resolved for it, so an operator can see what a provider
// actually advertises BEFORE pointing real mail at it.
//
// This is the read side of a question that otherwise only gets answered by a
// user-visible failure. Roles are detected once, from SPECIAL-USE, at sync; a
// provider that advertises no \Drafts makes compose.Draft return
// ErrNoDraftsMailbox and a provider with no \Sent makes the Sent copy
// unfileable — and both surface as a broken action long after cutover, not as
// anything at onboarding.
//
// It deliberately returns EVERY mailbox rather than only the ones with a role.
// A missing role has two very different causes — the folder does not exist at
// all, or it exists under a name the server never tagged — and a list filtered
// to roles cannot tell them apart. The second is the common one, and it is
// fixable by tagging the folder; the first is not.
//
// ⚠ INBOX is role-carrying BY NAME, not by SPECIAL-USE (RFC 3501 §5.1 reserves
// the name; RFC 6154 defines no \Inbox), so it is derived through
// roleForMailbox like every other read path and will show Role "inbox" with an
// empty SpecialUse. That is correct, not a gap.
func (tx *UserTx) MailboxRolesForAccount(ctx context.Context, accountID ulid.ULID) ([]MailboxRoleRow, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT imap_name, COALESCE(special_use, '')
		  FROM app.mailboxes
		 WHERE user_id = $1 AND account_id = $2
		 ORDER BY imap_name`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return nil, fmt.Errorf("store: mailbox roles for account: %w", err)
	}
	defer rows.Close()
	var out []MailboxRoleRow
	for rows.Next() {
		var name, specialUse string
		if err := rows.Scan(&name, &specialUse); err != nil {
			return nil, fmt.Errorf("store: scan mailbox role: %w", err)
		}
		out = append(out, MailboxRoleRow{
			IMAPName:   name,
			SpecialUse: specialUse,
			Role:       roleForMailbox(name, specialUse),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: mailbox roles for account: %w", err)
	}
	return out, nil
}

// MailboxLiveCount is one stored mailbox's LIVE provenance count, as
// MailboxLiveCounts reports it.
type MailboxLiveCount struct {
	IMAPName string
	// Role is the JMAP role Mailroom resolved for the folder (roleForMailbox:
	// INBOX by name, everything else by SPECIAL-USE), "" for none.
	Role string
	// LiveProvenance counts live (mailbox, uidvalidity, uid) rows — the
	// server UIDs Mailroom believes the folder holds, which is the number a
	// server's STATUS MESSAGES is comparable to. It is NOT a count of distinct
	// messages: a re-delivered copy is a second UID in the folder (#206) and
	// the server counts it too.
	LiveProvenance int64
}

// MailboxLiveCounts returns every mailbox Mailroom holds for an account with
// its live provenance count, ordered by imap_name. It backs `mailroom-admin
// consolidate-folders` (#647), which prints this beside the server's STATUS so
// the operator can see whether Mailroom has caught up with a folder before
// that folder is moved or deleted on the provider.
//
// READ-ONLY, and RLS-scoped like every UserTx read: another user's account id
// returns no rows.
func (tx *UserTx) MailboxLiveCounts(ctx context.Context, accountID ulid.ULID) ([]MailboxLiveCount, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT mb.imap_name, COALESCE(mb.special_use, ''), count(mp.id)
		  FROM app.mailboxes mb
		  LEFT JOIN app.message_provenance mp
		         ON mp.user_id = mb.user_id AND mp.mailbox_id = mb.id AND mp.state = 'live'
		 WHERE mb.user_id = $1 AND mb.account_id = $2
		 GROUP BY mb.id, mb.imap_name, mb.special_use
		 ORDER BY mb.imap_name`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return nil, fmt.Errorf("store: mailbox live counts: %w", err)
	}
	defer rows.Close()
	var out []MailboxLiveCount
	for rows.Next() {
		var (
			name, specialUse string
			n                int64
		)
		if err := rows.Scan(&name, &specialUse, &n); err != nil {
			return nil, fmt.Errorf("store: scan mailbox live count: %w", err)
		}
		out = append(out, MailboxLiveCount{
			IMAPName:       name,
			Role:           roleForMailbox(name, specialUse),
			LiveProvenance: n,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: mailbox live counts: %w", err)
	}
	return out, nil
}

// RecordKeywordProbes stores what read-write SELECTs said about new keywords
// (app.mailboxes.accepts_keywords, #740): probes maps an account's IMAP mailbox
// name to whether that SELECT's PERMANENTFLAGS contained `\*`.
//
// THE ONLY WRITER OF THE COLUMN. Until #740 the sync pass wrote it from its
// EXAMINE, whose PERMANENTFLAGS a server always sends empty (nothing is
// permanent in a read-only session, RFC 9051 §6.3.3), so the column read false
// for every mailbox and the gate on it (#576's $NotJunk) never opened. Only a
// read-write SELECT can answer, the writeback session issues those anyway, and
// what it observed is recorded here — true AND false, because a read-write
// SELECT that leaves `\*` out is an honest no. A mailbox Mailroom has never
// written to keeps its default (false) until it has.
//
// Sync bookkeeping, not JMAP-visible: no change_log row. A name that matches no
// mailbox of this account is skipped (a folder deleted mid-pass). It reports
// how many rows changed.
func (tx *UserTx) RecordKeywordProbes(ctx context.Context, accountID ulid.ULID, probes map[string]bool) (int, error) {
	changed := 0
	for name, accepts := range probes {
		tag, err := tx.tx.Exec(ctx, `
			UPDATE app.mailboxes SET accepts_keywords = $4
			 WHERE user_id = $1 AND account_id = $2 AND imap_name = $3
			   AND accepts_keywords IS DISTINCT FROM $4`,
			tx.userID.String(), accountID.String(), name, accepts)
		if err != nil {
			return changed, fmt.Errorf("store: record keyword probe %q: %w", name, err)
		}
		changed += int(tag.RowsAffected())
	}
	return changed, nil
}
