// SPDX-License-Identifier: AGPL-3.0-only

package writeback

// GMAIL WRITEBACK, END TO END OVER THE WIRE (#52, ADR-047 §3, increment 5b).
//
// Every test here drives the production engine in Gmail mode through the
// production sync.Dial / sync.Writer / vendored go-imap client against the
// scripted fake Gmail (internal/sync/gmailtest), and asserts on the FAKE'S
// COMMAND LOG and mailbox state — what reached the server and what it did —
// not on the engine's own bookkeeping. The store is the in-memory fakeStore,
// so no database is needed.
//
// The negative controls the mail-mutating paths owe (CLAUDE.md) are the
// assertions on MutatingCommands() and Expunged(): an archive is exactly one
// -X-GM-LABELS STORE; a label removal never MOVEs or EXPUNGEs; a delete is a
// MOVE to Trash and nothing else; a hard delete EXPUNGEs only in Trash; a
// draft retirement never touches the sent copy that shares its Message-ID.
// Each of those mechanisms was deleted or broken by hand once to see the
// test go red; the probe record is in the PR description.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

const (
	gmAll    = "[Gmail]/All Mail"
	gmTrash  = "[Gmail]/Trash"
	gmSpam   = "[Gmail]/Spam"
	gmDrafts = "[Gmail]/Drafts"
)

type gmailRig struct {
	t       *testing.T
	srv     *gmailtest.Server
	st      *fakeStore
	acct    ulid.ULID
	folders store.GmailFolders
}

func newGmailRig(t *testing.T) *gmailRig {
	t.Helper()
	srv := gmailtest.New(t)
	srv.StandardMailboxes()
	return &gmailRig{
		t: t, srv: srv, st: newFakeStore(), acct: store.NewULID(),
		folders: store.GmailFolders{
			All: gmAll, Trash: gmTrash, Spam: gmSpam, Drafts: gmDrafts, Inbox: "INBOX",
			Reserved: map[string]bool{
				gmAll: true, gmTrash: true, gmSpam: true, gmDrafts: true,
				"[Gmail]/Sent Mail": true, "[Gmail]/Starred": true, "[Gmail]/Important": true,
			},
		},
	}
}

// put files a message in a fake mailbox and returns it (UID assigned).
func (r *gmailRig) put(mailbox string, msgID uint64, labels ...string) *gmailtest.Message {
	if labels == nil {
		labels = []string{}
	}
	return r.srv.Append(mailbox, &gmailtest.Message{MsgID: msgID, Labels: labels, Body: []byte("x")})
}

// copyOf is Mailroom's provenance for a fake message.
func copyOf(mailbox string, m *gmailtest.Message) store.ProvenanceTarget {
	return store.ProvenanceTarget{
		ProvenanceID: store.NewULID(), MailboxID: store.NewULID(), IMAPName: mailbox,
		UID: int64(m.UID), UIDValidity: 1, State: store.ProvenanceLive, GmMsgID: m.MsgID,
	}
}

// intent queues one intent for the rig's account on a message with the
// given copies.
func (r *gmailRig) intent(op store.IntentOp, p intentPayload, copies ...store.ProvenanceTarget) store.Intent {
	r.t.Helper()
	msg := store.NewULID()
	body, err := json.Marshal(p)
	if err != nil {
		r.t.Fatal(err)
	}
	in := store.Intent{ID: store.NewULID(), MessageID: &msg, AccountID: r.acct, Op: op,
		Payload: body, IdempotencyKey: "k-" + store.NewULID().String(), State: store.IntentPending}
	r.st.add(in)
	if len(copies) > 0 {
		r.st.prov[provKey(msg, r.acct)] = copies
	}
	return in
}

// headerIntent queues a header-addressed expunge (draft retirement).
func (r *gmailRig) headerIntent(mailbox, messageID string) store.Intent {
	r.t.Helper()
	body, _ := json.Marshal(intentPayload{Mailbox: mailbox, MessageIDHeader: messageID})
	in := store.Intent{ID: store.NewULID(), AccountID: r.acct, Op: store.IntentExpunge,
		Payload: body, IdempotencyKey: "h-" + store.NewULID().String(), State: store.IntentPending,
		Source: store.IntentByDraft}
	r.st.add(in)
	return in
}

// drain runs one Gmail-mode pass over a real session to the fake.
func (r *gmailRig) drain(opts ...Option) Result {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := imapsync.Dial(ctx, r.srv.Listen(imapsync.TLSInsecure))
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	defer func() { _ = sess.Close() }()
	e := NewEngine(r.st, append([]Option{WithGmail(r.folders)}, opts...)...)
	res, err := e.ApplyPending(ctx, r.acct, imapsync.NewWriter(sess))
	if err != nil {
		r.t.Fatalf("apply: %v", err)
	}
	return res
}

func (r *gmailRig) state(in store.Intent) store.IntentState { return r.st.intents[in.ID].State }

func (r *gmailRig) labels(mailbox string, uid uint32) []string {
	m := r.srv.Message(mailbox, uid)
	if m == nil {
		r.t.Fatalf("no uid %d in %s", uid, mailbox)
	}
	return m.Labels
}

// mustOnly asserts the server saw exactly these mutating commands.
func (r *gmailRig) mustOnly(want ...string) {
	r.t.Helper()
	got := r.srv.MutatingCommands()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		r.t.Errorf("mutating commands =\n  %q\nwant\n  %q", got, want)
	}
}

func (r *gmailRig) noExpunge() {
	r.t.Helper()
	if x := r.srv.Expunged(); len(x) > 0 {
		r.t.Errorf("messages EXPUNGED: %q", x)
	}
	for _, c := range r.srv.Commands() {
		u := strings.ToUpper(c)
		if strings.Contains(u, "EXPUNGE") || strings.Contains(u, `\DELETED`) {
			r.t.Errorf("a destructive command reached the server: %q", c)
		}
	}
}

func TestGmailArchiveRemovesTheInboxLabelAndNothingElse(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 101, `\Inbox`, "Work")
	cp := copyOf(gmAll, m)
	in := r.intent(store.IntentMove, intentPayload{Mailbox: gmAll, TargetRole: "archive"}, cp)

	res := r.drain()

	if r.state(in) != store.IntentApplied || res.Applied != 1 {
		t.Fatalf("state = %s, res = %+v", r.state(in), res)
	}
	if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{"Work"}) {
		t.Errorf("labels after archive = %q, want [Work]", got)
	}
	// NEGATIVE CONTROL: exactly one label STORE — no MOVE, no \Deleted, no
	// EXPUNGE; the message is still at its All Mail UID.
	r.mustOnly(`UID STORE 1 -X-GM-LABELS (\Inbox)`)
	r.noExpunge()
	// Echo suppression: the confirmed delta goes on the copy's baseline.
	if len(r.st.gmailRecorded) != 1 || !reflect.DeepEqual(r.st.gmailRecorded[0].remove, []string{`\Inbox`}) ||
		r.st.gmailRecorded[0].prov != cp.ProvenanceID {
		t.Errorf("baseline delta = %+v", r.st.gmailRecorded)
	}
}

func TestGmailMoveToInboxAddsTheInboxLabel(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 102, "Work")
	in := r.intent(store.IntentMove, intentPayload{Mailbox: "INBOX", TargetRole: "inbox"}, copyOf(gmAll, m))
	r.drain()
	if r.state(in) != store.IntentApplied {
		t.Fatalf("state = %s (%s)", r.state(in), r.st.intents[in.ID].LastError)
	}
	if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{"Work", `\Inbox`}) {
		t.Errorf("labels = %q", got)
	}
	r.mustOnly(`UID STORE 1 +X-GM-LABELS (\Inbox)`)
	r.noExpunge()
}

func TestGmailLabelAddAndRemoveIncludingNonASCII(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 103, `\Inbox`)
	cp := copyOf(gmAll, m)
	add := r.intent(store.IntentAddKeyword, intentPayload{Keyword: "ete-2026", Label: "Été 2026"}, cp)
	r.drain()
	if r.state(add) != store.IntentApplied {
		t.Fatalf("add state = %s (%s)", r.state(add), r.st.intents[add.ID].LastError)
	}
	if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{`\Inbox`, "Été 2026"}) {
		t.Errorf("labels after add = %q", got)
	}
	// The label went on the wire in modified UTF-7, quoted, and NOT as the
	// keyword atom: Gmail's apps show labels, not keywords.
	r.mustOnly(`UID STORE 1 +X-GM-LABELS ("&AMk-t&AOk- 2026")`)

	rm := r.intent(store.IntentRemoveKeyword, intentPayload{Keyword: "ete-2026", Label: "Été 2026"}, cp)
	r.drain()
	if r.state(rm) != store.IntentApplied {
		t.Fatalf("remove state = %s (%s)", r.state(rm), r.st.intents[rm.ID].LastError)
	}
	if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{`\Inbox`}) {
		t.Errorf("labels after remove = %q", got)
	}
	// NEGATIVE CONTROL: a label removal is a label removal. The message is
	// still in All Mail at the same UID, and nothing destructive was sent.
	removal := r.srv.MutatingCommands()[1:] // after the add's one STORE
	if want := []string{`UID STORE 1 -X-GM-LABELS ("&AMk-t&AOk- 2026")`}; !reflect.DeepEqual(removal, want) {
		t.Errorf("removal commands = %q, want %q", removal, want)
	}
	r.noExpunge()
	if r.srv.Message(gmAll, m.UID) == nil {
		t.Error("a label removal deleted the message")
	}
	if got := r.st.gmailRecorded; len(got) != 2 || !reflect.DeepEqual(got[0].add, []string{"Été 2026"}) ||
		!reflect.DeepEqual(got[1].remove, []string{"Été 2026"}) {
		t.Errorf("baseline deltas = %+v", got)
	}
	// A label op never records a keyword on the copy (#576's record is for
	// FLAGS only; the atom never reached the server).
	if len(r.st.recorded) != 0 {
		t.Errorf("stored-flag records = %+v, want none", r.st.recorded)
	}
}

// TestGmailLabelCreation exercises each UNVERIFIED world of "does STORE
// create a missing label?" (ADR-047, Consequences).
func TestGmailLabelCreation(t *testing.T) {
	for _, tc := range []struct {
		world      string
		wantCreate bool
	}{
		{gmailtest.LabelStoreCreates, false},
		{gmailtest.LabelStoreNO, true},
		{gmailtest.LabelStoreIgnores, true},
	} {
		t.Run("world="+tc.world, func(t *testing.T) {
			r := newGmailRig(t)
			r.srv.LabelCreate = tc.world
			m := r.put(gmAll, 104, `\Inbox`)
			in := r.intent(store.IntentAddKeyword, intentPayload{Keyword: "brand-new", Label: "Brand new"}, copyOf(gmAll, m))
			res := r.drain()
			if r.state(in) != store.IntentApplied {
				t.Fatalf("state = %s (%s)", r.state(in), r.st.intents[in.ID].LastError)
			}
			if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{`\Inbox`, "Brand new"}) {
				t.Errorf("labels = %q", got)
			}
			created := false
			for _, c := range r.srv.Commands() {
				if strings.HasPrefix(c, "CREATE") {
					created = true
				}
			}
			if created != tc.wantCreate || (res.LabelsCreated == 1) != tc.wantCreate {
				t.Errorf("CREATE sent = %v, LabelsCreated = %d, want create = %v; commands %q",
					created, res.LabelsCreated, tc.wantCreate, r.srv.Commands())
			}
			r.noExpunge()
		})
	}
}

// TestGmailLabelThatNeverSticksIsNotApplied: a server that accepts the STORE
// and the CREATE but never applies the label must not be recorded as done.
func TestGmailLabelThatNeverSticksIsNotApplied(t *testing.T) {
	r := newGmailRig(t)
	r.srv.LabelCreate = gmailtest.LabelStoreNever
	m := r.put(gmAll, 105, `\Inbox`)
	in := r.intent(store.IntentAddKeyword, intentPayload{Keyword: "x", Label: "Ghost"}, copyOf(gmAll, m))
	r.drain()
	if got := r.state(in); got != store.IntentPending {
		t.Fatalf("a label that never appeared was recorded as applied; labels = %q", r.labels(gmAll, m.UID))
	}
	if len(r.st.gmailRecorded) != 0 {
		t.Errorf("baseline moved for a write that did not happen: %+v", r.st.gmailRecorded)
	}
}

func TestGmailDeleteMovesToTrashAndNeverExpunges(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 106, `\Inbox`, "Work")
	in := r.intent(store.IntentMove, intentPayload{Mailbox: gmTrash, TargetRole: "trash"}, copyOf(gmAll, m))
	r.drain()
	if r.state(in) != store.IntentApplied {
		t.Fatalf("state = %s (%s)", r.state(in), r.st.intents[in.ID].LastError)
	}
	if r.srv.Message(gmAll, m.UID) != nil {
		t.Error("still in All Mail after delete")
	}
	if got := r.srv.FindMsgID(gmTrash, 106); len(got) != 1 {
		t.Errorf("in Trash: %v, want one copy", got)
	}
	r.mustOnly(`UID MOVE 1 "[Gmail]/Trash"`)
	r.noExpunge()
}

func TestGmailRestoreFromTrash(t *testing.T) {
	for _, tc := range []struct {
		name, dest, role string
		wantInbox        bool
	}{
		{"to inbox", "INBOX", "inbox", true},
		{"to archive", gmAll, "archive", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGmailRig(t)
			m := r.put(gmTrash, 107, "Work")
			in := r.intent(store.IntentMove, intentPayload{Mailbox: tc.dest, TargetRole: tc.role}, copyOf(gmTrash, m))
			r.drain()
			if r.state(in) != store.IntentApplied {
				t.Fatalf("state = %s (%s)", r.state(in), r.st.intents[in.ID].LastError)
			}
			back := r.srv.FindMsgID(gmAll, 107)
			if len(back) != 1 || len(r.srv.FindMsgID(gmTrash, 107)) != 0 {
				t.Fatalf("after restore: All Mail %v, Trash %v", back, r.srv.FindMsgID(gmTrash, 107))
			}
			hasInbox := false
			for _, l := range r.labels(gmAll, back[0]) {
				hasInbox = hasInbox || l == `\Inbox`
			}
			if hasInbox != tc.wantInbox {
				t.Errorf("labels after restore = %q", r.labels(gmAll, back[0]))
			}
			r.noExpunge()
			// F2: never a MOVE into All Mail; the restore is the standard
			// untrash, a MOVE to INBOX (then, for the archive, -\Inbox).
			want := []string{`UID MOVE 1 INBOX`}
			if !tc.wantInbox {
				want = append(want, `UID STORE 1 -X-GM-LABELS (\Inbox)`)
			}
			r.mustOnly(want...)
		})
	}
}

// TestGmailMoveOutOfTrashThatLosesTheMessageIsDetected (#911 review F2): a
// server whose MOVE out of Trash drops the message (a COPY treated as a
// no-op, then the expunge half). The engine must notice — the message is not
// in the destination — and dead-letter loudly, not report success and not
// go on to archive.
func TestGmailMoveOutOfTrashThatLosesTheMessageIsDetected(t *testing.T) {
	for _, tc := range []struct{ name, dest, role string }{
		{"to inbox", "INBOX", "inbox"},
		{"to archive", gmAll, "archive"},
		{"to spam", gmSpam, "junk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGmailRig(t)
			r.srv.MoveDrops = true
			m := r.put(gmTrash, 120, "Work")
			in := r.intent(store.IntentMove, intentPayload{Mailbox: tc.dest, TargetRole: tc.role}, copyOf(gmTrash, m))
			r.drain()
			got := r.st.intents[in.ID]
			if got.State != store.IntentDead || !strings.Contains(got.DeadReason, "copies of it, not 1") {
				t.Fatalf("state %s, reason %q: a lost message was not detected", got.State, got.DeadReason)
			}
			for _, c := range r.srv.MutatingCommands() {
				if strings.Contains(c, "X-GM-LABELS") {
					t.Errorf("went on to %q after losing the message", c)
				}
			}
		})
	}
}

// TestGmailMoveOutOfTrashNeedsAMsgID: with no X-GM-MSGID the move could not
// be checked, so it is refused before anything is sent.
func TestGmailMoveOutOfTrashNeedsAMsgID(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmTrash, 121)
	cp := copyOf(gmTrash, m)
	cp.GmMsgID = 0
	in := r.intent(store.IntentMove, intentPayload{Mailbox: "INBOX", TargetRole: "inbox"}, cp)
	r.drain()
	if r.state(in) != store.IntentDead {
		t.Fatalf("state %s, want dead", r.state(in))
	}
	r.mustOnly()
}

// TestGmailDraftRetirementRefusesAMessageThatIsNotAnUnsentDraft (#911 review
// F1): the Drafts-view match is checked for \Draft and against \Sent.
func TestGmailDraftRetirementRefusesAMessageThatIsNotAnUnsentDraft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"carries \\Sent", []string{`\Draft`, `\Sent`}},
		{"no \\Draft", []string{"Work"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGmailRig(t)
			const mid = "<d@example.com>"
			r.srv.Append(gmAll, &gmailtest.Message{MsgID: 210, Labels: tc.labels, MessageID: mid, Body: []byte("d")})
			r.srv.Append(gmDrafts, &gmailtest.Message{MsgID: 210, Labels: tc.labels, MessageID: mid, Body: []byte("d")})
			in := r.headerIntent(gmDrafts, mid)
			r.drain()
			if r.state(in) != store.IntentDead {
				t.Fatalf("state %s, want dead", r.state(in))
			}
			r.mustOnly()
			if len(r.srv.FindMsgID(gmAll, 210)) != 1 {
				t.Error("the message was removed")
			}
		})
	}
}

// TestGmailDraftRetirementWithoutMoveIsRefused (#911 review F3): no MOVE
// means go-imap's COPY + \Deleted + EXPUNGE fallback in Drafts; refused.
func TestGmailDraftRetirementWithoutMoveIsRefused(t *testing.T) {
	r := newGmailRig(t)
	r.srv.NoMove = true
	const mid = "<d2@example.com>"
	r.srv.Append(gmDrafts, &gmailtest.Message{MsgID: 211, Labels: []string{`\Draft`}, MessageID: mid, Body: []byte("d")})
	in := r.headerIntent(gmDrafts, mid)
	r.drain()
	if r.state(in) != store.IntentPending {
		t.Fatalf("state %s, want pending", r.state(in))
	}
	r.mustOnly()
	r.noExpunge()
}

// TestGmailDraftRetirementWithTwoTrashMatchesDoesNotExpunge (#911 review
// F3): after the MOVE, X-GM-MSGID matching more than one message in Trash
// means the target is ambiguous; nothing is expunged.
func TestGmailDraftRetirementWithTwoTrashMatchesDoesNotExpunge(t *testing.T) {
	r := newGmailRig(t)
	const mid = "<d3@example.com>"
	r.srv.Append(gmDrafts, &gmailtest.Message{MsgID: 212, Labels: []string{`\Draft`}, MessageID: mid, Body: []byte("d")})
	r.srv.AddToTrash(212)
	in := r.headerIntent(gmDrafts, mid)
	r.drain()
	if r.state(in) != store.IntentPending {
		t.Fatalf("state %s, want pending", r.state(in))
	}
	r.noExpunge()
}

// TestGmailRefusesACopyOutsideTheSyncedMailboxes (#911 review F3): a copy
// in a label view or INBOX is not one this model put there.
func TestGmailRefusesACopyOutsideTheSyncedMailboxes(t *testing.T) {
	for _, mb := range []string{"Work", "INBOX"} {
		r := newGmailRig(t)
		m := r.put(mb, 213, `\Inbox`)
		in := r.intent(store.IntentAddKeyword, intentPayload{Keyword: "x", Label: "Receipts"}, copyOf(mb, m))
		r.drain()
		if r.state(in) != store.IntentDead {
			t.Errorf("copy in %s: state %s, want dead", mb, r.state(in))
		}
		r.mustOnly()
	}
}

// TestGmailAmbiguousTrashIsRefused (#911 review F9): if the Trash name is
// also All Mail's, a draft retirement neither moves nor expunges.
func TestGmailAmbiguousTrashIsRefused(t *testing.T) {
	r := newGmailRig(t)
	r.folders.Trash = gmAll
	const mid = "<d4@example.com>"
	r.srv.Append(gmDrafts, &gmailtest.Message{MsgID: 214, Labels: []string{`\Draft`}, MessageID: mid, Body: []byte("d")})
	in := r.headerIntent(gmDrafts, mid)
	r.drain()
	if r.state(in) != store.IntentDead {
		t.Fatalf("state %s, want dead", r.state(in))
	}
	r.mustOnly()
}

// TestGmailWithoutUIDPlusWritesNothing: Gmail mode requires UIDPLUS rather
// than risk a plain EXPUNGE.
func TestGmailWithoutUIDPlusWritesNothing(t *testing.T) {
	r := newGmailRig(t)
	r.srv.NoUIDPlus = true
	m := r.put(gmTrash, 215)
	in := r.intent(store.IntentExpunge, intentPayload{}, copyOf(gmTrash, m))
	lab := r.intent(store.IntentMove, intentPayload{Mailbox: gmAll, TargetRole: "archive"}, copyOf(gmTrash, m))
	r.drain()
	if r.state(in) != store.IntentPending || r.state(lab) != store.IntentPending {
		t.Fatalf("states %s %s, want pending", r.state(in), r.state(lab))
	}
	r.mustOnly()
}

// TestGmailLabelViewMoveKeepsTheInboxWhenTheLabelDoesNotStick (#911 review
// F5): -\Inbox is only sent once the label is confirmed.
func TestGmailLabelViewMoveKeepsTheInboxWhenTheLabelDoesNotStick(t *testing.T) {
	r := newGmailRig(t)
	r.srv.LabelCreate = gmailtest.LabelStoreNever
	m := r.put(gmAll, 216, `\Inbox`)
	in := r.intent(store.IntentMove, intentPayload{Mailbox: "Receipts"}, copyOf(gmAll, m))
	r.drain()
	if r.state(in) == store.IntentApplied {
		t.Fatal("applied although the label never stuck")
	}
	if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{`\Inbox`}) {
		t.Errorf("labels = %q: the message was archived without its label", got)
	}
	for _, c := range r.srv.MutatingCommands() {
		if strings.Contains(c, "-X-GM-LABELS") {
			t.Errorf("sent %q before the label was confirmed", c)
		}
	}
}

// TestGmailRefusesUserLabelsNamedLikeSystemFolders (#911 review F6).
func TestGmailRefusesUserLabelsNamedLikeSystemFolders(t *testing.T) {
	for _, tc := range []struct {
		op    store.IntentOp
		label string
	}{
		{store.IntentRemoveKeyword, "Inbox"},
		{store.IntentAddKeyword, "Trash"},
		{store.IntentAddKeyword, "sent mail"},
		{store.IntentAddKeyword, "[Gmail]/Starred"},
	} {
		r := newGmailRig(t)
		m := r.put(gmAll, 217, `\Inbox`)
		in := r.intent(tc.op, intentPayload{Keyword: "x", Label: tc.label}, copyOf(gmAll, m))
		r.drain()
		if got := r.st.intents[in.ID]; got.State != store.IntentDead || !strings.Contains(got.DeadReason, "system folder") {
			t.Errorf("%s %q: state %s reason %q", tc.op, tc.label, got.State, got.DeadReason)
		}
		r.mustOnly()
	}
}

// TestGmailLabelCaseFollowsGmail (#911 review F7): a Mailroom "work" written
// to a Gmail that has "Work" is confirmed, applied once, and recorded on the
// baseline as Gmail spells it.
func TestGmailLabelCaseFollowsGmail(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 218, `\Inbox`)
	in := r.intent(store.IntentAddKeyword, intentPayload{Keyword: "work", Label: "work"}, copyOf(gmAll, m))
	res := r.drain()
	if r.state(in) != store.IntentApplied || res.LabelsCreated != 0 {
		t.Fatalf("state %s, created %d (%s)", r.state(in), res.LabelsCreated, r.st.intents[in.ID].LastError)
	}
	if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{`\Inbox`, "Work"}) {
		t.Errorf("labels = %q", got)
	}
	if len(r.st.gmailRecorded) != 1 || !reflect.DeepEqual(r.st.gmailRecorded[0].add, []string{"Work"}) {
		t.Errorf("baseline delta = %+v, want Gmail's spelling", r.st.gmailRecorded)
	}
}

func TestGmailSpamAndNotSpam(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 108, `\Inbox`)
	spam := r.intent(store.IntentMove, intentPayload{Mailbox: gmSpam, TargetRole: "junk"}, copyOf(gmAll, m))
	r.drain()
	if r.state(spam) != store.IntentApplied || len(r.srv.FindMsgID(gmSpam, 108)) != 1 || r.srv.Message(gmAll, m.UID) != nil {
		t.Fatalf("spam: state %s, Spam %v", r.state(spam), r.srv.FindMsgID(gmSpam, 108))
	}
	inSpam := r.srv.Message(gmSpam, r.srv.FindMsgID(gmSpam, 108)[0])
	ham := r.intent(store.IntentMove, intentPayload{Mailbox: "INBOX", TargetRole: "inbox"}, copyOf(gmSpam, inSpam))
	r.drain()
	if r.state(ham) != store.IntentApplied || len(r.srv.FindMsgID(gmAll, 108)) != 1 || len(r.srv.FindMsgID(gmSpam, 108)) != 0 {
		t.Fatalf("not spam: state %s, All Mail %v", r.state(ham), r.srv.FindMsgID(gmAll, 108))
	}
	r.noExpunge()
}

func TestGmailFlagsAreFlags(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 109, `\Inbox`)
	cp := copyOf(gmAll, m)
	seen := r.intent(store.IntentAddFlag, intentPayload{Flag: `\Seen`}, cp)
	star := r.intent(store.IntentAddFlag, intentPayload{Flag: `\Flagged`}, cp)
	r.drain()
	if r.state(seen) != store.IntentApplied || r.state(star) != store.IntentApplied {
		t.Fatalf("states %s %s", r.state(seen), r.state(star))
	}
	if got := r.srv.Message(gmAll, m.UID).Flags; !reflect.DeepEqual(got, []string{`\Seen`, `\Flagged`}) {
		t.Errorf("flags = %q (\\Flagged is Gmail's star)", got)
	}
	r.mustOnly(`UID STORE 1 +FLAGS.SILENT (\Seen)`, `UID STORE 1 +FLAGS.SILENT (\Flagged)`)
	if len(r.st.recorded) != 2 {
		t.Errorf("stored-flag records = %+v", r.st.recorded)
	}
}

// TestGmailRefusesTheDeletedFlag: \Deleted is destructive on Gmail
// (auto-expunge), so Gmail mode dead-letters it before the wire.
func TestGmailRefusesTheDeletedFlag(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 110, `\Inbox`)
	in := r.intent(store.IntentAddFlag, intentPayload{Flag: `\Deleted`}, copyOf(gmAll, m))
	r.drain()
	if r.state(in) != store.IntentDead {
		t.Fatalf("state = %s, want dead", r.state(in))
	}
	r.mustOnly()
}

func TestGmailReresolvesAMovedUIDByMsgID(t *testing.T) {
	r := newGmailRig(t)
	r.put(gmAll, 1, `\Inbox`) // uid 1: another message
	m := r.put(gmAll, 111)
	stale := copyOf(gmAll, m) // Mailroom knows it at uid 2
	// In Gmail's own app: trashed, then restored — back in All Mail at a
	// new UID, which Mailroom's next sync has not seen yet.
	trashUID := r.srv.MoveMessage(gmAll, m.UID, gmTrash)
	newUID := r.srv.MoveMessage(gmTrash, trashUID, "INBOX")
	if newUID == m.UID {
		t.Fatal("fixture: the UID did not change")
	}
	in := r.intent(store.IntentAddKeyword, intentPayload{Keyword: "work", Label: "Work"}, stale)
	res := r.drain()
	if r.state(in) != store.IntentApplied || res.Reresolved != 1 {
		t.Fatalf("state %s, reresolved %d (%s)", r.state(in), res.Reresolved, r.st.intents[in.ID].LastError)
	}
	if got := r.labels(gmAll, newUID); !reflect.DeepEqual(got, []string{`\Inbox`, "Work"}) {
		t.Errorf("labels at the new UID = %q", got)
	}
	if got := r.labels(gmAll, 1); !reflect.DeepEqual(got, []string{`\Inbox`}) {
		t.Errorf("the message at uid 1 was touched: %q", got)
	}
}

// TestGmailNeverWritesToAStoredUIDItCannotFind: X-GM-MSGID matches nothing
// in the mailbox, so nothing is written — not to the stored UID either.
func TestGmailNeverWritesToAStoredUIDItCannotFind(t *testing.T) {
	r := newGmailRig(t)
	other := r.put(gmAll, 1, `\Inbox`)
	ghost := copyOf(gmAll, other)
	ghost.GmMsgID = 999 // Mailroom's copy is a message Gmail no longer holds here
	in := r.intent(store.IntentMove, intentPayload{Mailbox: gmAll, TargetRole: "archive"}, ghost)
	r.drain()
	if r.state(in) != store.IntentPending {
		t.Fatalf("state = %s, want pending (retry after the next sync)", r.state(in))
	}
	r.mustOnly()
	if got := r.labels(gmAll, other.UID); !reflect.DeepEqual(got, []string{`\Inbox`}) {
		t.Errorf("the message at the stored UID was archived: %q", got)
	}
}

// TestGmailRefusesAPermanentDeleteOutsideTrash (#911 review F4): a
// message-addressed expunge of a copy in All Mail is dead-lettered, never
// turned into MOVE-to-Trash-then-EXPUNGE.
func TestGmailRefusesAPermanentDeleteOutsideTrash(t *testing.T) {
	for _, noMove := range []bool{false, true} {
		r := newGmailRig(t)
		r.srv.NoMove = noMove
		m := r.put(gmAll, 112, "Work")
		in := r.intent(store.IntentExpunge, intentPayload{}, copyOf(gmAll, m))
		r.drain()
		if r.state(in) != store.IntentDead {
			t.Fatalf("noMove=%v: state %s, want dead", noMove, r.state(in))
		}
		r.mustOnly()
		r.noExpunge()
	}
}

func TestGmailHardDeleteInTrashExpungesThere(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmTrash, 113)
	in := r.intent(store.IntentExpunge, intentPayload{}, copyOf(gmTrash, m))
	res := r.drain()
	if r.state(in) != store.IntentApplied || res.Expunged != 1 {
		t.Fatalf("state %s, expunged %d", r.state(in), res.Expunged)
	}
	if got := r.srv.Expunged(); !reflect.DeepEqual(got, []string{gmTrash + " 1"}) {
		t.Errorf("expunged = %q", got)
	}
}

// TestGmailDraftRetirementNeverTouchesTheSentCopy: Gmail files the sent
// message in All Mail with the draft's Message-ID. The generic header
// expunge would SEARCH All Mail by Message-ID and expunge both.
func TestGmailDraftRetirementNeverTouchesTheSentCopy(t *testing.T) {
	r := newGmailRig(t)
	const mid = "<draft-1@example.com>"
	sent := r.srv.Append(gmAll, &gmailtest.Message{MsgID: 200, Labels: []string{`\Sent`}, MessageID: mid, Body: []byte("s")})
	r.srv.Append(gmAll, &gmailtest.Message{MsgID: 201, Labels: []string{`\Draft`}, MessageID: mid, Body: []byte("d")})
	r.srv.Append(gmDrafts, &gmailtest.Message{MsgID: 201, Labels: []string{`\Draft`}, MessageID: mid, Body: []byte("d")})
	// The store queues one per mailbox it knows a copy in: All Mail
	// (provenance) and Drafts (the APPEND target).
	a := r.headerIntent(gmAll, mid)
	d := r.headerIntent(gmDrafts, mid)

	res := r.drain()

	if r.state(a) != store.IntentApplied || r.state(d) != store.IntentApplied {
		t.Fatalf("states %s / %s (%s | %s)", r.state(a), r.state(d),
			r.st.intents[a.ID].LastError, r.st.intents[d.ID].LastError)
	}
	if r.srv.Message(gmAll, sent.UID) == nil {
		t.Fatal("THE SENT COPY WAS DESTROYED")
	}
	if len(r.srv.FindMsgID(gmAll, 201)) != 0 || len(r.srv.FindMsgID(gmDrafts, 201)) != 0 || len(r.srv.FindMsgID(gmTrash, 201)) != 0 {
		t.Errorf("draft left behind: all %v drafts %v trash %v",
			r.srv.FindMsgID(gmAll, 201), r.srv.FindMsgID(gmDrafts, 201), r.srv.FindMsgID(gmTrash, 201))
	}
	if got := r.srv.Expunged(); len(got) != 1 || !strings.HasPrefix(got[0], gmTrash) || res.Expunged != 1 {
		t.Errorf("expunged = %q (res %d), want one, in Trash", got, res.Expunged)
	}
	for _, c := range r.srv.Commands() {
		if strings.HasPrefix(c, "SELECT") && strings.Contains(c, "All Mail") {
			t.Errorf("the draft retirement selected All Mail: %q", c)
		}
	}
}

// TestGmailNoMoveRefusesTheExpungeFallback: without MOVE, go-imap would
// COPY + \Deleted + EXPUNGE in the source. Gmail mode refuses instead.
func TestGmailNoMoveRefusesTheExpungeFallback(t *testing.T) {
	r := newGmailRig(t)
	r.srv.NoMove = true
	m := r.put(gmAll, 114, `\Inbox`)
	in := r.intent(store.IntentMove, intentPayload{Mailbox: gmTrash, TargetRole: "trash"}, copyOf(gmAll, m))
	r.drain()
	if r.state(in) != store.IntentPending {
		t.Fatalf("state = %s, want pending", r.state(in))
	}
	r.mustOnly()
}

// TestGmailWithoutTheExtensionWritesNothing: Gmail mode on a session that
// cannot write labels never falls back to the generic folder ops.
func TestGmailWithoutTheExtensionWritesNothing(t *testing.T) {
	r := newGmailRig(t)
	r.srv.GmailExt = false
	m := r.put(gmAll, 115, `\Inbox`)
	moves := r.intent(store.IntentMove, intentPayload{Mailbox: gmAll, TargetRole: "archive"}, copyOf(gmAll, m))
	trash := r.intent(store.IntentMove, intentPayload{Mailbox: gmTrash, TargetRole: "trash"}, copyOf(gmAll, m))
	r.drain()
	if r.state(moves) != store.IntentPending || r.state(trash) != store.IntentPending {
		t.Fatalf("states %s %s", r.state(moves), r.state(trash))
	}
	r.mustOnly()
}

// TestGmailWritesOnlyTheOwningAccount: labels are user-wide, writes are not.
// A drain of account A claims only A's intents and writes only to A's
// server; B's server receives nothing and B's intent stays queued.
func TestGmailWritesOnlyTheOwningAccount(t *testing.T) {
	r := newGmailRig(t)
	other := gmailtest.New(t)
	other.StandardMailboxes()
	ma := r.put(gmAll, 300, `\Inbox`)
	mb := other.Append(gmAll, &gmailtest.Message{MsgID: 300, Labels: []string{`\Inbox`}, Body: []byte("x")})
	inA := r.intent(store.IntentAddKeyword, intentPayload{Keyword: "w", Label: "Work"}, copyOf(gmAll, ma))
	// The same label on the same message for account B, queued in the same
	// store.
	acctB := store.NewULID()
	msg := *inA.MessageID
	bodyB, _ := json.Marshal(intentPayload{Keyword: "w", Label: "Work"})
	inB := store.Intent{ID: store.NewULID(), MessageID: &msg, AccountID: acctB, Op: store.IntentAddKeyword,
		Payload: bodyB, IdempotencyKey: "b", State: store.IntentPending}
	r.st.add(inB)
	r.st.prov[provKey(msg, acctB)] = []store.ProvenanceTarget{copyOf(gmAll, mb)}

	r.drain()

	if r.state(inA) != store.IntentApplied || r.st.intents[inB.ID].State != store.IntentPending {
		t.Fatalf("A %s, B %s", r.state(inA), r.st.intents[inB.ID].State)
	}
	if c := other.Commands(); len(c) != 0 {
		t.Errorf("account B's server received %q", c)
	}
	if got := other.Message(gmAll, mb.UID).Labels; !reflect.DeepEqual(got, []string{`\Inbox`}) {
		t.Errorf("B's copy changed: %q", got)
	}
}

// TestGmailRefusesMovesGmailCannotRepresent: Sent, Drafts and Starred are
// not places to move mail on Gmail; the intent is dead-lettered with a
// reason and nothing reaches the server.
func TestGmailRefusesMovesGmailCannotRepresent(t *testing.T) {
	for _, dest := range []string{"[Gmail]/Sent Mail", gmDrafts, "[Gmail]/Starred", "[Gmail]", ""} {
		r := newGmailRig(t)
		m := r.put(gmAll, 116, `\Inbox`)
		in := r.intent(store.IntentMove, intentPayload{Mailbox: dest, TargetRole: "x"}, copyOf(gmAll, m))
		r.drain()
		if r.state(in) != store.IntentDead {
			t.Errorf("move to %q: state %s, want dead", dest, r.state(in))
		}
		r.mustOnly()
	}
}

func TestGmailMoveToALabelView(t *testing.T) {
	r := newGmailRig(t)
	m := r.put(gmAll, 117, `\Inbox`)
	in := r.intent(store.IntentMove, intentPayload{Mailbox: "Receipts"}, copyOf(gmAll, m))
	r.drain()
	if r.state(in) != store.IntentApplied {
		t.Fatalf("state = %s (%s)", r.state(in), r.st.intents[in.ID].LastError)
	}
	if got := r.labels(gmAll, m.UID); !reflect.DeepEqual(got, []string{"Receipts"}) {
		t.Errorf("labels = %q, want [Receipts] (Gmail's Move to)", got)
	}
	r.noExpunge()
	// Only the \Inbox removal is on the baseline: the next reading reports
	// the label, which gives the message the matching Mailroom label.
	if len(r.st.gmailRecorded) != 1 || r.st.gmailRecorded[0].add != nil {
		t.Errorf("baseline deltas = %+v", r.st.gmailRecorded)
	}
}
