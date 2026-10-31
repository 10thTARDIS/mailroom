// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"testing"

	"mailroom/internal/sync"
)

// THE MICROSOFT FOLDER MAP (ADR-046 §12). Exchange lists Sent Items, Deleted
// Items and friends; where it sends no SPECIAL-USE, the profile's stated map
// gives them their role. SPECIAL-USE, when sent, wins. A generic account is
// untouched.
//
// PROBE: drop the provider.FolderSpecialUse fallback in mailboxSpecialUse and
// this goes red.
func TestMicrosoftProfileFolderMap(t *testing.T) {
	mbs := []sync.Mailbox{
		inbox(),
		{Name: "Sent Items", Delimiter: '/'},
		{Name: "Deleted Items", Delimiter: '/'},
		{Name: "Junk Email", Delimiter: '/'},
		{Name: "Archive", Delimiter: '/', Attrs: []sync.MailboxAttr{`\Trash`}}, // SPECIAL-USE wins
		{Name: "Projects/Sent Items", Delimiter: '/'},                          // nested: just a folder
	}
	run := func(profile string) *recordingStore {
		boxes := map[string]*fakeBox{}
		for _, mb := range mbs {
			boxes[mb.Name] = &fakeBox{status: sync.MailboxStatus{UIDValidity: 1, UIDNext: 1}}
		}
		sess := &fakeSession{mailboxes: mbs, boxes: boxes}
		rs := &recordingStore{fakeStore: newFakeStore()}
		ing := newFakeIngester()
		ing.st = rs.fakeStore
		req := testReq()
		req.ProviderProfile = profile
		if _, err := New(rs, ing, factoryFor(sess)).SyncAccount(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return rs
	}
	rs := run("microsoft")
	want := map[string]string{
		"INBOX": "", "Sent Items": `\Sent`, "Deleted Items": `\Trash`, "Junk Email": `\Junk`,
		"Archive": `\Trash`, "Projects/Sent Items": "",
	}
	for name, su := range want {
		if got := rs.records[name].SpecialUse; got != su {
			t.Errorf("microsoft %q special-use = %q, want %q", name, got, su)
		}
	}
	if got := run("generic").records["Sent Items"].SpecialUse; got != "" {
		t.Errorf("generic account got a profile role for Sent Items: %q", got)
	}
}

// NON-MAIL FOLDERS NEED EVIDENCE, NOT JUST A NAME (review M3 of #910). On its
// first sighting, a name-matched folder is set not synced only when it is
// \Noselect or its first SELECT shows it EMPTY. A name-matched folder that
// holds mail — a localised mailbox's own folder, or a user's mail folder
// called "Notes" — keeps syncing. A generic account is never touched.
//
// PROBE: drop the `status.NumMessages == 0` condition in syncMailbox and the
// "Notes" (with mail) case goes red.
func TestNonMailFolderNeedsEvidence(t *testing.T) {
	mbs := []sync.Mailbox{
		{Name: "Calendar", Delimiter: '/'},                                         // empty: off
		{Name: "Contacts", Delimiter: '/', Attrs: []sync.MailboxAttr{`\Noselect`}}, // noselect: off
		{Name: "Notes", Delimiter: '/'},                                            // holds mail: stays on
	}
	run := func(profile string) (*fakeStore, *fakeSession) {
		sess := &fakeSession{mailboxes: mbs, boxes: map[string]*fakeBox{
			"Calendar": {status: sync.MailboxStatus{UIDValidity: 1, UIDNext: 1}},
			"Contacts": {status: sync.MailboxStatus{UIDValidity: 1, UIDNext: 1}},
			"Notes": {status: sync.MailboxStatus{UIDValidity: 1, UIDNext: 2, NumMessages: 1},
				msgs: []sync.Message{msg(1, 0, "a real note-to-self")}},
		}}
		st, ing := newFakeStore(), newFakeIngester()
		ing.st = st
		req := testReq()
		req.ProviderProfile = profile
		if _, err := New(st, ing, factoryFor(sess)).SyncAccount(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return st, sess
	}
	st, sess := run("microsoft")
	if st.rows["Calendar"].syncScope != "off" || st.rows["Contacts"].syncScope != "off" {
		t.Errorf("empty / \\Noselect non-mail folders not set off: Calendar=%s Contacts=%s",
			st.rows["Calendar"].syncScope, st.rows["Contacts"].syncScope)
	}
	if st.rows["Notes"].syncScope != "full" {
		t.Fatalf("a folder named Notes that HOLDS MAIL was switched off on its name alone")
	}
	fetchedNotes := false
	for _, f := range sess.fetches {
		if f.mailbox == "Notes" && f.items.BodyFull {
			fetchedNotes = true
		}
		if f.mailbox == "Calendar" {
			t.Errorf("the empty Calendar was fetched: %+v", f)
		}
	}
	if !fetchedNotes {
		t.Error("the Notes folder's mail was not fetched")
	}
	st, _ = run("generic")
	if len(st.scopeOffCalls) != 0 {
		t.Errorf("generic account had folders scoped off: %d", len(st.scopeOffCalls))
	}
}

// REVIEW M-H2 of #910: a non-mail folder that was set off on first sight,
// and that the user then switched back ON — still with no cursor, because it
// was never polled — must stay on and be polled on the next pass. The old
// rule (no cursor = first sight) switched it off again every pass.
//
// PROBE: decide on `stored.UIDValidity == 0 && stored.LastSeenUID == 0`
// instead of stored.Created and this goes red.
func TestReenabledNonMailFolderStaysOn(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{{Name: "Calendar", Delimiter: '/'}},
		boxes:     map[string]*fakeBox{"Calendar": {status: sync.MailboxStatus{UIDValidity: 3, UIDNext: 1}}},
	}
	st, ing := newFakeStore(), newFakeIngester()
	// Scoped off earlier, switched on by the user: no cursor at all.
	st.seedRow("Calendar", &storedRow{id: newULID(), syncScope: "full"})
	ing.st = st
	req := testReq()
	req.ProviderProfile = "microsoft"
	if _, err := New(st, ing, factoryFor(sess)).SyncAccount(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(st.scopeOffCalls) != 0 || st.rows["Calendar"].syncScope != "full" {
		t.Fatalf("a non-mail folder the user switched back on was switched off again")
	}
	if len(sess.selects) == 0 || sess.selects[0] != "Calendar" {
		t.Errorf("the re-enabled folder was not polled: selects %v", sess.selects)
	}
}
