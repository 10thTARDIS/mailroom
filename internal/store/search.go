// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The store surface the search indexer consumes (spec §7). Two reads and two
// lookups, all on a UserTx — so RLS scopes every statement to the user being
// indexed and the indexer inherits the database's fail-closed posture: no
// identity, no rows, and no way to index (or search) another user's mail.
//
// Change-log discipline (the load-bearing invariant): NOTHING here mutates
// anything, so nothing here appends a change_log row. The indexer is a READER of
// the stream (UserTx.Changes, the same method JMAP Foo/changes uses) and its
// cursor lives with the index, not in the database — see internal/search/index.go
// for why.

// IndexMessage is everything the search index needs about one message that lives
// in Postgres. The body and the recipient list are NOT here: the schema stores
// neither (app.messages projects only the envelope), so the indexer reads them
// from the encrypted blob.
type IndexMessage struct {
	ID          ulid.ULID
	ThreadID    ulid.ULID
	ContentHash []byte

	Subject     string
	Snippet     string
	FromName    string
	FromAddress string

	ReceivedAt time.Time
	SentAt     *time.Time
	SizeBytes  int64

	HasAttachments bool
	Trashed        bool
	Seen           bool
	Flagged        bool
	Answered       bool
	Draft          bool
	Junk           bool

	Keywords   []string
	LabelIDs   []ulid.ULID
	MailboxIDs []ulid.ULID
	AccountIDs []ulid.ULID

	// Index schema v3 (#722/#723): the List-Id and List-Unsubscribe header
	// values ingest projected onto the row ("" when absent), the address the
	// message was delivered to, and its attachment parts.
	ListID           string
	ListUnsubscribe  string
	AddressedAddress string
	Attachments      []IndexAttachment
}

// IndexAttachment is one app.attachments row as the index sees it: the declared
// file name ("" when the part has none) and MIME type.
type IndexAttachment struct {
	Filename    string
	ContentType string
}

// IndexMessages returns the index projections for the requested ids. Ids that do
// not resolve (destroyed, or another user's — indistinguishable by design) are
// simply absent from the result.
func (tx *UserTx) IndexMessages(ctx context.Context, ids []ulid.ULID) ([]IndexMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	idStrs := ulidStrings(ids)

	rows, err := tx.tx.Query(ctx, `
		SELECT id, COALESCE(thread_id, ''), content_hash,
		       subject, snippet, from_name, COALESCE(from_address, ''),
		       received_at, sent_at, size_bytes, has_attachments,
		       (trashed_at IS NOT NULL),
		       is_seen, is_flagged, is_answered, is_draft, is_junk, keywords,
		       COALESCE(list_id, ''), COALESCE(list_unsubscribe, ''),
		       COALESCE(addressed_address::text, '')
		  FROM app.messages
		 WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), idStrs)
	if err != nil {
		return nil, fmt.Errorf("store: index messages: %w", err)
	}
	defer rows.Close()

	byID := make(map[ulid.ULID]*IndexMessage, len(ids))
	order := make([]ulid.ULID, 0, len(ids))
	for rows.Next() {
		var (
			m              IndexMessage
			id, threadID   string
			keywords       []string
			sentAt         *time.Time
			hasAttachments bool
		)
		if err := rows.Scan(&id, &threadID, &m.ContentHash,
			&m.Subject, &m.Snippet, &m.FromName, &m.FromAddress,
			&m.ReceivedAt, &sentAt, &m.SizeBytes, &hasAttachments,
			&m.Trashed,
			&m.Seen, &m.Flagged, &m.Answered, &m.Draft, &m.Junk, &keywords,
			&m.ListID, &m.ListUnsubscribe, &m.AddressedAddress); err != nil {
			return nil, fmt.Errorf("store: scan index message: %w", err)
		}
		if m.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		if threadID != "" {
			if m.ThreadID, err = ulid.Parse(threadID); err != nil {
				return nil, fmt.Errorf("store: parse thread id %q: %w", threadID, err)
			}
		}
		m.SentAt = sentAt
		m.HasAttachments = hasAttachments
		m.Keywords = keywords
		byID[m.ID] = &m
		order = append(order, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: index messages: %w", err)
	}
	if len(order) == 0 {
		return nil, nil
	}

	if err := tx.attachIndexLabels(ctx, idStrs, byID); err != nil {
		return nil, err
	}
	if err := tx.attachIndexProvenance(ctx, idStrs, byID); err != nil {
		return nil, err
	}
	if err := tx.attachIndexAttachments(ctx, idStrs, byID); err != nil {
		return nil, err
	}

	out := make([]IndexMessage, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// attachIndexLabels fills in each message's label ids. The index stores label
// IDS, not names: a rename then costs no reindex, and a delete (which
// deliberately does not fan out per-Email change rows — see DeleteLabel) simply
// leaves an id that no longer resolves, rather than a stale name that still
// matches.
func (tx *UserTx) attachIndexLabels(ctx context.Context, idStrs []string, byID map[ulid.ULID]*IndexMessage) error {
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, array_agg(label_id)
		  FROM app.message_labels
		 WHERE user_id = $1 AND message_id = ANY($2)
		 GROUP BY message_id`,
		tx.userID.String(), idStrs)
	if err != nil {
		return fmt.Errorf("store: index messages (labels): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var labelIDs []string
		if err := rows.Scan(&messageID, &labelIDs); err != nil {
			return fmt.Errorf("store: scan index labels: %w", err)
		}
		m, ok := lookupIndexMessage(byID, messageID)
		if !ok {
			continue
		}
		ids, err := parseULIDs(labelIDs)
		if err != nil {
			return err
		}
		m.LabelIDs = ids
	}
	return rows.Err()
}

// attachIndexProvenance fills in each message's mailbox and account membership —
// what `account:` and mailbox scoping search on.
//
// This mirrors EFFECTIVE membership exactly as the JMAP read path derives it
// (jmap_email.go effectiveMembershipSQL), not raw live provenance, and for the
// same reason GetEmails/QueryEmails do: an INNER JOIN against
// app.message_provenance alone misses any message whose membership is decided
// by moved_mailbox_id (an archived/moved message — provenance still says the
// old folder) or, worse, by local_mailbox_id with NO provenance row at all (a
// draft Mailroom authored that has not been APPENDed yet, spec §5) — such a
// draft got NO mailbox/account facet and was invisible to mailbox- and
// account-scoped search (mailroom#90; free-text search already found it, since
// that path does not go through provenance).
//
// The account for each mailbox comes from app.mailboxes.account_id rather than
// from provenance directly: a move never crosses accounts (movePlacement only
// ever resolves within the message's existing accounts, jmap_email.go
// liveAccountIDs), so the mailbox's account agrees with the account the old
// provenance-only query reported, while additionally covering the
// local_mailbox_id (draft) and moved_mailbox_id cases provenance cannot see.
func (tx *UserTx) attachIndexProvenance(ctx context.Context, idStrs []string, byID map[ulid.ULID]*IndexMessage) error {
	rows, err := tx.tx.Query(ctx, `
		WITH membership AS (`+effectiveMembershipSQL+`)
		SELECT mb.message_id,
		       array_agg(DISTINCT mb.mailbox_id),
		       array_agg(DISTINCT bx.account_id)
		  FROM membership mb
		  JOIN app.mailboxes bx ON bx.id = mb.mailbox_id AND bx.user_id = $1
		 WHERE mb.message_id = ANY($2)
		 GROUP BY mb.message_id`,
		tx.userID.String(), idStrs)
	if err != nil {
		return fmt.Errorf("store: index messages (provenance): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var mailboxIDs, accountIDs []string
		if err := rows.Scan(&messageID, &mailboxIDs, &accountIDs); err != nil {
			return fmt.Errorf("store: scan index provenance: %w", err)
		}
		m, ok := lookupIndexMessage(byID, messageID)
		if !ok {
			continue
		}
		var err error
		if m.MailboxIDs, err = parseULIDs(mailboxIDs); err != nil {
			return err
		}
		if m.AccountIDs, err = parseULIDs(accountIDs); err != nil {
			return err
		}
	}
	return rows.Err()
}

// attachIndexAttachments fills in each message's attachment parts (#722
// filename:, #723 has:pdf and the other type families), in part order so a
// reindex writes the same document twice.
func (tx *UserTx) attachIndexAttachments(ctx context.Context, idStrs []string, byID map[ulid.ULID]*IndexMessage) error {
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, COALESCE(filename, ''), content_type
		  FROM app.attachments
		 WHERE user_id = $1 AND message_id = ANY($2)
		 ORDER BY message_id, part_path`,
		tx.userID.String(), idStrs)
	if err != nil {
		return fmt.Errorf("store: index messages (attachments): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var a IndexAttachment
		if err := rows.Scan(&messageID, &a.Filename, &a.ContentType); err != nil {
			return fmt.Errorf("store: scan index attachment: %w", err)
		}
		if m, ok := lookupIndexMessage(byID, messageID); ok {
			m.Attachments = append(m.Attachments, a)
		}
	}
	return rows.Err()
}

func lookupIndexMessage(byID map[ulid.ULID]*IndexMessage, id string) (*IndexMessage, bool) {
	u, err := ulid.Parse(id)
	if err != nil {
		return nil, false
	}
	m, ok := byID[u]
	return m, ok
}

func parseULIDs(in []string) ([]ulid.ULID, error) {
	out := make([]ulid.ULID, 0, len(in))
	for _, s := range in {
		u, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: parse id %q: %w", s, err)
		}
		out = append(out, u)
	}
	return out, nil
}

// MessageIDsAfter walks the user's message ids in ascending id order — the scan a
// full reindex reads. `after` is an exclusive low bound (pass the zero ULID to
// start). ULIDs sort by creation time, so this is a keyset walk over the primary
// key with no offset cost and no snapshot to hold open.
func (tx *UserTx) MessageIDsAfter(ctx context.Context, after ulid.ULID, limit int) ([]ulid.ULID, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id FROM app.messages
		 WHERE user_id = $1 AND id > $2
		 ORDER BY id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk message ids: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan message id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: walk message ids: %w", err)
	}
	return out, nil
}

// LabelIDByName resolves a label name (case-insensitive) to its id — how
// `label:Work` in a search query becomes the label id the index actually stores.
func (tx *UserTx) LabelIDByName(ctx context.Context, name string) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.labels
		 WHERE user_id = $1 AND lower(name) = lower($2)
		 ORDER BY id
		 LIMIT 1`,
		tx.userID.String(), name).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: label by name: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse label id %q: %w", id, err)
	}
	return u, true, nil
}

// AccountIDByName resolves an account name (case-insensitive) to its id — how
// `account:personal` becomes an account id.
func (tx *UserTx) AccountIDByName(ctx context.Context, name string) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.accounts
		 WHERE user_id = $1 AND lower(name) = lower($2)
		 ORDER BY id
		 LIMIT 1`,
		tx.userID.String(), name).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: account by name: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse account id %q: %w", id, err)
	}
	return u, true, nil
}

// IndexStalenessSeconds reports how far behind (in wall-clock seconds) the
// user's search index is, given the change-log sequence it has last indexed
// (sinceSeq — typically search.Stats.Cursor). It is the age of the OLDEST
// un-indexed Email change: "the index is N seconds behind" means there is a
// change that old still waiting to be searchable, which is what spec §7's
// "search may be N minutes stale" surfaces to the user (mailroom#41).
//
// Returns 0 when the index is caught up (sinceSeq >= every Email change_log
// row, so no rows match seq > sinceSeq).
func (tx *UserTx) IndexStalenessSeconds(ctx context.Context, sinceSeq int64) (float64, error) {
	var seconds float64
	err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)
		  FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Email' AND seq > $2`,
		tx.userID.String(), sinceSeq,
	).Scan(&seconds)
	if err != nil {
		return 0, fmt.Errorf("store: index staleness: %w", err)
	}
	return seconds, nil
}
