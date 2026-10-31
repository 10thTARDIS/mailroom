// SPDX-License-Identifier: AGPL-3.0-only

package kind

import "testing"

// THE ACCEPTANCE TEST (#156), settled DETERMINISTICALLY and with no history:
// a retailer's promotion and that retailer's receipt must not be the same kind,
// on the first message ever seen from either sender.
//
// This is the case rule induction cannot reach, because a rule needs examples
// and this has none. Both messages carry the same From domain and the same
// brand; only the sending infrastructure and the machine-mail markers differ.
func TestRetailerPromotionAndReceiptAreDifferentKinds(t *testing.T) {
	promo := Facts{
		FromAddress:         "deals@wayfair.example",
		FromDomain:          "wayfair.example",
		Subject:             "40% off everything — last chance!",
		ListUnsubscribe:     "<https://wayfair.example/unsub>",
		ListUnsubscribePost: "List-Unsubscribe=One-Click",
		DKIMDomains:         []string{"wayfair.example", "mail.klaviyomail.com"},
		ReturnPath:          "<bounce-8f2a91c4de77@mail.klaviyomail.com>",
		RecipientCount:      1,
	}
	receipt := Facts{
		FromAddress:    "orders@wayfair.example",
		FromDomain:     "wayfair.example",
		Subject:        "Your order #55012 has shipped",
		AutoSubmitted:  "auto-generated",
		DKIMDomains:    []string{"wayfair.example"},
		ReturnPath:     "<orders@wayfair.example>",
		RecipientCount: 1,
	}

	gotPromo := Classify(promo)
	gotReceipt := Classify(receipt)

	if gotPromo.Kind != Promotional {
		t.Errorf("promotion classified %q (scores %v, signals %v)", gotPromo.Kind, gotPromo.Scores, names(gotPromo))
	}
	if gotReceipt.Kind != Transactional {
		t.Errorf("receipt classified %q (scores %v, signals %v)", gotReceipt.Kind, gotReceipt.Scores, names(gotReceipt))
	}
	if gotPromo.Kind == gotReceipt.Kind {
		t.Fatal("THE ACCEPTANCE TEST FAILED: a retailer's promotion and its receipt landed in the same kind")
	}
}

// Mail from someone the user actually writes to is personal, even when it has no
// distinguishing structure at all — the case a bulk-marker-driven classifier
// gets wrong by having nothing to say.
func TestCorrespondentIsPersonal(t *testing.T) {
	got := Classify(Facts{
		FromAddress:     "sam@example.com",
		FromDomain:      "example.com",
		Subject:         "lunch tomorrow?",
		HasCorresponded: true,
		IsReply:         true,
		RecipientCount:  1,
	})
	if got.Kind != Personal {
		t.Errorf("kind = %q, want personal (scores %v, signals %v)", got.Kind, got.Scores, names(got))
	}
}

// A mailing list is a list, not a promotion — List-Id is the discriminator, and
// it must outweigh the unsubscribe header that every list also carries.
func TestMailingListIsNotPromotional(t *testing.T) {
	got := Classify(Facts{
		FromAddress:     "someone@lists.example.org",
		FromDomain:      "lists.example.org",
		Subject:         "Re: the patch series",
		ListID:          "<dev.lists.example.org>",
		ListUnsubscribe: "<mailto:dev-unsubscribe@lists.example.org>",
		IsReply:         true,
		RecipientCount:  1,
	})
	if got.Kind != List {
		t.Errorf("kind = %q, want list (scores %v, signals %v)", got.Kind, got.Scores, names(got))
	}
}

// UNKNOWN IS A REAL ANSWER. Bulk mail with no other distinguishing marks must
// not be forced into a category — it goes to the review queue instead. A
// classifier that always decides is one that is silently wrong some of the time,
// which is the failure this whole design exists to avoid.
func TestAmbiguousBulkIsUnknownRatherThanGuessed(t *testing.T) {
	got := Classify(Facts{
		FromAddress:     "hello@somecompany.example",
		FromDomain:      "somecompany.example",
		Subject:         "An update from us",
		ListUnsubscribe: "<https://somecompany.example/u>",
		RecipientCount:  1,
	})
	if got.Kind != Unknown {
		t.Errorf("kind = %q, want unknown — nothing here distinguishes a promotion from a notification (scores %v, signals %v)",
			got.Kind, got.Scores, names(got))
	}
}

// A message with no headers worth reading is Unknown, not Personal. "Nothing
// suggests bulk" is much weaker evidence than "someone I write to", and
// conflating them would file every stranger's mail as personal.
func TestBareMessageIsUnknownNotPersonal(t *testing.T) {
	got := Classify(Facts{
		FromAddress:    "stranger@example.net",
		FromDomain:     "example.net",
		Subject:        "hello",
		RecipientCount: 1,
	})
	if got.Kind == Personal {
		t.Errorf("a stranger with no correspondence history was classified personal (scores %v)", got.Scores)
	}
}

// The infrastructure signal must work on its own, because it is the part that
// has no history requirement. A campaign platform says promotional even when the
// subject is bland.
func TestMarketingInfrastructureIsEnough(t *testing.T) {
	got := Classify(Facts{
		FromAddress:         "news@brand.example",
		FromDomain:          "brand.example",
		Subject:             "Introducing our new collection",
		ListUnsubscribe:     "<https://brand.example/u>",
		ListUnsubscribePost: "List-Unsubscribe=One-Click",
		DKIMDomains:         []string{"em.mcsv.net"},
		RecipientCount:      1,
	})
	if got.Kind != Promotional {
		t.Errorf("kind = %q, want promotional (scores %v, signals %v)", got.Kind, got.Scores, names(got))
	}
}

// Every verdict must carry its reasons. An unexplained classification cannot be
// corrected, and correctability is the whole contract.
func TestVerdictExplainsItself(t *testing.T) {
	got := Classify(Facts{
		FromAddress:     "deals@shop.example",
		FromDomain:      "shop.example",
		Subject:         "Save 30% this weekend",
		ListUnsubscribe: "<https://shop.example/u>",
		DKIMDomains:     []string{"shop.klaviyomail.com"},
		RecipientCount:  1,
	})
	if len(got.Signals) == 0 {
		t.Fatal("a verdict came back with no signals — nothing to show a user asking why")
	}
	want := map[string]bool{"esp_marketing": false, "subject_promotional": false, "list_unsubscribe": false}
	for _, s := range got.Signals {
		if _, ok := want[s.Name]; ok {
			want[s.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("expected signal %q to fire", name)
		}
	}
}

// Classification must be deterministic. Go randomises map iteration, and a
// classifier that disagreed with itself between runs would make every downstream
// rule and stored kind unstable.
func TestClassificationIsDeterministic(t *testing.T) {
	f := Facts{
		FromAddress:     "x@ambiguous.example",
		FromDomain:      "ambiguous.example",
		Subject:         "Your order shipped — 20% off your next one",
		ListUnsubscribe: "<https://ambiguous.example/u>",
		AutoSubmitted:   "auto-generated",
		RecipientCount:  1,
	}
	first := Classify(f)
	for i := 0; i < 200; i++ {
		if got := Classify(f); got.Kind != first.Kind || got.Score != first.Score {
			t.Fatalf("run %d disagreed: %q/%v vs %q/%v", i, got.Kind, got.Score, first.Kind, first.Score)
		}
	}
}

// The ESP table must never be consulted on the From domain. From says who the
// mail claims to be from; only DKIM and Return-Path say who sent it, and reading
// the brand's own domain as infrastructure is the exact confusion this signal
// exists to resolve.
func TestESPIsNotMatchedOnTheFromDomain(t *testing.T) {
	if ESPClassOf(Facts{FromDomain: "mcsv.net", FromAddress: "x@mcsv.net"}) != ESPNone {
		t.Error("the ESP table matched on the From domain")
	}
	if ESPClassOf(Facts{DKIMDomains: []string{"em.mcsv.net"}}) != ESPMarketing {
		t.Error("the ESP table did not match a DKIM d= subdomain")
	}
	if ESPClassOf(Facts{ReturnPath: "<bounce-abc@mail.sendgrid.net>"}) != ESPMixed {
		t.Error("the ESP table did not match a Return-Path domain")
	}
}

func names(v Verdict) []string {
	out := make([]string, 0, len(v.Signals))
	for _, s := range v.Signals {
		out = append(out, s.Name)
	}
	return out
}
