// SPDX-License-Identifier: AGPL-3.0-only

// Package senderwarn judges whether a message's SENDING DOMAIN is trying to
// look like one the user already knows (#397, #55, spec §11).
//
// # What this is, and what internal/ml.AuthResults already is
//
// `senderAuth` (#55) reports what SPF, DKIM and DMARC said: whether the mail
// really came from the domain it claims. It is deliberately silent for the
// ~99% that pass, because a badge on 99.4% of mail is furniture.
//
// This is a different question on a different input. Authentication passing
// says the mail really is from `paypa1.com`. It says nothing about whether
// `paypa1.com` is a domain you have ever heard of, or one character away from
// one you trust. A lookalike domain authenticates perfectly — it belongs to the
// attacker.
//
// # ⚠ THE BOUNDARY WITH #70, STATED SO IT IS SETTLED
//
// #70 (2.x) holds phishing and homograph warnings about **URLs in message
// bodies**. This package is about **the From header's domain**. Same family,
// different object, and they share exactly one thing: Skeleton, which is
// exported so #70 applies the same reduction to an `href` rather than writing a
// second one that disagrees. A rule about `From:` belongs here; a rule about a
// link belongs there. Neither issue owns "phishing".
//
// # ⚠ WHAT WAS MEASURED, AND THE TWO FEATURES IT KILLED
//
// Against a real corpus, before any of this was written:
//
//   - **"External sender" marking is unbuildable here.** The mailbox is a
//     catch-all: almost none of the messages come from the user's own domains, so
//     nearly all are external. A marker that fires on nearly all mail is worse than no
//     marker.
//   - **Display-name impersonation over the contact list has near-zero precision.**
//     "The display name matches a known correspondent but the address does not"
//     fires on a few percent of the corpus and every single hit
//     is legitimate — mostly an ESP sending on a brand's behalf (crowdfunding,
//     survey and marketing platforms). Those senders authenticate
//     correctly, because the ESP signs as itself. The rule measures delegation,
//     not deception. `docs/hostile-content.md` already said the underlying
//     thing: `From: "Bank Security" <attacker@evil.example>` is not detectable
//     by any heuristic over a string the sender chose.
//
// What survived is here: a **visual** collision with a domain the user actually
// corresponds with, and the case where a stranger claims one of the user's own
// domains.
package senderwarn

import (
	"strings"
	"unicode"

	"golang.org/x/net/idna"
)

// The three warning kinds. Strings rather than an enum because they cross the
// JMAP boundary and become an i18n key on the client.
const (
	// LookalikeDomain: the sending domain renders like one the user knows, and
	// is not that domain.
	LookalikeDomain = "lookalikeDomain"
	// PunycodeDomain: the sending domain carries an `xn--` label.
	//
	// ⚠ THIS IS NOT AN ACCUSATION AND THE WORDING MUST NOT MAKE IT ONE.
	// Internationalized domains are legitimate and ordinary outside the
	// English-speaking internet. It is reported because it is the mechanic a
	// homograph attack needs, because mail headers carry the punycode form so
	// what a reader sees is not what the domain renders as elsewhere, and
	// because it is rare enough in real mail to be worth a word
	// rather than a shrug.
	PunycodeDomain = "punycodeDomain"
	// SelfSpoof: the sender claims one of the user's OWN domains, the message
	// was not written here, and authentication did not affirmatively pass.
	//
	// ⚠ MEASURED ON A DEPLOYED INSTANCE: it fires on a handful of messages
	// (well under 1%), and ALL OF THEM ARE TEST MAIL — messages injected
	// into the mailbox by hand during development, which never crossed an
	// authenticating hop. The mechanism is doing exactly what it is for;
	// the corpus is unusual.
	//
	// ⚠ STATED RATHER THAN QUIETLY ENJOYED, because 0.26% sits next to the auth
	// notice's honest 0.76% and is NOT the same kind of number: that one is
	// mostly true positives, this one is currently zero. A rate that looks
	// respectable while every hit is wrong is precisely what got display-name
	// impersonation dropped, and the only reason this survives the same test is
	// that its false positives are an artefact of a test mailbox rather than of
	// legitimate senders.
	//
	// ⚠ AND THE PREDICTION BEFORE IT SHIPPED WAS "ZERO", from a query over
	// app.messages.auth_fail. That column means "a method FAILED and never
	// passed"; this rule means "nothing affirmatively PASSED". A message with no
	// Authentication-Results at all satisfies the second and not the first, and
	// six did. Two different questions, one of which was a convenient proxy for
	// the other and was not.
	//
	// No suppression rule has been added for it. A few hand-injected messages are
	// not evidence about real mailboxes, and tuning a security signal to them
	// would be fitting to noise. `mailroom-admin sender-report` is what makes
	// the rate visible on whatever corpus it is pointed at.
	SelfSpoof = "selfSpoof"

	// MultipleFrom is a message carrying more than one From header FIELD
	// (#754), counted raw — an empty `From:` beside a real one included.
	// RFC 5322 allows exactly one. With two, the receiving MTA's DMARC verdict
	// may be about one author while Mailroom stores, shows and keys on the
	// other: a known parser-differential spoof. It needs no measurement to be
	// precise — a legitimate sender never writes two — and production carried
	// zero such messages when #754's report ran, so it is
	// silent on real mail by measurement as well as by construction.
	MultipleFrom = "multipleFrom"
)

// Warning is one finding about a sender.
type Warning struct {
	Kind string `json:"kind"`
	// Domain is the sending domain exactly as the message presented it.
	Domain string `json:"domain"`
	// Resembles is the known domain a lookalike collides with. Empty otherwise.
	//
	// ⚠ It is what makes the warning ACTIONABLE rather than ominous. "This is
	// not walmart.com" is something a reader can check in a second; "this domain
	// is suspicious" is something they can only worry about.
	Resembles string `json:"resembles,omitempty"`
	// Unicode is the decoded form of a punycode domain, when it decodes.
	Unicode string `json:"unicode,omitempty"`
}

// Sender is the one message's worth of input this package judges. Everything in
// it is already available without an extra read: the domain comes from the
// projected `from_address`, and the auth flags from the Authentication-Results
// header the reading pane has already parsed.
type Sender struct {
	Domain string
	// LocallyAuthored suppresses SelfSpoof: Mailroom wrote this message, so of
	// course it is from the user's own domain and of course it carries no
	// authentication.
	LocallyAuthored bool
	// AuthPassed is true when DKIM or DMARC affirmatively passed. ⚠ NOT "did
	// not fail": a message with no Authentication-Results at all has not
	// passed anything, and for mail claiming the user's OWN domain that
	// absence is the whole signal.
	AuthPassed bool
	// FromFields is how many From header fields the message carries, counted
	// raw (mime.Header.FieldCount). 0 means NOT COUNTED — a caller without the
	// message bytes — and raises nothing; more than one raises MultipleFrom.
	FromFields int
}

// Index is the user's known-domain set, built once per request.
//
// ⚠ "KNOWN" MEANS "HAS WRITTEN TO ME", NOT "I HAVE WRITTEN TO THEM", AND THAT
// IS A CONCESSION. app.contact_addresses carries both counts and its own schema
// comment argues that `outbound_count` — mail the user sent — "is the signal
// that actually means correspondence". It is right. It is also 0 on every
// address of the corpus this was measured against, because that mailbox sends
// no mail: defining "known" that way would define zero known domains and make
// this whole package a mechanism that cannot fire. So the weaker definition is
// used, and named here rather than discovered later.
type Index struct {
	own        map[string]bool
	bySkeleton map[string]string
}

// NewIndex prepares the lookup. own is the user's identity domains;
// corresponded is every domain they have received mail from.
func NewIndex(own, corresponded []string) *Index {
	ix := &Index{own: map[string]bool{}, bySkeleton: map[string]string{}}
	for _, d := range own {
		d = normalizeDomain(d)
		if d == "" {
			continue
		}
		ix.own[d] = true
		ix.bySkeleton[Skeleton(d)] = d
	}
	for _, d := range corresponded {
		d = normalizeDomain(d)
		if d == "" {
			continue
		}
		// ⚠ An own domain wins the skeleton slot. If a correspondent's domain
		// and the user's own reduce alike, the more useful thing to say is
		// "this is not YOUR domain".
		sk := Skeleton(d)
		if ix.own[ix.bySkeleton[sk]] {
			continue
		}
		// ⚠ DETERMINISTIC AMONG CORRESPONDENTS. See preferKnown: the winner
		// used to be whichever the query planner emitted first.
		ix.bySkeleton[sk] = preferKnown(ix.bySkeleton[sk], d)
	}
	return ix
}

// Check returns every warning that applies to one sender, worst first.
//
// ⚠ IT RETURNS NOTHING FOR THE OVERWHELMING MAJORITY OF MAIL, by construction
// and by measurement: no real correspondent domains collide with another,
// and almost no messages carry a non-ASCII sending domain. A notice that fires
// constantly is one a reader stops seeing, which is the failure senderAuth.ts
// was designed around and this package inherits.
func (ix *Index) Check(s Sender) []Warning {
	if ix == nil {
		return nil
	}
	d := normalizeDomain(s.Domain)
	var out []Warning

	// Self-spoofing first: it is the most serious thing that can be said about
	// a sender, and it is precise by construction — nobody legitimately sends
	// as you, from outside, without authenticating.
	if d != "" && ix.own[d] && !s.LocallyAuthored && !s.AuthPassed {
		out = append(out, Warning{Kind: SelfSpoof, Domain: d})
	}
	// Then more than one From field (#754). Decided BEFORE the empty-domain
	// return below, because a message whose stored From has no domain is
	// exactly the shape two From fields can produce, and it must not fall
	// silent for that reason.
	if s.FromFields > 1 {
		out = append(out, Warning{Kind: MultipleFrom, Domain: d})
	}
	if d == "" {
		return out
	}

	// ⚠ lookalikeDomain IS SUPPRESSED, AND THE REASON IS THAT ITS FAILURE MODE
	// IS MISDIRECTION RATHER THAN SILENCE (#411).
	//
	// The rule reads "the sender's domain reduces to the same skeleton as a
	// domain the user KNOWS, and is not it". Both halves are poisoned by one
	// fact the design overlooked: **every inbound From becomes a contact on
	// arrival** (ingest calls UpsertContactsFromMessage inside its transaction),
	// and "known" means nothing more than one row in app.contact_addresses. So
	// by the time you open the first message from `paypa1.example`, that domain
	// is already known — the detector's corpus contains the thing it detects.
	//
	// The skeleton slot then goes to whichever of the colliding pair the index
	// saw first, and `known != d` warns about the other one. When the lookalike
	// wins the slot, the impersonator is waved through and **the genuine
	// correspondent is presented to the user as an imitation of it**. That is
	// not a weaker warning; it is a warning pointed at the wrong party, on a
	// mailbox about to carry real correspondence.
	//
	// ⚠ NEITHER OF THE OTHER TWO WARNINGS HAS THIS SHAPE. selfSpoof and
	// punycodeDomain fail by staying quiet. Only this one can accuse.
	//
	// What re-enabling needs is a real definition of "known", and
	// app.contact_addresses already carries the discriminators: first_seen_at,
	// seen_count, and outbound_count — the strongest, and 0 on every address of
	// the mailbox this was built against, which is why #397 could not use it.
	// ⚠ A THRESHOLD OVER THEM MUST BE MEASURED BEFORE IT IS CHOSEN, on a corpus
	// that actually sends mail: display-name impersonation was refused in #397
	// for firing at 4.9% with zero precision, and a "known" rule picked by
	// intuition would earn the same fate. The detection below is kept, tested
	// and deterministic so that work starts from a working mechanism rather than
	// a rewrite.
	if lookalikeEnabled && !ix.own[d] {
		if known, ok := ix.bySkeleton[Skeleton(d)]; ok && known != d {
			out = append(out, Warning{Kind: LookalikeDomain, Domain: d, Resembles: known})
		}
	}

	if label, ok := punycodeLabel(d); ok {
		_ = label
		w := Warning{Kind: PunycodeDomain, Domain: d}
		if u, err := idna.ToUnicode(d); err == nil && u != d {
			w.Unicode = u
		}
		out = append(out, w)
	}
	return out
}

// normalizeDomain lowercases and strips a trailing root dot. It does NOT decode
// punycode: the stored address is what the message said and what a reader is
// shown, and silently substituting a prettier form would mean the warning and
// the address disagree about what the domain is.
func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimSuffix(d, ".")
	return d
}

func punycodeLabel(d string) (string, bool) {
	for _, label := range strings.Split(d, ".") {
		if strings.HasPrefix(label, "xn--") {
			return label, true
		}
	}
	return "", false
}

// Skeleton reduces a domain to WHAT IT LOOKS LIKE, so two domains that render
// alike compare equal. Exported because #70 needs exactly this over URLs.
//
// ⚠ THE MECHANIC IS VISUAL EQUIVALENCE, NOT EDIT DISTANCE, and the difference
// is the whole reason this is shippable. Edit distance ≤ 1 flags `examples.com`
// as a lookalike of `example.com` — two unrelated companies — and would have
// fired on legitimate mail. A skeleton collision means the two strings are hard
// to tell apart when READ, which is the actual attack.
//
// Measured on a real corpus before shipping: all distinct
// correspondent domains reduce to distinct skeletons — ZERO false
// positives — while crafted lookalikes of domains that really are in that
// corpus collide as intended (walrnart.com/walmart.com, arnazon.com/amazon.com,
// rno.gov/mo.gov, paypa1.com/paypal.com, 1inkedin.com/linkedin.com).
//
// ⚠ WHAT IT DOES NOT CATCH, named rather than papered over:
//
//   - **A domain that CONTAINS a brand.** `anthropic-com.net`,
//     `paypal-security.example`: not confusables, just different domains that
//     read convincingly. Catching those needs a brand list, which is a
//     different feature with a much worse false-positive profile.
//   - **A brand the user has never corresponded with.** The index is built from
//     the user's own mail, so a first-ever `paypa1.com` with no genuine
//     `paypal.com` in the mailbox collides with nothing. This warns about
//     imitations of relationships the user HAS.
//   - **Confusables beyond the table below.** The Unicode half covers the
//     Cyrillic and Greek letters that render as Latin ones — the classic
//     homograph set — and not the full UTS-39 table, which is thousands of
//     entries. `MixedScript` is the backstop for what the table misses.
func Skeleton(domain string) string {
	s := strings.ToLower(strings.TrimSpace(domain))

	// Map the Unicode confusables to their Latin twins first, so `раypal.com`
	// (Cyrillic а and р) and `paypal.com` reduce alike.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if latin, ok := confusables[r]; ok {
			b.WriteRune(latin)
			continue
		}
		b.WriteRune(r)
	}
	s = b.String()

	// Then the ASCII pairs a reader misses in a proportional font. `rn`→`m`
	// runs BEFORE the digit substitutions so `rn` is not broken up by them.
	s = strings.ReplaceAll(s, "rn", "m")
	s = strings.ReplaceAll(s, "vv", "w")
	s = strings.ReplaceAll(s, "0", "o")
	// 1, l and I all render as a bare vertical stroke in most UI faces, so they
	// collapse to one character rather than to each other.
	s = strings.ReplaceAll(s, "1", "i")
	s = strings.ReplaceAll(s, "l", "i")

	// ⚠ SEPARATORS ARE DROPPED, which is the most aggressive step here and the
	// one most likely to cause a false positive — `my-bank.com` and
	// `mybank.com` become the same. It is kept because moving or adding a
	// hyphen is a standard lookalike, and it was measured: zero collisions
	// among real domains even with it. If it ever does misfire, this is the
	// line to reconsider first.
	s = strings.NewReplacer("-", "", ".", "").Replace(s)
	return s
}

// MixedScript reports a domain label that mixes scripts — a Latin word with one
// Cyrillic letter in it, say.
//
// ⚠ IT IS THE BACKSTOP FOR WHAT THE CONFUSABLE TABLE MISSES. A curated table
// can only know the pairs somebody wrote down; mixing scripts inside a single
// label is a structural property, and outside a handful of genuinely
// multi-script languages it does not happen by accident. Reported separately
// from Skeleton because it needs no known domain to compare against — it is
// suspicious on its own terms.
func MixedScript(domain string) bool {
	for _, label := range strings.Split(strings.ToLower(domain), ".") {
		var latin, cyrillic, greek bool
		for _, r := range label {
			switch {
			case unicode.Is(unicode.Latin, r):
				latin = true
			case unicode.Is(unicode.Cyrillic, r):
				cyrillic = true
			case unicode.Is(unicode.Greek, r):
				greek = true
			}
		}
		n := 0
		for _, present := range []bool{latin, cyrillic, greek} {
			if present {
				n++
			}
		}
		if n > 1 {
			return true
		}
	}
	return false
}

// confusables maps the non-Latin characters that render as Latin letters.
//
// ⚠ A CURATED SUBSET OF UTS-39, NOT THE WHOLE TABLE, and the bound belongs
// here rather than in a commit message. It covers Cyrillic and Greek — the
// scripts that supply the classic `раypal` homograph — and nothing else. The
// full confusables table is several thousand entries and would be a generated
// file with its own update problem; MixedScript above catches the structural
// case this misses.
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', 'в': 'b', 'с': 'c', 'е': 'e', 'һ': 'h', 'і': 'i', 'ј': 'j',
	'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o', 'р': 'p', 'ԛ': 'q', 'ѕ': 's',
	'т': 't', 'у': 'y', 'х': 'x', 'ц': 'u', 'ѵ': 'v',
	// Greek
	'α': 'a', 'β': 'b', 'ε': 'e', 'ζ': 'z', 'η': 'n', 'ι': 'i', 'κ': 'k',
	'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x',
	// Latin lookalikes from other blocks
	'ı': 'i', 'ɩ': 'i', 'ϲ': 'c', 'ѐ': 'e',
}

// lookalikeEnabled gates the LookalikeDomain warning (#411).
//
// ⚠ A CONSTANT, NOT A CONFIG FLAG, DELIBERATELY. An operator toggle would be a
// promise that the feature works when switched on, and it does not yet: the
// "known domain" set it compares against is polluted by the very domain it is
// meant to catch, so turning it on can point the warning at the legitimate
// sender. That is a defect to fix, not a preference to expose. It flips back to
// true in the change that gives "known" a definition history can support.
const lookalikeEnabled = false

// preferKnown decides which of two colliding domains owns a skeleton slot.
//
// ⚠ IT EXISTS BECAUSE THE ANSWER USED TO BE THE QUERY PLANNER'S (#411).
// store.SenderDomainsFor is a UNION with no ORDER BY, so when two corresponded
// domains reduce to the same skeleton, the winner was whichever the planner's
// dedup happened to emit first — sort-based or hash-based, and free to differ
// between requests. A security warning whose subject depends on a plan is not a
// warning, and under sort-based dedup the digit lookalikes ('0' before 'o',
// '1' before 'l') are exactly the ones that would win.
//
// Lexicographic order is not a judgement about which domain is more legitimate —
// nothing here can make that judgement, which is why the warning is suppressed.
// It is only a guarantee that the same corpus yields the same answer twice.
func preferKnown(existing, candidate string) string {
	if existing == "" || candidate < existing {
		return candidate
	}
	return existing
}
