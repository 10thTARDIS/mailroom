// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

// Regression coverage for the ingest-only gate in (*SendWorker).Handle
// (send.go, ADR-028 §6).
//
// ⚠ THIS GUARDS THE ONE PLACE BYTES REACH SMTP. Before this file, NewSendWorker
// appeared in zero _test.go files in the tree (verified by grep) — so the
// four-line `if` at send.go's structural gate had never been exercised, let
// alone exercised in a way that could fail. EmailSubmission/set's refusal
// (internal/api, already tested) is the POLICY layer and cannot speak for a
// submission created while an account was live and dispatched after the
// account was switched to ingest-only — the send job runs asynchronously, on
// its own schedule, with no request in flight to refuse. This is the only
// test of the seam that actually matters: the job path, against a real store,
// with a fake provider.Sender that fails the test outright if it is ever
// handed bytes while the gate should be holding.
//
// Needs a database (the submission is claimed through a real UserTx), so this
// is an integration test against the throwaway Postgres from
// scripts/integration-test.sh — same shape as internal/writeback's
// newLiveFixture and internal/purge's purgeHarness/setWriteback.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// --- fakes -------------------------------------------------------------------

// fakeSender is the provider.Sender fake. When t is set, a call to Send FAILS
// THE TEST IMMEDIATELY — the ingest-only gate must never let a submission
// reach this method, so a silent record-and-check-later would let a slow test
// failure hide behind an otherwise-green run. calls is also kept so callers
// can assert a precise count (belt and braces).
type fakeSender struct {
	mu    sync.Mutex
	t     *testing.T // non-nil: any call is an immediate, fatal test failure
	calls []provider.SendRequest
	err   error // returned to the caller when non-nil (unused by these tests)
}

func (f *fakeSender) Send(_ context.Context, req provider.SendRequest) (provider.SendResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.t != nil {
		f.t.Fatalf("provider.Sender.Send was called with MailFrom=%q RcptTo=%v — "+
			"the ingest-only gate did not hold", req.MailFrom, req.RcptTo)
	}
	if f.err != nil {
		return provider.SendResult{}, f.err
	}
	return provider.SendResult{Response: "250 OK (fake sender)"}, nil
}

func (f *fakeSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var _ provider.Sender = (*fakeSender)(nil)

// fakeKeys satisfies compose.Keys with no real key hierarchy: MailDEK returns
// a fixed 32-byte key (the fake blob store below does not use it to decrypt
// anything — it just serves back whatever bytes the test seeded), and
// SMTPAuth returns fixed credentials nothing actually authenticates with,
// since fakeSender never dials out.
type fakeKeys struct{}

func (fakeKeys) MailDEK(context.Context, ulid.ULID) ([]byte, ulid.ULID, error) {
	return []byte("01234567890123456789012345678901"), store.NewULID(), nil
}

func (fakeKeys) SMTPAuth(_ context.Context, _ ulid.ULID, acct store.Account) (SMTPLogin, error) {
	return PasswordLogin(acct, "test-user", "test-pass"), nil
}

var _ Keys = fakeKeys{}

// fakeBlobs is a minimal blob.BlobStore keyed by content hash: the send path
// only ever reads back exactly what seedMessage put in, so there is no need
// for real encryption here — only for Get to hand back the bytes this test
// chose for a given hash.
type fakeBlobs struct {
	mu   sync.Mutex
	data map[blob.ContentHash][]byte
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{data: map[blob.ContentHash][]byte{}}
}

func (f *fakeBlobs) put(h blob.ContentHash, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[h] = raw
}

func (f *fakeBlobs) Put(context.Context, ulid.ULID, []byte, io.Reader) (blob.ContentHash, error) {
	return blob.ContentHash{}, errors.New("fakeBlobs: Put not implemented")
}

func (f *fakeBlobs) Get(_ context.Context, _ ulid.ULID, _ []byte, h blob.ContentHash) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.data[h]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (f *fakeBlobs) Exists(_ context.Context, _ ulid.ULID, h blob.ContentHash) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[h]
	return ok, nil
}

func (f *fakeBlobs) StoredSize(_ context.Context, _ ulid.ULID, h blob.ContentHash) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.data[h]
	if !ok {
		return 0, blob.ErrNotFound
	}
	return int64(len(raw)), nil
}

func (f *fakeBlobs) Delete(_ context.Context, _ ulid.ULID, h blob.ContentHash) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, h)
	return nil
}

var _ blob.BlobStore = (*fakeBlobs)(nil)

// --- harness -----------------------------------------------------------------
//
// Same shape as internal/writeback's newLiveFixture and internal/purge's
// newPurgeHarness: skip cleanly when the integration env is absent, seed a
// user/account/identity as the superuser, and build a real *store.Store from
// the app pool (mailroom_app, RLS enforced).

type sendGateFixture struct {
	super    *pgxpool.Pool
	st       *store.Store
	blobs    *fakeBlobs
	userID   ulid.ULID
	account  ulid.ULID
	identity ulid.ULID
}

func newSendGateFixture(t *testing.T) *sendGateFixture {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)

	f := &sendGateFixture{
		super: super, st: store.NewFromPool(app), blobs: newFakeBlobs(),
		userID: store.NewULID(), account: store.NewULID(), identity: store.NewULID(),
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com")
	// writeback_enabled defaults to true (schema.sql) — tests that need the
	// account shadowed call setAccountWriteback(t, false) explicitly.
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		f.account.String(), f.userID.String(), store.NewULID().String())
	exec(`INSERT INTO app.identities (id, user_id, account_id, kind, address)
	      VALUES ($1, $2, $3, 'fixed', 'sender@example.com')`,
		f.identity.String(), f.userID.String(), f.account.String())
	return f
}

// setAccountWriteback flips the account's per-account switch, as Account/set
// does (mirrors internal/purge/ingestonly_integration_test.go's setWriteback).
func (f *sendGateFixture) setAccountWriteback(t *testing.T, enabled bool) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.accounts SET writeback_enabled = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), f.account.String(), enabled); err != nil {
		t.Fatalf("set writeback_enabled=%v: %v", enabled, err)
	}
}

// seedMessage inserts a canonical message (plus its blob metadata row and
// thread) whose content hash resolves, through fakeBlobs, to raw.
func (f *sendGateFixture) seedMessage(t *testing.T, subject string, raw []byte) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	sum := sha256.Sum256(raw)
	hash := blob.ContentHash(sum)
	thread := store.NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), sum[:], store.NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		thread.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject,
	        from_address, received_at, size_bytes, message_id_hint)
	      VALUES ($1, $2, $3, $4, $5, 'sender@example.com', now(), 10, $6)`,
		id.String(), f.userID.String(), sum[:], thread.String(), subject,
		"<"+id.String()+"@example.com>")
	f.blobs.put(hash, raw)
	return id
}

// seedSubmission records a 'scheduled' submission through the real store —
// exactly the row CreateSubmission (via Composer.Submit) leaves behind — with
// no job enqueued, because ClaimSubmissionForSend only cares about the
// submission's own state, not the job that will (or, here, won't) trigger it.
func (f *sendGateFixture) seedSubmission(t *testing.T, msg ulid.ULID, rcptTo string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	var subID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		id, created, err := tx.CreateSubmission(ctx, store.SubmissionRow{
			AccountID:   f.account,
			IdentityID:  f.identity,
			MessageID:   msg,
			FromAddress: "sender@example.com",
			EnvelopeTo:  []string{rcptTo},
			SendAt:      time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		if !created {
			return errors.New("seed submission: unexpectedly already existed")
		}
		subID = id
		return nil
	}); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	return subID
}

// getSubmission reads a submission back through the real store (RLS-scoped,
// same path a caller would use), which is the ground truth for its state and
// stored failure cause.
func (f *sendGateFixture) getSubmission(t *testing.T, id ulid.ULID) store.Submission {
	t.Helper()
	ctx := context.Background()
	var sub store.Submission
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		subs, err := tx.GetSubmissions(ctx, []ulid.ULID{id})
		if err != nil {
			return err
		}
		if len(subs) == 0 {
			return store.ErrSubmissionNotFound
		}
		sub = subs[0]
		return nil
	}); err != nil {
		t.Fatalf("get submission: %v", err)
	}
	return sub
}

// job builds the durable-job row Handle unmarshals — the same
// `{"submission_id":"..."}` shape send.go's own deferForQuota enqueues.
func (f *sendGateFixture) job(subID ulid.ULID) store.Job {
	return store.Job{
		ID:      store.NewULID(),
		UserID:  &f.userID,
		Kind:    SubmitJobKind,
		Payload: []byte(`{"submission_id":"` + subID.String() + `"}`),
	}
}

// --- tests -------------------------------------------------------------------

// TestSendGateBlocksIngestOnlyAccounts is test 1: with the account shadowed —
// by EITHER half of the switch send.go ANDs together — a claimed, scheduled
// submission must never reach provider.Sender, must end up 'failed' with
// errIngestOnlyCannotSend recorded as the cause, and Handle must return nil
// (a permanent refusal is terminal, per Handle's own doc — not something the
// dispatcher should retry).
func TestSendGateBlocksIngestOnlyAccounts(t *testing.T) {
	cases := []struct {
		name             string
		accountWriteback bool // app.accounts.writeback_enabled
		instanceDisabled bool // Store.WritebackDisabled() — MAILROOM_WRITEBACK_DISABLED
	}{
		{name: "per-account writeback_enabled=false", accountWriteback: false, instanceDisabled: false},
		{name: "instance-level kill switch alone (account column true)", accountWriteback: true, instanceDisabled: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSendGateFixture(t)
			f.setAccountWriteback(t, c.accountWriteback)
			if c.instanceDisabled {
				f.st.SetWritebackDisabled(true)
				t.Cleanup(func() { f.st.SetWritebackDisabled(false) })
			}

			msg := f.seedMessage(t, "shadowed send", []byte("raw bytes: "+c.name))
			subID := f.seedSubmission(t, msg, "rcpt@example.com")

			sender := &fakeSender{t: t} // ANY call fails this test immediately
			w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)

			err := w.Handle(context.Background(), f.job(subID))
			if err != nil {
				t.Fatalf("Handle returned %v, want nil — a permanent ingest-only "+
					"refusal is terminal, not something the dispatcher should retry", err)
			}

			if n := sender.callCount(); n != 0 {
				t.Fatalf("provider.Sender.Send was called %d time(s), want 0 — "+
					"ingest-only mode must never hand bytes to SMTP", n)
			}

			sub := f.getSubmission(t, subID)
			if sub.State != store.SubmissionFailed {
				t.Fatalf("submission state = %q, want %q (not requeued, not canceled)",
					sub.State, store.SubmissionFailed)
			}
			if sub.SMTPResponse != errIngestOnlyCannotSend.Error() {
				t.Fatalf("submission cause = %q, want %q",
					sub.SMTPResponse, errIngestOnlyCannotSend.Error())
			}
		})
	}
}

// TestSendGateAllowsWritebackEnabledAccounts is test 2, THE POSITIVE CONTROL,
// and it is not optional.
//
// ⚠ REQUIRED. Every assertion in TestSendGateBlocksIngestOnlyAccounts is
// equally satisfied by a SendWorker that has simply stopped sending anything —
// a broken fixture, a Handle that returns before ever reaching the sender for
// an unrelated reason, or a store wiring error would all produce the exact
// same "the sender was never called" result. This project has shipped three
// vacuous security assertions this shape before (#131 attachment-preview
// sandbox test). The identical scenario with writeback ENABLED must reach
// provider.Sender exactly once and the submission must end up 'sent' — that is
// what proves the negative test actually got far enough to exercise the gate,
// rather than stalling on setup.
func TestSendGateAllowsWritebackEnabledAccounts(t *testing.T) {
	f := newSendGateFixture(t)
	f.setAccountWriteback(t, true)

	msg := f.seedMessage(t, "live send", []byte("raw bytes: positive control"))
	subID := f.seedSubmission(t, msg, "rcpt@example.com")

	sender := &fakeSender{}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)

	if err := w.Handle(context.Background(), f.job(subID)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}

	if n := sender.callCount(); n != 1 {
		t.Fatalf("provider.Sender.Send was called %d time(s), want exactly 1 — "+
			"a writeback-enabled account must still be able to send", n)
	}
	got := sender.calls[0]
	if got.MailFrom != "sender@example.com" || len(got.RcptTo) != 1 || got.RcptTo[0] != "rcpt@example.com" {
		t.Errorf("send request = %+v, want MailFrom=sender@example.com RcptTo=[rcpt@example.com]", got)
	}

	sub := f.getSubmission(t, subID)
	if sub.State != store.SubmissionSent {
		t.Fatalf("submission state = %q, want %q", sub.State, store.SubmissionSent)
	}
}

// THE CONTROL FOR #601, AND THE REASON THAT CHANGE IS SAFE.
//
// #601 removed the recipient check from Build and from internal/api/draft.go so
// a draft with nobody in the To field can be saved — which is what stopped the
// composer silently discarding the covering note someone writes at the top of a
// forward before choosing who needs it. The justification was "the rule still
// exists, in the place where it is true: submission".
//
// That justification was UNPROVEN when it was made. PlanSubmit appeared in ZERO
// _test.go files in the tree, exactly like NewSendWorker before this file
// existed — so the guard the relaxation leans on had never been exercised, let
// alone exercised in a way that could fail. Relaxing Build while trusting an
// untested check would have removed the only TESTED recipient guard and called
// it a relocation.
//
// Recipients are read from THE BYTES (PlanSubmit re-parses the blob), so the
// fixture is a real draft whose RFC 5322 has no destination header at all.
func TestPlanSubmitStillRefusesADraftWithNoRecipients(t *testing.T) {
	f := newSendGateFixture(t)
	ctx := context.Background()
	c := &Composer{blobs: f.blobs, keys: fakeKeys{}}

	raw := []byte("From: sender@example.com\r\n" +
		"Subject: no recipients\r\n" +
		"Date: Thu, 17 Sep 2026 12:00:00 +0000\r\n" +
		"Message-ID: <norcpt@example.com>\r\n\r\nbody\r\n")
	msg := f.seedMessage(t, "no recipients", raw)
	f.markDraft(t, msg)

	var err error
	if terr := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err = c.PlanSubmit(ctx, tx, f.userID, SubmitRequest{EmailID: msg})
		return nil
	}); terr != nil {
		t.Fatalf("tx: %v", terr)
	}
	if !errors.Is(err, ErrNoRecipients) {
		t.Fatalf("PlanSubmit err = %v, want ErrNoRecipients — a draft with no "+
			"recipients may be SAVED (#601) but must never be SENT", err)
	}
}

// The positive half. Without it the test above is satisfied by a PlanSubmit that
// refuses everything, which would be a different bug wearing the same green tick.
func TestPlanSubmitAcceptsADraftWithARecipient(t *testing.T) {
	f := newSendGateFixture(t)
	ctx := context.Background()
	c := &Composer{blobs: f.blobs, keys: fakeKeys{}}

	raw := []byte("From: sender@example.com\r\n" +
		"To: them@example.net\r\n" +
		"Subject: has a recipient\r\n" +
		"Date: Thu, 17 Sep 2026 12:00:00 +0000\r\n" +
		"Message-ID: <rcpt@example.com>\r\n\r\nbody\r\n")
	msg := f.seedMessage(t, "has a recipient", raw)
	f.markDraft(t, msg)

	var err error
	if terr := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err = c.PlanSubmit(ctx, tx, f.userID, SubmitRequest{EmailID: msg})
		return nil
	}); terr != nil {
		t.Fatalf("tx: %v", terr)
	}
	if errors.Is(err, ErrNoRecipients) {
		t.Fatalf("PlanSubmit refused a draft that HAS a recipient: %v", err)
	}
}

// markDraft flags a seeded message as a draft. seedMessage does not, because the
// send-gate tests it was written for operate on already-submitted messages —
// PlanSubmit refuses a non-draft before it ever reaches the recipient check, so
// without this the control above fails for the wrong reason.
func (f *sendGateFixture) markDraft(t *testing.T, id ulid.ULID) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.messages SET is_draft = true WHERE user_id = $1 AND id = $2`,
		f.userID.String(), id.String()); err != nil {
		t.Fatalf("mark draft: %v", err)
	}
}
