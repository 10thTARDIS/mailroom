// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestMailboxSpecialUse(t *testing.T) {
	tests := []struct {
		name    string
		attrs   []imap.MailboxAttr
		wantUse MailboxAttr
		wantOK  bool
	}{
		{"sent", []imap.MailboxAttr{imap.MailboxAttrHasNoChildren, imap.MailboxAttrSent}, MailboxAttrSent, true},
		{"drafts", []imap.MailboxAttr{imap.MailboxAttrDrafts}, MailboxAttrDrafts, true},
		{"trash", []imap.MailboxAttr{imap.MailboxAttrTrash}, MailboxAttrTrash, true},
		{"junk", []imap.MailboxAttr{imap.MailboxAttrJunk}, MailboxAttrJunk, true},
		{"archive", []imap.MailboxAttr{imap.MailboxAttrArchive}, MailboxAttrArchive, true},
		{"all", []imap.MailboxAttr{imap.MailboxAttrAll}, MailboxAttrAll, true},
		{"important", []imap.MailboxAttr{imap.MailboxAttrImportant}, MailboxAttrImportant, true},
		{"no special use", []imap.MailboxAttr{imap.MailboxAttrHasChildren, imap.MailboxAttrUnmarked}, "", false},
		{"noselect only", []imap.MailboxAttr{imap.MailboxAttrNoSelect}, "", false},
		{"empty", nil, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mbox := mapListData(&imap.ListData{Mailbox: "Test", Attrs: tt.attrs})
			use, ok := mbox.SpecialUse()
			if ok != tt.wantOK {
				t.Fatalf("SpecialUse() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && use != tt.wantUse {
				t.Errorf("SpecialUse() = %q, want %q", use, tt.wantUse)
			}
		})
	}
}

func TestMailboxHasAttr(t *testing.T) {
	mbox := mapListData(&imap.ListData{
		Mailbox: "INBOX",
		Attrs:   []imap.MailboxAttr{imap.MailboxAttrHasChildren, imap.MailboxAttrUnmarked},
	})
	if !mbox.HasAttr(MailboxAttrHasChildren) {
		t.Error("HasAttr(HasChildren) = false, want true")
	}
	if mbox.HasAttr(MailboxAttrSent) {
		t.Error("HasAttr(Sent) = true, want false")
	}
}

func TestMapListData(t *testing.T) {
	d := &imap.ListData{
		Mailbox: "INBOX/Sent",
		Delim:   '/',
		Attrs:   []imap.MailboxAttr{imap.MailboxAttrSent},
	}
	mbox := mapListData(d)
	if mbox.Name != "INBOX/Sent" {
		t.Errorf("Name = %q, want %q", mbox.Name, "INBOX/Sent")
	}
	if mbox.Delimiter != '/' {
		t.Errorf("Delimiter = %q, want '/'", mbox.Delimiter)
	}
	if len(mbox.Attrs) != 1 || mbox.Attrs[0] != MailboxAttrSent {
		t.Errorf("Attrs = %v, want [%q]", mbox.Attrs, MailboxAttrSent)
	}
}

func TestMapFlags(t *testing.T) {
	if got := mapFlags(nil); got != nil {
		t.Errorf("mapFlags(nil) = %v, want nil", got)
	}

	in := []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.Flag("$Custom")}
	want := []Flag{FlagSeen, FlagFlagged, Flag("$Custom")}
	got := mapFlags(in)
	if len(got) != len(want) {
		t.Fatalf("mapFlags() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mapFlags()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMapAddressAndAddr(t *testing.T) {
	a := mapAddress(imap.Address{Name: "Alice", Mailbox: "alice", Host: "example.com"})
	if a.Addr() != "alice@example.com" {
		t.Errorf("Addr() = %q, want alice@example.com", a.Addr())
	}

	groupStart := mapAddress(imap.Address{Mailbox: "a-group"})
	if got := groupStart.Addr(); got != "" {
		t.Errorf("Addr() for group start = %q, want empty", got)
	}
}

func TestMapEnvelopeNil(t *testing.T) {
	if got := mapEnvelope(nil); got != nil {
		t.Errorf("mapEnvelope(nil) = %v, want nil", got)
	}
}

func TestMapEnvelope(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	src := &imap.Envelope{
		Date:      now,
		Subject:   "Hello",
		From:      []imap.Address{{Name: "Bob", Mailbox: "bob", Host: "example.com"}},
		InReplyTo: []string{"msg-1@example.com"},
		MessageID: "msg-2@example.com",
	}
	env := mapEnvelope(src)
	if env == nil {
		t.Fatal("mapEnvelope() = nil")
	}
	if env.Subject != "Hello" || !env.Date.Equal(now) || env.MessageID != "msg-2@example.com" {
		t.Errorf("mapEnvelope() = %+v, unexpected fields", env)
	}
	if len(env.From) != 1 || env.From[0].Addr() != "bob@example.com" {
		t.Errorf("mapEnvelope().From = %+v", env.From)
	}
}

func TestMapSelectDataPermanentFlagsAllowNew(t *testing.T) {
	withWildcard := mapSelectData("INBOX", &imap.SelectData{
		UIDValidity:    123,
		UIDNext:        456,
		HighestModSeq:  789,
		NumMessages:    10,
		PermanentFlags: []imap.Flag{imap.FlagSeen, imap.FlagWildcard},
	})
	if !withWildcard.PermanentFlagsAllowNew {
		t.Error("PermanentFlagsAllowNew = false, want true when \\* present")
	}
	if withWildcard.UIDValidity != 123 || withWildcard.UIDNext != 456 || withWildcard.HighestModSeq != 789 || withWildcard.NumMessages != 10 {
		t.Errorf("mapSelectData() = %+v, unexpected fields", withWildcard)
	}

	withoutWildcard := mapSelectData("INBOX", &imap.SelectData{
		PermanentFlags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged},
	})
	if withoutWildcard.PermanentFlagsAllowNew {
		t.Error("PermanentFlagsAllowNew = true, want false without \\*")
	}
}

func TestUIDSetConstructors(t *testing.T) {
	single := UIDSetNum(5)
	if len(single) != 1 || single[0] != (UIDRange{Start: 5, Stop: 5}) {
		t.Errorf("UIDSetNum(5) = %v", single)
	}

	multi := UIDSetNum(1, 2, 3)
	if len(multi) != 3 {
		t.Errorf("UIDSetNum(1,2,3) len = %d, want 3", len(multi))
	}

	openEnded := UIDSetRange(10, 0)
	if len(openEnded) != 1 || openEnded[0].Start != 10 || openEnded[0].Stop != 0 {
		t.Errorf("UIDSetRange(10,0) = %v", openEnded)
	}
	if openEnded.IsEmpty() {
		t.Error("IsEmpty() = true for non-empty set")
	}

	var empty UIDSet
	if !empty.IsEmpty() {
		t.Error("IsEmpty() = false for nil UIDSet")
	}
}

func TestUIDSetToIMAP(t *testing.T) {
	tests := []struct {
		name string
		set  UIDSet
		want string
	}{
		{"single", UIDSetNum(5), "5"},
		{"open ended", UIDSetRange(10, 0), "10:*"},
		{"closed range", UIDSetRange(1, 4), "1:4"},
		{"multi", UIDSetNum(1, 3, 5), "1,3,5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uidSetToIMAP(tt.set).String()
			if got != tt.want {
				t.Errorf("uidSetToIMAP(%v).String() = %q, want %q", tt.set, got, tt.want)
			}
		})
	}
}

func TestMapFetchBuffer(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	buf := &imapclient.FetchMessageBuffer{
		SeqNum:       1,
		UID:          imap.UID(42),
		Flags:        []imap.Flag{imap.FlagSeen},
		Envelope:     &imap.Envelope{Subject: "Test"},
		InternalDate: now,
		RFC822Size:   1024,
		ModSeq:       99,
		BodySection: []imapclient.FetchBodySectionBuffer{
			{Bytes: []byte("From: a@example.com\r\n\r\nhi")},
		},
	}
	msg := mapFetchBuffer(buf)
	if msg.UID != UID(42) {
		t.Errorf("UID = %d, want 42", msg.UID)
	}
	if msg.SeqNum != 1 {
		t.Errorf("SeqNum = %d, want 1", msg.SeqNum)
	}
	if len(msg.Flags) != 1 || msg.Flags[0] != FlagSeen {
		t.Errorf("Flags = %v", msg.Flags)
	}
	if msg.Envelope == nil || msg.Envelope.Subject != "Test" {
		t.Errorf("Envelope = %+v", msg.Envelope)
	}
	if !msg.InternalDate.Equal(now) {
		t.Errorf("InternalDate = %v, want %v", msg.InternalDate, now)
	}
	if msg.RFC822Size != 1024 {
		t.Errorf("RFC822Size = %d, want 1024", msg.RFC822Size)
	}
	if msg.ModSeq != 99 {
		t.Errorf("ModSeq = %d, want 99", msg.ModSeq)
	}
	if string(msg.Body) != "From: a@example.com\r\n\r\nhi" {
		t.Errorf("Body = %q", msg.Body)
	}
}

func TestMapFetchBufferNoBody(t *testing.T) {
	buf := &imapclient.FetchMessageBuffer{SeqNum: 1, UID: imap.UID(1)}
	msg := mapFetchBuffer(buf)
	if msg.Body != nil {
		t.Errorf("Body = %v, want nil when BodySection unset", msg.Body)
	}
}

// LIST-STATUS (#620): a status is only handed to the engine when every number
// it compares was actually reported. go-imap leaves an omitted item at zero (or
// nil for MESSAGES), and a zero the engine mistook for a value could match a
// stored zero and skip a SELECT on an answer the server never gave.
func TestMapListStatus(t *testing.T) {
	n := func(v uint32) *uint32 { return &v }
	full := imap.StatusData{UIDValidity: 7, UIDNext: 42, NumMessages: n(40), HighestModSeq: 900}

	got := mapListData(&imap.ListData{Mailbox: "INBOX", Status: &full}).Status
	if got == nil {
		t.Fatal("a complete LIST-STATUS mapped to nil")
	}
	if want := (ListStatus{UIDValidity: 7, UIDNext: 42, NumMessages: 40, HighestModSeq: 900}); *got != want {
		t.Errorf("Status = %+v, want %+v", *got, want)
	}

	// An empty mailbox is a real zero for MESSAGES, not a missing one.
	empty := imap.StatusData{UIDValidity: 7, UIDNext: 1, NumMessages: n(0)}
	if got := mapListStatus(&empty); got == nil || got.NumMessages != 0 || got.HighestModSeq != 0 {
		t.Errorf("empty mailbox status = %+v, want present with MESSAGES 0 and modseq unknown", got)
	}

	for name, s := range map[string]*imap.StatusData{
		"no status at all":   nil,
		"no UIDVALIDITY":     {UIDNext: 42, NumMessages: n(40), HighestModSeq: 900},
		"no UIDNEXT":         {UIDValidity: 7, NumMessages: n(40), HighestModSeq: 900},
		"no MESSAGES":        {UIDValidity: 7, UIDNext: 42, HighestModSeq: 900},
		"nothing but modseq": {HighestModSeq: 900},
	} {
		if got := mapListData(&imap.ListData{Mailbox: "INBOX", Status: s}).Status; got != nil {
			t.Errorf("%s: Status = %+v, want nil — a partial answer must read as unknown", name, *got)
		}
	}
}
