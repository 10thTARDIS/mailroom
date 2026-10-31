// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// This file holds the message-snippet backfill store surface used by the
// mailroom-admin `resnippet` command: a keyset reader over a user's messages
// (id + content_hash + current snippet) and a guarded snippet update.
//
// The snippet IS a JMAP-visible property (Email.preview), so an actual change to
// it appends exactly one Email `updated` change_log row in the SAME UserTx as
// the UPDATE — the load-bearing invariant (CLAUDE.md). A no-op update (value
// already identical, or the row does not exist for the caller) appends NOTHING,
// so a re-run of the backfill costs no needless change_log churn or client
// re-sync.

// MessageBlobRef is one message's identity for the resnippet scan: its id, the
// content_hash that keys its raw blob, and the snippet currently stored. The
// current snippet lets a dry run report what WOULD change without decrypting a
// second time or issuing a write.
type MessageBlobRef struct {
	ID          ulid.ULID
	ContentHash []byte
	Snippet     string
}

// MessageAuthRef is one message's blob reference plus the two columns the
// authentication report reads alongside the header it parses out of the blob
// (#55 increment 0).
type MessageAuthRef struct {
	ID ulid.ULID
	// ProviderDated: received_at is a provider INTERNALDATE (providerDatedExpr:
	// no local mailbox and a provenance row). Use TrustTime for trust.
	ProviderDated bool
	ContentHash   []byte
	FromAddress   string
	// LocallyAuthored is true when the message has a local_mailbox_id — Mailroom
	// wrote it (a draft or a Sent copy) rather than receiving it.
	//
	// ⚠ IT IS THE DESIGN CONSTRAINT ON ANY SENDER-AUTH DISPLAY (#55). Locally
	// authored mail never transits an authenticating hop, so it carries no
	// Authentication-Results and lands in the ABSENT population. A badge that
	// showed "unverified" on the user's own sent mail would be actively wrong,
	// so the report has to be able to say how much of ABSENT this is rather
	// than leaving it to be inferred from a few example addresses.
	LocallyAuthored bool
	// HasAttachments is app.messages.has_attachments as ingest STORED it —
	// `len(mime.Message.Attachments) > 0` at the time the message arrived.
	//
	// It is carried so a report can compare it PER MESSAGE against what the
	// parser says today. Comparing two totals cannot do that job: it is unable
	// to distinguish four messages newly seen as having attachments from six
	// newly seen and two lost, and those call for opposite investigations.
	HasAttachments bool
	// StoredAuthFail is app.messages.auth_fail — what ingest PERSISTED. The
	// report parses the header again and compares, because a divergence between
	// the stored signal and a live parse of the same bytes is a finding rather
	// than a statistic: one of them is wrong.
	StoredAuthFail bool
	// ReceivedAt is app.messages.received_at, so a report can bucket by the
	// month mail arrived (`auth-report --authserv`, v1.90.1): a provider
	// migration and a change of receiving MX look identical in a total and
	// different over time. Set by MessageAuthRefsAfter and MessageAuthRefByID
	// (the image prefetch's arrival window, #922).
	ReceivedAt time.Time
}

// MessageAuthRefsAfter walks the caller's messages in ascending id order, the
// same keyset walk as MessageBlobRefsAfter, carrying the columns the
// authentication report needs. RLS scopes the scan to the caller.
func (tx *UserTx) MessageAuthRefsAfter(ctx context.Context, after ulid.ULID, limit int) ([]MessageAuthRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, content_hash, COALESCE(from_address::text, ''), auth_fail,
		       local_mailbox_id IS NOT NULL, has_attachments, received_at,
		       `+providerDatedExpr("app.messages")+`
		  FROM app.messages
		 WHERE user_id = $1 AND id > $2
		 ORDER BY id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk message auth refs: %w", err)
	}
	defer rows.Close()

	var out []MessageAuthRef
	for rows.Next() {
		var (
			r     MessageAuthRef
			idStr string
		)
		if err := rows.Scan(&idStr, &r.ContentHash, &r.FromAddress, &r.StoredAuthFail, &r.LocallyAuthored, &r.HasAttachments, &r.ReceivedAt, &r.ProviderDated); err != nil {
			return nil, fmt.Errorf("store: scan message auth ref: %w", err)
		}
		if r.ID, err = ulid.Parse(idStr); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", idStr, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: walk message auth refs rows: %w", err)
	}
	return out, nil
}

// MessageBlobRefsAfter walks the caller's messages in ascending id order,
// returning each message's blob reference and current snippet. `after` is an
// exclusive low bound (pass the zero ULID to start); ULIDs sort by creation
// time, so this is a keyset walk over the primary key with no offset cost and no
// long-lived snapshot — a large mailbox streams page by page. RLS scopes the
// scan to the caller.
func (tx *UserTx) MessageBlobRefsAfter(ctx context.Context, after ulid.ULID, limit int) ([]MessageBlobRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, content_hash, snippet
		  FROM app.messages
		 WHERE user_id = $1 AND id > $2
		 ORDER BY id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk message blob refs: %w", err)
	}
	defer rows.Close()
	var out []MessageBlobRef
	for rows.Next() {
		var (
			id   string
			ref  MessageBlobRef
			hash []byte
		)
		if err := rows.Scan(&id, &hash, &ref.Snippet); err != nil {
			return nil, fmt.Errorf("store: scan message blob ref: %w", err)
		}
		if ref.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		ref.ContentHash = hash
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: walk message blob refs: %w", err)
	}
	return out, nil
}

// UpdateMessageSnippet sets a message's list-preview snippet and, when the value
// actually changes, appends exactly one Email `updated` change_log row in the
// same transaction so clients re-sync the new preview (the invariant). It is a
// guarded UPDATE: `snippet IS DISTINCT FROM $3` writes no row when the snippet is
// already identical, and the same no-row outcome covers a message that does not
// exist for the caller (RLS) — both are true no-ops that append NOTHING and
// return changed=false. There is no writeback intent: the preview is a local
// derivation, not a remote-mirrored property.
func (tx *UserTx) UpdateMessageSnippet(ctx context.Context, msgID ulid.ULID, snippet string) (changed bool, err error) {
	// $3 (the snippet text) is reused across SET and the guard, but both uses are
	// the text column's type, so no 42P08 param-type ambiguity arises; $2 (id) is
	// used once, so no ::app.ulid cast is needed either.
	var touched string
	err = tx.tx.QueryRow(ctx, `
		UPDATE app.messages
		   SET snippet = $3
		 WHERE user_id = $1 AND id = $2
		   AND snippet IS DISTINCT FROM $3
		RETURNING id`,
		tx.userID.String(), msgID.String(), snippet).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			// Already identical, or no such row for the caller: a no-op that owes
			// no change_log row.
			return false, nil
		}
		return false, fmt.Errorf("store: update message snippet: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// MessageAuthRefByID returns one message's blob reference and sender columns, or
// ErrMessageNotFound. RLS scopes the lookup to the caller, so an id belonging to
// another user is indistinguishable from an id that does not exist — which is
// the correct answer to give, not a limitation to work around.
//
// It exists for the image proxy (ADR-026 §3b): the endpoint is handed a message
// id and must resolve the URL from THAT message's stored bytes, so it needs
// exactly this row and no walk.
func (tx *UserTx) MessageAuthRefByID(ctx context.Context, id ulid.ULID) (MessageAuthRef, error) {
	var r MessageAuthRef
	err := tx.tx.QueryRow(ctx, `
		SELECT id, content_hash, COALESCE(from_address::text, ''), auth_fail,
		       local_mailbox_id IS NOT NULL, received_at
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()).Scan(
		&r.ID, &r.ContentHash, &r.FromAddress, &r.StoredAuthFail, &r.LocallyAuthored, &r.ReceivedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MessageAuthRef{}, ErrMessageNotFound
		}
		return MessageAuthRef{}, fmt.Errorf("store: message auth ref by id: %w", err)
	}
	return r, nil
}

// CountMessagesWithAttachments returns how many of this user's messages carry
// app.messages.has_attachments — the flag ingest sets from
// `len(mime.Message.Attachments) > 0`.
//
// It exists for `mailroom-admin attachment-report` (#333) as a CROSS-CHECK, not
// as the report's answer. The report re-derives attachments from the message
// bytes; this is the number ingest recorded at the time. They come from the same
// expression, so they should agree exactly, and a difference means the parser's
// classification has drifted since those messages arrived — which is worth
// discovering before a scanner is built on top of either figure.
//
// ⚠ It is NOT a count of app.attachments. That table has existed since the
// baseline, carries a scan_status column, is referenced by blobreclaim.go as a
// blob referent — and is written by nothing, so it holds zero rows on every
// deployment. Counting it would answer "there is nothing to scan" for a corpus
// that demonstrably has attachments.
func (tx *UserTx) CountMessagesWithAttachments(ctx context.Context) (int, error) {
	var n int
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND has_attachments`,
		tx.userID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count messages with attachments: %w", err)
	}
	return n, nil
}

// AttachmentCoverage is the census of app.attachments for one user: how many
// rows exist, in what scan state, and — the number this is actually for — how
// completely the projection covers the messages that carry attachments.
//
// ⚠ IT IS DELIBERATELY NOT A SINGLE TOTAL (#177, #191, #206). "0 rows" and "0
// rows because nothing has ever written this table" are the same number and
// different findings, and this corpus is the second. MessagesWithRows against
// MessagesFlagged is what tells them apart: equal means covered, and
// MessagesWithRows == 0 while MessagesFlagged > 0 means the projection does not
// exist yet rather than that there is nothing to scan.
type AttachmentCoverage struct {
	// Rows per scan_status, exactly as stored. NOT normalised to a fixed set of
	// keys: an unexpected value is a finding, and mapping it into a known bucket
	// would hide it.
	ByScanStatus map[string]int
	// Total rows in app.attachments for this user.
	Rows int
	// Rows whose content_hash is set, i.e. whose bytes have been extracted to a
	// blob. ⚠ This is the count that makes blobreclaim.go's attachments clause
	// production-live: until it is non-zero, that clause matches nothing (ADR-027
	// §3). It is covered by tests in both directions; what has never happened is
	// it mattering on real data.
	WithContentHash int
	// Distinct messages having at least one app.attachments row.
	MessagesWithRows int
	// Messages whose has_attachments flag is set — what MessagesWithRows should
	// equal once the projection is complete and backfilled.
	MessagesFlagged int
}

// AttachmentCoverage reports the state of the app.attachments projection.
//
// It exists BEFORE the projection is written (ADR-027 increment 1 before
// increment 2) and that ordering is the point: a backfill is precisely the kind
// of change where "it ran and changed nothing" is a plausible outcome, so the
// measurement has to predate the fix rather than be written afterwards to
// confirm it.
//
// ⚠ WHAT IT CANNOT YET REPORT, AND WHY. ADR-027 §13 also asks for counts per
// re-scan tier, the oldest verdict, and stamped-vs-current signature version.
// None of those columns exist — app.attachments carries scan_status and nothing
// else about a verdict. They arrive with the increments that add them, and
// inventing them here would report fields the schema cannot back.
func (tx *UserTx) AttachmentCoverage(ctx context.Context) (AttachmentCoverage, error) {
	c := AttachmentCoverage{ByScanStatus: map[string]int{}}

	rows, err := tx.tx.Query(ctx,
		`SELECT scan_status, count(*), count(content_hash)
		   FROM app.attachments
		  WHERE user_id = $1
		  GROUP BY scan_status`,
		tx.userID.String())
	if err != nil {
		return AttachmentCoverage{}, fmt.Errorf("store: attachment coverage by state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n, hashed int
		if err := rows.Scan(&state, &n, &hashed); err != nil {
			return AttachmentCoverage{}, fmt.Errorf("store: attachment coverage scan: %w", err)
		}
		c.ByScanStatus[state] = n
		c.Rows += n
		c.WithContentHash += hashed
	}
	if err := rows.Err(); err != nil {
		return AttachmentCoverage{}, fmt.Errorf("store: attachment coverage rows: %w", err)
	}

	if err := tx.tx.QueryRow(ctx,
		`SELECT (SELECT count(DISTINCT message_id) FROM app.attachments WHERE user_id = $1),
		        (SELECT count(*) FROM app.messages WHERE user_id = $1 AND has_attachments)`,
		tx.userID.String()).Scan(&c.MessagesWithRows, &c.MessagesFlagged); err != nil {
		return AttachmentCoverage{}, fmt.Errorf("store: attachment coverage messages: %w", err)
	}
	return c, nil
}

// SetMessageHasAttachments repairs app.messages.has_attachments for one message
// and, when the value actually changes, appends exactly one Email `updated`
// change_log row in the same transaction.
//
// ⚠ THE CHANGE ROW IS NOT OPTIONAL, AND THIS IS THE INVARIANT THAT #351 B4 WAS
// CAUGHT VIOLATING. has_attachments is served as the JMAP Email property
// `hasAttachment` (internal/api/email.go) and drawn by the client as the
// paperclip in the message list. Repairing it silently would leave every synced
// client showing the old marker indefinitely — the row is what tells them to
// look again.
//
// Guarded on `IS DISTINCT FROM`, so a value that is already correct writes no
// row and appends nothing. That makes a re-run of the backfill free rather than
// a source of change_log churn and needless client re-sync, and it means the
// no-op case and the "no such row for this caller" case (RLS) are one path.
//
// There is no writeback intent: has_attachments is a local derivation of bytes
// we already hold, not a property mirrored to the IMAP server.
func (tx *UserTx) SetMessageHasAttachments(ctx context.Context, msgID ulid.ULID, has bool) (changed bool, err error) {
	var touched string
	err = tx.tx.QueryRow(ctx, `
		UPDATE app.messages
		   SET has_attachments = $3
		 WHERE user_id = $1 AND id = $2
		   AND has_attachments IS DISTINCT FROM $3
		RETURNING id`,
		tx.userID.String(), msgID.String(), has).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: set message has_attachments: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// TrustTime is the received time to hand ml.TrustedAt (zero when the provider
// did not date the message; see ProviderDated).
func (r MessageAuthRef) TrustTime() time.Time { return TrustTime(r.ReceivedAt, r.ProviderDated) }
