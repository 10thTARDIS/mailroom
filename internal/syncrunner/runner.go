// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	stdsync "sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/mailsync"
	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// DefaultInterval is the per-account BASE re-sync period when none is configured
// (mailroom#103 Stage 1). Each account is scheduled independently around this
// value: a healthy account re-syncs every effective interval (the base raised to
// the account's provider floor), while a failing account backs off on its own
// timeline without holding up any other account.
const DefaultInterval = 5 * time.Minute

// DefaultMaxBackoff caps the exponential backoff a repeatedly-failing account
// reaches (mailroom#103). Backoff doubles from the account's effective interval
// on each consecutive failure and is clamped here, so a persistently-broken
// account is retried at most this often — often enough to recover promptly when
// the provider comes back, rarely enough not to hammer it meanwhile.
const DefaultMaxBackoff = 30 * time.Minute

// schedulerTickCap bounds how coarse the scheduler's wakeup granularity can get.
// The Run loop wakes every min(baseInterval, schedulerTickCap) and syncs only the
// accounts that are due, so this is the worst-case latency between an account
// becoming due (its interval elapsing, or its backoff expiring) and it actually
// being picked up. Fine enough that per-account intervals and backoffs resolve
// meaningfully; coarse enough that the idle relist stays cheap.
const schedulerTickCap = 30 * time.Second

// backoffJitterFraction is the +/- proportion of random jitter applied to a
// failing account's next-due time (not to the stored backoff magnitude, which
// stays a clean power-of-two multiple for predictability). It spreads the retries
// of many accounts that failed together so they do not reconnect in lockstep.
const backoffJitterFraction = 0.10

// DefaultPoolIdleTimeout is how long a warm pooled sync connection may sit
// unused before the pool evicts it rather than reuse it (mailroom#103 Stage 3).
// Past this, a NOOP probe would likely fail on a server that has already dropped
// an idle connection, so the pool re-dials proactively instead of paying a
// doomed probe round-trip first. It is deliberately a few multiples of the base
// sync interval: long enough that back-to-back passes (and IDLE-triggered ones)
// reuse the connection, short enough that the pool never holds a soon-stale one.
const DefaultPoolIdleTimeout = 5 * time.Minute

// DefaultConcurrency caps how many of a user's accounts sync in parallel within
// one pass. It is a small bound on purpose: each account holds its own IMAP
// session(s), and providers limit concurrent connections (spec §4), so the win
// is overlapping many accounts' network waits, not saturating any one provider.
const DefaultConcurrency = 4

// AccountLister lists the accounts a pass should sync — and the ones it must
// NOT sync but still has to name (mailroom#435). Satisfied by StoreLister over
// a *store.Store.
type AccountLister interface {
	ListSyncableAccounts(ctx context.Context, userID ulid.ULID) ([]store.Account, error)
	// ListDisabledAccounts returns the user's sync-disabled accounts, so a pass
	// can say WHICH accounts it is leaving alone rather than listing zero and
	// looking idle.
	ListDisabledAccounts(ctx context.Context, userID ulid.ULID) ([]store.Account, error)
}

// AccountReader is an optional AccountLister extension: one account as it is
// now. The IDLE watcher re-reads its account before every dial with it, so a
// grant that went needs_reauth since the watcher started is not dialled again
// (#52). StoreLister implements it.
type AccountReader interface {
	GetAccount(ctx context.Context, userID, accountID ulid.ULID) (store.Account, error)
}

// ReconnectLister is an optional AccountLister extension: the sync-enabled
// accounts ListSyncableAccounts leaves out because their OAuth grant needs the
// user to reconnect (#52). A lister that has it gets them named once per pass,
// for the same reason disabled accounts are (mailroom#435): an account the
// runner silently stopped dialling reads exactly like an idle one.
type ReconnectLister interface {
	ListAwaitingReconnect(ctx context.Context, userID ulid.ULID) ([]store.Account, error)
}

// Syncer runs one account's sync pass. It is satisfied directly by
// *mailsync.Engine, whose SyncAccount/SyncAccountWith have these exact
// signatures.
//
//   - SyncAccount dials its own connection, runs the pass, and closes it. It is
//     the path the pool-disabled runner and mailroom-admin sync-once take.
//   - SyncAccountWith runs the same pass over a CALLER-SUPPLIED session it never
//     closes — the path the per-account sync pool takes (mailroom#103 Stage 3),
//     so a warm connection is reused across passes.
type Syncer interface {
	SyncAccount(ctx context.Context, req mailsync.AccountSync) (mailsync.Summary, error)
	SyncAccountWith(ctx context.Context, req mailsync.AccountSync, sess sync.SyncSession) (mailsync.Summary, error)
}

// Prepare converts a stored account into a ready-to-sync request: it opens the
// sealed credentials and resolves the mail-blob DEK. Production is
// CredentialResolver.Prepare; tests inject a fake so the loop needs no crypto
// or database.
type Prepare func(ctx context.Context, userID ulid.ULID, acct store.Account) (mailsync.AccountSync, error)

// ProgressEvent reports the outcome of syncing (or failing to sync) one
// account within a pass. It is the injectable hook for metrics/UX.
type ProgressEvent struct {
	Pass        int
	UserID      ulid.ULID
	AccountID   ulid.ULID
	AccountName string
	Summary     mailsync.Summary
	Err         error // non-nil if the account failed (prepare or sync)
}

// Progress is an optional per-account progress callback.
type Progress func(ev ProgressEvent)

// Writeback drains an account's pending writeback intents (spec §4a) before its
// sync pass. It is a seam rather than a hard dependency so the runner still
// unit-tests with no IMAP and no writeback engine.
//
// It runs BEFORE the pass, in the SAME goroutine, on purpose:
//
//   - before, because local deltas should reach the server before we reconcile
//     against it — otherwise a reconcile that has not yet seen our change can
//     look like a remote divergence;
//   - same goroutine, because writeback dials its own IMAP session, and running
//     it concurrently with the sync pass would double an account's concurrent
//     IMAP connections. Provider caps are a real constraint (spec §4).
//
// A writeback failure is NOT fatal to the pass: the intents stay queued and the
// next pass retries them, while sync still runs. It is recorded on the pass
// summary so the failure is visible.
type Writeback func(ctx context.Context, userID ulid.ULID, acct store.Account, cfg sync.Config) error

// HealthRecorder persists per-account sync-health telemetry after each pass over
// an account (spec §12, mailroom#43) — the operational signal the account-health
// panel reads on demand. It is an OPTIONAL seam so the runner still unit-tests
// with no store; production is StoreHealthRecorder over a *store.Store.
//
// Its writes are strictly FAIL-SOFT (see recordHealth): a telemetry write must
// never fail a sync pass or mask the real per-account outcome, so the runner
// logs and continues on any error. Each call opens its own UserTx, so it is safe
// to invoke from the per-account goroutines that run concurrently within a pass.
type HealthRecorder interface {
	RecordSyncSuccess(ctx context.Context, userID, accountID ulid.ULID, at time.Time) error
	RecordSyncFailure(ctx context.Context, userID, accountID ulid.ULID, errMsg string, at time.Time) error
}

// PushNotifier delivers a "new mail" Web Push to the user's registered devices
// after a sync pass ingests new messages (spec §12, mailroom#42, ADR-016). Like
// HealthRecorder it is an OPTIONAL seam (the loop unit-tests with no push and no
// store) and it is fired PER-USER from syncAccount, so its store implementation
// opens a UserTx under exactly that user's RLS scope — no bypass.
//
// It is strictly FAIL-SOFT (see notifyPush): a push failure must never fail a
// sync pass or mask its outcome. Production is StorePushNotifier (adapter.go).
//
// newMessages is the pass's notifiable count (pushCount); ids are those same
// messages, one each (mailsync.Summary.NotifyIDs), for the triage-aware modes
// that need to know WHICH mail arrived, not only how much (#59).
type PushNotifier interface {
	NotifyNewMail(ctx context.Context, userID ulid.ULID, acct store.Account, newMessages int, ids []ulid.ULID) error
}

// AccountError is a non-fatal failure syncing one account within a pass.
type AccountError struct {
	AccountID ulid.ULID
	Name      string
	Err       error
}

func (e AccountError) Error() string {
	return fmt.Sprintf("account %s (%s): %v", e.AccountID, e.Name, e.Err)
}
func (e AccountError) Unwrap() error { return e.Err }

// PassSummary aggregates one sync pass over all of a user's accounts.
type PassSummary struct {
	Accounts int // accounts considered
	Synced   int // accounts that synced without a fatal error
	Failed   int // accounts that failed (prepare or sync)
	// Disabled counts the user's accounts whose sync is switched off — listed,
	// named at WARN, and NOT synced (mailroom#435). It is what separates a pass
	// with nothing to do from a pass with nothing it is ALLOWED to do; before it
	// existed both logged accounts=0 at INFO.
	Disabled         int
	MailboxesSynced  int
	MessagesIngested int
	// MessagesMuted is the part of MessagesIngested that arrived in a muted
	// conversation and went to Archive without notifying (mailsync.Summary).
	MessagesMuted   int
	FlagUpdates     int
	WritebackFailed int // accounts whose writeback drain errored (sync still ran)

	// BodiesFetched and BodyBytesFetched aggregate mailsync.Summary's counters
	// of the same name across every account this pass synced (#523): how many
	// message bodies the pass actually downloaded, and their total size. See
	// mailsync.Summary for why FlagUpdates alone cannot answer that.
	BodiesFetched    int
	BodyBytesFetched int64

	// The expunge reaper (mailsync, #86) and the UIDVALIDITY roll reconciler
	// (mailsync, #88). These are the parts of sync that can DESTROY state —
	// provenance rows, and (soft) whole messages — so what they did is tallied and
	// logged rather than left silent: an operator must be able to see, without a
	// debugger, that mail is leaving their folders because the SERVER dropped it
	// and not because Mailroom decided to. MessagesVanished counts both paths: a
	// message left in no location at all is soft-deleted by the same code whether
	// its UID was expunged or its whole mailbox generation was thrown away.
	MailboxesReaped       int
	ProvenanceReaped      int
	MessagesVanished      int
	PlacementsRetired     int
	GenerationsReconciled int // mailboxes whose UIDVALIDITY rolled and was reconciled
	ProvenanceRetired     int // retired-generation provenance rows dropped

	// MailboxesDeferred aggregates mailsync.Summary's counter of the same name
	// (#638): due mailboxes this pass never observed because the server refused
	// a UID SEARCH for rate limiting. They are NOT failures — they are not in
	// Errors, nothing was concluded about them, and they are due next pass — so
	// they are counted separately from mailbox_errors rather than folded into
	// it. A number that stays high pass after pass means the configured search
	// rate is still above what the provider will actually answer.
	MailboxesDeferred int
	// MailboxesObserved aggregates mailsync.Summary's counter of the same name:
	// mailboxes whose live UID set the server answered, one UID SEARCH each. It
	// is the figure to compare against MAILROOM_IMAP_SEARCH_RATE.
	MailboxesObserved int
	// MailboxesUnchanged and ObservationsSkipped aggregate mailsync.Summary's
	// counters of the same names (#620): folders not SELECTed because
	// LIST-STATUS proved the poll had nothing to do (a subset of
	// MailboxesSynced), and due observations not issued because LIST-STATUS
	// matched what the last real observation saw. A skipped observation is not
	// in MailboxesObserved.
	MailboxesUnchanged  int
	ObservationsSkipped int

	Errors []AccountError
}

// schedule is one account's independent sync-scheduling state (mailroom#103).
// nextDue is the earliest wall-clock time the account should next be synced;
// backoff is its current exponential-backoff magnitude (0 while healthy). Both
// are advanced after each of the account's own passes and read under the
// Runner's scheduleMu — never shared between accounts, which is what keeps one
// account's backoff from delaying any other.
type schedule struct {
	nextDue time.Time
	backoff time.Duration
}

// Runner drives sync for one operator user.
//
// A single Runner serves every user (cmd/mailroom wires one and calls Run per
// user id), so the per-account schedule state is keyed by BOTH user and account.
// Account ids are globally-unique ULIDs, but keying by user as well keeps each
// user's pass pruning its OWN account set and never another user's.
type Runner struct {
	lister      AccountLister
	syncer      Syncer
	prepare     Prepare
	writeback   Writeback
	interval    time.Duration
	maxBackoff  time.Duration
	concurrency int
	log         *slog.Logger
	progress    Progress
	health      HealthRecorder
	push        PushNotifier

	// disabledSink receives, once per pass, how many of the user's accounts
	// have sync switched off (mailroom#435). Production points it at the
	// mailroom_sync_disabled_accounts gauge; nil means no metric, and the
	// count is still logged and carried in PassSummary.
	disabledSink func(userID ulid.ULID, n int)

	// lagSink receives, at the START of each account's pass, how long it had
	// been since that account last synced successfully — i.e. how stale the
	// local copy had become. It backs mailroom_sync_lag_seconds (#524).
	//
	// ⚠ MEASURED AT PASS START, NOT AT SUCCESS, and that is the whole design.
	// At the moment a sync succeeds the lag is zero by definition, so a gauge
	// set there reports 0 forever and cannot distinguish a healthy account from
	// one that has not synced since Tuesday. Sampling on entry reports the gap
	// that just elapsed, which for a healthy account is the poll interval and
	// for a stalled one climbs without bound — the shape an alert needs.
	//
	// Nothing is emitted before the first success after boot: the true lag is
	// then unknown, and reporting 0 would assert freshness we have not observed.
	lagSink      func(accountID ulid.ULID, seconds float64)
	lastSyncedMu stdsync.Mutex
	lastSyncedAt map[ulid.ULID]time.Time

	// now is the clock the scheduler reads; it defaults to time.Now and is a
	// seam so tests can advance time deterministically without real sleeps.
	now func() time.Time

	// scheduleMu guards schedules, gates AND pools (same lock, same lifecycle).
	// schedules maps userID -> accountID -> scheduling state; gates maps
	// userID -> accountID -> the per-account serialize+coalesce sync gate
	// (mailroom#103 Stage 2, Deliverable A); pools maps userID -> accountID ->
	// the account's warm, reusable sync connection (mailroom#103 Stage 3). All
	// three are reconciled against the live account set each scheduled pass and
	// dropped when the user's loop exits.
	//
	// A pooled session is CHECKED OUT of the map (deleted) for the duration of a
	// pass and returned on success, so the map only ever holds idle connections:
	// that is what lets the reconcile/teardown below close a pooled session
	// without ever touching one a pass is mid-flight on, with no lock beyond the
	// scheduleMu-guarded map access (the gate already serializes passes per
	// account, so at most one goroutine ever checks out a given account's entry).
	scheduleMu stdsync.Mutex
	schedules  map[ulid.ULID]map[ulid.ULID]*schedule
	gates      map[ulid.ULID]map[ulid.ULID]*syncGate
	pools      map[ulid.ULID]map[ulid.ULID]*pooledSession

	// --- IMAP sync connection pool (mailroom#103 Stage 3) ----------------
	// poolEnabled gates the whole feature: off => every pass dials a fresh
	// connection via Syncer.SyncAccount (dial+close), exactly as before Stage 3.
	// poolIdleTimeout evicts a warm connection unused longer than it. poolDial
	// opens a fresh sync connection (a seam so the pool unit-tests with no
	// network); production is sync.Dial.
	poolEnabled     bool
	poolIdleTimeout time.Duration
	poolDial        func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error)

	// --- IMAP IDLE (mailroom#103 Stage 2) --------------------------------
	// idleEnabled gates RunIdle entirely (off => poll-only, RunIdle returns
	// immediately). idleReissue is the RFC 2177 re-IDLE period. idleDial opens
	// a watcher's own IMAP session (a seam so the watcher unit-tests with no
	// network). idleState reports the per-account IDLE connection state to
	// metrics (0 down / 1 connecting / 2 idling).
	idleEnabled bool
	idleReissue time.Duration
	idleDial    func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error)
	idleState   func(accountID ulid.ULID, state float64)
}

// Option configures a Runner.
type Option func(*Runner)

// WithInterval sets the per-account BASE re-sync period (default
// DefaultInterval). It is a floor-able base: an account whose provider profile
// declares a larger minimum interval re-syncs at that larger cadence instead.
func WithInterval(d time.Duration) Option {
	return func(r *Runner) { r.interval = d }
}

// WithMaxBackoff sets the ceiling on a failing account's exponential backoff
// (default DefaultMaxBackoff). A value <= 0 restores the default.
func WithMaxBackoff(d time.Duration) Option {
	return func(r *Runner) { r.maxBackoff = d }
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(r *Runner) {
		if l != nil {
			r.log = l
		}
	}
}

// WithProgress sets the per-account progress callback.
func WithProgress(p Progress) Option {
	return func(r *Runner) { r.progress = p }
}

// WithConcurrency sets how many accounts sync in parallel within a pass
// (default DefaultConcurrency). A value <= 0 restores the default.
func WithConcurrency(n int) Option {
	return func(r *Runner) { r.concurrency = n }
}

// WithWriteback installs the pre-sync writeback drain (see Writeback). Without
// it, local Email/set changes are recorded canonically but never reach IMAP.
func WithWriteback(w Writeback) Option {
	return func(r *Runner) { r.writeback = w }
}

// WithHealthRecorder installs the per-account sync-health telemetry sink (see
// HealthRecorder). Without it the loop runs identically but records no health,
// so Account/getHealth reports every account as never-synced.
func WithHealthRecorder(h HealthRecorder) Option {
	return func(r *Runner) { r.health = h }
}

// WithDisabledAccountsSink installs the per-user sync-disabled-account count
// reporter (mailroom#435). Production points it at the
// mailroom_sync_disabled_accounts gauge so a disabled account can be alerted
// on; without it the count is still logged at WARN and carried in PassSummary.
func WithDisabledAccountsSink(f func(userID ulid.ULID, n int)) Option {
	return func(r *Runner) { r.disabledSink = f }
}

// WithSyncLagSink installs the per-account sync-lag reporter (see lagSink).
// Without it the loop runs identically and simply reports no lag.
func WithSyncLagSink(f func(accountID ulid.ULID, seconds float64)) Option {
	return func(r *Runner) { r.lagSink = f }
}

// noteSyncSuccess records when an account last synced cleanly, for the lag
// sampled at the start of its next pass.
func (r *Runner) noteSyncSuccess(accountID ulid.ULID, at time.Time) {
	r.lastSyncedMu.Lock()
	defer r.lastSyncedMu.Unlock()
	if r.lastSyncedAt == nil {
		r.lastSyncedAt = make(map[ulid.ULID]time.Time)
	}
	r.lastSyncedAt[accountID] = at
}

// reportSyncLag emits how stale the account's local copy had become, if we have
// seen it succeed at least once in this process. Returning silently on the
// first pass is deliberate: see lagSink.
func (r *Runner) reportSyncLag(accountID ulid.ULID, now time.Time) {
	if r.lagSink == nil {
		return
	}
	r.lastSyncedMu.Lock()
	prev, ok := r.lastSyncedAt[accountID]
	r.lastSyncedMu.Unlock()
	if !ok {
		return
	}
	r.lagSink(accountID, now.Sub(prev).Seconds())
}

// WithPushNotifier installs the new-mail Web Push notifier (see PushNotifier).
// Without it the loop runs identically and simply sends no pushes.
func WithPushNotifier(p PushNotifier) Option {
	return func(r *Runner) { r.push = p }
}

// WithIdle enables the IMAP IDLE watcher (mailroom#103 Stage 2) and sets the
// RFC 2177 re-IDLE period. Disabled (the zero default) makes RunIdle a no-op:
// sync stays poll-only. A non-positive reissue keeps DefaultIdleReissue. IDLE
// is purely additive — it only triggers the SAME sync pass sooner — so leaving
// it off never loses mail, it only loses the seconds-latency speedup.
func WithIdle(enabled bool, reissue time.Duration) Option {
	return func(r *Runner) {
		r.idleEnabled = enabled
		if reissue > 0 {
			r.idleReissue = reissue
		}
	}
}

// WithPool enables the per-account warm sync connection pool (mailroom#103
// Stage 3) and sets its idle-eviction timeout. Disabled (the zero default) makes
// every pass dial a fresh connection and close it, exactly as before Stage 3.
// A non-positive timeout keeps DefaultPoolIdleTimeout. Pooling is safe to leave
// on: a server that resets idle connections merely makes each NOOP probe fail,
// so the pool re-dials — it never skips or corrupts a sync.
func WithPool(enabled bool, idleTimeout time.Duration) Option {
	return func(r *Runner) {
		r.poolEnabled = enabled
		if idleTimeout > 0 {
			r.poolIdleTimeout = idleTimeout
		}
	}
}

// WithPoolDialer overrides how the sync pool opens a fresh connection (default
// sync.Dial). It is the seam that lets the pool unit-test against fake sessions
// with no network, and it is where a dial-counting factory is injected to prove
// reuse.
func WithPoolDialer(d func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error)) Option {
	return func(r *Runner) {
		if d != nil {
			r.poolDial = d
		}
	}
}

// WithIdleDialer overrides how an IDLE watcher opens its own IMAP session
// (default sync.Dial). It is the seam that lets the watcher unit-test against a
// fake SyncSession with no network.
func WithIdleDialer(d func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error)) Option {
	return func(r *Runner) {
		if d != nil {
			r.idleDial = d
		}
	}
}

// WithIdleStateSink installs the per-account IDLE-connection-state reporter
// (metrics.IdleConnState: 0 down / 1 connecting / 2 idling). Without it the
// watcher runs identically and simply reports no state.
func WithIdleStateSink(f func(accountID ulid.ULID, state float64)) Option {
	return func(r *Runner) {
		if f != nil {
			r.idleState = f
		}
	}
}

// New builds a Runner. lister, syncer, and prepare are required.
func New(lister AccountLister, syncer Syncer, prepare Prepare, opts ...Option) *Runner {
	r := &Runner{
		lister:          lister,
		syncer:          syncer,
		prepare:         prepare,
		interval:        DefaultInterval,
		maxBackoff:      DefaultMaxBackoff,
		concurrency:     DefaultConcurrency,
		log:             slog.Default(),
		now:             time.Now,
		schedules:       make(map[ulid.ULID]map[ulid.ULID]*schedule),
		gates:           make(map[ulid.ULID]map[ulid.ULID]*syncGate),
		pools:           make(map[ulid.ULID]map[ulid.ULID]*pooledSession),
		poolIdleTimeout: DefaultPoolIdleTimeout,
		poolDial:        sync.Dial,
		idleReissue:     DefaultIdleReissue,
		idleDial:        sync.Dial,
		idleState:       func(ulid.ULID, float64) {},
	}
	for _, o := range opts {
		o(r)
	}
	if r.interval <= 0 {
		r.interval = DefaultInterval
	}
	if r.maxBackoff <= 0 {
		r.maxBackoff = DefaultMaxBackoff
	}
	if r.concurrency <= 0 {
		r.concurrency = DefaultConcurrency
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.poolIdleTimeout <= 0 {
		r.poolIdleTimeout = DefaultPoolIdleTimeout
	}
	if r.poolDial == nil {
		r.poolDial = sync.Dial
	}
	if r.idleReissue <= 0 {
		r.idleReissue = DefaultIdleReissue
	}
	if r.idleDial == nil {
		r.idleDial = sync.Dial
	}
	if r.idleState == nil {
		r.idleState = func(ulid.ULID, float64) {}
	}
	return r
}

// Run drives continuous sync for userID until ctx is canceled (whereupon it
// returns ctx.Err()). It does NOT sync every account in lockstep: the loop wakes
// on a fine scheduler tick and each wakeup syncs only the accounts that are DUE —
// whose per-account interval has elapsed, or whose backoff has expired. The very
// first pass sees every account as newly-scheduled (due immediately), so it is a
// full catch-up exactly as before. A pass-level (list) failure is logged and the
// loop continues; it does not disturb any account's schedule.
func (r *Runner) Run(ctx context.Context, userID ulid.ULID) error {
	if userID == (ulid.ULID{}) {
		return errors.New("syncrunner: run: user id is required")
	}
	// A user's schedule state is torn down when its loop exits (user disabled,
	// removed, or shutdown), so a later re-run starts clean rather than acting on
	// a stale next-due computed before the gap.
	defer r.forgetUser(userID)

	tick := r.schedulerTick()
	r.log.Info("sync loop starting",
		"base_interval", r.interval.String(), "max_backoff", r.maxBackoff.String(),
		"scheduler_tick", tick.String(), "user_id", userID.String())

	pass := 0
	// Catch-up pass immediately on start: every account is newly-scheduled and
	// therefore due now.
	if _, err := r.runScheduledPass(ctx, userID, pass); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.log.Error("catch-up pass failed", "err", err)
	}
	pass++

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("sync loop stopping", "user_id", userID.String())
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.runScheduledPass(ctx, userID, pass); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.log.Error("sync pass failed", "pass", pass, "err", err)
			}
			pass++
		}
	}
}

// schedulerTick is how often Run wakes to look for due accounts: the base
// interval, but never coarser than schedulerTickCap so short intervals and
// backoff expiries still resolve promptly.
func (r *Runner) schedulerTick() time.Duration {
	tick := r.interval
	if tick > schedulerTickCap {
		tick = schedulerTickCap
	}
	if tick <= 0 {
		tick = schedulerTickCap
	}
	return tick
}

// NudgeWriteback drains this user's queued writeback intents NOW instead of at
// the next scheduled pass (mailroom#581).
//
// THE PROBLEM IT SOLVES. `Email/set` enqueues an intent and returns; nothing
// then carries it upward until the account's next pass, because writeback runs
// only as the pre-sync hook (syncAccount). Measured at a
// 5-minute interval: a message rescued from Junk reached the provider minutes
// after the click. IDLE does not help — it fires on SERVER events, and a local
// click produces none.
//
// ⚠ IT DRAINS, IT DOES NOT SYNC. A nudge is the cheap half on purpose: pushing
// one flag up must not also pull a full CONDSTORE reconcile over every mailbox,
// which at ~37k messages is the expensive half and is what the schedule is for.
// The next scheduled pass reconciles as it always did.
//
// SAFETY COMES FROM THE GATE, not from new machinery: each account's drain runs
// under runGated, which already SERIALIZES (never two passes at once for an
// account — the engine requires it, and a second concurrent IMAP session per
// account is exactly what spec §4's "stay conservative" forbids) and COALESCES
// (a burst of twenty labels becomes one drain, plus at most one guaranteed
// follow-up). So a nudge can never open a connection beside a running pass.
//
// It returns immediately: each account is drained on its own goroutine, because
// the caller is a JMAP handler answering a user.
//
// ⚠ ctx MUST OUTLIVE THE REQUEST. Pass the process context, not the handler's —
// a drain started on a request context dies when the response is written, which
// is precisely when it was about to do its work.
//
// An account whose writeback is suppressed (ingest-only, ADR-028) costs one
// cheap count query and no dial: DrainAccount checks `writeback_enabled` and the
// pending count before it considers connecting.
func (r *Runner) NudgeWriteback(ctx context.Context, userID ulid.ULID) {
	if r.writeback == nil {
		return // no writeback engine wired (tests, read-only deployments)
	}
	accts, err := r.lister.ListSyncableAccounts(ctx, userID)
	if err != nil {
		// Not worth failing the user's request over: the scheduled pass will
		// drain these intents on its own. Logged so a persistent failure is
		// visible rather than silently slow.
		r.log.Warn("writeback nudge: list accounts failed; intents wait for the next pass",
			"user_id", userID.String(), "err", err)
		return
	}
	for _, acct := range accts {
		go r.runGated(userID, acct.ID, func() { r.drainOnly(ctx, userID, acct) })
	}
}

// drainOnly runs the writeback hook for one account and nothing else. It is the
// body of a nudge; syncAccount does the same drain before its sync.
func (r *Runner) drainOnly(ctx context.Context, userID ulid.ULID, acct store.Account) {
	req, err := r.prepare(ctx, userID, acct)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("writeback nudge: prepare failed; intents wait for the next pass",
				"user_id", userID.String(), "account_id", acct.ID.String(), "err", err)
		}
		return
	}
	defer crypto.Zero(req.DEK) // scrub the decrypted mail DEK after use
	if err := r.writeback(ctx, userID, acct, req.Config); err != nil && ctx.Err() == nil {
		r.log.Error("writeback nudge: drain failed; intents stay queued for the next pass",
			"user_id", userID.String(), "account_id", acct.ID.String(), "err", err)
	}
}

// SyncOnce runs a single sync pass over the user's sync-enabled accounts and
// returns the aggregate. It is the manual/testing entry point. Per-account
// failures are collected in the summary, not returned as an error; only a
// failure listing accounts is returned.
func (r *Runner) SyncOnce(ctx context.Context, userID ulid.ULID) (PassSummary, error) {
	return r.runPass(ctx, userID, 0)
}

// runPass syncs EVERY listed account once, ignoring the per-account schedule. It
// backs SyncOnce (the manual/CLI one-shot), whose contract is "sync all accounts
// now", so it neither consults nor advances the schedule state.
func (r *Runner) runPass(ctx context.Context, userID ulid.ULID, pass int) (PassSummary, error) {
	accts, err := r.lister.ListSyncableAccounts(ctx, userID)
	if err != nil {
		return PassSummary{}, fmt.Errorf("syncrunner: list syncable accounts: %w", err)
	}
	disabled := r.reportDisabled(ctx, userID)
	sum := r.dispatch(ctx, userID, pass, accts, disabled, nil)
	return sum, ctx.Err()
}

// reportDisabled lists userID's sync-disabled accounts and makes them visible
// for as long as the condition persists (mailroom#435): one WARN per pass
// naming each account, and the count to the disabled-accounts sink. It returns
// the count for the pass summary.
//
// The WARN is deliberately repeated every pass rather than logged once. The
// failure mode this exists for is SILENCE — a disabled account produced one
// warning at boot and then `accounts=0` at INFO for the rest of the process's
// life, indistinguishable from an idle instance — so the signal has to be
// present in whatever window of the log an operator happens to read.
//
// A listing failure must not stop the pass, and a zero from a failed list is
// not a finding: it is logged as its own ERROR and the sink is left untouched
// rather than being told "0".
func (r *Runner) reportDisabled(ctx context.Context, userID ulid.ULID) int {
	dis, err := r.lister.ListDisabledAccounts(ctx, userID)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("could not list sync-disabled accounts; their count is unknown this pass",
				"user_id", userID.String(), "err", err)
		}
		return 0
	}
	if r.disabledSink != nil {
		r.disabledSink(userID, len(dis))
	}
	if len(dis) == 0 {
		return 0
	}
	names := make([]string, len(dis))
	for i, a := range dis {
		names[i] = a.Name + " (" + a.ID.String() + ")"
	}
	r.log.Warn("accounts with sync disabled: mail is not arriving for them",
		"user_id", userID.String(), "disabled", len(dis), "accounts", names,
		"recover", "mailroom-admin enable-account --user-id <user> --account-id <account>")
	return len(dis)
}

// reportAwaitingReconnect names, at WARN, the accounts this pass will not dial
// because their OAuth grant needs the user to reconnect. Their writeback
// intents and outgoing mail are held, not failed, and resume on reconnect; the
// user is told by the banner and one Web Push (internal/oauthmail/notify.go).
// A lister without the extension, or a failed list, says nothing — the count
// is not a pass statistic anything depends on.
func (r *Runner) reportAwaitingReconnect(ctx context.Context, userID ulid.ULID) {
	rl, ok := r.lister.(ReconnectLister)
	if !ok {
		return
	}
	held, err := rl.ListAwaitingReconnect(ctx, userID)
	if err != nil || len(held) == 0 {
		return
	}
	names := make([]string, len(held))
	for i, a := range held {
		names[i] = a.Name + " (" + a.ID.String() + ")"
	}
	r.log.Warn("accounts waiting for their user to reconnect: not dialled, writeback and sending held",
		"user_id", userID.String(), "waiting", len(held), "accounts", names)
}

// runScheduledPass lists the syncable accounts, reconciles the per-account
// schedule against that list (adding newly-seen accounts as due-now, dropping
// vanished ones), and syncs only the accounts that are currently DUE. After each
// account's own pass its schedule is advanced: healthy accounts to now+interval,
// failing accounts to now+backoff. It is the loop's per-tick pass.
func (r *Runner) runScheduledPass(ctx context.Context, userID ulid.ULID, pass int) (PassSummary, error) {
	accts, err := r.lister.ListSyncableAccounts(ctx, userID)
	if err != nil {
		return PassSummary{}, fmt.Errorf("syncrunner: list syncable accounts: %w", err)
	}
	// Capture one "now" for the whole pass so due-selection and the resulting
	// next-due are computed against a single, test-controllable instant.
	now := r.now()
	due := r.dueAccounts(userID, accts, now)
	disabled := r.reportDisabled(ctx, userID)
	r.reportAwaitingReconnect(ctx, userID)
	r.log.Info("scheduled sync pass starting",
		"pass", pass, "listed", len(accts), "due", len(due), "disabled", disabled,
		"concurrency", r.concurrency, "user_id", userID.String())

	sum := r.dispatch(ctx, userID, pass, due, disabled, func(acct store.Account, syncErr error) {
		// A cancelled pass is a shutdown, not an account failure: leave the
		// schedule untouched so a restart does not inherit a spurious backoff.
		if ctx.Err() != nil {
			return
		}
		r.advanceSchedule(userID, acct, now, syncErr != nil)
	})
	return sum, ctx.Err()
}

// dispatch syncs the given accounts CONCURRENTLY, bounded to r.concurrency, with
// per-account error isolation. Accounts complete out of order, so every write to
// the shared summary is guarded by mu; a per-account failure is recorded and
// never aborts the pass. onDone, if non-nil, is invoked after each account with
// its sync outcome (nil == success) — the seam through which the scheduled pass
// advances one account's schedule without coupling it to any other. On context
// cancellation dispatch stops and the pass returns once the in-flight accounts
// (which observe ctx themselves) drain — a clean shutdown.
func (r *Runner) dispatch(ctx context.Context, userID ulid.ULID, pass int, accts []store.Account, disabled int, onDone func(store.Account, error)) PassSummary {
	var sum PassSummary
	sum.Accounts = len(accts)
	sum.Disabled = disabled

	var (
		wg  stdsync.WaitGroup
		mu  stdsync.Mutex
		sem = make(chan struct{}, r.concurrency) // bounded worker slots
	)
dispatch:
	for i := range accts {
		acct := accts[i]
		// Acquire a slot, or stop dispatching if the pass is being cancelled.
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// Route the scheduled pass through the SAME per-account gate the
			// IDLE trigger uses (mailroom#103 Stage 2, Deliverable A): a
			// scheduled pass and an IDLE-triggered pass for one account can
			// never run SyncAccount concurrently, and a trigger that arrives
			// mid-pass coalesces into exactly one guaranteed follow-up. This
			// also closes the pre-existing latent overlap hazard between passes.
			r.runGated(userID, acct.ID, func() {
				syncErr := r.syncAccount(ctx, userID, acct, pass, &sum, &mu)
				if onDone != nil {
					onDone(acct, syncErr)
				}
			})
		}()
	}
	wg.Wait()

	if ctx.Err() != nil {
		return sum
	}
	r.log.Info("sync pass complete",
		"pass", pass, "accounts", sum.Accounts, "disabled", sum.Disabled, "synced", sum.Synced, "failed", sum.Failed,
		"messages", sum.MessagesIngested, "messages_muted", sum.MessagesMuted, "flag_updates", sum.FlagUpdates,
		"provenance_reaped", sum.ProvenanceReaped, "messages_vanished", sum.MessagesVanished,
		"mailboxes_observed", sum.MailboxesObserved, "mailboxes_deferred", sum.MailboxesDeferred,
		"mailboxes_unchanged", sum.MailboxesUnchanged, "observations_skipped", sum.ObservationsSkipped,
		"bodies_fetched", sum.BodiesFetched, "body_bytes_fetched", sum.BodyBytesFetched)
	return sum
}

// effectiveInterval is account X's base re-sync period raised to its provider
// floor: max(baseInterval, providerFloor(profile)). The floor only ever slows an
// account down toward a provider-safe cadence; it never syncs one faster than the
// operator's configured base.
func (r *Runner) effectiveInterval(acct store.Account) time.Duration {
	eff := r.interval
	if floor := provider.IMAPMinSyncInterval(acct.ProviderProfile); floor > eff {
		eff = floor
	}
	return eff
}

// dueAccounts reconciles userID's schedule map against the accounts a pass just
// listed and returns the subset that is due now (nextDue <= now). Newly-seen
// accounts are inserted as due-immediately; accounts no longer listed (disabled,
// deleted, sync turned off) are dropped so the map cannot grow without bound. It
// holds scheduleMu for the whole reconcile.
func (r *Runner) dueAccounts(userID ulid.ULID, accts []store.Account, now time.Time) []store.Account {
	r.scheduleMu.Lock()
	defer r.scheduleMu.Unlock()

	byAcct := r.schedules[userID]
	if byAcct == nil {
		byAcct = make(map[ulid.ULID]*schedule, len(accts))
		r.schedules[userID] = byAcct
	}

	live := make(map[ulid.ULID]struct{}, len(accts))
	var due []store.Account
	for i := range accts {
		acct := accts[i]
		live[acct.ID] = struct{}{}
		s := byAcct[acct.ID]
		if s == nil {
			s = &schedule{} // zero nextDue => due immediately (catch-up)
			byAcct[acct.ID] = s
		}
		if !s.nextDue.After(now) {
			due = append(due, acct)
		}
	}
	for id := range byAcct {
		if _, ok := live[id]; !ok {
			delete(byAcct, id)
		}
	}

	// Reap the sync gates of vanished accounts alongside their schedules — but
	// never a gate that is mid-pass (its holder is still driving SyncAccount).
	// A still-running gate is left for a later reconcile or the user's teardown;
	// a vanished account is not triggered or scheduled, so nothing recreates it.
	// Lock order is scheduleMu (held here) -> gate.mu, which runGated never
	// inverts (it releases gate.mu before advanceSchedule takes scheduleMu).
	if gatesByAcct := r.gates[userID]; gatesByAcct != nil {
		for id, g := range gatesByAcct {
			if _, ok := live[id]; ok {
				continue
			}
			g.mu.Lock()
			idle := !g.running
			g.mu.Unlock()
			if idle {
				delete(gatesByAcct, id)
			}
		}
	}

	// Reap the warm pooled connections of vanished accounts too. Anything still
	// in the pool map is idle (a running pass has its session checked out), so
	// these are always safe to close — see reapVanishedPools.
	r.reapVanishedPools(userID, live)
	return due
}

// advanceSchedule moves one account's next-due forward after its pass. On success
// the backoff resets and the account re-syncs one effective interval later; on
// failure the backoff doubles (from the effective interval, capped at
// maxBackoff), and next-due is that backoff out with a little jitter so accounts
// that failed together do not retry in lockstep. It touches ONLY this account's
// entry under scheduleMu, so it can never delay another account.
func (r *Runner) advanceSchedule(userID ulid.ULID, acct store.Account, now time.Time, failed bool) {
	eff := r.effectiveInterval(acct)

	r.scheduleMu.Lock()
	defer r.scheduleMu.Unlock()
	byAcct := r.schedules[userID]
	if byAcct == nil {
		return // user's loop was torn down mid-pass
	}
	s := byAcct[acct.ID]
	if s == nil {
		return // account was pruned mid-pass (disabled/deleted); nothing to advance
	}

	if !failed {
		s.backoff = 0
		s.nextDue = now.Add(eff)
		return
	}

	// Backoff = clamp(max(eff, backoff*2), eff, maxBackoff). The stored magnitude
	// stays a clean multiple; jitter is applied only to the next-due time. The cap
	// is raised to eff when the base interval itself exceeds maxBackoff, so a
	// failing account is never retried FASTER than a healthy one.
	next := s.backoff * 2
	if next < eff {
		next = eff
	}
	ceiling := r.maxBackoff
	if eff > ceiling {
		ceiling = eff
	}
	if next > ceiling {
		next = ceiling
	}
	s.backoff = next
	s.nextDue = now.Add(jitter(next))
}

// jitter returns d scaled by a random factor within +/- backoffJitterFraction.
func jitter(d time.Duration) time.Duration {
	frac := (rand.Float64()*2 - 1) * backoffJitterFraction // in [-f, +f]
	return time.Duration(float64(d) * (1 + frac))
}

// forgetUser drops all schedule state for userID. Called when a user's sync loop
// exits so a later re-run does not act on a stale schedule.
func (r *Runner) forgetUser(userID ulid.ULID) {
	r.scheduleMu.Lock()
	delete(r.schedules, userID)
	delete(r.gates, userID)
	r.closeUserPools(userID) // close every warm connection and drop the pool map
	r.scheduleMu.Unlock()
}

// scheduleState reports userID/accountID's current next-due and backoff (and
// whether an entry exists). It exists for the package's scheduling tests.
func (r *Runner) scheduleState(userID, accountID ulid.ULID) (nextDue time.Time, backoff time.Duration, ok bool) {
	r.scheduleMu.Lock()
	defer r.scheduleMu.Unlock()
	byAcct := r.schedules[userID]
	if byAcct == nil {
		return time.Time{}, 0, false
	}
	s := byAcct[accountID]
	if s == nil {
		return time.Time{}, 0, false
	}
	return s.nextDue, s.backoff, true
}

// syncAccount prepares and syncs one account, isolating any failure into the
// pass summary so one bad account never stops the others. It returns the account's
// sync outcome — nil on success, the prepare or sync error on failure — so the
// scheduled pass can advance this account's backoff without affecting any other.
// A writeback-drain failure is NOT a sync failure (the pass still runs and the
// account is considered synced), matching the existing degrade-don't-fail rule.
func (r *Runner) syncAccount(ctx context.Context, userID ulid.ULID, acct store.Account, pass int, sum *PassSummary, mu *stdsync.Mutex) error {
	// How stale this account had become before this pass began. Sampled here
	// rather than at success, where the lag is zero by definition.
	r.reportSyncLag(acct.ID, time.Now())
	req, err := r.prepare(ctx, userID, acct)
	if err != nil {
		err = fmt.Errorf("prepare: %w", err)
		r.recordFailure(ctx, mu, sum, userID, acct, pass, err)
		return err
	}
	defer crypto.Zero(req.DEK) // scrub the decrypted mail DEK after use

	// Push local deltas up before pulling remote state down (see Writeback). A
	// failure here degrades writeback, not sync: the intents stay queued.
	if r.writeback != nil {
		if err := r.writeback(ctx, userID, acct, req.Config); err != nil && ctx.Err() == nil {
			mu.Lock()
			sum.WritebackFailed++
			mu.Unlock()
			r.log.Error("writeback drain failed; intents stay queued for the next pass",
				"pass", pass, "account_id", acct.ID.String(), "name", acct.Name, "err", err)
		}
	}

	// With the pool on, reuse the account's warm connection across passes
	// (SyncAccountWith over a caller-owned session); with it off, dial-and-close
	// each pass exactly as before (SyncAccount). Both are reached only from inside
	// runGated, so the account is single-passed either way.
	var summary mailsync.Summary
	if r.poolEnabled {
		summary, err = r.syncWithPool(ctx, userID, acct, req)
	} else {
		summary, err = r.syncer.SyncAccount(ctx, req)
	}
	if err != nil {
		r.recordFailure(ctx, mu, sum, userID, acct, pass, err)
		return err
	}

	// Accounts finish out of order; fold this one's tallies in under the lock.
	mu.Lock()
	sum.Synced++
	sum.MailboxesSynced += summary.MailboxesSynced
	sum.MessagesIngested += summary.MessagesIngested
	sum.MessagesMuted += summary.MessagesMuted
	sum.FlagUpdates += summary.FlagUpdates
	sum.BodiesFetched += summary.BodiesFetched
	sum.BodyBytesFetched += summary.BodyBytesFetched
	sum.MailboxesReaped += summary.MailboxesReaped
	sum.ProvenanceReaped += summary.ProvenanceReaped
	sum.MessagesVanished += summary.MessagesVanished
	sum.PlacementsRetired += summary.PlacementsRetired
	sum.GenerationsReconciled += summary.GenerationsReconciled
	sum.ProvenanceRetired += summary.ProvenanceRetired
	sum.MailboxesDeferred += summary.MailboxesDeferred
	sum.MailboxesObserved += summary.MailboxesObserved
	sum.MailboxesUnchanged += summary.MailboxesUnchanged
	sum.ObservationsSkipped += summary.ObservationsSkipped
	mu.Unlock()

	// ⚠ ONE LINE PER PASS, NOT ONE PER MAILBOX (#638). This is the whole
	// reporting change: a throttled pass used to emit 300–600 identical WARNs,
	// one per refused mailbox, among which a mailbox that was genuinely broken
	// could not be seen. It is a WARN rather than an INFO because it names work
	// that did not happen — the deferred mailboxes' expunge reconciliation and
	// gap backfill slip to a later pass — but it is emphatically not an error:
	// nothing was concluded about those mailboxes and they are due next pass.
	if summary.MailboxesDeferred > 0 {
		r.log.Warn("mailboxes deferred to the next pass: server refused a UID SEARCH (rate limit)",
			"pass", pass, "account_id", acct.ID.String(), "name", acct.Name,
			"mailboxes_deferred", summary.MailboxesDeferred,
			// Zero means the server named no wait — not "retry immediately".
			"server_retry_after", summary.RateLimitRetryAfter.String())
	}
	// ⚠ A PER-MAILBOX FAILURE USED TO BE COUNTED AND ITS CAUSE THROWN AWAY (#277).
	//
	// summary.Errors carries {Mailbox, Err} for every mailbox that failed, and the
	// only consumer in the tree took len() of it. An operator saw `mailbox_errors:
	// 2` on a line marked INFO and had no way to learn WHICH two or WHY: the
	// `mailboxes` tally silently shrank and the pass still read as a success. That
	// happened on a real instance and the cause is permanently
	// unrecoverable, because nothing ever wrote it down.
	//
	// These failures are non-fatal BY DESIGN (mailsync.Summary: a failed mailbox's
	// cursors are left unadvanced so the next pass re-fetches it, and ingest is
	// idempotent). That design is right, and it is exactly why the failures have to
	// be VISIBLE — nothing else will ever surface them, so a mailbox that has failed
	// every pass for a week is indistinguishable from one that was briefly busy.
	level := slog.LevelInfo
	if len(summary.Errors) > 0 {
		level = slog.LevelWarn // a degraded pass is not an INFO event
	}
	r.log.Log(ctx, level, "account synced",
		"pass", pass, "account_id", acct.ID.String(), "name", acct.Name,
		"mailboxes", summary.MailboxesSynced, "messages", summary.MessagesIngested,
		"messages_muted", summary.MessagesMuted,
		"flag_updates", summary.FlagUpdates, "mailbox_errors", len(summary.Errors),
		"mailboxes_reaped", summary.MailboxesReaped,
		"provenance_reaped", summary.ProvenanceReaped,
		"messages_vanished", summary.MessagesVanished,
		"placements_retired", summary.PlacementsRetired,
		"generations_reconciled", summary.GenerationsReconciled,
		"provenance_retired", summary.ProvenanceRetired,
		"mailboxes_observed", summary.MailboxesObserved,
		"mailboxes_deferred", summary.MailboxesDeferred,
		"mailboxes_unchanged", summary.MailboxesUnchanged,
		"observations_skipped", summary.ObservationsSkipped,
		// Non-zero means a pass read a superseded draft's copy before the
		// writeback pass that removes it, and correctly did not ingest it (#686).
		"copies_awaiting_expunge", summary.CopiesAwaitingExpunge,
		"bodies_fetched", summary.BodiesFetched,
		"body_bytes_fetched", summary.BodyBytesFetched,
		// #908/#909 and the CONDSTORE-less flag sweep (ADR-046 §12): what a
		// log scan can count per pass. gaps_backfilled is the #93 recovery's
		// ingests (its new arrivals now push, #908); flags_absent should be 0.
		"gaps_backfilled", summary.GapsBackfilled,
		"flags_absent", summary.FlagsAbsent,
		"flag_refresh_aborted", summary.FlagRefreshAborted,
		"flag_sweep_read", summary.FlagSweepRead,
		"flag_sweep_applied", summary.FlagSweepApplied,
		// Wall time of the engine's pass over this account (SyncAccountWith) —
		// not the writeback drain or the dial before it. #620 is judged on it.
		"duration_ms", summary.Duration.Milliseconds())
	for _, e := range summary.Errors {
		mailbox := e.Mailbox
		if mailbox == "" {
			// MailboxError documents an empty Mailbox as an ACCOUNT-level failure
			// (the per-user retirement sweep). Name it rather than log a blank key.
			mailbox = "(account-level)"
		}
		r.log.Warn("mailbox sync failed; non-fatal, cursors left unadvanced for the next pass",
			"pass", pass, "account_id", acct.ID.String(), "name", acct.Name,
			"mailbox", mailbox, "err", e.Err)
	}
	r.recordHealth(ctx, userID, acct, pass, nil)
	// New mail arrived → notify the user's devices. Guarded on the count so a
	// no-op pass sends nothing, and fail-soft so a push failure never touches sync.
	if n := pushCount(summary); n > 0 {
		r.notifyPush(ctx, userID, acct, pass, n, summary.NotifyIDs)
	}
	r.emit(ProgressEvent{
		Pass: pass, UserID: userID, AccountID: acct.ID, AccountName: acct.Name, Summary: summary,
	})
	return nil
}

func (r *Runner) recordFailure(ctx context.Context, mu *stdsync.Mutex, sum *PassSummary, userID ulid.ULID, acct store.Account, pass int, err error) {
	err = explainIMAPAccess(acct, err)
	mu.Lock()
	sum.Failed++
	sum.Errors = append(sum.Errors, AccountError{AccountID: acct.ID, Name: acct.Name, Err: err})
	mu.Unlock()
	r.log.Error("account sync failed",
		"pass", pass, "account_id", acct.ID.String(), "name", acct.Name, "err", err)
	r.recordHealth(ctx, userID, acct, pass, err)
	r.emit(ProgressEvent{
		Pass: pass, UserID: userID, AccountID: acct.ID, AccountName: acct.Name, Err: err,
	})
}

// recordHealth persists this account's pass outcome as operational sync-health
// telemetry (mailroom#43). It is FAIL-SOFT by contract: any error is logged and
// swallowed, because a telemetry write must never fail — or mask — the real sync
// outcome the caller already recorded. It is a no-op when no recorder is
// installed (the unit-test configuration) and is skipped on a cancelled pass, so
// a shutdown is never mis-recorded as an account-health failure.
func (r *Runner) recordHealth(ctx context.Context, userID ulid.ULID, acct store.Account, pass int, syncErr error) {
	if r.health == nil || ctx.Err() != nil {
		return
	}
	var err error
	if syncErr != nil {
		err = r.health.RecordSyncFailure(ctx, userID, acct.ID, syncErr.Error(), time.Now())
	} else {
		now := time.Now()
		r.noteSyncSuccess(acct.ID, now)
		err = r.health.RecordSyncSuccess(ctx, userID, acct.ID, now)
	}
	if err != nil && ctx.Err() == nil {
		r.log.Warn("recording account sync-health failed; telemetry only, sync pass unaffected",
			"pass", pass, "account_id", acct.ID.String(), "name", acct.Name,
			"outcome_failed", syncErr != nil, "err", err)
	}
}

// pushCount is how many of a pass's new messages the user should be notified
// about: every new canonical message EXCEPT the ones that arrived in a muted
// conversation and were archived on arrival (spec §6). Muting a conversation is
// the user saying "stop bringing this to my attention", and a push is the loudest
// way Mailroom has of doing that — so a pass that brought ONLY muted replies
// sends nothing at all, and a mixed pass counts only the rest.
//
// Mail a skipInbox RULE archived is still counted, deliberately and unchanged:
// that is a filing decision, not a request for silence. The one overlap — a
// muted reply a rule archived first — is reported as muted by ingest, so it is
// left out here too (store.DivertMutedArrival).
//
// Mail from a BLOCKED sender (#59) is left out for the same reason as muted
// mail, more strongly: the user asked never to see it. The two are disjoint
// (ingest decides the block first and never reaches the mute check).
func pushCount(summary mailsync.Summary) int {
	return summary.MessagesIngested - summary.MessagesMuted - summary.MessagesBlocked
}

// notifyPush fires the new-mail Web Push for one account. It is FAIL-SOFT by
// contract, exactly like recordHealth: any error is logged and swallowed, because
// a push must never fail — or mask — the sync outcome already recorded. It is a
// no-op when no notifier is installed and is skipped on a cancelled pass.
func (r *Runner) notifyPush(ctx context.Context, userID ulid.ULID, acct store.Account, pass, newMessages int, ids []ulid.ULID) {
	if r.push == nil || ctx.Err() != nil {
		return
	}
	if err := r.push.NotifyNewMail(ctx, userID, acct, newMessages, ids); err != nil && ctx.Err() == nil {
		r.log.Warn("web push notify failed; notification only, sync pass unaffected",
			"pass", pass, "account_id", acct.ID.String(), "name", acct.Name, "err", err)
	}
}

func (r *Runner) emit(ev ProgressEvent) {
	if r.progress != nil {
		r.progress(ev)
	}
}
