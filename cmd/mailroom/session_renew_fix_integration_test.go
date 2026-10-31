// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// The fix round for the security review of renewal (#937): a half-checked
// identity never demotes, the rotated token survives a later failure, and a
// burst of stale sessions cannot exhaust a small connection pool.

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mailroom/internal/store"
)

// An admin whose ID token carries no groups (the provider keeps them on
// UserInfo) must NOT be demoted by a renewal that could not reach UserInfo: the
// role stays, the session is not confirmed, and the rotated token is stored.
// When UserInfo answers, the role follows it, including to "no groups".
func TestSessionRenewal_UnreachableUserInfoNeverDemotes(t *testing.T) {
	rig := newRenewRig(t, true, []string{adminGroup})
	rig.idp.set(func(p *fakeIDP) { p.idTokenNoGroups = true; p.userinfoFail = true })
	if rig.role() != "admin" {
		t.Fatalf("seeded role = %q, want admin", rig.role())
	}
	rig.age(time.Hour)

	if code := rig.request(rig.process()); code != http.StatusOK {
		t.Fatalf("status %d, want 200 (served within the outage allowance)", code)
	}
	if got := rig.role(); got != "admin" {
		t.Fatalf("role after a renewal that could not read UserInfo = %q, want admin (unchanged)", got)
	}
	row := rig.row()
	if row.revoked != nil {
		t.Fatalf("session revoked as %q", *row.revoked)
	}
	if row.checkedAgo < 50*time.Minute {
		t.Errorf("the half-checked renewal was stamped confirmed (%v ago)", row.checkedAgo)
	}
	if row.generation != 1 {
		t.Errorf("generation = %d, want 1: the rotation must be stored even though identity was not re-checked", row.generation)
	}

	// UserInfo recovers and says: no groups. A new process has no retry spacing.
	rig.idp.set(func(p *fakeIDP) { p.userinfoFail = false; p.groups = []string{} })
	if code := rig.request(rig.process()); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if got := rig.role(); got != "user" {
		t.Fatalf("role after UserInfo answered with no groups = %q, want user", got)
	}
	if row := rig.row(); row.checkedAgo > time.Minute || row.revoked != nil {
		t.Errorf("complete renewal: checked %v ago, revoked %v", row.checkedAgo, row.revoked)
	}
	// The second attempt presented the token the first one rotated.
	if uses, violations := rig.idp.stats(); uses != 2 || violations != 0 {
		t.Fatalf("provider saw %d uses and %d re-presentations; want 2 and 0", uses, violations)
	}
}

// With a two-connection pool, a burst of stale sessions must finish. Renewal
// used to hold a connection across the provider call while its resolver needed a
// second; now the resolver runs outside the transaction and the renewals in
// flight are bounded.
func TestSessionRenewal_SmallPoolDoesNotDeadlock(t *testing.T) {
	rig := newRenewRig(t, false, nil)
	rig.idp.delay = 300 * time.Millisecond

	pcfg, err := pgxpool.ParseConfig(os.Getenv("MAILROOM_TEST_APP_URL"))
	if err != nil {
		t.Fatal(err)
	}
	pcfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rig.st = store.NewFromPool(pool)

	secrets := []string{rig.secret, rig.addSession("rt-two"), rig.addSession("rt-three")}
	rig.age(time.Hour)
	if _, err := rig.h.super.Exec(context.Background(),
		`UPDATE app.sessions SET last_checked_at = now() - interval '1 hour' WHERE user_id = $1`, rig.userID.String()); err != nil {
		t.Fatal(err)
	}
	proc := rig.process()

	done := make(chan struct{})
	codes := make([]int, len(secrets))
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i, sec := range secrets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = rig.requestWith(proc, sec)
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a burst of stale sessions did not finish on a 2-connection pool")
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("session %d answered %d, want 200", i, c)
		}
	}
	// Those turned away by the concurrency bound renew on their next request.
	for _, sec := range secrets {
		if c := rig.requestWith(proc, sec); c != http.StatusOK {
			t.Errorf("second round answered %d, want 200", c)
		}
	}
	if uses, violations := rig.idp.stats(); uses != len(secrets) || violations != 0 {
		t.Fatalf("provider saw %d uses and %d re-presentations; want %d and 0", uses, violations, len(secrets))
	}
}
