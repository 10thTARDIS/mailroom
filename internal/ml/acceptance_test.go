// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/kind"
	"mailroom/internal/tabs"
)

// THE v1.6.0 EPIC'S ACCEPTANCE TEST (#156), as code.
//
//	"A retailer's promotion and that retailer's order receipt must not land in
//	 the same category. Label two Wayfair advertisements Promotions. Wayfair
//	 advertisements should start filing as Promotions — and a Wayfair order
//	 confirmation or shipping notice must go to Shopping, or be surfaced for
//	 manual classification. It must NOT be swept into Promotions."
//
// It lived as prose in an issue body for a month, was verified for the rule path
// only (#168), and went UNOWNED when #175 closed without settling it for a
// sender never seen before. Two consecutive milestone audits found it drifting.
// So it is written here, where it runs.
//
// WHAT THE TEST HAS TO COVER, and why it is these three things. The failure mode
// the criterion names is a mechanism GENERALISING FROM SENDER: "two Wayfair ads
// are Promotions" becoming "everything from Wayfair is Promotions", which sweeps
// in the receipt. Exactly three mechanisms could do that, and each gets a case:
//
//  1. an induced RULE keyed on the domain — guarded by #168's
//     narrowest-consistent-rule walk and tested in internal/rules;
//  2. a MODEL that learned the domain — case two below, and the case with teeth:
//     it is prevented structurally, by stripping identity features, and a test
//     that does not try to break it proves nothing;
//  3. a model promoted on evidence that is really about one sender — case three.
//
// And the positive half, case one: the FUNCTION classifier separates them
// correctly, from public facts, on a domain it has never seen.
//
// ⚠ WHAT THIS DOES NOT CLAIM. Nothing here assigns the labels `Shopping` and
// `Promotions`. Those are TOPIC, they are per-user, and no public fact implies
// them. The criterion's "must go to Shopping" is not satisfied and will not be
// from public facts alone; its "must NOT be swept into Promotions" is, on both
// axes. That distinction is the whole function-versus-topic argument (ADR-021)
// and it is stated here so a future reader does not read a green test as more
// than it is.

// An unseen retailer, sending its campaign and its receipt down the SAME
// infrastructure — which is what real retailers do, and the reason
// tabs.Classify puts the transactional lexicon ABOVE the ESP rules.
const acceptanceRetailerDomain = "unseen-retailer.example"

// ONE sending address for both, which many retailers really do use and which is
// the ADVERSARIAL choice here: it maximises the identity signal shared between
// the campaign and the receipt, so case two's sweep assertion is testing the
// strongest version of the failure rather than a version the fixture happens to
// make easy. With separate addresses (deals@ / orders@) the sweep scored 0.799
// against a 0.80 threshold when identity was restored — the mechanism was real
// and the assertion still could not fail. Measured, not assumed.
const acceptanceRetailerSender = "mail@" + acceptanceRetailerDomain

func retailerPromo() kind.Facts {
	return kind.Facts{
		FromAddress:         acceptanceRetailerSender,
		FromDomain:          acceptanceRetailerDomain,
		Subject:             "48-hour sale: save 40% on everything",
		ListUnsubscribe:     "<https://" + acceptanceRetailerDomain + "/u>",
		ListUnsubscribePost: "List-Unsubscribe=One-Click",
		DKIMDomains:         []string{acceptanceRetailerDomain},
		RecipientCount:      1,
	}
}

func retailerReceipt() kind.Facts {
	return kind.Facts{
		FromAddress: acceptanceRetailerSender,
		FromDomain:  acceptanceRetailerDomain,
		Subject:     "Your order confirmation #48812 has shipped",
		// SAME bulk apparatus as the campaign. Retailers send receipts through
		// the same platform, so the message's FORM cannot tell them apart —
		// only what it says can.
		ListUnsubscribe:     "<https://" + acceptanceRetailerDomain + "/u>",
		ListUnsubscribePost: "List-Unsubscribe=One-Click",
		DKIMDomains:         []string{acceptanceRetailerDomain},
		RecipientCount:      1,
	}
}

// CASE ONE: the function classifier separates them on a domain it has never
// seen. This is the half of the criterion that is actually SATISFIED rather than
// merely not-violated, and it is satisfied sender-independently — the classifier
// has no per-user state and never saw this domain.
func TestAcceptanceRetailerPromoAndReceiptGetDifferentTabs(t *testing.T) {
	promo := tabs.Classify(retailerPromo())
	receipt := tabs.Classify(retailerReceipt())

	if promo.Tab == receipt.Tab {
		t.Fatalf("a retailer's campaign and its receipt both landed in %q (rules %q and %q) — this is the epic's acceptance test",
			promo.Tab, promo.Rule, receipt.Rule)
	}
	if promo.Tab != tabs.Promotions {
		t.Errorf("campaign went to %q via %q, want promotions", promo.Tab, promo.Rule)
	}
	if receipt.Tab != tabs.Updates {
		t.Errorf("receipt went to %q via %q, want updates", receipt.Tab, receipt.Rule)
	}

	// ⚠ ASSERT THE RULE, NOT JUST THE TAB, and this is not belt-and-braces.
	// `bulk_apparatus` — the residue — also answers Updates, so a receipt whose
	// transactional language went unrecognised would land in the right tab FOR
	// THE WRONG REASON and this test would still pass. Probed: breaking the
	// transactional lexicon leaves the tab assertions green and turns these red.
	// Which rule fired is the difference between the classifier reading the
	// message and the classifier defaulting.
	if receipt.Rule != tabs.RuleTransactionalSubject {
		t.Errorf("the receipt reached updates via %q, not %q — it was not recognised as a receipt, it fell through to the default",
			receipt.Rule, tabs.RuleTransactionalSubject)
	}
	if promo.Rule != tabs.RulePromotionalSubject {
		t.Errorf("the campaign reached promotions via %q, not %q", promo.Rule, tabs.RulePromotionalSubject)
	}
}

// CASE TWO, and the one with teeth: a model trained ONLY on this retailer's
// campaigns must not sweep in its receipt.
//
// It is prevented structurally — labelModelFeatures strips `from:`/`dom:`/
// `alias:`, so there is no weight that could fire on the shared domain — and the
// test deliberately stacks the deck to prove that is what is doing the work: the
// model is trained on 40 campaign messages from this one sender, then asked
// about a receipt from the SAME sender.
//
// PROBED, and the probe is why the fixture looks the way it does. Restoring
// identity to the model's features scores the receipt at 0.881 against a 0.80
// apply threshold — swept in, exactly the failure the criterion names. An
// earlier fixture that gave the campaign and the receipt different local parts
// (deals@ / orders@) scored 0.799: the mechanism was real, the sweep assertion
// still could not fail, and only the fixture guard below went red. Measuring
// that is what turned this from a test that passes into a test that can fail.
func TestAcceptanceAModelTrainedOnOneRetailersAdsDoesNotSweepInItsReceipt(t *testing.T) {
	labelID := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")

	// Positives: this retailer's campaigns. Negatives: unrelated mail, so the
	// only thing every positive shares is the domain and the sale vocabulary.
	var ex []LabelExample
	promoFV := featuresFromFacts(acceptanceMessageFacts(retailerPromo()))
	for i := 0; i < 40; i++ {
		ex = append(ex, LabelExample{Features: promoFV, Positive: true, Group: acceptanceRetailerDomain})
	}
	for i := 0; i < 40; i++ {
		ex = append(ex, LabelExample{
			Features: featuresFromFacts(MessageFacts{
				SenderAddress: "hello@newsletter.example",
				SenderDomain:  "newsletter.example",
				Subject:       "This week in local theatre",
			}),
			Positive: false,
			Group:    "newsletter.example",
		})
	}

	m, err := TrainLabeler(labelID, ex, labelTrainEpochs)
	if err != nil {
		t.Fatalf("train: %v", err)
	}

	receiptFV := labelModelFeatures(featuresFromFacts(acceptanceMessageFacts(retailerReceipt())))
	score := m.Score(receiptFV)
	if score >= LabelApplyAbove {
		t.Fatalf("the retailer's RECEIPT scored %.3f for its ads' label — at or above the %.2f apply threshold, it would be swept in. "+
			"This is exactly the failure #156's acceptance test names.", score, LabelApplyAbove)
	}

	// The deck really was stacked: confirm the domain is shared, so a passing
	// test cannot be an accident of the fixtures being unrelated.
	rawReceipt := featuresFromFacts(acceptanceMessageFacts(retailerReceipt()))
	if rawReceipt["dom:"+acceptanceRetailerDomain] == 0 {
		t.Fatal("fixture defect: the receipt does not carry the retailer's domain, so this test is not testing the sweep")
	}
	if _, present := receiptFV["dom:"+acceptanceRetailerDomain]; present {
		t.Error("the label model can see the sender domain; identity stripping is not in effect")
	}
}

// CASE THREE: even if such a model were trained, the promotion gate refuses it —
// a label whose positives come from ONE sender is an unanswerable question, not
// a low score. So nothing reaches a user at all, and the message falls to the
// needs-a-category surface, which is the criterion's own permitted outcome
// ("or be surfaced for manual classification").
func TestAcceptanceASingleSenderLabelCannotBePromoted(t *testing.T) {
	g := labelPromotionGate(LabelCVResult{
		Groups:   1,
		AUCs:     []float64{0.99, 0.99, 0.99, 0.99, 0.99},
		Decision: DecisionStats{Applied: 40, AppliedPositives: 40},
	}, labelPromotionFolds)

	if g.Pass {
		t.Fatalf("a label with one sender and flawless in-sample precision was promoted: %s", g.Reason)
	}
	if !strings.Contains(g.Reason, "too few senders") {
		t.Errorf("refused for the wrong reason (%q): one sender is a rule with extra steps, not a bad model", g.Reason)
	}
}

// acceptanceMessageFacts converts the kind.Facts the tab classifier reads into
// the MessageFacts the feature extractor reads, so both halves of this file
// describe the SAME message rather than two hand-built approximations of it.
func acceptanceMessageFacts(f kind.Facts) MessageFacts {
	return MessageFacts{
		SenderAddress:      f.FromAddress,
		SenderDomain:       f.FromDomain,
		Subject:            f.Subject,
		IsListMail:         f.ListID != "",
		HasListUnsubscribe: f.ListUnsubscribe != "",
		ToCcCount:          f.RecipientCount,
		HasCorresponded:    f.HasCorresponded,
		IsReplyInThread:    f.IsReply,
	}
}
