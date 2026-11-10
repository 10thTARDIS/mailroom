// SPDX-License-Identifier: AGPL-3.0-only

package store

// Re-scan on open: the reads and the bounded enqueue behind ADR-056 Decision A.
//
// ADR-027 §5c stops re-scanning once mail is older than six months. ADR-056 lets
// OPENING that mail (a body fetch, a download, a preview) re-scan its stale
// attachments instead. Everything here is per message and per user; the policy
// (budgets, fail-open, the breaker) lives in internal/rescan.

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// OpenRescanJobKind is the durable-job kind a body fetch enqueues. Its payload is
// {"message_id": "<ulid>"} and nothing else (jobPayloadKeys).
const OpenRescanJobKind = "attachment_rescan_open"

// OldBandAge is the age above which mail is in the last re-scan tier ("> 6
// months"): the MaxAge of the last tier that IS re-scanned. Derived from
// RescanTiers, not restated, so moving a boundary there moves it here.
func OldBandAge() time.Duration {
	var last time.Duration
	for _, t := range RescanTiers {
		if t.Rescan && t.MaxAge > last {
			last = t.MaxAge
		}
	}
	return last
}

// OpenRescanCounts is what re-scan-on-open needs to know about one message.
type OpenRescanCounts struct {
	// Old counts the message's attachments that have bytes, when the MESSAGE is
	// in the old band. Zero for newer mail: the existing tiers own those.
	Old int
	// Stale is the subset of Old whose verdict is behind the current signature
	// database (never stamped, or stamped lower). Only computed when the caller
	// passed a version; zero otherwise.
	Stale int
}

// OpenRescanCountsFor reads OpenRescanCounts for a set of messages in one query.
// version <= 0 means "the current version is not known yet": Stale stays zero,
// which lets a caller skip the scanner round trip for mail that is not old.
//
// ⚠ A message that is not this user's (or does not exist) is simply ABSENT:
// every predicate carries user_id, and row-level security says the same. That is
// the ownership guarantee the callers lean on.
func (tx *UserTx) OpenRescanCountsFor(ctx context.Context, messageIDs []ulid.ULID, version int) (map[ulid.ULID]OpenRescanCounts, error) {
	out := map[ulid.ULID]OpenRescanCounts{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		ids = append(ids, id.String())
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT a.message_id,
		       count(*) AS old,
		       count(*) FILTER (WHERE $3 > 0
		                          AND (a.scanned_db_version IS NULL OR a.scanned_db_version < $3)) AS stale
		  FROM app.attachments a
		  JOIN app.messages m ON m.id = a.message_id AND m.user_id = a.user_id
		 WHERE a.user_id = $1
		   AND a.message_id = ANY($2::text[])
		   AND a.size_bytes > 0
		   AND m.received_at <= now() - make_interval(secs => $4)
		 GROUP BY a.message_id`,
		tx.userID.String(), ids, version, OldBandAge().Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: open rescan counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid string
		var c OpenRescanCounts
		if err := rows.Scan(&mid, &c.Old, &c.Stale); err != nil {
			return nil, fmt.Errorf("store: open rescan counts row: %w", err)
		}
		u, perr := ulid.Parse(mid)
		if perr != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", mid, perr)
		}
		out[u] = c
	}
	return out, rows.Err()
}

// OpenRescanTarget returns the message's blob reference when it still has a stale
// old attachment at this version, and found=false when there is nothing to do
// (fresh, newer than the band, not this user's, or gone). The job and the
// synchronous path both call it, so neither acts on a decision the other
// already made obsolete.
func (tx *UserTx) OpenRescanTarget(ctx context.Context, messageID ulid.ULID, version int) (RescanTarget, bool, error) {
	counts, err := tx.OpenRescanCountsFor(ctx, []ulid.ULID{messageID}, version)
	if err != nil {
		return RescanTarget{}, false, err
	}
	if counts[messageID].Stale == 0 {
		return RescanTarget{}, false, nil
	}
	t := RescanTarget{MessageID: messageID}
	if err := tx.tx.QueryRow(ctx,
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), messageID.String()).Scan(&t.ContentHash); err != nil {
		return RescanTarget{}, false, fmt.Errorf("store: open rescan target: %w", err)
	}
	return t, true, nil
}

// OpenRescanEnqueue is the outcome of EnqueueOpenRescan.
type OpenRescanEnqueue int

const (
	// OpenRescanEnqueued: a job was created.
	OpenRescanEnqueued OpenRescanEnqueue = iota
	// OpenRescanCooledDown: a job for this message exists, outstanding or
	// created within the cooldown. The repeated open coalesces into it.
	OpenRescanCooledDown
	// OpenRescanThrottled: the user already created the per-minute maximum.
	OpenRescanThrottled
)

// EnqueueOpenRescan creates a re-scan job for one message unless a bound says no.
//
// ⚠ BOUNDED TWICE, IN THE STATEMENT THAT INSERTS. The dispatcher is sequential,
// so a long re-scan delays `submit` and outbound mail stops; these two bounds are
// what stop a burst of opens becoming that (ADR-056):
//
//   - per message: no job for this message created within `cooldown`, which also
//     covers one still outstanding, so repeated opens coalesce and a message that
//     cannot be re-scanned is not retried by every open;
//   - per user: fewer than maxPerMinute jobs of this kind created in the last
//     60 seconds.
//
// The check and the insert are one statement. Concurrent requests can still
// overshoot the per-minute count by the number racing in the same instant; the
// live-key unique index keeps the per-message bound exact.
func (tx *UserTx) EnqueueOpenRescan(ctx context.Context, messageID ulid.ULID, maxPerMinute int, cooldown time.Duration) (OpenRescanEnqueue, error) {
	id := NewULID()
	payload := fmt.Sprintf(`{"message_id":%q}`, messageID.String())
	key := OpenRescanJobKind + ":" + messageID.String()
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		SELECT $1::app.ulid, $2::app.ulid, $3, now(), $4::jsonb, $5
		 WHERE (SELECT count(*) FROM app.jobs
		         WHERE user_id = $2::app.ulid AND kind = $3
		           AND created_at > now() - interval '60 seconds') < $6
		   AND NOT EXISTS (SELECT 1 FROM app.jobs
		         WHERE user_id = $2::app.ulid AND kind = $3
		           AND payload->>'message_id' = $7
		           AND created_at > now() - make_interval(secs => $8))
		ON CONFLICT DO NOTHING`,
		id.String(), tx.userID.String(), OpenRescanJobKind, payload, key,
		maxPerMinute, messageID.String(), cooldown.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: enqueue open rescan: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return OpenRescanEnqueued, nil
	}
	// Refused. Say which bound, for the counters: a total cannot tell a burst
	// from a repeat.
	var recent bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.jobs
		        WHERE user_id = $1::app.ulid AND kind = $2
		          AND payload->>'message_id' = $3
		          AND created_at > now() - make_interval(secs => $4))`,
		tx.userID.String(), OpenRescanJobKind, messageID.String(), cooldown.Seconds()).Scan(&recent); err != nil {
		return 0, fmt.Errorf("store: classify refused open rescan: %w", err)
	}
	if recent {
		return OpenRescanCooledDown, nil
	}
	return OpenRescanThrottled, nil
}
