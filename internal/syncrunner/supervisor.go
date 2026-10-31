// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// DefaultRefreshInterval is how often the Supervisor re-lists active users to
// pick up ones provisioned since the last pass (and drop ones disabled/deleted).
// It is the latency between a brand-new user's first login and their mail
// starting to sync, so it is deliberately short — a minute, not the sync
// interval — while still being a cheap indexed read of a ≤ ~24-row table.
const DefaultRefreshInterval = time.Minute

// UserLister lists the ids of every provisioned, enabled user the fan-out should
// drive. Satisfied by StoreUserLister over a *store.Store (store.ListActiveUserIDs,
// a narrow SECURITY DEFINER — the only way to enumerate users under RLS).
type UserLister interface {
	ListActiveUserIDs(ctx context.Context) ([]ulid.ULID, error)
}

// UserTask is one unit of per-user background work. The Supervisor starts it once
// per user when it first sees that user, under a context that is cancelled when
// the user is disabled/removed or the whole supervisor shuts down. A long-running
// task (the sync loop, the search indexer) blocks until then; a one-shot task
// (seeding a recurring Trash-purge job) simply returns early, which is fine.
//
// A task owns its own error handling: it must log-and-continue rather than return
// on a recoverable failure, exactly as Runner.Run and search.Indexer.Run already
// do. The Supervisor recovers a panicking task so one user can never crash the
// process, but a task that keeps panicking will not be restarted until the user
// is re-listed.
type UserTask func(ctx context.Context, userID ulid.ULID)

// Supervisor fans a fixed set of per-user tasks out over every active user, and
// keeps that set current: users provisioned at runtime (the OIDC auto-provision
// path, mailroom#98) start syncing without a restart, and users disabled or
// deleted have their loops torn down. One slow or broken user cannot stall the
// others — every user's tasks run in their own goroutines, and the discovery loop
// never blocks on them.
type Supervisor struct {
	lister   UserLister
	tasks    []UserTask
	interval time.Duration
	log      *slog.Logger
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*Supervisor)

// WithRefreshInterval overrides DefaultRefreshInterval.
func WithRefreshInterval(d time.Duration) SupervisorOption {
	return func(s *Supervisor) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithSupervisorLogger sets the logger (default slog.Default()).
func WithSupervisorLogger(l *slog.Logger) SupervisorOption {
	return func(s *Supervisor) {
		if l != nil {
			s.log = l
		}
	}
}

// NewSupervisor builds a Supervisor that runs tasks for each active user. With no
// tasks it is an inert discovery loop; the caller passes whichever per-user work
// the instance actually has (sync, indexing, purge seeding).
func NewSupervisor(lister UserLister, tasks []UserTask, opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		lister:   lister,
		tasks:    tasks,
		interval: DefaultRefreshInterval,
		log:      slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}
	if s.interval <= 0 {
		s.interval = DefaultRefreshInterval
	}
	return s
}

// Run discovers active users immediately, then every refresh interval, starting
// each task per newly-seen user and cancelling a vanished user's tasks. It blocks
// until ctx is cancelled, whereupon it cancels every per-user context and waits
// for all task goroutines to unwind before returning ctx.Err() — so a caller that
// waits on Run has a genuine clean-shutdown barrier for all per-user work.
func (s *Supervisor) Run(ctx context.Context) error {
	// active is only ever touched from THIS goroutine (in reconcile), so it needs
	// no lock; the WaitGroup is the only cross-goroutine coordination.
	active := make(map[ulid.ULID]context.CancelFunc)
	var wg sync.WaitGroup

	defer func() {
		for _, cancel := range active {
			cancel()
		}
		wg.Wait()
	}()

	s.reconcile(ctx, active, &wg)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log.Info("user fan-out: stopping", "active_users", len(active))
			return ctx.Err()
		case <-ticker.C:
			s.reconcile(ctx, active, &wg)
		}
	}
}

// reconcile brings the running per-user task set in line with the current active
// users. A listing error is logged and the current set is KEPT — a transient
// database blip must not tear down everyone's sync.
func (s *Supervisor) reconcile(ctx context.Context, active map[ulid.ULID]context.CancelFunc, wg *sync.WaitGroup) {
	ids, err := s.lister.ListActiveUserIDs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("user fan-out: listing active users failed; keeping the current set", "err", err)
		}
		return
	}

	seen := make(map[ulid.ULID]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
		if _, running := active[id]; running {
			continue
		}
		uctx, cancel := context.WithCancel(ctx)
		active[id] = cancel
		s.log.Info("user fan-out: starting per-user loops", "user_id", id.String())
		for _, task := range s.tasks {
			s.launch(uctx, id, task, wg)
		}
	}

	// Users that dropped out of the active set (disabled or deleted) get their
	// loops cancelled. Their goroutines are still tracked by wg until they unwind.
	for id, cancel := range active {
		if _, ok := seen[id]; !ok {
			s.log.Info("user fan-out: user no longer active; stopping its loops", "user_id", id.String())
			cancel()
			delete(active, id)
		}
	}
}

// launch runs one task for one user in its own goroutine, recovering a panic so a
// single user's failure can never take the process down.
func (s *Supervisor) launch(ctx context.Context, userID ulid.ULID, task UserTask, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("user fan-out: a per-user task panicked; other users are unaffected",
					"user_id", userID.String(), "panic", r)
			}
		}()
		task(ctx, userID)
	}()
}
