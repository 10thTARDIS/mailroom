// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// ONE WEB PUSH PER TRANSITION INTO needs_reauth (#52, ADR-046 §5; decided
// 2026-10-27: a banner, plus one push per transition, no repeats).
//
// How "exactly one" is built, layer by layer:
//
//   - ONE TRANSITION. store.SetOAuthGrantState reads the grant FOR UPDATE and
//     writes only when the state actually changes, so of N sync workers (in
//     one process or several) that all hear invalid_grant at once, exactly one
//     transaction makes the transition; the rest find the grant already
//     needs_reauth and write nothing.
//   - ONE JOB, IN THAT TRANSACTION. The transition enqueues the notice as a
//     durable job (a transactional outbox) in the SAME transaction, keyed by
//     the account and the transition time. The notice commits exactly when the
//     transition does: a crash before the commit loses both, a crash after it
//     loses neither, and a restart finds the job waiting. No restart can make
//     a second one, because the grant is already needs_reauth and a refresh is
//     never even attempted in that state (Manager.refresh), let alone a second
//     transition.
//   - NO REPEATS. Every later refresh, pass or restart finds the state already
//     needs_reauth: no transition, no job. The next notice needs the user to
//     reconnect (back to active) and the grant to die again.
//   - NOTHING STALE. The worker re-reads the grant under the user's identity
//     and sends only if it is still in the transition the job was queued for,
//     so a user who reconnected before the job ran is not told to reconnect.
//
// The one window left is the job dispatcher's own at-least-once: a process
// that dies after the push service accepted the notice but before the job is
// recorded done sends it again on the retry. No design closes that without
// the push service taking part in our transaction; it is stated here rather
// than claimed away.
//
// operator_error is deliberately NOT pushed to users: the provider refused
// this server's client registration, every user of that provider hit it at
// once, and only the operator can act (Manager.OperatorIssues).

// ReauthNotifyJobKind is the job that sends the notice.
const ReauthNotifyJobKind = "oauth_reauth_notify"

// reauthQuietDeferral is how long a notice that met the user's quiet hours
// waits before it looks again.
const reauthQuietDeferral = 15 * time.Minute

// reauthChangedAtSlack absorbs the timestamp's round trip through the
// database (microseconds there, nanoseconds here). Two real transitions of one
// grant are separated by a reconnect, which is a person signing in at the
// provider — never a millisecond.
const reauthChangedAtSlack = time.Millisecond

// reauthPayload carries ids and one timestamp, never anything from the mail or
// the provider (store.jobPayloadKeys).
type reauthPayload struct {
	AccountID string `json:"account_id"`
	ChangedAt string `json:"changed_at"`
}

// enqueueReauthNotice queues the notice for the transition that tx is making.
func enqueueReauthNotice(ctx context.Context, tx *store.UserTx, accountID ulid.ULID, at time.Time) error {
	at = at.UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(reauthPayload{AccountID: accountID.String(), ChangedAt: at.Format(time.RFC3339Nano)})
	if err != nil {
		return fmt.Errorf("oauthmail: reauth notice payload: %w", err)
	}
	key := fmt.Sprintf("%s:%s:%d", ReauthNotifyJobKind, accountID, at.UnixMicro())
	if _, err := tx.EnqueueJob(ctx, ReauthNotifyJobKind, at, payload, key); err != nil {
		return fmt.Errorf("oauthmail: queue reauth notice: %w", err)
	}
	return nil
}

// ReconnectPusher delivers the notice to the user's devices
// (syncrunner.StorePushNotifier implements it).
type ReconnectPusher interface {
	// PushReconnectNeeded tells every one of the user's devices that the
	// account must be signed in again. quiet is true when nothing was sent
	// because the user's quiet hours are in force; the caller tries later.
	PushReconnectNeeded(ctx context.Context, userID, accountID ulid.ULID, accountName string) (quiet bool, err error)
}

// ReauthNotifyHandler is the job handler for ReauthNotifyJobKind. now may be
// nil (time.Now).
func ReauthNotifyHandler(st *store.Store, p ReconnectPusher, log *slog.Logger, now func() time.Time) func(context.Context, store.Job) error {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, job store.Job) error {
		if job.UserID == nil {
			return fmt.Errorf("oauthmail: reauth notice job %s has no user", job.ID)
		}
		userID := *job.UserID
		var pl reauthPayload
		if err := json.Unmarshal(job.Payload, &pl); err != nil {
			return fmt.Errorf("oauthmail: reauth notice job %s: bad payload: %w", job.ID, err)
		}
		accountID, err := ulid.Parse(pl.AccountID)
		if err != nil {
			return fmt.Errorf("oauthmail: reauth notice job %s: bad account id: %w", job.ID, err)
		}
		changedAt, err := time.Parse(time.RFC3339Nano, pl.ChangedAt)
		if err != nil {
			return fmt.Errorf("oauthmail: reauth notice job %s: bad transition time: %w", job.ID, err)
		}
		lg := log.With("user_id", userID.String(), "account_id", accountID.String())

		var (
			name  string
			still bool
		)
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			g, err := tx.OAuthGrant(ctx, accountID)
			if errors.Is(err, store.ErrOAuthGrantNotFound) {
				return nil // disconnected or deleted since: nothing to say
			}
			if err != nil {
				return err
			}
			d := g.StateChangedAt.Sub(changedAt)
			if d < 0 {
				d = -d
			}
			if g.State != store.OAuthStateNeedsReauth || d > reauthChangedAtSlack {
				return nil // reconnected (and perhaps broken again: that transition has its own job)
			}
			a, err := tx.GetAccount(ctx, accountID)
			if err != nil {
				return err
			}
			name, still = a.Name, true
			return nil
		}); err != nil {
			return fmt.Errorf("oauthmail: reauth notice: %w", err)
		}
		if !still {
			lg.Info("oauth reconnect notice not sent: the grant left that state before the notice went out")
			return nil
		}
		quiet, err := p.PushReconnectNeeded(ctx, userID, accountID, name)
		if err != nil {
			return err
		}
		if !quiet {
			lg.Info("oauth reconnect notice sent")
			return nil
		}
		// Quiet hours: the notice waits rather than being dropped (it is the
		// only push this transition will ever get) or sent at 3 a.m. A fresh
		// job, so the wait costs none of this job's retry budget.
		at := now().Add(reauthQuietDeferral)
		key := fmt.Sprintf("%s:%s:%d:after:%d", ReauthNotifyJobKind, accountID, changedAt.UnixMicro(), at.Unix())
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			_, err := tx.EnqueueJob(ctx, ReauthNotifyJobKind, at, job.Payload, key)
			return err
		}); err != nil {
			return fmt.Errorf("oauthmail: defer reauth notice: %w", err)
		}
		lg.Info("oauth reconnect notice deferred: quiet hours", "retry_at", at.UTC().Format(time.RFC3339))
		return nil
	}
}
