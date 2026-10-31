// SPDX-License-Identifier: AGPL-3.0-only

package tabs

import (
	"strings"

	"mailroom/internal/kind"
)

// THE TAB CLASSIFIER: what tab a message belongs in, judged from public RFC
// facts about the message itself (mailroom#219). It runs on mail that no label
// maps — which, in a mailbox that is mostly bulk, is most of what a new user has.
//
// A DECISION LIST, NOT A SCORE. Every rule below is first-match-wins, named, and
// grounded in one public fact. That shape is chosen against this project's own
// history: internal/kind's failures all lived in its AGGREGATE — weights,
// margins, correlated signals, nested thresholds — and its successes all lived
// in its FACTS. Three tuning passes, two reverts and one wrong-direction metric
// later, the settled direction is to consume the facts and let something
// legible arbitrate. A first-match list has no weights to tune, explains itself
// in one sentence ("carries List-Id, so: Forums"), and — the property that
// matters for measurement — lets the acceptance test report precision PER RULE,
// so a badly chosen rule shows up as a bad rule instead of hiding inside a
// tab-level average.
//
// NO PER-USER BEHAVIOUR, DELIBERATELY. kind.Facts carries HasCorresponded, and
// this classifier does not set it. The entire justification for tabs is that
// function is sender-independent BY CONSTRUCTION — the RFCs define what the
// headers mean, so they mean the same thing for a sender never seen before, and
// the tab is therefore right on a stranger's FIRST message. A tab decided partly
// by "have I written to them" is no longer that. (It would also be dead weight
// here: a mostly-inbound mailbox has almost no sent messages.)
//
// NO TRAIN/SERVE GAP, BY CONSTRUCTION. A message's tab is derived at read time
// from the stored row (ADR-021), so replay and serve are the SAME path reading
// the SAME columns. The gap that #157 was fixed to close cannot open here,
// because there is no second path to disagree with.

// Rule names. They are stable identifiers: the acceptance test reports per-rule
// precision under these names, and `mailroom-admin tab-eval` output is compared
// across runs.
const (
	RuleListID               = "list_id"
	RuleSocialPlatform       = "social_platform"
	RuleAutoSubmitted        = "auto_submitted"
	RuleAutoReplied          = "auto_replied"
	RuleTransactionalSubject = "transactional_subject"
	RuleESPTransactional     = "esp_transactional"
	RuleESPMarketing         = "esp_marketing"
	RulePromotionalSubject   = "promotional_subject"
	RuleNotificationSubject  = "notification_subject"
	RuleESPNewsletter        = "esp_newsletter"
	RuleBulkApparatus        = "bulk_apparatus"
	RuleNoBulkMarkers        = "no_bulk_markers"
	// RuleNone is the defensive default, and is UNREACHABLE as the list stands:
	// the last two rules are exact complements (`hasBulkApparatus` and its
	// negation), so one of them always fires. TestClassifyIsTotal pins that.
	//
	// It is kept rather than deleted because the property is a fact about the
	// current rule list, not a guarantee of the design: narrow `no_bulk_markers`
	// and this becomes live, at which point the test goes red and whoever did it
	// finds out immediately instead of discovering a silent Primary default.
	RuleNone = "none"
)

// ClassifierVersion stamps every stored verdict (app.messages.classifier_version,
// migration 00029). A row whose version differs from this constant is STALE and
// is exactly what `mailroom-admin classify-tabs` re-visits.
//
// ⚠ BUMP THIS WHENEVER THE RULE LIST CHANGES — a rule added, removed, reordered,
// or re-pointed at a different tab. Forgetting would leave every existing row
// holding a verdict the current code would not produce, with nothing reporting
// it, which is this project's most expensive failure shape: code that ran
// without erroring while doing nothing.
//
// It is not left to discipline. TestClassifierVersionTracksTheRuleList
// fingerprints the rule list and fails when the fingerprint moves without this
// constant moving with it, so the reminder is a red build rather than a comment.
const ClassifierVersion = 1

// Verdict is one message's tab decision, and how it was reached.
//
// THERE IS NO `Decided` BOOLEAN HERE, and the absence is deliberate. One was
// drafted — to separate "Primary on evidence" from "Primary because nothing
// fired" — and removed once it turned out the rule list is TOTAL, which would
// have made it permanently true. A field that cannot vary is a metric that
// cannot fail: the report would have printed a fallback count of 0 forever and
// read as reassurance. The rule NAME carries strictly more information anyway,
// and `tab-eval` decomposes precision per rule, which is where the real humility
// belongs — `bulk_apparatus` is the rule that knows least, and it is measured
// under its own name instead of hidden behind a boolean.
type Verdict struct {
	// Tab is where the message goes.
	Tab Tab
	// Rule names the rule that fired.
	Rule string
	// Why is the one-sentence explanation shown to a user. A classification a
	// user cannot interrogate is one they cannot correct, and the whole design
	// rests on being correctable.
	Why string
}

// Classify decides a message's tab from its header facts.
//
// TOTAL BY CONSTRUCTION: the last two rules partition every message, so this
// always returns a rule-backed verdict. That is not the same as guessing — "no
// bulk apparatus of any kind" is a real observation about a message, not a
// shrug. The place where this classifier genuinely does not know is the
// `bulk_apparatus` residue, and that is named, reported separately, and not
// dressed up as an abstention.
func Classify(f kind.Facts) Verdict {
	for _, r := range rules {
		if r.test(f) {
			return Verdict{Tab: r.tab, Rule: r.name, Why: r.why}
		}
	}
	return Verdict{Tab: Primary, Rule: RuleNone,
		Why: "No bulk, list or campaign markers were recognised."}
}

type rule struct {
	name string
	tab  Tab
	why  string
	test func(kind.Facts) bool
}

// rules is the decision list, in order. Order IS the logic here, so each entry
// says why it sits where it does.
var rules = []rule{
	{
		// RFC 2919. The one tab decided by a protocol fact rather than a
		// judgement, which is why it goes first: a message carrying List-Id is
		// list traffic whatever else is true of it.
		//
		// FORUMS IS List-Id AND NOTHING ELSE. Newsletter platforms that omit the
		// header were considered for this rule and deliberately left out — the
		// tab means "RFC 2919 list traffic", and widening it to "things that feel
		// like lists" would make the one unambiguous rule in this file a matter
		// of taste.
		name: RuleListID, tab: Forums,
		why:  "Carries a List-Id header (RFC 2919), so it is mailing-list traffic.",
		test: func(f kind.Facts) bool { return strings.TrimSpace(f.ListID) != "" },
	},
	{
		// A curated list of public social platforms — the same class of evidence
		// as internal/kind's ESP table, and legitimate for the same reason: these
		// are published facts about public companies, not anybody's mail.
		//
		// It is ALSO the weakest rule here, because it is the only one that is a
		// name list rather than a header semantic, and a name list can be quietly
		// fitted to whatever corpus is being scored. `tab-eval` therefore reports
		// how many DISTINCT SENDERS this rule fired on: a list that scores 1.00
		// across three senders has demonstrated nothing.
		name: RuleSocialPlatform, tab: Social,
		why:  "Sent by a social platform.",
		test: func(f kind.Facts) bool { return isSocialPlatform(f) },
	},
	{
		// RFC 3834, and it beats every commercial signal below because marketing
		// AVOIDS it deliberately (it suppresses the auto-replies campaigns want).
		// Low coverage — a tiny share of a real mailbox — and that
		// number is the measured half of internal/kind's "low coverage, high
		// precision" claim. Cheap to keep, and it is never the reason a message
		// is misfiled.
		name: RuleAutoSubmitted, tab: Updates,
		why: "Marked Auto-Submitted (RFC 3834): generated by a machine, not a campaign.",
		test: func(f kind.Facts) bool {
			v := autoSubmitted(f)
			return v != "" && v != "no" && !strings.HasPrefix(v, "auto-replied")
		},
	},
	{
		// RFC 3834's other value: this message IS an automatic reply — a vacation
		// responder, a ticket acknowledgement. It answers something the user did,
		// which is the definition of the Updates tab rather than of Primary.
		name: RuleAutoReplied, tab: Updates,
		why:  "An automatic reply (RFC 3834), answering something you sent.",
		test: func(f kind.Facts) bool { return strings.HasPrefix(autoSubmitted(f), "auto-replied") },
	},
	{
		// THE PROMOTIONS/UPDATES DISCRIMINATOR, and the reason it is this high in
		// the list. #219 names that boundary as the known-hard one — both sides
		// are bulk with an unsubscribe header — and this lexicon is the measured
		// separator: it fires on a large share of Shopping mail against a small share
		// of Promotions. Real signal, partial recall.
		//
		// Above the ESP rules on purpose. A retailer's receipts and its campaigns
		// routinely leave on the SAME infrastructure, so "Shipped: order #…" from
		// a marketing platform is still a dispatch notice. What the message SAYS
		// it is beats what sent it.
		name: RuleTransactionalSubject, tab: Updates,
		why: "The subject reads like a receipt, confirmation or delivery notice.",
		test: func(f kind.Facts) bool {
			for _, c := range kind.SubjectClasses(f.Subject) {
				if c == kind.SubjectTransactional || c == kind.SubjectOrderNumber {
					return true
				}
			}
			return false
		},
	},
	{
		name: RuleESPTransactional, tab: Updates,
		why:  "Sent through transactional infrastructure — receipts and confirmations, not campaigns.",
		test: func(f kind.Facts) bool { return kind.ESPClassOf(f) == kind.ESPTransactional },
	},
	{
		name: RuleESPMarketing, tab: Promotions,
		why:  "Sent through a marketing campaign platform.",
		test: func(f kind.Facts) bool { return kind.ESPClassOf(f) == kind.ESPMarketing },
	},
	{
		name: RulePromotionalSubject, tab: Promotions,
		why: "The subject reads like an offer — a sale, a discount, a limited-time deal.",
		test: func(f kind.Facts) bool {
			for _, c := range kind.SubjectClasses(f.Subject) {
				if c == kind.SubjectPromotional {
					return true
				}
			}
			return false
		},
	},
	{
		// Something HAPPENED and you are being told, without having transacted:
		// a digest, an alert, a statement being ready. Below the promotional
		// lexicon because a campaign that also says "ends today" is still a
		// campaign.
		name: RuleNotificationSubject, tab: Updates,
		why: "The subject reports that something happened — an alert, digest or reminder.",
		test: func(f kind.Facts) bool {
			for _, c := range kind.SubjectClasses(f.Subject) {
				if c == kind.SubjectNotification {
					return true
				}
			}
			return false
		},
	},
	{
		// Publishing platforms — subscribed editorial rather than retail
		// marketing. Updates rather than Promotions because a publication telling
		// you an issue is out is not selling you something; Updates rather than
		// Forums because Forums is List-Id and this message does not carry it.
		//
		// A JUDGEMENT CALL, AND FLAGGED AS ONE. It happens to agree with the
		// a typical mapping (Patreon and News both route to Updates),
		// and that agreement is NOT evidence — it is the sort of coincidence that
		// makes a fitted rule look validated. The eval reports this rule's own
		// precision so it stands or falls on its own.
		name: RuleESPNewsletter, tab: Updates,
		why:  "Sent through a newsletter or publishing platform.",
		test: func(f kind.Facts) bool { return kind.ESPClassOf(f) == kind.ESPNewsletter },
	},
	{
		// THE RESIDUE, AND THE MOST IMPORTANT LINE IN THIS FILE TO GET RIGHT.
		//
		// What is left is mail with bulk apparatus — one-click unsubscribe,
		// List-Unsubscribe, a Feedback-ID, a Precedence marker, a segregated bulk
		// sending subdomain — and NO signal about what it is FOR. That is a large
		// population: one-click alone is on most of a typical mailbox.
		//
		// It goes to Updates, and the reason it must not go to Promotions is
		// written into internal/kind by name: RFC 8058 one-click says REGULATED
		// BULK SENDER and nothing more, because the 2024 Gmail/Yahoo rules
		// compelled every sender above 5,000/day to emit it. A delivery-preview
		// notification service carries it exactly like a retailer does. Reading "is bulk" as
		// "is marketing" is the specific mistake that once had promotional
		// leading most of a mailbox on apparatus alone, without a single word of
		// marketing being read.
		//
		// It is named separately from every other Updates rule PRECISELY so the
		// eval can decompose the Updates tab: "Updates precision 0.8" means
		// something very different if most of it arrives through this rule rather
		// than through the transactional lexicon. Reporting them together would
		// hide the only number that says whether Promotions/Updates separation
		// works at all.
		name: RuleBulkApparatus, tab: Updates,
		why:  "Bulk-sender apparatus with no sign of what it is for — filed as machine mail, not marketing.",
		test: func(f kind.Facts) bool { return hasBulkApparatus(f) },
	},
	{
		// No bulk apparatus of any kind: not a list, no unsubscribe, no
		// Precedence, no Feedback-ID, no recognised ESP, no bulk-stream subdomain.
		// This is the POSITIVE Primary verdict, as distinct from the fallback.
		//
		// Expect it to be thin. #219 predicts that from the mailbox's own shape
		// (mostly bulk, almost no sent messages) — a property of such a mailbox, not a
		// defect in the rule.
		name: RuleNoBulkMarkers, tab: Primary,
		why:  "No list, unsubscribe or bulk-sending markers at all.",
		test: func(f kind.Facts) bool { return !hasBulkApparatus(f) },
	},
}

// hasBulkApparatus reports the presence of ANY machine-bulk marker. It is the
// test behind both of the last two rules — the residue and its complement — so
// the two cannot drift apart and leave a message matching neither or both.
func hasBulkApparatus(f kind.Facts) bool {
	if strings.TrimSpace(f.ListUnsubscribe) != "" ||
		strings.TrimSpace(f.ListUnsubscribePost) != "" ||
		strings.TrimSpace(f.FeedbackID) != "" ||
		strings.TrimSpace(f.ListID) != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(f.Precedence)) {
	case "bulk", "list", "junk":
		return true
	}
	if kind.ESPClassOf(f) != kind.ESPNone {
		return true
	}
	return kind.HasBulkStreamShape(f.FromDomain)
}

// autoSubmitted normalises the Auto-Submitted header: RFC 3834 allows
// parameters after the keyword ("auto-replied; owner=…"), so the keyword is
// taken up to the first ";".
func autoSubmitted(f kind.Facts) string {
	v := strings.ToLower(strings.TrimSpace(f.AutoSubmitted))
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}
