// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Disabling a user through the real definer (app.set_user_disabled), end to end.
//
// The per-entry-point refusals were already pinned one at a time, by flipping
// disabled_at in SQL: TestSessionLookup (sessions_integration_test.go),
// TestAPIKeyTokenResolution (api_keys_integration_test.go) and
// TestOAuthAccessLookupPredicates (oauth_integration_test.go). What those could
// not show is that the SUPPORTED way of disabling (the function the operator CLI
// calls) reaches every one of them, that enabling restores each one, that the
// job dispatcher leaves the user's jobs alone, and that a refused refresh token
// is not used up. That is this file.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestDisableUserReachesEveryEntryPointAndEnableRestoresThem(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	// A has a session, an API key, an OAuth grant with an access and a refresh
	// token, and a due job. B has a due job and is never touched: it is the
	// control that the refusals are about A, not about everything.
	createSession(t, s, aID, "dis-secret-a", nil)
	newTestKey(t, s, aID, "agent", "mr_dis-token-a", nil)
	grant := oaGrant(t, s, aID, "https://app.example.com/dis.json")
	fam := NewULID()
	oaToken(t, s, aID, grant, OAuthTokenAccess, "mra_dis_access", fam, time.Hour)
	oaToken(t, s, aID, grant, OAuthTokenRefresh, "mrr_dis_refresh", fam, time.Hour)
	jobA, jobB := ulid.Make(), ulid.Make()
	for _, j := range []struct {
		id   ulid.ULID
		user string
	}{{jobA, a.ID}, {jobB, b.ID}} {
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
			VALUES ($1, $2, 'purge_trash', now() - interval '1 minute', '{}'::jsonb, $3)`,
			j.id.String(), j.user, "dis-"+j.id.String()); err != nil {
			t.Fatal(err)
		}
	}
	// Claimed jobs go to 'running', so each phase below looks at what ONE dequeue
	// returns, and a job that was skipped must still be pending afterwards.
	dequeued := func() []ulid.ULID {
		t.Helper()
		var ids []ulid.ULID
		if err := s.SystemTx(ctx, func(tx *SystemTx) error {
			js, err := tx.DequeueDueJobs(ctx, "dis-worker", 5000)
			for _, j := range js {
				ids = append(ids, j.ID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	dueCount := func() int64 {
		t.Helper()
		var n int64
		if err := s.SystemTx(ctx, func(tx *SystemTx) error {
			return tx.tx.QueryRow(ctx, `SELECT app.count_due_jobs()`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	jobState := func(id ulid.ULID) string {
		t.Helper()
		var st string
		if err := h.super.QueryRow(ctx, `SELECT state::text FROM app.jobs WHERE id = $1`, id.String()).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	type probe struct {
		session, key, access, refreshOK, signIn, active bool
	}
	probeA := func(consumeRefresh bool) probe {
		t.Helper()
		var p probe
		_, p.session = lookupSessionOK(t, s, "dis-secret-a")
		_, _, p.key, _ = s.ResolveAPIKeyToken(ctx, hashOf("mr_dis-token-a"))
		_, _, p.access, _ = s.ResolveOAuthAccessToken(ctx, oaSum("mra_dis_access"))
		if consumeRefresh {
			r, found, err := s.ConsumeOAuthRefresh(ctx, oaSum("mrr_dis_refresh"))
			if err != nil || !found {
				t.Fatalf("consume refresh: found=%v err=%v", found, err)
			}
			p.refreshOK = r.UserOK && !r.Revoked && !r.Reuse
		}
		_, _, p.signIn, _ = s.ResolveLogin(ctx, "oidc-"+a.Tag, a.Tag+"@example.com", "User "+a.Tag, false)
		ids, err := s.ListActiveUserIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p.active = slices.Contains(ids, aID)
		return p
	}

	// POSITIVE CONTROL. Without it every refusal below would pass against a
	// definer that refuses everything.
	if got := probeA(false); got != (probe{true, true, true, false, true, true}) {
		t.Fatalf("before disabling, A should be fully usable: %+v", got)
	}
	dueBefore := dueCount()

	// DISABLE (by email, to exercise that lookup too).
	res, err := s.SetUserDisabled(ctx, a.Tag+"@example.com", true)
	if err != nil || res.Outcome != UserOutcomeDisabled || res.ID != aID || res.Email != a.Tag+"@example.com" {
		t.Fatalf("disable: %+v %v", res, err)
	}
	// Twice is safe and says so.
	res, err = s.SetUserDisabled(ctx, a.ID, true)
	if err != nil || res.Outcome != UserOutcomeAlreadyDisabled || res.ID != aID {
		t.Fatalf("second disable: %+v %v", res, err)
	}

	if got := probeA(true); got != (probe{}) {
		t.Fatalf("a disabled user must be refused everywhere (session, key, access token, refresh token, sign-in, fan-out): %+v", got)
	}
	// The refusal did not use the refresh token up: a token burned while the user
	// was disabled would read as a REUSE after enabling and end the grant family.
	var used *time.Time
	if err := h.super.QueryRow(ctx, `SELECT used_at FROM app.oauth_tokens WHERE token_hash = $1`, sumB("mrr_dis_refresh")).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != nil {
		t.Fatal("refusing a refresh token for a disabled user marked it used")
	}
	// Nothing was deleted: the session, key, grant and tokens are all still rows.
	for table, want := range map[string]int{"sessions": 1, "api_keys": 2, "oauth_tokens": 2} {
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.`+table+` WHERE user_id = $1`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n < want {
			t.Errorf("disabling deleted rows: app.%s has %d for A, want at least %d", table, n, want)
		}
	}

	// The dispatcher skips A's due job and still hands out B's.
	got := dequeued()
	if slices.Contains(got, jobA) {
		t.Fatal("the dispatcher handed out a disabled user's job")
	}
	if !slices.Contains(got, jobB) {
		t.Fatal("control: the dispatcher did not hand out the enabled user's job")
	}
	if st := jobState(jobA); st != "pending" {
		t.Fatalf("a disabled user's job is %q, want it left pending", st)
	}
	// And the backlog gauge no longer counts it (B's was claimed, so the delta is
	// exactly what the two jobs and the skipped one explain: A's job is not counted).
	if n := dueCount(); n >= dueBefore {
		t.Errorf("count_due_jobs = %d after disabling, before = %d; a disabled user's job is still counted", n, dueBefore)
	}

	// RLS still isolates: B, in B's own transaction, sees one users row, B's.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		var n int
		if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.users`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("B sees %d users rows, want 1", n)
		}
		if ok, err := tx.SelfEnabled(ctx); err != nil || !ok {
			t.Errorf("B SelfEnabled = %v, %v; want true", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		ok, err := tx.SelfEnabled(ctx)
		if err != nil || ok {
			t.Errorf("a disabled user's SelfEnabled = %v, %v; want false", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// ENABLE restores every one of them, and the waiting job runs.
	res, err = s.SetUserDisabled(ctx, a.ID, false)
	if err != nil || res.Outcome != UserOutcomeEnabled {
		t.Fatalf("enable: %+v %v", res, err)
	}
	res, err = s.SetUserDisabled(ctx, a.ID, false)
	if err != nil || res.Outcome != UserOutcomeAlreadyEnabled {
		t.Fatalf("second enable: %+v %v", res, err)
	}
	if got := probeA(true); got != (probe{true, true, true, true, true, true}) {
		t.Fatalf("enabling must restore session, key, access token, refresh token, sign-in and fan-out: %+v", got)
	}
	if !slices.Contains(dequeued(), jobA) {
		t.Fatal("after enabling, the user's waiting job was not handed out")
	}

	// An unknown user is not found, and nothing is invented.
	res, err = s.SetUserDisabled(ctx, "nobody@example.com", true)
	if err != nil || res.Outcome != UserOutcomeNotFound {
		t.Fatalf("unknown user: %+v %v", res, err)
	}
}

func lookupSessionOK(t *testing.T, s *Store, secret string) (SessionRef, bool) {
	t.Helper()
	ref, ok, err := s.LookupSession(context.Background(), hashOf(secret))
	if err != nil {
		t.Fatal(err)
	}
	return ref, ok
}

// Disabling the last enabled user is refused, and nothing changes.
func TestDisableRefusesTheLastEnabledUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()

	// Make A the only enabled user for the length of this test (the database is
	// shared with every other test, and they leave users behind).
	rows, err := h.super.Query(ctx, `SELECT id FROM app.users WHERE id <> $1 AND disabled_at IS NULL AND deleted_at IS NULL`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var others []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		others = append(others, id)
	}
	rows.Close()
	if _, err := h.super.Exec(ctx, `UPDATE app.users SET disabled_at = now() WHERE id = ANY($1)`, others); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.super.Exec(context.Background(), `UPDATE app.users SET disabled_at = NULL WHERE id = ANY($1)`, others)
	})

	res, err := s.SetUserDisabled(ctx, a.ID, true)
	if err != nil || res.Outcome != UserOutcomeLastEnabled {
		t.Fatalf("disabling the last enabled user: %+v %v, want %s", res, err, UserOutcomeLastEnabled)
	}
	var disabled bool
	if err := h.super.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM app.users WHERE id = $1`, a.ID).Scan(&disabled); err != nil || disabled {
		t.Fatalf("the last user was disabled anyway (disabled=%v err=%v)", disabled, err)
	}

	// With another enabled user it goes through (control: the refusal is the
	// last-user rule and not a function that refuses everything).
	if _, err := h.super.Exec(ctx, `UPDATE app.users SET disabled_at = NULL WHERE id = $1`, others[0]); err != nil {
		t.Fatal(err)
	}
	res, err = s.SetUserDisabled(ctx, a.ID, true)
	if err != nil || res.Outcome != UserOutcomeDisabled {
		t.Fatalf("with another enabled user: %+v %v", res, err)
	}
	if _, err := s.SetUserDisabled(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
}
