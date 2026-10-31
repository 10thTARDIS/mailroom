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

// Quarantine state: what the download gate reads, and the release that opens it
// (#396, ADR-027 §8).
//
// # Two objects, and the reason they are not one
//
// A VERDICT is a fact about bytes that a scanner produced, and it lives on the
// attachment row because that is where the scanner's answer about that part
// belongs. A RELEASE is a decision a human made about the same bytes, and it
// lives in app.attachment_releases keyed on the content digest.
//
// ⚠ ADR-027 §5 modelled the release as a fifth value of `scan_status`, and
// migration 00044 records the three reasons it moved. The one that matters most
// here is the first: §8 requires a release to reach EVERY copy of the content,
// and the same attachment sits on N different messages (in practice one
// newsletter image can sit on a dozen messages). As a column value that is an N-row
// write which a copy arriving LATER never receives. As a row keyed on
// part_hash it is one write and every copy, past and future, is covered by
// construction.
//
// # What the gate blocks, and what it does not
//
// `flagged` and `unknown` both block, with different wording, per §5b: if
// `unknown` were a badge with a free download the adversary never has to beat
// the scanner, only avoid it — an encrypted zip with the password in the body
// is commodity technique.
//
// ⚠ `unscanned` DOES NOT BLOCK, and that is a decision rather than an
// oversight. It means "no scanner configured, or not yet reached". An instance
// running no scanner at all has every row `unscanned`, and blocking those would
// break every attachment in a mailbox whose operator never asked for a scanner.
// It is also the state of outbound mail the user wrote themselves (ADR-027 §14)
// and of rows the projection backfill has created but re-scan has not yet
// visited. Inbound mail cannot rest there while a scanner IS configured: ingest
// is fail-closed, so a daemon outage withholds the whole sync pass rather than
// admitting rows with no verdict, and a per-file failure writes `unknown`. The
// state is visible in the aggregate (§5d) and in the JMAP property, so "not
// checked" is something a client can say out loud.

// PartScanState is one attachment part's quarantine state, as the gate, the
// JMAP property and the exporter all read it.
type PartScanState struct {
	// PartPath is the MIME part path, which is also JMAP's partId — the same
	// derivation on both sides (internal/mime.PartPaths), so a state recorded
	// here names a part a blobId resolves to.
	PartPath string
	// Status is 'unscanned'|'clean'|'flagged'|'unknown'. Never 'released'.
	Status string
	// Released is true when the user has overridden a quarantine on these
	// bytes. ⚠ It is INDEPENDENT of Status rather than replacing it: the
	// verdict stays `flagged` and keeps being refreshed by re-scan, so a client
	// can say "we flagged this, you released it" instead of losing the reason.
	Released bool
	// ReleasedAt is when that override was recorded, or nil when there is
	// none. ⚠ IT IS THE USER'S ACTION, NOT THE SCANNER'S: unlike verdict age
	// and signature-DB version (deliberately unexposed, see attachment_scan.go),
	// this timestamp never moves on a re-scan — it only ever changes when the
	// user releases or revokes, so syncing it costs no extra change churn.
	ReleasedAt *time.Time
	// PartHash is the content digest, or nil for a part no scan has reached.
	// ⚠ nil is what makes a release impossible: releasing content requires
	// naming it, and there is nothing honest to key on until a scan has run.
	PartHash []byte
}

// ErrAttachmentNotFound is returned when no projected row names the part.
var ErrAttachmentNotFound = errors.New("store: no such attachment part")

// ErrAttachmentNoContentHash is returned by a release against a part that has
// never been scanned, and therefore has no content digest to key the release
// on. ⚠ It is an ERROR rather than a silently narrower release: releasing "this
// row" while believing you released the content is precisely the
// hit-1-of-N-copies bug §8 names.
var ErrAttachmentNoContentHash = errors.New("store: attachment has no content digest; it has not been scanned")

// AttachmentScanState reads one part's quarantine state. found is false when
// the message carries no projected row for that path — which is the ordinary
// case for a text body part, not an error.
func (tx *UserTx) AttachmentScanState(ctx context.Context, messageID ulid.ULID, partPath string) (PartScanState, bool, error) {
	st := PartScanState{PartPath: partPath}
	err := tx.tx.QueryRow(ctx, `
		SELECT a.scan_status,
		       a.part_hash,
		       r.released_at
		  FROM app.attachments a
		  LEFT JOIN app.attachment_releases r
		         ON r.user_id = a.user_id AND r.part_hash = a.part_hash
		 WHERE a.user_id = $1 AND a.message_id = $2 AND a.part_path = $3`,
		tx.userID.String(), messageID.String(), partPath).
		Scan(&st.Status, &st.PartHash, &st.ReleasedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PartScanState{}, false, nil
	}
	if err != nil {
		return PartScanState{}, false, fmt.Errorf("store: attachment scan state: %w", err)
	}
	st.Released = st.ReleasedAt != nil
	return st, true, nil
}

// AttachmentScanStates reads every projected part's state for the given
// messages, for the JMAP property and the exporter.
//
// ⚠ A message with no attachment rows is ABSENT from the map rather than
// present-and-empty, and callers must keep those apart: "this message has no
// attachments" and "this message's parts have not been projected" are different
// facts, and only the second is a reason to go looking.
func (tx *UserTx) AttachmentScanStates(ctx context.Context, messageIDs []ulid.ULID) (map[ulid.ULID][]PartScanState, error) {
	out := map[ulid.ULID][]PartScanState{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		ids = append(ids, id.String())
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT a.message_id, a.part_path, a.scan_status, a.part_hash,
		       r.released_at
		  FROM app.attachments a
		  LEFT JOIN app.attachment_releases r
		         ON r.user_id = a.user_id AND r.part_hash = a.part_hash
		 WHERE a.user_id = $1 AND a.message_id = ANY($2::text[])
		 ORDER BY a.message_id, a.part_path`,
		tx.userID.String(), ids)
	if err != nil {
		return nil, fmt.Errorf("store: attachment scan states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid string
		var st PartScanState
		if err := rows.Scan(&mid, &st.PartPath, &st.Status, &st.PartHash, &st.ReleasedAt); err != nil {
			return nil, fmt.Errorf("store: scan state row: %w", err)
		}
		st.Released = st.ReleasedAt != nil
		u, perr := ulid.Parse(mid)
		if perr != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", mid, perr)
		}
		out[u] = append(out[u], st)
	}
	return out, rows.Err()
}

// ReleaseAttachment records the user's override of a quarantine on the CONTENT
// the named part carries, and returns the digest it released plus every message
// whose view of that content changed.
//
// ⚠ IT RELEASES BYTES, NOT A ROW (ADR-027 §8). Every copy the user holds is
// covered, including copies that arrive afterwards — those need no further
// action, because the release is keyed on the digest rather than on the rows
// that happened to exist when the decision was made.
//
// Every affected message is a JMAP-visible change, so each gets a change_log
// row in this transaction.
func (tx *UserTx) ReleaseAttachment(ctx context.Context, messageID ulid.ULID, partPath string) ([]byte, []ulid.ULID, error) {
	st, found, err := tx.AttachmentScanState(ctx, messageID, partPath)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, ErrAttachmentNotFound
	}
	if len(st.PartHash) == 0 {
		return nil, nil, ErrAttachmentNoContentHash
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.attachment_releases
			(user_id, part_hash, released_from_message_id, released_from_part)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, part_hash) DO NOTHING`,
		tx.userID.String(), st.PartHash, messageID.String(), partPath); err != nil {
		return nil, nil, fmt.Errorf("store: record attachment release: %w", err)
	}
	affected, err := tx.messagesCarrying(ctx, st.PartHash)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.appendEmailChanges(ctx, affected); err != nil {
		return nil, nil, err
	}
	return st.PartHash, affected, nil
}

// RevokeAttachmentRelease undoes a release, re-arming the gate for that content.
// It returns the messages whose view changed, and nil when there was no release
// to undo.
func (tx *UserTx) RevokeAttachmentRelease(ctx context.Context, partHash []byte) ([]ulid.ULID, error) {
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.attachment_releases WHERE user_id = $1 AND part_hash = $2`,
		tx.userID.String(), partHash)
	if err != nil {
		return nil, fmt.Errorf("store: revoke attachment release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	affected, err := tx.messagesCarrying(ctx, partHash)
	if err != nil {
		return nil, err
	}
	return affected, tx.appendEmailChanges(ctx, affected)
}

// messagesCarrying lists every message of this user holding a part with the
// given content digest, in id order.
func (tx *UserTx) messagesCarrying(ctx context.Context, partHash []byte) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT message_id FROM app.attachments
		 WHERE user_id = $1 AND part_hash = $2
		 ORDER BY message_id`,
		tx.userID.String(), partHash)
	if err != nil {
		return nil, fmt.Errorf("store: messages carrying content: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: carrying message row: %w", err)
		}
		u, perr := ulid.Parse(id)
		if perr != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, perr)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// appendEmailChanges appends one Email change row per message, in the caller's
// transaction — the load-bearing invariant (CLAUDE.md, spec §13/§14). The scan
// verdict and the release are both JMAP-visible properties of an Email, so a
// client that never sees these rows silently keeps a stale quarantine state.
func (tx *UserTx) appendEmailChanges(ctx context.Context, messageIDs []ulid.ULID) error {
	// ⚠ ONE PAIR OF STATEMENTS, NOT ONE PER MESSAGE (#417). This is the call
	// that propagates a quarantine to every copy of the same content, and on a
	// real mailbox "every copy" can be ten thousand messages — see
	// AppendChanges for why a loop here held the change_seq lock long enough to
	// stall every other Email write for the account.
	return tx.AppendChanges(ctx, ObjectEmail, messageIDs, OpUpdated)
}

// CountAttachmentReleases returns how many distinct contents the user has
// released, and how many attachment rows those cover — for `attachment-report`.
//
// ⚠ TWO NUMBERS, NOT ONE. One release can cover twelve rows, and the gap
// between them is the whole point of keying on content: a report showing only
// "1 released" would hide that a single decision opened twelve doors.
func (tx *UserTx) CountAttachmentReleases(ctx context.Context) (releases, rowsCovered int, err error) {
	err = tx.tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM app.attachment_releases WHERE user_id = $1),
		       (SELECT count(*) FROM app.attachments a
		         WHERE a.user_id = $1 AND a.part_hash IS NOT NULL
		           AND EXISTS (SELECT 1 FROM app.attachment_releases r
		                        WHERE r.user_id = $1 AND r.part_hash = a.part_hash))`,
		tx.userID.String()).Scan(&releases, &rowsCovered)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count attachment releases: %w", err)
	}
	return releases, rowsCovered, nil
}

// PartReleased reports whether the user has released content with this digest
// (app.attachment_releases, keyed on attachscan.PartHash). It is how a part
// that exists only in a request — an attachment inside a decrypted message
// (#47, ADR-037), scanned on demand — honours a release the user already made
// for the same bytes. Read-only.
func (tx *UserTx) PartReleased(ctx context.Context, partHash []byte) (bool, error) {
	if len(partHash) == 0 {
		return false, nil
	}
	var released bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.attachment_releases
		                WHERE user_id = $1 AND part_hash = $2)`,
		tx.userID.String(), partHash).Scan(&released); err != nil {
		return false, fmt.Errorf("store: part released: %w", err)
	}
	return released, nil
}
