// SPDX-License-Identifier: AGPL-3.0-only

package tabs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/kind"
)

// The classifier's tests are ORDER tests as much as rule tests. A decision list
// is its ordering: every case below could be made to pass by a single rule in
// isolation, and what is actually being pinned is which rule wins when several
// apply at once. That is where a rule cascade goes wrong, and it is invisible to
// a test that feeds one signal at a time.

func TestListIdBeatsEverythingElse(t *testing.T) {
	// A message that is, simultaneously: RFC 2919 list traffic, sent through a
	// marketing platform, with a subject full of offer language, carrying
	// one-click unsubscribe. Every commercial signal there is, plus List-Id.
	f := kind.Facts{
		FromAddress:         "news@list.example.org",
		FromDomain:          "list.example.org",
		Subject:             "50% off — last chance, shop now!",
		ListID:              "<discuss.example.org>",
		ListUnsubscribe:     "<https://example.org/u>",
		ListUnsubscribePost: "List-Unsubscribe=One-Click",
		DKIMDomains:         []string{"mcsv.net"},
	}
	got := Classify(f)
	if got.Tab != Forums || got.Rule != RuleListID {
		t.Errorf("Classify = (%q via %q), want forums via %q — List-Id is a protocol fact and outranks every commercial signal",
			got.Tab, got.Rule, RuleListID)
	}
}

// TestOneClickIsNotMarketing is the delivery-preview trap, described in internal/kind. RFC 8058
// one-click says REGULATED BULK SENDER and
// nothing more — the 2024 Gmail/Yahoo rules compelled everyone above 5k/day to
// emit it — so reading it as "marketing" is how promotional once led most of a
// mailbox on apparatus alone, without a word of marketing being read.
//
// ⚠ THE SUBJECT HERE MUST MATCH NO LEXICON, and that is not a detail. The first
// version of this test used "Your Daily Digest", which the notification lexicon
// decides two rules earlier — so it passed while filing bare one-click mail as
// Promotions, and a probe that flipped the residue rule to Promotions left it
// green. The test's stated subject was never exercised (#62's failure exactly).
// The assertion below therefore also pins WHICH rule decided, so the fixture
// cannot drift back into being decided by something else.
func TestOneClickIsNotMarketing(t *testing.T) {
	f := kind.Facts{
		FromAddress:         "preview@email.delivery.example",
		FromDomain:          "email.delivery.example",
		Subject:             "Mail for the week ahead",
		ListUnsubscribe:     "<https://delivery.example/u>",
		ListUnsubscribePost: "List-Unsubscribe=One-Click",
		FeedbackID:          "1234:5678:preview",
	}
	got := Classify(f)
	if got.Rule != RuleBulkApparatus {
		t.Fatalf("decided by %q, not %q — this fixture no longer exercises the one-click path it exists to guard",
			got.Rule, RuleBulkApparatus)
	}
	if got.Tab == Promotions {
		t.Errorf("one-click bulk was filed as promotions — one-click means regulated bulk, never marketing")
	}
	if got.Tab != Updates {
		t.Errorf("Classify = %q, want updates", got.Tab)
	}
}

// TestBareBulkApparatusIsUpdatesNotPromotions covers the residue directly: bulk
// markers and NOTHING that says what the mail is for.
func TestBareBulkApparatusIsUpdatesNotPromotions(t *testing.T) {
	f := kind.Facts{
		FromAddress:     "hello@e.brand.example",
		FromDomain:      "e.brand.example",
		Subject:         "A note from us",
		ListUnsubscribe: "<https://brand.example/u>",
	}
	got := Classify(f)
	if got.Tab != Updates || got.Rule != RuleBulkApparatus {
		t.Errorf("Classify = (%q via %q), want updates via %q", got.Tab, got.Rule, RuleBulkApparatus)
	}
}

// TestTransactionalSubjectBeatsMarketingInfrastructure is the other half of the
// Promotions/Updates boundary. A retailer's receipts and its campaigns routinely
// leave on the SAME infrastructure, so what the message says it is has to beat
// what sent it — otherwise every dispatch notice from a brand that uses one
// platform for everything is filed as marketing.
func TestTransactionalSubjectBeatsMarketingInfrastructure(t *testing.T) {
	f := kind.Facts{
		FromAddress:         "orders@shop.example",
		FromDomain:          "shop.example",
		Subject:             "Your order has shipped",
		DKIMDomains:         []string{"mcsv.net"}, // a marketing platform
		ListUnsubscribe:     "<https://shop.example/u>",
		ListUnsubscribePost: "List-Unsubscribe=One-Click",
	}
	got := Classify(f)
	if got.Tab != Updates || got.Rule != RuleTransactionalSubject {
		t.Errorf("Classify = (%q via %q), want updates via %q — a receipt on marketing infrastructure is still a receipt",
			got.Tab, got.Rule, RuleTransactionalSubject)
	}
}

func TestMarketingPlatformWithoutTransactionalLanguageIsPromotions(t *testing.T) {
	f := kind.Facts{
		FromAddress:     "hello@shop.example",
		FromDomain:      "shop.example",
		Subject:         "New season, new styles",
		DKIMDomains:     []string{"mcsv.net"},
		ListUnsubscribe: "<https://shop.example/u>",
	}
	got := Classify(f)
	if got.Tab != Promotions || got.Rule != RuleESPMarketing {
		t.Errorf("Classify = (%q via %q), want promotions via %q", got.Tab, got.Rule, RuleESPMarketing)
	}
}

func TestPromotionalSubjectWithoutRecognisedInfrastructure(t *testing.T) {
	f := kind.Facts{
		FromAddress:     "deals@brand.example",
		FromDomain:      "brand.example",
		Subject:         "Limited time: 30% off everything",
		ListUnsubscribe: "<https://brand.example/u>",
	}
	got := Classify(f)
	if got.Tab != Promotions || got.Rule != RulePromotionalSubject {
		t.Errorf("Classify = (%q via %q), want promotions via %q", got.Tab, got.Rule, RulePromotionalSubject)
	}
}

// TestAutoSubmittedOutranksCommercialSignals. RFC 3834 has very low coverage on a
// real corpus but marketing avoids it deliberately, so
// where it appears it is decisive. Low coverage, high precision, and this pins
// the precision half.
func TestAutoSubmittedOutranksCommercialSignals(t *testing.T) {
	f := kind.Facts{
		FromAddress:     "system@service.example",
		FromDomain:      "service.example",
		Subject:         "Save 40% today only",
		AutoSubmitted:   "auto-generated",
		ListUnsubscribe: "<https://service.example/u>",
		DKIMDomains:     []string{"mcsv.net"},
	}
	got := Classify(f)
	if got.Tab != Updates || got.Rule != RuleAutoSubmitted {
		t.Errorf("Classify = (%q via %q), want updates via %q", got.Tab, got.Rule, RuleAutoSubmitted)
	}
}

func TestAutoRepliedIsItsOwnRule(t *testing.T) {
	f := kind.Facts{
		FromAddress:   "support@service.example",
		FromDomain:    "service.example",
		Subject:       "Re: your request",
		AutoSubmitted: "auto-replied; owner=support@service.example",
	}
	got := Classify(f)
	if got.Rule != RuleAutoReplied || got.Tab != Updates {
		t.Errorf("Classify = (%q via %q), want updates via %q", got.Tab, got.Rule, RuleAutoReplied)
	}
}

// TestAutoSubmittedNoIsNotAMarker: RFC 3834 says `no` means "this is ordinary
// mail". Treating the header's mere PRESENCE as evidence would file human mail
// from any client that sets it as machine mail.
func TestAutoSubmittedNoIsNotAMarker(t *testing.T) {
	f := kind.Facts{
		FromAddress:   "person@example.com",
		FromDomain:    "example.com",
		Subject:       "lunch?",
		AutoSubmitted: "no",
	}
	got := Classify(f)
	if got.Tab != Primary || got.Rule != RuleNoBulkMarkers {
		t.Errorf("Classify = (%q via %q), want primary via %q — Auto-Submitted: no means ordinary mail",
			got.Tab, got.Rule, RuleNoBulkMarkers)
	}
}

func TestSocialPlatformMatchesOnEverySendingIdentifier(t *testing.T) {
	cases := map[string]kind.Facts{
		"from domain": {
			FromAddress: "notify@facebookmail.com", FromDomain: "facebookmail.com",
			Subject: "You have 3 notifications",
		},
		"dkim d=": {
			FromAddress: "x@brand.example", FromDomain: "brand.example",
			Subject: "Someone viewed your profile", DKIMDomains: []string{"linkedin.com"},
		},
		"return-path": {
			FromAddress: "x@brand.example", FromDomain: "brand.example",
			Subject: "Weekly digest", ReturnPath: "<bounce-1@reddit.com>",
		},
		"bulk subdomain of a platform": {
			FromAddress: "jobs@maild.linkedin.com", FromDomain: "maild.linkedin.com",
			Subject: "Jobs for you",
		},
	}
	for name, f := range cases {
		got := Classify(f)
		if got.Tab != Social || got.Rule != RuleSocialPlatform {
			t.Errorf("%s: Classify = (%q via %q), want social via %q",
				name, got.Tab, got.Rule, RuleSocialPlatform)
		}
	}
}

// TestSocialListDoesNotMatchLookalikeDomains. Suffix matching on domains needs
// the label boundary or it is string matching pretending to be DNS — the exact
// bug internal/kind's sameOrSubdomain comment documents, where
// HasSuffix("notexample.com", "example.com") is true.
func TestSocialListDoesNotMatchLookalikeDomains(t *testing.T) {
	for _, domain := range []string{"notfacebook.com", "myx.com", "fake-reddit.com", "linkedin.com.evil.example"} {
		f := kind.Facts{FromAddress: "a@" + domain, FromDomain: domain, Subject: "hello"}
		if got := Classify(f); got.Rule == RuleSocialPlatform {
			t.Errorf("%q matched the social list — suffix matching must respect the label boundary", domain)
		}
	}
}

// TestCollaborationToolsAreNotSocial pins the stated boundary: Gmail's Social
// tab is social networks and media sharing, not everything with users. A GitHub
// notification is a notification about work.
func TestCollaborationToolsAreNotSocial(t *testing.T) {
	for _, domain := range []string{"github.com", "slack.com", "atlassian.com", "notion.so"} {
		f := kind.Facts{
			FromAddress: "notifications@" + domain, FromDomain: domain,
			Subject: "New comment on your issue",
		}
		if got := Classify(f); got.Tab == Social {
			t.Errorf("%q was filed as social via %q — collaboration tools are Updates, by the stated boundary",
				domain, got.Rule)
		}
	}
}

func TestNoBulkMarkersIsPrimary(t *testing.T) {
	f := kind.Facts{
		FromAddress: "friend@example.com",
		FromDomain:  "example.com",
		Subject:     "are you free thursday",
		IsReply:     true,
	}
	got := Classify(f)
	if got.Tab != Primary || got.Rule != RuleNoBulkMarkers {
		t.Errorf("Classify = (%q via %q), want primary via %q", got.Tab, got.Rule, RuleNoBulkMarkers)
	}
}

// TestClassifyIsTotal pins the property that made a `Decided` boolean pointless
// and that RuleNone's comment depends on: the last two rules are exact
// complements, so SOMETHING always fires and no message ever defaults silently
// into Primary. Narrowing either rule makes this red — which is the point.
func TestClassifyIsTotal(t *testing.T) {
	cases := []kind.Facts{
		{},
		{FromAddress: "x"},
		{FromAddress: "@", FromDomain: ""},
		{Subject: "                "},
		{ListUnsubscribe: "   "},
		{Precedence: "nonsense"},
		{DKIMDomains: []string{}},
		{DKIMDomains: []string{""}},
		{ReturnPath: "<>"},
		{AutoSubmitted: ";"},
		{RecipientCount: 9999},
	}
	for i, f := range cases {
		got := Classify(f)
		if got.Rule == RuleNone {
			t.Errorf("case %d (%+v): no rule fired — the decision list is supposed to be total", i, f)
		}
		if !got.Tab.Valid() {
			t.Errorf("case %d: Classify returned tab %q, which is not in the vocabulary", i, got.Tab)
		}
		if got.Why == "" {
			t.Errorf("case %d: verdict via %q carries no explanation; every verdict must explain itself",
				i, got.Rule)
		}
	}
}

// TestClassifierVersionTracksTheRuleList makes forgetting to bump
// ClassifierVersion a red build rather than a silent inconsistency.
//
// Stored verdicts (app.messages.classified_tab) carry the version that produced
// them, and a re-classify visits exactly the rows whose version is stale. If the
// rules change and the version does not, every existing row keeps a verdict the
// current code would not produce and NOTHING reports it — code that ran without
// erroring while doing nothing, which is the failure shape this project keeps
// paying for.
//
// The fingerprint is over each rule's NAME and TARGET TAB in order, so adding,
// removing, reordering or re-pointing a rule all move it. It deliberately does
// NOT cover the `why` strings or the test closures: reworded prose does not
// change any stored verdict, and forcing a version bump for it would train
// whoever hits it to bump without thinking.
//
// ⚠ A closure's BEHAVIOUR can change without the fingerprint moving — tighten a
// predicate in place and this test stays green. That is a real limit and it is
// why the constant's own comment carries the instruction too; this guards the
// common case, not every case.
func TestClassifierVersionTracksTheRuleList(t *testing.T) {
	var b strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&b, "%s=>%s;", r.name, r.tab)
	}
	sum := sha256.Sum256([]byte(b.String()))
	got := hex.EncodeToString(sum[:8])

	// Bump BOTH when the rule list changes: this fingerprint and ClassifierVersion.
	const wantFingerprint = "f955d3129c59274f"
	const wantVersion = 1

	if got != wantFingerprint {
		t.Errorf("the rule list changed (fingerprint %s, was %s).\n"+
			"Bump tabs.ClassifierVersion so stored verdicts are re-classified, then update\n"+
			"wantFingerprint and wantVersion here to match.", got, wantFingerprint)
	}
	if ClassifierVersion != wantVersion {
		t.Errorf("ClassifierVersion = %d but this test still expects %d — update wantVersion "+
			"(and wantFingerprint) together, so the pair cannot drift apart",
			ClassifierVersion, wantVersion)
	}
}

// TestEveryRuleHasAnExplanationAndAValidTab. A rule added without a `why` would
// ship a tab decision the user cannot interrogate, and the whole design rests on
// being correctable.
func TestEveryRuleHasAnExplanationAndAValidTab(t *testing.T) {
	seen := map[string]bool{}
	for i, r := range rules {
		if r.name == "" || r.name == RuleNone {
			t.Errorf("rule %d has no name", i)
		}
		if seen[r.name] {
			t.Errorf("two rules share the name %q; per-rule metrics would merge them", r.name)
		}
		seen[r.name] = true
		if !r.tab.Valid() {
			t.Errorf("rule %q targets %q, which is not in the vocabulary", r.name, r.tab)
		}
		if r.why == "" {
			t.Errorf("rule %q has no explanation", r.name)
		}
	}
}

// TestHasBulkApparatusRecognisesEachMarkerAlone. The residue rule and its
// complement both go through this one predicate, so a marker it misses is a
// message wrongly called Primary — mail that plainly announces itself as bulk
// landing in the tab reserved for people.
func TestHasBulkApparatusRecognisesEachMarkerAlone(t *testing.T) {
	cases := map[string]kind.Facts{
		"List-Unsubscribe":      {ListUnsubscribe: "<https://e.example/u>"},
		"List-Unsubscribe-Post": {ListUnsubscribePost: "List-Unsubscribe=One-Click"},
		"List-Id":               {ListID: "<l.example.org>"},
		"Feedback-ID":           {FeedbackID: "a:b:c"},
		"Precedence: bulk":      {Precedence: "bulk"},
		"Precedence: list":      {Precedence: "list"},
		"Precedence: junk":      {Precedence: "junk"},
		"a recognised ESP":      {DKIMDomains: []string{"sendgrid.net"}},
		"bulk-stream subdomain": {FromDomain: "e.brand.example"},
	}
	for name, f := range cases {
		if !hasBulkApparatus(f) {
			t.Errorf("%s alone was not recognised as bulk apparatus", name)
		}
	}
	if hasBulkApparatus(kind.Facts{FromAddress: "a@example.com", FromDomain: "example.com", Subject: "hi"}) {
		t.Error("plain mail was called bulk apparatus")
	}
	// Precedence has values that are NOT bulk markers; treating the header's
	// presence as the signal would sweep in ordinary mail.
	if hasBulkApparatus(kind.Facts{Precedence: "first-class"}) {
		t.Error("Precedence: first-class was read as a bulk marker")
	}
}
