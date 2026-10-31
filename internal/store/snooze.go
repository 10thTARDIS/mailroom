// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Snooze (spec §6, mailroom#51). "Wake me about this later": the message leaves
// the inbox view until snoozed_until passes, then comes back.
//
// snoozed_until is the WHOLE state — snoozed exactly while it is set and in the
// future — so waking is one UPDATE to NULL and "snoozed with no wake time" is
// unrepresentable rather than defended against.
//
// Local-only: no IMAP move, for the reasons migration 00013 records. The wake
// rides the durable job model as SnoozeJobKind, and internal/jobs' dispatcher
// runs a catch-up pass on boot, so a wake whose time passed while the server was
// down fires at next start instead of being lost (spec §6's durability
// requirement).

// SnoozeJobKind is the durable-job kind the snooze wake worker registers for. It
// is the kind db/schema.sql's app.jobs comment reserved from the beginning.
const SnoozeJobKind = "snooze_wake"

// SETTING a snooze lives in ApplyEmailUpdate (jmap_email.go), not here, and that
// is deliberate: a snooze is one field of an Email/set patch, and routing it
// through a second write path would append a SECOND Email change_log row for a
// single update — the load-bearing invariant says one mutation, one row. This
// file owns the parts that have no Email/set to belong to: the wake, the timer,
// and the question of when the next one is due.

// WakeSnoozed clears snoozed_until for every message of this user whose wake
// time has passed, and returns the ids it woke.
//
// IDEMPOTENT BY CONSTRUCTION, which is the property that matters: a durable job
// can fire more than once (a retry after a crash between the work and the
// completion write, or two dispatchers racing a batch). The guard is the `<= now
// AND IS NOT NULL` predicate — a second run simply matches nothing and appends
// nothing. Nothing here depends on the job having fired exactly once.
//
// It wakes by TIME rather than by an id list carried in the job payload, on
// purpose. A payload list would go stale: the message may have been un-snoozed,
// re-snoozed to a different time, trashed, or purged between enqueue and fire.
// Asking the database "what is due right now" is the only formulation that
// cannot act on a stale premise.
//
// It stamps snooze_woke_at so a returning message can be ordered by when it came
// back rather than by how old it is (mailroom#152). The stamp is the instant the
// wake RAN — the same `now` the guard compares against — not the snoozed_until
// the user chose, and the difference matters when a timer is late: after the
// server has been down a day, a message due yesterday morning should return to
// the TOP of the inbox now, not to the position yesterday morning would have
// given it, which is exactly the burial snoozing is meant to prevent.
func (tx *UserTx) WakeSnoozed(ctx context.Context, now time.Time) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.messages
		   SET snoozed_until = NULL, snooze_woke_at = $2
		 WHERE user_id = $1
		   AND snoozed_until IS NOT NULL
		   AND snoozed_until <= $2
		RETURNING id`,
		tx.userID.String(), now.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: wake snoozed: %w", err)
	}
	defer rows.Close()
	var woke []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan woken id: %w", err)
		}
		u, perr := ulid.Parse(id)
		if perr != nil {
			return nil, fmt.Errorf("store: parse woken id %q: %w", id, perr)
		}
		woke = append(woke, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: wake snoozed: %w", err)
	}
	rows.Close()

	for _, id := range woke {
		if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpUpdated); err != nil {
			return nil, err
		}
	}
	return woke, nil
}

// EnsureSnoozeWakeJob schedules a wake at fireAt, collapsing requests that land
// in the same minute into one job. It reports whether a job was inserted.
//
// NOT EnsureRecurringUserJob, and the difference is a correctness one. That
// helper permits at most one outstanding job per kind, which is right for a
// periodic sweep but WRONG here: snoozing to 9am when a wake is already pending
// for 5pm would insert nothing, and the 9am message would sleep until 5pm. Every
// distinct wake time needs its own timer.
//
// Bounding the resulting job count is what the minute-truncated idempotency key
// does: snoozing fifty messages to "tomorrow morning" produces ONE job, not
// fifty, while a genuinely different wake time still gets its own. ON CONFLICT
// DO NOTHING makes the collapse a no-op rather than a unique-violation error.
//
// Extra or duplicate jobs are harmless regardless — WakeSnoozed wakes by time,
// so a job that finds nothing due does nothing — but "harmless" is not a reason
// to generate them.
//
// ⚠ THE CONFLICT TARGET REPEATS THE INDEX PREDICATE, AND IT IS NOT DECORATION.
// The arbiter is the PARTIAL unique index jobs_live_idempotency_key, which
// covers only outstanding ('pending','running') jobs — because this key is
// DERIVED FROM DATA, not chosen, so a job that dead-lettered used to hold it
// against every future attempt to re-derive it, and the wake never came back
// (mailroom#357, migration 00042). Postgres will not infer a partial index from
// a bare column list; dropping the WHERE here raises 42P10 rather than silently
// widening anything, which is the failure mode to want.
func (tx *UserTx) EnsureSnoozeWakeJob(ctx context.Context, fireAt time.Time) (bool, error) {
	at := fireAt.UTC()
	key := fmt.Sprintf("%s:%s:%s", SnoozeJobKind, tx.userID.String(), at.Truncate(time.Minute).Format(time.RFC3339))
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		VALUES ($1, $2, $3, $4, '{}'::jsonb, $5)
		ON CONFLICT (idempotency_key) WHERE state IN ('pending', 'running')
		DO NOTHING`,
		NewULID().String(), tx.userID.String(), SnoozeJobKind, at, key)
	if err != nil {
		return false, fmt.Errorf("store: ensure snooze wake job: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// NextSnoozeWake reports the earliest pending wake time for this user, if any.
// The wake worker uses it to reschedule itself precisely rather than polling: a
// snooze set for next month should cost one timer, not a wake-up every minute
// for a month.
func (tx *UserTx) NextSnoozeWake(ctx context.Context) (time.Time, bool, error) {
	var at *time.Time
	err := tx.tx.QueryRow(ctx, `
		SELECT min(snoozed_until) FROM app.messages
		 WHERE user_id = $1 AND snoozed_until IS NOT NULL`,
		tx.userID.String()).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: next snooze wake: %w", err)
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return *at, true, nil
}
