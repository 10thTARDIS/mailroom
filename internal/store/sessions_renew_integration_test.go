// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// RenewSessionLocked (#937, ADR-048 section 4): the per-session lock, the
// locked re-read and the same-transaction write, as mailroom_app.

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRenewSessionLocked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	stale := func(id string) {
		t.Helper()
		if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_checked_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	run := func(user, id interface{ String() string }, staleBefore time.Time, fn func(SessionRenewState) (SessionRenewWrite, error)) (SessionRenewState, bool, error) {
		t.Helper()
		var st SessionRenewState
		var ran bool
		uid := mustULID(t, user.String())
		sid := mustULID(t, id.String())
		err := s.UserTx(ctx, uid, func(tx *UserTx) error {
			var e error
			st, ran, e = tx.RenewSessionLocked(ctx, sid, staleBefore, fn)
			return e
		})
		return st, ran, err
	}

	t.Run("a stale live session runs fn with the locked state and applies the write", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-apply", func(n *NewSession) { n.RefreshCT = []byte{0xAA} })
		stale(id.String())
		var seen SessionRenewState
		st, ran, err := run(aID, id, time.Now().Add(-15*time.Minute), func(cur SessionRenewState) (SessionRenewWrite, error) {
			seen = cur
			return SessionRenewWrite{Action: SessionRenewRotate, RefreshCT: []byte{0xBB}, IDTokenCT: []byte{0xCC}}, nil
		})
		if err != nil || !ran {
			t.Fatalf("ran=%v err=%v; want fn called", ran, err)
		}
		if !bytes.Equal(seen.RefreshCT, []byte{0xAA}) || seen.Revoked || seen.Subject == "" {
			t.Fatalf("fn saw %+v; want the stored refresh_ct, live, with the user's subject", seen)
		}
		var subject string
		if err := h.super.QueryRow(ctx, `SELECT oidc_subject FROM app.users WHERE id = $1`, a.ID).Scan(&subject); err != nil || subject != seen.Subject {
			t.Fatalf("subject %q vs users.oidc_subject %q (%v)", seen.Subject, subject, err)
		}
		if time.Since(st.LastCheckedAt) < 50*time.Minute || st.Generation != 1 {
			t.Errorf("state after = %+v; want last_checked_at NOT stamped by a rotation, and generation 1", st)
		}
		var refresh, idt []byte
		var gen int64
		var age float64
		if err := h.super.QueryRow(ctx, `SELECT refresh_ct, id_token_ct, generation, extract(epoch FROM now() - last_checked_at) FROM app.sessions WHERE id = $1`, id.String()).Scan(&refresh, &idt, &gen, &age); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(refresh, []byte{0xBB}) || !bytes.Equal(idt, []byte{0xCC}) || gen != 1 || age < 3000 {
			t.Fatalf("row after = refresh %v id %v gen %d age %.0fs", refresh, idt, gen, age)
		}
	})

	t.Run("nil ciphertexts keep the stored ones", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-keep", nil) // refresh {1,2}, id {3,4}
		stale(id.String())
		if _, ran, err := run(aID, id, time.Now().Add(-15*time.Minute), func(SessionRenewState) (SessionRenewWrite, error) {
			return SessionRenewWrite{Action: SessionRenewRotate}, nil
		}); err != nil || !ran {
			t.Fatalf("ran=%v err=%v", ran, err)
		}
		var refresh, idt []byte
		if err := h.super.QueryRow(ctx, `SELECT refresh_ct, id_token_ct FROM app.sessions WHERE id = $1`, id.String()).Scan(&refresh, &idt); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(refresh, []byte{1, 2}) || !bytes.Equal(idt, []byte{3, 4}) {
			t.Fatalf("a nil write replaced the stored tokens: %v %v", refresh, idt)
		}
	})

	t.Run("an already fresh session does not run fn", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-fresh", nil) // last_checked_at = now
		st, ran, err := run(aID, id, time.Now().Add(-15*time.Minute), func(SessionRenewState) (SessionRenewWrite, error) {
			t.Error("fn was called for a session another process had just renewed")
			return SessionRenewWrite{}, nil
		})
		if err != nil || ran || st.Revoked || st.Generation != 0 {
			t.Fatalf("ran=%v state=%+v err=%v; want not run, live, untouched", ran, st, err)
		}
	})

	t.Run("a revoked session does not run fn", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-revoked", nil)
		stale(id.String())
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			_, e := tx.RevokeSession(ctx, id, SessionRevokedSignedOut)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		st, ran, err := run(aID, id, time.Now().Add(-15*time.Minute), func(SessionRenewState) (SessionRenewWrite, error) {
			t.Error("fn was called for a revoked session")
			return SessionRenewWrite{}, nil
		})
		if err != nil || ran || !st.Revoked {
			t.Fatalf("ran=%v state=%+v err=%v; want not run and revoked", ran, st, err)
		}
	})

	t.Run("refuse revokes as refused", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-refuse", nil)
		stale(id.String())
		st, ran, err := run(aID, id, time.Now().Add(-15*time.Minute), func(SessionRenewState) (SessionRenewWrite, error) {
			return SessionRenewWrite{Action: SessionRenewRefuse}, nil
		})
		if err != nil || !ran || !st.Revoked {
			t.Fatalf("ran=%v state=%+v err=%v", ran, st, err)
		}
		var reason *string
		if err := h.super.QueryRow(ctx, `SELECT revoked_reason FROM app.sessions WHERE id = $1`, id.String()).Scan(&reason); err != nil || reason == nil || *reason != SessionRevokedRefused {
			t.Fatalf("revoked_reason = %v, %v; want refused", reason, err)
		}
		if _, ok, err := s.LookupSession(ctx, hashOf("renew-refuse")); err != nil || ok {
			t.Fatalf("a refused session still resolves: ok=%v err=%v", ok, err)
		}
	})

	t.Run("leave writes nothing, and an error from fn rolls back and is returned", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-leave", nil)
		stale(id.String())
		for name, fn := range map[string]func(SessionRenewState) (SessionRenewWrite, error){
			"leave": func(SessionRenewState) (SessionRenewWrite, error) { return SessionRenewWrite{}, nil },
			"error": func(SessionRenewState) (SessionRenewWrite, error) {
				return SessionRenewWrite{Action: SessionRenewRotate, RefreshCT: []byte{9}}, errors.New("provider unreachable")
			},
		} {
			_, _, err := run(aID, id, time.Now().Add(-15*time.Minute), fn)
			if (name == "error") != (err != nil) {
				t.Fatalf("%s: err = %v", name, err)
			}
			var gen int64
			var refresh []byte
			var age float64
			if err := h.super.QueryRow(ctx, `SELECT generation, refresh_ct, extract(epoch FROM now() - last_checked_at) FROM app.sessions WHERE id = $1`, id.String()).Scan(&gen, &refresh, &age); err != nil {
				t.Fatal(err)
			}
			if gen != 0 || !bytes.Equal(refresh, []byte{1, 2}) || age < 3000 {
				t.Fatalf("%s changed the row: gen %d refresh %v age %.0fs", name, gen, refresh, age)
			}
		}
	})

	t.Run("another user's session is not renewable", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-iso", nil)
		stale(id.String())
		st, ran, err := run(bID, id, time.Now().Add(-15*time.Minute), func(SessionRenewState) (SessionRenewWrite, error) {
			t.Error("fn ran for a session of another user")
			return SessionRenewWrite{Action: SessionRenewRotate}, nil
		})
		if err != nil || ran || !st.Revoked {
			t.Fatalf("ran=%v state=%+v err=%v; want nothing live for B", ran, st, err)
		}
		var gen int64
		if err := h.super.QueryRow(ctx, `SELECT generation FROM app.sessions WHERE id = $1`, id.String()).Scan(&gen); err != nil || gen != 0 {
			t.Fatalf("A's session changed: gen %d %v", gen, err)
		}
	})

	t.Run("writes append no change_log row", func(t *testing.T) {
		id := createSession(t, s, aID, "renew-changelog", nil)
		stale(id.String())
		if _, _, err := run(aID, id, time.Now().Add(-15*time.Minute), func(SessionRenewState) (SessionRenewWrite, error) {
			return SessionRenewWrite{Action: SessionRenewRotate}, nil
		}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_id = $2`, a.ID, id.String()).Scan(&n); err != nil || n != 0 {
			t.Fatalf("change_log rows = %d, %v; want 0", n, err)
		}
	})
}

// The lock itself: concurrent renewals of one session run one at a time. A
// rotation does not stamp last_checked_at, so each waiter, once it holds the
// lock, still finds the row stale and runs its own fn, but it must see the
// generation the previous one left: eight callers see 0 through 7, never the
// same value twice. Without the lock (and the row lock) several read the same
// generation before any commits.
func TestRenewSessionLockedSerialisesConcurrentRenewals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	id := createSession(t, s, aID, "renew-race", nil)
	if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_checked_at = now() - interval '1 hour' WHERE id = $1`, id.String()); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var mu sync.Mutex
	var seen []int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := s.UserTx(ctx, aID, func(tx *UserTx) error {
				_, _, err := tx.RenewSessionLocked(ctx, id, time.Now().Add(-15*time.Minute),
					func(cur SessionRenewState) (SessionRenewWrite, error) {
						mu.Lock()
						seen = append(seen, cur.Generation)
						mu.Unlock()
						time.Sleep(100 * time.Millisecond) // the provider call
						return SessionRenewWrite{Action: SessionRenewRotate}, nil
					})
				return err
			})
			if err != nil {
				t.Errorf("renewal: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	got := map[int64]bool{}
	for _, g := range seen {
		got[g] = true
	}
	if len(seen) != n || len(got) != n {
		t.Fatalf("fn ran %d times and saw generations %v; want %d distinct generations (serialised)", len(seen), seen, n)
	}
	var gen int64
	if err := h.super.QueryRow(ctx, `SELECT generation FROM app.sessions WHERE id = $1`, id.String()).Scan(&gen); err != nil || gen != n {
		t.Fatalf("generation = %d, %v; want %d", gen, err, n)
	}
}
