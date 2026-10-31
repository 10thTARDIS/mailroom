// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// This file holds the read surface for JMAP Mailbox/get and Mailbox/query
// (RFC 8621 §2). Folders are Mailboxes (spec §13); labels are keywords and are
// handled on the Email object, not here. All reads run on a UserTx, so RLS
// scopes every row to the caller.

// MailboxView is a JMAP Mailbox projection. Role is the RFC 8621 role derived
// from the IMAP SPECIAL-USE attribute (data-model/spec §16 SPECIAL-USE
// detection); it is "" when the mailbox has no special use. Mailroom's IMAP
// mailboxes are flat (no stored parent), so ParentID is always nil in 0.5.
type MailboxView struct {
	ID ulid.ULID
	// AccountID is the owning account (app.mailboxes.account_id). It is carried
	// on the projection because folder NAMES collide across accounts — every
	// account has an INBOX, a Sent, an Archive — so a client cannot tell one
	// account's Inbox from another's by name alone (#488).
	AccountID    ulid.ULID
	Name         string
	Role         string
	SortOrder    int
	TotalEmails  int
	UnreadEmails int
	// SyncScope is 'full' or 'off' (#62). It is carried on the JMAP projection
	// because a client MUST be able to tell a live folder from a frozen one: an
	// 'off' mailbox keeps every message it already had and gains nothing, so
	// rendering it like any other folder would present a snapshot as current mail.
	SyncScope string
	// FeedsDesk is "folders feed the desk" (#442, #446): mail with live
	// provenance here counts as unactioned exactly like INBOX for the desk's
	// AreaSummary. Carried on the projection for the same reason SyncScope is —
	// a client configuring which folders feed the desk needs to read the
	// current setting back, not just write it.
	FeedsDesk bool
}

// roleForMailbox derives the lowercase JMAP mailbox role (RFC 8621 §2) from a
// mailbox's IMAP name and SPECIAL-USE attribute. INBOX is the reserved IMAP
// mailbox name (RFC 3501 §5.1, case-insensitive) and is the inbox BY NAME, not
// by a SPECIAL-USE attribute — RFC 6154 defines no `\Inbox`, so servers send no
// special-use for it. It must therefore be recognized by name, or the inbox ends
// up role-less and drops out of any inbox-role view (e.g. the client's unified
// inbox). Every other role comes from SPECIAL-USE.
func roleForMailbox(imapName, specialUse string) string {
	if strings.EqualFold(imapName, "INBOX") {
		return "inbox"
	}
	return roleForSpecialUse(specialUse)
}

// roleForSpecialUse maps an IMAP SPECIAL-USE attribute (RFC 6154, e.g. `\Sent`)
// to the lowercase JMAP mailbox role (RFC 8621 §2). Unknown/absent → "". INBOX
// has no SPECIAL-USE attribute in practice; use roleForMailbox to also honor the
// reserved INBOX name.
func roleForSpecialUse(specialUse string) string {
	switch strings.ToLower(strings.TrimPrefix(specialUse, `\`)) {
	case "inbox":
		return "inbox"
	case "archive":
		return "archive"
	case "drafts":
		return "drafts"
	case "sent":
		return "sent"
	case "trash":
		return "trash"
	case "junk":
		return "junk"
	case "all":
		return "all"
	case "flagged":
		return "flagged"
	default:
		return ""
	}
}

// ListMailboxIDs returns every mailbox id for the user, ordered by display name
// then id — the stable, total order Mailbox/query needs.
func (tx *UserTx) ListMailboxIDs(ctx context.Context) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id FROM app.mailboxes
		 WHERE user_id = $1
		 ORDER BY display_name, id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list mailbox ids: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan mailbox id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list mailbox ids: %w", err)
	}
	return out, nil
}

// GetMailboxes returns Mailbox projections for the requested ids, with
// totalEmails / unreadEmails computed from live provenance (the physical
// membership index behind messages). Ids not returned were not found (or belong
// to another user — RLS makes those indistinguishable, which is the isolation
// property). Order of the result is unspecified; the caller re-associates by id.
func (tx *UserTx) GetMailboxes(ctx context.Context, ids []ulid.ULID) ([]MailboxView, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// The counts run over EFFECTIVE membership — the same derivation Email/get
	// and Email/query use (jmap_email.go): a message the user moved is counted in
	// the mailbox they moved it to, from the instant the Email/set commits.
	// Counting raw provenance instead is how an archived message keeps its
	// unread badge on the Inbox until the IMAP round-trip (and, since provenance
	// is never expunged, after it too).
	rows, err := tx.tx.Query(ctx, `
		WITH membership AS (`+effectiveMembershipSQL+`)
		SELECT m.id, m.account_id, m.display_name, m.imap_name, COALESCE(m.special_use, ''),
		       m.sync_scope, m.feeds_desk,
		       COALESCE(cnt.total, 0), COALESCE(cnt.unread, 0)
		  FROM app.mailboxes m
		  LEFT JOIN (
		      SELECT mailbox_id,
		             count(DISTINCT message_id) AS total,
		             count(DISTINCT message_id)
		               FILTER (WHERE NOT is_seen AND trashed_at IS NULL) AS unread
		        FROM membership
		       GROUP BY mailbox_id
		  ) cnt ON cnt.mailbox_id = m.id
		 WHERE m.user_id = $1 AND m.id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get mailboxes: %w", err)
	}
	defer rows.Close()
	var out []MailboxView
	for rows.Next() {
		var (
			mv         MailboxView
			id         string
			accountID  string
			imapName   string
			specialUse string
		)
		if err := rows.Scan(&id, &accountID, &mv.Name, &imapName, &specialUse, &mv.SyncScope, &mv.FeedsDesk,
			&mv.TotalEmails, &mv.UnreadEmails); err != nil {
			return nil, fmt.Errorf("store: scan mailbox: %w", err)
		}
		if mv.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
		}
		if mv.AccountID, err = ulid.Parse(accountID); err != nil {
			return nil, fmt.Errorf("store: parse mailbox account id %q: %w", accountID, err)
		}
		mv.Role = roleForMailbox(imapName, specialUse)
		out = append(out, mv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get mailboxes: %w", err)
	}
	return out, nil
}

// MailboxRole resolves a mailbox id to its JMAP role (derived from SPECIAL-USE),
// used by Email/set to interpret a mailboxIds change (a move into a trash-role
// mailbox is a trash, etc.). found is false when the mailbox does not exist for
// this user (RLS makes another user's id indistinguishable from a missing one).
func (tx *UserTx) MailboxRole(ctx context.Context, id ulid.ULID) (role string, found bool, err error) {
	var imapName, specialUse string
	e := tx.tx.QueryRow(ctx,
		`SELECT m.imap_name, COALESCE(m.special_use, '') FROM app.mailboxes m
		  WHERE m.user_id = $1 AND m.id = $2`,
		tx.userID.String(), id.String()).Scan(&imapName, &specialUse)
	if e != nil {
		if isNoRows(e) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: mailbox role: %w", e)
	}
	return roleForMailbox(imapName, specialUse), true, nil
}

// TrashMailboxIDForAccount returns the id of the account's Trash mailbox
// (special_use \Trash), used to resolve the deletion-lifecycle target when
// Email/set trashes a message. found is false when the account has no detected
// Trash mailbox yet (the writeback engine then falls back to a name-based move).
func (tx *UserTx) TrashMailboxIDForAccount(ctx context.Context, accountID ulid.ULID) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.mailboxes
		 WHERE user_id = $1 AND account_id = $2 AND special_use = '\Trash'
		 LIMIT 1`,
		tx.userID.String(), accountID.String()).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: trash mailbox for account: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
	}
	return u, true, nil
}

// trashRoleMailboxIDs returns every mailbox of the caller's, across all
// accounts, whose JMAP role is "trash": the mailboxes a Trash view lists. The
// role comes from roleForMailbox, the same function MailboxRole uses, so a
// search for `is:trashed` and the Trash view cannot disagree about which folders
// are Trash (#623).
func (tx *UserTx) trashRoleMailboxIDs(ctx context.Context) ([]ulid.ULID, error) {
	return tx.roleMailboxIDs(ctx, "trash")
}

// junkRoleMailboxIDs is trashRoleMailboxIDs for the "junk" role: the folders a
// Junk view lists, which is what `in:junk` and search's default junk exclusion
// treat as junk alongside the provider's $junk verdict (#723).
func (tx *UserTx) junkRoleMailboxIDs(ctx context.Context) ([]ulid.ULID, error) {
	return tx.roleMailboxIDs(ctx, "junk")
}

// roleMailboxIDs returns every mailbox of the caller's, across all accounts,
// whose JMAP role (roleForMailbox) is role.
func (tx *UserTx) roleMailboxIDs(ctx context.Context, role string) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, imap_name, COALESCE(special_use, '') FROM app.mailboxes
		 WHERE user_id = $1`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: %s mailboxes: %w", role, err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var id, imapName, specialUse string
		if err := rows.Scan(&id, &imapName, &specialUse); err != nil {
			return nil, fmt.Errorf("store: scan %s mailbox: %w", role, err)
		}
		if roleForMailbox(imapName, specialUse) != role {
			continue
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s mailboxes: %w", role, err)
	}
	return out, nil
}
