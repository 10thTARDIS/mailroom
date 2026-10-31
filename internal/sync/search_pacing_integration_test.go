// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package sync_test

import (
	"context"
	"errors"
	stdsync "sync"
	"testing"
	"time"

	mrsync "mailroom/internal/sync"
)

// ⚠ THIS IS THE TEST THAT PROVES THE WIRE IS CONNECTED (#638).
//
// internal/sync's unit tests prove the token bucket paces and that a refusal is
// classified; internal/mailsync's prove the engine stops asking. NONE of them
// would notice if Config.SearchLimiter were simply never consulted by the
// session — the limiter would be built, handed around, shared per account, and
// do nothing, and every other test in the tree would still be green. That is
// exactly the "ran without erroring while doing nothing" failure this project
// keeps paying for, so the check lives here, against a real IMAP server, where
// the three UIDSearch call sites actually execute.

// countingLimiter records every Wait and can be made to refuse.
type countingLimiter struct {
	mu   stdsync.Mutex
	n    int
	fail error
}

func (c *countingLimiter) Wait(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.fail
}

func (c *countingLimiter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

var _ mrsync.Limiter = (*countingLimiter)(nil)

// TestLiveSearchLimiterIsConsultedBeforeEverySearch: each of the three commands
// that issue a UID SEARCH must spend exactly one unit of the account's budget,
// and nothing else may spend any.
func TestLiveSearchLimiterIsConsultedBeforeEverySearch(t *testing.T) {
	cfg := testConfig(t)
	lim := &countingLimiter{}
	cfg.SearchLimiter = lim
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// THE NEGATIVE CONTROL, FIRST. LIST, SELECT and FETCH are not SEARCHes and
	// must not consume the SEARCH budget — a limiter wired into withContext
	// generally, rather than at the three call sites, would throttle the whole
	// sync to 30 commands a minute and this is what would catch it.
	if _, err := sess.ListMailboxes(ctx); err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if _, err := sess.Select(ctx, mailboxInbox, mrsync.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if err := sess.FetchByUID(ctx, mrsync.UIDSetNum(1), mrsync.FetchItems{Flags: true},
		func(mrsync.Message) error { return nil }); err != nil {
		t.Fatalf("FetchByUID: %v", err)
	}
	if got := lim.count(); got != 0 {
		t.Fatalf("LIST+SELECT+FETCH spent %d units of the SEARCH budget, want 0", got)
	}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"LiveUIDs", func() error {
			_, _, err := sess.LiveUIDs(ctx, mailboxInbox)
			return err
		}},
		{"SearchMessageID", func() error {
			_, err := sess.SearchMessageID(ctx, mailboxInbox, "nothing-matches-this@example.com")
			return err
		}},
		{"UIDPresent", func() error {
			_, err := sess.UIDPresent(ctx, mailboxInbox, 1)
			return err
		}},
	} {
		before := lim.count()
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := lim.count() - before; got != 1 {
			t.Errorf("%s spent %d units of the SEARCH budget, want exactly 1 — "+
				"it is a UID SEARCH and the server counts it", tc.name, got)
		}
	}
}

// TestLiveSearchLimiterRefusalStopsTheSearch: when Wait refuses, the command
// must fail with THAT error. A silently skipped pace would return the server's
// answer instead, which is precisely the burst the limiter exists to prevent.
func TestLiveSearchLimiterRefusalStopsTheSearch(t *testing.T) {
	refused := errors.New("limiter refused")
	cfg := testConfig(t)
	lim := &countingLimiter{fail: refused}
	cfg.SearchLimiter = lim
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A control first: the SAME call against the SAME mailbox succeeds when the
	// limiter permits it, so the failure below is the limiter and not the fixture.
	permissive := dial(t, testConfig(t))
	if _, _, err := permissive.LiveUIDs(ctx, mailboxInbox); err != nil {
		t.Fatalf("control: LiveUIDs with no limiter: %v", err)
	}

	status, uids, err := sess.LiveUIDs(ctx, mailboxInbox)
	if !errors.Is(err, refused) {
		t.Fatalf("LiveUIDs with a refusing limiter = (%v, %v, %v), want the limiter's error",
			status, uids, err)
	}
	if status != nil || uids != nil {
		t.Errorf("LiveUIDs returned (%v, %v) alongside the error; a refused pace must yield nothing",
			status, uids)
	}
	if _, err := sess.SearchMessageID(ctx, mailboxInbox, "x@example.com"); !errors.Is(err, refused) {
		t.Errorf("SearchMessageID with a refusing limiter = %v, want the limiter's error", err)
	}
	if _, err := sess.UIDPresent(ctx, mailboxInbox, 1); !errors.Is(err, refused) {
		t.Errorf("UIDPresent with a refusing limiter = %v, want the limiter's error", err)
	}
}

// TestLiveNilSearchLimiterIsUnpaced is the off switch, end to end: a Config with
// no limiter must behave exactly as it did before #638.
func TestLiveNilSearchLimiterIsUnpaced(t *testing.T) {
	cfg := testConfig(t)
	if cfg.SearchLimiter != nil {
		t.Fatal("testConfig should not set a limiter")
	}
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, _, err := sess.LiveUIDs(ctx, mailboxInbox); err != nil {
			t.Fatalf("LiveUIDs #%d: %v", i, err)
		}
	}
	// Five searches at any sane pacing rate would take seconds; unpaced they are
	// five round trips against a container on the same network.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("5 unpaced LiveUIDs took %v; something is pacing a nil limiter", elapsed)
	}
}
