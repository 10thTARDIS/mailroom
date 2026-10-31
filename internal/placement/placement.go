// SPDX-License-Identifier: AGPL-3.0-only

// Package placement is LEARNED PLACEMENT's decision (#714, ADR-032): given
// the user's own filings for a sender, which area does a new message from
// that sender belong in, or should Mailroom abstain?
//
// It is pure: no database, no clock of its own. internal/store gathers the
// evidence (a query over app.message_labels and app.learned_placements) and
// calls Decide at ingest; mailroom-admin placement-report gathers the same
// evidence for every key and calls the same Decide. One calculation, so the
// report can never describe a decision the ingest path would not make
// (ADR-023 §2, "the diagnostic and the decision are one calculation").
//
// THE MODEL IS A MEMORY, NOT A FIT. A key (the From address, or its domain as a
// fallback) is CONFIDENT for area X when every one of the user's own filings
// on that key went into X, there are at least MinFilings of them, and no
// correction of a learned placement on that key is newer than the newest
// filing. Anything else abstains and leaves the desk's tab placement alone.
// Because the evidence is read fresh at every decision, every filing the user
// makes changes the next decision: that is the "continually trained" half of
// the operator's request, with no training job to fall behind.
package placement

import (
	"math"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/stats"
)

// MinFilings is how many of the user's own filings a key needs, all into one
// area, before learned placement acts on it.
//
// WHY 2. Leave-one-out over a real set of area filings (#714,
// 2026-10-16): predicting each filing from the OTHER filings on the same From
// address, acting only with >= 2 others that all agree, was nearly all right and none
// wrong, with some abstentions and no mixed keys. One filing is a single act
// that may be a slip; two agreeing filings are the smallest evidence that is
// a pattern. A higher floor costs reach (12 of 43 senders had >= 2) for no
// measured precision gain.
//
// ⚠ THE 887/0 IS OPTIMISTIC: most of those filings came from bulk sessions,
// where a sender's batch was filed at once and so agrees with itself by
// construction. It justifies starting at 2; it is not the safeguard. The
// safeguard is the live correction gate below (GateMinResolved, GateMinLCB).
const MinFilings = 2

// MinDomainAddresses is how many DISTINCT From addresses a domain's filings
// must span before the domain may act as a fallback for an address with no
// evidence of its own.
//
// A domain whose filings all come from one address is that address's
// evidence relabelled: a bank's alerts@ filed into Updates says nothing yet
// about its offers@. Two agreeing addresses are the smallest evidence that
// the domain, not one stream, is what the user is sorting. This is stricter
// than #714's brief (which asked only that the domain's filings agree); it
// narrows the fallback and never widens it.
const MinDomainAddresses = 2

// KeptAfter is how long a learned placement stays OPEN. One that has not been
// corrected (moved out of its area by the user) by then is KEPT.
//
// Seven days: long enough to cover a weekly reader who only looks at a quiet
// area at the weekend, short enough that the gate below sees outcomes within
// the first week of acting. A user who archives the message from the area it
// was placed in has acted on it there, and that resolves it as kept at once
// (store: KeptByArchive). Reading it does NOT: opening a message to see what
// it is and then re-filing it is the correction path itself.
const KeptAfter = 7 * 24 * time.Hour

// GateMinResolved is how many resolved (kept or corrected) placements an area
// needs before its correction gate can judge it. Below this the gate does not
// stop anything: twenty outcomes is too few for any interval to say much, and
// a gate that judged on three would switch the feature off on its first slip.
const GateMinResolved = 20

// GateMinLCB is the precision floor: with at least GateMinResolved resolved
// placements, an area whose 95% Wilson lower bound on kept/(kept+corrected)
// falls below this stops acting.
//
// What the pair means in practice (stats.WilsonLowerBound): 20 of 20 kept is a
// bound of 0.839, and 19 of 20 is 0.764, so at exactly 20 resolved the FIRST
// correction stops the area. At 40 resolved, three corrections are tolerated
// (37/40 is 0.801) and four are not; at larger counts the tolerance tends to
// roughly 15%. That is strict by design: there is no shadow period,
// so the gate is what stands between a wrong memory and mail silently leaving
// Primary, and CLAUDE.md asks the gate to sit on the bottom of the interval,
// not the point estimate.
const GateMinLCB = 0.80

// KeyKind names what a key is built from.
type KeyKind string

const (
	// KeyAddress is the lowercased From address: the measured best key (#714:
	// the domain ties on the operator's history but is coarser, and several
	// high-volume domains split across two areas by stream).
	KeyAddress KeyKind = "address"
	// KeyDomain is the From address's domain: a fallback only, see Decide.
	KeyDomain KeyKind = "domain"
)

// Key is one sender identity learned placement remembers.
type Key struct {
	Kind  KeyKind
	Value string
}

// KeysFor derives the address and domain keys of a From address. ok is false
// when the address has no usable domain, and then nothing is learned or
// placed: a message with no sender identity has nothing to remember.
func KeysFor(from string) (address, domain Key, ok bool) {
	a := strings.ToLower(strings.TrimSpace(from))
	at := strings.LastIndexByte(a, '@')
	if at <= 0 || at == len(a)-1 {
		return Key{}, Key{}, false
	}
	return Key{Kind: KeyAddress, Value: a}, Key{Kind: KeyDomain, Value: a[at+1:]}, true
}

// Evidence is everything known about one key at decision time.
type Evidence struct {
	// Filings counts the user's own filings on this key, per area: rows of
	// app.message_labels for an area's OWN label with applied_by = 'user'.
	// Never desk, setup, rule, ml or learned rows (#169: a model trained on its
	// own output confirms its own mistakes).
	Filings map[ulid.ULID]int
	// Addresses is how many distinct From addresses the filings span. Only
	// read for a domain key (MinDomainAddresses).
	Addresses int
	// LatestFiling is the newest filing's time. Zero when there are none.
	LatestFiling time.Time
	// LatestCorrection is when a learned placement on this key was last
	// corrected, or zero if never.
	LatestCorrection time.Time
}

// Total is the number of filings across every area.
func (e Evidence) Total() int {
	n := 0
	for _, c := range e.Filings {
		n += c
	}
	return n
}

// Reason is why a key is or is not confident.
type Reason string

const (
	// Confident: act.
	Confident Reason = "confident"
	// NoEvidence: the user has never filed mail on this key.
	NoEvidence Reason = "none"
	// Thin: every filing agrees, but there are fewer than MinFilings (or, for
	// a domain, they span fewer than MinDomainAddresses addresses).
	Thin Reason = "thin"
	// Mixed: the user has filed this key into more than one area.
	Mixed Reason = "mixed"
	// Contradicted: a learned placement on this key was corrected, and the
	// user has not filed on the key since. It outranks Mixed and Thin when
	// reporting, because it is the reinforcement signal itself.
	Contradicted Reason = "contradicted"
	// NoKey: the message has no usable From address.
	NoKey Reason = "nokey"
	// DomainBlocked: the address has no evidence, its domain has some, and
	// the domain is not a sender (a freemail provider or the user's own), so
	// the domain fallback does not apply. See DomainFallbackBlocked.
	DomainBlocked Reason = "blocked"
	// Unauthenticated: a key is confident, but the message's sender is not
	// authenticated (SenderAuth.Authenticated), so it gets the desk's normal
	// placement. From is forgeable; a placement says "learned from your
	// moves", and that must not be lent to a forgery of a known sender.
	Unauthenticated Reason = "unauthenticated"
	// SenderWarned: a key is confident, but the sender spoofing / look-alike
	// warnings (internal/senderwarn) would flag the message.
	SenderWarned Reason = "warned"
	// AmbiguousFrom: the message's From is not exactly one mailbox in exactly
	// one From header (NewSender), so there is no single identity to
	// authenticate and act on.
	AmbiguousFrom Reason = "ambiguous"
)

// SenderAuth is what the ingest path knows about a new message's sender
// authentication, computed from its Authentication-Results headers with the
// SAME trusted parse the sender warnings use (ml.TrustedAuthResults and the
// configured MAILROOM_AUTHSERV_IDS).
type SenderAuth struct {
	// TrustConfigured: the operator named at least one trusted authserv-id.
	TrustConfigured bool
	// DMARCPass: the topmost TRUSTED header carries a DMARC pass whose own
	// header.from is the SENDER'S domain, or exactly that domain's
	// organisational domain (relaxed alignment, #846), and no DMARC fail
	// (ml.AuthResults.DMARCMatchFor with ml.DMARCRelaxed on
	// ml.TopmostTrustedAuthResults). Bound to the domain NewSender hands its
	// authFor, never to whatever domain the header happens to name.
	DMARCPass bool
	// DMARCMatch is how that pass matched, for placement-explain only:
	// "exact", "org-domain", or "" for no pass (ml.DMARCMatch). DMARCPass is
	// what Authenticated reads; this field decides nothing.
	DMARCMatch string
	// AffirmativePass: the same topmost trusted header says DKIM or DMARC
	// passed: the selfSpoof warning's input (ml.AuthResults.AffirmativelyPassed).
	AffirmativePass bool
	// AuthFail: authentication CLEARLY failed across all headers
	// (ml.AuthResultsFail, the conservative rule; stored as messages.auth_fail).
	AuthFail bool
}

// FromHeaders is the raw material NewSender checks the From address against.
type FromHeaders struct {
	// Fields is how many From header FIELDS the message carries, counted raw
	// (mime.Header.FieldCount) — empty and unparsable ones included (#754).
	// Raw cannot answer that: Values("From") drops a field that is empty once
	// sanitised, so an empty `From:` beside a real one counted as ONE, while
	// which of the two a receiving MTA's DMARC verdict was about is unknowable.
	Fields int
	// Raw is every From header field value, in message order
	// (mime.Header.Values("From")).
	Raw []string
	// Parsed is every mailbox Mailroom parsed out of the From header
	// (mime.Header.From()), as addresses.
	Parsed []string
}

// Sender is THE identity learned placement authenticates and acts on (#714
// security review). It is built once, at ingest, from one value: the From
// address Mailroom stores on the message and keys on. The keys, the domain
// fallback, the domain DMARC must pass for, the sender-warning check and
// the placement log's key_value are all read from it, so the identity that
// was authenticated cannot be a different identity from the one acted on.
// Its fields are unexported so no caller can assemble one from separate
// parses.
type Sender struct {
	address, domain Key
	ok              bool
	ambiguous       bool
	auth            SenderAuth
}

// NewSender builds the sender from `from` (the stored From address), the
// From headers it came from, and authFor, which evaluates authentication FOR
// A DOMAIN. NewSender calls authFor with the domain of `from` itself, the
// same domain the domain key carries: the binding is made here, not by the
// caller.
//
// AMBIGUOUS, AND THEREFORE NOT ACTED ON, when any of these holds: other than
// exactly one From FIELD (hdr.Fields, counted raw, #754); more than one
// non-empty From header; a From header holding a group (RFC 5322 `name: list;`);
// more than one mailbox parsed from it; or the parsed mailbox differing from
// `from`. Each is a way for the receiving MTA's idea of "the From domain"
// (what its DMARC verdict is about) and Mailroom's to differ.
func NewSender(from string, hdr FromHeaders, authFor func(domain string) SenderAuth) Sender {
	a, d, ok := KeysFor(from)
	s := Sender{address: a, domain: d, ok: ok}
	switch {
	case hdr.Fields != 1, len(hdr.Raw) != 1, hasGroup(hdr.Raw[0]), len(hdr.Parsed) != 1:
		s.ambiguous = true
	default:
		pa, _, pok := KeysFor(hdr.Parsed[0])
		s.ambiguous = !pok || pa != a
	}
	if ok && authFor != nil {
		s.auth = authFor(d.Value)
	}
	return s
}

// Keys returns the sender's address and domain keys; ok is false when the
// address has no usable domain.
func (s Sender) Keys() (address, domain Key, ok bool) { return s.address, s.domain, s.ok }

// Ambiguous reports a From that is not one mailbox in one header.
func (s Sender) Ambiguous() bool { return s.ambiguous }

// Auth is the sender's authentication, evaluated for its own domain.
func (s Sender) Auth() SenderAuth { return s.auth }

// HasGroup is hasGroup, exported so `mailroom-admin from-field-report` (#754)
// classifies a From header with the SAME rule NewSender acts on, rather than a
// second copy that could disagree with it.
func HasGroup(raw string) bool { return hasGroup(raw) }

// hasGroup reports RFC 5322 group syntax: a ':' outside quoted strings,
// comments and angle brackets.
func hasGroup(raw string) bool {
	inQuote, escaped, depth, angle := false, false, 0, 0
	for _, r := range raw {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case inQuote:
			if r == '"' {
				inQuote = false
			}
		case r == '"' && depth == 0:
			inQuote = true
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth > 0:
		case r == '<':
			angle++
		case r == '>' && angle > 0:
			angle--
		case r == ':' && angle == 0:
			return true
		}
	}
	return false
}

// Authenticated is the condition learned placement acts under (#714).
//
//   - With trusted authserv-ids configured: a trusted DMARC pass, the aligned
//     verdict on the visible From domain. No trusted header, a trusted fail,
//     or a pass claimed by an UNtrusted id (dropped by the trusted parse) all
//     abstain.
//   - With none configured nothing is believed as a pass, so the floor is the
//     conservative "did not clearly fail" (AuthFail false). Weaker by
//     necessity; the server logs at startup that it is running this way.
func (s SenderAuth) Authenticated() bool {
	if s.TrustConfigured {
		return s.DMARCPass
	}
	return !s.AuthFail
}

// freemailDomains are consumer mailbox providers: a domain shared by
// unrelated people. Two friends at gmail.com filed into Updates say nothing
// about a third person writing from gmail.com, and the correction gate would
// only notice after that person's mail had been filed away. So the DOMAIN
// fallback never applies to these; learning on each ADDRESS is unaffected.
//
// Fixed and explicit on purpose (#714 review): short enough to read, and a
// provider missing from it costs only what the gate already bounds. Not a
// public-suffix or "is this a mailbox provider" heuristic, which would be a
// second classifier to get wrong.
var freemailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true,
	"outlook.com": true, "hotmail.com": true, "live.com": true, "msn.com": true,
	"hotmail.co.uk": true, "outlook.co.uk": true, "live.co.uk": true,
	"yahoo.com": true, "ymail.com": true, "yahoo.co.uk": true, "rocketmail.com": true,
	"icloud.com": true, "me.com": true, "mac.com": true,
	"aol.com":   true,
	"proton.me": true, "protonmail.com": true, "pm.me": true,
	"gmx.com": true, "gmx.net": true, "gmx.de": true, "web.de": true,
	"mail.com": true, "zoho.com": true,
	"yandex.com": true, "yandex.ru": true, "mail.ru": true,
	"fastmail.com": true, "hey.com": true, "tutanota.com": true, "tuta.io": true,
	"qq.com": true, "163.com": true,
}

// IsFreemail reports whether domain (lowercase) is a consumer mailbox
// provider on the fixed list above.
func IsFreemail(domain string) bool { return freemailDomains[domain] }

// DomainFallbackBlocked reports whether the domain fallback must not apply to
// domain: a freemail provider, or one of the user's OWN domains (the domains
// of their enabled identities). The user's own domain is the sharpest case:
// on a real mailbox it is the single biggest sender, and its mail (colleagues,
// family, the user's own services) is split across areas by design. own is
// the set of the user's domains, lowercased; nil means none.
func DomainFallbackBlocked(domain string, own map[string]bool) bool {
	return IsFreemail(domain) || own[domain]
}

// Verdict is one key's judgement.
type Verdict struct {
	Reason Reason
	// Area is the area every filing went into. Set for Confident and Thin,
	// and for Contradicted when the evidence is otherwise consistent; nil
	// for Mixed and NoEvidence.
	Area *ulid.ULID
}

// Judge decides one key on its own evidence.
//
// THE CONTRADICTION RULE. A correction blocks the key until the user files on
// it AGAIN, strictly after the correction: the filing that IS the correction
// (a re-file swaps the learned label for the user's own, in one transaction
// with the same timestamp) does not lift it. In the usual case that filing
// also makes the evidence mixed, which abstains on its own; the rule matters
// when the user removes a learned label without filing the message anywhere,
// which leaves the old evidence consistent and would otherwise act again on
// the next message.
//
// NO DECAY in increment 1. Evidence is the CURRENT state of the user's
// labels, not a history, so it already changes when the user re-files old
// mail: a re-file swaps the user's old filing for a new one. A key that went
// mixed comes back once the user has re-filed the stragglers (those filings
// are newer than the correction, so the contradiction lifts too). A time
// decay would also forget agreeing evidence the user never revisited, which
// is most of it; whether that is worth it is an increment-2 measurement.
func Judge(kind KeyKind, e Evidence) Verdict {
	total := e.Total()
	if total == 0 {
		return Verdict{Reason: NoEvidence}
	}
	var area *ulid.ULID
	if len(e.Filings) == 1 {
		for a := range e.Filings {
			id := a
			area = &id
		}
	}
	if !e.LatestCorrection.IsZero() && !e.LatestFiling.After(e.LatestCorrection) {
		return Verdict{Reason: Contradicted, Area: area}
	}
	if area == nil {
		return Verdict{Reason: Mixed}
	}
	if total < MinFilings || (kind == KeyDomain && e.Addresses < MinDomainAddresses) {
		return Verdict{Reason: Thin, Area: area}
	}
	return Verdict{Reason: Confident, Area: area}
}

// Decision is what learned placement does with one new message.
type Decision struct {
	// Act is true when the message should be filed into Area under Key.
	Act  bool
	Key  Key
	Area ulid.ULID
	// Reason is the verdict of the key that decided: Confident when acting,
	// otherwise why not (the address's reason when it has evidence, else the
	// domain's).
	Reason Reason
}

// Decide applies the key precedence: the ADDRESS decides whenever it has any
// evidence of its own; the DOMAIN is consulted only when the address has
// none. So the domain fallback can never override, or even break a tie
// with, an address the user has filed: an address with one filing into
// Promotions in a domain confidently filed into Updates abstains (Thin),
// rather than following the domain.
//
// A domain for which DomainFallbackBlocked is true (freemail, or the user's
// own) is never consulted: the decision is DomainBlocked when the domain had
// evidence that would otherwise have been read, else NoEvidence.
func Decide(address, domain Key, addrEv, domainEv Evidence, own map[string]bool) Decision {
	if v := Judge(KeyAddress, addrEv); v.Reason != NoEvidence {
		if v.Reason == Confident {
			return Decision{Act: true, Key: address, Area: *v.Area, Reason: Confident}
		}
		return Decision{Key: address, Reason: v.Reason}
	}
	if DomainFallbackBlocked(domain.Value, own) {
		if domainEv.Total() > 0 {
			return Decision{Key: domain, Reason: DomainBlocked}
		}
		return Decision{Key: domain, Reason: NoEvidence}
	}
	v := Judge(KeyDomain, domainEv)
	if v.Reason == Confident {
		return Decision{Act: true, Key: domain, Area: *v.Area, Reason: Confident}
	}
	return Decision{Key: domain, Reason: v.Reason}
}

// GateCounts are an area's resolved placements since its last gate reset.
type GateCounts struct {
	Kept, Corrected int
}

// GateStatus is an area's correction gate.
type GateStatus struct {
	Resolved int
	// Precision is kept/resolved, NaN with nothing resolved.
	Precision float64
	// LCB is the 95% Wilson lower bound on Precision, NaN with nothing
	// resolved.
	LCB float64
	// Judged is false while Resolved < GateMinResolved: too few outcomes to
	// say anything, so the gate does not stop the area.
	Judged bool
	// Stopped is true when the area must not act: judged, and LCB below
	// GateMinLCB.
	Stopped bool
}

// Gate evaluates an area's correction gate.
//
// STATELESS, OVER THE DATA SINCE THE LAST RESET. Nothing latches: the gate is
// recomputed from the placement log at every decision. So it clears on
// newer data whenever placements made before it tripped later resolve as
// kept and lift the bound, and an operator can re-enable an area by
// recording a reset (mailroom-admin placement-report --reset-gate), which
// starts the count afresh from that moment without rewriting any outcome.
func Gate(c GateCounts) GateStatus {
	n := c.Kept + c.Corrected
	s := GateStatus{Resolved: n, Judged: n >= GateMinResolved}
	if n == 0 {
		s.Precision = nan()
		s.LCB = nan()
		return s
	}
	s.Precision = float64(c.Kept) / float64(n)
	s.LCB = stats.WilsonLowerBound(c.Kept, n, stats.Z95)
	s.Stopped = s.Judged && s.LCB < GateMinLCB
	return s
}

func nan() float64 { return math.NaN() }
