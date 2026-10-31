// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mailsync"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// pooledSession is one account's warm, reusable sync connection plus the time it
// was last used. It lives in Runner.pools while idle; a pass CHECKS IT OUT (see
// checkoutPooled) for its whole duration and RETURNS it on success (returnPooled),
// so a session in the map is never one a pass is mid-flight on.
type pooledSession struct {
	sess     sync.SyncSession
	lastUsed time.Time
}

// checkoutPooled removes and returns the account's warm session, or nil if the
// pool holds none. Removing it (rather than reading it in place) is the whole
// safety story: while a pass runs, its session is absent from the map, so the
// reconcile/teardown paths that Close pooled sessions can only ever touch idle
// ones. The per-account gate guarantees a single checkout at a time, so this
// needs no lock beyond the scheduleMu-guarded map access.
func (r *Runner) checkoutPooled(userID, accountID ulid.ULID) *pooledSession {
	r.scheduleMu.Lock()
	defer r.scheduleMu.Unlock()
	byAcct := r.pools[userID]
	if byAcct == nil {
		return nil
	}
	ps := byAcct[accountID]
	if ps != nil {
		delete(byAcct, accountID)
	}
	return ps
}

// returnPooled parks a still-healthy session back in the pool, warm, for the next
// pass, lazily creating the user's map on first use — the same lazy-create +
// forgetUser-delete lifecycle the gates follow (gateFor). A pass that completes
// AFTER forgetUser tore the user down re-creates the map with this one session;
// it is then orphaned until the user's next reconcile or process exit — the same
// benign teardown race the gates carry, except it costs one live connection, so
// it can only happen for a pass that SUCCEEDED (only success returns to the pool)
// after teardown, a window a cancelled shutdown pass never reaches.
func (r *Runner) returnPooled(userID, accountID ulid.ULID, sess sync.SyncSession) {
	r.scheduleMu.Lock()
	byAcct := r.pools[userID]
	if byAcct == nil {
		byAcct = make(map[ulid.ULID]*pooledSession)
		r.pools[userID] = byAcct
	}
	byAcct[accountID] = &pooledSession{sess: sess, lastUsed: r.now()}
	r.scheduleMu.Unlock()
}

// reapVanishedPools closes and drops the pooled sessions of accounts no longer in
// live, called from dueAccounts under scheduleMu alongside the gate reap. Because
// a pooled session is checked out for the length of its pass, anything still in
// the map here is idle, so closing it can never hit a pass mid-flight — no
// gate.running check is needed (unlike the gates, which ARE held across a pass).
// The caller holds scheduleMu.
func (r *Runner) reapVanishedPools(userID ulid.ULID, live map[ulid.ULID]struct{}) {
	byAcct := r.pools[userID]
	if byAcct == nil {
		return
	}
	for id, ps := range byAcct {
		if _, ok := live[id]; ok {
			continue
		}
		_ = ps.sess.Close()
		delete(byAcct, id)
	}
}

// closeUserPools closes every warm session for userID and drops the user's pool
// map. Called from forgetUser when the user's loop exits. A pass still draining at
// that instant has its session CHECKED OUT (absent from the map), so it is not
// closed here; its returnPooled then finds the map gone and closes the session
// itself. The caller holds scheduleMu.
func (r *Runner) closeUserPools(userID ulid.ULID) {
	for _, ps := range r.pools[userID] {
		_ = ps.sess.Close()
	}
	delete(r.pools, userID)
}

// acquireSession returns a live session to run a pass over: a warm pooled one
// that is neither past the idle-eviction timeout NOR failing a NOOP probe, or
// else a freshly dialed one. reused reports whether the returned session came
// from the pool and passed its probe — the retry-once guard needs that, because
// only a reused session can have died in the race between the probe and the
// pass's first real command.
func (r *Runner) acquireSession(ctx context.Context, userID ulid.ULID, acct store.Account, cfg sync.Config) (sess sync.SyncSession, reused bool, err error) {
	if ps := r.checkoutPooled(userID, acct.ID); ps != nil {
		// Reuse only a connection that is both fresh enough AND provably alive.
		// A NOOP that returns nil is the health gate; any error (or an expired
		// entry) means discard and re-dial.
		if r.now().Sub(ps.lastUsed) <= r.poolIdleTimeout && ps.sess.Noop(ctx) == nil {
			return ps.sess, true, nil
		}
		_ = ps.sess.Close()
	}
	sess, err = r.poolDial(ctx, cfg)
	return sess, false, err
}

// syncWithPool runs one account's pass over a warm, reused connection when
// pooling is enabled, re-dialing only when there is no live pooled session. It is
// only ever called from inside runGated, which serializes passes per account, so
// no two goroutines ever race on one account's pooled entry.
//
// The connection lifecycle is: acquire (probe or dial) -> run the pass ->
// release. On a SUCCESSFUL pass the connection is parked back in the pool, warm.
// On a FAILED pass it is closed and NEVER returned to the pool — a failed pass
// may have broken the connection, and even if it did not, discarding is always
// safe because the next pass simply re-dials (the NOOP probe is the sole reuse
// gate, so a stale connection can never be silently believed healthy).
func (r *Runner) syncWithPool(ctx context.Context, userID ulid.ULID, acct store.Account, req mailsync.AccountSync) (mailsync.Summary, error) {
	sess, reused, err := r.acquireSession(ctx, userID, acct, req.Config)
	if err != nil {
		return mailsync.Summary{}, fmt.Errorf("mailsync: dial account %s: %w", acct.ID, err)
	}

	summary, err := r.syncer.SyncAccountWith(ctx, req, sess)
	if err == nil {
		r.returnPooled(userID, acct.ID, sess) // healthy: keep it warm for the next pass
		return summary, nil
	}
	_ = sess.Close() // the pass failed: never return a suspect connection to the pool

	// RETRY-ONCE — the silent-latency guard. A REUSED session passed its NOOP
	// probe at acquire and then failed the pass, so the connection may have died
	// in the race between the probe and the first real command. Re-dial ONCE and
	// re-run the pass, so pooling can never turn a live-connection race into a
	// skipped sync — which would surface only as mail latency, exactly what this
	// stage must not regress. A FRESHLY-dialed session that failed is a genuine
	// failure, not a stale-connection race, so it is NOT retried: that is the
	// same single-attempt outcome the pool-disabled dial-per-pass path would have
	// had. The retry is also skipped on shutdown (ctx cancelled), where re-running
	// would only fail again.
	if !reused || ctx.Err() != nil {
		return summary, err
	}
	fresh, derr := r.poolDial(ctx, req.Config)
	if derr != nil {
		return summary, err // surface the ORIGINAL pass error; the re-dial failure is secondary
	}
	summary, err = r.syncer.SyncAccountWith(ctx, req, fresh)
	if err == nil {
		r.returnPooled(userID, acct.ID, fresh)
		return summary, nil
	}
	_ = fresh.Close()
	return summary, err // a second failure stands as the pass error (backoff, as today)
}
