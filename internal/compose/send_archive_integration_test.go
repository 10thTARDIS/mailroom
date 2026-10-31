// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package compose

// SEND & ARCHIVE ARCHIVES ONLY WHAT WAS SENT (#648).
//
// Observed with v1.80.0: the client archived the
// conversation as soon as EmailSubmission/set accepted the reply, the provider
// then refused it permanently (550 5.7.1, spam detected), and the conversation
// sat in Archive while the reply never left. The archive now happens in the
// send worker, in the transaction that records the submission as 'sent'.
//
// The tests drive the REAL SendWorker.Handle against a real store, with the
// parent seeded as the user's archive would find it: live provenance in INBOX,
// on an account that has an Archive mailbox. The failure, cancel and
// exhaustion tests are the negative controls; the sent test is the positive
// control that proves the fixture could archive at all.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// archiveBoxes are the three mailboxes the parent can be in.
type archiveBoxes struct {
	inbox, archive, stuff ulid.ULID
}

// seedArchiveMailboxes gives the fixture's account INBOX, an \Archive-role
// Archive, and a plain folder.
func (f *sendGateFixture) seedArchiveMailboxes(t *testing.T) archiveBoxes {
	t.Helper()
	b := archiveBoxes{inbox: store.NewULID(), archive: store.NewULID(), stuff: store.NewULID()}
	for _, m := range []struct {
		id         ulid.ULID
		name, role string
	}{{b.inbox, "INBOX", ""}, {b.archive, "Archive", `\Archive`}, {b.stuff, "Stuff", ""}} {
		if _, err := f.super.Exec(context.Background(), `
			INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
			VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''))`,
			m.id.String(), f.userID.String(), f.account.String(), m.name, m.role); err != nil {
			t.Fatalf("seed mailbox %s: %v", m.name, err)
		}
	}
	return b
}

// seedParent inserts the message being answered, with live provenance in mailbox.
func (f *sendGateFixture) seedParent(t *testing.T, subject string, mailbox ulid.ULID) ulid.ULID {
	t.Helper()
	id := f.seedMessage(t, subject, []byte("raw bytes: parent "+subject))
	if _, err := f.super.Exec(context.Background(), `
		INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		VALUES ($1, $2, $3, $4, $5, (SELECT COALESCE(max(uid), 0) + 1 FROM app.message_provenance), 1, 'live')`,
		store.NewULID().String(), f.userID.String(), id.String(), f.account.String(), mailbox.String()); err != nil {
		t.Fatalf("seed parent provenance: %v", err)
	}
	return id
}

// seedArchivingSubmission is seedSubmission with Send & archive requested.
func (f *sendGateFixture) seedArchivingSubmission(t *testing.T, msg ulid.ULID, archive ...ulid.ULID) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	var subID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		id, _, err := tx.CreateSubmission(ctx, store.SubmissionRow{
			AccountID: f.account, IdentityID: f.identity, MessageID: msg,
			FromAddress: "sender@example.com", EnvelopeTo: []string{"rcpt@example.com"},
			SendAt: time.Now().UTC(), ArchiveOnSuccess: archive,
		})
		subID = id
		return err
	}); err != nil {
		t.Fatalf("seed archiving submission: %v", err)
	}
	return subID
}

// parentState is everything an archive writes, read as the superuser: the
// canonical placement, the writeback move intents, and the Email change_log
// rows for the parent.
type parentState struct {
	moved       string
	moveIntents int
	changes     int
}

func (f *sendGateFixture) parentState(t *testing.T, id ulid.ULID) parentState {
	t.Helper()
	var s parentState
	if err := f.super.QueryRow(context.Background(), `
		SELECT COALESCE((SELECT moved_mailbox_id FROM app.messages WHERE user_id = $1 AND id = $2), ''),
		       (SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND message_id = $2 AND op = 'move'),
		       (SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2)`,
		f.userID.String(), id.String()).Scan(&s.moved, &s.moveIntents, &s.changes); err != nil {
		t.Fatalf("read parent state: %v", err)
	}
	return s
}

// assertUntouched fails if anything an archive would write has appeared.
func (f *sendGateFixture) assertUntouched(t *testing.T, id ulid.ULID, before parentState, why string) {
	t.Helper()
	after := f.parentState(t, id)
	if after != before {
		t.Fatalf("%s — but the parent changed: before %+v, after %+v", why, before, after)
	}
	if after.moved != "" {
		t.Fatalf("%s — but the parent was moved to %s", why, after.moved)
	}
}

// TestSendAndArchiveArchivesWhenSent is the positive control: a SENT
// submission archives its parent through the same path Email/set's archive
// takes — canonical placement, one writeback move intent, one change row.
func TestSendAndArchiveArchivesWhenSent(t *testing.T) {
	f := newSendGateFixture(t)
	boxes := f.seedArchiveMailboxes(t)
	parent := f.seedParent(t, "the conversation", boxes.inbox)
	reply := f.seedMessage(t, "the reply", []byte("raw bytes: reply that goes"))
	subID := f.seedArchivingSubmission(t, reply, parent)
	before := f.parentState(t, parent)

	sender := &fakeSender{}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)
	if err := w.Handle(context.Background(), f.job(subID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sub := f.getSubmission(t, subID); sub.State != store.SubmissionSent {
		t.Fatalf("submission state = %q, want %q", sub.State, store.SubmissionSent)
	}

	after := f.parentState(t, parent)
	if after.moved != boxes.archive.String() {
		t.Errorf("parent moved_mailbox_id = %q, want the Archive mailbox %s", after.moved, boxes.archive)
	}
	if n := after.moveIntents - before.moveIntents; n != 1 {
		t.Errorf("writeback move intents added = %d, want exactly 1", n)
	}
	if n := after.changes - before.changes; n != 1 {
		t.Errorf("Email change_log rows added for the parent = %d, want exactly 1 (the invariant)", n)
	}
}

// TestSendAndArchiveDoesNotArchiveAPermanentFailure is THE defect: the
// provider refuses the reply for good, so the conversation must stay put.
func TestSendAndArchiveDoesNotArchiveAPermanentFailure(t *testing.T) {
	f := newSendGateFixture(t)
	boxes := f.seedArchiveMailboxes(t)
	parent := f.seedParent(t, "the conversation", boxes.inbox)
	reply := f.seedMessage(t, "the reply", []byte("raw bytes: reply the provider refuses"))
	subID := f.seedArchivingSubmission(t, reply, parent)
	before := f.parentState(t, parent)

	sender := &fakeSender{err: fmt.Errorf("%w: 550 5.7.1 message rejected - spam detected", provider.ErrPermanent)}
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)
	if err := w.Handle(context.Background(), f.job(subID)); err != nil {
		t.Fatalf("Handle: %v (a permanent refusal is terminal, want nil)", err)
	}
	if n := sender.callCount(); n != 1 {
		t.Fatalf("sender called %d times, want 1 — the failure path was not reached", n)
	}
	if sub := f.getSubmission(t, subID); sub.State != store.SubmissionFailed {
		t.Fatalf("submission state = %q, want %q", sub.State, store.SubmissionFailed)
	}
	f.assertUntouched(t, parent, before, "the reply was refused permanently")
}

// TestSendAndArchiveDoesNotArchiveACancelledSend: Undo Send inside the window.
func TestSendAndArchiveDoesNotArchiveACancelledSend(t *testing.T) {
	f := newSendGateFixture(t)
	boxes := f.seedArchiveMailboxes(t)
	parent := f.seedParent(t, "the conversation", boxes.inbox)
	reply := f.seedMessage(t, "the reply", []byte("raw bytes: reply that is undone"))
	subID := f.seedArchivingSubmission(t, reply, parent)
	before := f.parentState(t, parent)

	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		return tx.CancelSubmission(ctx, subID)
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	sender := &fakeSender{t: t} // a cancelled send must never reach SMTP
	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, sender, nil)
	if err := w.Handle(ctx, f.job(subID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sub := f.getSubmission(t, subID); sub.State != store.SubmissionCanceled {
		t.Fatalf("submission state = %q, want %q", sub.State, store.SubmissionCanceled)
	}
	f.assertUntouched(t, parent, before, "the send was undone")
}

// TestSendAndArchiveDoesNotArchiveAnExhaustedSend: the job's last attempt
// fails transiently, so the submission is marked failed (#624 §4).
func TestSendAndArchiveDoesNotArchiveAnExhaustedSend(t *testing.T) {
	f := newSendGateFixture(t)
	boxes := f.seedArchiveMailboxes(t)
	parent := f.seedParent(t, "the conversation", boxes.inbox)
	reply := f.seedMessage(t, "the reply", []byte("raw bytes: reply that runs out of retries"))
	subID := f.seedArchivingSubmission(t, reply, parent)
	before := f.parentState(t, parent)

	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, &alwaysTransientSender{}, nil)
	job := f.job(subID)
	job.Attempts, job.MaxAttempts = 5, 5
	if err := w.Handle(context.Background(), job); err == nil {
		t.Fatal("Handle returned nil on the exhausting attempt; want the send error for the dispatcher")
	}
	if sub := f.getSubmission(t, subID); sub.State != store.SubmissionFailed {
		t.Fatalf("submission state = %q, want %q", sub.State, store.SubmissionFailed)
	}
	f.assertUntouched(t, parent, before, "the send ran out of retries")
}

// TestSendAndArchiveSkipsWhatLeftTheInboxOrIsGone: the client's "parent is in
// the Inbox" was true when the user pressed the button; by the time the send
// fires the user has filed the message elsewhere. The server re-checks and
// leaves the user's own choice alone. A second id that resolves to nothing
// (deleted, or never this user's) is skipped too — and neither blocks the
// send's record or a third, still-in-Inbox message's archive.
func TestSendAndArchiveSkipsWhatLeftTheInboxOrIsGone(t *testing.T) {
	f := newSendGateFixture(t)
	boxes := f.seedArchiveMailboxes(t)
	filed := f.seedParent(t, "filed while the send was queued", boxes.inbox)
	stillInbox := f.seedParent(t, "still in the inbox", boxes.inbox)
	gone := store.NewULID() // no such message for this user

	ctx := context.Background()
	// The user files it in Stuff (canonically; the IMAP move has not landed).
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, filed, store.EmailUpdate{MoveToRole: "", MoveToMailboxID: &boxes.stuff})
		return err
	}); err != nil {
		t.Fatalf("file the parent: %v", err)
	}
	filedBefore := f.parentState(t, filed)
	if filedBefore.moved != boxes.stuff.String() {
		t.Fatalf("fixture: filed parent moved_mailbox_id = %q, want Stuff", filedBefore.moved)
	}

	reply := f.seedMessage(t, "the reply", []byte("raw bytes: reply with stale targets"))
	subID := f.seedArchivingSubmission(t, reply, filed, gone, stillInbox)

	w := NewSendWorker(f.st, f.blobs, fakeKeys{}, &fakeSender{}, nil)
	if err := w.Handle(ctx, f.job(subID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sub := f.getSubmission(t, subID); sub.State != store.SubmissionSent {
		t.Fatalf("submission state = %q, want %q — a skipped archive must not affect the send", sub.State, store.SubmissionSent)
	}
	if got := f.parentState(t, filed); got != filedBefore {
		t.Errorf("the filed parent changed: before %+v, after %+v — the user's own filing was overridden", filedBefore, got)
	}
	if got := f.parentState(t, stillInbox); got.moved != boxes.archive.String() {
		t.Errorf("the still-in-Inbox parent moved_mailbox_id = %q, want Archive — a skipped sibling blocked it", got.moved)
	}

	// The per-message verdicts, read by re-running the store call on the now
	// 'sent' submission (idempotent: the archived one is already there, so it
	// reports a no-op archive rather than moving anything again).
	var outcomes []store.SendArchiveOutcome
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		outcomes, e = tx.ArchiveAfterSend(ctx, subID)
		return e
	}); err != nil {
		t.Fatalf("ArchiveAfterSend re-run: %v", err)
	}
	byID := map[ulid.ULID]store.SendArchiveResult{}
	for _, o := range outcomes {
		byID[o.MessageID] = o.Result
	}
	if byID[filed] != store.SendArchiveNotInInbox {
		t.Errorf("filed parent outcome = %q, want %q", byID[filed], store.SendArchiveNotInInbox)
	}
	if byID[gone] != store.SendArchiveGone {
		t.Errorf("unknown id outcome = %q, want %q", byID[gone], store.SendArchiveGone)
	}
	if byID[stillInbox] != store.SendArchiveNotInInbox {
		// Already archived by the worker, so on the re-run it is no longer in
		// the Inbox — which is the point: running it twice moves nothing twice.
		t.Errorf("re-run outcome for the archived parent = %q, want %q", byID[stillInbox], store.SendArchiveNotInInbox)
	}
}

// TestArchiveAfterSendRefusesAnUnsentSubmission is the store's own guard,
// independent of the worker: called for a submission that is not 'sent', it
// refuses and writes nothing.
func TestArchiveAfterSendRefusesAnUnsentSubmission(t *testing.T) {
	f := newSendGateFixture(t)
	boxes := f.seedArchiveMailboxes(t)
	parent := f.seedParent(t, "the conversation", boxes.inbox)
	reply := f.seedMessage(t, "the reply", []byte("raw bytes: reply still scheduled"))
	subID := f.seedArchivingSubmission(t, reply, parent)
	before := f.parentState(t, parent)

	ctx := context.Background()
	for _, state := range []string{store.SubmissionScheduled, store.SubmissionFailed} {
		if state == store.SubmissionFailed {
			if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
				return tx.FinishSubmission(ctx, subID, store.SubmissionFailed, "550 refused")
			}); err != nil {
				t.Fatalf("fail the submission: %v", err)
			}
		}
		var err error
		if terr := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
			_, err = tx.ArchiveAfterSend(ctx, subID)
			return nil
		}); terr != nil {
			t.Fatalf("tx: %v", terr)
		}
		if !errors.Is(err, store.ErrSubmissionNotSent) {
			t.Errorf("state %s: ArchiveAfterSend err = %v, want ErrSubmissionNotSent", state, err)
		}
		f.assertUntouched(t, parent, before, "ArchiveAfterSend was called for a "+state+" submission")
	}
}

// TestSendAndArchiveByAKeyKeepsNoLearnedPlacement (#49): the archive the
// worker performs after an API key's send is the key's, so it does not keep a
// learned placement on the user's behalf, and its writeback intent says
// 'agent'. The same send by a person keeps it (the positive control).
//
// RED PROBE: drop the byKey attribution in archiveOneAfterSend and the key's
// archive keeps the placement.
func TestSendAndArchiveByAKeyKeepsNoLearnedPlacement(t *testing.T) {
	f := newSendGateFixture(t)
	ctx := context.Background()
	boxes := f.seedArchiveMailboxes(t)
	var area store.AreaView
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		views, err := tx.ListAreas(ctx)
		if err == nil && len(views) > 0 {
			area = views[0]
		}
		return err
	}); err != nil || area.ID == (ulid.ULID{}) {
		t.Fatalf("areas: %v", err)
	}
	learnedParent := func(subject string) ulid.ULID {
		p := f.seedParent(t, subject, boxes.inbox)
		for _, q := range []string{
			`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by) VALUES ($1, $2, $3, 'learned')`,
			`INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id)
			 VALUES ($1, $2, 'address', 'x@example.com', (SELECT id FROM app.areas WHERE label_id = $3))`,
		} {
			if _, err := f.super.Exec(ctx, q, f.userID.String(), p.String(), area.LabelID.String()); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	outcome := func(m ulid.ULID) (o string) {
		_ = f.super.QueryRow(ctx, `SELECT outcome FROM app.learned_placements WHERE message_id=$1`, m.String()).Scan(&o)
		return o
	}
	for _, c := range []struct {
		who    string
		key    *ulid.ULID
		want   string
		source string
	}{{"an API key", ptrULID(store.NewULID()), store.PlacementOpen, store.IntentByAgent},
		{"a person (positive control)", nil, store.PlacementKept, store.IntentByUser}} {
		parent := learnedParent("conversation for " + c.who)
		reply := f.seedMessage(t, "reply by "+c.who, []byte("raw bytes: reply by "+c.who))
		var subID ulid.ULID
		if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
			id, _, err := tx.CreateSubmission(ctx, store.SubmissionRow{
				AccountID: f.account, IdentityID: f.identity, MessageID: reply,
				FromAddress: "sender@example.com", EnvelopeTo: []string{"rcpt@example.com"},
				SendAt: time.Now().UTC(), ArchiveOnSuccess: []ulid.ULID{parent}, APIKeyID: c.key,
			})
			subID = id
			return err
		}); err != nil {
			t.Fatal(err)
		}
		w := NewSendWorker(f.st, f.blobs, fakeKeys{}, &fakeSender{}, nil)
		if err := w.Handle(ctx, f.job(subID)); err != nil {
			t.Fatalf("%s: Handle: %v", c.who, err)
		}
		if moved := f.parentState(t, parent).moved; moved != boxes.archive.String() {
			t.Fatalf("%s: fixture: the parent was not archived (moved %q)", c.who, moved)
		}
		if got := outcome(parent); got != c.want {
			t.Errorf("%s's send and archive: placement %q, want %q", c.who, got, c.want)
		}
		var source string
		_ = f.super.QueryRow(ctx, `SELECT COALESCE(source, '') FROM app.writeback_intents
			WHERE user_id = $1 AND message_id = $2 AND op = 'move'`, f.userID.String(), parent.String()).Scan(&source)
		if source != c.source {
			t.Errorf("%s's archive intent source = %q, want %q", c.who, source, c.source)
		}
	}
}

func ptrULID(id ulid.ULID) *ulid.ULID { return &id }
