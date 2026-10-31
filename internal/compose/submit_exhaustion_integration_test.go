// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

// #624 §4: does a send whose retries run out stay "Sending…" forever?
//
// Before send.go's failExhausted existed: a submit job that hit transient
// failures until jobs.Dispatcher dead-lettered it left the submission row
// 'scheduled' — send.go's requeue path was the ONLY thing that ever touched
// submission state on a transient failure, and the dispatcher's own
// dead-letter decision (internal/jobs/dispatcher.go's fail, app.fail_job) has
// no idea what a "submission" is. A client's Outbox reads that as
// "Sending…" forever, for a message no job will ever retry again.
//
// This drives a submission through the REAL jobs.Dispatcher (not just a
// direct Handle call) with a sender that always fails transiently, capped at
// a small max_attempts so the job dead-letters in milliseconds rather than
// the schema default's five exponential-backoff retries.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/jobs"
	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// alwaysTransientSender fails every Send with a plain transient error — never
// provider.ErrPermanent/ErrEAIUnsupported — so this exercises send.go's
// requeue-or-fail-exhausted branch, not the already-covered permanent-refusal
// one (TestSendGateBlocksIngestOnlyAccounts uses a different fake for that).
type alwaysTransientSender struct {
	mu    sync.Mutex
	calls int
}

func (s *alwaysTransientSender) Send(context.Context, provider.SendRequest) (provider.SendResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return provider.SendResult{}, errors.New("fake transient smtp failure: connection reset by peer")
}

func (s *alwaysTransientSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

var _ provider.Sender = (*alwaysTransientSender)(nil)

// enqueueSubmitJob writes a real, persisted `submit` job row — the same shape
// Composer.Submit leaves via tx.EnqueueJob — so the REAL jobs.Dispatcher can
// dequeue and retry it. Unlike sendGateFixture.job (an in-memory store.Job for
// a direct Handle call), this one actually exists in app.jobs.
func (f *sendGateFixture) enqueueSubmitJob(t *testing.T, subID ulid.ULID) ulid.ULID {
	t.Helper()
	var jobID ulid.ULID
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		id, err := tx.EnqueueJob(context.Background(), SubmitJobKind, time.Now().UTC(),
			[]byte(`{"submission_id":"`+subID.String()+`"}`), "submit-exhaustion-"+subID.String())
		if err != nil {
			return err
		}
		jobID = id
		return nil
	}); err != nil {
		t.Fatalf("enqueue submit job: %v", err)
	}
	return jobID
}

// capMaxAttempts lowers a job's max_attempts so the test reaches exhaustion in
// milliseconds instead of the schema default's five exponential-backoff
// retries (db/schema.sql: app.jobs.max_attempts DEFAULT 5).
func (f *sendGateFixture) capMaxAttempts(t *testing.T, jobID ulid.ULID, n int) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.jobs SET max_attempts = $2 WHERE id = $1`, jobID.String(), n); err != nil {
		t.Fatalf("cap max_attempts: %v", err)
	}
}

// jobState reads a job's row state directly — the same ground truth
// internal/store/job_reclaim_integration_test.go reads — so the wait loop
// below does not depend on the dispatcher's in-memory view of anything.
func (f *sendGateFixture) jobState(t *testing.T, jobID ulid.ULID) string {
	t.Helper()
	var state string
	if err := f.super.QueryRow(context.Background(),
		`SELECT state FROM app.jobs WHERE id = $1`, jobID.String()).Scan(&state); err != nil {
		t.Fatalf("read job state: %v", err)
	}
	return state
}

// TestSubmitJobExhaustingRetriesFailsTheSubmission is #624 §4's pin.
func TestSubmitJobExhaustingRetriesFailsTheSubmission(t *testing.T) {
	f := newSendGateFixture(t)
	f.setAccountWriteback(t, true)

	msg := f.seedMessage(t, "exhausts retries", []byte("raw bytes: exhausts retries"))
	subID := f.seedSubmission(t, msg, "rcpt@example.com")
	jobID := f.enqueueSubmitJob(t, subID)
	f.capMaxAttempts(t, jobID, 2)

	sender := &alwaysTransientSender{}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)

	d := jobs.New(jobs.StoreQueue{Store: f.st}, jobs.Config{
		Worker:       "test-exhaustion-worker",
		PollInterval: 5 * time.Millisecond,
		Backoff:      func(int) time.Duration { return time.Millisecond },
	})
	d.Register(SubmitJobKind, w.Handle)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		state = f.jobState(t, jobID)
		if state == string(store.JobDead) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-runDone

	if state != string(store.JobDead) {
		t.Fatalf("job state = %q after waiting, want %q — the fixture never reached exhaustion", state, store.JobDead)
	}
	if n := sender.callCount(); n < 2 {
		t.Errorf("provider.Sender.Send was called %d time(s), want at least max_attempts (2)", n)
	}

	sub := f.getSubmission(t, subID)
	if sub.State != store.SubmissionFailed {
		t.Fatalf("submission state = %q after its job dead-lettered, want %q — a client's "+
			"Outbox would show \"Sending…\" forever for a message no job will ever retry again",
			sub.State, store.SubmissionFailed)
	}
	if !strings.Contains(sub.SMTPResponse, "retries exhausted") {
		t.Errorf("submission smtp_response = %q, want it to name that retries were exhausted", sub.SMTPResponse)
	}
	if !strings.Contains(sub.SMTPResponse, "connection reset") {
		t.Errorf("submission smtp_response = %q, want it to carry the last error too", sub.SMTPResponse)
	}
}

// TestSubmitJobRetriesBeforeExhaustion is the control: with retries
// remaining, a transient failure must requeue (state stays 'scheduled') and
// must NOT fail the submission early. Without this, the exhaustion test above
// would pass just as well for a SendWorker that marks every transient failure
// 'failed' regardless of budget — a different, worse bug (no retries at all)
// wearing the same green tick.
func TestSubmitJobRetriesBeforeExhaustion(t *testing.T) {
	f := newSendGateFixture(t)
	f.setAccountWriteback(t, true)

	msg := f.seedMessage(t, "not yet exhausted", []byte("raw bytes: not yet exhausted"))
	subID := f.seedSubmission(t, msg, "rcpt@example.com")

	sender := &alwaysTransientSender{}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)

	// Attempt 1 of 5 (the schema default): call Handle directly, exactly as
	// TestSendGateBlocksIngestOnlyAccounts does, with a job.Attempts/
	// MaxAttempts pair that says retries remain.
	job := f.job(subID)
	job.Attempts = 1
	job.MaxAttempts = 5
	if err := w.Handle(context.Background(), job); err == nil {
		t.Fatal("Handle returned nil for a transient failure with retries remaining, want the error (so the dispatcher retries)")
	}

	sub := f.getSubmission(t, subID)
	if sub.State != store.SubmissionScheduled {
		t.Fatalf("submission state = %q after a transient failure with retries left, want %q",
			sub.State, store.SubmissionScheduled)
	}
	if strings.Contains(sub.SMTPResponse, "retries exhausted") {
		t.Errorf("submission smtp_response = %q says retries exhausted with retries remaining", sub.SMTPResponse)
	}
}
