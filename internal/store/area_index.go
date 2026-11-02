// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
)

// THE STORED RESOLVED AREA'S REPORT AND BACKFILL (ADR-049 increment 1,
// migration 00086). app.messages.area_id caches the resolver's answer and the
// triggers keep it correct for every change from then on; this file is the
// operator's side: how far the stored column is from the resolver today, and
// the batched write that closes the gap.
//
// THE ORACLE IS areaJoinSQL, NOT THE TRIGGER FUNCTIONS. Both the comparison and
// the backfill's write read the very same SQL every reader uses (areas_resolve.go),
// so a drift in the SQL twins that live in the triggers shows up here as a
// disagreement instead of being copied into the column.

// AreaIndexDisagreement is one (stored, resolved) pair that differs, with how
// many messages carry it and which arm of the resolver decided the answer.
// A nil Stored or Resolved is NULL (Needs sorting / not yet stored).
type AreaIndexDisagreement struct {
	Stored   *ulid.ULID
	Resolved *ulid.ULID
	// Via is "label" (a feeding label decided), "tab" (the resolved tab did) or
	// "none" (no area; Needs sorting).
	Via   string
	Count int
}

// AreaIndexReport is one user's stored-versus-resolver comparison.
type AreaIndexReport struct {
	Messages      int // every message the user has
	StoredSet     int // messages whose area_id is not NULL
	Disagreements []AreaIndexDisagreement
	ReadyAt       *time.Time
}

// Disagreeing is the total over Disagreements: the rows a backfill would set.
func (r AreaIndexReport) Disagreeing() int {
	n := 0
	for _, d := range r.Disagreements {
		n += d.Count
	}
	return n
}

// AreaIndexReport compares the stored column with the resolver for every
// message of the caller. Read-only.
func (tx *UserTx) AreaIndexReport(ctx context.Context) (AreaIndexReport, error) {
	var rep AreaIndexReport
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*), count(area_id) FROM app.messages WHERE user_id = $1`,
		tx.userID.String()).Scan(&rep.Messages, &rep.StoredSet); err != nil {
		return rep, fmt.Errorf("store: area index counts: %w", err)
	}
	if err := tx.tx.QueryRow(ctx,
		`SELECT ready_at FROM app.area_index_state WHERE user_id = $1`,
		tx.userID.String()).Scan(&rep.ReadyAt); err != nil && !isNoRows(err) {
		return rep, fmt.Errorf("store: area index readiness: %w", err)
	}

	args := []any{tx.userID.String()}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	joins, labelArea, _ := areaJoinSQL(bind, nil)
	rows, err := tx.tx.Query(ctx, `
		SELECT m.area_id, `+areaResolvedSQL+`,
		       CASE WHEN `+labelArea+` IS NOT NULL THEN 'label'
		            WHEN `+areaResolvedSQL+` IS NOT NULL THEN 'tab' ELSE 'none' END,
		       count(*)
		  FROM app.messages m`+joins+`
		 WHERE m.user_id = $1 AND m.area_id IS DISTINCT FROM `+areaResolvedSQL+`
		 GROUP BY 1, 2, 3
		 ORDER BY 4 DESC, 1, 2, 3`, args...)
	if err != nil {
		return rep, fmt.Errorf("store: area index disagreements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			stored, resolved *string
			d                AreaIndexDisagreement
		)
		if err := rows.Scan(&stored, &resolved, &d.Via, &d.Count); err != nil {
			return rep, fmt.Errorf("store: scan area index disagreement: %w", err)
		}
		for _, p := range []struct {
			raw *string
			dst **ulid.ULID
		}{{stored, &d.Stored}, {resolved, &d.Resolved}} {
			if p.raw == nil {
				continue
			}
			id, perr := ulid.Parse(*p.raw)
			if perr != nil {
				return rep, fmt.Errorf("store: parse area id %q: %w", *p.raw, perr)
			}
			*p.dst = &id
		}
		rep.Disagreements = append(rep.Disagreements, d)
	}
	return rep, rows.Err()
}

// AreaIndexBackfillBatch writes the resolver's answer to area_id for up to
// limit messages with id > after (keyset order), and returns the last id it
// scanned (the next call's after), how many it scanned and how many it
// changed. last is the zero ULID with scanned == 0 when the walk is done.
//
// The batch's rows are locked FOR UPDATE before the resolver statement runs,
// so a concurrent label or verdict change (whose trigger writes the same row)
// either committed before the resolver's snapshot, which then sees it, or waits
// for this transaction and re-resolves afterwards. Without the lock the write
// could carry an answer read before that change. Idempotent: only rows whose
// stored value differs are written.
//
// No change_log row: the column is a cache of a derived value, not a
// JMAP-visible property (ADR-049 section 3).
func (tx *UserTx) AreaIndexBackfillBatch(ctx context.Context, after ulid.ULID, limit int) (last ulid.ULID, scanned, changed int, err error) {
	if limit <= 0 {
		return after, 0, 0, fmt.Errorf("store: area index backfill: batch must be positive")
	}
	// The first page has no lower bound; later pages continue after the last id.
	q := `SELECT id FROM app.messages WHERE user_id = $1 ORDER BY id LIMIT $2 FOR UPDATE`
	qargs := []any{tx.userID.String(), limit}
	if after != (ulid.ULID{}) {
		q = `SELECT id FROM app.messages WHERE user_id = $1 AND id > $3 ORDER BY id LIMIT $2 FOR UPDATE`
		qargs = append(qargs, after.String())
	}
	rows, err := tx.tx.Query(ctx, q, qargs...)
	if err != nil {
		return after, 0, 0, fmt.Errorf("store: area index backfill: lock batch: %w", err)
	}
	var ids []ulid.ULID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return after, 0, 0, fmt.Errorf("store: area index backfill: scan: %w", err)
		}
		id, perr := ulid.Parse(raw)
		if perr != nil {
			rows.Close()
			return after, 0, 0, fmt.Errorf("store: area index backfill: parse %q: %w", raw, perr)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return after, 0, 0, fmt.Errorf("store: area index backfill: %w", err)
	}
	if len(ids) == 0 {
		return after, 0, 0, nil
	}

	args := []any{tx.userID.String(), ulidStrings(ids)}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	joins, _, _ := areaJoinSQL(bind, func(col string) string { return col + " = ANY($2)" })
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages mu
		   SET area_id = x.area_id
		  FROM (SELECT m.id, `+areaResolvedSQL+` AS area_id
		          FROM app.messages m`+joins+`
		         WHERE m.user_id = $1 AND m.id = ANY($2)) x
		 WHERE mu.user_id = $1 AND mu.id = x.id
		   AND mu.area_id IS DISTINCT FROM x.area_id`, args...)
	if err != nil {
		return after, 0, 0, fmt.Errorf("store: area index backfill: write: %w", err)
	}
	return ids[len(ids)-1], len(ids), int(tag.RowsAffected()), nil
}

// MarkAreaIndexReady records that the caller's stored column is complete. An
// existing ready_at is kept, so a second run does not move it. Callers must
// have just seen AreaIndexReport.Disagreeing() == 0.
func (tx *UserTx) MarkAreaIndexReady(ctx context.Context) error {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.area_index_state (user_id, ready_at) VALUES ($1, now())
		ON CONFLICT (user_id) DO UPDATE
		   SET ready_at = COALESCE(app.area_index_state.ready_at, EXCLUDED.ready_at)`,
		tx.userID.String()); err != nil {
		return fmt.Errorf("store: mark area index ready: %w", err)
	}
	return nil
}
