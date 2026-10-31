// SPDX-License-Identifier: AGPL-3.0-only

// Package tabs is the Gmail-style category tab layer (mailroom#219): the closed
// vocabulary of tabs, the rule that picks ONE of them for a message, and the
// fold that applies the user's enabled set.
//
// WHY THIS IS NOT THE LABEL LAYER, AND WHY THAT DISTINCTION IS THE WHOLE POINT.
// #156 spent a long time failing because one mechanism was asked to do two
// orthogonal jobs, and #169 measured the failure from both directions:
//
//   - FUNCTION is what a message DOES — marketing, receipt, notification, list
//     traffic. It is legible from message FORM: List-Id (RFC 2919),
//     List-Unsubscribe (RFC 2369), one-click (RFC 8058), Auto-Submitted
//     (RFC 3834), Precedence, Feedback-ID, DKIM d=, Return-Path. Because the
//     RFCs define what those headers MEAN, they mean the same thing for a sender
//     never seen before — so function is sender-independent BY CONSTRUCTION.
//   - TOPIC is what a message is ABOUT — theatre, gaming, a particular creator.
//     Open-ended vocabulary, not enumerable in a way that survives an unseen
//     sender. That is what needs a semantic representation, i.e. embeddings, i.e.
//     #67, i.e. v2.
//
// GMAIL'S FIVE TABS ARE ALL FUNCTION CATEGORIES. Gmail never claims to know that
// a newsletter is about theatre; a theatre newsletter and a tech newsletter land
// in the same tab. So the thing that blocked topic generalisation does not block
// tabs, and tabs need no embeddings and no corpus — only public RFC facts, which
// spec §8 explicitly permits.
//
// Tabs are therefore a SECOND AXIS alongside labels, not a replacement for them.
// Labels stay topic, decided from identity and rules; tabs are function, decided
// here. A message has exactly one tab and any number of labels.
//
// This package holds no state and touches no database: internal/store owns the
// per-user mapping and enabled set (migration 00028), and calls in here for the
// decisions. It is pure so the same rules run on the serve path, the replay
// path, and in tests — a tab rule that could differ between them would be
// measuring something nobody ships.
package tabs

// Tab is one category tab. The vocabulary is closed and mirrors the
// app.category_tab enum: a tab is only useful if something can decide it, the
// decider is code, so a sixth tab is a release either way.
type Tab string

const (
	// Primary is person-to-person mail and anything that is not functional bulk.
	// It is also the RESIDUE: everything a tab was not decided for lands here,
	// which is why it can never be disabled.
	Primary Tab = "primary"
	// Social is mail from social and community platforms.
	Social Tab = "social"
	// Promotions is marketing bulk — the advertisements half of the retailer
	// problem.
	Promotions Tab = "promotions"
	// Updates is machine mail about something that happened: confirmations,
	// receipts, statements, alerts. The receipts half.
	Updates Tab = "updates"
	// Forums is RFC 2919 list traffic — discussion lists and the newsletter
	// platforms that carry List-Id.
	//
	// It ships even though a typical mapping routes nothing to it,
	// by design: it can ship empty and the mapping is expected to be
	// rearranged later. A Forums recall of 0 against today's mapping is EXPECTED,
	// not a broken tab (#219).
	Forums Tab = "forums"
)

// All is the vocabulary in Gmail's own strip order, which is the default
// position for a newly materialised set of tabs. A user rearranges it from
// there (app.category_tabs.position).
var All = []Tab{Primary, Social, Promotions, Updates, Forums}

// Valid reports whether t is one of the five.
func (t Tab) Valid() bool {
	for _, k := range All {
		if k == t {
			return true
		}
	}
	return false
}

// Parse converts a stored or wire value to a Tab. ok is false for anything
// outside the vocabulary — callers reject rather than coerce, since a tab that
// silently became Primary would be a mapping the operator set and cannot see.
func Parse(s string) (Tab, bool) {
	t := Tab(s)
	return t, t.Valid()
}

// precedence orders the tabs from NARROWEST function to broadest, and is the
// tie-break when a message's labels map to more than one tab. A tab holds a
// message exactly once (Gmail's model), so some rule is required; this is that
// rule, stated rather than derived from an attempt to divine per-message intent.
//
// The reasoning, in order:
//
//   - Forums first, because it is the only tab decided by a PROTOCOL FACT. A
//     message carrying List-Id is list traffic per RFC 2919 whatever else is
//     true of it.
//   - Social next: a named, bounded class of sender.
//   - Primary last, because it is the residue — "none of the above" is the
//     weakest possible claim about a message and must never outrank a positive
//     one.
//
// PROMOTIONS-BEFORE-UPDATES IS THE ONE ARBITRARY PAIR and is flagged rather than
// justified. Both are broad bulk classes and neither is obviously narrower;
// #219 names Promotions vs Updates as the known-hard boundary for the classifier
// too. It is tolerable because it currently fires on NOTHING: #223 left all 738
// labelled messages with exactly one label each, so no message reaches this
// tie-break at all. `mailroom-admin tabs` counts how often each pair is actually
// invoked, so if that stops being zero the choice can be revisited on evidence
// instead of argument.
var precedence = []Tab{Forums, Social, Promotions, Updates, Primary}

// Precedence returns the tie-break order, narrowest first, as a fresh slice.
//
// EXPORTED SO SQL DOES NOT HAVE TO REDEFINE IT. The Email/query tab filter
// orders a message's mapped labels with `array_position($1::text[], l.tab::text)`
// and binds this slice as $1 (ADR-022). That keeps the order defined in exactly
// one place: a lookup table or an IMMUTABLE function in the database would be a
// second definition, and the first edit to either would silently diverge.
//
// A COPY, not the package slice: a caller that sorted or truncated the returned
// value would otherwise reorder the tie-break for the whole process.
func Precedence() []Tab {
	out := make([]Tab, len(precedence))
	copy(out, precedence)
	return out
}

// PrecedenceNames is Precedence() as strings, ready to bind as a SQL text[].
func PrecedenceNames() []string {
	out := make([]string, len(precedence))
	for i, t := range precedence {
		out[i] = string(t)
	}
	return out
}

// rank is a tab's position in the precedence order; lower wins.
func rank(t Tab) int {
	for i, k := range precedence {
		if k == t {
			return i
		}
	}
	// Not in the vocabulary: sorts behind everything real. Unreachable through
	// Parse, and deliberately not a panic — a stray value must not take down a
	// mailbox render.
	return len(precedence)
}

// FromLabels picks the tab for a message from the tabs its labels map to.
//
// ok is false when the message has no labels, or none of its labels is mapped —
// which is not an error but the signal to fall through to the classifier
// (function facts), and the population `mailroom-admin tabs` reports as the
// classifier's actual workload.
//
// contested carries the distinct OTHER tabs that lost the tie-break, empty when
// the labels agreed. It exists so the report can count how often precedence
// actually arbitrated rather than assuming it never does: a rule nobody measures
// is a rule nobody can correct.
func FromLabels(mapped []Tab) (winner Tab, contested []Tab, ok bool) {
	best := -1
	seen := map[Tab]bool{}
	var distinct []Tab
	for _, t := range mapped {
		if !t.Valid() {
			continue
		}
		if !seen[t] {
			seen[t] = true
			distinct = append(distinct, t)
		}
		if r := rank(t); best < 0 || r < best {
			best, winner = r, t
		}
	}
	if best < 0 {
		return "", nil, false
	}
	for _, t := range distinct {
		if t != winner {
			contested = append(contested, t)
		}
	}
	return winner, contested, true
}

// Fold applies the user's enabled set: mail whose tab is switched off appears in
// Primary, which is what unchecking a category does in Gmail. Primary itself
// cannot be disabled (app.category_tabs enforces it), so this always terminates
// at a tab the user can actually see.
func Fold(t Tab, enabled map[Tab]bool) Tab {
	if enabled[t] {
		return t
	}
	return Primary
}
