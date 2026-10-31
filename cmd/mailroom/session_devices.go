// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// Signed-in devices, server side (#937, ADR-048 sections 6 and 7): provider
// revocation for a device sign-out, the stream re-check, and the daily sweep.

// sessionSweepInterval is how often the session sweep runs (ADR-048 section 7).
const sessionSweepInterval = 24 * time.Hour

// deviceRevokeTimeout bounds one detached provider revocation: the wait for a
// slot, the time to open the sealed token, and the call's own bound (5 s).
const deviceRevokeTimeout = 15 * time.Second

// deviceRevokeConcurrency is how many provider revocations may be in flight at
// once. "*others*" for a user with many sessions would otherwise start one
// goroutine and one provider call per session, all at once.
const deviceRevokeConcurrency = 4

// providerRevoker revokes a signed-out device's refresh token at the identity
// provider (RFC 7009). The API holds only ciphertext; this opens it and calls the
// Authenticator.
//
// The Authenticator is built later than the API (oidcGate runs inside mountApp),
// so it is installed after the fact; until then, and always under dev-auth, a
// revocation is a no-op.
type providerRevoker struct {
	sessions *sessionStore
	base     context.Context
	log      *slog.Logger
	authn    atomic.Pointer[auth.Authenticator]

	// slots bounds the revocations in flight (deviceRevokeConcurrency). A
	// revocation waits for a slot inside its own goroutine, under its overall
	// timeout, and is dropped (logged) if the timeout passes first.
	slots chan struct{}
	// timeout is the overall bound (deviceRevokeTimeout; a test seam).
	timeout time.Duration
	// work does one revocation once it holds a slot (a test seam; nil means the
	// real one: open the sealed token, call the provider).
	work func(ctx context.Context, authn *auth.Authenticator, userID, sessionID ulid.ULID, sealed []byte)
}

func newProviderRevoker(base context.Context, sessions *sessionStore, log *slog.Logger) *providerRevoker {
	return &providerRevoker{sessions: sessions, base: base, log: log,
		slots: make(chan struct{}, deviceRevokeConcurrency), timeout: deviceRevokeTimeout}
}

// set installs the Authenticator. Nil-safe on the receiver, because the gate
// tests build the gate without one.
func (p *providerRevoker) set(a *auth.Authenticator) {
	if p != nil {
		p.authn.Store(a)
	}
}

// revoke is api.SessionRevoker. It returns at once: the user is not waiting on
// the provider to sign out a device that is somewhere else. The session row is
// already revoked, so a failure here costs only the provider-side grant, and is
// logged, never the token.
func (p *providerRevoker) revoke(userID, sessionID ulid.ULID, sealed []byte) {
	authn := p.authn.Load()
	if authn == nil || len(sealed) == 0 {
		return
	}
	go func() {
		// Detached from the request (it has finished), bounded on its own, but
		// still ended by process shutdown.
		ctx, cancel := context.WithTimeout(p.base, p.timeout)
		defer cancel()
		select {
		case p.slots <- struct{}{}:
			defer func() { <-p.slots }()
		case <-ctx.Done():
			// Too many revocations ahead of this one. The device is signed out here
			// regardless; only the provider-side grant is left to expire on its own.
			p.log.InfoContext(context.WithoutCancel(ctx), "device sign-out: dropped the provider revocation of a session, the provider was busy for too long",
				"session", sessionID.String())
			return
		}
		if p.work != nil {
			p.work(ctx, authn, userID, sessionID, sealed)
			return
		}
		p.revokeNow(ctx, authn, userID, sessionID, sealed)
	}()
}

// revokeNow opens the sealed token and revokes it at the provider.
func (p *providerRevoker) revokeNow(ctx context.Context, authn *auth.Authenticator, userID, sessionID ulid.ULID, sealed []byte) {
	token, err := p.sessions.openRefresh(ctx, userID, sessionID, sealed)
	if err != nil {
		p.log.WarnContext(ctx, "device sign-out: could not open the refresh token to revoke it at the provider",
			"session", sessionID.String(), "err", err)
		return
	}
	if err := authn.RevokeRefreshToken(ctx, token); err != nil {
		p.log.WarnContext(ctx, "device sign-out: revoking the refresh token at the provider failed; the device is signed out here regardless",
			"session", sessionID.String(), "err", err)
	}
}

var _ api.SessionRevoker = (*providerRevoker)(nil).revoke

// sessionLiveCheck is api.SessionLive over the store: the session is live when it
// is not revoked, its user can still sign in, and it is inside the outage
// allowance. It stamps nothing and never renews.
func sessionLiveCheck(st *store.Store, allowance time.Duration) api.SessionLive {
	return func(ctx context.Context, sessionID, userID ulid.ULID) (bool, error) {
		var live bool
		err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var err error
			live, err = tx.SessionLive(ctx, sessionID, allowance, time.Now())
			return err
		})
		return live, err
	}
}

// runSessionSweep runs the session sweep once now and then every interval until
// ctx ends. It is a ticker, like the other instance-wide periodic sweeps, not a
// durable job: the durable queue's recurring kind is per user, this has no user,
// and a missed run costs nothing (the next one deletes the same rows).
// Several processes running it at once is harmless: the delete is idempotent.
func runSessionSweep(ctx context.Context, st *store.Store, interval time.Duration, log *slog.Logger) {
	sweepSessionsOnce(ctx, st, time.Now(), log)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sweepSessionsOnce(ctx, st, now, log)
		}
	}
}

// sweepSessionsOnce runs one sweep and logs the count PER REASON, every reason
// listed even when zero: a single total cannot tell "nothing to do" from "the
// reason I measured is always empty" (CLAUDE.md).
func sweepSessionsOnce(ctx context.Context, st *store.Store, now time.Time, log *slog.Logger) {
	counts, err := st.SweepSessions(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("session sweep failed", "err", err)
		}
		return
	}
	attrs := make([]any, 0, 2*len(store.SweepReasons))
	for _, r := range store.SweepReasons {
		attrs = append(attrs, r, counts[r])
	}
	log.Info("session sweep", attrs...)
}
