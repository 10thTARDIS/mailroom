// SPDX-License-Identifier: AGPL-3.0-only

package senderwarn

import "testing"

// ⚠ EVERY DOMAIN HERE IS SYNTHETIC, under the reserved `.example` TLD. The
// measurement that justified this package ran over a real set of
// correspondent domains, and that list is not something to commit: it says
// where people bank, shop and live. The fixtures are invented.
var known = []string{
	"mailbox.example", "paypal.example", "linked.example",
	"bank.example", "my-bank.example", "shop.example",
}

func newTestIndex() *Index { return NewIndex([]string{"me.example"}, known) }

// The precision claim, stated as a test: NONE of the known domains is a
// lookalike of another. On a real corpus this held for every domain; here it holds
// for a set deliberately seeded with near-misses (`bank`/`my-bank`,
// `linked`/`linkedin`) that a distance-based rule would have flagged.
func TestKnownDomainsDoNotWarnAboutEachOther(t *testing.T) {
	ix := newTestIndex()
	for _, d := range append([]string{"me.example"}, known...) {
		if w := ix.Check(Sender{Domain: d, AuthPassed: true}); len(w) != 0 {
			t.Errorf("%s produced %+v — a domain the user already corresponds with is not a "+
				"lookalike of anything, and a notice that fires on ordinary mail is one nobody reads", d, w)
		}
	}
	// The near-miss that edit distance gets wrong and a skeleton gets right.
	if w := ix.Check(Sender{Domain: "shops.example", AuthPassed: true}); len(w) != 0 {
		t.Errorf("shops.example was called a lookalike of shop.example: %+v — they read differently, "+
			"and flagging every one-character neighbour is how this becomes noise", w)
	}
}

// THE NEGATIVE CONTROL for the test above: crafted lookalikes of the SAME
// fixture domains must collide. Without this, "nothing fired" is equally
// consistent with a Skeleton that returns its input unchanged.
//
// ⚠ IT ASSERTS THE SKELETON, NOT THE WARNING, SINCE #411 SUPPRESSED THE WARNING.
// Check no longer emits LookalikeDomain (see lookalikeEnabled), so asserting on
// its output would now assert the suppression and quietly stop covering the
// reduction underneath — which is the part that must keep working for the day
// "known" gets a definition history can support. The collision is the mechanism;
// the warning is a policy on top of it, and they are pinned separately.
func TestCraftedLookalikesAreCaught(t *testing.T) {
	for _, tc := range []struct{ from, resembles, why string }{
		{"rnailbox.example", "mailbox.example", "rn reads as m"},
		{"paypa1.example", "paypal.example", "the digit one reads as an l"},
		{"paypaI.example", "paypal.example", "a capital I reads as an l"},
		{"linked.example", "linked.example", "identical — see below"},
		{"mybank.example", "my-bank.example", "a dropped hyphen"},
		{"раypal.example", "paypal.example", "Cyrillic р and а — the classic homograph"},
		{"shoρ.example", "shop.example", "Greek rho"},
	} {
		if tc.from == tc.resembles {
			continue
		}
		if Skeleton(tc.from) != Skeleton(tc.resembles) {
			t.Errorf("%s does not reduce to the same skeleton as %s (%s): %q vs %q — the confusable "+
				"reduction is the mechanism the whole package rests on, and a Skeleton that "+
				"returned its input unchanged would make every other test here vacuous",
				tc.from, tc.resembles, tc.why, Skeleton(tc.from), Skeleton(tc.resembles))
		}
	}
}

// ⚠ THE GAP, PINNED AS A TEST RATHER THAN LEFT AS A COMMENT. A domain that
// CONTAINS a brand is not a confusable, and this package does not claim to
// catch it. Asserting the current behaviour means a future change that starts
// catching it is a deliberate decision with a red test, not a silent widening
// of the false-positive surface.
func TestADomainThatMerelyContainsABrandIsNotCaught(t *testing.T) {
	ix := newTestIndex()
	for _, d := range []string{"paypal-security.example", "mailbox-support.example", "bank.example.net"} {
		for _, w := range ix.Check(Sender{Domain: d, AuthPassed: true}) {
			if w.Kind == LookalikeDomain {
				t.Errorf("%s was reported as a lookalike of %q — this package matches on VISUAL "+
					"equivalence, and if that changed it needs a brand list and a new "+
					"false-positive measurement", d, w.Resembles)
			}
		}
	}
}

// Self-spoofing: the user's own domain, from outside, without passing auth.
func TestSelfSpoof(t *testing.T) {
	ix := newTestIndex()

	if w := kinds(ix.Check(Sender{Domain: "me.example"})); !has(w, SelfSpoof) {
		t.Errorf("mail claiming the user's own domain, unauthenticated and not locally authored, "+
			"produced %v — nobody legitimately sends as you from outside", w)
	}
	// THE TWO CONTROLS, and both are load-bearing.
	//
	// A message Mailroom itself wrote is from the user's own domain and carries
	// no Authentication-Results, because authentication never applied. Warning
	// about it would mean telling someone their own outbox is being spoofed.
	if w := kinds(ix.Check(Sender{Domain: "me.example", LocallyAuthored: true})); has(w, SelfSpoof) {
		t.Errorf("a locally authored message was called a spoof: %v", w)
	}
	// And mail that really is from the user's domain and proves it.
	if w := kinds(ix.Check(Sender{Domain: "me.example", AuthPassed: true})); has(w, SelfSpoof) {
		t.Errorf("authenticated mail from the user's own domain was called a spoof: %v", w)
	}
	// ⚠ AuthPassed is "affirmatively passed", NOT "did not fail". A stranger
	// sending as you with no Authentication-Results header at all is the exact
	// case this exists for, and it is covered by the first assertion above.
}

// A punycode label is reported and decoded, and an ordinary domain is not.
func TestPunycodeIsReportedAndDecoded(t *testing.T) {
	ix := newTestIndex()
	got := ix.Check(Sender{Domain: "xn--80ak6aa92e.example", AuthPassed: true})
	var pc *Warning
	for i := range got {
		if got[i].Kind == PunycodeDomain {
			pc = &got[i]
		}
	}
	if pc == nil {
		t.Fatalf("an xn-- domain produced %+v, want a punycodeDomain warning", got)
	}
	if pc.Unicode == "" || pc.Unicode == pc.Domain {
		t.Errorf("the punycode warning carries no decoded form (%q) — the decoded name is the "+
			"whole point: the header shows the xn-- form and other software will not", pc.Unicode)
	}
	// THE CONTROL: an ordinary ASCII domain must not get one, or every message
	// in the mailbox carries this warning.
	if w := kinds(ix.Check(Sender{Domain: "shop.example", AuthPassed: true})); has(w, PunycodeDomain) {
		t.Errorf("an ordinary ASCII domain was reported as punycode: %v", w)
	}
}

// MixedScript is the backstop for confusables the curated table does not list.
func TestMixedScriptCatchesWhatTheTableMisses(t *testing.T) {
	// A Latin label with one Cyrillic 'ѡ' — a character deliberately NOT in the
	// confusables map, so Skeleton alone cannot see it.
	const sneaky = "shѡp.example"
	if Skeleton(sneaky) == Skeleton("shop.example") {
		t.Fatalf("FIXTURE FAILED: %q is in the confusables table after all, so this test is not "+
			"exercising the backstop it claims to", sneaky)
	}
	if !MixedScript(sneaky) {
		t.Errorf("MixedScript(%q) = false — a Latin label with one Cyrillic letter is the shape "+
			"a curated table will always be one entry behind on", sneaky)
	}
	// THE CONTROL: ordinary domains are single-script.
	for _, d := range known {
		if MixedScript(d) {
			t.Errorf("MixedScript(%q) = true for an ordinary domain — this would fire on everything", d)
		}
	}
}

// A nil Index answers nothing rather than panicking: a user with no contacts
// and no identities is a real state (a brand-new account), and the reading pane
// must render for them.
func TestNilIndexIsSafe(t *testing.T) {
	var ix *Index
	if w := ix.Check(Sender{Domain: "anything.example"}); len(w) != 0 {
		t.Errorf("a nil index produced %+v", w)
	}
	if w := NewIndex(nil, nil).Check(Sender{Domain: "anything.example"}); len(w) != 0 {
		t.Errorf("an empty index produced %+v", w)
	}
}

func kinds(ws []Warning) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Kind)
	}
	return out
}

func has(ks []string, k string) bool {
	for _, got := range ks {
		if got == k {
			return true
		}
	}
	return false
}

// ── #411: the arrangement that matters in production, which was untested ────
//
// ⚠ THE EXISTING FIXTURE'S `known` SET CONTAINS NO COLLIDING PAIR, so every test
// above either checks that known domains do not warn about each other (they do
// not collide) or feeds a crafted lookalike that is ABSENT from `known`. The one
// arrangement production actually produces — BOTH domains known, because ingest
// upserts every inbound From into contacts on arrival — never appeared.
//
// That is the same gap as the corpus measurement it came from: every real domain
// produced a distinct skeleton, so a collision within the known set never
// occurred and an absence was generalised into a property.
func TestAKnownDomainIsNeverAccusedOfImitatingAnotherKnownDomain(t *testing.T) {
	// ⚠ BOTH ORDERS, EXPLICITLY. store.SenderDomainsFor is a UNION with no
	// ORDER BY, so which of a colliding pair reaches the index first was the
	// planner's choice. Relying on one order — or on map iteration — would make
	// this test a coin flip about the same defect it is meant to exclude.
	for _, order := range [][]string{
		{"mailbox.example", "rnailbox.example"},
		{"rnailbox.example", "mailbox.example"},
	} {
		ix := NewIndex(nil, order)

		// CONTROL: the fixture really does collide. Without this, "no warning"
		// below is also what a Skeleton that never collides would produce.
		if Skeleton(order[0]) != Skeleton(order[1]) {
			t.Fatalf("CONTROL FAILED: %v do not collide, so this test excludes nothing", order)
		}

		for _, d := range order {
			for _, w := range ix.Check(Sender{Domain: d, AuthPassed: true}) {
				if w.Kind != LookalikeDomain {
					continue
				}
				t.Errorf("index order %v: %s was warned about as a lookalike of %q. Both domains are "+
					"KNOWN — which is the ordinary state, since every inbound From becomes a contact "+
					"at ingest — so this warning can land on the GENUINE correspondent and present "+
					"them to the user as an imitation of the impersonator. Misdirection, not silence",
					order, d, w.Resembles)
			}
		}
	}
}

// The slot assignment must not depend on the order the index is built in, so
// that re-enabling the warning starts from something deterministic rather than
// from the query planner's dedup strategy (#411).
func TestTheSkeletonSlotIsDeterministicWhateverTheInputOrder(t *testing.T) {
	a := NewIndex(nil, []string{"mailbox.example", "rnailbox.example"})
	b := NewIndex(nil, []string{"rnailbox.example", "mailbox.example"})
	sk := Skeleton("mailbox.example")
	if a.bySkeleton[sk] != b.bySkeleton[sk] {
		t.Errorf("the skeleton slot went to %q in one input order and %q in the other — a warning "+
			"whose subject depends on the order a UNION happened to emit is not a warning",
			a.bySkeleton[sk], b.bySkeleton[sk])
	}
	// ⚠ AND AN OWN DOMAIN STILL WINS. The determinism must not have been bought
	// by dropping the rule that "this is not YOUR domain" is the more useful
	// thing to say.
	c := NewIndex([]string{"mailbox.example"}, []string{"rnailbox.example"})
	if got := c.bySkeleton[sk]; got != "mailbox.example" {
		t.Errorf("the skeleton slot went to %q, want the user's OWN domain to win it", got)
	}
}

// #754: more than one From field raises multipleFrom — counted raw, so the
// caller decides what a field is — and nothing else about the sender
// suppresses it. Zero means "not counted" and raises nothing.
func TestMultipleFrom(t *testing.T) {
	ix := newTestIndex()
	if w := kinds(ix.Check(Sender{Domain: "bank.example", FromFields: 2, AuthPassed: true})); !has(w, MultipleFrom) {
		t.Errorf("two From fields, even authenticated, produced %v", w)
	}
	// The stored From can have no domain when two fields were present; the
	// warning must not fall silent for that.
	if w := kinds(ix.Check(Sender{Domain: "", FromFields: 3})); !has(w, MultipleFrom) {
		t.Errorf("two From fields with no stored domain produced %v", w)
	}
	// Controls: one field, and an uncounted message, are silent.
	for _, n := range []int{0, 1} {
		if w := kinds(ix.Check(Sender{Domain: "bank.example", FromFields: n, AuthPassed: true})); has(w, MultipleFrom) {
			t.Errorf("FromFields=%d produced %v", n, w)
		}
	}
	// Worst first: a self-spoof still leads.
	w := kinds(ix.Check(Sender{Domain: "me.example", FromFields: 2}))
	if len(w) < 2 || w[0] != SelfSpoof || w[1] != MultipleFrom {
		t.Errorf("order = %v, want selfSpoof then multipleFrom", w)
	}
}
