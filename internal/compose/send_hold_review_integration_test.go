// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

// Review findings on the send holds (#52, PR #903):
//
//   - an Undo Send that commits between the hold's read and its write must
//     STAY cancelled, with nothing requeued (MEDIUM-1);
//   - a disconnected OAuth account (no grant) is held like needs_reauth, with
//     the reason written once, instead of claim-fail-requeue every pass
//     (MEDIUM-2);
//   - operator_error holds with its own reason instead of failing every
//     user's queued mail, and a held submission notices the operator's fix
//     through a token probe (by design).

import (
	"context"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

func (f *sendGateFixture) disconnect(t *testing.T) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(), `DELETE FROM app.oauth_grants WHERE account_id = $1`, f.account.String()); err != nil {
		t.Fatal(err)
	}
}

func (f *sendGateFixture) pendingSubmitJobs(t *testing.T, subID ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(), `SELECT count(*) FROM app.jobs
		WHERE user_id = $1 AND kind = 'submit' AND state = 'pending' AND payload->>'submission_id' = $2`,
		f.userID.String(), subID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// probeKeys is fakeKeys plus the token probe the operator_error hold asks.
type probeKeys struct {
	fakeKeys
	err   error
	calls int
}

func (p *probeKeys) ProbeToken(context.Context, ulid.ULID, store.Account) error {
	p.calls++
	return p.err
}

func TestHoldNeverUndoesACancel(t *testing.T) {
	f := newSendGateFixture(t)
	f.makeOAuth(t, store.OAuthStateNeedsReauth)
	sub := f.seedSubmission(t, f.seedMessage(t, "undo", []byte("Subject: undo\r\n\r\nx\r\n")), "friend@example.net")

	// The user's Undo Send lands in the window between the hold's unlocked
	// read and its write, committed in its own transaction.
	holdTestHook = func() {
		if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
			return tx.CancelSubmission(context.Background(), sub)
		}); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}
	defer func() { holdTestHook = nil }()

	refuse := &fakeSender{t: t}
	if err := NewSendWorker(f.st, f.blobs, fakeKeys{}, refuse, nil).Handle(context.Background(), f.job(sub)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if st := f.getSubmission(t, sub).State; st != store.SubmissionCanceled {
		t.Fatalf("after a cancel raced the hold, state = %q, want canceled", st)
	}
	if n := f.pendingSubmitJobs(t, sub); n != 0 {
		t.Fatalf("a cancelled submission was requeued: %d pending submit jobs", n)
	}
}

func TestDisconnectedAccountHoldsOnce(t *testing.T) {
	f := newSendGateFixture(t)
	f.makeOAuth(t, store.OAuthStateNeedsReauth)
	sub := f.seedSubmission(t, f.seedMessage(t, "disc", []byte("Subject: disc\r\n\r\nx\r\n")), "friend@example.net")
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, &fakeSender{t: t}, nil)
	if err := w.Handle(context.Background(), f.job(sub)); err != nil {
		t.Fatal(err)
	}
	afterHold := f.submissionChanges(t, sub)

	// Then the account is disconnected (no grant): several more passes write
	// nothing more and never reach the claim.
	f.disconnect(t)
	for i := 0; i < 4; i++ {
		if err := w.Handle(context.Background(), f.job(sub)); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if d := f.submissionChanges(t, sub) - afterHold; d > 1 {
		t.Fatalf("four passes on a disconnected account appended %d change rows, want at most 1", d)
	}
	got := f.getSubmission(t, sub)
	if got.State != store.SubmissionScheduled || store.SubmissionHeldKind(got) != store.HeldKindReconnect {
		t.Fatalf("disconnected: state %q held %q", got.State, store.SubmissionHeldKind(got))
	}

	// A submission first looked at on an already-disconnected account is held
	// too, before any claim.
	sub2 := f.seedSubmission(t, f.seedMessage(t, "disc2", []byte("Subject: disc2\r\n\r\nx\r\n")), "friend@example.net")
	before := f.submissionChanges(t, sub2)
	for i := 0; i < 3; i++ {
		if err := w.Handle(context.Background(), f.job(sub2)); err != nil {
			t.Fatal(err)
		}
	}
	if d := f.submissionChanges(t, sub2) - before; d != 1 {
		t.Fatalf("three looks at a send on a disconnected account appended %d change rows, want 1", d)
	}
}

func TestOperatorErrorHoldsInsteadOfFailing(t *testing.T) {
	f := newSendGateFixture(t)
	f.makeOAuth(t, store.OAuthStateOperatorError)
	sub := f.seedSubmission(t, f.seedMessage(t, "op", []byte("Subject: op\r\n\r\nx\r\n")), "friend@example.net")

	// Still broken: the probe says so, the send is held with the provider's
	// reason, written once, and nothing is handed to SMTP.
	keys := &probeKeys{err: fmt.Errorf("refresh: %w", xoauth2.ErrProviderMisconfigured)}
	w := NewSendWorker(f.st, f.blobs, keys, &fakeSender{t: t}, nil)
	before := f.submissionChanges(t, sub)
	for i := 0; i < 3; i++ {
		if err := w.Handle(context.Background(), f.job(sub)); err != nil {
			t.Fatal(err)
		}
	}
	got := f.getSubmission(t, sub)
	if got.State != store.SubmissionScheduled || got.SMTPResponse != store.HeldProviderReason("microsoft") ||
		store.SubmissionHeldKind(got) != store.HeldKindProvider {
		t.Fatalf("operator_error: state %q response %q", got.State, got.SMTPResponse)
	}
	if d := f.submissionChanges(t, sub) - before; d != 1 || keys.calls != 3 {
		t.Fatalf("three looks: %d change rows (want 1), %d probes (want 3)", d, keys.calls)
	}

	// The operator fixes it: the probe gets a token, and the next look sends,
	// whether or not anything else refreshed the grant.
	keys.err = nil
	ok := &fakeSender{}
	if err := NewSendWorker(f.st, f.blobs, keys, ok, nil).Handle(context.Background(), f.job(sub)); err != nil {
		t.Fatal(err)
	}
	if ok.callCount() != 1 || f.getSubmission(t, sub).State != store.SubmissionSent {
		t.Fatalf("after the fix: sends %d, state %q", ok.callCount(), f.getSubmission(t, sub).State)
	}

	// Mid-send: the token refresh is refused for the server's client while
	// the grant still looked active — held with the provider reason, not failed.
	f.setGrant(t, store.OAuthStateActive)
	sub2 := f.seedSubmission(t, f.seedMessage(t, "op2", []byte("Subject: op2\r\n\r\nx\r\n")), "friend@example.net")
	dying := &fakeSender{err: fmt.Errorf("%w: provider: smtp auth: %w", provider.ErrPermanent, xoauth2.ErrProviderMisconfigured)}
	if err := NewSendWorker(f.st, f.blobs, fakeKeys{}, dying, nil).Handle(context.Background(), f.job(sub2)); err != nil {
		t.Fatal(err)
	}
	if got := f.getSubmission(t, sub2); got.State != store.SubmissionScheduled || store.SubmissionHeldKind(got) != store.HeldKindProvider {
		t.Fatalf("mid-send operator_error: state %q response %q; want held, not failed", got.State, got.SMTPResponse)
	}
	if f.pendingSubmitJobs(t, sub2) != 1 {
		t.Fatal("no later job was queued")
	}
}
