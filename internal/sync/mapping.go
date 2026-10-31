// SPDX-License-Identifier: AGPL-3.0-only

package sync

// mapping.go is the actual anti-corruption boundary: every conversion
// between go-imap's types and mailroom's own types lives here. All
// functions are unexported — go-imap types are expected in their
// signatures, since converting them away is the whole point.

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// mapListData converts one LIST response into our Mailbox type.
func mapListData(d *imap.ListData) Mailbox {
	return Mailbox{
		Name:      d.Mailbox,
		Delimiter: d.Delim,
		Attrs:     mapMailboxAttrs(d.Attrs),
		Status:    mapListStatus(d.Status),
	}
}

// mapListStatus converts the STATUS a LIST-STATUS response attached to one
// mailbox, or returns nil when it cannot be relied on (#620).
//
// go-imap leaves a status item the server did not send at its zero value
// (UIDValidity, UIDNext) or nil (NumMessages), which makes an omitted item
// indistinguishable from a real zero. UIDVALIDITY and UIDNEXT are never zero
// on a real mailbox (RFC 9051 §2.3.1.1: both are non-zero), so a zero there
// means "not reported"; MESSAGES is a pointer, so nil means the same. Any one
// of them missing makes the whole status nil: a caller deciding "nothing has
// changed, skip the SELECT" must never decide it from a partial answer.
func mapListStatus(s *imap.StatusData) *ListStatus {
	if s == nil || s.UIDValidity == 0 || s.UIDNext == 0 || s.NumMessages == nil {
		return nil
	}
	return &ListStatus{
		UIDValidity:   s.UIDValidity,
		UIDNext:       UID(s.UIDNext),
		NumMessages:   *s.NumMessages,
		HighestModSeq: s.HighestModSeq,
	}
}

// mapMailboxAttrs converts go-imap mailbox attributes (base attributes plus
// RFC 6154 SPECIAL-USE roles) to our MailboxAttr enum.
func mapMailboxAttrs(attrs []imap.MailboxAttr) []MailboxAttr {
	if attrs == nil {
		return nil
	}
	out := make([]MailboxAttr, len(attrs))
	for i, a := range attrs {
		out[i] = MailboxAttr(a)
	}
	return out
}

// mapFlags converts go-imap message flags to our Flag type.
func mapFlags(flags []imap.Flag) []Flag {
	if flags == nil {
		return nil
	}
	out := make([]Flag, len(flags))
	for i, f := range flags {
		out[i] = Flag(f)
	}
	return out
}

func mapAddress(a imap.Address) Address {
	return Address{Name: a.Name, Mailbox: a.Mailbox, Host: a.Host}
}

func mapAddresses(addrs []imap.Address) []Address {
	if addrs == nil {
		return nil
	}
	out := make([]Address, len(addrs))
	for i, a := range addrs {
		out[i] = mapAddress(a)
	}
	return out
}

func mapEnvelope(e *imap.Envelope) *Envelope {
	if e == nil {
		return nil
	}
	return &Envelope{
		Date:      e.Date,
		Subject:   e.Subject,
		From:      mapAddresses(e.From),
		Sender:    mapAddresses(e.Sender),
		ReplyTo:   mapAddresses(e.ReplyTo),
		To:        mapAddresses(e.To),
		Cc:        mapAddresses(e.Cc),
		Bcc:       mapAddresses(e.Bcc),
		InReplyTo: e.InReplyTo,
		MessageID: e.MessageID,
	}
}

// mapSelectData converts a SELECT/EXAMINE response into our MailboxStatus.
func mapSelectData(mailbox string, d *imap.SelectData) *MailboxStatus {
	status := &MailboxStatus{
		Name:           mailbox,
		UIDValidity:    d.UIDValidity,
		UIDNext:        UID(d.UIDNext),
		HighestModSeq:  d.HighestModSeq,
		NumMessages:    d.NumMessages,
		Flags:          mapFlags(d.Flags),
		PermanentFlags: mapFlags(d.PermanentFlags),
	}
	for _, f := range d.PermanentFlags {
		if f == imap.FlagWildcard {
			status.PermanentFlagsAllowNew = true
			break
		}
	}
	return status
}

// mapFetchBuffer converts a collected FETCH response for one message into
// our Message type.
func mapFetchBuffer(buf *imapclient.FetchMessageBuffer) Message {
	msg := Message{
		MessageMeta: MessageMeta{
			UID:          UID(buf.UID),
			SeqNum:       buf.SeqNum,
			Flags:        mapFlags(buf.Flags),
			Envelope:     mapEnvelope(buf.Envelope),
			InternalDate: buf.InternalDate,
			RFC822Size:   buf.RFC822Size,
			ModSeq:       buf.ModSeq,
			GmailMsgID:   buf.GmailMsgID,
			GmailThrID:   buf.GmailThrID,
			GmailLabels:  buf.GmailLabels,
		},
	}
	if len(buf.BodySection) > 0 {
		msg.Body = buf.BodySection[0].Bytes
	}
	return msg
}

// uidSetToIMAP converts our UIDSet to go-imap's imap.UIDSet.
func uidSetToIMAP(s UIDSet) imap.UIDSet {
	var out imap.UIDSet
	for _, r := range s {
		out.AddRange(imap.UID(r.Start), imap.UID(r.Stop))
	}
	return out
}

// flagsToIMAP converts our Flag type to go-imap message flags, for STORE and
// APPEND — the reverse direction of mapFlags.
func flagsToIMAP(flags []Flag) []imap.Flag {
	if flags == nil {
		return nil
	}
	out := make([]imap.Flag, len(flags))
	for i, f := range flags {
		out[i] = imap.Flag(f)
	}
	return out
}
