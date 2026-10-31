// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Prefetched-image retention (#922, app.proxy_image_refs, migration 00081).
//
// The rule under test, as decided on 2026-10-27: an image a
// live message references is kept for as long as that message is kept — the
// unread TTL does not evict it and the size cap does not evict it — and it is
// released when the last referencing message is DESTROYED (the row deleted).
// Trash does not release. The cap still bounds unreferenced entries.
//
// Each test asserts on specific url hashes and carries a control that the
// eviction under test actually evicts something, so "kept" cannot be a pass
// for an eviction that does nothing.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func ageCacheRow(t *testing.T, ctx context.Context, h *harness, userID string, urlHash []byte, ago string) {
	t.Helper()
	if _, err := h.super.Exec(ctx,
		`UPDATE app.proxy_image_cache SET last_used_at = now() - $3::interval
		  WHERE user_id = $1 AND url_hash = $2`, userID, urlHash, ago); err != nil {
		t.Fatalf("age cache row: %v", err)
	}
}

func evictAs(t *testing.T, ctx context.Context, s *Store, userID ulid.ULID, maxBytes int64, ttl time.Duration) [][]byte {
	t.Helper()
	var reclaimed [][]byte
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		reclaimed, e = tx.EvictProxyImages(ctx, maxBytes, ttl)
		return e
	}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	return reclaimed
}

func addRef(t *testing.T, ctx context.Context, s *Store, userID ulid.ULID, urlHash []byte, msg ulid.ULID) {
	t.Helper()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.AddProxyImageRef(ctx, urlHash, msg)
	}); err != nil {
		t.Fatalf("add ref: %v", err)
	}
}

func refCount(t *testing.T, ctx context.Context, s *Store, userID ulid.ULID, urlHash []byte) int {
	t.Helper()
	var n int
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		n, e = tx.ProxyImageRefCount(ctx, urlHash)
		return e
	}); err != nil {
		t.Fatalf("ref count: %v", err)
	}
	return n
}

// TestReferencedImageOutlivesTheTTLUntilItsMessageIsDestroyed is the retention
// rule end to end: past the TTL and kept; trashed and kept; destroyed and then
// evicted by the very same TTL pass that kept it before.
func TestReferencedImageOutlivesTheTTLUntilItsMessageIsDestroyed(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	h, s := f.h, f.st
	user := f.userID.String()
	clearProxyCache(t, ctx, h, user)
	const ttl = 30 * 24 * time.Hour

	msg := f.seedMessage(t, "a newsletter with a signed image")
	kept, keptBlob := seedCachedImage(t, ctx, h, user, "prefetched-"+msg.String(), 100)
	// CONTROL: an unreferenced entry just as old. If the TTL pass does not
	// evict THIS, "kept" below proves nothing.
	dropped, _ := seedCachedImage(t, ctx, h, user, "opened-once-"+msg.String(), 100)
	ageCacheRow(t, ctx, h, user, kept, "60 days")
	ageCacheRow(t, ctx, h, user, dropped, "60 days")
	addRef(t, ctx, s, f.userID, kept, msg)

	evictAs(t, ctx, s, f.userID, 0, ttl)
	if cacheRowExists(t, ctx, h, user, dropped) {
		t.Fatal("CONTROL: an unreferenced entry 60 days unread survived a 30-day TTL — " +
			"this test cannot tell retention from an eviction that does nothing")
	}
	if !cacheRowExists(t, ctx, h, user, kept) {
		t.Fatal("a referenced image was evicted by the unread TTL while its message is " +
			"still kept; its URL may have expired, so the picture is gone for good")
	}

	// Trash is restorable, so it must not release the image.
	trash(t, f, msg, time.Now().Add(-time.Hour))
	evictAs(t, ctx, s, f.userID, 0, ttl)
	if !cacheRowExists(t, ctx, h, user, kept) {
		t.Fatal("moving the message to Trash released its image; an undo would bring " +
			"back a message whose picture cannot be fetched again")
	}
	if n := refCount(t, ctx, s, f.userID, kept); n != 1 {
		t.Fatalf("references after trash = %d, want 1", n)
	}

	// Destroy it through the real purge path.
	if _, _, ok := hardDelete(t, f, msg, time.Now()); !ok {
		t.Fatal("hard delete did not run")
	}
	if n := refCount(t, ctx, s, f.userID, kept); n != 0 {
		t.Fatalf("references after the message was destroyed = %d, want 0 — the "+
			"cascade did not release the image", n)
	}
	reclaimed := evictAs(t, ctx, s, f.userID, 0, ttl)
	if cacheRowExists(t, ctx, h, user, kept) {
		t.Error("released image (60 days unread, no reference) survived the TTL — " +
			"it should fall back to the ordinary TTL from its last use")
	}
	var freed bool
	for _, r := range reclaimed {
		if string(r) == string(keptBlob) {
			freed = true
		}
	}
	if !freed {
		t.Error("the released image's blob row was not reclaimed with it")
	}
}

// TestSizeCapNeverEvictsAReferencedImage: the cap bounds UNREFERENCED entries
// only (decided 2026-10-27). A referenced entry is neither a victim
// nor counted toward the cap.
func TestSizeCapNeverEvictsAReferencedImage(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	h, s := f.h, f.st
	user := f.userID.String()
	clearProxyCache(t, ctx, h, user)

	msg := f.seedMessage(t, "big referenced image")
	ref, _ := seedCachedImage(t, ctx, h, user, "ref-"+msg.String(), 1000)
	older, _ := seedCachedImage(t, ctx, h, user, "older-"+msg.String(), 100)
	newer, _ := seedCachedImage(t, ctx, h, user, "newer-"+msg.String(), 100)
	// The referenced entry is the MOST recently used and alone is over the cap,
	// so a cap that still counted it would evict it AND everything after it.
	ageCacheRow(t, ctx, h, user, ref, "1 hour")
	ageCacheRow(t, ctx, h, user, older, "2 days")
	ageCacheRow(t, ctx, h, user, newer, "1 day")
	addRef(t, ctx, s, f.userID, ref, msg)

	evictAs(t, ctx, s, f.userID, 150, 0)

	if !cacheRowExists(t, ctx, h, user, ref) {
		t.Error("the size cap evicted a referenced image")
	}
	// The cap still bounds the unreferenced half: 200 bytes over a 150 cap, so
	// the older one goes ...
	if cacheRowExists(t, ctx, h, user, older) {
		t.Error("CONTROL: the cap evicted nothing unreferenced — it no longer bounds " +
			"ordinary opens at all")
	}
	// ... and the newer one stays, which is only true if the referenced 1000
	// bytes were NOT counted toward the cap.
	if !cacheRowExists(t, ctx, h, user, newer) {
		t.Error("the newest unreferenced image was evicted: the referenced bytes were " +
			"counted toward a cap that no longer applies to them")
	}

	var split ProxyCacheSplit
	if err := s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		split, e = tx.ProxyCacheBreakdown(ctx)
		return e
	}); err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if split != (ProxyCacheSplit{ReferencedEntries: 1, ReferencedBytes: 1000, UnreferencedEntries: 1, UnreferencedBytes: 100}) {
		t.Errorf("breakdown = %+v, want 1/1000 referenced and 1/100 unreferenced", split)
	}
}

// TestProxyImageRefsAreUserIsolated is the table's negative test beyond the
// generic RLS inventory (fixtures_test.go seeds a row per user for that): B
// cannot see or release A's references, and B's own reference to the SAME url
// hash does not protect A's entry — the eviction matches on (user, url_hash),
// and a cross-user match would let one user pin another's cache.
func TestProxyImageRefsAreUserIsolated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	aMsg, bMsg := mustULID(t, a.MessageID), mustULID(t, b.MessageID)
	clearProxyCache(t, ctx, h, a.ID)
	clearProxyCache(t, ctx, h, b.ID)

	body := "shared-newsletter-image-" + a.ID
	aKept, _ := seedCachedImage(t, ctx, h, a.ID, body, 10)
	addRef(t, ctx, s, aID, aKept, aMsg)

	// B sees none of A's references, even naming A's user id.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		var n int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.proxy_image_refs WHERE user_id = $1`, a.ID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("B sees %d of A's image references", n)
		}
		tag, err := tx.tx.Exec(ctx, `DELETE FROM app.proxy_image_refs WHERE url_hash = $1`, aKept)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("B deleted %d of A's references — B released A's prefetched images", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("as B: %v", err)
	}
	if n := refCount(t, ctx, s, aID, aKept); n != 1 {
		t.Fatalf("A's references = %d after B's attempts, want 1", n)
	}

	// B references the same image in B's own mail, and A's reference goes.
	seedCachedImage(t, ctx, h, b.ID, body, 10)
	addRef(t, ctx, s, bID, aKept, bMsg)
	if _, err := h.super.Exec(ctx, `DELETE FROM app.proxy_image_refs WHERE user_id = $1`, a.ID); err != nil {
		t.Fatalf("drop A's ref: %v", err)
	}
	ageCacheRow(t, ctx, h, a.ID, aKept, "60 days")
	evictAs(t, ctx, s, aID, 0, 30*24*time.Hour)
	if cacheRowExists(t, ctx, h, a.ID, aKept) {
		t.Error("B's reference kept A's cache entry alive: eviction matched a reference across users")
	}
	// CONTROL: B's own entry is still protected by B's reference.
	ageCacheRow(t, ctx, h, b.ID, aKept, "60 days")
	evictAs(t, ctx, s, bID, 0, 30*24*time.Hour)
	if !cacheRowExists(t, ctx, h, b.ID, aKept) {
		t.Error("CONTROL: B's referenced entry was evicted — this test cannot tell isolation from refs never protecting anything")
	}
}

// TestEnqueueProxyPrefetchIfAllowed: ingest's half queues one job for an
// allowed sender, none for anyone else, and a repeat is not a second job. The
// payload carries the id and nothing else (jobPayloadKeys).
func TestEnqueueProxyPrefetchIfAllowed(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	h, s := f.h, f.st
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.remote_allows (id, user_id, kind, value) VALUES ($1, $2, 'domain', 'news.example')`,
		NewULID().String(), f.userID.String()); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	allowed := f.seedMessage(t, "from an allowed sender")
	stranger := f.seedMessage(t, "from a stranger")
	junkMsg := f.seedMessage(t, "allowed sender, delivered to junk")
	junk := NewULID()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		 VALUES ($1, $2, $3, 'Spam', 'Spam', '\Junk')`,
		junk.String(), f.userID.String(), f.accountID.String()); err != nil {
		t.Fatalf("seed junk mailbox: %v", err)
	}

	mailbox := f.inbox
	enqueue := func(msg ulid.ULID, from string) bool {
		t.Helper()
		var queued bool
		if err := s.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var e error
			queued, _, e = tx.EnqueueProxyPrefetchIfAllowed(ctx, msg, from, mailbox, time.Now())
			return e
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return queued
	}
	// Ingest passes the parsed address (app.messages.from_address), not the
	// header: the proxy's rule refuses anything that is not exactly one bare
	// address, angle brackets included (ADR-026 §3c), so that is the control.
	if enqueue(stranger, "Letters <letters@news.example>") {
		t.Error("an unparsed From header was queued; the proxy rule is fail-closed on anything but a bare address")
	}
	if !enqueue(allowed, "letters@news.example") {
		t.Error("an allowed sender's message was not queued")
	}
	if enqueue(allowed, "letters@news.example") {
		t.Error("a second enqueue for the same message queued a second job")
	}
	// The proxy's rule, not the client's: two @ is refused (ADR-026 §3c).
	if enqueue(stranger, "attacker@evil.example@news.example") {
		t.Error("a multiple-@ From matching the grant's domain was queued; the proxy rule is fail-closed")
	}
	if enqueue(stranger, "someone@elsewhere.example") {
		t.Error("a sender with no grant was queued")
	}
	// An allowed sender's mail that the provider filed as Junk is not queued:
	// Junk is evidence the From may be forged (#922 review).
	mailbox = junk
	if enqueue(junkMsg, "letters@news.example") {
		t.Error("an allowed sender's message arriving in Junk was queued")
	}

	var n int
	var payload string
	if err := h.super.QueryRow(ctx,
		`SELECT count(*), COALESCE(min(payload::text), '') FROM app.jobs WHERE user_id = $1 AND kind = $2`,
		f.userID.String(), ProxyPrefetchJobKind).Scan(&n, &payload); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("prefetch jobs = %d, want exactly 1", n)
	}
	if want := fmt.Sprintf(`{"message_id": "%s"}`, allowed); payload != want {
		t.Errorf("payload = %s, want %s", payload, want)
	}
}

// TestCollapseCarriesPrefetchedImageRefsToTheKeeper: collapsing duplicate copies
// deletes the losers, and the cascade would take their image references with
// them — handing the keeper's images (the same message) back to the unread TTL.
// The collapse moves them first; a reference both copies hold stays one row.
func TestCollapseCarriesPrefetchedImageRefsToTheKeeper(t *testing.T) {
	f := seedRecords(t, "proxy-refs")
	ctx := context.Background()
	s := f.h.appStore()
	uid := mustULID(t, f.u.ID)
	clearProxyCache(t, ctx, f.h, f.u.ID)

	onlyLoser, _ := seedCachedImage(t, ctx, f.h, f.u.ID, "collapse-loser-"+f.loser.String(), 10)
	both, _ := seedCachedImage(t, ctx, f.h, f.u.ID, "collapse-both-"+f.loser.String(), 10)
	addRef(t, ctx, s, uid, onlyLoser, f.loser)
	addRef(t, ctx, s, uid, both, f.loser)
	addRef(t, ctx, s, uid, both, f.keeper)

	if _, ok := f.collapse(t); !ok {
		t.Fatal("collapse refused")
	}
	for name, h := range map[string][]byte{"loser-only": onlyLoser, "shared": both} {
		var onKeeper, total int
		if err := f.h.super.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE message_id = $3), count(*)
			  FROM app.proxy_image_refs WHERE user_id = $1 AND url_hash = $2`,
			f.u.ID, h, f.keeper.String()).Scan(&onKeeper, &total); err != nil {
			t.Fatalf("count refs: %v", err)
		}
		if onKeeper != 1 || total != 1 {
			t.Errorf("%s image: %d reference(s) on the keeper of %d total, want exactly 1 on the keeper", name, onKeeper, total)
		}
	}
	// And it still holds against the TTL, which is the point.
	ageCacheRow(t, ctx, f.h, f.u.ID, onlyLoser, "60 days")
	evictAs(t, ctx, s, uid, 0, 30*24*time.Hour)
	if !cacheRowExists(t, ctx, f.h, f.u.ID, onlyLoser) {
		t.Error("the collapsed copy's image was evicted: its reference did not reach the keeper")
	}
}

// TestDeletingTheAccountReleasesItsMessagesImages: account removal is a destroy
// path of its own (DeleteAccount deletes the account's messages), and the
// cascade must release their images exactly as the Trash purge does.
func TestDeletingTheAccountReleasesItsMessagesImages(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	h, s := f.h, f.st
	user := f.userID.String()
	clearProxyCache(t, ctx, h, user)

	msg := f.seedMessage(t, "mail in the account being removed")
	f.seedProvenance(t, msg, f.inbox, 41, 1, "live")
	kept, _ := seedCachedImage(t, ctx, h, user, "account-"+msg.String(), 100)
	ageCacheRow(t, ctx, h, user, kept, "60 days")
	addRef(t, ctx, s, f.userID, kept, msg)

	// CONTROL: while the account exists, the TTL keeps the image.
	evictAs(t, ctx, s, f.userID, 0, 30*24*time.Hour)
	if !cacheRowExists(t, ctx, h, user, kept) {
		t.Fatal("CONTROL: the referenced image was evicted before the account was removed")
	}

	if err := s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		found, _, e := tx.DeleteAccount(ctx, f.accountID)
		if e == nil && !found {
			t.Fatal("DeleteAccount did not find the account")
		}
		return e
	}); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if n := refCount(t, ctx, s, f.userID, kept); n != 0 {
		t.Fatalf("references after the account was removed = %d, want 0", n)
	}
	evictAs(t, ctx, s, f.userID, 0, 30*24*time.Hour)
	if cacheRowExists(t, ctx, h, user, kept) {
		t.Error("the removed account's image survived the TTL after its message was destroyed")
	}
}
