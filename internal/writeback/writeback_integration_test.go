// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package writeback

// End-to-end coverage for #296 against a real PostgreSQL with RLS enforced
// (connecting as mailroom_app, through the real StoreIntents adapter), with the
// recording fakeWriter from engine_test.go standing in for the IMAP server.
//
// It closes the gap the two unit suites leave. internal/store proves the
// resolver returns every live copy; internal/writeback's unit tests prove the
// engine writes to every target it is given. Only this file proves the two are
// actually wired to each other — which is the whole of the bug: both halves were
// individually reasonable and the join between them dropped every copy but one.
//
// The issue asked for this test in internal/store/writeback_integration_test.go.
// It cannot live there: internal/writeback imports internal/store, so a test in
// package store importing writeback back is "import cycle not allowed in test"
// (verified, not assumed). The store-side assertion lives there instead; the
// drain lives here.
//
// Runs against the throwaway Postgres from scripts/integration-test.sh and skips
// when the env is absent.

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// --- harness -----------------------------------------------------------------

type liveFixture struct {
	super   *pgxpool.Pool
	st      *store.Store
	userID  ulid.ULID
	account ulid.ULID
	inbox   ulid.ULID
	trash   ulid.ULID
}

func newLiveFixture(t *testing.T) *liveFixture {
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

	f := &liveFixture{
		super: super, st: store.NewFromPool(app),
		userID: store.NewULID(), account: store.NewULID(),
		inbox: store.NewULID(), trash: store.NewULID(),
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com")
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		f.account.String(), f.userID.String(), store.NewULID().String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	      VALUES ($1, $2, $3, 'INBOX', 'Inbox')`,
		f.inbox.String(), f.userID.String(), f.account.String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1, $2, $3, 'Trash', 'Trash', '\Trash')`,
		f.trash.String(), f.userID.String(), f.account.String())
	return f
}

// seedMessage inserts a canonical message (plus its blob and thread).
func (f *liveFixture) seedMessage(t *testing.T, subject string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	thread := store.NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), hash[:], store.NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		thread.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject,
	        from_address, received_at, size_bytes, message_id_hint)
	      VALUES ($1, $2, $3, $4, $5, 'friend@example.com', now(), 10, $6)`,
		id.String(), f.userID.String(), hash[:], thread.String(), subject,
		"<"+id.String()+"@example.com>")
	return id
}

// seedLiveCopy attaches one LIVE provenance row: a physical copy of the message
// at (mailbox, uidvalidity 1, uid). Calling it twice for the same message and
// account is exactly the shape re-delivered mail produces (#206) and the shape
// #296 is about.
func (f *liveFixture) seedLiveCopy(t *testing.T, msg, mailbox ulid.ULID, uid int64) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id,
		        mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, $6, 1, 'live')`,
		store.NewULID().String(), f.userID.String(), msg.String(), f.account.String(),
		mailbox.String(), uid); err != nil {
		t.Fatalf("seed live copy: %v", err)
	}
}

// markTrashedAndDue puts a message into the LOGICAL state the purge sweep
// requires before it would ever enqueue an EXPUNGE for it: trashed, with its
// retention deadline already past.
//
// ⚠ SEEDING COPIES INTO THE TRASH MAILBOX IS NOT THE SAME THING, and the
// difference is what #555 turned on. seedLiveCopy(…, f.trash, …) establishes
// where the copies physically are; trashed_at/purge_after are whether the USER
// asked for the message to be destroyed. Those two come apart exactly in this
// bug's window — a restore clears the columns at once while the copies stay in
// Trash until the MOVE is written back — which is why the engine now re-checks
// the logical state rather than the physical one.
//
// The purge tests below were written before that distinction mattered and armed
// intents for messages that were never logically trashed at all: a state the
// sweep cannot produce. They call this now so their fixture describes something
// production can actually reach.
func (f *liveFixture) markTrashedAndDue(t *testing.T, msg ulid.ULID) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.messages SET trashed_at = now() - interval '31 days',
		        purge_after = now() - interval '1 day'
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()); err != nil {
		t.Fatalf("mark trashed+due: %v", err)
	}
}

// enqueue records one writeback intent through the real store surface.
func (f *liveFixture) enqueue(t *testing.T, msg ulid.ULID, op store.IntentOp, payload, key string) ulid.ULID {
	t.Helper()
	var id ulid.ULID
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		var err error
		id, err = tx.EnqueueIntent(context.Background(), store.IntentRow{
			MessageID: &msg, AccountID: f.account, Op: op,
			Payload: []byte(payload), IdempotencyKey: key,
		})
		return err
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

// drain runs one real writeback pass over the account.
func (f *liveFixture) drain(t *testing.T, w IMAPWriter, opts ...Option) Result {
	t.Helper()
	e := NewEngine(NewStoreIntents(f.st, f.userID), opts...)
	res, err := e.ApplyPending(context.Background(), f.account, w)
	if err != nil {
		t.Fatalf("ApplyPending: %v", err)
	}
	return res
}

// intentState reads an intent's state as the superuser (ground truth, not
// through RLS).
func (f *liveFixture) intentState(t *testing.T, id ulid.ULID) string {
	t.Helper()
	var state string
	if err := f.super.QueryRow(context.Background(),
		`SELECT state::text FROM app.writeback_intents WHERE user_id = $1 AND id = $2`,
		f.userID.String(), id.String()).Scan(&state); err != nil {
		t.Fatalf("intent state: %v", err)
	}
	return state
}

// --- tests -------------------------------------------------------------------

// ⚠ THE LOAD-BEARING TEST FOR #296.
//
// A message with N live provenance rows in ONE account, one `-flag \Seen`
// intent, drained through the real store: the server must be told N times, once
// per copy. Before the fix it was told once, whatever N was — and because ingest
// merges every live copy's flags with OR, the untouched copies put \Seen straight
// back on the next poll. The user marks a message unread and it marks itself
// read again.
//
// THE copies=1 SUBTEST IS THE NEGATIVE CONTROL. Asserting "2 store calls" for a
// two-copy message proves nothing on its own — two claimed intents would produce
// the same number. Running the identical assertion against a ONE-copy message
// and getting 1 is what shows the count tracks copy multiplicity. Both subtests
// enqueue exactly one intent, and Applied is asserted at 1 in both.
func TestFlagRemovalIsWrittenBackToEveryLiveCopy(t *testing.T) {
	for _, copies := range []int{1, 2} {
		t.Run(map[int]string{1: "one live copy (negative control)", 2: "two live copies"}[copies], func(t *testing.T) {
			f := newLiveFixture(t)
			msg := f.seedMessage(t, "re-delivered mail")
			for i := 0; i < copies; i++ {
				f.seedLiveCopy(t, msg, f.inbox, int64(10+i))
			}
			id := f.enqueue(t, msg, store.IntentRemoveFlag, `{"flag":"\\Seen"}`,
				"unread-"+msg.String())

			w := &fakeWriter{}
			res := f.drain(t, w)

			if res.Applied != 1 {
				t.Fatalf("result = %+v, want exactly 1 intent applied", res)
			}
			got := w.storeCalls()
			if len(got) != copies {
				t.Fatalf("UID STORE calls = %d, want %d — one per live copy of the message in this account",
					len(got), copies)
			}
			wantUIDs := make([]int, copies)
			for i := range wantUIDs {
				wantUIDs[i] = 10 + i
			}
			gotUIDs := uidsOf(got)
			for i := range wantUIDs {
				if gotUIDs[i] != wantUIDs[i] {
					t.Fatalf("stored against UIDs %v, want %v", gotUIDs, wantUIDs)
				}
			}
			for _, c := range got {
				if c.mailbox != "INBOX" {
					t.Errorf("uid %d: mailbox = %q, want INBOX", c.uid, c.mailbox)
				}
				if len(c.remove) != 1 || string(c.remove[0]) != `\Seen` {
					t.Errorf("uid %d: remove = %v, want [\\Seen]", c.uid, c.remove)
				}
			}
			if s := f.intentState(t, id); s != string(store.IntentApplied) {
				t.Errorf("intent state = %q, want applied", s)
			}
		})
	}
}

// A purge destroys EVERY live copy in the account in one pass, and reports
// exactly that many destructions.
//
// Before the fix the sweep expunged one copy per account per pass and — because
// the purge idempotency key was enqueued without Revive — could never get a
// second pass: the applied row swallowed every later re-enqueue while still
// counting as enqueued. A message the user destroyed kept 1–3 copies on the
// server forever, its Trash entry never cleared, and nothing dead-lettered
// (#296).
func TestPurgeExpungeDestroysEveryLiveCopy(t *testing.T) {
	f := newLiveFixture(t)
	msg := f.seedMessage(t, "destroy every copy")
	f.seedLiveCopy(t, msg, f.trash, 4)
	f.seedLiveCopy(t, msg, f.trash, 5)
	f.seedLiveCopy(t, msg, f.trash, 6)
	// The sweep only ever enqueues for a message that is logically trashed and
	// past its deadline; arming one that is not describes a state production
	// cannot reach (#555).
	f.markTrashedAndDue(t, msg)

	var enqueued int
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		var err error
		accts, _, err := tx.ExpungeableAccountIDs(context.Background(), msg)
		if err != nil {
			return err
		}
		enqueued, err = tx.EnqueuePurgeExpungeIntents(context.Background(), msg, accts)
		return err
	}); err != nil {
		t.Fatalf("enqueue purge expunges: %v", err)
	}
	// One intent, because intents are per ACCOUNT. That is correct and is not
	// the bug: the bug was that one intent reached one COPY.
	if enqueued != 1 {
		t.Fatalf("enqueued %d intents, want 1 (a single account)", enqueued)
	}

	w := &fakeWriter{expungeHolds: map[coord]bool{
		{mailbox: "Trash", uid: 4}: true,
		{mailbox: "Trash", uid: 5}: true,
		{mailbox: "Trash", uid: 6}: true,
	}}
	res := f.drain(t, w)

	if res.Applied != 1 {
		t.Fatalf("result = %+v, want the one intent applied", res)
	}
	if n := len(w.callsOfKind("expunge")); n != 3 {
		t.Fatalf("expunge calls = %d, want 3 — one per live copy", n)
	}
	if res.Expunged != 3 {
		t.Fatalf("Expunged = %d, want 3 — the server confirmed removing three messages", res.Expunged)
	}
	for _, c := range w.callsOfKind("expunge") {
		if c.mailbox != "Trash" {
			t.Errorf("expunged in %q, want Trash", c.mailbox)
		}
	}
}

// ⚠ THE REGRESSION TEST FOR #296's SECOND DEFECT: a purge expunge that failed
// must be retryable.
//
// The purge key is deterministic per (message, account), and EnqueueIntent's
// ON CONFLICT arm is `WHERE $8::boolean AND …` — constant false without Revive.
// So once the intent reached a terminal state, every later sweep re-enqueued
// into it, changed nothing, returned the old id, and still counted an
// ExpungeEnqueued. Nothing retried and nothing surfaced.
//
// Here the server refuses the expunge until the attempt budget is spent, the
// intent dead-letters, and the NEXT sweep must find it pending again and finish
// the job.
func TestPurgeExpungeIsRetriedAfterItFails(t *testing.T) {
	f := newLiveFixture(t)
	msg := f.seedMessage(t, "expunge fails then succeeds")
	f.seedLiveCopy(t, msg, f.trash, 4)
	f.seedLiveCopy(t, msg, f.trash, 5)
	f.markTrashedAndDue(t, msg)

	sweep := func() {
		t.Helper()
		if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
			accts, _, err := tx.ExpungeableAccountIDs(context.Background(), msg)
			if err != nil {
				return err
			}
			_, err = tx.EnqueuePurgeExpungeIntents(context.Background(), msg, accts)
			return err
		}); err != nil {
			t.Fatalf("enqueue purge expunges: %v", err)
		}
	}

	sweep()
	// WithMaxAttempts(1): the first failure is terminal, which is the state that
	// used to be a one-way door.
	broken := &fakeWriter{failKinds: map[string]error{"expunge": errTestServerRefused}}
	res := f.drain(t, broken, WithMaxAttempts(1))
	if res.DeadLettered != 1 {
		t.Fatalf("result = %+v, want the intent dead-lettered", res)
	}

	var id ulid.ULID
	if err := f.super.QueryRow(context.Background(),
		`SELECT id FROM app.writeback_intents WHERE user_id = $1 AND message_id = $2`,
		f.userID.String(), msg.String()).Scan(&id); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if s := f.intentState(t, id); s != string(store.IntentDead) {
		t.Fatalf("intent state = %q, want dead before the re-sweep", s)
	}

	// The next sweep. Without Revive this is a silent no-op that still reports
	// success to the purge worker.
	sweep()
	if s := f.intentState(t, id); s != string(store.IntentPending) {
		t.Fatalf("intent state after the next sweep = %q, want pending — a terminal row must not swallow the re-enqueue", s)
	}
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND message_id = $2`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if n != 1 {
		t.Fatalf("writeback intents = %d, want 1 — reviving must re-arm the row, not add another", n)
	}

	// And the retry finishes the job: both copies destroyed.
	healthy := &fakeWriter{expungeHolds: map[coord]bool{
		{mailbox: "Trash", uid: 4}: true,
		{mailbox: "Trash", uid: 5}: true,
	}}
	res = f.drain(t, healthy)
	if res.Applied != 1 || res.Expunged != 2 {
		t.Fatalf("retry result = %+v, want 1 applied / 2 expunged", res)
	}
}

// A writeback pass mutates the SERVER, never the local row, so it must append no
// change_log rows: the JMAP-visible mutation already happened (and already
// logged its change) when the user acted, and app.writeback_intents is private
// machinery, not a JMAP object. If this ever goes red, a client's /changes
// cursor is being moved by something the user did not do.
func TestWritebackAppendsNoChangeLogRows(t *testing.T) {
	f := newLiveFixture(t)
	msg := f.seedMessage(t, "no change rows")
	f.seedLiveCopy(t, msg, f.inbox, 10)
	f.seedLiveCopy(t, msg, f.inbox, 11)
	f.enqueue(t, msg, store.IntentRemoveFlag, `{"flag":"\\Seen"}`, "nochange-"+msg.String())

	before := f.changeRows(t)
	if res := f.drain(t, &fakeWriter{}); res.Applied != 1 {
		t.Fatalf("result = %+v, want 1 applied", res)
	}
	if after := f.changeRows(t); after != before {
		t.Errorf("change_log rows went %d -> %d; a writeback pass must append none", before, after)
	}
}

func (f *liveFixture) changeRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1`,
		f.userID.String()).Scan(&n); err != nil {
		t.Fatalf("count change rows: %v", err)
	}
	return n
}

// errTestServerRefused stands in for an IMAP server that will not expunge.
var errTestServerRefused = errors.New("server refused the expunge")

// TestRestoredMessageIsNotExpungedByItsQueuedPurge pins #555.
//
// A purge EXPUNGE is enqueued when phase 1 decides every live copy of a message
// sits in that account's Trash and its retention has expired. That decision was
// then simply assumed to still hold whenever the drain got round to it — and it
// can stop holding in the minutes in between. The user restores the message;
// the EXPUNGE is older than the restore's MOVE so it is claimed first; the
// engine resolves live provenance, still finds the copy in Trash because the
// MOVE has not been written back yet, and destroys it. The user asked to keep
// the message and its only server copy is gone.
//
// ⚠ THE RESTORE HERE IS DELIBERATELY RAW SQL, NOT ApplyEmailUpdate. That is what
// makes this a test of the ENGINE's apply-time re-check rather than of the
// store's cancel-on-restore. Both halves exist (#555 option 3) and they fix
// DIFFERENT cases: the cancel only catches a restore that goes through
// Mailroom, while a copy can also leave Trash with no local transaction to hook
// onto at all — another client moves it and Mailroom learns at the next sync.
// Driving this through ApplyEmailUpdate would let the cancel satisfy it and
// leave the general case untested, which is the case that has no other guard.
//
// ⚠ HOW THIS WAS PROVED TO FAIL, stated as what was actually run rather than
// as the mutation one would expect. The first implementation of the guard was
// PRESENT but consulted the wrong predicate: it re-asked ExpungeableAccountIDs,
// which answers a PHYSICAL question (is every live copy in a \Trash mailbox)
// and not the logical one. A restore clears trashed_at/purge_after at once
// while the copy stays physically in Trash until the MOVE is written back — so
// the re-check reported "still eligible" and this test went red at its own
// assertions: `Expunged = 1, want 0` (line ~484) and `Withdrawn = 0, want 1`,
// with the control below green throughout.
//
// That is stronger evidence than deleting the branch would have been. It shows
// the test is sensitive to the guard being CORRECT, not merely present — a
// distinction that matters here, because the plausible-looking wrong version
// would have shipped a counter that sits at zero forever while the mail-
// destroying bug stayed fully intact.
func TestRestoredMessageIsNotExpungedByItsQueuedPurge(t *testing.T) {
	f := newLiveFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "restore race")
	f.seedLiveCopy(t, msg, f.trash, 77)

	f.markTrashedAndDue(t, msg)

	// Arm the EXPUNGE through the real purge path, not by hand.
	var enqueued, nAccounts int
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		accts, _, err := tx.ExpungeableAccountIDs(ctx, msg)
		if err != nil {
			return err
		}
		nAccounts = len(accts)
		enqueued, err = tx.EnqueuePurgeExpungeIntents(ctx, msg, accts)
		return err
	}); err != nil {
		t.Fatalf("enqueue purge expunge: %v", err)
	}
	// ARMING CONTROLS: without these a green result could mean "nothing was ever
	// queued", which is not the same claim at all.
	if nAccounts != 1 {
		t.Fatalf("ExpungeableAccountIDs returned %d accounts, want 1 — nothing was armed, so this "+
			"test proves nothing about #555", nAccounts)
	}
	if enqueued != 1 {
		t.Fatalf("EnqueuePurgeExpungeIntents enqueued %d, want 1 — nothing was armed, so this "+
			"test proves nothing about #555", enqueued)
	}

	// The user restores it. Provenance is deliberately left alone: the server
	// copy is still physically in Trash, because the restore's MOVE back to
	// INBOX has not been written back yet. That is the whole race.
	if _, err := f.super.Exec(ctx,
		`UPDATE app.messages SET trashed_at = NULL, purge_after = NULL
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()); err != nil {
		t.Fatalf("seed restore: %v", err)
	}

	w := &fakeWriter{}
	res := f.drain(t, w)

	if res.Expunged != 0 {
		t.Errorf("res.Expunged = %d, want 0 — a restore that leaves a queued purge EXPUNGE in "+
			"place destroyed mail the user just asked to keep (#555)", res.Expunged)
	}
	for _, c := range w.callsOfKind("expunge") {
		if c.mailbox == "Trash" && c.uid == 77 {
			t.Errorf("engine issued EXPUNGE against mailbox=%q uid=%d — the restored message was "+
				"expunged from the server anyway", c.mailbox, c.uid)
		}
	}
	// The guard must be OBSERVABLE, not merely effective: a withdrawn intent
	// leaves no row, so this counter and the runner's log line are the only
	// evidence an operator can ever read that it fired.
	if res.Withdrawn != 1 {
		t.Errorf("res.Withdrawn = %d, want 1 — the expunge was not applied, but nothing recorded "+
			"that the guard is why, which is indistinguishable from it never having been queued",
			res.Withdrawn)
	}
}

// TestStillTrashedMessageIsStillExpunged is the positive control for the test
// above, and it is what stops the #555 fix from being "never expunge anything".
//
// Identical fixture and identical drain, WITHOUT the restore: the message is
// still trashed and still overdue, so the purge must go through and destroy the
// copy. If this goes red the re-check is refusing legitimate purges, which
// would leave Trash never actually emptying on the server — a quieter failure
// than #555 but a real one.
func TestStillTrashedMessageIsStillExpunged(t *testing.T) {
	f := newLiveFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "restore race control")
	f.seedLiveCopy(t, msg, f.trash, 77)

	f.markTrashedAndDue(t, msg)

	var enqueued, nAccounts int
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		accts, _, err := tx.ExpungeableAccountIDs(ctx, msg)
		if err != nil {
			return err
		}
		nAccounts = len(accts)
		enqueued, err = tx.EnqueuePurgeExpungeIntents(ctx, msg, accts)
		return err
	}); err != nil {
		t.Fatalf("enqueue purge expunge: %v", err)
	}
	if nAccounts != 1 || enqueued != 1 {
		t.Fatalf("armed %d accounts / %d intents, want 1/1 — control UNTESTED", nAccounts, enqueued)
	}

	// No restore this time.

	w := &fakeWriter{}
	res := f.drain(t, w)

	if res.Expunged != 1 {
		t.Fatalf("res.Expunged = %d, want 1 — a message that IS still trashed and overdue was not "+
			"purged, so the #555 re-check is refusing legitimate purges and Trash never empties "+
			"on the server", res.Expunged)
	}
	if res.Withdrawn != 0 {
		t.Errorf("res.Withdrawn = %d, want 0 — the guard fired on a message that was still "+
			"genuinely due", res.Withdrawn)
	}
}
