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

// The app.attachments projection (#393, ADR-027 §3).
//
// # What this table is for, and what it is not
//
// It is NOT where attachments live. The bytes live in the message blob, and
// every attachment view the client sees is derived from those bytes on demand
// (internal/api/body.go). This table exists so that something can be SAID about
// a part — currently only `scan_status`, eventually a full verdict — which has
// nowhere to live when the only representation of a part is a substring of a
// blob computed at read time.
//
// ⚠ IT HAD NEVER BEEN WRITTEN BEFORE THIS. The table shipped in the scaffold
// commit with a `scan_status` column already reading
// 'unscanned'|'clean'|'flagged', and until #393 no production code inserted a
// row: measured at 0 rows against messages carrying has_attachments. Two
// consequences worth carrying forward:
//
//   - `internal/store/blobreclaim.go`'s attachments clause becomes
//     PRODUCTION-LIVE the moment content_hash here is non-null. The guard is
//     correct and tested in both directions; what has never happened is it
//     mattering on real data.
//   - Anything that reasoned about this table being empty is now wrong.
//
// # change_log rows, on transitions only
//
// This is what the warning that used to stand here was about. From ADR-027
// increment 5 (#396) the verdict IS a JMAP property, so the load-bearing
// invariant applies and writeVerdicts appends an Email change row in the same
// transaction as the mutation.
//
// ⚠ ON TRANSITIONS, NOT ON WRITES, and the distinction is the difference
// between correct sync and a weekly full resync. Re-scan rewrites every row of
// every due message on every pass; §5c's tiers keep the whole corpus in an
// actively re-scanned band. A change row per UPDATE would be hundreds of Email
// changes a week for verdicts that never moved — exactly the noise §9 refuses
// for verdict age and DB version. See writeVerdicts.

// AttachmentRow is one projected attachment part.
type AttachmentRow struct {
	// PartPath is the IMAP-style path within the message ("2.1"), from
	// mime.PartPaths — the SAME derivation JMAP's partId uses, so a verdict
	// recorded here names a part a download URL can resolve.
	PartPath string
	// Filename as the sender supplied it, already sanitized by the parser.
	// May be empty; many legitimate parts carry none.
	Filename string
	// ContentType is the DECLARED media type. Deliberately not the sniffed
	// one: this is a record of what the message claims, and the gap between
	// claim and bytes is itself a signal a scanner wants.
	ContentType string
	// SizeBytes is the part's decoded size.
	SizeBytes int64
	// ContentHash is the blob id of the part's bytes, or nil when the part
	// has not been extracted to its own blob. ⚠ Non-nil is what arms the
	// reclaim guard's attachments clause.
	ContentHash []byte
}

// ReplaceAttachments makes app.attachments for one message exactly match rows.
//
// Idempotent by construction, because it has to be: ingest can re-run over the
// same message (a redelivery, a repaired sync), and a backfill runs over mail
// that may already be projected. Re-running must converge rather than
// accumulate or fail.
//
// ⚠ IT DELETES ROWS THAT ARE NO LONGER PRESENT. A message's bytes are immutable
// under ADR-001 with exactly one exception — PGP signing rewrites them at
// Submit — so the part list CAN legitimately change for locally-authored mail.
// That is the same rewrite that made has_attachments wrong on signed sent mail
// (#377), and leaving stale rows behind would repeat the defect in a new table.
func (tx *UserTx) ReplaceAttachments(ctx context.Context, messageID ulid.ULID, rows []AttachmentRow) error {
	// The delete is scoped by part_path so a concurrent writer adding a part
	// cannot have it removed by a stale caller: only paths absent from THIS
	// caller's view go.
	keep := make([]string, 0, len(rows))
	for _, r := range rows {
		keep = append(keep, r.PartPath)
	}
	if _, err := tx.tx.Exec(ctx, `
		DELETE FROM app.attachments
		 WHERE user_id = $1 AND message_id = $2
		   AND NOT (part_path = ANY($3::text[]))`,
		tx.userID.String(), messageID.String(), keep); err != nil {
		return fmt.Errorf("store: prune attachments: %w", err)
	}

	for _, r := range rows {
		var hash any
		if len(r.ContentHash) > 0 {
			hash = r.ContentHash
		}
		// ON CONFLICT on the table's own UNIQUE (message_id, part_path).
		// scan_status is NOT touched on conflict — a re-ingest must never
		// silently reset a verdict back to 'unscanned'. That is the whole
		// reason this is an upsert of the descriptive columns only.
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.attachments (
				id, user_id, message_id, part_path,
				filename, content_type, size_bytes, content_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (message_id, part_path) DO UPDATE SET
				filename     = EXCLUDED.filename,
				content_type = EXCLUDED.content_type,
				size_bytes   = EXCLUDED.size_bytes,
				content_hash = EXCLUDED.content_hash`,
			NewULID().String(), tx.userID.String(), messageID.String(), r.PartPath,
			nullIfEmpty(r.Filename), r.ContentType, r.SizeBytes, hash); err != nil {
			return fmt.Errorf("store: upsert attachment %q: %w", r.PartPath, err)
		}
	}
	return nil
}

// nullIfEmpty maps "" to SQL NULL. filename is nullable and an empty string
// would be a different value from "absent" to anything reading it later.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// MessagesMissingAttachmentRows returns ids of messages that carry
// has_attachments but have no projected rows, after the given id, for the
// backfill to walk in ULID order.
//
// ⚠ IT IS DELIBERATELY NOT "messages with attachments". The population that
// matters is the DISAGREEMENT — projected coverage against what ingest
// recorded — because that is the number `attachment-report` prints and the one
// that says whether the backfill has anything to do.
func (tx *UserTx) MessagesMissingAttachmentRows(ctx context.Context, after ulid.ULID, limit int) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id
		  FROM app.messages m
		 WHERE m.user_id = $1
		   AND m.has_attachments
		   AND m.id > $2
		   AND NOT EXISTS (SELECT 1 FROM app.attachments a
		                    WHERE a.user_id = $1 AND a.message_id = m.id)
		 ORDER BY m.id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: messages missing attachment rows: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan missing-attachment id: %w", err)
		}
		u, perr := ulid.Parse(id)
		if perr != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, perr)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: missing-attachment rows: %w", err)
	}
	return out, nil
}

// ScanVerdict is one attachment's scan outcome, ready to store.
type ScanVerdict struct {
	PartPath string
	// Status is one of 'unscanned'|'clean'|'flagged'|'unknown'. ⚠ 'released'
	// is not among them: a release is a human decision about CONTENT and lives
	// in app.attachment_releases, where nothing a scan writes can reach it
	// (migration 00044). Before that it was a value here, guarded by a WHERE
	// clause every writer had to remember.
	Status string
	// Signature is clamd's own name for what matched, for the operator's log.
	// Empty for clean.
	Signature string
	// PartHash is the SHA-256 of the part's DECODED bytes — the content
	// identity a quarantine and a release act on (ADR-027 §8). Computed in
	// internal/attachscan, where both scanning paths already hold the bytes, so
	// ingest and re-scan cannot derive it differently and neither pays an extra
	// read.
	//
	// ⚠ NOT app.attachments.content_hash, which is a BLOB REFERENCE the reclaim
	// guard reads and which this projection deliberately leaves NULL. See
	// migration 00044.
	PartHash []byte
}

// ScanStamp is the scanner state that produced a set of verdicts: when they
// were made, and what the daemon knew at that moment.
//
// ⚠ IT IS A REQUIRED PARAMETER, NOT AN OPTION, AND THAT IS THE FIX FOR #407.
// There used to be two write paths — SetScanVerdicts for ingest and
// StampScanVerdicts for re-scan — and only the second one stamped. So a message
// scanned the instant it arrived got a real verdict and three NULL freshness
// columns, which made `attachment-report` count freshly scanned mail as "never
// scanned" (the one field whose whole job is to make a stalled backlog legible)
// and made every new message due for an immediate redundant re-scan. Two entry
// points, one of which forgot, IS the defect; collapsing them into one method
// with an explicit stamp forces every caller to answer the question.
//
// Nil is still legal and still means "no stamp available" — a scanner whose
// VERSION could not be read. The row then stays unstamped, which is honest and
// self-healing: re-scan's IS NULL arm picks it up.
//
// ⚠ THE VERSION MUST BE THE ONE READ *BEFORE* THE SCAN. If freshclam updates in
// between, stamping the newer number claims a scan against signatures that were
// not loaded when the bytes went through, and the row then looks current and
// stops being re-scanned. Stamping the older one is self-healing instead: the
// row stays behind the new current and the next pass picks it up.
type ScanStamp struct {
	At        time.Time
	DBVersion int
	Engine    string
}

// SetScanVerdicts records scan outcomes for one message's parts, together with
// the scanner state that produced them.
//
// ⚠ IT DOES NOT REFUSE TO OVERWRITE 'released', because that state does not live
// in this column. ADR-027 §5's concern was exactly right — a malicious verdict
// essentially never reverts, so a re-scan WILL re-flag an attachment a human
// already released, resurrecting the quarantine on a cadence — and migration
// 00044 answers it structurally instead of with a WHERE clause every writer had
// to remember: the release is a row in app.attachment_releases keyed on the
// content digest, which nothing a scanner writes can reach. The verdict here
// stays `flagged` and keeps being refreshed, and the gate stays open. There is
// no fight to have.
func (tx *UserTx) SetScanVerdicts(ctx context.Context, messageID ulid.ULID, verdicts []ScanVerdict, stamp *ScanStamp) error {
	return tx.writeVerdicts(ctx, messageID, verdicts, stamp)
}

// RecordScanAttempt marks every projected attachment of one message as VISITED
// by a scan pass at this signature version, without touching the verdict, its
// freshness, or the digest.
//
// ⚠ THIS IS WHAT MAKES THE RE-SCAN QUEUE TERMINATE, and it is the whole of
// #409/#415 on the write side. A pass that reads a blob and fails, parses a
// message and finds no parts, or sends bytes and gets an error, produces no
// verdict — so before this, nothing about the row changed and it was due again
// thirty seconds later, forever. Since MessagesDueForRescan is
// `ORDER BY m.id LIMIT 50`, fifty such rows starve every message behind them
// while the job logs a full chunk each pass.
//
// ⚠ IT DELIBERATELY DOES NOT TOUCH scanned_at. That column is what ADR-027 §1a
// surfaces as the age of the verdict, and it is the entire justification for the
// `> 6 months` tier never being re-scanned. Refreshing it here would tell the
// operator a verdict is fresh when nothing was scanned — a lie in the one
// surface that exists to prevent it.
//
// ⚠ AND IT APPENDS NO change_log ROW, correctly: nothing JMAP-visible moved. The
// verdict, the digest and the gate are all exactly as they were. What changed is
// bookkeeping about our own queue, which no client can see.
func (tx *UserTx) RecordScanAttempt(ctx context.Context, messageID ulid.ULID, dbVersion int, at time.Time) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.attachments
		   SET attempted_at = $3, attempted_db_version = $4
		 WHERE user_id = $1 AND message_id = $2`,
		tx.userID.String(), messageID.String(), at.UTC(), dbVersion); err != nil {
		return fmt.Errorf("store: record scan attempt: %w", err)
	}
	return nil
}

// writeVerdicts is the ONE write path for a scan result, shared by ingest and
// re-scan so the two cannot diverge on the three things below.
//
// ⚠ 1. CHANGE ROWS ARE APPENDED ON TRANSITIONS ONLY. The verdict is
// JMAP-visible from ADR-027 increment 5, so the load-bearing invariant applies
// — but applying it to every UPDATE would be a disaster rather than a
// correctness win. Re-scan rewrites every row of every due message on every
// pass, and §5c puts this whole corpus in an actively re-scanned tier: that is
// ~257 Email change rows a week, forever, for verdicts that did not move,
// forcing a full client refetch each time. Exactly the sync noise §9 refuses
// for verdict age and DB version. So the UPDATE reports the PREVIOUS status and
// only a genuine move appends a row.
//
// ⚠ 2. A FLAG PROPAGATES TO EVERY COPY OF THE SAME CONTENT (§8). The same
// attachment sits on N different messages, and a re-scan that flags one copy
// while another still reads `clean` from an older signature DB leaves the
// identical bytes downloadable through the other message — the
// writeback-hit-1-of-N-copies bug in a new place. Propagating at WRITE time
// rather than resolving at read time keeps the stored state honest, so the
// gate, the §5d aggregate, the JMAP property and the export all say the same
// thing.
//
// ⚠ 3. IT DOES NOT PROPAGATE `clean`. The downgrade direction is left to the
// tiers: if a signature is retracted upstream, each copy clears when its own
// tier comes due. Propagating a clean verdict would let one message's re-scan
// unblock content another message's scanner still flags, which is the same bug
// pointed the dangerous way.
//
// ⚠ 4. A STAMPED PASS ALSO RECORDS THE ATTEMPT, and that is not the same column
// as the verdict's own freshness. scanned_* says when the VERDICT was made;
// attempted_* says when a pass last VISITED the row. On this path they move
// together, which is why the distinction is easy to miss — it exists for the
// paths where they DIVERGE (RecordScanAttempt), and the re-scan predicate's
// termination rests on every pass moving attempted_* exactly once.
func (tx *UserTx) writeVerdicts(ctx context.Context, messageID ulid.ULID, verdicts []ScanVerdict, stamp *ScanStamp) error {
	moved := false
	for _, v := range verdicts {
		var partHash any
		if len(v.PartHash) > 0 {
			partHash = v.PartHash
		}
		// ⚠ `RETURNING old.scan_status` IS PostgreSQL 18's, AND IT IS THE POINT.
		// A bare UPDATE ... RETURNING yields the NEW row, which cannot tell a
		// transition from a rewrite; this reads the pre-update tuple.
		//
		// It replaces a CTE that SELECTed the previous status and updated it in
		// the same statement (#418). That worked, but had one residual: under
		// READ COMMITTED, a concurrent commit between the statement's snapshot
		// and its row lock makes the UPDATE re-target the new tuple via EPQ
		// while the SELECT half keeps the ORIGINAL snapshot's value — so a real
		// flagged→clean move could report clean→clean and append no change row,
		// leaving a client's quarantine badge stale until the next transition.
		// `old` is the tuple the UPDATE actually replaced, EPQ included.
		sql := `
			UPDATE app.attachments a
			   SET scan_status = $4,
			       part_hash   = COALESCE($5::bytea, a.part_hash)`
		args := []any{tx.userID.String(), messageID.String(), v.PartPath, v.Status, partHash}
		if stamp != nil {
			sql += `,
			       scanned_at = $6,
			       scanned_db_version = $7,
			       scanned_engine_version = $8,
			       attempted_at = $6,
			       attempted_db_version = $7`
			args = append(args, stamp.At.UTC(), stamp.DBVersion, stamp.Engine)
		}
		sql += `
			 WHERE a.user_id = $1 AND a.message_id = $2 AND a.part_path = $3
			RETURNING old.scan_status`

		var old string
		err := tx.tx.QueryRow(ctx, sql, args...).Scan(&old)
		if errors.Is(err, pgx.ErrNoRows) {
			// No projected row for that path. Not an error: the caller scans
			// what the parser found, and a message can be re-parsed into a
			// different part list (PGP signing rewrites bytes at Submit).
			continue
		}
		if err != nil {
			return fmt.Errorf("store: set scan verdict %q: %w", v.PartPath, err)
		}
		if old != v.Status {
			moved = true
		}
		if v.Status == ScanFlagged && len(v.PartHash) > 0 {
			others, perr := tx.propagateFlag(ctx, v.PartHash, messageID)
			if perr != nil {
				return perr
			}
			if err := tx.appendEmailChanges(ctx, others); err != nil {
				return err
			}
		}
	}
	if moved {
		if _, err := tx.AppendChange(ctx, ObjectEmail, messageID, OpUpdated); err != nil {
			return fmt.Errorf("store: change row for scan verdict: %w", err)
		}
	}
	return nil
}

// ScanFlagged is the stored status a quarantine acts on. Named here rather than
// imported from internal/clamd because internal/store must not depend on a
// protocol client, and this is the value the SQL above compares.
const ScanFlagged = "flagged"

// propagateFlag marks every OTHER copy of the same content flagged, and returns
// the distinct messages it touched. See writeVerdicts' second warning.
func (tx *UserTx) propagateFlag(ctx context.Context, partHash []byte, except ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.attachments
		   SET scan_status = $4
		 WHERE user_id = $1 AND part_hash = $2 AND message_id <> $3
		   AND scan_status IS DISTINCT FROM $4
		RETURNING message_id`,
		tx.userID.String(), partHash, except.String(), ScanFlagged)
	if err != nil {
		return nil, fmt.Errorf("store: propagate flag to other copies: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: propagated flag row: %w", err)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		u, perr := ulid.Parse(id)
		if perr != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, perr)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountAttachmentsByScanStatus is the aggregate behind "is my mailbox clean"
// (ADR-027 §5d). ⚠ Per state, never a single total: "0 flagged" and "0 flagged
// because nothing has been scanned" are the same number and opposite answers.
func (tx *UserTx) CountAttachmentsByScanStatus(ctx context.Context) (map[string]int, error) {
	rows, err := tx.tx.Query(ctx,
		`SELECT scan_status, count(*) FROM app.attachments WHERE user_id = $1 GROUP BY scan_status`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: count attachments by scan status: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, fmt.Errorf("store: scan status row: %w", err)
		}
		out[s] = n
	}
	return out, rows.Err()
}
