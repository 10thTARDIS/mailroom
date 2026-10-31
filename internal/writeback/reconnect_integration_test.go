// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package writeback

// #52, ADR-046 §5 (decided 2026-10-27): while an OAuth account's
// grant needs its user to reconnect, its writeback intents are HELD — no
// connection is opened, nothing is dead-lettered, no attempt is charged — and
// they drain normally once the grant is active again. The positive half (the
// same queue on an active grant DOES dial, and then applies) is what keeps the
// negative half from passing on a runner that simply refused everyone.

import (
	"context"
	"errors"
	"testing"

	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
)

func (f *liveFixture) makeOAuth(t *testing.T, state string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.super.Exec(ctx, `UPDATE app.accounts SET auth_kind = 'oauth2', provider_profile = 'microsoft'
		WHERE user_id = $1 AND id = $2`, f.userID.String(), f.account.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.super.Exec(ctx, `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, state)
		VALUES ($1, $2, 'microsoft', 'oid-1', 'you@example.com', $3)`,
		f.account.String(), f.userID.String(), state); err != nil {
		t.Fatal(err)
	}
}

func (f *liveFixture) setGrantState(t *testing.T, state string) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(), `UPDATE app.oauth_grants SET state = $3
		WHERE user_id = $1 AND account_id = $2`, f.userID.String(), f.account.String(), state); err != nil {
		t.Fatal(err)
	}
}

func (f *liveFixture) maxAttempts(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT coalesce(max(attempts), 0) FROM app.writeback_intents WHERE user_id = $1 AND account_id = $2`,
		f.userID.String(), f.account.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntentsHeldWhileAwaitingReconnectThenDrain(t *testing.T) {
	f := newLiveFixture(t)
	f.makeOAuth(t, store.OAuthStateNeedsReauth)
	msg := f.seedMessage(t, "flagged while the grant was dead")
	f.seedLiveCopy(t, msg, f.inbox, 1)
	id := f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "held")

	// Held: several drains, as several passes and nudges would make. Not one
	// dials (exploderDial fails the test if it is called).
	r := NewRunner(f.st, exploderDial(t), nil, nil)
	for i := 0; i < 3; i++ {
		res, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{})
		if err != nil {
			t.Fatalf("DrainAccount: %v", err)
		}
		if res != (Result{}) {
			t.Fatalf("a held drain did work: %+v", res)
		}
	}
	if got := f.intentState(t, id); got != "pending" {
		t.Fatalf("held intent state = %q, want pending (not dead, not suppressed)", got)
	}
	if n := f.maxAttempts(t); n != 0 {
		t.Fatalf("attempts = %d, want 0: waiting for a reconnect must not spend the retry budget", n)
	}

	// Disconnected (no grant) is held the same way: still no dial.
	if _, err := f.super.Exec(context.Background(), `DELETE FROM app.oauth_grants WHERE account_id = $1`, f.account.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{}); err != nil {
		t.Fatalf("disconnected DrainAccount: %v", err)
	}
	if got := f.intentState(t, id); got != "pending" || f.maxAttempts(t) != 0 {
		t.Fatalf("disconnected: intent %q, attempts %d", got, f.maxAttempts(t))
	}
	if _, err := f.super.Exec(context.Background(), `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, state)
		VALUES ($1, $2, 'microsoft', 'oid-1', 'you@example.com', 'needs_reauth')`, f.account.String(), f.userID.String()); err != nil {
		t.Fatal(err)
	}

	// operator_error is not held: the account keeps trying on the normal
	// backoff, so the dial is attempted.
	f.setGrantState(t, store.OAuthStateOperatorError)
	dialed := 0
	failing := func(context.Context, imapsync.Config) (imapsync.SyncSession, error) {
		dialed++
		return nil, errors.New("dial attempted")
	}
	if _, err := NewRunner(f.st, failing, nil, nil).DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{}); err == nil || dialed != 1 {
		t.Fatalf("operator_error drain: dialed %d, err %v; want one attempted dial", dialed, err)
	}

	// Reconnected: the same queue now dials, and drains.
	f.setGrantState(t, store.OAuthStateActive)
	dialed = 0
	if _, err := NewRunner(f.st, failing, nil, nil).DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{}); err == nil || dialed != 1 {
		t.Fatalf("after reconnect: dialed %d, err %v; want one attempted dial", dialed, err)
	}
	w := &fakeWriter{}
	res := f.drain(t, w)
	if res.Applied != 1 || f.intentState(t, id) != "applied" {
		t.Fatalf("after reconnect the held intent did not apply: %+v, state %q", res, f.intentState(t, id))
	}
}
