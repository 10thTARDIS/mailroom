// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mailsync"
	"mailroom/internal/store"
	mrsync "mailroom/internal/sync"
)

// readingLister is a fakeLister that can also re-read one account (the
// AccountReader extension), answering with whatever `now` holds.
type readingLister struct {
	fakeLister
	now atomic.Pointer[store.Account]
}

func (l *readingLister) GetAccount(_ context.Context, _, id ulid.ULID) (store.Account, error) {
	a := l.now.Load()
	if a == nil || a.ID != id {
		return store.Account{}, store.ErrAccountNotFound
	}
	return *a, nil
}

// #52: the IDLE watcher captured its account when it started. A grant that has
// since gone needs_reauth (or been disconnected) must stop the dials at once —
// not after reconcileIdle next drops the account, several redials later.
func TestIdleSessionRereadsTheGrantBeforeDialling(t *testing.T) {
	started := store.Account{ID: ulid.Make(), Name: "outlook", AuthKind: "oauth2", AuthState: store.OAuthStateActive}
	lister := &readingLister{fakeLister: fakeLister{accts: []store.Account{started}}}
	var prepares, dials atomic.Int32
	prepare := func(ctx context.Context, u ulid.ULID, a store.Account) (mailsync.AccountSync, error) {
		prepares.Add(1)
		return idPrepare(ctx, u, a)
	}
	r := New(lister, &fakeSyncer{}, prepare,
		WithLogger(discardLogger()),
		WithIdle(true, 0),
		WithIdleDialer(func(context.Context, mrsync.Config) (mrsync.SyncSession, error) {
			dials.Add(1)
			return nil, errors.New("dial attempted")
		}),
	)

	// The control: an active grant is dialled.
	lister.now.Store(&started)
	if err := r.idleSession(context.Background(), ulid.Make(), started); err == nil || dials.Load() != 1 {
		t.Fatalf("active grant: err=%v dials=%d, want one dial", err, dials.Load())
	}

	for _, st := range []string{store.OAuthStateNeedsReauth, ""} {
		now := started
		now.AuthState = st
		lister.now.Store(&now)
		p, d := prepares.Load(), dials.Load()
		for i := 0; i < 3; i++ {
			if err := r.idleSession(context.Background(), ulid.Make(), started); !errors.Is(err, errIdleAwaitingReconnect) {
				t.Fatalf("state %q: err = %v, want errIdleAwaitingReconnect", st, err)
			}
		}
		if prepares.Load() != p || dials.Load() != d {
			t.Fatalf("state %q: %d prepares and %d dials after the transition, want 0 and 0",
				st, prepares.Load()-p, dials.Load()-d)
		}
	}
}
