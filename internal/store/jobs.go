// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Job is a due timer row dequeued for dispatch (spec §16 durable job model).
type Job struct {
	ID     ulid.ULID
	UserID *ulid.ULID // nil = system job
	Kind   string
	FireAt time.Time
	// Payload carries IDS ONLY — see jobPayloadKeys. Not "opaque", which is what
	// this said until #309: it is opaque to the STORE, and that is exactly why
	// the constraint on it has to be written down rather than typed.
	Payload        []byte
	IdempotencyKey string
	Attempts       int
	MaxAttempts    int
}

// ⚠ THE JOB PAYLOAD CARRIES IDS, NEVER MAIL CONTENT (#309 item 3).
//
// This is a security contract on a table that is read ACROSS USERS. Jobs are
// dequeued through app.dequeue_due_jobs, a SECURITY DEFINER that necessarily
// bypasses RLS — the dispatcher has no identity of its own, so it cannot see due
// work any other way (#16). That function is declared `RETURNS SETOF app.jobs`,
// so it hands the dispatcher WHOLE ROWS, payload included, for every user.
//
// The guarantee "the dispatcher never sees another user's mail" therefore does
// not live in the definer, the policies, or the grants. It lives in what CALLERS
// put in the payload — and it held by convention alone until #309. The audit
// measured a live table and found it true (across thousands of rows every payload was
// an id or empty), but nothing CONSTRAINED it: a future job kind embedding a
// subject line, a snippet or a body would silently turn a definer into a
// cross-user mail conduit, and the blast radius is real because the dispatcher
// goes on to assert those users' identities.
//
// So: an id, or something a reference can express (a count, an enum). If a
// handler needs the message, it re-reads it under the target user's identity,
// which is the only context RLS can check.
//
// jobPayloadKeys is the enforceable half — the allowlist that makes the contract
// checkable instead of merely stated. Key: job kind. Value: the payload object
// keys that kind may carry; an empty set means "no payload at all", which is the
// majority. TestJobPayloadsCarryReferencesOnly walks the live table against it.
//
// It lives in the store rather than beside each kind's own constant on purpose:
// the risk is cross-user disclosure through one shared definer, so what may
// cross that boundary belongs in one place where adding to it is visible. The
// kinds are string literals for the same reason — importing compose/ml/purge
// here would invert the dependency direction.
//
// ⚠ Adding a key is a decision about what the dispatcher may see for EVERY user,
// not a local change to one handler.
var jobPayloadKeys = map[string]map[string]bool{
	"submit":       {"submission_id": true},
	"snooze_wake":  {},
	"purge_trash":  {},
	"ml_train":     {},
	"drawer_sweep": {},
	// Day-one setup (#446): the run id, and nothing else. The worker re-reads
	// the run (and its plan) under the target user's identity, which is the only
	// context RLS can check — see the contract above.
	"setup_import": {"run_id": true},
	// change_log compaction (mailroom#526, internal/changelog): a per-user
	// recurring timer, same shape as drawer_sweep — the worker reads the
	// configured retention and batch cap from its own construction, not from
	// the payload.
	"change_log_compact": {},
	// Out of office (#50, internal/outofoffice): the account id, and nothing
	// else. The worker re-reads the reply and its settings under the user's
	// identity, so the reply text never crosses the definer.
	"out_of_office_sync": {"account_id": true},
	// The one Web Push a grant owes when it starts needing its user to
	// reconnect (#52, internal/oauthmail/notify.go): the account id, and
	// the time of the state transition it is for (so a notice queued for one
	// transition is never spent on a later one), and nothing else. The worker
	// re-reads the grant and the account's name under the user's identity.
	"oauth_reauth_notify": {"account_id": true, "changed_at": true},
	// The image prefetch at arrival (#922, proxy_refs.go): the message id and
	// two continuation counts, and nothing else. The worker re-reads the message and
	// its parked image URLs under the user's identity, so no URL — which carries
	// a per-recipient token — ever crosses the definer.
	"proxy_prefetch": {"message_id": true, "round": true, "rate_rounds": true},
}

// PayloadKeysOutsideAllowlist reports the payload keys a job of this kind must
// not carry, and whether the kind is described at all. It is the whole decision
// procedure behind the contract above, in one place, so it can be tested
// WITHOUT a database.
//
// ⚠ THAT SEPARATION IS THE POINT, and it was found the hard way. The first
// version of this check lived only inside an integration test that walked
// app.jobs and held each row's payload against the map. That test can only see
// violations some other code actually inserted during the run — so widening the
// allowlist, or adding a content-bearing key to a kind whose enqueue path the
// suite never exercises, left it green. It could confirm the table was clean; it
// could not confirm the RULE was right. A rule that cannot be shown to reject
// anything is documentation (#131, #309 item 3).
//
// Both callers matter and they check different things: the unit test proves this
// function discriminates, and the integration test proves the live table
// satisfies it.
func PayloadKeysOutsideAllowlist(kind string, keys []string) (bad []string, knownKind bool) {
	allowed, ok := jobPayloadKeys[kind]
	if !ok {
		return keys, false
	}
	for _, k := range keys {
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	return bad, true
}

// JobState mirrors the app.job_state enum.
type JobState string

const (
	JobPending JobState = "pending"
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
	JobDead    JobState = "dead"
	// JobUnchanged is the zero value returned when a transition function's
	// guard did not match (the worker lost its lock, or the job was already
	// terminal) — the SQL function returned NULL.
	JobUnchanged JobState = ""
)

// EnqueueJob inserts a user-scoped durable job. Jobs are internal machinery,
// not JMAP-visible objects, so no change_log row is appended. The RLS policy
// requires the row's user_id to match the transaction identity, so the job is
// always attributed to this user. idempotencyKey must be unique.
//
// ⚠ payload carries ids, never mail content — see jobPayloadKeys.
func (tx *UserTx) EnqueueJob(ctx context.Context, kind string, fireAt time.Time, payload []byte, idempotencyKey string) (ulid.ULID, error) {
	id := NewULID()
	if payload == nil {
		payload = []byte("{}")
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id.String(), tx.userID.String(), kind, fireAt, payload, idempotencyKey,
	); err != nil {
		return ulid.ULID{}, fmt.Errorf("store: enqueue job: %w", err)
	}
	return id, nil
}

// EnqueueSystemJob inserts a system (user-less) durable job — purges,
// change-log compaction, DEK re-encryption, and similar cross-cutting timers.
//
// ⚠ payload carries ids, never mail content — see jobPayloadKeys.
func (tx *SystemTx) EnqueueSystemJob(ctx context.Context, kind string, fireAt time.Time, payload []byte, idempotencyKey string) (ulid.ULID, error) {
	id := NewULID()
	if payload == nil {
		payload = []byte("{}")
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		VALUES ($1, NULL, $2, $3, $4, $5)`,
		id.String(), kind, fireAt, payload, idempotencyKey,
	); err != nil {
		return ulid.ULID{}, fmt.Errorf("store: enqueue system job: %w", err)
	}
	return id, nil
}

// EnsureRecurringUserJob schedules a user-scoped recurring timer at fireAt, but
// ONLY when the user has no outstanding job of that kind (pending or running) —
// so the pattern converges on exactly one live timer per user no matter how many
// times it is called. It backs the purge sweep (and any future recurring per-user
// job): main seeds one at boot, and the handler reschedules the next as it
// finishes.
//
// excludeJobID is the currently-running job to disregard, so a reschedule from
// INSIDE that job's handler (where the job is still 'running') does not see
// itself and refuse to schedule the successor. Pass the zero ULID at boot, where
// there is nothing to exclude. A fresh ULID idempotency key satisfies the UNIQUE
// constraint while the NOT EXISTS guard — not the key — is what enforces "at most
// one". It reports whether a new job was enqueued.
func (tx *UserTx) EnsureRecurringUserJob(ctx context.Context, kind string, fireAt time.Time, excludeJobID ulid.ULID) (bool, error) {
	id := NewULID()
	key := kind + ":" + tx.userID.String() + ":" + NewULID().String()
	// The ULID params carry explicit ::app.ulid casts: in an INSERT ... SELECT
	// (unlike VALUES) Postgres does not infer a bare parameter's type from the
	// target column, and $2 appears both as an inserted value and in the NOT
	// EXISTS predicate, so without the cast it deduces inconsistent types
	// (SQLSTATE 42P08).
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		SELECT $1::app.ulid, $2::app.ulid, $3, $4, '{}'::jsonb, $5
		 WHERE NOT EXISTS (
		     SELECT 1 FROM app.jobs
		      WHERE user_id = $2::app.ulid AND kind = $3
		        AND state IN ('pending', 'running')
		        AND id <> $6::app.ulid)`,
		id.String(), tx.userID.String(), kind, fireAt.UTC(), key, excludeJobID.String())
	if err != nil {
		return false, fmt.Errorf("store: ensure recurring user job (%s): %w", kind, err)
	}
	return tag.RowsAffected() > 0, nil
}

// CompleteJob transitions a claimed job (running -> done) via the SECURITY
// DEFINER app.complete_job, guarded so only the claiming worker succeeds. It
// returns true when the job was transitioned, false when the guard did not
// match (lock lost or already terminal).
func (tx *SystemTx) CompleteJob(ctx context.Context, id ulid.ULID, worker string) (bool, error) {
	var done *bool
	if err := tx.tx.QueryRow(ctx,
		`SELECT app.complete_job($1, $2)`, id.String(), worker,
	).Scan(&done); err != nil {
		return false, fmt.Errorf("store: complete job: %w", err)
	}
	return done != nil && *done, nil
}

// FailJob records that a claimed job errored, via the SECURITY DEFINER
// app.fail_job. If attempts remain the job returns to pending with fire_at set
// to retryAt (caller-computed backoff); once attempts have reached
// max_attempts it moves to the dead-letter state. It returns the resulting
// state, or JobUnchanged when the guard did not match.
func (tx *SystemTx) FailJob(ctx context.Context, id ulid.ULID, worker, errMsg string, retryAt time.Time) (JobState, error) {
	var state *string
	if err := tx.tx.QueryRow(ctx,
		`SELECT app.fail_job($1, $2, $3, $4)`, id.String(), worker, errMsg, retryAt,
	).Scan(&state); err != nil {
		return JobUnchanged, fmt.Errorf("store: fail job: %w", err)
	}
	if state == nil {
		return JobUnchanged, nil
	}
	return JobState(*state), nil
}

// CountDueJobs returns how many jobs are pending and already due across all
// users, via app.count_due_jobs — the backlog behind mailroom_job_queue_depth
// (#524).
//
// ⚠ IT IS A DEFINER FOR THE SAME REASON DequeueDueJobs IS, and a plain count
// here would be WORSE than an error. This runs in a SystemTx, where
// app.current_user_id() is NULL and the app.jobs policy therefore matches system
// rows only — so `SELECT count(*) FROM app.jobs` returns a plausible number that
// silently omits every per-user job (purge_trash, ml_train, snooze_wake,
// submit). The definer returns one bigint and cannot name a user, a job or a
// payload, so it discloses strictly less than DequeueDueJobs already does to the
// same caller.
//
// "Due" excludes rows scheduled for the future — a snooze for next week is not
// backlog — and excludes jobs stranded in 'running', which migration 00041's
// lease returns to the claimable set on their own.
func (tx *SystemTx) CountDueJobs(ctx context.Context) (int64, error) {
	var n int64
	if err := tx.tx.QueryRow(ctx, `SELECT app.count_due_jobs()`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count due jobs: %w", err)
	}
	return n, nil
}

// DequeueDueJobs claims up to limit due jobs for the named worker via
// app.dequeue_due_jobs — the ONE deliberate, narrow cross-user surface that
// exposes job ROWS (SECURITY DEFINER; SKIP LOCKED; job rows, never mail
// content). CountDueJobs above is the other definer on this table and returns
// an aggregate only.
// Claimed jobs move to state 'running' with attempts incremented.
func (tx *SystemTx) DequeueDueJobs(ctx context.Context, worker string, limit int) ([]Job, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, user_id, kind, fire_at, payload,
		       idempotency_key, attempts, max_attempts
		  FROM app.dequeue_due_jobs($1, $2)`,
		worker, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: dequeue due jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var (
			j      Job
			id     string
			userID *string
		)
		if err := rows.Scan(&id, &userID, &j.Kind, &j.FireAt, &j.Payload,
			&j.IdempotencyKey, &j.Attempts, &j.MaxAttempts); err != nil {
			return nil, fmt.Errorf("store: scan job row: %w", err)
		}
		if j.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: job id %q: %w", id, err)
		}
		if userID != nil {
			u, err := ulid.Parse(*userID)
			if err != nil {
				return nil, fmt.Errorf("store: job user id %q: %w", *userID, err)
			}
			j.UserID = &u
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: dequeue due jobs: %w", err)
	}
	return jobs, nil
}
