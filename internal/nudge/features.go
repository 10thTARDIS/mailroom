// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"sort"
	"time"

	"mailroom/internal/kind"
)

// ONE RULE IMPLEMENTATION (#58 phase 2). Everything in this file is called by
// BOTH the measurement (Evaluate, behind `mailroom-admin nudge-report`) and the
// live nudge (Due, behind FollowUp/get), so the rule the user sees is exactly
// a rule that was measured. features_test.go pins that the report's rule
// tables hold these very functions, not copies of them.

// Features are one inbound message's signals.
type Features struct {
	NoListID, NotBulk, NotAutomated, NotNoReply bool
	MeInTo, MeCcOnly                            bool
	Personal, NotMachine                        bool
	Corresponded, IsReply                       bool
	Question, Request                           bool
}

// Person is "from a person": no List-Id, no bulk apparatus, not
// Auto-Submitted, not a no-reply sender.
func (f Features) Person() bool { return f.NoListID && f.NotBulk && f.NotAutomated && f.NotNoReply }

// Asks is a question mark or request phrasing.
func (f Features) Asks() bool { return f.Question || f.Request }

// WaitingOnYou is the SHIPPED inbound rule, "you haven't replied to this":
// from a person, with the user in To, and either decided personal by the kind
// classifier or itself a reply. Measured over a year of real mail, only a
// minority of such messages were true "waiting on you" cases, which
// is why the nudge built on it is opt-in.
func WaitingOnYou(f Features) bool {
	return f.Person() && f.MeInTo && (f.Personal || f.IsReply)
}

// InboundFeatures computes m's features. corresponded must be AS OF m: the
// user had written to m's sender before m arrived (index.correspondedBefore),
// never "has ever written", which a reply to m itself would make true.
func InboundFeatures(m *Message, me Me, corresponded bool) Features {
	f := m.Facts
	f.HasCorresponded = corresponded
	f.IsReply = f.IsReply || m.InReplyTo != ""
	v := kind.Classify(f)
	ft := Features{
		NoListID:     !HasListID(f),
		NotBulk:      !IsBulk(f),
		NotAutomated: !IsAutomated(f),
		NotNoReply:   !kind.IsNoReply(m.From),
		Personal:     KindPersonal(v),
		NotMachine:   KindNotMachine(v),
		Corresponded: corresponded,
		IsReply:      f.IsReply,
		Question:     m.Question,
		Request:      m.Request,
	}
	inTo, inCc := false, false
	for _, a := range m.To {
		if me.Is(a) {
			inTo = true
		}
	}
	for _, a := range m.Cc {
		if me.Is(a) {
			inCc = true
		}
	}
	ft.MeInTo, ft.MeCcOnly = inTo, inCc && !inTo
	return ft
}

// OutFeatures are one sent message's signals.
type OutFeatures struct {
	Started          bool // first message of its thread, and not a reply
	ToSelfOnly       bool // every recipient is the user (or none parsed)
	NoReplyRecipient bool // a recipient is a no-reply address
	ListThread       bool // the conversation holds inbound list/bulk/automated mail
	Asks             bool
}

// ToPeople is "sent to people": not only to the user, no no-reply recipient,
// and not a conversation with list, bulk or automated mail.
func (f OutFeatures) ToPeople() bool { return !f.ToSelfOnly && !f.NoReplyRecipient && !f.ListThread }

// NoOneAnswered is the SHIPPED outbound rule, "no one answered your mail":
// to-people. Measured over a year of real mail: few were answered within 7 days,
// almost none answered late by day 30, and it produces under one nudge a week.
func NoOneAnswered(f OutFeatures) bool { return f.ToPeople() }

// IsHumanAnswer reports whether o, arriving after one of the user's messages,
// counts as an answer to it: not the user's own, not junk, and not an
// automatic reply (RFC 3834 Auto-Submitted, e.g. a vacation responder).
func IsHumanAnswer(o *Message) bool {
	return !o.Outbound() && !o.Junk && (!o.Parsed || !IsAutomated(o.Facts))
}

// isBulkish is inbound mail that makes a conversation "list traffic".
func isBulkish(o *Message) bool {
	return o.Parsed && (HasListID(o.Facts) || IsBulk(o.Facts) || IsAutomated(o.Facts) || kind.IsNoReply(o.From))
}

// index is the conversation structure both Evaluate and Due read.
type index struct {
	ms        []*Message // sorted by ID
	byThread  map[string][]*Message
	byMsgID   map[string]*Message
	byParent  map[string][]*Message
	firstSent map[string]time.Time
	memo      map[*Message][]*Message
}

func newIndex(msgs []Message, me Me) *index {
	ix := &index{
		byThread:  map[string][]*Message{},
		byMsgID:   map[string]*Message{},
		byParent:  map[string][]*Message{},
		firstSent: map[string]time.Time{},
		memo:      map[*Message][]*Message{},
	}
	ix.ms = make([]*Message, len(msgs))
	for i := range msgs {
		ix.ms[i] = &msgs[i]
	}
	sort.Slice(ix.ms, func(i, j int) bool { return ix.ms[i].ID < ix.ms[j].ID })
	for _, m := range ix.ms {
		if m.Thread != "" {
			ix.byThread[m.Thread] = append(ix.byThread[m.Thread], m)
		}
		if m.MessageID != "" {
			if _, dup := ix.byMsgID[m.MessageID]; !dup {
				ix.byMsgID[m.MessageID] = m
			}
		}
		if m.InReplyTo != "" {
			ix.byParent[m.InReplyTo] = append(ix.byParent[m.InReplyTo], m)
		}
		// As-of correspondence: the first time the user sent to each address,
		// built from the user's sent mail itself, NOT contact_addresses.
		// outbound_count, which is today's total — a reply to the message
		// being judged also bumps it (the #220 fixed-point trap).
		if m.Outbound() {
			for _, r := range recipients(m) {
				if me.Is(r) {
					continue
				}
				if t, ok := ix.firstSent[r]; !ok || m.At.Before(t) {
					ix.firstSent[r] = m.At
				}
			}
		}
	}
	for _, t := range ix.byThread {
		sort.SliceStable(t, func(i, j int) bool { return t[i].At.Before(t[j].At) })
	}
	return ix
}

// correspondedBefore: the user had written to m's sender before m arrived.
func (ix *index) correspondedBefore(m *Message) bool {
	first, ok := ix.firstSent[norm(m.From)]
	return ok && first.Before(m.At)
}

// related returns the messages that arrive after m in its conversation: the
// rest of its thread, plus anything whose In-Reply-To names it (a reply that
// threading missed still counts), oldest first. Memoised.
func (ix *index) related(m *Message) []*Message {
	if out, ok := ix.memo[m]; ok {
		return out
	}
	var out []*Message
	seen := map[*Message]bool{}
	if m.Thread != "" {
		for _, o := range ix.byThread[m.Thread] {
			if o != m && o.At.After(m.At) {
				out = append(out, o)
				seen[o] = true
			}
		}
	}
	if m.MessageID != "" {
		for _, o := range ix.byParent[m.MessageID] {
			if !seen[o] && o != m && o.At.After(m.At) {
				out = append(out, o)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	ix.memo[m] = out
	return out
}

// outboundFeatures computes a sent message's features from its conversation.
func (ix *index) outboundFeatures(m *Message, me Me) OutFeatures {
	f := OutFeatures{Asks: m.Question || m.Request}
	f.Started = m.InReplyTo == ""
	if m.Thread != "" {
		for _, o := range ix.byThread[m.Thread] {
			if o == m || !o.At.Before(m.At) {
				continue
			}
			f.Started = false
			if !o.Outbound() && isBulkish(o) {
				f.ListThread = true
			}
		}
	}
	if m.InReplyTo != "" {
		if p, ok := ix.byMsgID[m.InReplyTo]; ok && !p.Outbound() && isBulkish(p) {
			f.ListThread = true
		}
	}
	f.ToSelfOnly = true
	for _, r := range recipients(m) {
		if !me.Is(r) {
			f.ToSelfOnly = false
		}
		if kind.IsNoReply(r) {
			f.NoReplyRecipient = true
		}
	}
	return f
}

func recipients(m *Message) []string {
	out := make([]string, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	for _, l := range [][]string{m.To, m.Cc, m.Bcc} {
		for _, a := range l {
			if a = norm(a); a != "" {
				out = append(out, a)
			}
		}
	}
	return out
}
