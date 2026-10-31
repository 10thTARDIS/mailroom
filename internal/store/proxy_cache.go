// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"
)

// The image proxy's cache (app.proxy_image_cache, migration 00038, ADR-026 §4
// step 4).
//
// # What a cache is actually for here
//
// It is the ONLY thing that hides WHEN a message was opened, and only from the
// second open onward. The proxy hides the reader's browser and address on every
// fetch; without a cache, every re-read of the same message re-pings the sender
// and hands them a fresh timestamp. That is the whole benefit, and it is worth
// stating narrowly because the feature's name invites a larger claim.
//
// # Why it lives in the blob store
//
// Decision (ADR-026 decision B). It buys encryption at rest under
// the user's own DEK for free, content-addressed dedup that matters at the
// typical shape (many images from few hosts — a publisher's logo recurs in every
// issue), and no second store for #54's hard-delete sweep to learn about.
//
// ⚠ It costs three things, and all three are built rather than assumed:
// app.blobs.kind so the storage quota can exclude these bytes (quota.go), the
// promotion rule in InsertBlobMeta so a hash collision cannot hide the user's
// own mail from their quota, and a fourth referent in the reclaim guard
// (blobreclaim.go) so a purge cannot delete a cached image's file — and its
// dek_id — out from under this table.
//
// # The URL is never stored
//
// Only its SHA-256. A proxied image URL usually carries a per-recipient token in
// its path or query — seen as `token-hash` on many real images —
// so the URL identifies the reader. A hash finds the entry again and cannot be
// turned back into the token.
//
// # Prefetch and references (#922)
//
// Entries are also written at ARRIVAL, for senders the proxy's own rule allows,
// so an image behind a signed, expiring URL is cached while the URL still
// works. Such an entry carries references (app.proxy_image_refs, proxy_refs.go)
// and is exempt from eviction until the last referencing message is destroyed.
// For a prefetched image the sender learns delivery time instead of open time;
// ADR-026's amendment of 2026-10-27 states that trade.
//
// # No change_log
//
// The cache is operational state, not a JMAP-visible object: no write in this
// file appends a change_log row, and none may — nothing a client syncs changes
// when an image is cached or evicted.

// ProxyCacheEntry is one cached image.
type ProxyCacheEntry struct {
	URLHash     []byte
	ContentHash []byte
	ContentType string
	SizeBytes   int64
	FetchedAt   time.Time
	LastUsedAt  time.Time
}

// LookupProxyImage returns the cached image for a URL hash, advancing its
// last-used time in the same statement.
//
// ⚠ THE TOUCH IS PART OF THE READ, not a follow-up. Eviction is
// least-recently-USED, so a hit that forgot to record itself would make a
// frequently-read image look like a stale one and evict exactly the entries the
// cache is working hardest for. Doing it in one statement also means there is no
// window in which a concurrent eviction sees the old timestamp.
//
// found=false is an ordinary miss, not an error.
func (tx *UserTx) LookupProxyImage(ctx context.Context, urlHash []byte) (ProxyCacheEntry, bool, error) {
	var e ProxyCacheEntry
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.proxy_image_cache
		   SET last_used_at = now()
		 WHERE user_id = $1 AND url_hash = $2
		RETURNING url_hash, content_hash, content_type, size_bytes, fetched_at, last_used_at`,
		tx.userID.String(), urlHash).Scan(
		&e.URLHash, &e.ContentHash, &e.ContentType, &e.SizeBytes, &e.FetchedAt, &e.LastUsedAt)
	if isNoRows(err) {
		return ProxyCacheEntry{}, false, nil
	}
	if err != nil {
		return ProxyCacheEntry{}, false, fmt.Errorf("store: lookup proxy image: %w", err)
	}
	return e, true, nil
}

// PutProxyImage records a freshly fetched image. The caller has already written
// the bytes to the blob store and the app.blobs row (with
// Kind: BlobKindProxyCache).
//
// A repeat replaces the row rather than being ignored: the same URL can legally
// return different bytes over time, and a cache that kept the first answer
// forever would be serving a stale picture with no way to notice.
func (tx *UserTx) PutProxyImage(ctx context.Context, e ProxyCacheEntry) error {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.proxy_image_cache
		       (user_id, url_hash, content_hash, content_type, size_bytes)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, url_hash) DO UPDATE
		   SET content_hash = EXCLUDED.content_hash,
		       content_type = EXCLUDED.content_type,
		       size_bytes   = EXCLUDED.size_bytes,
		       fetched_at   = now(),
		       last_used_at = now()`,
		tx.userID.String(), e.URLHash, e.ContentHash, e.ContentType, e.SizeBytes,
	); err != nil {
		return fmt.Errorf("store: put proxy image: %w", err)
	}
	return nil
}

// ProxyCacheUsage is what the cache currently costs this user.
type ProxyCacheUsage struct {
	Entries int
	Bytes   int64
}

// ProxyCacheSize reports the user's cache usage. Separate from StorageQuota on
// purpose: these bytes are deliberately NOT part of the mail quota, and a report
// that folded them in would restate the bug the exclusion exists to prevent.
func (tx *UserTx) ProxyCacheSize(ctx context.Context) (ProxyCacheUsage, error) {
	var u ProxyCacheUsage
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(size_bytes), 0) FROM app.proxy_image_cache`,
	).Scan(&u.Entries, &u.Bytes); err != nil {
		return ProxyCacheUsage{}, fmt.Errorf("store: proxy cache size: %w", err)
	}
	return u, nil
}

// EvictProxyImages removes cache entries until the cache is inside both bounds,
// oldest-used first, and returns the content hashes whose app.blobs rows were
// reclaimed as a result.
//
// ⚠ BOTH BOUNDS APPLY TO UNREFERENCED ENTRIES ONLY (#922, decided
// 2026-10-27). An entry that a live message references (app.proxy_image_refs)
// is never evicted here — not by age and not by size — because it was
// prefetched while its URL was still valid and the URL may since have expired:
// evicting it would lose the picture for good. maxBytes therefore bounds the
// UNREFERENCED bytes; referenced entries are excluded from the running total
// as well as from the victims. When the last reference goes (the message row is
// deleted), the entry is an ordinary one again and ages out by last_used_at
// like any other — for a prefetched image nobody opened, that is its fetch
// time, so it usually goes on the next pass.
//
// ⚠ THE RETURNED HASHES ARE A CALLER OBLIGATION, not information. The row delete
// and the FILE delete must happen in the same transaction, before it commits —
// the same contract HardDeleteMessage documents in blobreclaim.go. A caller that
// ignores the return value leaves files on disk with no row pointing at them,
// which is the one direction of this that is silent.
//
// maxBytes <= 0 means no size bound; ttl <= 0 means no age bound. Both zero is a
// no-op and returns nothing, which is a legitimate configuration (cache
// everything, forever) rather than a mistake to guess about.
func (tx *UserTx) EvictProxyImages(ctx context.Context, maxBytes int64, ttl time.Duration) ([][]byte, error) {
	var victims [][]byte
	if maxBytes <= 0 && ttl <= 0 {
		return nil, nil
	}
	// Serialised against the prefetch's reference step (LockProxyCache). A
	// caller that writes cache rows first must already hold it — see there.
	if err := tx.LockProxyCache(ctx); err != nil {
		return nil, err
	}

	if ttl > 0 {
		rows, err := tx.tx.Query(ctx, `
			DELETE FROM app.proxy_image_cache c
			 WHERE c.user_id = $1 AND c.last_used_at < now() - $2::interval
			   AND NOT EXISTS (SELECT 1 FROM app.proxy_image_refs r
			                    WHERE r.user_id = $1 AND r.url_hash = c.url_hash)
			RETURNING c.content_hash`,
			tx.userID.String(), ttl.String())
		if err != nil {
			return nil, fmt.Errorf("store: evict proxy images by age: %w", err)
		}
		hashes, err := scanHashes(rows)
		if err != nil {
			return nil, err
		}
		victims = append(victims, hashes...)
	}

	if maxBytes > 0 {
		// ⚠ TWO STEPS, DELIBERATELY. The obvious one-statement version — a CTE
		// computing a running total, joined into a DELETE ... USING — deleted
		// NOTHING and reported no error, which is the worst possible shape for a
		// bound: the cache simply grew forever and every "did it evict?" check
		// that only looked at the error would have said yes. Selecting the
		// victims first and deleting them by key is longer and is obviously
		// right.
		//
		// Newest-used first, accumulating until the cap is exceeded; everything
		// from there on is evicted. So the entry that pushes the total over the
		// cap is the first one dropped, and the most recently read survive.
		//
		// Referenced entries are left out of the ranking entirely, so they
		// neither count toward the cap nor become victims (#922). The DELETE
		// re-checks the same predicate: a reference added between the two
		// statements must still protect its entry.
		rows, err := tx.tx.Query(ctx, `
			SELECT c.url_hash, c.size_bytes
			  FROM app.proxy_image_cache c
			 WHERE c.user_id = $1
			   AND NOT EXISTS (SELECT 1 FROM app.proxy_image_refs r
			                    WHERE r.user_id = $1 AND r.url_hash = c.url_hash)
			 ORDER BY c.last_used_at DESC, c.url_hash`,
			tx.userID.String())
		if err != nil {
			return nil, fmt.Errorf("store: rank proxy cache: %w", err)
		}
		var (
			running int64
			doomed  [][]byte
		)
		for rows.Next() {
			var h []byte
			var size int64
			if err := rows.Scan(&h, &size); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: rank proxy cache: %w", err)
			}
			running += size
			if running > maxBytes {
				doomed = append(doomed, h)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: rank proxy cache: %w", err)
		}
		if len(doomed) > 0 {
			del, err := tx.tx.Query(ctx, `
				DELETE FROM app.proxy_image_cache c
				 WHERE c.user_id = $1 AND c.url_hash = ANY($2::bytea[])
				   AND NOT EXISTS (SELECT 1 FROM app.proxy_image_refs r
				                    WHERE r.user_id = $1 AND r.url_hash = c.url_hash)
				RETURNING c.content_hash`,
				tx.userID.String(), doomed)
			if err != nil {
				return nil, fmt.Errorf("store: evict proxy images by size: %w", err)
			}
			hashes, err := scanHashes(del)
			if err != nil {
				return nil, err
			}
			victims = append(victims, hashes...)
		}
	}

	if len(victims) == 0 {
		return nil, nil
	}
	// ⚠ THROUGH THE SHARED GUARD, never a direct delete. A cached image's bytes
	// may also be a message's or an attachment's (content addressing), and
	// deleting that row would take the dek_id with it. reclaimUnreferencedBlobs
	// is the one place that question is asked.
	return tx.reclaimUnreferencedBlobs(ctx, victims)
}

// scanHashes drains a RETURNING content_hash cursor.
func scanHashes(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([][]byte, error) {
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: scan evicted hash: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: evict proxy images: %w", err)
	}
	return out, nil
}
