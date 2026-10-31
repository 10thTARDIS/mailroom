// SPDX-License-Identifier: AGPL-3.0-only

// Package kind is the deterministic message-KIND classifier (spec §8,
// mailroom#175): what sort of mail this is, judged from headers alone, on the
// first message ever seen from a sender.
//
// WHY THIS EXISTS. Automatic categorisation has a cold-start problem, and the
// prior art is unanimous about the shape of the solution: every shipped system
// pairs a GLOBAL model with a per-user delta. Gmail's Priority Inbox sums frozen
// global log-odds with a personal model that only encodes how the user differs;
// SpamAssassin and Fastmail run deterministic rules alone until a personal Bayes
// database has 200 ham and 200 spam; Apple ships an on-device model that works
// with no history at all. Nobody starts a per-user model from zero — which is
// exactly what Mailroom's first attempt did, and why it learned nothing but the
// senders it had already seen.
//
// We cannot ship a model trained on other people's mail: single-user,
// self-hosted, no corpus, and spec §8 forbids it. But the global slot does not
// require anyone's mail. It requires PUBLIC knowledge — RFC semantics and public
// sender-infrastructure facts — which §8 explicitly permits. This package is
// that: SpamAssassin's static ruleset, aimed at categories instead of spam.
//
// It got materially easier recently. Since the 2024 Gmail/Yahoo bulk-sender
// rules, senders above 5,000 messages a day are COMPELLED to emit RFC 8058
// one-click unsubscribe with aligned SPF/DKIM/DMARC. Bulk mail now self-
// identifies by regulation rather than by convention, which makes a header-only
// classifier far more reliable than it would have been five years ago.
//
// KINDS ARE NOT CATEGORIES. Nothing here is ever shown to the user as a folder
// or a tab — that would be Gmail's Primary/Social/Promotions, which spec §8 and
// mailroom#156 explicitly reject. A kind BINDS to one of the user's own labels,
// through a visible and editable T0 rule they accept. The taxonomy is internal
// machinery for answering "what sort of thing is this?" before anyone has said
// what they want done about it.
//
// EVERY VERDICT EXPLAINS ITSELF. Scoring is additive over named signals and the
// hit list comes back with the answer, in the SpamAssassin tradition, because a
// classification a user cannot interrogate is one they cannot correct — and the
// whole design rests on being correctable.
package kind

import (
	"regexp"
	"sort"
	"strings"
)

// Kind is what sort of mail a message is.
type Kind string

const (
	// Personal is mail from a human, to this human.
	Personal Kind = "personal"
	// Transactional is machine-generated mail about something the user DID: an
	// order, a booking, a payment, a password reset. The receipts half of the
	// retailer problem.
	Transactional Kind = "transactional"
	// Promotional is marketing: the advertisements half.
	Promotional Kind = "promotional"
	// Notification is an automated status or activity report — a service telling
	// the user something happened, without the user having transacted.
	Notification Kind = "notification"
	// List is discussion-list and newsletter traffic, identified by List-Id
	// infrastructure rather than marketing infrastructure.
	List Kind = "list"
	// Automated is machine mail that fits nothing narrower — bounces, cron
	// output, vacation replies.
	Automated Kind = "automated"
	// Unknown is the honest answer. Nothing scored high enough, or two kinds were
	// too close to separate. It is a first-class outcome, not a failure: mail
	// whose kind is unclear is exactly what the needs-a-category surface is for
	// (#176), and guessing here is how a categoriser loses a user's trust.
	Unknown Kind = "unknown"
)

// Facts is the header projection the classifier reads. It is deliberately
// header-only: no body, no bytes beyond what a mail server already had to parse.
// That keeps this cheap enough to run on every delivery, and it matches the
// privacy posture SaneBox ships (headers and behaviour only).
type Facts struct {
	FromAddress string // the From address, lowercased by the caller or not
	FromDomain  string
	FromName    string

	Subject string

	// ListID is List-Id (RFC 2919), ListUnsubscribe is List-Unsubscribe
	// (RFC 2369), and ListUnsubscribePost is List-Unsubscribe-Post (RFC 8058) —
	// the one-click header the 2024 bulk-sender rules made near-universal on
	// commercial marketing.
	ListID              string
	ListUnsubscribe     string
	ListUnsubscribePost string

	// AutoSubmitted is RFC 3834. Low coverage, HIGH precision: when a sender
	// bothers to set it, the mail really is machine-generated, and marketing
	// avoids it because it suppresses auto-replies they want.
	AutoSubmitted string
	// Precedence is the pre-standard bulk/list marker. Real but inconsistently
	// set, so it is worth a small score and never a decision.
	Precedence string
	// FeedbackID is Gmail's feedback-loop header: presence means a high-volume
	// ESP sender.
	FeedbackID string

	// DKIMDomains are the d= values from DKIM-Signature headers, and ReturnPath
	// is the envelope sender. Together they identify the sending INFRASTRUCTURE,
	// which is the highest-value signal available and the one Mailroom was not
	// using at all.
	DKIMDomains []string
	ReturnPath  string

	// HasCorresponded is true when the user has written to this sender, and
	// IsReply when the message is a reply in a thread. Both are strong personal
	// evidence and both come from the user's OWN behaviour — the per-user half
	// of the prior, available even on a cold start.
	HasCorresponded bool
	IsReply         bool
	// RecipientCount is To + Cc. Large fan-out is bulk; one recipient is not
	// evidence by itself, since transactional mail is also single-recipient.
	RecipientCount int
}

// Signal is one named test that fired, and how much it moved each kind. The
// list of fired signals IS the explanation shown to a user.
type Signal struct {
	Name string
	// Deltas is the score this signal contributes per kind.
	Deltas map[Kind]float64
}

// Verdict is the classifier's answer.
type Verdict struct {
	Kind Kind
	// Leader is the highest-scoring kind REGARDLESS of the thresholds — the kind
	// this would have been if it had cleared them. On a Kind of Unknown it is the
	// difference between "nearly decided, thresholds too strict" and "nothing
	// scored at all", which are opposite problems with opposite fixes.
	Leader Kind
	// Score is the winning kind's total, and Margin how far ahead of the
	// runner-up it finished. A large score with a small margin means "clearly
	// machine mail, unclear which sort" — which is Unknown, not a coin toss.
	Score  float64
	Margin float64
	// Scores is every kind's total, for the explanation panel.
	Scores map[Kind]float64
	// Signals are the tests that fired, strongest first.
	Signals []Signal
}

// decideMin is the score the leading kind must reach, and decideMargin how far
// clear of the runner-up it must finish, before the verdict is anything but
// Unknown.
//
// Both are deliberately conservative. The cost of a wrong kind is a wrong label
// on the user's mail, arrived at silently; the cost of Unknown is a message in a
// review queue they chose to open. Those are not symmetric, and the thresholds
// say so.
const (
	decideMin    = 1.5
	decideMargin = 0.75
)

// Classify judges a message from its headers.
func Classify(f Facts) Verdict {
	scores := map[Kind]float64{}
	var fired []Signal

	for _, s := range signals {
		if !s.test(f) {
			continue
		}
		sig := Signal{Name: s.name, Deltas: s.deltas}
		fired = append(fired, sig)
		for k, d := range s.deltas {
			scores[k] += d
		}
	}

	// Rank. Sorting by the absolute size of the largest delta puts the signals
	// that actually decided it at the top of the explanation.
	sort.SliceStable(fired, func(i, j int) bool {
		return maxAbs(fired[i].Deltas) > maxAbs(fired[j].Deltas)
	})

	kind, best, bestScore, margin := Decide(scores, decideMin, decideMargin)
	return Verdict{Kind: kind, Leader: best, Score: bestScore, Margin: margin, Scores: scores, Signals: fired}
}

// Decide turns per-kind scores into a verdict at the given thresholds. It is
// exported and parameterised so a THRESHOLD SWEEP over real mail runs the exact
// decision the serve path runs (mailroom#175): a sweep that could disagree with
// Classify would be measuring something nobody ships, which is worse than not
// measuring at all.
//
// It returns the decided kind (Unknown when the thresholds are not met), the
// LEADER regardless of thresholds, and the leader's score and margin. The leader
// matters on its own: a kind that never leads even at zero thresholds is missing
// its signals, and no threshold change will ever surface it — which is a
// completely different problem from one that leads but falls short.
func Decide(scores map[Kind]float64, min, margin float64) (decided, leader Kind, score, lead float64) {
	best, second := Unknown, Unknown
	bestScore, secondScore := 0.0, 0.0
	for _, k := range ranked {
		v := scores[k]
		if v > bestScore {
			second, secondScore = best, bestScore
			best, bestScore = k, v
		} else if v > secondScore {
			second, secondScore = k, v
		}
	}
	_ = second
	lead = bestScore - secondScore
	if bestScore >= min && lead >= margin {
		return best, best, bestScore, lead
	}
	return Unknown, best, bestScore, lead
}

// ranked fixes the iteration order so a tie is broken deterministically rather
// than by Go's randomised map order — two runs over the same message must never
// disagree.
var ranked = []Kind{Personal, Transactional, Promotional, Notification, List, Automated}

func maxAbs(m map[Kind]float64) float64 {
	out := 0.0
	for _, v := range m {
		if a := abs(v); a > out {
			out = a
		}
	}
	return out
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// rule is one scored test.
type rule struct {
	name   string
	test   func(Facts) bool
	deltas map[Kind]float64
}

// ---- the signal table ------------------------------------------------------
//
// Weights are hand-set, in the manner of coldstart.go, and are meant to be
// TUNED AGAINST REAL MAIL rather than argued about. `mailroom-admin classify`
// reports the distribution over an actual mailbox, which is the only honest way
// to set them (the lesson of #157, where a plausible-looking weight made a daily
// newsletter the user's closest correspondent).
//
// Sizes follow the evidence quality found in the design review:
//
//	~2.0  near-decisive on its own (Auto-Submitted, List-Id, marketing ESP)
//	~1.0  strong but not sufficient (one-click unsubscribe, correspondence)
//	~0.5  real but weak (Precedence, no-reply, fan-out)
//
// Anything folklore — X-Priority, Importance — is deliberately absent.
var signals = []rule{
	// -- machine-generated markers -------------------------------------------
	{
		// RFC 3834. High precision when present: marketing avoids it because it
		// suppresses the auto-replies they want, so it is a strong pull AWAY from
		// promotional and toward the transactional/notification side.
		//
		// This is `auto-generated` and anything else non-"no" — mail a machine
		// produced. `auto-replied` is a DIFFERENT thing and is handled below.
		name: "auto_submitted",
		test: func(f Facts) bool {
			v := autoSubmittedValue(f)
			return v != "" && v != "no" && !strings.HasPrefix(v, "auto-replied")
		},
		deltas: map[Kind]float64{Transactional: 1.2, Notification: 1.2, Promotional: -1.0, Personal: -2.0},
	},
	{
		// RFC 3834's OTHER value, and the one that makes `automated` a real kind
		// rather than a dead one: `auto-replied` means this message IS an
		// automatic reply — a vacation responder, a ticket acknowledgement.
		//
		// Before this, `automated` shared every signal it had with transactional
		// and notification and was scored LOWER on all of them, so it could not
		// lead on any message ever written; the sweep's leader table showed it at
		// a flat 0.0%. A kind that cannot win by construction is not a
		// conservative kind, it is a bug. (Bounces, the other classic member, are
		// recognised earlier and separately by internal/dsn — spec §4 — so they
		// never needed to be this kind's job.)
		name: "auto_replied",
		test: func(f Facts) bool {
			return strings.HasPrefix(autoSubmittedValue(f), "auto-replied")
		},
		deltas: map[Kind]float64{Automated: 2.2, Transactional: -0.5, Promotional: -1.0, Personal: -1.5},
	},
	{
		name:   "noreply_sender",
		test:   func(f Facts) bool { return isNoReply(f.FromAddress) },
		deltas: map[Kind]float64{Transactional: 0.5, Notification: 0.5, Automated: 0.5, Personal: -1.5},
	},

	// -- bulk markers ---------------------------------------------------------
	{
		// RFC 2369. Says BULK, and nothing finer: most of a real
		// mailbox carries it, so treating it as a promotional marker would sweep
		// in every newsletter and notification they get. It is here purely to
		// separate bulk from personal.
		name:   "list_unsubscribe",
		test:   func(f Facts) bool { return f.ListUnsubscribe != "" },
		deltas: map[Kind]float64{Promotional: 0.5, Notification: 0.4, List: 0.4, Personal: -2.0},
	},
	{
		// RFC 8058 one-click. It says REGULATED BULK SENDER — and, since the 2024
		// Gmail/Yahoo rules compelled every sender over 5k/day to emit it, that is
		// all it says. It was scored as a promotional marker at +1.2, which is the
		// exact mistake this file already calls out for List-Unsubscribe one rule
		// below: treating "is bulk" as "is marketing".
		//
		// The evidence is in real mail. A transactional notification sender
		// (a delivery-preview service, say) is high-volume, carries one-click
		// like everyone else, and was landing on the promotional side of every
		// comparison. Combined with feedback_id and VERP — also pure bulk
		// infrastructure — promotional accumulated enough from apparatus alone
		// to lead most of the mailbox without a single word of marketing being read.
		//
		// It still leans commercial, and strongly: transactional streams mostly
		// omit it (an unsubscribe must not be able to stop an order confirmation),
		// and on real mail most one-click senders genuinely are marketing.
		//
		// The first attempt at this fix cut it to 0.5 and MEASURED WORSE: unknown
		// 47% -> 56%, and Promotions — the one solidly bound label — fell from
		// 91%/85% coverage/purity to 50%/64%. Cutting promotional was the only
		// lever available while notification had no signal of its own; now that it
		// does, the two can compete on evidence instead, and a delivery-preview
		// sender reaches notification on its own subject rather than by promotional being
		// weakened out from under it. The lift belongs on the kind that deserves
		// it, not as a penalty on the kind that does not.
		name:   "one_click_unsubscribe",
		test:   func(f Facts) bool { return f.ListUnsubscribePost != "" },
		deltas: map[Kind]float64{Promotional: 1.2, Notification: 0.4, Transactional: -0.4, Personal: -1.5},
	},
	{
		// RFC 2919. Discussion lists and newsletter platforms; marketing mostly
		// omits it. In practice far fewer messages carry List-Id than
		// List-Unsubscribe, which is exactly the separation this exploits.
		name:   "list_id",
		test:   func(f Facts) bool { return f.ListID != "" },
		deltas: map[Kind]float64{List: 2.0, Promotional: -0.3, Personal: -1.5},
	},
	{
		// Pre-standard and inconsistently set. Worth a nudge, never a decision.
		name: "precedence_bulk",
		test: func(f Facts) bool {
			v := strings.ToLower(strings.TrimSpace(f.Precedence))
			return v == "bulk" || v == "list" || v == "junk"
		},
		deltas: map[Kind]float64{Promotional: 0.3, List: 0.3, Notification: 0.3, Personal: -1.0},
	},
	{
		// Gmail's feedback-loop header: a high-volume sender who has set up
		// complaint handling. Volume and professionalism, not intent — a bank's
		// alert stream runs the same apparatus as a retailer's campaigns.
		name:   "feedback_id",
		test:   func(f Facts) bool { return f.FeedbackID != "" },
		deltas: map[Kind]float64{Promotional: 0.3, Notification: 0.3, List: 0.2, Personal: -1.2},
	},
	{
		// A Return-Path whose domain differs from From and whose local part looks
		// like VERP/bounce-tracking is bulk infrastructure, whoever is sending —
		// which is why it is scored as bulk-not-personal and split evenly across
		// the machine kinds rather than read as marketing.
		name:   "verp_return_path",
		test:   func(f Facts) bool { return isVERP(f.ReturnPath, f.FromDomain) },
		deltas: map[Kind]float64{Promotional: 0.3, Notification: 0.3, List: 0.2, Transactional: 0.2, Personal: -1.2},
	},
	{
		// MEASURED AND LEFT ALONE (#175). Flagged in review as probably dead and
		// probably harmful — dead because bulk mail is delivered with one
		// recipient, harmful because a large human thread would be pushed away
		// from `personal`.
		//
		// Half confirmed: over a real mailbox, nearly every message has exactly one
		// recipient, so it fires on a tiny share of
		// mail. The other half is NOT confirmed and cannot be here — the single
		// firing was a phishing attempt, and that mailbox had no genuine human
		// correspondence at all, so the case where it would do damage does not
		// exist in the data.
		//
		// Removing a signal for being rare is not a fix, and re-weighting one on
		// evidence that cannot show the harm is how the two reverted tuning passes
		// happened. Recorded here so the next reader does not re-derive it.
		name:   "large_fanout",
		test:   func(f Facts) bool { return f.RecipientCount > 12 },
		deltas: map[Kind]float64{Promotional: 0.3, List: 0.3, Personal: -0.5},
	},

	// -- sending infrastructure (the highest-value untapped signal) -----------
	{
		name:   "esp_marketing",
		test:   func(f Facts) bool { return ESPClassOf(f) == ESPMarketing },
		deltas: map[Kind]float64{Promotional: 1.8, Personal: -1.5},
	},
	{
		name:   "esp_transactional",
		test:   func(f Facts) bool { return ESPClassOf(f) == ESPTransactional },
		deltas: map[Kind]float64{Transactional: 1.5, Notification: 0.8, Promotional: -0.5, Personal: -1.0},
	},
	{
		name:   "esp_newsletter",
		test:   func(f Facts) bool { return ESPClassOf(f) == ESPNewsletter },
		deltas: map[Kind]float64{List: 1.5, Promotional: 0.3, Personal: -1.0},
	},
	{
		// Mixed-purpose infrastructure (SendGrid, SparkPost, SES). Says
		// "machine-sent at volume" and nothing about WHAT, so it only pushes away
		// from personal — the honest reading.
		name:   "esp_mixed",
		test:   func(f Facts) bool { return ESPClassOf(f) == ESPMixed },
		deltas: map[Kind]float64{Personal: -1.2, Promotional: 0.2, Transactional: 0.2, Notification: 0.2},
	},
	{
		// The brand runs a segregated bulk stream (news.shop.example, mail.store.example).
		// Aligned DKIM has hidden WHO sends for them, but not THAT they have a
		// bulk stream — see esp.go's header for the measurement behind this.
		//
		// It fires only when the ESP table did not, so the two never stack: this
		// is the fallback for the 17-of-20 case the table cannot reach.
		name: "bulk_stream_subdomain",
		test: func(f Facts) bool {
			return ESPClassOf(f) == ESPNone && HasBulkStreamShape(f.FromDomain)
		},
		deltas: map[Kind]float64{Promotional: 0.8, Notification: 0.3, Personal: -1.2},
	},

	// -- subject lexicons ----------------------------------------------------
	{
		// Order numbers, receipts, tracking, verification codes. Good precision;
		// English-centric, which is why these live in one visible place.
		//
		// Weighted at 1.8 to match esp_marketing, because it is evidence of the
		// same strength: "Shipped: ..." names what the message IS as directly as a
		// campaign platform does. At 1.3 it could not clear decideMin ALONE, and
		// alone is exactly how it arrives — Amazon's dispatch notices carry no
		// List-Unsubscribe, no List-Id, no ESP and no order number in the subject,
		// so subject_transactional was the only rule firing for them and the
		// mailbox's most unambiguously transactional mail scored 1.3 against a
		// threshold of 1.5. Most Shopping messages sat in
		// unknown for want of 0.2.
		name:   "subject_transactional",
		test:   func(f Facts) bool { return transactionalSubject.MatchString(strings.ToLower(f.Subject)) },
		deltas: map[Kind]float64{Transactional: 1.8, Promotional: -0.5},
	},
	{
		name:   "subject_order_number",
		test:   func(f Facts) bool { return orderNumber.MatchString(f.Subject) },
		deltas: map[Kind]float64{Transactional: 0.8},
	},
	{
		name:   "subject_promotional",
		test:   func(f Facts) bool { return promotionalSubject.MatchString(strings.ToLower(f.Subject)) },
		deltas: map[Kind]float64{Promotional: 1.0, Transactional: -0.3},
	},
	{
		// NOTIFICATION HAD NO SIGNAL OF ITS OWN. Every other kind has something
		// that can carry it — an ESP class, a List-Id, a subject lexicon — while
		// notification only ever collected 0.2-0.4 crumbs from rules aimed
		// elsewhere. It led a tiny share of a real mailbox, where one
		// delivery-preview sender alone outnumbered it.
		//
		// A notification reports that something HAPPENED, without the user having
		// transacted: a digest, an alert, a statement being ready, activity on an
		// account. That is a distinct vocabulary from transactional ("your order
		// has shipped") and it is why `Information` — a typical label for
		// exactly this mail — sat at low coverage.
		name:   "subject_notification",
		test:   func(f Facts) bool { return notificationSubject.MatchString(strings.ToLower(f.Subject)) },
		deltas: map[Kind]float64{Notification: 1.3, Promotional: -0.4, Transactional: -0.2},
	},

	// -- the user's own behaviour (the per-user half of the prior) -----------
	{
		// The user has WRITTEN to this sender. The strongest personal signal
		// available, and it costs nothing to compute — it is the same
		// correspondence measure #157 established.
		name:   "has_corresponded",
		test:   func(f Facts) bool { return f.HasCorresponded },
		deltas: map[Kind]float64{Personal: 2.0, Promotional: -1.0},
	},
	{
		name:   "is_reply",
		test:   func(f Facts) bool { return f.IsReply },
		deltas: map[Kind]float64{Personal: 1.0, List: 0.3, Promotional: -0.5},
	},
	{
		// No bulk apparatus of any sort. Weak on its own — plenty of transactional
		// mail is equally unadorned — so it only leans personal.
		name: "no_bulk_markers",
		test: func(f Facts) bool {
			return f.ListUnsubscribe == "" && f.ListID == "" && f.Precedence == "" &&
				f.FeedbackID == "" && ESPClassOf(f) == ESPNone
		},
		deltas: map[Kind]float64{Personal: 1.0},
	},
}

// SubjectClass names one of the curated subject vocabularies. These are the
// only signals in this package that depend on NOTHING but the subject line.
type SubjectClass string

const (
	SubjectTransactional SubjectClass = "transactional"
	SubjectOrderNumber   SubjectClass = "ordernum"
	SubjectPromotional   SubjectClass = "promotional"
	SubjectNotification  SubjectClass = "notification"
)

// SubjectClasses reports which curated vocabularies a subject line matches, in a
// fixed order.
//
// IT EXISTS TO BE A MACHINE-LEARNING FEATURE (#169), which is a use this package
// was not written for, so it is worth saying why it belongs here rather than as
// a copy in internal/ml.
//
// The per-label classifier does not generalise across senders: measured by
// grouped 5-fold CV over a set of hand-labelled messages, every label except one
// swung 0.5–0.7 AUC depending on which senders were held out. Its subject signal
// is RAW TOKENS (`subtok:verge`, `subtok:patreon`), and raw tokens are partly
// sender identity in disguise — hold the sender out and its vocabulary goes with
// it. What the model lacks is an abstraction over surface forms: something that
// says "this is order-confirmation language" whoever wrote it.
//
// These lexicons are exactly that, already curated and already measured against
// real mail (#175). Duplicating them into the ml package would give the two
// layers separate ideas of what "promotional language" means, which would drift
// the moment either was tuned.
//
// SAFE ACROSS TRAIN AND SERVE, which is the constraint that decided the scope.
// The subject is a stored column, so this function returns the same answer on
// the ingest path and on the replay path. The rest of this package's signals —
// DKIM, Return-Path, Auto-Submitted, Feedback-ID — are readable only from the
// message bytes, so a model trained on them would see them absent in training
// and present in production. That is the train/serve gap #157 was fixed to
// close, and it is why only the subject rules are exposed.
func SubjectClasses(subject string) []SubjectClass {
	lower := strings.ToLower(subject)
	var out []SubjectClass
	if transactionalSubject.MatchString(lower) {
		out = append(out, SubjectTransactional)
	}
	if orderNumber.MatchString(subject) {
		out = append(out, SubjectOrderNumber)
	}
	if promotionalSubject.MatchString(lower) {
		out = append(out, SubjectPromotional)
	}
	if notificationSubject.MatchString(lower) {
		out = append(out, SubjectNotification)
	}
	return out
}

var (
	// `order(ed|s)?` and `deliver(y|ed|ies)`, not `order` and `delivered`. The
	// word-boundary forms missed the two commonest surface variants in real
	// mail: Amazon's confirmations say "Ordered", and carriers say "Out for
	// delivery" — which `\bdelivered\b` does not match. On a real
	// mailbox that was a handful of messages, most of them Shopping, i.e. a visible share of that label's
	// positives falling through the one lexicon that demonstrably helps it
	// (#175, #169).
	//
	// Cosmetic while this layer was wired to nothing; load-bearing now that
	// SubjectClasses is a classifier feature.
	transactionalSubject = regexp.MustCompile(
		`\b(order(ed|s)?|receipt|invoice|payment|shipped|shipping|deliver(y|ed|ies)|tracking|` +
			`confirm(ation|ed)?|` +
			`booking|reservation|itinerary|refund|verify|verification|password|sign[- ]?in|` +
			`security code|one[- ]time|statement|billing)\b`)
	promotionalSubject = regexp.MustCompile(
		`(\b\d{1,3}%\s*off\b|\bsale\b|\bdeal(s)?\b|\bsave\b|\bfree shipping\b|\bexclusive\b|` +
			`\blast chance\b|\blimited time\b|\bends (today|tonight|soon)\b|\bshop now\b|\bclearance\b|\bbogo\b)`)
	// Something HAPPENED and you are being told, without having transacted.
	// Deliberately excludes the transactional vocabulary above: "your order
	// shipped" is transactional, "your daily digest is ready" is not. English-
	// centric like the others, which is why they live together in one visible
	// place rather than being scattered through the rules.
	// Bare "update" and "now available" are deliberately ABSENT. Both were in the
	// first draft and both are newsletter boilerplate — "An update from us", "New
	// collection now available" — so they turned ambiguous bulk into confident
	// notifications. TestAmbiguousBulkIsUnknownRatherThanGuessed caught it, which
	// is exactly the failure that test exists to catch. The account-change sense
	// of "update" survives below in "has been updated", where it is unambiguous.
	notificationSubject = regexp.MustCompile(
		`\b(digest|alert|reminder|notification|summary|activity|` +
			`is ready|has been (added|changed|updated|posted)|` +
			`new (sign[- ]?in|device|login|message|comment|follower|post)|` +
			`expir(es|ed|ing)|renew(s|al|ing)?|due (soon|today|tomorrow)|` +
			`your (daily|weekly|monthly))\b`)
	// A run of digits long enough to be an order or tracking number rather than a
	// date or a quantity.
	orderNumber = regexp.MustCompile(`#?\b\d{6,}\b`)

	// Compiled ONCE. It used to be built inside isVERP, which runs per delivery —
	// regexp.MustCompile on every message for a pattern that never changes.
	opaqueLocalPart = regexp.MustCompile(`[0-9a-f]{12,}`)
)

// autoSubmittedValue normalises the Auto-Submitted header for comparison. RFC
// 3834 allows parameters after the keyword ("auto-replied; owner=..."), so the
// keyword is taken up to the first ";".
func autoSubmittedValue(f Facts) string {
	v := strings.ToLower(strings.TrimSpace(f.AutoSubmitted))
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// IsNoReply reports a local part that announces nobody is listening — the
// same test the noreply_sender signal runs, exported so the follow-up-nudge
// measurement (#58) excludes exactly the senders the classifier calls
// no-reply rather than a second list that could drift from this one.
func IsNoReply(addr string) bool { return isNoReply(addr) }

// isNoReply reports a local part that announces nobody is listening.
func isNoReply(addr string) bool {
	at := strings.Index(addr, "@")
	if at <= 0 {
		return false
	}
	local := strings.ToLower(addr[:at])
	local = strings.NewReplacer(".", "", "-", "", "_", "").Replace(local)
	for _, p := range []string{"noreply", "donotreply", "nevereply", "no-reply"} {
		if strings.Contains(local, strings.ReplaceAll(p, "-", "")) {
			return true
		}
	}
	return false
}

// isVERP reports a bounce-tracking envelope sender: a Return-Path whose domain
// differs from the From domain and whose local part carries an opaque token.
func isVERP(returnPath, fromDomain string) bool {
	rp := strings.Trim(strings.TrimSpace(returnPath), "<>")
	at := strings.LastIndex(rp, "@")
	if at <= 0 {
		return false
	}
	local, domain := strings.ToLower(rp[:at]), strings.ToLower(rp[at+1:])
	if domain == "" || fromDomain == "" || sameOrSubdomain(domain, strings.ToLower(fromDomain)) {
		return false
	}
	// An opaque local part: long, or carrying a long digit/hex run.
	return len(local) >= 20 || opaqueLocalPart.MatchString(local)
}

// sameOrSubdomain reports whether `domain` is `parent` or a subdomain of it.
//
// A BARE HasSuffix IS NOT THIS. `strings.HasSuffix("notexample.com",
// "example.com")` is true, so a Return-Path at an unrelated organisation whose
// domain merely ENDS with the From domain read as same-org — and isVERP's
// same-org check is an early return, so the effect was a silently missed VERP
// signal rather than a spurious one. Suffix matching on domains needs the label
// boundary or it is string matching pretending to be DNS.
func sameOrSubdomain(domain, parent string) bool {
	return domain == parent || strings.HasSuffix(domain, "."+parent)
}
