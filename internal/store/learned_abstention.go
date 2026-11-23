// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"
)

// WHY LEARNED PLACEMENT HELD BACK, PER DAY (migration 00100).
//
// The arrival counter (mailroom_learned_placement_total) already says WHY
// learned placement held back, but it is a process-lifetime tally for the whole
// instance. This is the same fact kept for one user, one UTC day and one reason
// at a time, so `mailroom-admin placement-measure` can print the last thirty
// days of a mailbox. It records what ingest decided and changes nothing about
// it.
//
// CHANGE_LOG: none, and deliberately. app.learned_placement_abstentions is a
// private tally, not a JMAP-visible object (no /get, no /changes, no state
// string), so the load-bearing invariant (every mutation of a JMAP-visible
// object appends a change_log row in the same transaction) does not apply. It
// is written in the ingest transaction, so a message that rolls back leaves no
// count behind.

// LearnedAbstentionReason is the reason under which a held-back arrival is
// counted, and whether it is counted at all. The counter's own label
// (LearnedPlacementResult.Label) is the vocabulary; two of its values are not
// abstentions:
//
//   - "placed" is the success, recorded per message in app.learned_placements;
//   - "error" is a failed decision, which is a fault rather than a verdict and
//     has its own log line and counter series.
//
// "other" cannot be produced by a decision (it is the counter's catch-all for a
// label outside the closed set), so it is not recorded either: a value nobody
// can name is not a reason anyone could read back.
func LearnedAbstentionReason(r LearnedPlacementResult) (string, bool) {
	l := r.Label()
	switch l {
	case LearnedLabelPlaced, LearnedLabelError, LearnedLabelOther:
		return "", false
	}
	return l, true
}

// RecordLearnedAbstention adds one to today's (UTC) count for the reason r
// held the message back, and reports whether it recorded anything (false for a
// placement, which is not an abstention).
//
// It runs in its own savepoint, so a failure here can never poison the ingest
// transaction, and the caller logs it rather than treating it as a failed
// placement: the message was already placed or held back by the time this runs.
func (tx *UserTx) RecordLearnedAbstention(ctx context.Context, r LearnedPlacementResult) (bool, error) {
	reason, ok := LearnedAbstentionReason(r)
	if !ok {
		return false, nil
	}
	err := tx.inSavepoint(ctx, func() error {
		_, e := tx.tx.Exec(ctx, `
			INSERT INTO app.learned_placement_abstentions (user_id, day, reason, n)
			VALUES ($1, (now() AT TIME ZONE 'UTC')::date, $2, 1)
			ON CONFLICT (user_id, day, reason)
			DO UPDATE SET n = app.learned_placement_abstentions.n + 1`,
			tx.userID.String(), reason)
		return e
	})
	if err != nil {
		return false, fmt.Errorf("store: record learned abstention (%s): %w", reason, err)
	}
	return true, nil
}

// AbstentionTally is the last N days of held-back arrivals, by reason.
type AbstentionTally struct {
	// Days is the window asked for; Since is its first UTC day.
	Days  int
	Since time.Time
	// ByReason counts held-back arrivals inside the window. A reason with no
	// arrivals is absent (the printer lists the whole vocabulary).
	ByReason map[string]int
	// FirstRecorded is the earliest UTC day with any row at all (zero when the
	// table is empty): the tally is recorded going forward only, so a window
	// reaching back past it is a LOWER BOUND, and DaysCovered says how much of
	// the window the recording actually spans.
	FirstRecorded time.Time
	DaysCovered   int
}

// Total is the sum over reasons.
func (t AbstentionTally) Total() int {
	n := 0
	for _, c := range t.ByReason {
		n += c
	}
	return n
}

// AbstentionTally reads the window ending today (UTC), asOf being the reference
// instant (the caller's clock, so a test can pin it).
func (tx *UserTx) AbstentionTally(ctx context.Context, days int, asOf time.Time) (AbstentionTally, error) {
	today := time.Date(asOf.UTC().Year(), asOf.UTC().Month(), asOf.UTC().Day(), 0, 0, 0, 0, time.UTC)
	out := AbstentionTally{Days: days, Since: today.AddDate(0, 0, -(days - 1)), ByReason: map[string]int{}}
	rows, err := tx.tx.Query(ctx, `
		SELECT reason, sum(n)::int
		  FROM app.learned_placement_abstentions
		 WHERE user_id = $1 AND day >= $2::date AND day <= $3::date
		 GROUP BY reason`, tx.userID.String(), out.Since.Format("2006-01-02"), today.Format("2006-01-02"))
	if err != nil {
		return out, fmt.Errorf("store: abstention tally: %w", err)
	}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: abstention tally (scan): %w", err)
		}
		out.ByReason[reason] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: abstention tally: %w", err)
	}
	var first *time.Time
	if err := tx.tx.QueryRow(ctx,
		`SELECT min(day) FROM app.learned_placement_abstentions WHERE user_id = $1`,
		tx.userID.String()).Scan(&first); err != nil {
		return out, fmt.Errorf("store: abstention tally (first day): %w", err)
	}
	if first != nil {
		f := first.UTC()
		out.FirstRecorded = time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, time.UTC)
		start := out.Since
		if out.FirstRecorded.After(start) {
			start = out.FirstRecorded
		}
		out.DaysCovered = int(today.Sub(start).Hours()/24) + 1
	}
	return out, nil
}
