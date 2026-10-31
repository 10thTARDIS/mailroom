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

	"mailroom/internal/gmaillabel"
)

// THE GMAIL READ PATH'S STORE SURFACE (#52, ADR-047, increment 5a).
//
// A live Gmail OAuth account syncs only [Gmail]/All Mail, Spam and Trash, and
// reads each message's labels from X-GM-LABELS. This file turns one such
// reading into Mailroom's model — folder placement, Mailroom labels — and
// keeps the bookkeeping the newest-first, byte-budgeted import needs (the
// backfill floor, the label-sweep stamp, the daily download meter).
//
// # Gmail's CHANGES are applied, not Gmail's state
//
// Each physical copy remembers the label set Gmail last reported
// (message_provenance.gm_labels). An observation is diffed against it, and
// only the difference is applied: a label Gmail added is added, a label Gmail
// removed is removed, and the folder moves only when the label set's folder
// changed. A label the user put on the message in Mailroom, or a move they
// made, is therefore not undone by the next sweep re-reading an unchanged
// Gmail — and a change made in Gmail's own apps still arrives.
//
// # Attribution: a Gmail label is the user's own filing ('user')
//
// ADR-047 §2a. A user label on a Gmail message was put there by the person —
// by hand in Gmail's apps, or by a Gmail filter they wrote — so it is applied
// with applied_by = 'user', the same attribution the Takeout importer gives
// the same labels (internal/importer, zero LabelSource). That makes it
// evidence for what learns from the user's filings (learned placement, the
// label model, rule induction), which is the point: years of the person's own
// sorting. It is NOT the #169 fixed point that keeps 'rule'/'desk'/'learned'
// out of the evidence, because nothing Mailroom decided is being fed back to
// itself. Gmail's own automatic signals are never imported at all: \Important,
// the category tabs, and every other system label are dropped
// (gmaillabel.Classify), and \Starred arrives as the \Flagged flag.
//
// The record of WHICH labels came from Gmail is not lost by sharing the
// attribution: gm_labels on the provenance row says exactly what Gmail
// reported for the copy, so a later decision to weigh Gmail filings
// differently is a join, not an unrecoverable mix.
//
// # Folder placement is a canonical placement
//
// The folder Gmail's labels imply (\Inbox -> INBOX, \Sent -> Sent, \Draft ->
// Drafts) is not where the bytes physically are — they are in All Mail — so it
// is written as app.messages.moved_mailbox_id, the canonical placement that
// overrides provenance for JMAP membership (effectiveMembershipSQL). Archived
// mail (no folder-like label) keeps no placement and is a member of All Mail
// through its provenance. RetireCanonicalPlacements never clears an INBOX
// placement here, because the message has no live provenance IN the INBOX —
// which is exactly its guard.
//
// No writeback intent is ever enqueued from this path: a change Gmail
// reported is not a change to push back to Gmail.

// GmailWritebackReady mirrors oauthmail.GmailWritebackReady (ADR-047 §3,
// ADR-028): while it is false a Gmail OAuth account is INGEST-ONLY, forced
// HERE, at the one place an intent is born, whatever its writeback_enabled
// column says — every intent for it is 'suppressed' — and the writeback
// runner never dials it (GmailWritebackHeld). Increment 5b built the
// writeback (internal/writeback/gmail.go) and left both FALSE; a
// live test flips both. (oauthmail imports store, so the switch cannot live
// in only one of them; oauthmail's TestGmailWritebackSwitchesAgree checks
// they agree.)
const GmailWritebackReady = false

// gmailWritebackHeldSQL is the predicate, over an app.accounts row aliased
// `a`, that holds a Gmail OAuth account's writeback off.
var gmailWritebackHeldSQL = func() string {
	if GmailWritebackReady {
		return `false`
	}
	return `(a.auth_kind = 'oauth2' AND a.provider_profile = 'gmail')`
}()

// GmailObservation is one X-GM-EXT-1 reading of a physical copy.
type GmailObservation struct {
	MailboxID   ulid.ULID
	UIDValidity int64
	UID         int64
	MsgID       uint64
	ThrID       uint64
	// Labels is the decoded X-GM-LABELS list; never nil for a real reading.
	Labels []string
}

// GmailApplyResult says what one observation changed.
type GmailApplyResult struct {
	Found         bool // a live provenance row existed at the coordinate
	FirstSeen     bool // no earlier label set was recorded for the copy
	LabelsAdded   int  // message_labels rows inserted
	LabelsRemoved int  // message_labels rows deleted
	LabelsCreated int  // Mailroom labels created for a Gmail label first seen
	FolderChanged bool // moved_mailbox_id changed
	Changed       bool // something JMAP-visible changed (an Email change_log row was appended)
}

// gmailFolderSQL finds the account's mailbox for a gmaillabel folder role.
func gmailFolderClause(folder string) (string, bool) {
	switch folder {
	case gmaillabel.FolderInbox:
		return `upper(imap_name) = 'INBOX'`, true
	case gmaillabel.FolderSent:
		return `special_use = '\Sent'`, true
	case gmaillabel.FolderDrafts:
		return `special_use = '\Drafts'`, true
	}
	return "", false
}

// ErrGmailNoReading is ApplyGmailObservation's refusal of an observation with
// nil Labels: no X-GM-LABELS reading at all, which must never be read as an
// empty label set (#52 review H1).
var ErrGmailNoReading = errors.New("store: a Gmail observation without an X-GM-LABELS reading")

// ApplyGmailObservation records a Gmail label reading for the message living
// at obs's coordinate and applies what CHANGED since the last reading (see
// the file comment). It must run in the same transaction as the ingest or
// flag refresh that observed the copy, so the change_log row lands with the
// mutation (the load-bearing invariant).
//
// It runs under a SAVEPOINT (#52 review H2): on any error everything it did
// is rolled back and the caller's transaction stays usable, so a label that
// cannot be stored never blocks ingesting the message itself. Label names are
// sanitised (gmaillabel.Sanitize) before anything is written.
func (tx *UserTx) ApplyGmailObservation(ctx context.Context, msgID ulid.ULID, obs GmailObservation) (GmailApplyResult, error) {
	if obs.Labels == nil {
		return GmailApplyResult{}, ErrGmailNoReading
	}
	obs.Labels = gmaillabel.Sanitize(obs.Labels)
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return GmailApplyResult{}, fmt.Errorf("store: gmail observation (savepoint): %w", err)
	}
	inner := *tx
	inner.tx = sp
	res, err := inner.applyGmailObservation(ctx, msgID, obs)
	if err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		return GmailApplyResult{}, err
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return GmailApplyResult{}, fmt.Errorf("store: gmail observation (release): %w", err)
	}
	return res, nil
}

func (tx *UserTx) applyGmailObservation(ctx context.Context, msgID ulid.ULID, obs GmailObservation) (GmailApplyResult, error) {
	var (
		res        GmailApplyResult
		prior      []string
		specialUse string
		accountID  string
		moved      string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT p.gm_labels, COALESCE(mb.special_use, ''), p.account_id,
		       COALESCE(m.moved_mailbox_id, '')
		  FROM app.message_provenance p
		  JOIN app.mailboxes mb ON mb.id = p.mailbox_id AND mb.user_id = p.user_id
		  JOIN app.messages m ON m.id = p.message_id AND m.user_id = p.user_id
		 WHERE p.user_id = $1 AND p.mailbox_id = $2 AND p.uidvalidity = $3 AND p.uid = $4
		   AND p.state = 'live' AND p.message_id = $5
		   FOR UPDATE OF p, m`,
		tx.userID.String(), obs.MailboxID.String(), obs.UIDValidity, obs.UID, msgID.String()).
		Scan(&prior, &specialUse, &accountID, &moved)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("store: gmail observation (read): %w", err)
	}
	res.Found = true
	res.FirstSeen = prior == nil
	labels := obs.Labels // never nil: ApplyGmailObservation refused that

	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.message_provenance
		   SET gm_labels = $5, gm_msgid = COALESCE($6, gm_msgid), gm_thrid = COALESCE($7, gm_thrid)
		 WHERE user_id = $1 AND mailbox_id = $2 AND uidvalidity = $3 AND uid = $4`,
		tx.userID.String(), obs.MailboxID.String(), obs.UIDValidity, obs.UID, labels,
		gmID(obs.MsgID), gmID(obs.ThrID)); err != nil {
		return res, fmt.Errorf("store: gmail observation (record): %w", err)
	}
	if prior != nil && gmaillabel.Equal(prior, labels) {
		return res, nil // nothing changed in Gmail: nothing to apply
	}

	// --- user labels: apply the difference --------------------------------
	added, removed := gmaillabel.Diff(prior, labels)
	if len(added) > 0 {
		ids, created, err := tx.ensureLabelsByName(ctx, added)
		if err != nil {
			return res, err
		}
		res.LabelsCreated = created
		appliedBy, _ := LabelSource{Kind: LabelByUser}.columns()
		tag, err := tx.tx.Exec(ctx, `
			INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
			SELECT $1, $2, l, $4 FROM unnest($3::text[]) AS l
			ON CONFLICT DO NOTHING`,
			tx.userID.String(), msgID.String(), ids, appliedBy)
		if err != nil {
			return res, fmt.Errorf("store: gmail observation (add labels): %w", err)
		}
		res.LabelsAdded = int(tag.RowsAffected())
	}
	if len(removed) > 0 {
		tag, err := tx.tx.Exec(ctx, `
			DELETE FROM app.message_labels ml
			 USING app.labels l
			 WHERE ml.user_id = $1 AND ml.message_id = $2
			   AND l.user_id = ml.user_id AND l.id = ml.label_id AND l.name = ANY($3)`,
			tx.userID.String(), msgID.String(), removed)
		if err != nil {
			return res, fmt.Errorf("store: gmail observation (remove labels): %w", err)
		}
		res.LabelsRemoved = int(tag.RowsAffected())
	}

	// --- folder placement --------------------------------------------------
	switch specialUse {
	case `\All`:
		next := gmaillabel.Classify(labels).Folder
		// A later reading moves the placement only when the label set's
		// folder changed. The first reading places it, unless ingest already
		// did (a blocked sender's message moved to Junk on arrival keeps that
		// move).
		apply := gmaillabel.Classify(prior).Folder != next
		if prior == nil {
			apply = moved == ""
		}
		if apply && prior != nil && moved != "" {
			// GMAIL MOVES ONLY WHAT GMAIL PLACED (#52 review M3). The current
			// placement is overwritten only when it is the one the PRIOR Gmail
			// folder implies (or none at all). A move the user made in
			// Mailroom — into a folder Gmail's labels never named — is theirs,
			// and a Gmail folder change does not undo it.
			was, err := tx.gmailFolderMailbox(ctx, accountID, gmaillabel.Classify(prior).Folder)
			if err != nil {
				return res, err
			}
			apply = moved == was
			if !apply && was == "" && moved == obs.MailboxID.String() {
				// An archive made in Mailroom places the message in All Mail
				// itself (MailboxForRole's Gmail archive, #52 increment 5b),
				// which is the placement Gmail's Archive folder implies, so a
				// later Gmail move out of the archive (back to the inbox) is
				// still Gmail's to make.
				apply = true
			}
		}
		if apply {
			// A placement overrides EVERY location for membership, so it is
			// only this account's to make while the message lives in this
			// account alone. The same bytes also live in another account (a
			// message sent between two of the user's own mailboxes, deduped by
			// content hash, ADR-001): placing it in this Gmail's INBOX would
			// take it out of the other account's folder. It then stays a member
			// of All Mail and of the other account's folders, by provenance.
			var elsewhere bool
			if err := tx.tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM app.message_provenance
				                WHERE user_id = $1 AND message_id = $2 AND state = 'live'
				                  AND account_id <> $3)`,
				tx.userID.String(), msgID.String(), accountID).Scan(&elsewhere); err != nil {
				return res, fmt.Errorf("store: gmail observation (other accounts): %w", err)
			}
			apply = !elsewhere
		}
		if apply {
			target, err := tx.gmailFolderMailbox(ctx, accountID, next)
			if err != nil {
				return res, err
			}
			if target != moved {
				if _, err := tx.tx.Exec(ctx, `
					UPDATE app.messages SET moved_mailbox_id = NULLIF($3, '')
					 WHERE user_id = $1 AND id = $2`,
					tx.userID.String(), msgID.String(), target); err != nil {
					return res, fmt.Errorf("store: gmail observation (place): %w", err)
				}
				res.FolderChanged = true
			}
		}
	case `\Junk`, `\Trash`:
		// The copy is in Gmail's Spam or Trash. A placement this path made
		// from All Mail's labels (INBOX, Sent, Drafts of this account) would
		// hide it there, so it goes; the message is then a member of
		// Spam/Trash through its provenance.
		if moved != "" {
			tag, err := tx.tx.Exec(ctx, `
				UPDATE app.messages m SET moved_mailbox_id = NULL
				 WHERE m.user_id = $1 AND m.id = $2
				   AND m.moved_mailbox_id IN (
				       SELECT id FROM app.mailboxes
				        WHERE user_id = $1 AND account_id = $3
				          AND (upper(imap_name) = 'INBOX' OR special_use IN ('\Sent', '\Drafts')))`,
				tx.userID.String(), msgID.String(), accountID)
			if err != nil {
				return res, fmt.Errorf("store: gmail observation (unplace): %w", err)
			}
			res.FolderChanged = tag.RowsAffected() > 0
		}
	}

	res.Changed = res.LabelsAdded > 0 || res.LabelsRemoved > 0 || res.FolderChanged
	if res.Changed {
		if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
			return res, err
		}
	}
	return res, nil
}

// clearGmailLabelViewPlacement clears a message's moved_mailbox_id when it is
// a Gmail label-view placement (INBOX, Sent or Drafts of the Gmail OAuth
// account that owns reapedMailbox) and that account no longer holds a live
// copy of the message (#52 review H3; called by reapMessage, which holds the
// message row lock). It reports whether it cleared one. Any other placement —
// a generic account's, or a Mailroom-side move into another folder — is left
// alone.
func (tx *UserTx) clearGmailLabelViewPlacement(ctx context.Context, msgID, reapedMailbox ulid.ULID) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages m SET moved_mailbox_id = NULL
		 WHERE m.user_id = $1 AND m.id = $2
		   AND m.moved_mailbox_id IN (
		       SELECT v.id
		         FROM app.mailboxes r
		         JOIN app.accounts a ON a.id = r.account_id AND a.user_id = r.user_id
		         JOIN app.mailboxes v ON v.account_id = r.account_id AND v.user_id = r.user_id
		        WHERE r.user_id = $1 AND r.id = $3
		          AND a.auth_kind = 'oauth2' AND a.provider_profile = 'gmail'
		          AND (upper(v.imap_name) = 'INBOX' OR v.special_use IN ('\Sent', '\Drafts')))
		   AND NOT EXISTS (
		       SELECT 1 FROM app.message_provenance p
		        WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'
		          AND p.account_id = (SELECT account_id FROM app.mailboxes WHERE user_id = $1 AND id = $3))`,
		tx.userID.String(), msgID.String(), reapedMailbox.String())
	if err != nil {
		return false, fmt.Errorf("store: clear gmail placement: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// gmailFolderMailbox returns the account's mailbox id for a folder role, or
// "" for Archive (All Mail membership comes from provenance) and for a role
// whose mailbox the account does not list.
func (tx *UserTx) gmailFolderMailbox(ctx context.Context, accountID, folder string) (string, error) {
	clause, ok := gmailFolderClause(folder)
	if !ok {
		return "", nil
	}
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.mailboxes
		 WHERE user_id = $1 AND account_id = $2 AND `+clause+`
		 ORDER BY imap_name LIMIT 1`,
		tx.userID.String(), accountID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: gmail folder mailbox: %w", err)
	}
	return id, nil
}

// ensureLabelsByName returns the ids of the user's labels with these exact
// names, creating each missing one (mirrored to IMAP, as the Takeout importer
// creates them: ADR-047 writes a mirrored label back as X-GM-LABELS in 5b).
// Labels are user-wide: a Gmail label named like an existing Mailroom label
// IS that label (ADR-047 §2).
func (tx *UserTx) ensureLabelsByName(ctx context.Context, names []string) ([]string, int, error) {
	have, err := tx.LabelIDsByName(ctx, names)
	if err != nil {
		return nil, 0, err
	}
	created := 0
	ids := make([]string, 0, len(names))
	for _, n := range names {
		if id, ok := have[n]; ok {
			ids = append(ids, id.String())
			continue
		}
		id, err := tx.CreateLabel(ctx, LabelRow{Name: n, MirrorToIMAP: true})
		if errors.Is(err, ErrLabelNameTaken) {
			// Created concurrently (another account's sync, a client): re-read.
			again, rerr := tx.LabelIDsByName(ctx, []string{n})
			if rerr != nil {
				return nil, 0, rerr
			}
			got, ok := again[n]
			if !ok {
				return nil, 0, fmt.Errorf("store: label %q exists but cannot be found", n)
			}
			ids = append(ids, got.String())
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		created++
		ids = append(ids, id.String())
	}
	return ids, created, nil
}

// LabelIDsByName maps each of names that is an existing label of the user to
// its id (exact, case-sensitive match: app.labels' UNIQUE (user_id, name)).
func (tx *UserTx) LabelIDsByName(ctx context.Context, names []string) (map[string]ulid.ULID, error) {
	out := map[string]ulid.ULID{}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, name FROM app.labels WHERE user_id = $1 AND name = ANY($2)`,
		tx.userID.String(), names)
	if err != nil {
		return nil, fmt.Errorf("store: labels by name: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("store: labels by name (scan): %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse label id %q: %w", id, err)
		}
		out[name] = u
	}
	return out, rows.Err()
}

func gmID(v uint64) any {
	if v == 0 {
		return nil
	}
	return strconv.FormatUint(v, 10)
}

// ThreadByGmailThrID returns the thread of a message this account holds a
// live copy of with this X-GM-THRID — the threading hint (ADR-047 §2, ADR-030)
// ingest falls back to when no Message-ID hint matched. found is false when
// none does.
func (tx *UserTx) ThreadByGmailThrID(ctx context.Context, accountID ulid.ULID, thrID uint64) (ulid.ULID, bool, error) {
	if thrID == 0 {
		return ulid.ULID{}, false, nil
	}
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT m.thread_id
		  FROM app.message_provenance p
		  JOIN app.messages m ON m.id = p.message_id AND m.user_id = p.user_id
		 WHERE p.user_id = $1 AND p.account_id = $2 AND p.gm_thrid = $3
		   AND p.state = 'live' AND m.thread_id IS NOT NULL
		 ORDER BY m.received_at, m.id
		 LIMIT 1`,
		tx.userID.String(), accountID.String(), strconv.FormatUint(thrID, 10)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: thread by gmail thrid: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse thread id %q: %w", id, err)
	}
	return u, true, nil
}

// ---------------------------------------------------------------------------
// Newest-first import bookkeeping
// ---------------------------------------------------------------------------

// SetBackfillFloor records a mailbox's newest-first import floor (migration
// 00079). Sync bookkeeping, not JMAP-visible: no change_log row.
func (tx *UserTx) SetBackfillFloor(ctx context.Context, mailboxID ulid.ULID, floor int64) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.mailboxes SET backfill_floor_uid = $3
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String(), floor); err != nil {
		return fmt.Errorf("store: set backfill floor: %w", err)
	}
	return nil
}

// SetLabelSweepNext records where an interrupted label sweep resumes (nil =
// none in progress). Bookkeeping: no change_log row.
func (tx *UserTx) SetLabelSweepNext(ctx context.Context, mailboxID ulid.ULID, next *int64) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.mailboxes SET label_sweep_next_uid = $3
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String(), next); err != nil {
		return fmt.Errorf("store: set label sweep progress: %w", err)
	}
	return nil
}

// StampLabelSweep records when the X-GM-LABELS sweep last read a mailbox.
// Bookkeeping: no change_log row.
func (tx *UserTx) StampLabelSweep(ctx context.Context, mailboxID ulid.ULID, at time.Time) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.mailboxes SET labels_swept_at = $3
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String(), at.UTC()); err != nil {
		return fmt.Errorf("store: stamp label sweep: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The download budget's meter: a ROLLING 24 HOURS
// ---------------------------------------------------------------------------
//
// Whether Gmail's daily download limit is a calendar day (and in which time
// zone) or a rolling 24 hours is UNVERIFIED (#52 review M2). A calendar-day
// meter would allow twice the budget inside 24 hours across its midnight —
// 2,000 MB at 23:00 and 2,000 MB more at 00:00 is 4,000 MB against a 2,500 MB
// limit. A rolling window is safe under either reading, so the meter keeps
// one-hour buckets (app.account_sync_status.download_hours, a ring indexed by
// hour-since-epoch mod meterHours) and the hour of its latest write
// (download_hour). The window total is the ring's sum after the hours since
// that write have been zeroed. It is per MAILROOM ACCOUNT, not per Google
// identity: one Google account connected twice would have two meters.
//
// ⚠ 25 BUCKETS, NOT 24: CONSERVATIVE BY UP TO ONE HOUR (#907 security
// review). A bucket covers a whole hour, so a byte is only known to have been
// fetched SOMEWHERE in it. With 24 buckets, hour H's bucket is cleared when
// hour H+24 starts, so a byte fetched at H:00:01 stopped counting as little as
// 23h59m later: a full budget at 00:01 and another at 24:00 put twice the
// budget inside one true 24 hours. With 25, a byte stays counted until hour
// H+25 starts — at least 24 hours after the END of its hour, and at most 25
// after it was fetched. The window over-counts by up to one hour; it never
// under-counts.

const meterHours = 25

// hourOf is the absolute hour index of t.
func hourOf(t time.Time) int64 { return t.UTC().Unix() / 3600 }

// advanceRing zeroes the buckets of the hours after last up to and including
// now, so ring holds only the 24 hours ending at now. A ring that is not 24
// long (a fresh row) is replaced. A clock that went backwards zeroes nothing.
func advanceRing(ring []int64, last, now int64) []int64 {
	if len(ring) != meterHours {
		return make([]int64, meterHours)
	}
	out := append([]int64(nil), ring...)
	if now-last >= meterHours {
		return make([]int64, meterHours)
	}
	for h := last + 1; h <= now; h++ {
		out[h%meterHours] = 0
	}
	return out
}

func ringSum(ring []int64) int64 {
	var n int64
	for _, v := range ring {
		n += v
	}
	return n
}

// readMeter reads the ring and its hour (lock = FOR UPDATE).
func (tx *UserTx) readMeter(ctx context.Context, accountID ulid.ULID, lock bool) (ring []int64, last int64, err error) {
	var at *time.Time
	q := `SELECT download_hours, download_hour FROM app.account_sync_status WHERE user_id = $1 AND account_id = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	err = tx.tx.QueryRow(ctx, q, tx.userID.String(), accountID.String()).Scan(&ring, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("store: read download meter: %w", err)
	}
	if at != nil {
		last = hourOf(*at)
	}
	return ring, last, nil
}

// DownloadedLast24h returns the message-body bytes metered for the account in
// the 24 hours (whole hours) ending at at.
func (tx *UserTx) DownloadedLast24h(ctx context.Context, accountID ulid.ULID, at time.Time) (int64, error) {
	ring, last, err := tx.readMeter(ctx, accountID, false)
	if err != nil || ring == nil {
		return 0, err
	}
	return ringSum(advanceRing(ring, last, hourOf(at))), nil
}

// AddDownloadBytes meters n bytes at time at and returns the rolling window's
// total after it (24 hours, conservative by up to one: see meterHours). Sync
// bookkeeping: no change_log row.
//
// THE ROW IS MADE TO EXIST BEFORE IT IS LOCKED (#907 re-review). Two first
// writes for an account could otherwise both read "no row", both compute a
// ring holding only their own bytes, and the second INSERT ... ON CONFLICT
// would overwrite the first ring: bytes lost from the meter. INSERT ... ON
// CONFLICT DO NOTHING waits for a concurrent inserter to commit, and the
// SELECT ... FOR UPDATE after it then serializes the two read-modify-writes
// on the one row, so every caller adds to what the previous one wrote.
func (tx *UserTx) AddDownloadBytes(ctx context.Context, accountID ulid.ULID, at time.Time, n int64) (int64, error) {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.account_sync_status (account_id, user_id) VALUES ($2, $1)
		ON CONFLICT (account_id) DO NOTHING`,
		tx.userID.String(), accountID.String()); err != nil {
		return 0, fmt.Errorf("store: add download bytes (row): %w", err)
	}
	ring, last, err := tx.readMeter(ctx, accountID, true)
	if err != nil {
		return 0, err
	}
	now := hourOf(at)
	if ring != nil && now < last {
		now = last // a clock that went backwards charges the latest hour
	}
	ring = advanceRing(ring, last, now)
	ring[now%meterHours] += n
	hourStart := time.Unix(now*3600, 0).UTC()
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.account_sync_status SET download_hour = $3, download_hours = $4
		 WHERE user_id = $1 AND account_id = $2`,
		tx.userID.String(), accountID.String(), hourStart, ring); err != nil {
		return 0, fmt.Errorf("store: add download bytes: %w", err)
	}
	return ringSum(ring), nil
}

// ---------------------------------------------------------------------------
// Progress (Account/getHealth, computed — no change_log)
// ---------------------------------------------------------------------------

// GmailImportProgress is the newest-first import's progress for one account,
// over its synced Gmail mailboxes (All Mail, Spam, Trash).
type GmailImportProgress struct {
	// MessagesOnServer is the sum of each synced mailbox's MESSAGES at its
	// last poll; nil when no mailbox has been polled yet.
	MessagesOnServer *int64
	// MessagesImported is the live copies Mailroom holds in those mailboxes.
	MessagesImported int64
	// BytesImported is the stored size of those messages.
	BytesImported int64
	// HistoryComplete is true when every synced mailbox's floor has reached
	// UID 1 (or it was never floored).
	HistoryComplete bool
	// BytesLast24h is the meter's rolling 24-hour reading.
	BytesLast24h int64
	// Mailboxes is how many synced Gmail mailboxes the account has rows for.
	Mailboxes int
}

// GmailImportProgress computes the progress of an account's Gmail import.
func (tx *UserTx) GmailImportProgress(ctx context.Context, accountID ulid.ULID, now time.Time) (GmailImportProgress, error) {
	var (
		p        GmailImportProgress
		onServer *int64
		pending  int
	)
	err := tx.tx.QueryRow(ctx, `
		WITH mb AS (
			SELECT id, poll_messages, backfill_floor_uid
			  FROM app.mailboxes
			 WHERE user_id = $1 AND account_id = $2
			   AND special_use IN ('\All', '\Junk', '\Trash')
		)
		SELECT (SELECT count(*) FROM mb),
		       (SELECT sum(poll_messages) FROM mb),
		       (SELECT count(*) FROM mb WHERE backfill_floor_uid > 1),
		       COALESCE((SELECT count(*) FROM app.message_provenance p
		                  WHERE p.user_id = $1 AND p.state = 'live'
		                    AND p.mailbox_id IN (SELECT id FROM mb)), 0),
		       COALESCE((SELECT sum(m.size_bytes) FROM app.messages m
		                  WHERE m.user_id = $1 AND m.id IN (
		                        SELECT p.message_id FROM app.message_provenance p
		                         WHERE p.user_id = $1 AND p.state = 'live'
		                           AND p.mailbox_id IN (SELECT id FROM mb))), 0)`,
		tx.userID.String(), accountID.String()).
		Scan(&p.Mailboxes, &onServer, &pending, &p.MessagesImported, &p.BytesImported)
	if err != nil {
		return p, fmt.Errorf("store: gmail import progress: %w", err)
	}
	p.MessagesOnServer = onServer
	p.HistoryComplete = p.Mailboxes > 0 && pending == 0
	if p.BytesLast24h, err = tx.DownloadedLast24h(ctx, accountID, now); err != nil {
		return p, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// The census (read-only)
// ---------------------------------------------------------------------------

// CountMessageIDsElsewhere counts how many of the given Message-IDs (with or
// without angle brackets) the user already has a message for that does NOT
// live in accountID — the census's dedup preview: mail already in Mailroom
// from another account or an import. Read-only.
func (tx *UserTx) CountMessageIDsElsewhere(ctx context.Context, accountID ulid.ULID, messageIDs []string) (int, error) {
	if len(messageIDs) == 0 {
		return 0, nil
	}
	bare := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id == "" {
			continue
		}
		if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
			id = id[1 : len(id)-1]
		}
		bare = append(bare, id)
	}
	var n int
	err := tx.tx.QueryRow(ctx, `
		SELECT count(DISTINCT q.id)
		  FROM unnest($2::text[]) AS q(id)
		 WHERE EXISTS (
		       SELECT 1 FROM app.messages m
		        WHERE m.user_id = $1
		          AND m.message_id_hint IN (q.id, '<' || q.id || '>')
		          AND NOT EXISTS (
		              SELECT 1 FROM app.message_provenance p
		               WHERE p.user_id = m.user_id AND p.message_id = m.id
		                 AND p.account_id = $3 AND p.state = 'live'))`,
		tx.userID.String(), bare, accountID.String()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count message-ids elsewhere: %w", err)
	}
	return n, nil
}
