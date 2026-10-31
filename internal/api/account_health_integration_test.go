// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration test for Account/getHealth (mailroom#43): the computed
// account-health panel read. It asserts the method folds four independent
// signals into one per-account entry — persisted sync status, recent delivery
// failures, dead-lettered writeback intents, and the outbound send quota — and
// (in rls_integration_test.go) that it never crosses users.

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/search"
	"mailroom/internal/store"
)

func TestAccountGetHealth(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	f := seedAPIUser(t, h.super, "gethealth-"+store.NewULID().String())

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// A verified identity, a submission on the account, and a FAILED DSN linked to
	// it — the delivery-failure signal AccountDeliveryFailures resolves.
	identityID := store.NewULID().String()
	exec(`INSERT INTO app.identities (id, user_id, account_id, kind, address)
	      VALUES ($1,$2,$3,'fixed',$4)`,
		identityID, f.UserID.String(), f.AccountID.String(), "me@example.com")
	submissionID := store.NewULID().String()
	exec(`INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id,
	        from_address, envelope_to, state)
	      VALUES ($1,$2,$3,$4,$5,$6,$7::citext[],'sent')`,
		submissionID, f.UserID.String(), f.AccountID.String(), identityID, f.MessageID.String(),
		"me@example.com", []string{"bounce@example.net"})
	exec(`INSERT INTO app.dsn_events (id, user_id, origin_submission_id, recipient, action,
	        status_code, diagnostic)
	      VALUES ($1,$2,$3,$4,'failed','5.1.1','no such user')`,
		store.NewULID().String(), f.UserID.String(), submissionID, "bounce@example.net")

	// A dead-lettered writeback intent — the dead-letter signal ListDeadIntents
	// surfaces, with its reason.
	exec(`INSERT INTO app.writeback_intents (id, user_id, message_id, account_id, op,
	        payload, idempotency_key, state, dead_reason)
	      VALUES ($1,$2,$3,$4,'move','{}'::jsonb,$5,'dead','archive role is missing')`,
		store.NewULID().String(), f.UserID.String(), f.MessageID.String(), f.AccountID.String(),
		"wb-dead-"+store.NewULID().String())

	// Today's outbound spend — the send-quota signal OutboundUsage reports against
	// the account's default cap (100).
	exec(`INSERT INTO app.outbound_ledger (account_id, user_id, day, sent_count)
	      VALUES ($1,$2,CURRENT_DATE,3)`, f.AccountID.String(), f.UserID.String())

	// Persist a real sync-health lifecycle through the store: a failure, then a
	// success one hour ago. getHealth must surface the RESET state (no error,
	// consecutiveFailures 0) and a lag of ~1h.
	success := time.Now().Add(-time.Hour)
	if err := h.store.UserTx(ctx, f.UserID, func(tx *store.UserTx) error {
		if err := tx.RecordSyncFailure(ctx, f.AccountID, "earlier blip", success.Add(-time.Minute)); err != nil {
			return err
		}
		return tx.RecordSyncSuccess(ctx, f.AccountID, success)
	}); err != nil {
		t.Fatalf("seed sync health: %v", err)
	}

	// ---- the call ----------------------------------------------------------
	res := call(t, h.api.accountGetHealth, f.UserID, map[string]any{})
	if res["accountId"] != f.UserID.String() {
		t.Errorf("accountId = %v, want %s", res["accountId"], f.UserID)
	}
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	entry := list[0].(map[string]any)
	if entry["id"] != f.AccountID.String() {
		t.Errorf("entry id = %v, want %s", entry["id"], f.AccountID)
	}

	// syncStatus: reset after the success, lag ~= 3600s.
	sync := entry["syncStatus"].(map[string]any)
	if sync["consecutiveFailures"].(float64) != 0 {
		t.Errorf("consecutiveFailures = %v, want 0", sync["consecutiveFailures"])
	}
	if sync["lastError"] != nil {
		t.Errorf("lastError = %v, want null after a success", sync["lastError"])
	}
	if sync["lastSuccessAt"] == nil {
		t.Error("lastSuccessAt is null, want a timestamp")
	}
	lag, ok := sync["lagSeconds"].(float64)
	if !ok {
		t.Fatalf("lagSeconds = %v, want a number", sync["lagSeconds"])
	}
	if lag < 3400 || lag > 3800 {
		t.Errorf("lagSeconds = %v, want ~3600", lag)
	}

	// deliveryFailures: the one failed DSN.
	df := entry["deliveryFailures"].(map[string]any)
	if df["count"].(float64) != 1 {
		t.Errorf("deliveryFailures.count = %v, want 1", df["count"])
	}
	recent := df["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("deliveryFailures.recent len = %d, want 1", len(recent))
	}
	if r0 := recent[0].(map[string]any); r0["recipient"] != "bounce@example.net" || r0["statusCode"] != "5.1.1" {
		t.Errorf("recent[0] = %v, want the seeded bounce", r0)
	}

	// deadLetters: the one dead intent, with its reason surfaced.
	dl := entry["deadLetters"].(map[string]any)
	if dl["count"].(float64) != 1 {
		t.Errorf("deadLetters.count = %v, want 1", dl["count"])
	}
	reasons := dl["reasons"].([]any)
	if len(reasons) != 1 || reasons[0] != "archive role is missing" {
		t.Errorf("deadLetters.reasons = %v, want [\"archive role is missing\"]", reasons)
	}

	// sendQuota: 3 of the default 100.
	sq := entry["sendQuota"].(map[string]any)
	if sq["sent"].(float64) != 3 || sq["limit"].(float64) != 100 {
		t.Errorf("sendQuota = %v, want sent=3 limit=100", sq)
	}

	// This harness never calls SetSearch: no search backend is installed, so the
	// envelope must OMIT searchStalenessSeconds entirely rather than claim an
	// index that does not exist is up to date (mailroom#41).
	if _, ok := res["searchStalenessSeconds"]; ok {
		t.Errorf("searchStalenessSeconds = %v, want the key absent (no search service installed)", res["searchStalenessSeconds"])
	}
}

// With a search service installed, Account/getHealth's envelope carries
// searchStalenessSeconds — the per-USER (not per-account) signal spec §7 calls
// "search may be N minutes stale" (mailroom#41).
func TestAccountGetHealthSearchStaleness(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "gethealth-search-"+store.NewULID().String())

	ix, err := search.NewIndexer(t.TempDir(), search.MemoryFactory, search.NewStoreSource(h.store), nil)
	if err != nil {
		t.Fatalf("search indexer: %v", err)
	}
	h.api.SetSearch(search.NewService(ix, search.NewStoreResolver(h.store)))

	res := call(t, h.api.accountGetHealth, f.UserID, map[string]any{})

	secs, ok := res["searchStalenessSeconds"]
	if !ok {
		t.Fatal("searchStalenessSeconds absent, want present when a search service is installed")
	}
	staleness, ok := secs.(float64)
	if !ok {
		t.Fatalf("searchStalenessSeconds = %v (%T), want a number", secs, secs)
	}
	if staleness < 0 {
		t.Errorf("searchStalenessSeconds = %v, want >= 0", staleness)
	}
}
