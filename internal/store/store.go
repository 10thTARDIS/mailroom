// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

// ErrNoIdentity is returned by UserTx when no user identity is supplied.
// The store fails closed (data-model §5): no identity, no rows.
var ErrNoIdentity = errors.New("store: user transaction requires a user identity")

// DefaultTrashRetention is the Trash dwell time before the purge job hard-
// deletes a soft-deleted message (spec §4 / ADR-009). It is the fallback the
// store carries when no other value is configured; production overrides it from
// config (MAILROOM_TRASH_RETENTION) via SetTrashRetention.
const DefaultTrashRetention = 30 * 24 * time.Hour

// Store owns the database pool. The pool must connect as mailroom_app —
// a non-owner role without BYPASSRLS, so FORCE ROW LEVEL SECURITY applies
// to every statement issued through it.
type Store struct {
	pool *pgxpool.Pool
	// trashRetention is stamped onto purge_after when a message is soft-deleted
	// (ApplyEmailUpdate's trash path and the reaper's vanish path). It is read
	// once per UserTx into UserTx.trashRetention; a setter, not an Open argument,
	// so config threads it through without a ripple across every UserTx caller.
	// Set at boot before any transaction runs (SetTrashRetention); never mutated
	// concurrently with use.
	trashRetention time.Duration
	// writebackDisabled is the INSTANCE-level kill switch
	// (MAILROOM_WRITEBACK_DISABLED, ADR-028 §2). It ORs with the per-account
	// app.accounts.writeback_enabled column: either one being off suppresses
	// writeback. Threaded the same way trashRetention is — a setter called once
	// at boot, copied into each UserTx — so config reaches the enqueue path
	// without a ripple across every caller.
	//
	// ⚠ It must be visible HERE, not only at the drain, or the instance flag
	// would leave intents piling up as 'pending' for an account whose own column
	// says true — which is exactly the accumulate-then-fire hazard sync_enabled
	// already has.
	writebackDisabled bool
}

// DefaultMaxConns is the connection pool size when the DSN sets no
// pool_max_conns.
const DefaultMaxConns = 16

// Open connects a pool and wraps it in a Store.
//
// The pool size is explicit: pgx's own default is max(4, NumCPU), which a small
// host can exhaust with a few long-held transactions (silent session renewal
// holds one across a provider call, #937). DefaultMaxConns applies unless the
// DSN says otherwise with the pgx parameter pool_max_conns=N.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	if !strings.Contains(dsn, "pool_max_conns") {
		pcfg.MaxConns = DefaultMaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	return &Store{pool: pool, trashRetention: DefaultTrashRetention}, nil
}

// NewFromPool wraps an existing pool (tests, custom pool config).
func NewFromPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, trashRetention: DefaultTrashRetention}
}

// SetTrashRetention overrides the Trash dwell time stamped onto purge_after
// when a message is soft-deleted (ADR-009). A non-positive value is ignored so
// a mis-set config can never collapse the window to zero (which would purge on
// trash). Call once at boot, before serving traffic.
func (s *Store) SetTrashRetention(d time.Duration) {
	if d > 0 {
		s.trashRetention = d
	}
}

// SetWritebackDisabled sets the INSTANCE-level writeback kill switch
// (MAILROOM_WRITEBACK_DISABLED, ADR-028 §2). When true, every account on this
// instance behaves as ingest-only regardless of its own writeback_enabled
// column, and Account/set refuses to turn the per-account flag back on.
//
// This is the safety catch for a cutover window: it lives in the deployment's
// environment rather than in user data, so it cannot be lifted by a misclick in
// the settings UI. Call once at boot, before serving traffic.
func (s *Store) SetWritebackDisabled(v bool) { s.writebackDisabled = v }

// WritebackDisabled reports the instance-level kill switch, for the API layer's
// refusal to enable writeback per-account while it is set.
func (s *Store) WritebackDisabled() bool { return s.writebackDisabled }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// UserTx is the ONLY way to touch user data (data-model §5, ADR-010).
// It exists solely inside Store.UserTx callbacks; the wrapped pgx.Tx is
// unexported so no code outside this package can issue raw statements.
type UserTx struct {
	tx     pgx.Tx
	userID ulid.ULID
	// trashRetention is copied from the Store at transaction start so the soft-
	// delete paths (ApplyEmailUpdate trash, reaper vanish) stamp a consistent,
	// configurable purge_after without reaching back to the Store.
	trashRetention time.Duration
	// writebackDisabled is the instance-level half of the ingest-only switch,
	// copied from the Store at transaction start (ADR-028 §2).
	writebackDisabled bool
}

// UserID returns the identity this transaction is scoped to.
func (tx *UserTx) UserID() ulid.ULID { return tx.userID }

// UserTx begins a transaction scoped to userID and runs fn inside it.
// The identity is installed with set_config(..., is_local=true) — the exact
// parameterized equivalent of SET LOCAL: transaction-scoped, so pooled
// connections can never leak identity between requests. A zero userID is
// rejected before any statement runs.
func (s *Store) UserTx(ctx context.Context, userID ulid.ULID, fn func(tx *UserTx) error) error {
	if userID == (ulid.ULID{}) {
		return ErrNoIdentity
	}
	return pgx.BeginFunc(ctx, s.pool, func(ptx pgx.Tx) error {
		if _, err := ptx.Exec(ctx,
			`SELECT set_config('mailroom.user_id', $1, true)`, userID.String(),
		); err != nil {
			return fmt.Errorf("store: set user identity: %w", err)
		}
		retention := s.trashRetention
		if retention <= 0 {
			retention = DefaultTrashRetention
		}
		return fn(&UserTx{tx: ptx, userID: userID, trashRetention: retention,
			writebackDisabled: s.writebackDisabled})
	})
}

// SystemTx is the door for the job dispatcher and reconcilers.
// mailroom.user_id stays unset, so app.current_user_id() is NULL and every
// RLS policy evaluates to not-visible: user rows are invisible here.
// Cross-user work happens only through narrow SECURITY DEFINER functions
// (app.dequeue_due_jobs) that expose job rows, never mail content.
type SystemTx struct {
	tx pgx.Tx
}

// SystemTx begins an identity-less transaction and runs fn inside it.
func (s *Store) SystemTx(ctx context.Context, fn func(tx *SystemTx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(ptx pgx.Tx) error {
		// Defensive clear: '' reads back as NULL through the NULLIF in
		// app.current_user_id(). A fresh transaction is already unset; this
		// guards against a hypothetical session-level leak ever appearing.
		if _, err := ptx.Exec(ctx,
			`SELECT set_config('mailroom.user_id', '', true)`,
		); err != nil {
			return fmt.Errorf("store: clear user identity: %w", err)
		}
		return fn(&SystemTx{tx: ptx})
	})
}

// entropy backs NewULID: crypto/rand-based and monotonic within a
// millisecond, locked for concurrent use.
var entropy = &ulid.LockedMonotonicReader{MonotonicReader: ulid.Monotonic(crand.Reader, 0)}

// NewULID returns a new app-side object id (ADR-007). crypto/rand entropy
// keeps ids non-enumerable (spec §10).
func NewULID() ulid.ULID {
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy)
}
