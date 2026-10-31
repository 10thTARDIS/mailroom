// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"sort"
	"time"
)

// THE LIVE NUDGE (#58 phase 2): what is due NOW, for FollowUp/get. Computed on
// read from the messages the store hands over; nothing about a nudge is stored
// except the user's "Done" (app.nudge_dismissals). The rules are the shipped
// ones in features.go — the same functions the report measured.

// Kind is which nudge.
type Kind string

const (
	// KindOutbound is "no one answered your mail from <day>".
	KindOutbound Kind = "outbound"
	// KindInbound is "you haven't replied to this".
	KindInbound Kind = "inbound"
)

// OutboundAfter is how long a sent message waits for a human answer before
// it is nudged — the 7 days measured (#58). Fixed, not a setting: it is the
// horizon the measurement chose (most answers had arrived by then, none late by day 30).
const OutboundAfter = 7 * day

// MaxAge is how old a message may be and still be nudged. Past it the nudge
// expires on its own: the measurement found no late answer by day 30, so a
// month-old silence is a closed question, and a list that only ever grew
// would become a second inbox. It also bounds the work a read does.
const MaxAge = 30 * day

// DefaultInboundAfter is the inbound delay when the user has not chosen one.
const DefaultInboundAfter = 3 * day

// LiveConfig is what the user has switched on, and when "now" is.
type LiveConfig struct {
	Now          time.Time
	Outbound     bool
	Inbound      bool
	InboundAfter time.Duration
}

// Nudge is one due follow-up.
type Nudge struct {
	MessageID string
	Kind      Kind
	// Since is when the message arrived (or was sent) — the "<day>".
	Since time.Time
	// DueAt is when the nudge became due: Since plus the delay.
	DueAt time.Time
}

// Due returns the nudges due at cfg.Now, oldest first.
//
// msgs must hold every candidate (the user's sent mail and inbound mail in
// the MaxAge window), every message after them in their conversations, and —
// for the inbound rule's as-of correspondence — the user's sent mail before
// them. dismissed holds message ids the user marked Done; they never return.
// Snoozed, trashed and drafted messages are the store's to leave out.
//
// Unlike the report, which asks "was it answered within N days", this asks
// "has it been answered AT ALL by now": a late answer makes the nudge vanish,
// which is what the user wants from it.
func Due(msgs []Message, me Me, cfg LiveConfig, dismissed map[string]bool) []Nudge {
	ix := newIndex(msgs, me)
	inboundAfter := cfg.InboundAfter
	if inboundAfter <= 0 {
		inboundAfter = DefaultInboundAfter
	}
	oldest := cfg.Now.Add(-MaxAge)
	var out []Nudge
	for _, m := range ix.ms {
		if m.ContextOnly || dismissed[m.ID] || !m.Parsed || m.At.Before(oldest) {
			continue
		}
		if m.Outbound() {
			if !cfg.Outbound || cfg.Now.Sub(m.At) < OutboundAfter {
				continue
			}
			if !NoOneAnswered(ix.outboundFeatures(m, me)) {
				continue
			}
			waiting := true
			for _, o := range ix.related(m) {
				// A human answer closes it; the user's own later message in the
				// conversation means the nudge belongs to THAT one, if anything.
				if IsHumanAnswer(o) || o.Outbound() {
					waiting = false
					break
				}
			}
			if waiting {
				out = append(out, Nudge{MessageID: m.ID, Kind: KindOutbound, Since: m.At, DueAt: m.At.Add(OutboundAfter)})
			}
			continue
		}

		if !cfg.Inbound || m.Junk || m.Answered || cfg.Now.Sub(m.At) < inboundAfter {
			continue
		}
		if !WaitingOnYou(InboundFeatures(m, me, ix.correspondedBefore(m))) {
			continue
		}
		waiting := true
		for _, o := range ix.related(m) {
			if o.Outbound() || (o.Answered && !o.Junk) {
				waiting = false // the user replied (here, or from a client whose Sent copy is not synced)
				break
			}
			// A later message in the same conversation that would itself wait
			// on the user takes the nudge — one per conversation, the newest —
			// and a Done on that one covers the conversation, so this earlier
			// message does not come back in its place.
			if !o.Junk && o.Parsed && WaitingOnYou(InboundFeatures(o, me, ix.correspondedBefore(o))) {
				waiting = false
				break
			}
		}
		if waiting {
			out = append(out, Nudge{MessageID: m.ID, Kind: KindInbound, Since: m.At, DueAt: m.At.Add(inboundAfter)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
