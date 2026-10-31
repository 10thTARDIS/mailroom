// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The image proxy's cache (migration 00038, ADR-026 §3e/§4 step 4), and
// specifically the THREE PREREQUISITES that putting it in the blob store cost.
//
// The decision was the blob store over a separate one. What that bought is
// real; what it cost is these, and none of them is optional:
//
//  1. app.blobs.kind, so StorageQuota can exclude a stranger's images. Without
//     it a cached image consumes the quota that bounds the user's OWN mail, and
//     a user pushed over it stops being able to receive or send.
//  2. the promotion rule in InsertBlobMeta, so a content-hash collision cannot
//     leave the user's mail filed under 'proxy_cache' and invisible to quota.
//  3. a fourth referent in the reclaim guard, so a purge cannot delete a cached
//     image's file — and its dek_id — out from under this table.
//
// Every test asserts on a SPECIFIC hash or a specific delta, never on a total: a
// single number cannot tell "the exclusion works" from "the query found nothing".

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedCachedImage writes an app.blobs row of the given kind plus the
// proxy_image_cache row that references it, as the SUPERUSER so the test is not
// also testing the write path it is about to measure.
func seedCachedImage(t *testing.T, ctx context.Context, h *harness, userID string, body string, size int64) (urlHash, contentHash []byte) {
	t.Helper()
	ch := sha256.Sum256([]byte(body))
	uh := sha256.Sum256([]byte("https://cdn.example/" + body))
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id, kind)
		VALUES ($1, $2, $3, $4, $5, 'proxy_cache')
		ON CONFLICT (user_id, content_hash) DO NOTHING`,
		userID, ch[:], size, size, NewULID().String()); err != nil {
		t.Fatalf("seed cache blob: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.proxy_image_cache
		       (user_id, url_hash, content_hash, content_type, size_bytes)
		VALUES ($1, $2, $3, 'image/png', $4)`,
		userID, uh[:], ch[:], size); err != nil {
		t.Fatalf("seed cache row: %v", err)
	}
	return uh[:], ch[:]
}

func quotaUsed(t *testing.T, ctx context.Context, s *Store, userID ulid.ULID) int64 {
	t.Helper()
	var used int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		_, used, e = tx.StorageQuota(ctx)
		return e
	}); err != nil {
		t.Fatalf("storage quota: %v", err)
	}
	return used
}

// TestProxyCacheBytesAreNotChargedToTheMailQuota is prerequisite 1, and it is
// the one whose absence breaks MAIL DELIVERY rather than a number: a user over
// quota cannot receive or send.
func TestProxyCacheBytesAreNotChargedToTheMailQuota(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	clearProxyCache(t, ctx, h, a.ID)

	before := quotaUsed(t, ctx, s, aID)
	seedCachedImage(t, ctx, h, a.ID, "a-cached-image", 5_000_000)
	after := quotaUsed(t, ctx, s, aID)

	if after != before {
		t.Errorf("a 5 MB cached image moved the mail quota from %d to %d. "+
			"A stranger's marketing images must not consume the allowance that "+
			"bounds the user's own mail — over quota means they stop being able "+
			"to receive or send", before, after)
	}

	// ⚠ CONTROL: a MAIL blob of the same size DOES move it. Without this the
	// test would pass for a StorageQuota that returned a constant, or that had
	// stopped counting anything at all.
	mailHash := sha256.Sum256([]byte("a-mail-blob"))
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1, $2, 5000000, 5000000, $3)`,
		a.ID, mailHash[:], NewULID().String()); err != nil {
		t.Fatalf("seed mail blob: %v", err)
	}
	if withMail := quotaUsed(t, ctx, s, aID); withMail != after+5_000_000 {
		t.Fatalf("a mail blob of the same size moved the quota to %d, want %d — "+
			"the exclusion is not selective, it is counting nothing",
			withMail, after+5_000_000)
	}
}

// TestMailInsertPromotesACacheBlob is prerequisite 2.
//
// Blobs are addressed by the hash of their PLAINTEXT, so if a cached image's
// bytes ever equalled a message's, ONE row would serve both. If the cache got
// there first that row says 'proxy_cache' — and the user's own mail would be
// silently excluded from their quota, which is the single thing the column
// exists to get right.
func TestMailInsertPromotesACacheBlob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	_, contentHash := seedCachedImage(t, ctx, h, a.ID, "bytes-that-collide", 1_000)
	if got := blobKind(t, ctx, h, a.ID, contentHash); got != "proxy_cache" {
		t.Fatalf("seed kind = %q, want proxy_cache", got)
	}
	beforeQuota := quotaUsed(t, ctx, s, aID)

	// The same bytes arrive as MAIL.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.InsertBlobMeta(ctx, BlobMeta{
			ContentHash: contentHash, SizePlain: 1_000, SizeStored: 1_000,
			DEKID: NewULID(), EnvelopeVer: 1, Kind: BlobKindMail,
		})
	}); err != nil {
		t.Fatalf("insert mail blob: %v", err)
	}

	if got := blobKind(t, ctx, h, a.ID, contentHash); got != "mail" {
		t.Errorf("kind = %q after a mail insert, want mail — the user's own bytes "+
			"are still filed as a stranger's image and are invisible to their quota", got)
	}
	if after := quotaUsed(t, ctx, s, aID); after != beforeQuota+1_000 {
		t.Errorf("quota went %d -> %d, want +1000: the promoted row must now be counted",
			beforeQuota, after)
	}

	// ⚠ AND THE REVERSE MUST NOT HAPPEN. A cache insert over a mail row would
	// DEMOTE the user's mail out of their quota, which is the same bug from the
	// other side.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.InsertBlobMeta(ctx, BlobMeta{
			ContentHash: contentHash, SizePlain: 1_000, SizeStored: 1_000,
			DEKID: NewULID(), EnvelopeVer: 1, Kind: BlobKindProxyCache,
		})
	}); err != nil {
		t.Fatalf("insert cache blob over mail: %v", err)
	}
	if got := blobKind(t, ctx, h, a.ID, contentHash); got != "mail" {
		t.Errorf("a proxy_cache insert DEMOTED a mail row to %q — the user's mail "+
			"just fell out of their own quota", got)
	}
}

func blobKind(t *testing.T, ctx context.Context, h *harness, userID string, hash []byte) string {
	t.Helper()
	var kind string
	if err := h.super.QueryRow(ctx,
		`SELECT kind FROM app.blobs WHERE user_id = $1 AND content_hash = $2`,
		userID, hash).Scan(&kind); err != nil {
		t.Fatalf("read blob kind: %v", err)
	}
	return kind
}

// TestReclaimKeepsABlobTheCacheStillReferences is prerequisite 3, and it is
// #300's ml_models hazard with a new writer.
//
// A cache entry names its bytes by content hash with NO foreign key, exactly
// like an attachment. Reclaiming that row would delete the file AND the dek_id
// the ciphertext is opened with — so the bytes would not leak, they would become
// permanently unreadable.
func TestReclaimKeepsABlobTheCacheStillReferences(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	_, cached := seedCachedImage(t, ctx, h, a.ID, "still-referenced", 100)
	// An unreferenced blob alongside it, so the test can tell "the guard held"
	// from "the reclaim did nothing at all".
	orphan := sha256.Sum256([]byte("nothing-references-me"))
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id, kind)
		VALUES ($1, $2, 10, 10, $3, 'proxy_cache')`,
		a.ID, orphan[:], NewULID().String()); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	var reclaimed [][]byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		reclaimed, e = tx.reclaimUnreferencedBlobs(ctx, [][]byte{cached, orphan[:]})
		return e
	}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	for _, h2 := range reclaimed {
		if bytes.Equal(h2, cached) {
			t.Error("the reclaim took a blob the image cache still references. Its row " +
				"carries the dek_id the ciphertext is opened with, so the cached image " +
				"is now permanently unreadable rather than merely missing (#300)")
		}
	}
	if !blobRowExists(t, ctx, h, a.ID, cached) {
		t.Error("the cache's blob row is gone")
	}
	// ⚠ CONTROL. Without this the test passes for a reclaim that never deletes
	// anything, which is exactly what "the guard held" would look like.
	if blobRowExists(t, ctx, h, a.ID, orphan[:]) {
		t.Error("the UNREFERENCED blob survived too — this test cannot tell a working " +
			"guard from a reclaim that does nothing")
	}
}

// TestProxyCacheLookupTouchesLastUsed: eviction is least-recently-USED, so a hit
// that did not record itself would make a frequently-read image look stale and
// evict exactly what the cache works hardest for.
func TestProxyCacheLookupTouchesLastUsed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	urlHash, _ := seedCachedImage(t, ctx, h, a.ID, "touch-me", 10)
	if _, err := h.super.Exec(ctx,
		`UPDATE app.proxy_image_cache SET last_used_at = now() - interval '10 days'
		  WHERE user_id = $1 AND url_hash = $2`, a.ID, urlHash); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	var entry ProxyCacheEntry
	var found bool
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		entry, found, e = tx.LookupProxyImage(ctx, urlHash)
		return e
	}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found {
		t.Fatal("the seeded entry was not found")
	}
	if time.Since(entry.LastUsedAt) > time.Minute {
		t.Errorf("last_used_at is %s old after a hit — the read did not touch it, so "+
			"eviction is least-recently-FETCHED and will drop hot entries",
			time.Since(entry.LastUsedAt))
	}
	// A miss is an ordinary false, not an error.
	miss := sha256.Sum256([]byte("never-cached"))
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, found, _ = tx.LookupProxyImage(ctx, miss[:])
		return nil
	}); err != nil {
		t.Fatalf("miss lookup: %v", err)
	}
	if found {
		t.Error("a URL that was never cached reported a hit")
	}
}

// TestProxyCacheEvictsOldestUsedFirst: the cache must be bounded, and it must
// drop the right entries.
func TestProxyCacheEvictsOldestUsedFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// ⚠ CLEAR THE SHARED FIXTURE'S ROW FIRST. seedRLSUser seeds one
	// proxy_image_cache row per user so the RLS table inventory has something to
	// probe, so this test does NOT start empty. An earlier version assumed three
	// rows, got four, and read a perfectly correct eviction as a failure — the
	// population a test asserts on has to be the population it created.
	clearProxyCache(t, ctx, h, a.ID)

	// Three 100-byte entries, used 3 / 2 / 1 days ago.
	type seeded struct {
		url  []byte
		days int
	}
	var rows []seeded
	for i, days := range []int{3, 2, 1} {
		u, _ := seedCachedImage(t, ctx, h, a.ID, string(rune('a'+i))+"-image", 100)
		// The interval is built in Go and passed as TEXT: pgx will not encode a
		// Go int into an untyped SQL parameter that Postgres has inferred as
		// text, which is what `($3 || ' days')` asks for.
		if _, err := h.super.Exec(ctx,
			`UPDATE app.proxy_image_cache SET last_used_at = now() - $3::interval
			  WHERE user_id = $1 AND url_hash = $2`,
			a.ID, u, fmt.Sprintf("%d days", days)); err != nil {
			t.Fatalf("age row: %v", err)
		}
		rows = append(rows, seeded{url: u, days: days})
	}

	// Cap at 250 bytes: two entries fit, the oldest-used must go.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, e := tx.EvictProxyImages(ctx, 250, 0)
		return e
	}); err != nil {
		t.Fatalf("evict: %v", err)
	}

	var usage ProxyCacheUsage
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		usage, e = tx.ProxyCacheSize(ctx)
		return e
	}); err != nil {
		t.Fatalf("size: %v", err)
	}
	if usage.Bytes > 250 {
		t.Errorf("cache is %d bytes after eviction to a 250-byte cap", usage.Bytes)
	}
	if usage.Entries != 2 {
		t.Fatalf("entries = %d, want 2 — eviction dropped the wrong number", usage.Entries)
	}
	// The 3-day-old one is the one that should be gone, and the 1-day-old one
	// must have survived: asserting only the count would pass if it dropped the
	// newest.
	if cacheRowExists(t, ctx, h, a.ID, rows[0].url) {
		t.Error("the least-recently-used entry survived eviction")
	}
	if !cacheRowExists(t, ctx, h, a.ID, rows[2].url) {
		t.Error("the most-recently-used entry was evicted — the order is inverted")
	}
}

// TestProxyCacheEvictsByAge covers the TTL bound, which is what stops an image
// nobody has looked at in months from sitting there forever under the size cap.
func TestProxyCacheEvictsByAge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	clearProxyCache(t, ctx, h, a.ID)

	oldURL, _ := seedCachedImage(t, ctx, h, a.ID, "ancient", 10)
	freshURL, _ := seedCachedImage(t, ctx, h, a.ID, "recent", 10)
	if _, err := h.super.Exec(ctx,
		`UPDATE app.proxy_image_cache SET last_used_at = now() - interval '40 days'
		  WHERE user_id = $1 AND url_hash = $2`, a.ID, oldURL); err != nil {
		t.Fatalf("age row: %v", err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, e := tx.EvictProxyImages(ctx, 0, 30*24*time.Hour)
		return e
	}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if cacheRowExists(t, ctx, h, a.ID, oldURL) {
		t.Error("an entry unused for 40 days survived a 30-day TTL")
	}
	// CONTROL: the fresh one is untouched, or the TTL is deleting everything.
	if !cacheRowExists(t, ctx, h, a.ID, freshURL) {
		t.Error("the recently used entry was evicted by a 30-day TTL too")
	}
}

// TestProxyCacheEvictionIsANoOpWithNoBounds: both bounds off is a legitimate
// configuration (cache everything, forever), not a mistake to guess about.
func TestProxyCacheEvictionIsANoOpWithNoBounds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	u, _ := seedCachedImage(t, ctx, h, a.ID, "keep-me", 10)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		reclaimed, e := tx.EvictProxyImages(ctx, 0, 0)
		if len(reclaimed) != 0 {
			t.Errorf("unbounded eviction reclaimed %d hashes", len(reclaimed))
		}
		return e
	}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if !cacheRowExists(t, ctx, h, a.ID, u) {
		t.Error("an unbounded eviction deleted an entry")
	}
}

// TestProxyCacheIsPerUser: the cache is keyed by URL hash, which is NOT
// user-scoped in its own right. RLS is what keeps one reader's cache from
// answering another's lookup — and the same image really is cached by two people
// at the same URL, so this is an ordinary case rather than an exotic one.
func TestProxyCacheIsPerUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	urlHash, _ := seedCachedImage(t, ctx, h, b.ID, "b-cached-this", 10)

	var found bool
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		_, found, _ = tx.LookupProxyImage(ctx, urlHash)
		return nil
	}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found {
		t.Error("A got a cache hit on an image only B has fetched — the cache is a " +
			"cross-user read, and it also discloses that B was sent that image")
	}
	// CONTROL: B does get the hit, or this passes for a cache that never hits.
	if err := s.UserTx(ctx, mustULID(t, b.ID), func(tx *UserTx) error {
		_, found, _ = tx.LookupProxyImage(ctx, urlHash)
		return nil
	}); err != nil {
		t.Fatalf("lookup as B: %v", err)
	}
	if !found {
		t.Fatal("B did not get a hit on B's own cached image — this test cannot " +
			"distinguish isolation from a cache that never returns anything")
	}
}

func cacheRowExists(t *testing.T, ctx context.Context, h *harness, userID string, urlHash []byte) bool {
	t.Helper()
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.proxy_image_cache WHERE user_id = $1 AND url_hash = $2`,
		userID, urlHash).Scan(&n); err != nil {
		t.Fatalf("count cache rows: %v", err)
	}
	return n > 0
}

// clearProxyCache removes a user's cache rows so a test controls its own
// population. seedRLSUser seeds one row per user for the RLS inventory probe,
// which is invisible to a test that only counts what it inserted itself.
func clearProxyCache(t *testing.T, ctx context.Context, h *harness, userID string) {
	t.Helper()
	if _, err := h.super.Exec(ctx,
		`DELETE FROM app.proxy_image_cache WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clear proxy cache: %v", err)
	}
}
