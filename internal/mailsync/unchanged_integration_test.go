// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package mailsync

// #620 against a REAL server and a REAL database. The unit tests prove the
// decisions; only this proves the numbers they compare are the same numbers in
// practice: that Dovecot's LIST-STATUS reports exactly what its SELECT and its
// EXAMINE report, that the baselines survive the round trip through the store
// adapter and migration 00058's columns, and so that the skips actually FIRE —
// and stop firing the moment the other client changes something. Run via
// scripts/mailsync-test.sh; without Dovecot + PostgreSQL it skips.

import (
	"context"
	gosync "sync"
	"testing"
	"time"

	"mailroom/internal/ingest"
	"mailroom/internal/sync"
)

// countingSession is a real session that counts the two commands #620 skips.
type countingSession struct {
	sync.SyncSession
	mu      gosync.Mutex
	selects map[string]int
	lives   map[string]int
}

func (s *countingSession) Select(ctx context.Context, name string, opts sync.SelectOptions) (*sync.MailboxStatus, error) {
	s.mu.Lock()
	s.selects[name]++
	s.mu.Unlock()
	return s.SyncSession.Select(ctx, name, opts)
}

func (s *countingSession) LiveUIDs(ctx context.Context, name string) (*sync.MailboxStatus, []sync.UID, error) {
	s.mu.Lock()
	s.lives[name]++
	s.mu.Unlock()
	return s.SyncSession.LiveUIDs(ctx, name)
}

// countingPass runs one pass through a fresh counting session.
func (f *liveFixture) countingPass(t *testing.T) (Summary, *countingSession) {
	t.Helper()
	counter := &countingSession{selects: map[string]int{}, lives: map[string]int{}}
	e := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			counter.SyncSession = s
			return counter, nil
		}, WithReapInterval(time.Nanosecond)) // every folder is due every pass
	return f.run(t, e), counter
}

func TestLiveUnchangedFoldersAreSkippedUntilTheyChange(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	f.appendMsg(t, f.inbox, "skip-inbox-1")
	archived := f.appendMsg(t, f.archive, "skip-archive-1")

	// Pass 1: no baseline exists yet, so every folder is SELECTed and observed —
	// exactly the behaviour before #620.
	sum, c := f.countingPass(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("pass 1 errors: %v", sum.Errors)
	}
	folders := sum.MailboxesSynced
	if folders < 2 || sum.MailboxesUnchanged != 0 || sum.ObservationsSkipped != 0 {
		t.Fatalf("pass 1: synced=%d unchanged=%d skipped=%d, want >=2/0/0", folders, sum.MailboxesUnchanged, sum.ObservationsSkipped)
	}
	if c.selects[f.inbox] != 1 || c.lives[f.inbox] != 1 {
		t.Fatalf("pass 1: INBOX selects=%d observations=%d, want 1/1", c.selects[f.inbox], c.lives[f.inbox])
	}

	// Pass 2: nothing changed anywhere. EVERY folder is skipped by both phases —
	// which can only happen if Dovecot's LIST-STATUS numbers equal the SELECT's
	// (poll baseline) AND the EXAMINE's (observation baseline), and both
	// baselines made it into the database.
	sum, c = f.countingPass(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("pass 2 errors: %v", sum.Errors)
	}
	if sum.MailboxesUnchanged != folders {
		t.Errorf("pass 2: MailboxesUnchanged = %d, want all %d folders (selects %v)", sum.MailboxesUnchanged, folders, c.selects)
	}
	if sum.ObservationsSkipped != folders || sum.MailboxesObserved != 0 {
		t.Errorf("pass 2: ObservationsSkipped = %d, MailboxesObserved = %d; want %d and 0 (observations %v)",
			sum.ObservationsSkipped, sum.MailboxesObserved, folders, c.lives)
	}
	if len(c.selects) != 0 || len(c.lives) != 0 {
		t.Errorf("pass 2 sent SELECTs %v and observations %v, want none", c.selects, c.lives)
	}
	if sum.MailboxesSynced != folders {
		t.Errorf("pass 2: MailboxesSynced = %d, want %d — a skipped folder is still in good standing", sum.MailboxesSynced, folders)
	}

	// Pass 3: the OTHER client delivers to INBOX and flags the Archive message.
	// Both folders must be polled and observed again, and the new mail arrive.
	f.appendMsg(t, f.inbox, "skip-inbox-2")
	if err := f.sess.StoreFlags(context.Background(), f.archive, archived, []sync.Flag{sync.FlagFlagged}, nil); err != nil {
		t.Fatalf("flag on server: %v", err)
	}
	sum, c = f.countingPass(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("pass 3 errors: %v", sum.Errors)
	}
	for _, mb := range []string{f.inbox, f.archive} {
		if c.selects[mb] != 1 || c.lives[mb] != 1 {
			t.Errorf("pass 3: %s selects=%d observations=%d, want 1/1 — it changed on the server", mb, c.selects[mb], c.lives[mb])
		}
	}
	if _, ok := f.messageIDBySubject(t, "skip-inbox-2"); !ok {
		t.Error("the new INBOX message was not ingested")
	}
	if sum.MailboxesUnchanged != folders-2 {
		t.Errorf("pass 3: MailboxesUnchanged = %d, want %d (every folder but the two that changed)", sum.MailboxesUnchanged, folders-2)
	}
}
