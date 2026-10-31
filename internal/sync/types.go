// SPDX-License-Identifier: AGPL-3.0-only

package sync

import "time"

// UID is a message unique identifier, scoped to a mailbox's UIDVALIDITY
// (RFC 9051 §2.3.1.1). Our own type, so go-imap's imap.UID never needs to
// appear in an exported signature.
type UID uint32

// UIDRange is an inclusive range of UIDs. A Stop of 0 means "the highest UID
// in the mailbox" — the IMAP "*" wildcard — enabling open-ended ranges such
// as "everything from lastKnownUID+1 onward" for incremental sync.
type UIDRange struct {
	Start, Stop UID
}

// UIDSet identifies a set of messages by UID, for FetchByUID.
type UIDSet []UIDRange

// UIDSetNum returns a UIDSet containing exactly the given UIDs.
func UIDSetNum(uids ...UID) UIDSet {
	s := make(UIDSet, len(uids))
	for i, u := range uids {
		s[i] = UIDRange{Start: u, Stop: u}
	}
	return s
}

// UIDSetRange returns a UIDSet containing the single inclusive range
// [start, stop]. stop == 0 means open-ended ("start:*"): everything from
// start onward, including messages that arrive after the range is
// constructed — the shape incremental sync needs to say "give me everything
// since I last looked".
func UIDSetRange(start, stop UID) UIDSet {
	return UIDSet{{Start: start, Stop: stop}}
}

// IsEmpty reports whether the set contains no ranges.
func (s UIDSet) IsEmpty() bool {
	return len(s) == 0
}

// Flag is an IMAP message flag (RFC 9051 §2.3.2), e.g. FlagSeen. Our own
// type, but deliberately kept wire-compatible (same string spelling as
// go-imap's imap.Flag) so flags round-trip unchanged through writeback
// (spec §6's IMAP keyword mirroring) without a lossy conversion table.
type Flag string

const (
	FlagSeen     Flag = "\\Seen"
	FlagAnswered Flag = "\\Answered"
	FlagFlagged  Flag = "\\Flagged"
	FlagDeleted  Flag = "\\Deleted"
	FlagDraft    Flag = "\\Draft"

	FlagForwarded Flag = "$Forwarded"
	FlagMDNSent   Flag = "$MDNSent" // Message Disposition Notification sent
	FlagJunk      Flag = "$Junk"
	FlagNotJunk   Flag = "$NotJunk"
	FlagPhishing  Flag = "$Phishing"
	FlagImportant Flag = "$Important" // RFC 8457

	// FlagWildcard ("\*"), when present in MailboxStatus.PermanentFlags,
	// means the mailbox accepts arbitrary user-defined keywords. See
	// MailboxStatus.PermanentFlagsAllowNew.
	FlagWildcard Flag = "\\*"
)

// MailboxAttr is a mailbox attribute (RFC 9051 §7.3.1), including RFC 6154
// SPECIAL-USE roles. Our own type; wire-compatible values (same rationale
// as Flag).
type MailboxAttr string

const (
	MailboxAttrNonExistent   MailboxAttr = "\\NonExistent"
	MailboxAttrNoInferiors   MailboxAttr = "\\Noinferiors"
	MailboxAttrNoSelect      MailboxAttr = "\\Noselect"
	MailboxAttrHasChildren   MailboxAttr = "\\HasChildren"
	MailboxAttrHasNoChildren MailboxAttr = "\\HasNoChildren"
	MailboxAttrMarked        MailboxAttr = "\\Marked"
	MailboxAttrUnmarked      MailboxAttr = "\\Unmarked"
	MailboxAttrSubscribed    MailboxAttr = "\\Subscribed"
	MailboxAttrRemote        MailboxAttr = "\\Remote"

	// RFC 6154 SPECIAL-USE roles.
	MailboxAttrAll       MailboxAttr = "\\All"
	MailboxAttrArchive   MailboxAttr = "\\Archive"
	MailboxAttrDrafts    MailboxAttr = "\\Drafts"
	MailboxAttrFlagged   MailboxAttr = "\\Flagged"
	MailboxAttrJunk      MailboxAttr = "\\Junk"
	MailboxAttrSent      MailboxAttr = "\\Sent"
	MailboxAttrTrash     MailboxAttr = "\\Trash"
	MailboxAttrImportant MailboxAttr = "\\Important" // RFC 8457
)

// specialUseAttrs are the RFC 6154 role attributes.
var specialUseAttrs = map[MailboxAttr]bool{
	MailboxAttrAll:       true,
	MailboxAttrArchive:   true,
	MailboxAttrDrafts:    true,
	MailboxAttrFlagged:   true,
	MailboxAttrJunk:      true,
	MailboxAttrSent:      true,
	MailboxAttrTrash:     true,
	MailboxAttrImportant: true,
}

// Mailbox describes one mailbox as returned by ListMailboxes.
type Mailbox struct {
	Name      string
	Delimiter rune // hierarchy delimiter; zero if the server sent NIL
	Attrs     []MailboxAttr

	// Status is the mailbox's status as the server reported it INLINE in the
	// LIST response (RFC 5819 LIST-STATUS, folded into IMAP4rev2), or nil when
	// there is none: the server supports neither, it sent no STATUS for this
	// mailbox (a \Noselect node never gets one), or what it sent was missing
	// one of the numbers a caller needs to compare (see ListStatus). NIL MEANS
	// "UNKNOWN", never "unchanged" — the sync engine treats a nil Status
	// exactly as it treated every mailbox before LIST-STATUS was requested
	// (#620): it SELECTs it.
	Status *ListStatus
}

// ListStatus is the per-mailbox status a LIST-STATUS response carries (#620):
// the four numbers that together say whether ANYTHING about a mailbox's
// message set or flags has changed since they were last recorded.
//
//   - UIDValidity names the UID generation.
//   - UIDNext moves when a message is added (it never moves backwards).
//   - NumMessages moves when a message is added or expunged.
//   - HighestModSeq (RFC 7162) moves when any flag changes; with CONDSTORE it
//     is the only one of the four that sees a flag change at all.
//
// A ListStatus is only ever constructed with UIDValidity, UIDNext and
// NumMessages all reported. HighestModSeq is zero when CONDSTORE is not
// supported (it is then not requested) or the server did not report it; a
// caller comparing it must treat zero as "unknown", not as a value.
type ListStatus struct {
	UIDValidity   uint32
	UIDNext       UID
	NumMessages   uint32
	HighestModSeq uint64
}

// SpecialUse returns the mailbox's RFC 6154 role attribute, if any. Per
// RFC 6154 a mailbox has at most one role in practice, but nothing in the
// RFC forbids a server from sending more than one; SpecialUse returns the
// first one found.
func (m Mailbox) SpecialUse() (MailboxAttr, bool) {
	for _, a := range m.Attrs {
		if specialUseAttrs[a] {
			return a, true
		}
	}
	return "", false
}

// HasAttr reports whether the mailbox carries the given attribute.
func (m Mailbox) HasAttr(a MailboxAttr) bool {
	for _, x := range m.Attrs {
		if x == a {
			return true
		}
	}
	return false
}

// SelectOptions controls a Select call.
type SelectOptions struct {
	ReadOnly bool
	// CondStore requests CONDSTORE for the session (RFC 7162). Only set
	// this after confirming Supports(CapabilityCondStore); the server
	// rejects SELECT/EXAMINE otherwise.
	CondStore bool
}

// MailboxStatus is the data returned by Select (SELECT/EXAMINE,
// RFC 9051 §6.3.1/§6.3.2).
type MailboxStatus struct {
	Name        string
	UIDValidity uint32
	UIDNext     UID
	// HighestModSeq is 0 when CONDSTORE was not requested or not supported
	// by the server.
	HighestModSeq  uint64
	NumMessages    uint32
	Flags          []Flag
	PermanentFlags []Flag
	// PermanentFlagsAllowNew is true when PermanentFlags contains
	// FlagWildcard ("\*"), meaning the mailbox accepts arbitrary
	// user-defined keywords. Per spec §6, mailboxes where this is false are
	// local-labels-only for the hybrid label model.
	PermanentFlagsAllowNew bool
}

// Address is a message sender/recipient (RFC 9051 §6.4.5 address
// structure).
type Address struct {
	Name    string
	Mailbox string
	Host    string
}

// Addr returns "mailbox@host", or "" for group start/end markers (where
// Host is empty).
func (a Address) Addr() string {
	if a.Mailbox == "" || a.Host == "" {
		return ""
	}
	return a.Mailbox + "@" + a.Host
}

// Envelope is a message's envelope (RFC 9051 §6.4.5 ENVELOPE data item).
type Envelope struct {
	Date      time.Time
	Subject   string
	From      []Address
	Sender    []Address
	ReplyTo   []Address
	To        []Address
	Cc        []Address
	Bcc       []Address
	InReplyTo []string
	MessageID string
}

// FetchItems selects which data FetchByUID retrieves.
type FetchItems struct {
	Flags        bool
	Envelope     bool
	InternalDate bool
	RFC822Size   bool
	// ModSeq requests the per-message MODSEQ (RFC 7162 CONDSTORE); requires
	// Supports(CapabilityCondStore).
	ModSeq bool
	// ChangedSince, when non-zero, restricts the fetch to messages whose
	// MODSEQ has changed since this value (RFC 7162 CONDSTORE); requires
	// Supports(CapabilityCondStore).
	ChangedSince uint64
	// BodyFull requests the full RFC 822 message body.
	BodyFull bool
	// GmailExt requests Gmail's X-GM-MSGID, X-GM-THRID and X-GM-LABELS
	// (#52, ADR-047); requires Supports(CapabilityGmailExt). A server without
	// the extension rejects the whole FETCH.
	GmailExt bool
}

// MessageMeta is a message's metadata, without body bytes.
type MessageMeta struct {
	UID          UID
	SeqNum       uint32
	Flags        []Flag
	Envelope     *Envelope
	InternalDate time.Time
	RFC822Size   int64
	// ModSeq is 0 when FetchItems.ModSeq was not requested or CONDSTORE is
	// unsupported.
	ModSeq uint64

	// The Gmail X-GM-EXT-1 items (FetchItems.GmailExt). GmailMsgID is Gmail's
	// 64-bit message id, the same in every folder the message appears in;
	// GmailThrID its conversation id. Both are 0 when not requested.
	// GmailLabels is nil when not requested and non-nil (possibly empty) when
	// the server answered: system labels keep their backslash (`\Inbox`,
	// `\Sent`, `\Important`), user labels are decoded from modified UTF-7.
	GmailMsgID  uint64
	GmailThrID  uint64
	GmailLabels []string
}

// Message is a fetched message: metadata plus optional body bytes.
type Message struct {
	MessageMeta
	// Body holds the full RFC 822 message, or nil if FetchItems.BodyFull
	// was not set.
	Body []byte
}

// Capability is an IMAP capability name (RFC 9051 §7.2.1), e.g.
// CapabilityCondStore. Our own type; wire-compatible values so callers can
// check capabilities we haven't named a constant for.
type Capability string

const (
	CapabilityCondStore Capability = "CONDSTORE" // RFC 7162
	// CapabilityQResync is RFC 7162's other half. See NOTES.md: go-imap
	// v2 beta.8 exposes the capability *name* for detection only — there is
	// no way to actually drive a QRESYNC exchange with this client library.
	CapabilityQResync    Capability = "QRESYNC"
	CapabilityIdle       Capability = "IDLE"        // RFC 2177 / RFC 9051
	CapabilityMove       Capability = "MOVE"        // RFC 6851
	CapabilityUIDPlus    Capability = "UIDPLUS"     // RFC 4315
	CapabilitySpecialUse Capability = "SPECIAL-USE" // RFC 6154
	CapabilityNamespace  Capability = "NAMESPACE"   // RFC 2342
	// CapabilityGmailExt is Gmail's X-GM-EXT-1 (#52, ADR-047): X-GM-MSGID,
	// X-GM-THRID and X-GM-LABELS. Gmail advertises it before authentication.
	CapabilityGmailExt Capability = "X-GM-EXT-1"
)

// NotificationKind classifies unilateral server data delivered on
// SyncSession.Notifications.
type NotificationKind int

const (
	NotificationUnknown NotificationKind = iota
	// NotificationExpunge: a message was expunged; SeqNum is set.
	NotificationExpunge
	// NotificationExists: the mailbox's message count changed (new mail
	// arrived, or an EXPUNGE lowered it); NumMessages is set.
	NotificationExists
	// NotificationFlags: the mailbox's FLAGS or PERMANENTFLAGS changed.
	NotificationFlags
)

// Notification is one piece of unilateral server data — a hint that the
// caller should reconcile, not a delivery-guaranteed event. See
// SyncSession.Notifications.
type Notification struct {
	Kind        NotificationKind
	SeqNum      uint32
	NumMessages *uint32
}
