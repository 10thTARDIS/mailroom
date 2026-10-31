// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Store-layer integration tests for per-account sync-health telemetry
// (mailroom#43). Proves the failure->success lifecycle: RecordSyncFailure
// increments the consecutive-failure counter and records the error while
// LEAVING last_success_at untouched, and RecordSyncSuccess then resets the
// counter to zero, clears the error, and stamps the success. Runs against the
// throwaway Postgres from scripts/integration-test.sh; skips when absent.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestAccountSyncHealthLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := seedUser(t, h.super, "health-"+NewULID().String())
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)

	// seedUser plants a baseline status row; start from a clean slate so this test
	// exercises the insert branch of the upserts and the never-synced state.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.account_sync_status WHERE user_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}

	// Microsecond precision matches Postgres timestamptz, so a round-trip Equal
	// is exact.
	t1 := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	t2 := t1.Add(time.Minute)
	t3 := time.Now().UTC().Truncate(time.Microsecond)

	readHealth := func() AccountHealth {
		t.Helper()
		var got AccountHealth
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			m, err := tx.AccountHealth(ctx, []ulid.ULID{acctID})
			if err != nil {
				return err
			}
			got = m[acctID]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// No status row yet: AccountHealth returns no entry (rendered as never-synced).
	if got := readHealth(); got.ConsecutiveFailures != 0 || got.LastAttemptAt != nil {
		t.Fatalf("before any pass: got %+v, want zero-value/never-synced", got)
	}

	// Two failures: the counter reaches 2, last_error is the latest, and there is
	// still no success (last_success_at stays NULL).
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.RecordSyncFailure(ctx, acctID, "dial timeout", t1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.RecordSyncFailure(ctx, acctID, "login refused", t2)
	}); err != nil {
		t.Fatal(err)
	}

	got := readHealth()
	if got.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures = %d, want 2", got.ConsecutiveFailures)
	}
	if got.LastError != "login refused" {
		t.Errorf("last_error = %q, want %q", got.LastError, "login refused")
	}
	if got.LastSuccessAt != nil {
		t.Errorf("last_success_at = %v, want nil (no success yet)", got.LastSuccessAt)
	}
	if got.LastErrorAt == nil || !got.LastErrorAt.Equal(t2) {
		t.Errorf("last_error_at = %v, want %v", got.LastErrorAt, t2)
	}
	if got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(t2) {
		t.Errorf("last_attempt_at = %v, want %v", got.LastAttemptAt, t2)
	}

	// A success resets the counter, clears the error, and stamps the success.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.RecordSyncSuccess(ctx, acctID, t3)
	}); err != nil {
		t.Fatal(err)
	}

	got = readHealth()
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures after success = %d, want 0", got.ConsecutiveFailures)
	}
	if got.LastError != "" {
		t.Errorf("last_error after success = %q, want empty", got.LastError)
	}
	if got.LastErrorAt != nil {
		t.Errorf("last_error_at after success = %v, want nil", got.LastErrorAt)
	}
	if got.LastSuccessAt == nil || !got.LastSuccessAt.Equal(t3) {
		t.Errorf("last_success_at = %v, want %v", got.LastSuccessAt, t3)
	}
	if got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(t3) {
		t.Errorf("last_attempt_at after success = %v, want %v", got.LastAttemptAt, t3)
	}
}
