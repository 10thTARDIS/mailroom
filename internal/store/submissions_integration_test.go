// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the EmailSubmission layer + outbound ledger
// (mailroom#22, spec §5): the change_log invariant, the Undo-Send race (cancel
// vs. the send job — exactly one wins), the per-account daily cap, and RLS
// isolation of one user's submissions from another's.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// newSubmission creates a submission for the fixture user's seeded message.
func newSubmission(t *testing.T, ctx context.Context, tx *UserTx, u fixtureUser, sendAt time.Time) ulid.ULID {
	t.Helper()
	acct := mustULID(t, u.AccountID)
	identity, err := tx.CreateIdentity(ctx, IdentityRow{
		AccountID: acct, Kind: IdentityFixed,
		Address: "sender-" + NewULID().String() + "@example.com", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	id, _, err := tx.CreateSubmission(ctx, SubmissionRow{
		AccountID:   acct,
		IdentityID:  identity,
		MessageID:   mustULID(t, u.MessageID),
		FromAddress: "sender@example.com",
		EnvelopeTo:  []string{"them@example.net"},
		SendAt:      sendAt,
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	return id
}

// Every submission mutation appends exactly one EmailSubmission change_log row,
// in the same transaction (CLAUDE.md's load-bearing invariant), so /changes and
// the outbox view see the send progress.
func TestSubmissionChangeLogInvariant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}
		id := newSubmission(t, ctx, tx, a, time.Now().Add(time.Minute))

		afterCreate, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}
		if afterCreate != before+1 {
			t.Errorf("create appended %d change rows, want 1", afterCreate-before)
		}

		if _, _, err := tx.ClaimSubmissionForSend(ctx, id); err != nil {
			return err
		}
		afterClaim, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}
		if afterClaim != afterCreate+1 {
			t.Errorf("claim appended %d change rows, want 1", afterClaim-afterCreate)
		}

		if err := tx.FinishSubmission(ctx, id, SubmissionSent, "250 ok"); err != nil {
			return err
		}
		afterFinish, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}
		if afterFinish != afterClaim+1 {
			t.Errorf("finish appended %d change rows, want 1", afterFinish-afterClaim)
		}

		subs, err := tx.GetSubmissions(ctx, []ulid.ULID{id})
		if err != nil {
			return err
		}
		if subs[0].State != SubmissionSent || subs[0].UndoStatus() != "final" {
			t.Errorf("state = %q / undoStatus = %q, want sent / final", subs[0].State, subs[0].UndoStatus())
		}
		if subs[0].SentAt == nil {
			t.Error("a sent submission must carry sent_at")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// THE UNDO-SEND RACE. Cancel and the send job both guard on the SAME 'scheduled'
// state, so exactly one can win. This is what makes "undo" honest: there is no
// window in which a submission is reported canceled AND handed to SMTP.
func TestUndoSendRaceIsExclusive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// Case 1: the user cancels first — the job then finds nothing to claim.
	var canceled ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		canceled = newSubmission(t, ctx, tx, a, time.Now().Add(time.Hour))
		if err := tx.CancelSubmission(ctx, canceled); err != nil {
			return err
		}
		_, claimed, err := tx.ClaimSubmissionForSend(ctx, canceled)
		if err != nil {
			return err
		}
		if claimed {
			t.Fatal("the send job claimed a submission the user had already canceled — the mail would go out after an 'undo'")
		}
		subs, err := tx.GetSubmissions(ctx, []ulid.ULID{canceled})
		if err != nil {
			return err
		}
		if subs[0].UndoStatus() != "canceled" {
			t.Errorf("undoStatus = %q, want canceled", subs[0].UndoStatus())
		}
		return nil
	}); err != nil {
		t.Fatalf("cancel-first: %v", err)
	}

	// Case 2: the job claims first — the cancel then loses, LOUDLY. A user who
	// hits undo a moment too late must be told, not lied to.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id := newSubmission(t, ctx, tx, a, time.Now())
		if _, claimed, err := tx.ClaimSubmissionForSend(ctx, id); err != nil || !claimed {
			t.Fatalf("claim: claimed=%v err=%v", claimed, err)
		}
		err := tx.CancelSubmission(ctx, id)
		if !errors.Is(err, ErrSubmissionNotCancelable) {
			t.Fatalf("cancel after claim: err = %v, want ErrSubmissionNotCancelable", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("claim-first: %v", err)
	}

	// Cancelling something that does not exist is notFound, not "cancelable".
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		err := tx.CancelSubmission(ctx, NewULID())
		if !errors.Is(err, ErrSubmissionNotFound) {
			t.Fatalf("cancel unknown: err = %v, want ErrSubmissionNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("cancel-unknown: %v", err)
	}
}

// Cancelling a submission must also kill its pending timer, or the job would
// still fire (find nothing to claim, and do nothing — correct, but it would
// leave a lie in the queue).
func TestCancelSubmissionCancelsTheJob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	var jobID ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id := newSubmission(t, ctx, tx, a, time.Now().Add(time.Hour))
		var err error
		jobID, err = tx.EnqueueJob(ctx, "submit", time.Now().Add(time.Hour), nil, "submit:"+id.String())
		if err != nil {
			return err
		}
		if err := tx.SetSubmissionJob(ctx, id, jobID); err != nil {
			return err
		}
		return tx.CancelSubmission(ctx, id)
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	var state string
	if err := h.super.QueryRow(ctx,
		`SELECT state FROM app.jobs WHERE id = $1`, jobID.String()).Scan(&state); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if state == "pending" {
		t.Error("cancelling a submission must cancel its pending send job")
	}
}

// The outbound ledger (spec §5): provider daily caps are counted per ACCOUNT.
// The reservation is atomic, so the cap cannot be overshot, and a failed send
// gives its reservation back rather than permanently burning the user's budget.
func TestOutboundLedgerEnforcesDailyCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)

	// A cap of 2 for this account.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.accounts SET outbound_daily_quota = 2 WHERE id = $1`, acct.String()); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	// seedUser already spent 1 of today's budget.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.outbound_ledger SET sent_count = 0 WHERE account_id = $1`, acct.String()); err != nil {
		t.Fatalf("reset ledger: %v", err)
	}

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for i := 1; i <= 2; i++ {
			remaining, err := tx.ReserveOutbound(ctx, acct)
			if err != nil {
				t.Fatalf("reserve %d: %v", i, err)
			}
			if want := 2 - i; remaining != want {
				t.Errorf("reserve %d: remaining = %d, want %d", i, remaining, want)
			}
		}
		// The cap is spent: the third send must NOT go out.
		if _, err := tx.ReserveOutbound(ctx, acct); !errors.Is(err, ErrOverQuota) {
			t.Fatalf("third reserve: err = %v, want ErrOverQuota", err)
		}

		// A send that failed hands its reservation back.
		if err := tx.ReleaseOutbound(ctx, acct); err != nil {
			return err
		}
		if _, err := tx.ReserveOutbound(ctx, acct); err != nil {
			t.Fatalf("after release, a send should be possible again: %v", err)
		}

		sent, quota, err := tx.OutboundUsage(ctx, acct)
		if err != nil {
			return err
		}
		if sent != 2 || quota != 2 {
			t.Errorf("usage = %d/%d, want 2/2", sent, quota)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// RLS: one user's submissions are invisible to another, and cannot be canceled
// by them.
func TestSubmissionIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	var subID ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		subID = newSubmission(t, ctx, tx, a, time.Now().Add(time.Hour))
		return nil
	}); err != nil {
		t.Fatalf("create as A: %v", err)
	}

	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		subs, err := tx.GetSubmissions(ctx, []ulid.ULID{subID})
		if err != nil {
			return err
		}
		if len(subs) != 0 {
			t.Fatalf("user B can see user A's submission: %+v", subs)
		}
		if err := tx.CancelSubmission(ctx, subID); !errors.Is(err, ErrSubmissionNotFound) {
			t.Fatalf("user B cancelling user A's send: err = %v, want ErrSubmissionNotFound", err)
		}
		_, claimed, err := tx.ClaimSubmissionForSend(ctx, subID)
		if err != nil {
			return err
		}
		if claimed {
			t.Fatal("user B claimed user A's submission for sending")
		}
		return nil
	}); err != nil {
		t.Fatalf("tx as B: %v", err)
	}

	// A's submission is untouched and still cancelable.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		subs, err := tx.GetSubmissions(ctx, []ulid.ULID{subID})
		if err != nil {
			return err
		}
		if len(subs) != 1 || subs[0].State != SubmissionScheduled {
			t.Fatalf("A's submission was disturbed by B: %+v", subs)
		}
		return tx.CancelSubmission(ctx, subID)
	}); err != nil {
		t.Fatalf("tx as A: %v", err)
	}
}
