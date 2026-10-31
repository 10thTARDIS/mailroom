// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// RECEIVE-ONLY (ADR-046 §12). A send the provider refuses with 5.7.139 (SMTP
// AUTH disabled by the organisation) FAILS with the readable reason — it is
// not held like a reconnect — and marks the account receive-only, so the next
// queued submission fails WITHOUT dialling the provider again.
//
// PROBE: drop the ErrSMTPAuthDisabled branch in Send and the first submission
// fails with raw provider text and the account is never marked (the second
// submission dials).
func TestSMTPAuthDisabledFailsAndMarksReceiveOnly(t *testing.T) {
	f := newSendGateFixture(t)
	ctx := context.Background()
	sender := &fakeSender{err: fmt.Errorf("%w: %w: 535 5.7.139 Authentication unsuccessful",
		provider.ErrPermanent, provider.ErrSMTPAuthDisabled)}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)

	first := f.seedSubmission(t, f.seedMessage(t, "one", []byte("Subject: one\r\n\r\nx\r\n")), "them@example.net")
	if err := w.Handle(ctx, f.job(first)); err != nil {
		t.Fatalf("Handle: %v (a permanent refusal is terminal)", err)
	}
	sub := f.getSubmission(t, first)
	if sub.State != store.SubmissionFailed {
		t.Fatalf("state = %s, want failed (5.7.139 is not held)", sub.State)
	}
	if !strings.Contains(sub.SMTPResponse, "5.7.139") || !strings.Contains(sub.SMTPResponse, "administrator") {
		t.Errorf("stored cause = %q, want the readable receive-only reason", sub.SMTPResponse)
	}
	var code string
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		code, err = tx.SendBlocked(ctx, f.account)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code != store.SendBlockedSMTPAuthDisabled {
		t.Fatalf("send-blocked code = %q, want %q", code, store.SendBlockedSMTPAuthDisabled)
	}

	second := f.seedSubmission(t, f.seedMessage(t, "two", []byte("Subject: two\r\n\r\ny\r\n")), "them@example.net")
	before := sender.callCount()
	if err := w.Handle(ctx, f.job(second)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sender.callCount() != before {
		t.Errorf("a receive-only account's queued submission dialled the provider again")
	}
	if s := f.getSubmission(t, second); s.State != store.SubmissionFailed || !strings.Contains(s.SMTPResponse, "receive mail but not send") {
		t.Errorf("second submission = %s / %q, want failed with the reason", s.State, s.SMTPResponse)
	}

	// Clearing (a reconnect whose SMTP check passed) lets the next one dial.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		cleared, err := tx.ClearSendBlocked(ctx, f.account)
		if err == nil && !cleared {
			err = fmt.Errorf("nothing to clear")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sender.err = nil
	third := f.seedSubmission(t, f.seedMessage(t, "three", []byte("Subject: three\r\n\r\nz\r\n")), "them@example.net")
	if err := w.Handle(ctx, f.job(third)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if s := f.getSubmission(t, third); s.State != store.SubmissionSent {
		t.Errorf("after clearing, state = %s, want sent", s.State)
	}
}
