// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

// #691: a completed send must wake the writeback drain.
//
// The send job queues the Sent APPEND and the Drafts EXPUNGE in its step-3
// transaction, and until WithOnSent nothing carried them up: the only nudge
// was Email/set's (#581), which fires seconds BEFORE the job runs. These tests
// pin both halves of the contract — once per send that actually happened, and
// never for one that did not, since those queued nothing to drain.
//
// They need the database because SendWorker.Send has no store seam; the
// callback itself is the only thing under test.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// sentRecorder counts WithOnSent callbacks per user.
type sentRecorder struct {
	mu    sync.Mutex
	users []ulid.ULID
}

func (r *sentRecorder) record(u ulid.ULID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, u)
}

func (r *sentRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.users)
}

func TestOnSentFiresOnceForASentSubmission(t *testing.T) {
	f := newSendGateFixture(t)
	f.setAccountWriteback(t, true)
	msg := f.seedMessage(t, "sent once", []byte("raw bytes: on-sent fires"))
	subID := f.seedSubmission(t, msg, "rcpt@example.com")

	rec := &sentRecorder{}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, &fakeSender{}, nil, WithOnSent(rec.record))
	if err := w.Handle(context.Background(), f.job(subID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sub := f.getSubmission(t, subID); sub.State != store.SubmissionSent {
		t.Fatalf("submission state = %q, want %q — the success path was not reached", sub.State, store.SubmissionSent)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("on-sent fired %d times for one send, want 1 — the Sent APPEND and Drafts EXPUNGE "+
			"this send queued wait for the next scheduled pass (#691)", n)
	}
	if rec.users[0] != f.userID {
		t.Errorf("on-sent fired for user %s, want the submission's user %s", rec.users[0], f.userID)
	}

	// A redelivered job for a submission already sent claims nothing and sends
	// nothing, so it must not nudge again.
	if err := w.Handle(context.Background(), f.job(subID)); err != nil {
		t.Fatalf("Handle (replay): %v", err)
	}
	if n := rec.count(); n != 1 {
		t.Errorf("on-sent fired %d times after a replay of an already-sent job, want still 1", n)
	}
}

func TestOnSentNeverFiresWhenNothingWasSent(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, f *sendGateFixture, subID ulid.ULID)
		sender provider.Sender
		job    func(j store.Job) store.Job
		state  string
	}{
		{
			name:   "permanent refusal",
			sender: &fakeSender{err: fmt.Errorf("%w: 550 5.1.1 no such user", provider.ErrPermanent)},
			state:  store.SubmissionFailed,
		},
		{
			name:   "transient failure, requeued",
			sender: &alwaysTransientSender{},
			job:    func(j store.Job) store.Job { j.Attempts, j.MaxAttempts = 1, 5; return j },
			state:  store.SubmissionScheduled,
		},
		{
			name:   "transient failure, retries exhausted",
			sender: &alwaysTransientSender{},
			job:    func(j store.Job) store.Job { j.Attempts, j.MaxAttempts = 5, 5; return j },
			state:  store.SubmissionFailed,
		},
		{
			name: "cancelled inside the undo window",
			setup: func(t *testing.T, f *sendGateFixture, subID ulid.ULID) {
				if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
					return tx.CancelSubmission(context.Background(), subID)
				}); err != nil {
					t.Fatalf("cancel: %v", err)
				}
			},
			sender: &fakeSender{},
			state:  store.SubmissionCanceled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSendGateFixture(t)
			f.setAccountWriteback(t, true)
			msg := f.seedMessage(t, tc.name, []byte("raw bytes: "+tc.name))
			subID := f.seedSubmission(t, msg, "rcpt@example.com")
			if tc.setup != nil {
				tc.setup(t, f, subID)
			}
			job := f.job(subID)
			if tc.job != nil {
				job = tc.job(job)
			}

			rec := &sentRecorder{}
			w := NewSendWorker(f.st, f.blobs, fakeKeys{}, tc.sender, nil, WithOnSent(rec.record))
			_ = w.Handle(context.Background(), job) // some of these return the send error by design

			// Arming control: the case really reached the outcome it names.
			if sub := f.getSubmission(t, subID); sub.State != tc.state {
				t.Fatalf("submission state = %q, want %q — the case did not exercise its path", sub.State, tc.state)
			}
			if n := rec.count(); n != 0 {
				t.Errorf("on-sent fired %d time(s) for a send that did not happen, want 0", n)
			}
		})
	}
}
