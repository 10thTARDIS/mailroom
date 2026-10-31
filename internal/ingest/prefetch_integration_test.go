// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// The image prefetch at arrival (#922): ingest QUEUES one job — it fetches
// nothing — for a new message from a sender the proxy's own rule allows,
// received inside the window. Every "no job" case below is a control for the
// one that queues, and the first case is the control for all of them: without
// it, a queue that never fires would pass every refusal.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func (f *fixture) prefetchJobs(t *testing.T, msg ulid.ULID) int {
	t.Helper()
	return f.scalar(t, `SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = $2 AND payload->>'message_id' = $3`,
		f.userID.String(), store.ProxyPrefetchJobKind, msg.String())
}

func TestIngestQueuesTheImagePrefetchOnlyWhereTheProxyWouldFetch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.remote_allows (id, user_id, kind, value) VALUES ($1, $2, 'domain', 'news.example')`,
		ulid.Make().String(), f.userID.String()); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	const window = 7 * 24 * time.Hour
	f.ing = New(f.st, f.ing.blobs, WithImagePrefetch(window))

	junk := ulid.Make()
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		 VALUES ($1, $2, $3, 'Spam', 'Spam', '\Junk')`,
		junk.String(), f.userID.String(), f.accountID.String()); err != nil {
		t.Fatalf("seed junk: %v", err)
	}

	uid := int64(7000)
	mailbox := f.mailboxA
	var extra []string
	arrive := func(from string, received time.Time) Result {
		t.Helper()
		uid++
		p := f.prov(mailbox, uid, nil)
		p.InternalDate = received
		res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
			rawFrom(from, "issue "+ulid.Make().String(), ulid.Make().String()+"@news.example", extra...), p)
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if !res.Created {
			t.Fatal("expected a new message")
		}
		return res
	}

	allowed := arrive("Letters <letters@news.example>", time.Now().Add(-time.Hour))
	if n := f.prefetchJobs(t, allowed.MessageID); n != 1 {
		t.Fatalf("an allowed sender's new message queued %d prefetch jobs, want 1", n)
	}
	if n := f.prefetchJobs(t, arrive("someone@stranger.example", time.Now()).MessageID); n != 0 {
		t.Errorf("a sender with no grant queued %d jobs", n)
	}
	if n := f.prefetchJobs(t, arrive("letters@news.example", time.Now().Add(-30*24*time.Hour)).MessageID); n != 0 {
		t.Errorf("mail received 30 days ago queued %d jobs; the window is %s", n, window)
	}
	// Authentication that clearly failed: learned placement's verdict refuses
	// it, so the prefetch is not queued (#922 review M1).
	extra = []string{"Authentication-Results: mx.example; spf=fail smtp.mailfrom=news.example; dkim=fail; dmarc=fail header.from=news.example"}
	if n := f.prefetchJobs(t, arrive("letters@news.example", time.Now()).MessageID); n != 0 {
		t.Errorf("mail whose authentication failed queued %d jobs", n)
	}
	extra = nil
	// Arriving in Junk: not queued, even from an allowed sender.
	mailbox = junk
	if n := f.prefetchJobs(t, arrive("letters@news.example", time.Now()).MessageID); n != 0 {
		t.Errorf("an allowed sender's mail arriving in Junk queued %d jobs", n)
	}
	mailbox = f.mailboxA

	// Without the option (bulk import, the CLIs) nothing is queued at all.
	f.ing = New(f.st, f.ing.blobs)
	if n := f.prefetchJobs(t, arrive("letters@news.example", time.Now()).MessageID); n != 0 {
		t.Errorf("an Ingestor without WithImagePrefetch queued %d jobs", n)
	}
	// The message still landed normally in every case: the enqueue is fail-soft
	// and additive. (Counted rather than assumed.)
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id = $1`, f.userID.String()); got != 6 {
		t.Errorf("messages stored = %d, want 6", got)
	}
}
