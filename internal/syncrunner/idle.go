// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	stdsync "sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// DefaultIdleReissue is how often a watcher stops and re-issues IMAP IDLE to
// keep the connection alive (RFC 2177: a server may silently drop an IDLE that
// has run for ~30 minutes, so re-issue safely before that).
const DefaultIdleReissue = 29 * time.Minute

// DefaultIdleTriggerDebounce is the minimum spacing between two IDLE-triggered
// syncs for one account (mailroom#103 Stage 2, Deliverable B). It coalesces a
// burst of new-mail hints into at most one sync per window so a chatty mailbox
// cannot hammer the provider. It is combined with the provider's poll floor
// (see idleDebounce): the actual debounce is max(this, providerFloor), because
// a few-second const alone would NOT protect a throttled provider (e.g. Gmail,
// 5-minute floor) — an IDLE-triggered pass opens a fresh connection just like a
// poll, so its rate must honor the same floor. Skipped hints never lose mail:
// the scheduled poll is always the safety net.
const DefaultIdleTriggerDebounce = 5 * time.Second

// idleAccountRefreshInterval is how often RunIdle re-lists the user's accounts
// to start a watcher for a newly-added account and stop one for a removed
// account. Matches the supervisor's user-refresh cadence.
const idleAccountRefreshInterval = time.Minute

// idleReconnectBackoffBase is the first backoff after an IDLE session drops. It
// doubles from here up to the Runner's maxBackoff (the same ceiling stage-1
// polling uses), with jitter, so a flapping server is retried promptly at first
// and then progressively less often — while the scheduler keeps polling.
const idleReconnectBackoffBase = 2 * time.Second

// idleSelectMailbox is the mailbox a watcher SELECTs to receive new-mail
// (EXISTS) hints. New mail lands in INBOX; the triggered SyncAccount pass
// reconciles every mailbox regardless.
const idleSelectMailbox = "INBOX"

// IDLE connection states reported through the idleState sink to
// metrics.IdleConnState.
const (
	idleStateDown       = 0.0
	idleStateConnecting = 1.0
	idleStateIdling     = 2.0
)

// idleTriggerPass is the sentinel "pass number" stamped on an IDLE-triggered
// sync's logs, distinguishing it from the scheduler's monotonic pass counter.
const idleTriggerPass = -1

// errIdleUnsupported signals that the server does not advertise IDLE. It is not
// a failure to retry: the watcher exits and the account stays poll-only.
var errIdleUnsupported = errors.New("syncrunner: server does not support IMAP IDLE")

// errIdleAwaitingReconnect: the OAuth account needs its user to sign in again
// (#52). The session ends before any dial; the watcher backs off and looks
// again, quietly, until reconcileIdle stops it or the user reconnects.
var errIdleAwaitingReconnect = errors.New("syncrunner: the account must be reconnected; not dialling")

// RunIdle maintains one IMAP IDLE watcher per IDLE-capable account of userID so
// new mail triggers a sync within seconds instead of waiting for the poll tick
// (mailroom#103 Stage 2). It is a per-user UserTask, a sibling of Run: the two
// run concurrently and share the Runner's per-account sync gate, so an
// IDLE-triggered pass and a scheduled pass never overlap.
//
// It returns immediately when IDLE is disabled (poll-only). Otherwise it blocks
// until ctx is cancelled, whereupon it cancels every watcher and waits for them
// to unwind — a clean drain barrier mirroring the supervisor's.
//
// IDLE is PURELY ADDITIVE and never a source of truth: a watcher only SELECTs
// to receive lossy EXISTS hints and triggers a full CONDSTORE SyncAccount pass
// (which advances cursors only on success). While any watcher is down —
// unsupported, backing off, or redialing — the scheduler keeps polling that
// account, so IDLE being unavailable costs latency, never mail.
func (r *Runner) RunIdle(ctx context.Context, userID ulid.ULID) error {
	if userID == (ulid.ULID{}) {
		return errors.New("syncrunner: run idle: user id is required")
	}
	if !r.idleEnabled {
		return nil // IDLE disabled: sync stays poll-only, this task is a no-op.
	}

	r.log.Info("idle watcher loop starting",
		"reissue", r.idleReissue.String(), "user_id", userID.String())

	// active maps accountID -> the cancel for that account's watcher context.
	// Touched only from this goroutine (reconcileIdle), so it needs no lock; wg
	// is the only cross-goroutine coordination, exactly like the supervisor.
	active := make(map[ulid.ULID]context.CancelFunc)
	var wg stdsync.WaitGroup
	defer func() {
		for _, cancel := range active {
			cancel()
		}
		wg.Wait()
		r.log.Info("idle watcher loop stopped", "user_id", userID.String())
	}()

	r.reconcileIdle(ctx, userID, active, &wg)

	ticker := time.NewTicker(idleAccountRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.reconcileIdle(ctx, userID, active, &wg)
		}
	}
}

// reconcileIdle brings the running watcher set in line with the user's current
// syncable accounts: it starts a watcher for each newly-seen account and
// cancels the watcher of an account that vanished. A listing error keeps the
// current set — a transient database blip must not tear down live watchers.
func (r *Runner) reconcileIdle(ctx context.Context, userID ulid.ULID, active map[ulid.ULID]context.CancelFunc, wg *stdsync.WaitGroup) {
	accts, err := r.lister.ListSyncableAccounts(ctx, userID)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("idle: listing accounts failed; keeping current watchers",
				"user_id", userID.String(), "err", err)
		}
		return
	}

	seen := make(map[ulid.ULID]struct{}, len(accts))
	for i := range accts {
		acct := accts[i]
		seen[acct.ID] = struct{}{}
		if _, running := active[acct.ID]; running {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		active[acct.ID] = cancel
		r.launchIdleWatcher(wctx, userID, acct, wg)
	}

	for id, cancel := range active {
		if _, ok := seen[id]; !ok {
			r.log.Info("idle: account no longer syncable; stopping its watcher",
				"user_id", userID.String(), "account_id", id.String())
			cancel()
			delete(active, id)
			r.idleState(id, idleStateDown)
		}
	}
}

// launchIdleWatcher runs one account's watcher in its own goroutine, recovering
// a panic so a single account can never crash the process — the account simply
// falls back to poll-only.
func (r *Runner) launchIdleWatcher(ctx context.Context, userID ulid.ULID, acct store.Account, wg *stdsync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if p := recover(); p != nil {
				r.log.Error("idle: watcher panicked; account stays poll-only",
					"user_id", userID.String(), "account_id", acct.ID.String(), "panic", p)
				r.idleState(acct.ID, idleStateDown)
			}
		}()
		r.idleWatcher(ctx, userID, acct)
	}()
}

// idleWatcher is one account's watcher: it holds an IDLE session for as long as
// it can and, on any error, backs off and redials — forever, until ctx is
// cancelled. The scheduler polls this account throughout, so every gap here is
// a latency cost, never a mail-loss one.
func (r *Runner) idleWatcher(ctx context.Context, userID ulid.ULID, acct store.Account) {
	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return
		}

		err := r.idleSession(ctx, userID, acct)

		if ctx.Err() != nil {
			// Cancellation (shutdown or the account vanishing): exit quietly —
			// a forced connection close is expected here, not an error.
			r.idleState(acct.ID, idleStateDown)
			return
		}
		if errors.Is(err, errIdleUnsupported) {
			// Server has no IDLE: idleSession already logged it. Exit the
			// watcher; the account remains poll-only (the correct fallback).
			return
		}

		r.idleState(acct.ID, idleStateDown)
		if err != nil && !errors.Is(err, errIdleAwaitingReconnect) {
			r.log.Warn("idle: session ended; redialing after backoff (scheduler keeps polling meanwhile)",
				"user_id", userID.String(), "account_id", acct.ID.String(), "name", acct.Name, "err", err)
		}
		backoff = nextIdleBackoff(backoff, r.maxBackoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
	}
}

// nextIdleBackoff doubles cur, clamped to [idleReconnectBackoffBase, ceiling].
func nextIdleBackoff(cur, ceiling time.Duration) time.Duration {
	next := cur * 2
	if next < idleReconnectBackoffBase {
		next = idleReconnectBackoffBase
	}
	if next > ceiling {
		next = ceiling
	}
	return next
}

// idleSession runs one dial-to-drop IDLE session lifetime: dial a SEPARATE
// connection (never the sync connection — a session in IDLE can service no
// other command), SELECT INBOX, then loop issuing IDLE and reacting to what the
// server pushes. It returns nil only on a clean internal transition (it never
// returns nil in practice — the inner loop only exits via error or ctx); any
// error is the caller's cue to back off and redial. errIdleUnsupported is the
// one terminal outcome.
func (r *Runner) idleSession(ctx context.Context, userID ulid.ULID, acct store.Account) error {
	r.idleState(acct.ID, idleStateConnecting)

	// THE ACCOUNT AS IT IS NOW, not as it was when the watcher started (#52).
	// `acct` was captured at launch; an OAuth grant that has since gone
	// needs_reauth (or been disconnected) would otherwise be dialled on every
	// redial until reconcileIdle next drops it. One query per session.
	if ar, ok := r.lister.(AccountReader); ok {
		fresh, err := ar.GetAccount(ctx, userID, acct.ID)
		if err != nil {
			return err
		}
		if fresh.AwaitingReconnect() {
			return errIdleAwaitingReconnect
		}
		acct = fresh
	}

	req, err := r.prepare(ctx, userID, acct)
	if err != nil {
		return err
	}
	// The watcher NEVER ingests mail — it only listens for EXISTS hints and
	// triggers a full SyncAccount pass (the source of truth). It has no use for
	// the decrypted mail-blob DEK prepare derived, so scrub it right away.
	crypto.Zero(req.DEK)

	// A GMAIL ACCOUNT IS POLL-ONLY (#52, ADR-047 §1). The watcher SELECTs
	// INBOX, and on Gmail INBOX is a label folder: the Gmail sync never
	// selects one, and neither does anything that holds a connection for it.
	// Its poll floor is the gmail profile's five minutes. (IDLE on All Mail
	// is the later, measured improvement.)
	if req.Gmail != nil {
		r.log.Info("idle: Gmail accounts are poll-only; the watcher exits",
			"user_id", userID.String(), "account_id", acct.ID.String(), "name", acct.Name)
		r.idleState(acct.ID, idleStateDown)
		return errIdleUnsupported
	}

	sess, err := r.idleDial(ctx, req.Config)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	if !sess.Supports(sync.CapabilityIdle) {
		r.log.Info("idle: server does not advertise IDLE; account stays poll-only",
			"user_id", userID.String(), "account_id", acct.ID.String(), "name", acct.Name)
		r.idleState(acct.ID, idleStateDown)
		return errIdleUnsupported
	}

	if _, err := sess.Select(ctx, idleSelectMailbox, sync.SelectOptions{ReadOnly: true}); err != nil {
		return err
	}

	notifs := sess.Notifications()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.idleState(acct.ID, idleStateIdling)

		idle, err := sess.Idle(ctx)
		if err != nil {
			return err
		}

		trigger, err := r.awaitIdleEvent(ctx, sess, notifs, idle)
		if err != nil {
			return err
		}
		if trigger {
			// A hint that the mailbox grew: run a full pass through the gate.
			// The gate coalesces this with any scheduled/other triggered pass.
			r.TriggerAccountSync(ctx, userID, acct.ID)
		}
		// Loop and re-enter IDLE (after an EXISTS trigger, or the reissue timer).
	}
}

// awaitIdleEvent blocks on a running IDLE until one of three things happens,
// then ends the IDLE (so the connection can issue its next command) and reports
// whether a sync should be triggered:
//
//   - an EXISTS hint (the mailbox's message count changed) => trigger a sync;
//   - the reissue timer (RFC 2177) => no trigger, just refresh IDLE;
//   - ctx cancellation => stop IDLE, force the connection closed so Stop/Wait
//     cannot hang on a dead server, and return ctx.Err().
//
// Non-EXISTS hints (flag/expunge changes) are ignored WITHOUT leaving IDLE —
// they are not new mail, and the periodic reap/poll reconciles them.
func (r *Runner) awaitIdleEvent(ctx context.Context, sess sync.SyncSession, notifs <-chan sync.Notification, idle sync.IdleSession) (trigger bool, err error) {
	reissue := time.NewTimer(r.idleReissue)
	defer reissue.Stop()

	for {
		select {
		case n := <-notifs:
			if n.Kind != sync.NotificationExists {
				continue // flag/expunge hint: keep idling
			}
			trigger = true
		case <-reissue.C:
			trigger = false
		case <-ctx.Done():
			// Teardown: end IDLE and force the socket shut so a wedged server
			// cannot make Wait block the drain. Double Close (with the deferred
			// sess.Close in idleSession) is harmless.
			_ = idle.Stop()
			_ = sess.Close()
			_ = idle.Wait()
			return false, ctx.Err()
		}

		// End this IDLE so the next command (a fresh IDLE, or the trigger's own
		// separate connection) is unblocked. A Stop/Wait error here means the
		// connection broke: surface it so the watcher redials.
		if serr := stopIdle(idle); serr != nil {
			return false, serr
		}
		return trigger, nil
	}
}

// stopIdle ends an IDLE and waits for the server to acknowledge, returning the
// first error. Wait is always called (even if Stop errored) so the IDLE
// command's goroutine is not left dangling.
func stopIdle(idle sync.IdleSession) error {
	stopErr := idle.Stop()
	waitErr := idle.Wait()
	if stopErr != nil {
		return stopErr
	}
	return waitErr
}

// TriggerAccountSync runs one gated SyncAccount pass for a single account in
// response to an IDLE new-mail hint (mailroom#103 Stage 2, Deliverable D). It
// is safe to call from an IDLE watcher goroutine while Run's scheduled loop is
// active — that is the entire purpose of the per-account gate.
//
// It reuses the EXACT pass the scheduler uses (r.syncAccount + advanceSchedule),
// so it inherits the engine's "advance cursors only on success" safety and
// keeps the poll cadence consistent (a triggered success resets backoff and
// pushes nextDue just like a scheduled one). It NEVER fetches or ingests mail
// itself — the EXISTS hint is lossy; SyncAccount's CONDSTORE reconcile is the
// source of truth.
//
// A trigger for an unknown or vanished account is a no-op (the account listing
// is the authority on what still exists). Rapid repeat triggers are debounced
// per account (Deliverable B); a debounced hint is not lost — the scheduled
// poll remains the safety net.
//
// ctx bounds the triggered pass so it cancels on shutdown; it comes from the
// watcher's (ultimately the user's) context.
func (r *Runner) TriggerAccountSync(ctx context.Context, userID, accountID ulid.ULID) {
	if !r.idleEnabled {
		return
	}

	accts, err := r.lister.ListSyncableAccounts(ctx, userID)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("idle trigger: listing accounts failed; sync deferred to the next poll",
				"user_id", userID.String(), "err", err)
		}
		return
	}
	var (
		acct  store.Account
		found bool
	)
	for i := range accts {
		if accts[i].ID == accountID {
			acct = accts[i]
			found = true
			break
		}
	}
	if !found {
		return // vanished (disabled/deleted): no-op
	}

	g := r.gateFor(userID, accountID)
	now := r.now()
	g.mu.Lock()
	// Debounce ONLY when no pass is in flight. If a pass is already running,
	// let the trigger through: the gate turns it into the one mandatory
	// follow-up pass (an EXISTS during a running pass may point at mail that
	// pass began too early to see), which must not be debounced away.
	if !g.running && now.Sub(g.lastIdleTrigger) < r.idleDebounce(acct) {
		g.mu.Unlock()
		return
	}
	g.lastIdleTrigger = now
	g.mu.Unlock()

	r.log.Info("idle: new-mail hint; triggering sync",
		"user_id", userID.String(), "account_id", acct.ID.String(), "name", acct.Name)

	r.runGated(userID, accountID, func() {
		var (
			sum PassSummary
			mu  stdsync.Mutex
		)
		syncErr := r.syncAccount(ctx, userID, acct, idleTriggerPass, &sum, &mu)
		if ctx.Err() != nil {
			// Cancelled mid-pass: leave the schedule untouched so a restart does
			// not inherit a spurious backoff (mirrors the scheduled path).
			return
		}
		r.advanceSchedule(userID, acct, r.now(), syncErr != nil)
	})
}

// idleDebounce is the minimum spacing between IDLE-triggered syncs for acct:
// max(DefaultIdleTriggerDebounce, providerFloor). Combining with the provider's
// poll floor is what actually protects a throttled provider — a fixed
// few-second const would let a Gmail account (5-minute floor) be resynced far
// too aggressively over IDLE, since each triggered pass opens a fresh
// connection exactly like a poll. It is deliberately SEPARATE from the poll
// effectiveInterval (which the fallback poll keeps using): for a floor-free
// provider the debounce is just the small const, so IDLE stays near-instant.
func (r *Runner) idleDebounce(acct store.Account) time.Duration {
	d := DefaultIdleTriggerDebounce
	if floor := provider.IMAPMinSyncInterval(acct.ProviderProfile); floor > d {
		d = floor
	}
	return d
}
