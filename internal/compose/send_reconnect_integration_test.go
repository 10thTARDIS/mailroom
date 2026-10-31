// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

// #52, ADR-046 §5 (decided 2026-10-27): outgoing mail waiting on an
// account whose OAuth grant needs its user to reconnect is HELD, never failed
// and never handed to SMTP — and it goes out once the grant is active again.
// Two ways in: the grant was already dead when the job ran (held before the
// claim), and the grant died during the send (the token refresh said so).

import (
	"context"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

func (f *sendGateFixture) makeOAuth(t *testing.T, state string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.super.Exec(ctx, `UPDATE app.accounts SET auth_kind = 'oauth2', provider_profile = 'microsoft'
		WHERE user_id = $1 AND id = $2`, f.userID.String(), f.account.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.super.Exec(ctx, `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, state)
		VALUES ($1, $2, 'microsoft', 'oid-1', 'sender@example.com', $3)`,
		f.account.String(), f.userID.String(), state); err != nil {
		t.Fatal(err)
	}
}

func (f *sendGateFixture) setGrant(t *testing.T, state string) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(), `UPDATE app.oauth_grants SET state = $2 WHERE account_id = $1`,
		f.account.String(), state); err != nil {
		t.Fatal(err)
	}
}

func (f *sendGateFixture) submitJobs(t *testing.T, subID ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(), `SELECT count(*) FROM app.jobs
		WHERE user_id = $1 AND kind = 'submit' AND state = 'pending' AND payload->>'submission_id' = $2
		  AND fire_at > now()`, f.userID.String(), subID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *sendGateFixture) submissionChanges(t *testing.T, subID ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(), `SELECT count(*) FROM app.change_log
		WHERE user_id = $1 AND object_type = 'EmailSubmission' AND object_id = $2`,
		f.userID.String(), subID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSubmissionHeldWhileAwaitingReconnectThenSent(t *testing.T) {
	f := newSendGateFixture(t)
	f.makeOAuth(t, store.OAuthStateNeedsReauth)
	msg := f.seedMessage(t, "held", []byte("Subject: held\r\n\r\nbody\r\n"))
	sub := f.seedSubmission(t, msg, "friend@example.net")

	refuse := &fakeSender{t: t} // any call fails the test
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, refuse, nil)
	changesBefore := f.submissionChanges(t, sub)
	for i := 0; i < 3; i++ {
		if err := w.Handle(context.Background(), f.job(sub)); err != nil {
			t.Fatalf("Handle #%d: %v (a hold is not a failure the dispatcher should retry)", i, err)
		}
	}
	got := f.getSubmission(t, sub)
	if got.State != store.SubmissionScheduled || got.SMTPResponse != AwaitingReconnectReason {
		t.Fatalf("held submission: state %q response %q", got.State, got.SMTPResponse)
	}
	if n := f.submitJobs(t, sub); n < 1 {
		t.Fatal("no later job was queued: the held submission would never be looked at again")
	}
	// The hold's reason is written ONCE: three looks, one change row.
	if d := f.submissionChanges(t, sub) - changesBefore; d != 1 {
		t.Errorf("three looks at a held submission appended %d change rows, want 1", d)
	}
	if got.JobID == nil {
		t.Error("the held submission is not linked to the job that will send it (Undo would miss it)")
	}

	// Reconnected: the next look sends it.
	f.setGrant(t, store.OAuthStateActive)
	ok := &fakeSender{}
	if err := NewSendWorker(f.st, f.blobs, fakeKeys{}, ok, nil).Handle(context.Background(), f.job(sub)); err != nil {
		t.Fatalf("after reconnect: %v", err)
	}
	if ok.callCount() != 1 || f.getSubmission(t, sub).State != store.SubmissionSent {
		t.Fatalf("after reconnect: sends %d, state %q", ok.callCount(), f.getSubmission(t, sub).State)
	}
}

func TestSubmissionHeldWhenGrantDiesMidSend(t *testing.T) {
	f := newSendGateFixture(t)
	f.makeOAuth(t, store.OAuthStateActive)
	msg := f.seedMessage(t, "mid-send", []byte("Subject: mid\r\n\r\nbody\r\n"))
	sub := f.seedSubmission(t, msg, "friend@example.net")
	// What provider.SMTPSender returns when the token refresh hears
	// invalid_grant: a permanent-looking error that is also the reauth
	// sentinel (provider/sender.go tokenError).
	dying := &fakeSender{err: fmt.Errorf("%w: provider: smtp auth: %w", provider.ErrPermanent, xoauth2.ErrReauthRequired)}
	if err := NewSendWorker(f.st, f.blobs, fakeKeys{}, dying, nil).Handle(context.Background(), f.job(sub)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := f.getSubmission(t, sub)
	if got.State != store.SubmissionScheduled || got.SMTPResponse != AwaitingReconnectReason {
		t.Fatalf("a send whose grant died mid-flight: state %q response %q; want held, not failed", got.State, got.SMTPResponse)
	}
	if f.submitJobs(t, sub) != 1 {
		t.Fatal("no later job was queued")
	}
	// The control: the same error WITHOUT the reauth sentinel is a permanent
	// failure, exactly as before.
	sub2 := f.seedSubmission(t, f.seedMessage(t, "perm", []byte("Subject: perm\r\n\r\nx\r\n")), "friend@example.net")
	perm := &fakeSender{err: fmt.Errorf("%w: 554 refused", provider.ErrPermanent)}
	if err := NewSendWorker(f.st, f.blobs, fakeKeys{}, perm, nil).Handle(context.Background(), f.job(sub2)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if st := f.getSubmission(t, sub2).State; st != store.SubmissionFailed {
		t.Fatalf("an ordinary permanent refusal ended %q, want failed", st)
	}
}
