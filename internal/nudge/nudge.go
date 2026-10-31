// SPDX-License-Identifier: AGPL-3.0-only

// Package nudge holds the signals and the accuracy measurement behind
// follow-up nudges (#58): "you haven't replied to this in N days" (an inbound
// thread waiting on the user) and "no one answered the mail you sent on
// <day>" (an outbound thread waiting on someone else).
//
// It is pure — no database, no clock of its own, no blob access.
// mailroom-admin nudge-report gathers the messages and parses their headers;
// Evaluate turns them into per-rule accuracy against what the user actually
// did next. When the nudge itself is built, it should run the SAME predicates
// this package measures, so the report can never describe a rule that is not
// the one shipped (the ADR-023 §2 discipline placement follows).
//
// WHY A REPORT FIRST. An early audit on #58 found no signal at all on a
// real mailbox then (almost nothing sent, nothing \Answered). The signal exists
// now, and the rule has to be chosen on measured precision — gated on the
// bottom of its Wilson interval — and on how many nudges a week it would
// produce, because alert fatigue is the harm a nudge does.
package nudge

import (
	"regexp"
	"strings"

	"mailroom/internal/kind"
)

// Me is the user's own addresses: fixed identity addresses and wildcard
// identity domains, compared case-insensitively. A subaddress (me+tag@d) of a
// fixed address is also me (RFC 5233), matching the store's subaddress rule.
type Me struct {
	addrs   map[string]bool
	domains map[string]bool
}

// NewMe builds a Me from identity addresses and wildcard domains.
func NewMe(addresses, domains []string) Me {
	m := Me{addrs: map[string]bool{}, domains: map[string]bool{}}
	for _, a := range addresses {
		if a = norm(a); a != "" {
			m.addrs[a] = true
		}
	}
	for _, d := range domains {
		if d = strings.TrimSuffix(norm(d), "."); d != "" {
			m.domains[d] = true
		}
	}
	return m
}

// Is reports whether addr is one of the user's own addresses.
func (m Me) Is(addr string) bool {
	addr = norm(addr)
	if addr == "" {
		return false
	}
	if m.addrs[addr] {
		return true
	}
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	local, domain := addr[:at], addr[at+1:]
	if m.domains[domain] {
		return true
	}
	if plus := strings.Index(local, "+"); plus > 0 && m.addrs[local[:plus]+"@"+domain] {
		return true
	}
	return false
}

// Empty reports a Me with no addresses and no domains: direction cannot be
// decided from From alone, which a report must say rather than read as
// "the user never sends mail".
func (m Me) Empty() bool { return len(m.addrs) == 0 && len(m.domains) == 0 }

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// IsBulk reports any bulk-sending apparatus: List-Unsubscribe, RFC 8058
// one-click, a feedback-loop id, or Precedence bulk/list/junk. List-Id is
// separate (HasListID) because the operator's rule names it separately.
func IsBulk(f kind.Facts) bool {
	if strings.TrimSpace(f.ListUnsubscribe) != "" || strings.TrimSpace(f.ListUnsubscribePost) != "" ||
		strings.TrimSpace(f.FeedbackID) != "" {
		return true
	}
	switch norm(f.Precedence) {
	case "bulk", "list", "junk":
		return true
	}
	return false
}

// HasListID reports a List-Id header (RFC 2919).
func HasListID(f kind.Facts) bool { return strings.TrimSpace(f.ListID) != "" }

// IsAutomated reports RFC 3834 Auto-Submitted with any value but "no": the
// message was generated or replied by a machine (a vacation responder
// included). Such a message never needs a reply, and is never an ANSWER to
// the user's mail either.
func IsAutomated(f kind.Facts) bool {
	v := norm(f.AutoSubmitted)
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v != "" && v != "no"
}

// KindPersonal is the kind classifier deciding "personal" outright.
func KindPersonal(v kind.Verdict) bool { return v.Kind == kind.Personal }

// KindNotMachine is the weaker reading: decided personal, OR undecided with
// nothing machine-like leading. The classifier needs 1.5 points to decide
// personal and a stranger's first plain message earns 1.0 (no_bulk_markers),
// so KindPersonal alone excludes nearly every first contact by construction;
// this variant exists so the report can show what that costs.
func KindNotMachine(v kind.Verdict) bool {
	if v.Kind == kind.Personal {
		return true
	}
	return v.Kind == kind.Unknown && (v.Leader == kind.Personal || v.Leader == kind.Unknown)
}

var (
	urlRE = regexp.MustCompile(`(?i)\b(?:https?|ftp|mailto):\S+|\bwww\.\S+`)
	// requestRE is request phrasing in English. It is a CANDIDATE signal and is
	// measured, not trusted: "please" in particular appears in footers.
	requestRE = regexp.MustCompile(`(?i)\b(?:can|could|would|will) you\b|\bplease\b|\blet me know\b|` +
		`\bwhat do you think\b|\bdo you (?:have|know|want|think|mind|need)\b|` +
		`\bare you (?:free|available|able|around|ok|okay)\b|\bget back to me\b|` +
		`\bany (?:thoughts|updates?|news)\b|\bwhen (?:can|could|would|will|are|is|do)\b`)
	// attributionRE ends the new text: the line that introduces a quoted reply.
	attributionRE = regexp.MustCompile(`(?i)^(?:on .+ wrote:?|-{2,} ?original message ?-{2,}|-{2,} ?forwarded message ?-{2,}|_{5,}|from: .+@.+|begin forwarded message:?)$`)
)

// maxNewText bounds how much of a body is searched; a question is near the top.
const maxNewText = 8000

// NewText is the part of a plain-text body the sender wrote in THIS message:
// quoted lines (">") are skipped, and everything after a reply attribution,
// an "Original Message" separator, or a signature delimiter is cut. A question
// in a quote is the user's own, asked earlier, and is not evidence that this
// message asks anything.
func NewText(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimRight(line, "\r")
		if t == "-- " || t == "--" {
			break
		}
		trimmed := strings.TrimSpace(t)
		if attributionRE.MatchString(trimmed) {
			break
		}
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		b.WriteString(t)
		b.WriteByte('\n')
		if b.Len() >= maxNewText {
			break
		}
	}
	return b.String()
}

// Asks reports whether a message asks something: a question mark, or request
// phrasing, in the subject or the new text of the body. URLs are removed
// first, since a query string is not a question.
func Asks(subject, body string) (question, request bool) {
	text := urlRE.ReplaceAllString(subject+"\n"+NewText(body), " ")
	return strings.Contains(text, "?"), requestRE.MatchString(text)
}
