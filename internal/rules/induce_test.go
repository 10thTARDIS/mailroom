// SPDX-License-Identifier: AGPL-3.0-only

package rules

import "testing"

// THE ACCEPTANCE TEST for #156: a retailer's promotion and that retailer's order
// receipt must not land in the same category.
//
// The fixture is a representative retail case — Wayfair — with the
// shape real retail mail has: marketing carries List-Unsubscribe and a List-Id,
// transactional mail carries neither, and the two go out from different senders
// at the same domain.

func promo(from, subject, list string) MessageFacts {
	return MessageFacts{
		From:            from,
		FromDomain:      "wayfair.example",
		Subject:         subject,
		ListID:          list,
		ListUnsubscribe: "<https://wayfair.example/unsub>",
	}
}

func transactional(from, subject string) MessageFacts {
	return MessageFacts{From: from, FromDomain: "wayfair.example", Subject: subject}
}

// Bulk mail from somewhere else entirely. The fixture NEEDS this: without other
// newsletters, the degenerate hypothesis "List-Unsubscribe exists" matches
// nothing beyond the examples and looks perfectly safe, so a test that omits
// them passes while proving nothing about the rule that would relabel a real
// mailbox. (It did exactly that on the first draft.)
func otherBulk(domain, subject string) MessageFacts {
	return MessageFacts{
		From:            "news@" + domain,
		FromDomain:      domain,
		Subject:         subject,
		ListID:          "<news." + domain + ">",
		ListUnsubscribe: "<https://" + domain + "/unsub>",
	}
}

// The receipts are UNLABELLED — the hard case, and the realistic one. A fresh
// mailbox has nothing to collide with, so "reject candidates that conflict with
// an existing label" cannot save us here; the rule must be excluded because the
// examples share a property the receipts lack.
func TestInduceDoesNotSweepReceiptsIntoPromotions(t *testing.T) {
	examples := []Example{
		{Facts: promo("deals@wayfair.example", "40% off everything", "<deals.wayfair.example>"), Positive: true},
		{Facts: promo("deals@wayfair.example", "Flash sale ends tonight", "<deals.wayfair.example>"), Positive: true},

		// Everything below is NOT labelled Promotions and must not be caught.
		{Facts: transactional("orders@wayfair.example", "Your order has shipped")},
		{Facts: transactional("orders@wayfair.example", "Order confirmation #55012")},
		{Facts: transactional("orders@wayfair.example", "Your package was delivered")},
		{Facts: transactional("service@wayfair.example", "Your password was changed")},
		{Facts: transactional("service@wayfair.example", "A new device signed in")},
		{Facts: transactional("orders@wayfair.example", "Your refund is on the way")},

		// Newsletters the user reads and has NOT called Promotions. These are what
		// make the "all bulk mail" hypothesis visibly wrong; without them it is
		// indistinguishable from the correct answer.
		{Facts: otherBulk("goodnews.example", "This week in science")},
		{Facts: otherBulk("localpaper.example", "Tuesday briefing")},
		{Facts: otherBulk("club.example", "Meeting minutes")},
		{Facts: otherBulk("charity.example", "Your impact this year")},
		{Facts: otherBulk("devblog.example", "Release notes")},
		{Facts: otherBulk("library.example", "New arrivals")},
	}

	got := Induce(examples)
	if len(got) == 0 {
		t.Fatal("induced nothing from two clearly-related examples")
	}
	best := got[0]

	// The rule must be about WAYFAIR, not about bulk mail in general.
	if best.Conditions.Matches(otherBulk("goodnews.example", "This week in science")) {
		t.Errorf("induced a rule that relabels unrelated newsletters as Promotions: %+v", best.Conditions)
	}
	if !usesField(best.Conditions, FieldFromDomain) && !usesField(best.Conditions, FieldFrom) {
		t.Errorf("induced a rule that does not identify the sender or domain: %+v", best.Conditions)
	}

	if best.Covers != best.Positives {
		t.Fatalf("best candidate covers %d/%d positives", best.Covers, best.Positives)
	}
	// THE POINT: not one receipt, shipping notice or security alert.
	if best.Collateral != 0 {
		t.Errorf("best candidate also matches %d non-promotional messages — receipts are being swept into Promotions", best.Collateral)
	}
	// And it must not have achieved that by pinning the exact subjects, which
	// would match these two messages and nothing ever again.
	if usesField(best.Conditions, FieldSubject) {
		t.Errorf("induced a subject-pinned rule that cannot generalise: %+v", best.Conditions)
	}
	// The bulk marker is what does the separating; a rule without it is the
	// domain rule that eats receipts.
	if !usesField(best.Conditions, FieldListUnsub) && !usesField(best.Conditions, FieldListID) {
		t.Errorf("induced a rule with no bulk-vs-transactional discriminator: %+v", best.Conditions)
	}
}

// The same mailbox, but the user has already filed one receipt under Shopping.
// Now there IS something to collide with, and a colliding candidate must never
// be offered — not ranked lower, not offered at all.
func TestInduceNeverOffersACandidateThatCollidesWithAnotherLabel(t *testing.T) {
	examples := []Example{
		{Facts: promo("deals@wayfair.example", "40% off everything", ""), Positive: true},
		{Facts: promo("deals@wayfair.example", "Flash sale", ""), Positive: true},
		{
			Facts:           transactional("orders@wayfair.example", "Order confirmation"),
			OtherUserLabels: []string{"Shopping"},
		},
	}

	for _, c := range Induce(examples) {
		if c.Conflicts > 0 {
			t.Errorf("offered a candidate matching mail the user labelled %v: %+v", c.ConflictLabels, c.Conditions)
		}
		if c.Conditions.Matches(transactional("orders@wayfair.example", "Order confirmation")) {
			t.Errorf("offered a candidate that matches the Shopping receipt: %+v", c.Conditions)
		}
	}
}

// THE KNEE. Generalising from "this sender" to "this whole domain" is usually
// right — but not when the domain sends a lot of OTHER bulk mail the user has
// not labelled. Then the broader rule stops meaning "mail like the examples".
//
// This is the only test that exercises the stop-at-the-knee walk: in the Wayfair
// fixture every surviving rung has zero collateral, so that test passes with the
// selection logic broken. Verified by breaking it deliberately.
func TestInduceStaysWithTheSenderWhenTheDomainWouldSweepMore(t *testing.T) {
	examples := []Example{
		{Facts: promo("deals@wayfair.example", "40% off everything", "<deals.wayfair.example>"), Positive: true},
		{Facts: promo("deals@wayfair.example", "Flash sale", "<deals.wayfair.example>"), Positive: true},
	}
	// Plenty of other bulk mail from the SAME domain, on a different list, that
	// the user has never called Promotions. Generalising to the domain would
	// swallow all of it.
	for _, subject := range []string{
		"Rate your recent purchase", "How did we do?", "Tell us about delivery",
		"Product review request", "Two minutes of your time", "We value your feedback",
		"Quick survey", "Share your experience",
	} {
		examples = append(examples, Example{Facts: MessageFacts{
			From:            "surveys@wayfair.example",
			FromDomain:      "wayfair.example",
			Subject:         subject,
			ListID:          "<surveys.wayfair.example>",
			ListUnsubscribe: "<https://wayfair.example/unsub>",
		}})
	}

	got := Induce(examples)
	if len(got) == 0 {
		t.Fatal("induced nothing")
	}
	best := got[0]
	if best.Collateral != 0 {
		t.Errorf("best candidate sweeps in %d survey messages; it should have stayed with the sender or the list", best.Collateral)
	}
	if !usesField(best.Conditions, FieldFrom) && !usesField(best.Conditions, FieldListID) {
		t.Errorf("generalised past the knee to %+v, which no longer means \"mail like the examples\"", best.Conditions)
	}
}

// Generalisation must still HAPPEN when it is safe. Two newsletters from
// different senders on the same list should induce the list rule, not fail for
// lack of a shared sender.
func TestInduceGeneralisesAcrossSendersOnTheSameList(t *testing.T) {
	examples := []Example{
		{Facts: MessageFacts{
			From: "alice@list.example", FromDomain: "list.example",
			ListID: "<weekly.list.example>", ListUnsubscribe: "<https://list.example/u>",
		}, Positive: true},
		{Facts: MessageFacts{
			From: "bob@list.example", FromDomain: "list.example",
			ListID: "<weekly.list.example>", ListUnsubscribe: "<https://list.example/u>",
		}, Positive: true},
	}

	got := Induce(examples)
	if len(got) == 0 {
		t.Fatal("induced nothing")
	}
	// A third sender on the same list must be caught, or the rule is useless for
	// the mail that has not arrived yet.
	third := MessageFacts{
		From: "carol@list.example", FromDomain: "list.example",
		ListID: "<weekly.list.example>", ListUnsubscribe: "<https://list.example/u>",
	}
	if !got[0].Conditions.Matches(third) {
		t.Errorf("best candidate does not match a new sender on the same list: %+v", got[0].Conditions)
	}
}

// Examples with nothing in common must induce NOTHING. Returning a rule that
// happens to cover two unrelated messages would be a confident answer to a
// question the evidence cannot answer.
func TestInduceReturnsNothingWhenExamplesShareNothing(t *testing.T) {
	examples := []Example{
		{Facts: MessageFacts{From: "a@one.example", FromDomain: "one.example"}, Positive: true},
		{Facts: MessageFacts{From: "b@two.example", FromDomain: "two.example"}, Positive: true},
	}
	if got := Induce(examples); len(got) != 0 {
		t.Errorf("induced %d candidates from unrelated examples: %+v", len(got), got)
	}
}

// Every candidate offered must explain ALL the evidence. A rule covering one of
// two labelled messages is not an induction of what the user did.
func TestInduceOnlyOffersCandidatesCoveringEveryPositive(t *testing.T) {
	examples := []Example{
		{Facts: promo("deals@wayfair.example", "Sale", "<deals.wayfair.example>"), Positive: true},
		{Facts: promo("news@wayfair.example", "Newsletter", "<news.wayfair.example>"), Positive: true},
		{Facts: transactional("orders@wayfair.example", "Shipped")},
	}
	for _, c := range Induce(examples) {
		if c.Covers != c.Positives {
			t.Errorf("offered a candidate covering %d/%d positives: %+v", c.Covers, c.Positives, c.Conditions)
		}
	}
}

// THE REAL-DATA REGRESSION (#173). Built from the shape of a real Promotions
// label, not a tidy fixture: 22 hand-labelled messages from 17 distinct senders
// at 17 distinct domains, only 20 of which carry List-Unsubscribe.
//
// The first version of Induce asked what ALL of them had in common. The answer was
// genuinely nothing, so it proposed nothing for the largest label in the
// mailbox — correct on the Wayfair fixture, inert on real mail. This is the
// shape that has to keep working.
func TestInduceRealCategorySpanningManySenders(t *testing.T) {
	var examples []Example

	// Three clusters of different sizes.
	for domain, n := range map[string]int{
		"e.shop.example":   4,
		"e.press.example":  2,
		"knitting.example": 2,
	} {
		for i := 0; i < n; i++ {
			examples = append(examples, Example{Positive: true, Facts: MessageFacts{
				From:            "news@" + domain,
				FromDomain:      domain,
				Subject:         "offer",
				ListUnsubscribe: "<https://" + domain + "/u>",
			}})
		}
	}
	// The long tail: 14 one-off senders, two of them with no unsubscribe header
	// (matching a realistic split). No rule should be induced from these.
	for i := 0; i < 14; i++ {
		domain := "shop" + string(rune('a'+i)) + ".example"
		f := MessageFacts{From: "news@" + domain, FromDomain: domain, Subject: "sale"}
		if i < 12 {
			f.ListUnsubscribe = "<https://" + domain + "/u>"
		}
		examples = append(examples, Example{Positive: true, Facts: f})
	}

	got := Induce(examples)
	if len(got) != 3 {
		t.Fatalf("induced %d rules, want 3 — one per cluster, none from the 14 one-off senders", len(got))
	}
	// Biggest evidence first.
	if got[0].Positives != 4 {
		t.Errorf("first candidate covers a cluster of %d, want the 4-message one", got[0].Positives)
	}
	for _, c := range got {
		if c.Covers != c.Positives {
			t.Errorf("candidate covers %d of its %d-message cluster", c.Covers, c.Positives)
		}
		// A cluster rule must not reach into the long tail.
		if c.Collateral != 0 {
			t.Errorf("cluster rule has %d collateral: %+v", c.Collateral, c.Conditions)
		}
	}
}

// A label whose every message is from a different sender yields NOTHING. There
// is no rule in that: it is the long tail the classifier handles (#169), and
// proposing one would be inventing evidence.
func TestInduceProposesNothingForAllOneOffSenders(t *testing.T) {
	var examples []Example
	for i := 0; i < 8; i++ {
		domain := "shop" + string(rune('a'+i)) + ".example"
		examples = append(examples, Example{Positive: true, Facts: MessageFacts{
			From: "news@" + domain, FromDomain: domain,
			ListUnsubscribe: "<https://" + domain + "/u>",
		}})
	}
	if got := Induce(examples); len(got) != 0 {
		t.Errorf("induced %d rules from 8 unrelated senders: %+v", len(got), got)
	}
}

// usesField reports whether the tree tests a field anywhere.
func usesField(c Condition, field string) bool {
	if c.Field == field {
		return true
	}
	for _, sub := range append(append([]Condition{}, c.All...), c.Any...) {
		if usesField(sub, field) {
			return true
		}
	}
	return false
}

// THE ALIAS AS EVIDENCE (mailroom#177).
//
// A catchall user hands out a per-service address, which categorises the mail at
// signup time. The label below spans three different sending domains — no
// sender, domain or list is shared — so before the alias conjunct existed there
// was nothing consistent to induce from and the inducer proposed NOTHING.
//
// This is the case a real mailbox is full of: a shop's mails from
// its own domain, marketplace senders and shipping partners, and all of it arrives
// at shop@.
func TestInduceUsesTheAliasWhenSendersDiffer(t *testing.T) {
	atAlias := func(from, domain, subject string) MessageFacts {
		return MessageFacts{From: from, FromDomain: domain, Subject: subject, Alias: "shop@example.net"}
	}
	examples := []Example{
		{Facts: atAlias("ship@amazon.example", "amazon.example", "Your package is out for delivery"), Positive: true},
		{Facts: atAlias("orders@marketplace.example", "marketplace.example", "Order confirmation"), Positive: true},
		{Facts: atAlias("no-reply@logistics.example", "logistics.example", "Delivered to your porch"), Positive: true},

		// Other mail, at other aliases, that must not be caught.
		{Facts: MessageFacts{From: "news@yarn.example", FromDomain: "yarn.example", Alias: "yarn@example.net"}},
		{Facts: MessageFacts{From: "hi@sub.example", FromDomain: "sub.example", Alias: "sub@example.net"}},
		{Facts: MessageFacts{From: "a@bank.example", FromDomain: "bank.example", Alias: "bofa@example.net"}},
	}

	got := Induce(examples)
	if len(got) == 0 {
		t.Fatal("induced nothing: three senders with nothing in common but the alias the user handed out")
	}
	best := got[0]
	if !usesField(best.Conditions, FieldAlias) {
		t.Fatalf("induced a rule that ignores the alias: %+v", best.Conditions)
	}
	if best.Covers != best.Positives {
		t.Errorf("best candidate covers %d/%d positives", best.Covers, best.Positives)
	}
	if best.Collateral != 0 {
		t.Errorf("best candidate also matches %d messages at other aliases", best.Collateral)
	}
}

// THE ACCEPTANCE TEST, UNDER A CATCHALL — the reason the alias conjunct is
// ordered BEFORE the bulk marker rather than after it.
//
// Under a catchall a retailer's promos and its receipts arrive at the SAME
// alias, so the alias alone cannot tell them apart: `alias = shop@` matches
// both, which is precisely the merge #156's acceptance test forbids. Ordering
// alias before ListUnsub in sharedConjuncts means the most general rung the
// ladder can reach is `alias AND unsubscribe`, which still separates them.
//
// Move the alias conjunct after the ListUnsub one and this test fails: the
// ladder's last real rung becomes the alias alone and the receipts come with it.
func TestInduceKeepsReceiptsOutWhenTheyShareTheAlias(t *testing.T) {
	const alias = "shop@example.net"
	promoAt := func(subject string) MessageFacts {
		return MessageFacts{
			From: "deals@amazon.example", FromDomain: "amazon.example", Subject: subject,
			Alias: alias, ListUnsubscribe: "<https://amazon.example/unsub>",
		}
	}
	receiptAt := func(subject string) MessageFacts {
		return MessageFacts{
			From: "orders@amazon.example", FromDomain: "amazon.example", Subject: subject,
			Alias: alias,
		}
	}
	examples := []Example{
		{Facts: promoAt("Deals of the day"), Positive: true},
		{Facts: promoAt("Lightning deals end tonight"), Positive: true},

		// Same alias, same domain, NOT labelled — and must not be swept in.
		{Facts: receiptAt("Your order has shipped")},
		{Facts: receiptAt("Order confirmation #55012")},
		{Facts: receiptAt("Your refund is on the way")},

		// Bulk mail elsewhere, so "unsubscribe exists" is visibly too broad.
		{Facts: otherBulk("goodnews.example", "This week in science")},
		{Facts: otherBulk("localpaper.example", "Tuesday briefing")},
		{Facts: otherBulk("club.example", "Meeting minutes")},
	}

	got := Induce(examples)
	if len(got) == 0 {
		t.Fatal("induced nothing from two clearly-related promos")
	}
	best := got[0]
	for _, r := range []MessageFacts{
		receiptAt("Your order has shipped"),
		receiptAt("Order confirmation #55012"),
		receiptAt("Your refund is on the way"),
	} {
		if best.Conditions.Matches(r) {
			t.Fatalf("induced a rule that sweeps receipts arriving at the same alias into Promotions: %+v",
				best.Conditions)
		}
	}
	if best.Conditions.Matches(otherBulk("goodnews.example", "This week in science")) {
		t.Errorf("induced a rule that relabels unrelated newsletters: %+v", best.Conditions)
	}
	if best.Collateral != 0 {
		t.Errorf("best candidate also matches %d non-promotional messages", best.Collateral)
	}
}
