// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// The store half of agent access (#49, ADR-042): token resolution through the
// definer, the CHECK constraints that back the API's validation, the account
// restriction's one predicate, the limit arithmetic, and the expiry sweep.

func newTestKey(t *testing.T, s *Store, user ulid.ULID, name, token string, mutate func(*NewAPIKey)) APIKey {
	t.Helper()
	n := NewAPIKey{
		Name: name, TokenHash: sha256.Sum256([]byte(token)), TokenHint: "abcdefg",
		Scopes: []string{"mail:read"}, RatePerMin: 60, DailyBudget: 500, SuspendOnAnomaly: true,
	}
	if mutate != nil {
		mutate(&n)
	}
	var k APIKey
	if err := s.UserTx(context.Background(), user, func(tx *UserTx) error {
		var err error
		k, err = tx.CreateAPIKey(context.Background(), n)
		return err
	}); err != nil {
		t.Fatalf("create key %s: %v", name, err)
	}
	return k
}

func TestAPIKeyTokenResolution(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	ka := newTestKey(t, s, aID, "agent", "mr_token-of-a", nil)
	kb := newTestKey(t, s, bID, "agent", "mr_token-of-b", nil)

	resolve := func(token string) (ulid.ULID, ulid.ULID, bool) {
		t.Helper()
		u, k, ok, err := s.ResolveAPIKeyToken(ctx, sha256.Sum256([]byte(token)))
		if err != nil {
			t.Fatalf("resolve %q: %v", token, err)
		}
		return u, k, ok
	}

	// Positive control first: without it every "not resolved" below would pass
	// against a definer that resolves nothing at all.
	if u, k, ok := resolve("mr_token-of-a"); !ok || u != aID || k != ka.ID {
		t.Fatalf("a live key did not resolve to its user and key: ok=%v user=%s key=%s", ok, u, k)
	}
	if u, k, ok := resolve("mr_token-of-b"); !ok || u != bID || k != kb.ID {
		t.Fatalf("B's live key did not resolve to B: ok=%v user=%s key=%s", ok, u, k)
	}
	if _, _, ok := resolve("mr_not-a-token"); ok {
		t.Fatal("an unknown token resolved")
	}

	// Suspended: refused, and resumable (so it is the suspension, not damage).
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error { _, err := tx.SuspendAPIKey(ctx, ka.ID, "test", "test"); return err }); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := resolve("mr_token-of-a"); ok {
		t.Fatal("a suspended key resolved")
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		k, _, err := tx.GetAPIKey(ctx, ka.ID)
		if err != nil {
			return err
		}
		k.SuspendedAt, k.SuspendedReason = nil, ""
		return tx.UpdateAPIKey(ctx, k)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := resolve("mr_token-of-a"); !ok {
		t.Fatal("a resumed key did not resolve")
	}

	// Revoked: refused for good; an update cannot bring it back.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		found, err := tx.RevokeAPIKey(ctx, ka.ID)
		if !found {
			t.Error("revoke did not find the key")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := resolve("mr_token-of-a"); ok {
		t.Fatal("a revoked key resolved")
	}
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		k, _, _ := tx.GetAPIKey(ctx, ka.ID)
		return tx.UpdateAPIKey(ctx, k)
	})
	if err != ErrAPIKeyNotFound {
		t.Fatalf("updating a revoked key: err = %v, want ErrAPIKeyNotFound", err)
	}

	// A user who can no longer sign in takes their keys with them.
	if _, err := h.super.Exec(ctx, `UPDATE app.users SET disabled_at = now() WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := resolve("mr_token-of-b"); ok {
		t.Fatal("the key of a disabled user resolved")
	}

	// Only the hash is stored: the row holds sha256(token) and no column holds
	// the token's text.
	var stored []byte
	var asText string
	if err := h.super.QueryRow(ctx, `SELECT token_hash, row_to_json(k)::text FROM app.api_keys k WHERE id = $1`,
		kb.ID.String()).Scan(&stored, &asText); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("mr_token-of-b"))
	if !slices.Equal(stored, want[:]) {
		t.Fatal("token_hash is not SHA-256 of the token")
	}
	if strings.Contains(asText, "token-of-b") {
		t.Fatalf("the token's text is stored in app.api_keys: %s", asText)
	}
}

func TestAPIKeyConstraintsRefuse(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	ctx := context.Background()

	try := func(name string, m func(*NewAPIKey)) error {
		n := NewAPIKey{Name: name, TokenHash: sha256.Sum256([]byte(name)), Scopes: []string{"mail:read"},
			RatePerMin: 60, DailyBudget: 500}
		m(&n)
		return s.UserTx(ctx, aID, func(tx *UserTx) error {
			_, err := tx.CreateAPIKey(ctx, n)
			return err
		})
	}
	// Positive control: the baseline row is accepted, so each refusal below is
	// the one field it changes.
	if err := try("ok", func(*NewAPIKey) {}); err != nil {
		t.Fatalf("baseline key refused: %v", err)
	}
	for name, m := range map[string]func(*NewAPIKey){
		"unknown scope":      func(n *NewAPIKey) { n.Scopes = []string{"mail:read", "admin:all"} },
		"no scope":           func(n *NewAPIKey) { n.Scopes = []string{} },
		"confirm non-destr.": func(n *NewAPIKey) { n.ConfirmScopes = []string{"mail:read"} },
		"rate zero":          func(n *NewAPIKey) { n.RatePerMin = 0 },
		"rate over cap":      func(n *NewAPIKey) { n.RatePerMin = 601 },
		"budget zero":        func(n *NewAPIKey) { n.DailyBudget = 0 },
		"budget over cap":    func(n *NewAPIKey) { n.DailyBudget = 20001 },
	} {
		if err := try(name, m); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	if err := try("ok", func(n *NewAPIKey) { n.TokenHash = sha256.Sum256([]byte("other")) }); err != ErrAPIKeyNameTaken {
		t.Errorf("duplicate name: err = %v, want ErrAPIKeyNameTaken", err)
	}
}

func TestAPIKeyAccountRestrictionPredicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct1 := mustULID(t, a.AccountID)
	m1 := mustULID(t, a.MessageID)

	// A second account of A's with one message in it, and a local-only draft
	// shown in that account's mailbox (no provenance yet).
	acct2, mbox2, m2, draft := NewULID(), NewULID(), NewULID(), NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'second', 'mail.example.com', 'smtp.example.com', $3, $4)`,
		acct2.String(), a.ID, []byte{1}, NewULID().String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	      VALUES ($1, $2, $3, 'INBOX', 'Inbox')`, mbox2.String(), a.ID, acct2.String())
	for i, id := range []ulid.ULID{m2, draft} {
		hash := sha256.Sum256([]byte("restrict-" + id.String()))
		exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		      VALUES ($1, $2, 1, 1, $3)`, a.ID, hash[:], NewULID().String())
		exec(`INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes, local_mailbox_id)
		      VALUES ($1, $2, $3, 'second account', now(), 1, $4)`,
			id.String(), a.ID, hash[:], map[int]any{0: nil, 1: mbox2.String()}[i])
	}
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
	      VALUES ($1, $2, $3, $4, $5, 7, 1)`, NewULID().String(), a.ID, m2.String(), acct2.String(), mbox2.String())

	bMsg := mustULID(t, b.MessageID)
	var in1, in2, inBoth map[ulid.ULID]bool
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		ids := []ulid.ULID{m1, m2, draft, bMsg}
		if in1, err = tx.MessagesInAccounts(ctx, ids, []ulid.ULID{acct1}); err != nil {
			return err
		}
		if in2, err = tx.MessagesInAccounts(ctx, ids, []ulid.ULID{acct2}); err != nil {
			return err
		}
		inBoth, err = tx.MessagesInAccounts(ctx, ids, []ulid.ULID{acct1, acct2})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !in1[m1] || in1[m2] || in1[draft] || len(in1) != 1 {
		t.Errorf("restricted to account 1: %v, want only %s", in1, m1)
	}
	if in2[m1] || !in2[m2] || !in2[draft] || len(in2) != 2 {
		t.Errorf("restricted to account 2: %v, want %s and the local draft %s", in2, m2, draft)
	}
	if len(inBoth) != 3 || inBoth[bMsg] {
		t.Errorf("both accounts: %v, want A's three messages and never B's", inBoth)
	}

	query := func(restrict []ulid.ULID) []ulid.ULID {
		t.Helper()
		var ids []ulid.ULID
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var err error
			ids, _, err = tx.QueryEmails(ctx, EmailQuery{RestrictAccounts: restrict, Limit: 50, LimitSet: true})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	// Positive control: unrestricted, the query finds mail in both accounts.
	all := query(nil)
	if !slices.Contains(all, m1) || !slices.Contains(all, m2) {
		t.Fatalf("unrestricted query: %v, want both %s and %s", all, m1, m2)
	}
	if got := query([]ulid.ULID{acct1}); !slices.Contains(got, m1) || slices.Contains(got, m2) || slices.Contains(got, draft) {
		t.Errorf("query restricted to account 1 returned %v", got)
	}
	if got := query([]ulid.ULID{acct2}); slices.Contains(got, m1) || !slices.Contains(got, m2) {
		t.Errorf("query restricted to account 2 returned %v", got)
	}
	if got := query([]ulid.ULID{}); len(got) != 0 {
		t.Errorf("a non-nil empty restriction matched %v, want nothing", got)
	}
}

func TestAPIKeyUsageCountsOnlyAdmittedCalls(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	k := newTestKey(t, s, aID, "usage", "mr_usage", nil)

	var u APIKeyUsage
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for _, e := range []APIKeyEvent{
			{Outcome: KeyOutcomeOK, Cost: 3, Deletes: 2},
			{Outcome: KeyOutcomePending, Cost: 1},
			{Outcome: KeyOutcomeApproved, Cost: 1, Sends: 1},
			// Refusals are written at cost 1 (#49 review): they spend the
			// minute, never the day, and their deletes count for nothing.
			{Outcome: KeyOutcomeDenied, Cost: 1, Deletes: 50},
			{Outcome: KeyOutcomeRateLimited, Cost: 1},
			{Outcome: KeyOutcomeOverBudget, Cost: 1},
		} {
			e.KeyID, e.Method = k.ID, "Email/set"
			if _, err := tx.InsertAPIKeyEvent(ctx, e); err != nil {
				return err
			}
		}
		// An admitted call from two hours ago: in the day, out of the minute.
		if _, err := tx.tx.Exec(ctx, `INSERT INTO app.api_key_events (id, user_id, key_id, at, method, outcome, cost, deletes)
		      VALUES ($1, $2, $3, now() - interval '2 hours', 'Email/set', 'ok', 7, 7)`,
			NewULID().String(), a.ID, k.ID.String()); err != nil {
			return err
		}
		var err error
		u, err = tx.APIKeyUsageAt(ctx, k.ID, time.Now(), 10*time.Minute, 10*time.Minute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if u.CostLastMinute != 8 || u.CostLast24h != 12 || u.DeletesInWindow != 2 || u.SendsInWindow != 1 || u.RoutineRefusalsLastMinute != 3 || u.CriticalRefusalsLastMinute != 0 {
		t.Fatalf("usage = %+v, want minute 8 (refusals included), day 12, deletes 2, sends 1, 3 routine refusals", u)
	}
}

func TestAPIKeyPendingExpirySweep(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	ka := newTestKey(t, s, aID, "pend", "mr_pend_a", nil)
	kb := newTestKey(t, s, bID, "pend", "mr_pend_b", nil)

	var stale, fresh ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if stale, err = tx.CreateAPIKeyPending(ctx, APIKeyPending{KeyID: ka.ID, Kind: "delete", Method: "Email/set",
			Payload: []byte(`{"destroy":[]}`), Summary: "s", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
			return err
		}
		fresh, err = tx.CreateAPIKeyPending(ctx, APIKeyPending{KeyID: ka.ID, Kind: "send", Method: "EmailSubmission/set",
			Payload: []byte(`{}`), Summary: "f", ExpiresAt: time.Now().Add(time.Hour)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		_, err := tx.CreateAPIKeyPending(ctx, APIKeyPending{KeyID: kb.ID, Kind: "delete", Method: "Email/set",
			Payload: []byte(`{}`), Summary: "b", ExpiresAt: time.Now().Add(time.Hour)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	due, err := s.UsersWithDuePending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(due, aID) || slices.Contains(due, bID) {
		t.Fatalf("due users = %v, want A (one stale request) and not B (none)", due)
	}
	var expired []APIKeyPending
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		expired, err = tx.ExpireAPIKeyPending(ctx, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != stale || expired[0].State != PendingStateExpired {
		t.Fatalf("expired = %+v, want exactly the stale request, now expired", expired)
	}
	due, err = s.UsersWithDuePending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(due, aID) {
		t.Fatal("A is still due after the sweep")
	}
	// A decided request cannot be decided again; a fresh one can.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if ok, err := tx.DecideAPIKeyPending(ctx, stale, PendingStateApproved, ""); err != nil || ok {
			t.Errorf("approving an expired request: ok=%v err=%v, want refused", ok, err)
		}
		if ok, err := tx.DecideAPIKeyPending(ctx, fresh, PendingStateDenied, ""); err != nil || !ok {
			t.Errorf("denying a fresh request: ok=%v err=%v, want done", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// One approval is one run (#49 review, F5): MarkAPIKeyPendingRan reports
// whether it actually marked the request, and refuses a request that is not
// approved or has already run.
//
// RED PROBE: return true unconditionally from MarkAPIKeyPendingRan and the
// second call and the pending request both fail.
func TestMarkAPIKeyPendingRanOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	k := newTestKey(t, s, aID, "once", "mr_once_a", nil)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		mk := func(summary string) ulid.ULID {
			id, err := tx.CreateAPIKeyPending(ctx, APIKeyPending{KeyID: k.ID, Kind: "delete", Method: "Email/set",
				Payload: []byte(`{"destroy":[]}`), Summary: summary, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		approved, pending := mk("approved"), mk("pending")
		if ok, err := tx.DecideAPIKeyPending(ctx, approved, PendingStateApproved, ""); err != nil || !ok {
			t.Fatalf("approve: ok=%v err=%v", ok, err)
		}
		if ran, err := tx.MarkAPIKeyPendingRan(ctx, approved); err != nil || !ran {
			t.Errorf("first run of an approved request: ran=%v err=%v, want true", ran, err)
		}
		if ran, err := tx.MarkAPIKeyPendingRan(ctx, approved); err != nil || ran {
			t.Errorf("second run of the same approval: ran=%v err=%v, want false", ran, err)
		}
		if ran, err := tx.MarkAPIKeyPendingRan(ctx, pending); err != nil || ran {
			t.Errorf("run of a request nobody approved: ran=%v err=%v, want false", ran, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
