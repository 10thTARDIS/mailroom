// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

import (
	"context"
	"strings"
	"sync"
	"testing"

	"mailroom/internal/egress/egresstest"
	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// #625 §3: the submission dial refuses an SMTP host that now resolves to a
// private or loopback address. That refusal must end the submission — failed,
// with a cause the user can read — rather than requeue it: waiting does not make
// the address permitted, so a retry only re-resolves and refuses again, and the
// Outbox would show "Sending…" meanwhile.
//
// The sender here is the REAL provider.SMTPSender with only its DNS faked, so
// the error the worker classifies is exactly the one production produces.

type countingSender struct {
	inner provider.Sender
	mu    sync.Mutex
	calls int
}

func (s *countingSender) Send(ctx context.Context, req provider.SendRequest) (provider.SendResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.inner.Send(ctx, req)
}

func TestSendRefusedSMTPHostFailsTheSubmissionWithoutRetry(t *testing.T) {
	f := newSendGateFixture(t)
	f.setAccountWriteback(t, true)

	msg := f.seedMessage(t, "rebound smtp host", []byte("raw bytes: rebound smtp host"))
	subID := f.seedSubmission(t, msg, "rcpt@example.com")

	sender := &countingSender{inner: provider.SMTPSender{
		Resolver: egresstest.NewResolver(t, []string{"127.0.0.1"}),
	}}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)

	if err := w.Handle(context.Background(), f.job(subID)); err != nil {
		t.Fatalf("Handle returned %v, want nil — a policy refusal is terminal; returning an "+
			"error hands the job back to the dispatcher to retry", err)
	}
	// Positive control: the refusal came from the sender, not from something
	// that stopped the worker before it got there.
	if sender.calls != 1 {
		t.Fatalf("sender called %d time(s), want exactly 1", sender.calls)
	}

	sub := f.getSubmission(t, subID)
	if sub.State != store.SubmissionFailed {
		t.Fatalf("submission state = %q, want %q (not requeued)", sub.State, store.SubmissionFailed)
	}
	if !strings.Contains(sub.SMTPResponse, "MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS") ||
		!strings.Contains(sub.SMTPResponse, "not a permitted") {
		t.Errorf("recorded cause = %q, want it to say the host is not permitted and name the switch", sub.SMTPResponse)
	}
	if strings.Contains(sub.SMTPResponse, "test-pass") {
		t.Errorf("recorded cause leaks the SMTP password: %q", sub.SMTPResponse)
	}
}
