// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// Mute at ingest (spec §6, #51): a NEW reply arriving in the inbox of a muted
// conversation is placed in the account's Archive — in Mailroom only, unread,
// with no writeback — and reported as Result.Muted so the sync engine leaves it
// out of the new-mail push. Before this, app.threads.muted_at was read by
// Thread/get and nothing else, so muting a conversation changed a banner and
// nothing about where its replies went.
//
// Every "stays put" test below is the CONTROL for the headline one: the
// divert must happen for exactly a muted conversation's inbox arrival, and not
// for an unmuted one, a Sent copy, an account with nowhere to divert to, or a
// bulk archive import.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// setMuted mutes or unmutes a thread through the store's own Thread/set path.
func (f *fixture) setMuted(t *testing.T, threadID ulid.ULID, muted bool) {
	t.Helper()
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		changed, err := tx.SetThreadMuted(context.Background(), threadID, muted)
		if err != nil {
			return err
		}
		if !changed {
			t.Fatalf("SetThreadMuted(%s, %v) changed nothing — the fixture is not in the state the test assumes", threadID, muted)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// placement is the message's canonical placement override, "" when none.
func (f *fixture) placement(t *testing.T, msgID ulid.ULID) string {
	t.Helper()
	var moved string
	if err := f.super.QueryRow(context.Background(),
		`SELECT COALESCE(moved_mailbox_id::text, '') FROM app.messages WHERE id = $1`,
		msgID.String()).Scan(&moved); err != nil {
		t.Fatalf("placement: %v", err)
	}
	return moved
}

// inView reports whether msgID is in the Email/query result for one mailbox,
// with the web client's own desk filter (core/views.ts: `inMailbox` plus
// `isSnoozed: false`) — the store query the desk actually runs, not a
// re-derivation of membership.
func (f *fixture) inView(t *testing.T, mailbox, msgID ulid.ULID) bool {
	t.Helper()
	notSnoozed := false
	var found bool
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		ids, _, err := tx.QueryEmails(context.Background(), store.EmailQuery{InMailbox: &mailbox, Snoozed: &notSnoozed})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id == msgID {
				found = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// startConversation ingests a root message into INBOX and returns it. Replies
// to it (rawMessage with refs = [rootMsgID]) join its thread.
func (f *fixture) startConversation(t *testing.T, rootMsgID string, uid int64) Result {
	t.Helper()
	root, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		rawMessage("Topic "+rootMsgID, rootMsgID, nil), f.prov(f.mailboxA, uid, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !root.Created {
		t.Fatal("root: expected a new canonical message")
	}
	return root
}

// reply ingests a reply to rootMsgID into mailbox, unread, and checks it
// joined the root's conversation — the precondition every assertion about a
// MUTED CONVERSATION rests on.
func (f *fixture) reply(t *testing.T, root Result, rootMsgID, msgID string, mailbox ulid.ULID, uid int64) Result {
	t.Helper()
	res, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		rawMessage("Re: Topic "+rootMsgID, msgID, []string{rootMsgID}), f.prov(mailbox, uid, nil))
	if err != nil {
		t.Fatalf("ingest reply: %v", err)
	}
	if !res.Created {
		t.Fatal("reply: expected a new canonical message")
	}
	if res.ThreadID != root.ThreadID {
		t.Fatalf("reply thread %s != root thread %s — the reply did not join the conversation, so nothing below tests mute",
			res.ThreadID, root.ThreadID)
	}
	return res
}

// ⚠ THE HEADLINE. A reply to a muted conversation, arriving unread in INBOX:
// placed in Archive, still unread, no writeback, out of the desk, visible in
// Archive, still in INBOX ON THE SERVER, and reported as Muted.
func TestMutedConversationReplyIsArchivedOnArrival(t *testing.T) {
	f := newFixture(t)
	f.markMailboxRoles(t)
	root := f.startConversation(t, "mute-root-1@example.com", 1)
	f.setMuted(t, root.ThreadID, true)

	reply := f.reply(t, root, "mute-root-1@example.com", "mute-reply-1@example.com", f.mailboxA, 2)

	if got := f.placement(t, reply.MessageID); got != f.mailboxB.String() {
		t.Fatalf("moved_mailbox_id = %q, want the account's Archive %q — the muted reply was not diverted", got, f.mailboxB.String())
	}
	if !reply.Muted {
		t.Error("Result.Muted = false for a diverted muted reply — the sync engine would push for it")
	}
	if got := f.scalar(t, `SELECT is_seen::int FROM app.messages WHERE id=$1`, reply.MessageID.String()); got != 0 {
		t.Error("the muted reply was marked read — mute must leave read state alone")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1`, reply.MessageID.String()); got != 0 {
		t.Errorf("writeback intents for the muted reply = %d, want 0 — mute is Mailroom-only", got)
	}
	// Mailroom-only, from the other side: the server copy is still where the
	// server put it. Nothing here asked the server to move anything.
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE message_id=$1 AND mailbox_id=$2 AND state='live'`,
		reply.MessageID.String(), f.mailboxA.String()); got != 1 {
		t.Errorf("live INBOX provenance for the muted reply = %d, want 1 (the server copy is untouched)", got)
	}
	if f.inView(t, f.mailboxA, reply.MessageID) {
		t.Error("the muted reply is in the INBOX desk query")
	}
	if !f.inView(t, f.mailboxB, reply.MessageID) {
		t.Error("the muted reply is not findable in Archive — mute must never hide mail")
	}
	// The change_log invariant: the reply's own OpCreated row carries its final
	// placement; the divert appends nothing of its own (no `updated` row with a
	// higher seq than the `created` one).
	if got := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1 AND op='created'`,
		reply.MessageID.String()); got != 1 {
		t.Errorf("Email created rows for the reply = %d, want 1", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1 AND op='updated'`,
		reply.MessageID.String()); got != 0 {
		t.Errorf("Email updated rows for the reply = %d, want 0 — the divert must not append its own", got)
	}
}

// CONTROL: the same reply to a conversation that is NOT muted surfaces.
func TestUnmutedConversationReplyStaysInTheInbox(t *testing.T) {
	f := newFixture(t)
	f.markMailboxRoles(t)
	root := f.startConversation(t, "mute-root-2@example.com", 1)

	reply := f.reply(t, root, "mute-root-2@example.com", "mute-reply-2@example.com", f.mailboxA, 2)

	if got := f.placement(t, reply.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none — an unmuted conversation's reply was diverted", got)
	}
	if reply.Muted {
		t.Error("Result.Muted = true for a reply to an unmuted conversation — it would not notify")
	}
	if !f.inView(t, f.mailboxA, reply.MessageID) {
		t.Error("the reply is not in the INBOX desk query")
	}
}

// CONTROL: a muted conversation's message that arrives in Sent (the user's own
// reply, synced back) is left in Sent. Mute keeps replies out of the INBOX; it
// has no business moving mail that was never going to be there.
func TestMutedConversationSentCopyIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.markMailboxRoles(t)
	sent := ulid.Make()
	if _, err := f.super.Exec(context.Background(),
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		 VALUES ($1, $2, $3, 'Sent', 'Sent', '\Sent')`,
		sent.String(), f.userID.String(), f.accountID.String()); err != nil {
		t.Fatalf("seed Sent: %v", err)
	}
	root := f.startConversation(t, "mute-root-3@example.com", 1)
	f.setMuted(t, root.ThreadID, true)

	mine := f.reply(t, root, "mute-root-3@example.com", "mute-reply-3@example.com", sent, 7)

	if got := f.placement(t, mine.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none — a Sent copy was moved by mute", got)
	}
	if mine.Muted {
		t.Error("Result.Muted = true for a message that did not arrive in the inbox")
	}
	if !f.inView(t, sent, mine.MessageID) {
		t.Error("the Sent copy is no longer in Sent")
	}
}

// CONTROL: an account with no Archive mailbox has nowhere to divert to, and the
// reply stays in the inbox rather than being hidden somewhere it cannot be
// found. It is not Muted either, so it notifies — it did surface.
func TestMutedConversationWithoutAnArchiveStaysInTheInbox(t *testing.T) {
	f := newFixture(t)
	// No markMailboxRoles: INBOX is still the inbox (by its reserved name), but
	// the fixture's "Archive" folder carries no \Archive SPECIAL-USE, so the
	// account has no archive-role mailbox.
	root := f.startConversation(t, "mute-root-4@example.com", 1)
	f.setMuted(t, root.ThreadID, true)

	reply := f.reply(t, root, "mute-root-4@example.com", "mute-reply-4@example.com", f.mailboxA, 2)

	if got := f.placement(t, reply.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none — there is no Archive to divert to", got)
	}
	if reply.Muted {
		t.Error("Result.Muted = true for a reply that stayed in the inbox — it surfaced, so it must notify")
	}
	if !f.inView(t, f.mailboxA, reply.MessageID) {
		t.Error("the reply is not in the INBOX desk query")
	}
}

// Unmuting is the way back: the NEXT reply surfaces normally. What mute already
// archived stays in Archive — unmute changes what happens from now on, it is
// not an undo of placements already made.
func TestUnmuteLetsTheNextReplySurface(t *testing.T) {
	f := newFixture(t)
	f.markMailboxRoles(t)
	root := f.startConversation(t, "mute-root-5@example.com", 1)
	f.setMuted(t, root.ThreadID, true)

	first := f.reply(t, root, "mute-root-5@example.com", "mute-reply-5a@example.com", f.mailboxA, 2)
	if got := f.placement(t, first.MessageID); got != f.mailboxB.String() {
		t.Fatalf("while muted: moved_mailbox_id = %q, want Archive — the precondition for testing unmute does not hold", got)
	}

	f.setMuted(t, root.ThreadID, false)
	second := f.reply(t, root, "mute-root-5@example.com", "mute-reply-5b@example.com", f.mailboxA, 3)

	if got := f.placement(t, second.MessageID); got != "" {
		t.Errorf("after unmute: moved_mailbox_id = %q, want none — the conversation is still being diverted", got)
	}
	if second.Muted {
		t.Error("after unmute: Result.Muted = true — the reply would not notify")
	}
	if !f.inView(t, f.mailboxA, second.MessageID) {
		t.Error("after unmute: the reply is not in the INBOX desk query")
	}
	if got := f.placement(t, first.MessageID); got != f.mailboxB.String() {
		t.Errorf("unmute moved the earlier reply: moved_mailbox_id = %q, want it left in Archive", got)
	}
}

// A skipInbox RULE and a mute on the same reply: the rule places it first (it
// runs first), the mute's placement is then a no-op, and the reply is STILL
// reported as a muted arrival — "a muted conversation does not notify you"
// must not depend on which of the two happened to run first. The half without
// a mute is the control that rule-archived mail keeps notifying exactly as it
// did before: Muted is about the conversation, not about the placement.
func TestSkipInboxRuleAndMuteOnTheSameReply(t *testing.T) {
	for _, muted := range []bool{true, false} {
		name := "unmuted"
		if muted {
			name = "muted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.markMailboxRoles(t)
			ctx := context.Background()
			root := f.startConversation(t, "mute-rule-root@example.com", 1)
			if muted {
				f.setMuted(t, root.ThreadID, true)
			}
			// Created AFTER the root so the rule acts only on the reply.
			if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
				_, err := tx.CreateRule(ctx, store.RuleRow{
					Name: "skip-from-alice", Enabled: true,
					Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
					Actions:    []rules.Action{{Type: rules.ActionSkipInbox}},
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			reply := f.reply(t, root, "mute-rule-root@example.com", "mute-rule-reply@example.com", f.mailboxA, 2)

			if got := f.placement(t, reply.MessageID); got != f.mailboxB.String() {
				t.Errorf("moved_mailbox_id = %q, want Archive %q", got, f.mailboxB.String())
			}
			if reply.Muted != muted {
				t.Errorf("Result.Muted = %v, want %v", reply.Muted, muted)
			}
		})
	}
}

// A bulk ARCHIVE IMPORT is history being loaded, not a reply arriving: the same
// gate as rules (localMailbox == nil) keeps mute off it, even when the imported
// message belongs to a muted conversation and is being placed in the inbox.
//
// ⚠ WHAT THIS DOES AND DOES NOT GUARD, measured by probe: deleting that gate
// ALONE leaves this green, because the archive path carries no account and no
// arriving mailbox, so skipInboxForNewMessage finds no Archive to divert to —
// a second, structural guard. Deleting the gate AND giving the archive path its
// mailbox's account (what a future "import into an account" change would do)
// turns it red at the placement assertion. So it pins the OUTCOME, and becomes
// the gate's guard exactly when the structural one goes away.
func TestArchiveImportIntoAMutedConversationIsNotDiverted(t *testing.T) {
	f := newFixture(t)
	f.markMailboxRoles(t)
	root := f.startConversation(t, "mute-root-6@example.com", 1)
	f.setMuted(t, root.ThreadID, true)

	res, err := f.ing.IngestArchive(context.Background(), f.userID, f.dek, f.dekID,
		rawMessage("Re: Topic", "mute-import-6@example.com", []string{"mute-root-6@example.com"}),
		Archive{MailboxID: f.mailboxA})
	if err != nil {
		t.Fatalf("archive ingest: %v", err)
	}
	if !res.Created || res.ThreadID != root.ThreadID {
		t.Fatalf("import: created=%v thread=%s, want a new message in the muted conversation %s", res.Created, res.ThreadID, root.ThreadID)
	}
	if got := f.placement(t, res.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none — an archive import was diverted by mute", got)
	}
	if res.Muted {
		t.Error("Result.Muted = true on an archive import")
	}
}
