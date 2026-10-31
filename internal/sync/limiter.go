// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	stdsync "sync"
	"time"
)

// Limiter paces a class of IMAP commands. Wait blocks until the caller may
// issue the next one, or returns ctx's error if the caller gave up first.
//
// It is an interface, not a concrete type, for the same reason everything else
// in this package is: the sync engine must be able to run unpaced in tests and
// against a server with no such limit, and a deployment must be able to supply
// its own policy without this package growing a dependency (there is no
// golang.org/x/time in this module, deliberately — NewSearchLimiter is the
// forty-line answer).
type Limiter interface {
	// Wait blocks until one unit of the limiter's budget is available. A
	// non-nil error means the caller MUST NOT issue the command; it is
	// ctx.Err() for a cancelled or expired context.
	Wait(ctx context.Context) error
}

// NewSearchLimiter returns a token-bucket Limiter admitting at most perMinute
// calls per minute, allowing burst of them back-to-back after an idle period
// (burst <= 0 is treated as 1 — strict spacing, no bursting).
//
// perMinute <= 0 RETURNS NIL, meaning "no pacing at all". That is the documented
// off switch, and a nil Limiter is what every caller in this package already
// treats as "behave exactly as before" — so a test, or a deployment against a
// server with no SEARCH limit, costs nothing.
//
// # Why this exists (#638)
//
// Migadu refuses anything over 60 UID SEARCHes per rolling minute, per account.
// An account with hundreds of mailboxes issued one SEARCH per due mailbox as
// fast as round trips allowed, so most observation passes had SEARCHes refused
// and thousands of commands were sent to a server that had already said
// no. Spacing the calls is the only fix that also leaves budget for the user's
// other clients and for writeback, which spends the same budget
// (SearchMessageID/UIDPresent are UID SEARCHes too).
//
// Strict spacing rather than a refill-and-drain bucket is deliberate: a burst of
// B on top of perMinute can put B+perMinute commands into one 60-second window,
// which is exactly the shape the server is counting.
func NewSearchLimiter(perMinute int, burst int) Limiter {
	if perMinute <= 0 {
		return nil
	}
	return newTokenBucket(perMinute, burst, time.Now, sleepContext)
}

// tokenBucket is the NewSearchLimiter implementation. Safe for concurrent use:
// the mutex covers only the reservation, and every caller sleeps outside it, so
// N goroutines reserve N distinct slots rather than all sleeping for the same
// one.
type tokenBucket struct {
	spacing time.Duration // one token every spacing
	credit  time.Duration // how far in the past allowAt may be pulled back: the burst
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error

	mu      stdsync.Mutex
	allowAt time.Time // the instant the NEXT reservation may proceed
}

var _ Limiter = (*tokenBucket)(nil)

// newTokenBucket builds the bucket with an injectable clock and sleep, which is
// what makes the pacing testable without a test that actually waits minutes.
func newTokenBucket(perMinute, burst int, now func() time.Time, sleep func(context.Context, time.Duration) error) *tokenBucket {
	if burst < 1 {
		burst = 1
	}
	spacing := time.Minute / time.Duration(perMinute)
	return &tokenBucket{
		spacing: spacing,
		// burst-1 spacings of credit: after a long idle the first call goes
		// immediately and burst-1 more follow it without waiting.
		credit: spacing * time.Duration(burst-1),
		now:    now,
		sleep:  sleep,
	}
}

func (b *tokenBucket) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	b.mu.Lock()
	now := b.now()
	// Clamp forward before reserving. Without this, an account idle for an hour
	// would bank an hour of tokens and then fire them all at once — which is the
	// burst the server refuses, reintroduced by the thing meant to prevent it.
	if earliest := now.Add(-b.credit); b.allowAt.Before(earliest) {
		b.allowAt = earliest
	}
	at := b.allowAt
	b.allowAt = at.Add(b.spacing)
	b.mu.Unlock()

	d := at.Sub(now)
	if d <= 0 {
		return nil
	}
	return b.sleep(ctx, d)
}

// sleepContext waits d, or returns early with ctx's error. A cancelled wait is
// an ordinary error the caller propagates — never a silently skipped pace, which
// would hand the burst straight back to the server.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
