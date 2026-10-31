// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// THE GMAIL WRITEBACK'S STORE SURFACE (#52, ADR-047 §3, increment 5b).
//
// The writeback engine applies a Gmail account's intents with Gmail's own
// primitives (labels, not folders; no EXPUNGE outside Trash and Spam). It
// needs three things from here: which mailbox is which on the account
// (GmailWritebackFolders), a way to record a confirmed label write on the
// copy's Gmail baseline (RecordGmailLabelDelta — the echo suppression), and
// whether the account's writeback is held at all (GmailWritebackHeld).

// GmailWritebackHeld reports whether an account's writeback is held off by
// the Gmail writeback switch (GmailWritebackReady, mirrored by
// oauthmail.GmailWritebackReady). It is the Go form of
// gmailWritebackHeldSQL, for the writeback runner's drain gate.
func GmailWritebackHeld(authKind, providerProfile string) bool {
	return !GmailWritebackReady && IsGmailOAuth(authKind, providerProfile)
}

// IsGmailOAuth reports whether an account is a live Gmail OAuth account, the
// only kind ADR-047's label model governs. A Gmail account connected with an
// app password through the generic path is NOT one: it syncs and writes back
// label folders like any IMAP server (ADR-047, Consequences).
func IsGmailOAuth(authKind, providerProfile string) bool {
	return authKind == "oauth2" && providerProfile == "gmail"
}

// GmailFolders are the raw IMAP names of the Gmail mailboxes the writeback
// engine needs, as the account's last LIST reported them. An empty name means
// the account lists no such mailbox.
type GmailFolders struct {
	All    string // \All: [Gmail]/All Mail
	Trash  string // \Trash
	Spam   string // \Junk
	Drafts string // \Drafts
	Inbox  string // INBOX
	// Reserved is every mailbox name on the account that carries a
	// SPECIAL-USE attribute (All Mail, Spam, Trash, Sent, Drafts, Starred,
	// Important, ...). Anything else except INBOX is a user-label view.
	Reserved map[string]bool
}

// GmailWritebackFolders returns the account's Gmail folders by role. Read
// only.
func (tx *UserTx) GmailWritebackFolders(ctx context.Context, accountID ulid.ULID) (GmailFolders, error) {
	f := GmailFolders{Reserved: map[string]bool{}}
	rows, err := tx.tx.Query(ctx, `
		SELECT imap_name, COALESCE(special_use, '')
		  FROM app.mailboxes
		 WHERE user_id = $1 AND account_id = $2
		   AND (special_use IS NOT NULL OR upper(imap_name) = 'INBOX')
		 ORDER BY imap_name`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return f, fmt.Errorf("store: gmail writeback folders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, su string
		if err := rows.Scan(&name, &su); err != nil {
			return f, fmt.Errorf("store: gmail writeback folders (scan): %w", err)
		}
		if su != "" {
			f.Reserved[name] = true
		}
		var dupe string
		set := func(dst *string, role string) {
			if *dst != "" && dupe == "" {
				dupe = fmt.Sprintf("%s is both %q and %q", role, *dst, name)
				return
			}
			*dst = name
		}
		switch {
		case strings.EqualFold(name, "INBOX"):
			set(&f.Inbox, "INBOX")
		case su == `\All`:
			set(&f.All, `\All`)
		case su == `\Trash`:
			set(&f.Trash, `\Trash`)
		case su == `\Junk`:
			set(&f.Spam, `\Junk`)
		case su == `\Drafts`:
			set(&f.Drafts, `\Drafts`)
		}
		if dupe != "" {
			// AMBIGUOUS ROLES ARE REFUSED (#911 review F9): which "Trash" a
			// permanent delete expunges in must not be decided by sort order.
			return GmailFolders{}, fmt.Errorf("%w: %s", ErrGmailAmbiguousFolders, dupe)
		}
	}
	return f, rows.Err()
}

// ErrGmailAmbiguousFolders is GmailWritebackFolders' refusal of an account
// that lists two mailboxes for one role: the writeback runner then writes
// nothing to the account until its folders are unambiguous.
var ErrGmailAmbiguousFolders = errors.New("store: a Gmail account lists two mailboxes for one role")

// RecordGmailLabelDelta records, on one live Gmail copy, a label change the
// server just CONFIRMED Mailroom made (#52, ADR-047 §3): add is appended to
// the copy's gm_labels baseline, remove is taken out of it. It reports whether
// a row changed.
//
// # Why: echo suppression
//
// The read path applies Gmail's CHANGES, not Gmail's state: a reading is
// diffed against gm_labels (ApplyGmailObservation). A label Mailroom wrote is
// a change on the server that the baseline does not know about, so without
// this the next reading would report it as a change made in Gmail. That is
// harmless only while nothing else is pending. When the user has since undone
// it in Mailroom and that undo is still queued, the reading re-applies the
// stale state over the user's newer one — the sweep fighting the user. With
// the baseline moved by exactly the delta that was confirmed, the next reading
// of an otherwise unchanged Gmail diffs to nothing.
//
// Only by the DELTA, never by replacing the baseline with what the STORE
// echoed: a label someone added in Gmail's own apps since the last reading is
// in that echo too, and adopting it as the baseline would mean it is never
// imported at all.
//
// A copy with NO baseline yet (gm_labels NULL: never read) is left alone, so
// its first reading still counts as a first reading. Labels are compared
// case-insensitively — Gmail's names are (#911 review F7) — so removing "work"
// takes out a baseline "Work", and an add already present in another case is
// not added twice; the engine passes Gmail's echoed spelling for adds.
// Bookkeeping, not JMAP-visible: no change_log row.
func (tx *UserTx) RecordGmailLabelDelta(ctx context.Context, provenanceID ulid.ULID, add, remove []string) (bool, error) {
	if len(add) == 0 && len(remove) == 0 {
		return false, nil
	}
	if add == nil {
		add = []string{}
	}
	lowRemove := make([]string, 0, len(remove))
	for _, l := range remove {
		lowRemove = append(lowRemove, strings.ToLower(l))
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.message_provenance p
		   SET gm_labels = ARRAY(
		       SELECT x.l FROM (
		           SELECT t.l, t.o,
		                  row_number() OVER (PARTITION BY lower(t.l) ORDER BY t.o) AS rn
		             FROM unnest(p.gm_labels || $3::text[]) WITH ORDINALITY AS t(l, o)
		            WHERE NOT lower(t.l) = ANY($4::text[])) x
		        WHERE x.rn = 1
		        ORDER BY x.o)
		 WHERE p.user_id = $1 AND p.id = $2 AND p.state = 'live'
		   AND p.gm_labels IS NOT NULL`,
		tx.userID.String(), provenanceID.String(), add, lowRemove)
	if err != nil {
		return false, fmt.Errorf("store: record gmail label delta: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// gmailArchiveMailbox is MailboxForRole's Gmail fallback for role "archive":
// a Gmail OAuth account has no \Archive mailbox, and ADR-047 §2 makes All
// Mail its Archive (no folder-like label = archived). found is false for any
// other account, so a generic server's \All (a virtual folder on some
// servers) is never mistaken for a place to move mail to.
func (tx *UserTx) gmailArchiveMailbox(ctx context.Context, accountID ulid.ULID) (MailboxTarget, bool, error) {
	var id, name string
	err := tx.tx.QueryRow(ctx, `
		SELECT mb.id, mb.imap_name
		  FROM app.mailboxes mb
		  JOIN app.accounts a ON a.id = mb.account_id AND a.user_id = mb.user_id
		 WHERE mb.user_id = $1 AND mb.account_id = $2 AND mb.special_use = '\All'
		   AND a.auth_kind = 'oauth2' AND a.provider_profile = 'gmail'
		 ORDER BY mb.imap_name LIMIT 1`,
		tx.userID.String(), accountID.String()).Scan(&id, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return MailboxTarget{}, false, nil
	}
	if err != nil {
		return MailboxTarget{}, false, fmt.Errorf("store: gmail archive mailbox: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return MailboxTarget{}, false, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
	}
	return MailboxTarget{ID: u, AccountID: accountID, IMAPName: name}, true, nil
}
