// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// Prefetched proxied images and the messages that keep them (#922,
// app.proxy_image_refs, migration 00081, ADR-026 amendment of 2026-10-27).
//
// # Why references exist at all
//
// The proxy used to fetch only when a message was opened. A sender whose image
// URLs are signed and EXPIRING answers 403 by the time an older message is first
// opened, so the picture is lost for good. The fix fetches at arrival for
// senders the proxy's own rule allows — and then the cached bytes have to
// outlive the cache's 30-day unread TTL, or the prefetch would buy at most a
// month. A reference is "a message the user still has shows this image": while
// one exists, neither the TTL nor the size cap evicts the entry. It is released
// by the message row's deletion (ON DELETE CASCADE), so every destroy path
// releases it and Trash, which is restorable, does not.
//
// # change_log
//
// None. The cache and its references are operational state, not JMAP-visible
// objects: nothing a client syncs changes when an image is cached, referenced or
// released, so the load-bearing invariant does not reach these writes — exactly
// as for app.proxy_image_cache, whose writes have never appended a row either.

// ProxyPrefetchJobKind is the durable-job kind the image prefetch registers for.
// The payload is the message id and a continuation round (jobPayloadKeys).
const ProxyPrefetchJobKind = "proxy_prefetch"

// ProxyPrefetchPayload is the job payload: an id and a count, never mail
// content (see jobPayloadKeys).
type ProxyPrefetchPayload struct {
	MessageID string `json:"message_id"`
	// Round counts continuations: 0 for the job ingest queued, n for the n-th
	// successor a time-boxed or rate-limited run queued for the rest.
	Round int `json:"round,omitempty"`
	// RateRounds counts rate deferrals, which keep Round (a wait for the
	// reader's budget is not progress) but are bounded on their own so a
	// message cannot be re-queued forever behind a busy window (#922 review).
	RateRounds int `json:"rate_rounds,omitempty"`
}

// EnqueueProxyPrefetch queues a continuation of the image prefetch for one
// message, under a SAVEPOINT, and reports whether a job was queued.
//
// round is the continuation count. A run that ran out of TIME queues round+1;
// a run that was RATE-limited re-queues the SAME round at the window reset
// (rateDeferred), so waiting for the reader's budget never spends the bounded
// supply of rounds (#922 review M3). The idempotency key of a rate deferral
// carries its fire time, because the job doing the deferring still holds the
// round's own key while it runs. rateRounds counts rate deferrals separately
// (bounded by the worker), and a rate deferral's key carries it, so each
// deferral has a key of its own.
//
// A duplicate key (that exact continuation is already outstanding) is "already
// queued", not an error.
func (tx *UserTx) EnqueueProxyPrefetch(ctx context.Context, messageID ulid.ULID, fireAt time.Time, round, rateRounds int, rateDeferred bool) (bool, error) {
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: enqueue proxy prefetch (savepoint): %w", err)
	}
	suffix := ""
	if rateDeferred {
		suffix = fmt.Sprintf(":rate-%d", rateRounds)
	}
	queued, err := tx.insertProxyPrefetchJob(ctx, sp, messageID, fireAt, round, rateRounds, suffix)
	if err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		return false, err
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("store: enqueue proxy prefetch (release): %w", err)
	}
	return queued, nil
}

// Why ingest did not queue a prefetch (EnqueueProxyPrefetchIfAllowed). A closed
// set; "" means queued (or already queued).
const (
	PrefetchEnqueueNotAllowed = "not_allowed"
	PrefetchEnqueueJunk       = "junk"
)

// EnqueueProxyPrefetchIfAllowed is ingest's half (#922): it queues the prefetch
// for a newly stored message only when its sender passes the PROXY's own,
// fail-closed remote-content rule (ProxyRemoteAllowsMatch, ADR-026 §3c) and it
// did not arrive in a junk-role mailbox (arrivalMailbox; the zero id skips that
// check), and reports whether it did and, if not, why.
//
// ⚠ JUNK IS REFUSED EVEN FOR AN ALLOWED SENDER. The provider's filter putting
// a message in Junk is evidence the From may be forged, and a prefetch is the
// server acting on the From before the reader has looked (#922 review).
//
// ⚠ IT IS FAIL-SOFT BY CONSTRUCTION, because ingest calls it inside the
// transaction that stores the mail: an error here must never cost the message.
// Everything — the grant read as well as the INSERT — runs under one SAVEPOINT,
// because in PostgreSQL a failed SELECT aborts the transaction just as a failed
// INSERT does. The caller logs a returned error and carries on.
//
// The rule is checked again when the job runs (a grant can be withdrawn in
// between); checking it here as well is what keeps the job table from growing a
// row per arriving message from senders who were never going to qualify.
func (tx *UserTx) EnqueueProxyPrefetchIfAllowed(ctx context.Context, messageID ulid.ULID, fromAddress string, arrivalMailbox ulid.ULID, fireAt time.Time) (bool, string, error) {
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, "", fmt.Errorf("store: enqueue proxy prefetch (savepoint): %w", err)
	}
	sub := *tx
	sub.tx = sp
	skip := func(why string) (bool, string, error) {
		if err := sp.Commit(ctx); err != nil {
			return false, "", fmt.Errorf("store: enqueue proxy prefetch (release): %w", err)
		}
		return false, why, nil
	}
	if arrivalMailbox != (ulid.ULID{}) {
		role, _, err := sub.MailboxRole(ctx, arrivalMailbox)
		if err != nil {
			_ = sp.Rollback(ctx)
			return false, "", err
		}
		if role == "junk" {
			return skip(PrefetchEnqueueJunk)
		}
	}
	allows, err := sub.ListRemoteAllows(ctx)
	if err != nil {
		_ = sp.Rollback(ctx)
		return false, "", err
	}
	if !ProxyRemoteAllowsMatch(fromAddress, allows) {
		return skip(PrefetchEnqueueNotAllowed)
	}
	queued, err := tx.insertProxyPrefetchJob(ctx, sp, messageID, fireAt, 0, 0, "")
	if err != nil {
		_ = sp.Rollback(ctx)
		return false, "", err
	}
	if err := sp.Commit(ctx); err != nil {
		return false, "", fmt.Errorf("store: enqueue proxy prefetch (release): %w", err)
	}
	return queued, "", nil
}

// MessageInJunk reports whether the message currently sits in a junk-role
// mailbox: its local move, its local mailbox, or any live server copy. The
// prefetch re-checks it when the job runs, since the message may have been
// moved to Junk (by the user, a rule or a block) after it was queued. The role
// is derived by the same roleForMailbox every other caller uses.
func (tx *UserTx) MessageInJunk(ctx context.Context, messageID ulid.ULID) (bool, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT mb.imap_name, COALESCE(mb.special_use, '')
		  FROM app.mailboxes mb
		 WHERE mb.user_id = $1
		   AND (mb.id IN (SELECT moved_mailbox_id FROM app.messages WHERE user_id = $1 AND id = $2)
		     OR mb.id IN (SELECT local_mailbox_id FROM app.messages WHERE user_id = $1 AND id = $2)
		     OR mb.id IN (SELECT p.mailbox_id FROM app.message_provenance p
		                   WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'))`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return false, fmt.Errorf("store: message in junk: %w", err)
	}
	defer rows.Close()
	junk := false
	for rows.Next() {
		var name, use string
		if err := rows.Scan(&name, &use); err != nil {
			return false, fmt.Errorf("store: message in junk: %w", err)
		}
		if roleForMailbox(name, use) == "junk" {
			junk = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: message in junk: %w", err)
	}
	return junk, nil
}

// LockProxyCache serialises this user's cache-writing transactions — eviction,
// the prefetch's "already cached, add a reference" step, and storing a fetched
// image — for the rest of the transaction (#922 review L1). Without it an
// eviction whose snapshot predates a new reference could delete the entry the
// reference was just added for. With it, each side's statements run after the
// other's commit and see its rows.
//
// ⚠ IT MUST PRECEDE ANY ROW LOCK IN A CACHE-WRITING TRANSACTION. Taken after an
// INSERT/UPDATE has locked rows (app.blobs, app.proxy_image_cache,
// app.proxy_image_refs), it forms a lock-order cycle with a transaction that
// took it first and then wants those rows, and PostgreSQL aborts one of them
// as a deadlock. Re-taking it later in the same transaction is free (EvictProxyImages
// does, as a guard for a caller that writes nothing else).
func (tx *UserTx) LockProxyCache(ctx context.Context) error {
	if _, err := tx.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"proxy_cache:"+tx.userID.String()); err != nil {
		return fmt.Errorf("store: lock proxy cache: %w", err)
	}
	return nil
}

// RefProxyImageIfCached is the prefetch's "already cached" step, done safely:
// under the cache lock it reads the entry and, only if it exists and accept
// approves its stored type, adds the message's reference in the same
// transaction. hit=false means "fetch it": no entry, an entry whose type the
// allowlist no longer accepts, or one an eviction removed first. It does not
// advance last_used_at — a prefetch is not a reader.
func (tx *UserTx) RefProxyImageIfCached(ctx context.Context, urlHash []byte, messageID ulid.ULID, accept func(contentType string) bool) (bool, error) {
	if err := tx.LockProxyCache(ctx); err != nil {
		return false, err
	}
	ct, found, err := tx.ProxyImageCachedType(ctx, urlHash)
	if err != nil || !found || !accept(ct) {
		return false, err
	}
	if err := tx.AddProxyImageRef(ctx, urlHash, messageID); err != nil {
		return false, err
	}
	return true, nil
}

// insertProxyPrefetchJob writes the job row on q (a savepoint). A conflict on
// the live idempotency key means the job is already outstanding: queued=false,
// no error. The conflict target repeats the partial index's predicate, as
// jobs_live_idempotency_key requires (see snooze.go).
func (tx *UserTx) insertProxyPrefetchJob(ctx context.Context, q pgx.Tx, messageID ulid.ULID, fireAt time.Time, round, rateRounds int, keySuffix string) (bool, error) {
	payload, err := json.Marshal(ProxyPrefetchPayload{MessageID: messageID.String(), Round: round, RateRounds: rateRounds})
	if err != nil {
		return false, fmt.Errorf("store: proxy prefetch payload: %w", err)
	}
	key := ProxyPrefetchJobKind + ":" + messageID.String()
	if round > 0 {
		key = fmt.Sprintf("%s:%d", key, round)
	}
	key += keySuffix
	tag, err := q.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) WHERE state IN ('pending', 'running')
		DO NOTHING`,
		NewULID().String(), tx.userID.String(), ProxyPrefetchJobKind, fireAt, payload, key)
	if err != nil {
		return false, fmt.Errorf("store: enqueue proxy prefetch: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ProxyImageCachedType reports whether an image is cached under urlHash and the
// type it was stored as, WITHOUT advancing last_used_at.
//
// The prefetch uses it to skip a fetch it does not need. It must not count as a
// use: a prefetch is not a reader, and touching the entry would make an image
// nobody has looked at read as recently used.
func (tx *UserTx) ProxyImageCachedType(ctx context.Context, urlHash []byte) (string, bool, error) {
	var ct string
	err := tx.tx.QueryRow(ctx, `
		SELECT content_type FROM app.proxy_image_cache
		 WHERE user_id = $1 AND url_hash = $2`,
		tx.userID.String(), urlHash).Scan(&ct)
	if isNoRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: proxy image cached type: %w", err)
	}
	return ct, true, nil
}

// AddProxyImageRef records that messageID shows the image cached under urlHash.
// A repeat is a no-op.
//
// The row is the caller's own (RLS WITH CHECK pins user_id), and it protects
// only the caller's own cache entries: eviction matches references on
// (user_id, url_hash). Callers pass a message id they have just read under the
// same identity; the foreign key, like every message_id foreign key in this
// schema, checks existence and not ownership.
func (tx *UserTx) AddProxyImageRef(ctx context.Context, urlHash []byte, messageID ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.proxy_image_refs (user_id, url_hash, message_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, url_hash, message_id) DO NOTHING`,
		tx.userID.String(), urlHash, messageID.String()); err != nil {
		return fmt.Errorf("store: add proxy image ref: %w", err)
	}
	return nil
}

// ProxyImageRefCount reports how many references the image under urlHash has.
// Tests and reports read it; the eviction path asks the same question inline.
func (tx *UserTx) ProxyImageRefCount(ctx context.Context, urlHash []byte) (int, error) {
	var n int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FROM app.proxy_image_refs WHERE user_id = $1 AND url_hash = $2`,
		tx.userID.String(), urlHash).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: proxy image ref count: %w", err)
	}
	return n, nil
}

// ProxyCacheSplit is the cache's size, split by whether a live message still
// references each entry.
//
// ⚠ THE SPLIT IS THE POINT. Referenced entries are bounded by nothing but the
// mail that references them (decided 2026-10-27): the size cap
// applies only to the unreferenced half. A single total would hide exactly the
// part that can grow.
type ProxyCacheSplit struct {
	ReferencedEntries   int
	ReferencedBytes     int64
	UnreferencedEntries int
	UnreferencedBytes   int64
}

// ProxyCacheBreakdown reports the user's cache usage split by reference. One
// aggregate over the user's cache rows; callers run it after an eviction pass,
// never per cache hit.
func (tx *UserTx) ProxyCacheBreakdown(ctx context.Context) (ProxyCacheSplit, error) {
	var s ProxyCacheSplit
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE referenced),
		       COALESCE(sum(size_bytes) FILTER (WHERE referenced), 0),
		       count(*) FILTER (WHERE NOT referenced),
		       COALESCE(sum(size_bytes) FILTER (WHERE NOT referenced), 0)
		  FROM (SELECT c.size_bytes,
		               EXISTS (SELECT 1 FROM app.proxy_image_refs r
		                        WHERE r.user_id = c.user_id AND r.url_hash = c.url_hash) AS referenced
		          FROM app.proxy_image_cache c
		         WHERE c.user_id = $1) t`,
		tx.userID.String()).Scan(
		&s.ReferencedEntries, &s.ReferencedBytes, &s.UnreferencedEntries, &s.UnreferencedBytes); err != nil {
		return ProxyCacheSplit{}, fmt.Errorf("store: proxy cache breakdown: %w", err)
	}
	return s, nil
}

// carryProxyImageRefs moves collapsed copies' image references onto the keeper
// (#922). Without it the cascade from deleting the losers would release images
// the keeper — the same message — still shows, and they would fall back to the
// unread TTL. Operational rows, so no change_log.
func (tx *UserTx) carryProxyImageRefs(ctx context.Context, keeperID ulid.ULID, loserStrs []string) error {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.proxy_image_refs (user_id, url_hash, message_id, created_at)
		SELECT user_id, url_hash, $2::app.ulid, min(created_at)
		  FROM app.proxy_image_refs
		 WHERE user_id = $1 AND message_id = ANY($3)
		 GROUP BY user_id, url_hash
		ON CONFLICT (user_id, url_hash, message_id) DO NOTHING`,
		tx.userID.String(), keeperID.String(), loserStrs); err != nil {
		return fmt.Errorf("store: collapse (carry proxy image refs): %w", err)
	}
	return nil
}
