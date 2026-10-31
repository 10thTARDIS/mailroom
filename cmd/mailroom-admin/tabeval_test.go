// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"mailroom/internal/kind"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// tab-eval's numbers are the acceptance criterion for the whole tab classifier,
// which makes the SCORING itself something that has to be audited before its
// verdict is trusted. That is this issue's own lesson, stated in #169's thread
// and in CLAUDE.md: a measurement is a claim about the code that produced it,
// and every number the classification epic was judged on for a month came from
// an evaluation with four defects.
//
// So these tests are about the arithmetic, not about the classifier: what gets
// excluded from ground truth, whether precision and recall are the right way
// round, and whether an absent measurement is distinguishable from a bad one.

// evalMsg builds a replay row. `mapped` is the ground truth (the tabs this
// message's labels point at); `labels` is how many labels it carries.
func evalMsg(from string, labels int, mapped []tabs.Tab, f kind.Facts) store.MessageTabFacts {
	f.FromAddress = from
	if at := len(from) - 1; at >= 0 {
		for i := len(from) - 1; i >= 0; i-- {
			if from[i] == '@' {
				f.FromDomain = from[i+1:]
				break
			}
		}
	}
	return store.MessageTabFacts{ID: store.NewULID(), Facts: f, Labels: labels, Mapped: mapped}
}

func TestTabEvalExcludesWhatItCannotScore(t *testing.T) {
	e := newTabEval()
	// Unlabelled — the classifier's real workload, but nothing to score against.
	e.record(evalMsg("a@example.com", 0, nil, kind.Facts{Subject: "hi"}))
	// Labelled, but the label maps to no tab — a mapping gap, not a classifier
	// problem, and counted apart from the line above for that reason.
	e.record(evalMsg("b@example.com", 1, nil, kind.Facts{Subject: "hi"}))
	// Ambiguous: labels spanning two tabs. Every answer is both right and wrong,
	// so scoring it would corrupt the per-tab precision the report exists for.
	e.record(evalMsg("c@example.com", 2, []tabs.Tab{tabs.Promotions, tabs.Updates},
		kind.Facts{Subject: "hi"}))
	// Scorable.
	e.record(evalMsg("d@example.com", 1, []tabs.Tab{tabs.Primary}, kind.Facts{Subject: "hi"}))

	if e.scanned != 4 {
		t.Errorf("scanned = %d, want 4", e.scanned)
	}
	if e.truth != 1 {
		t.Errorf("ground truth = %d, want 1", e.truth)
	}
	if e.unlabelled != 1 {
		t.Errorf("unlabelled = %d, want 1", e.unlabelled)
	}
	if e.labelUnmapped != 1 {
		t.Errorf("labelUnmapped = %d, want 1 — a mapping gap is not an unlabelled message", e.labelUnmapped)
	}
	if e.ambiguousLabels != 1 {
		t.Errorf("ambiguousLabels = %d, want 1 — a message spanning two tabs must be excluded, not resolved",
			e.ambiguousLabels)
	}
	// Every scanned message is accounted for by exactly one bucket.
	if got := e.truth + e.unlabelled + e.labelUnmapped + e.ambiguousLabels; got != e.scanned {
		t.Errorf("buckets sum to %d, want the %d scanned — a message fell through the report",
			got, e.scanned)
	}
}

// TestTabEvalPrecisionAndRecallAreNotSwapped builds a deliberately lopsided
// confusion and checks both numbers against hand arithmetic. Getting these the
// wrong way round is the classic silent evaluation defect: both are ratios in
// [0,1], both look plausible, and swapping them flatters exactly the tab that is
// over-predicted.
func TestTabEvalPrecisionAndRecallAreNotSwapped(t *testing.T) {
	promo := []tabs.Tab{tabs.Promotions}
	updates := []tabs.Tab{tabs.Updates}
	// Classified Promotions (a marketing platform, no transactional language).
	marketing := kind.Facts{Subject: "New season, new styles", DKIMDomains: []string{"mcsv.net"}}
	// Classified Updates (a receipt).
	receipt := kind.Facts{Subject: "Your order has shipped"}

	e := newTabEval()
	// 3 true Promotions, all predicted Promotions.
	for i := 0; i < 3; i++ {
		e.record(evalMsg("shop@a.example", 1, promo, marketing))
	}
	// 2 true Updates, both predicted Promotions — Promotions is over-predicted.
	for i := 0; i < 2; i++ {
		e.record(evalMsg("shop@b.example", 1, updates, marketing))
	}
	// 5 true Updates, all predicted Updates.
	for i := 0; i < 5; i++ {
		e.record(evalMsg("shop@c.example", 1, updates, receipt))
	}

	// Promotions: predicted 5, of which 3 correct → precision 3/5. True 3, of
	// which 3 found → recall 3/3.
	if got := metric(e.tpByTab[tabs.Promotions], e.predByTab[tabs.Promotions]); got != "0.600" {
		t.Errorf("promotions precision = %s, want 0.600 (3 of 5 predicted)", got)
	}
	if got := metric(e.tpByTab[tabs.Promotions], e.truthByTab[tabs.Promotions]); got != "1.000" {
		t.Errorf("promotions recall = %s, want 1.000 (3 of 3 true)", got)
	}
	// Updates: predicted 5, all correct → precision 1.000. True 7, 5 found →
	// recall 5/7.
	if got := metric(e.tpByTab[tabs.Updates], e.predByTab[tabs.Updates]); got != "1.000" {
		t.Errorf("updates precision = %s, want 1.000", got)
	}
	if got := metric(e.tpByTab[tabs.Updates], e.truthByTab[tabs.Updates]); got != "0.714" {
		t.Errorf("updates recall = %s, want 0.714 (5 of 7 true)", got)
	}
	// And the confusion cell #219 says to read.
	if got := e.confusion[[2]tabs.Tab{tabs.Updates, tabs.Promotions}]; got != 2 {
		t.Errorf("updates-misread-as-promotions = %d, want 2", got)
	}
}

// TestTabEvalMetricDistinguishesAbsentFromZero. A rate over an empty
// denominator is not 0.000 — printing it as a number invites a reader to see a
// measurement that was never taken as a bad result, which is precisely how
// "Forums recall 0" would be misread as a broken tab (#155's lower-bound
// lesson, applied to a rate).
func TestTabEvalMetricDistinguishesAbsentFromZero(t *testing.T) {
	if got := metric(0, 0); got != "—" {
		t.Errorf("metric(0,0) = %q, want an em dash: no measurement was taken", got)
	}
	if got := metric(0, 7); got != "0.000" {
		t.Errorf("metric(0,7) = %q, want 0.000: the measurement was taken and it is zero", got)
	}
}

// TestTabEvalRuleAttributionSeparatesFiredFromScored. `fired` counts every
// message; `scored` counts only those with ground truth. Merging them would let
// a rule that fires constantly on unlabelled mail look well-evidenced.
func TestTabEvalRuleAttributionSeparatesFiredFromScored(t *testing.T) {
	receipt := kind.Facts{Subject: "Your receipt from us"}
	e := newTabEval()
	e.record(evalMsg("a@x.example", 0, nil, receipt))                         // fired, not scored
	e.record(evalMsg("b@x.example", 1, []tabs.Tab{tabs.Updates}, receipt))    // fired, scored, correct
	e.record(evalMsg("c@y.example", 1, []tabs.Tab{tabs.Promotions}, receipt)) // fired, scored, wrong

	r := tabs.RuleTransactionalSubject
	if e.ruleFired[r] != 3 {
		t.Errorf("fired = %d, want 3", e.ruleFired[r])
	}
	if e.ruleScored[r] != 2 {
		t.Errorf("scored = %d, want 2 — an unlabelled message cannot be scored", e.ruleScored[r])
	}
	if got := metric(e.ruleCorrect[r], e.ruleScored[r]); got != "0.500" {
		t.Errorf("rule precision = %s, want 0.500", got)
	}
	// Distinct senders, which is what stops a high precision over a tiny sender
	// pool from reading as evidence.
	if got := len(e.ruleSenders[r]); got != 3 {
		t.Errorf("distinct senders = %d, want 3", got)
	}
}

// TestTabEvalListIDReconciliationCountsEveryMessage. The reader's question is
// "which of my mail is RFC list traffic", and answering it only for mail they
// have already labelled would under-report the very population they are being
// asked to reconsider.
func TestTabEvalListIDReconciliationCountsEveryMessage(t *testing.T) {
	list := kind.Facts{ListID: "Foo Discussion <foo.example.org>", Subject: "[foo] hello"}
	other := kind.Facts{ListID: "<bar.example.org>", Subject: "[bar] hi"}
	e := newTabEval()
	e.record(evalMsg("a@x.example", 0, nil, list))                      // unlabelled
	e.record(evalMsg("b@x.example", 1, []tabs.Tab{tabs.Updates}, list)) // mapped to Updates
	e.record(evalMsg("c@x.example", 1, []tabs.Tab{tabs.Promotions}, other))

	if e.listIDTotal != 3 {
		t.Errorf("List-Id total = %d, want 3 (including the unlabelled one)", e.listIDTotal)
	}
	if e.listIDByTab[tabs.Updates] != 1 || e.listIDByTab[tabs.Promotions] != 1 {
		t.Errorf("mapping breakdown = %v, want one Updates and one Promotions", e.listIDByTab)
	}
	// `Foo Discussion <foo.example.org>` and a bare `<foo.example.org>` are one
	// list, not two, or the reconciliation list double-counts.
	if n := e.listIDNames["foo.example.org"]; n != 2 {
		t.Errorf("foo.example.org counted %d times, want 2 — the bracketed id is the identity", n)
	}
	if len(e.listIDNames) != 2 {
		t.Errorf("distinct lists = %d, want 2: %v", len(e.listIDNames), e.listIDNames)
	}
}

// TestProbeSeparationNamesTheClassesItCompares reproduces the defect the probe
// shipped with, using the shape that exposed it on a real corpus:
// promotions 93, updates 74, social 10.
//
// The defect was NOT the arithmetic — max-minus-min over the classes is the
// largest pairwise difference, so the number was right. It was that the number
// was printed with nothing saying which two classes it described.
// `bulk_stream_subdomain` headlined 55.4%, which reads as the separation between
// the two tabs the rule is failing on; that pair actually differs by 16.7
// points, in the wrong direction, and the 55.4% belonged to a ten-message class.
//
// So what this pins is the LABEL, not the value.
func TestProbeSeparationNamesTheClassesItCompares(t *testing.T) {
	e := newTabEval()
	e.probeTotal = map[tabs.Tab]int{tabs.Promotions: 93, tabs.Updates: 74, tabs.Social: 10}
	e.probeSignals = map[tabs.Tab]map[string]int{
		tabs.Promotions: {"bulk_stream_subdomain": 36}, // 38.7%
		tabs.Updates:    {"bulk_stream_subdomain": 41}, // 55.4%
		tabs.Social:     {"bulk_stream_subdomain": 0},  // 0%
	}
	present := []tabs.Tab{tabs.Social, tabs.Promotions, tabs.Updates}

	a, b, gap := e.bestPairGap("bulk_stream_subdomain", present)
	// The largest separation really is social-vs-updates here, and that is fine —
	// what matters is that the pair is NAMED, so a 55-point number cannot be read
	// as separating promotions from updates.
	if a == tabs.Promotions && b == tabs.Updates {
		t.Errorf("named promotions vs updates with gap %.3f; that pair differs by ~0.167, not the headline", gap)
	}
	if a != tabs.Social && b != tabs.Social {
		t.Errorf("best pair = %s vs %s; the widest gap involves the social class", a, b)
	}
	// And the pair that matters, asked for directly, reports its real separation.
	_, _, pu := e.bestPairGap("bulk_stream_subdomain", []tabs.Tab{tabs.Promotions, tabs.Updates})
	if pu < 0.16 || pu > 0.18 {
		t.Errorf("promotions vs updates separation = %.3f, want ~0.167", pu)
	}
}

// TestProbeSeparationFlagsThinClasses: the named pair drives the ⚠ marker, so a
// separation resting on ten messages is visibly a hypothesis rather than a
// finding. Same discipline as never printing a precision without its sender
// count.
func TestProbeSeparationFlagsThinClasses(t *testing.T) {
	e := newTabEval()
	e.probeTotal = map[tabs.Tab]int{tabs.Promotions: 93, tabs.Social: 10}
	e.probeSignals = map[tabs.Tab]map[string]int{
		tabs.Promotions: {"sig": 0},
		tabs.Social:     {"sig": 10},
	}
	a, b, gap := e.bestPairGap("sig", []tabs.Tab{tabs.Promotions, tabs.Social})
	if gap != 1.0 {
		t.Errorf("gap = %.3f, want 1.0", gap)
	}
	if e.probeTotal[a] >= probeMinClass && e.probeTotal[b] >= probeMinClass {
		t.Errorf("pair %s vs %s would not be flagged, but social has %d messages",
			a, b, e.probeTotal[tabs.Social])
	}
}

// TestProbeSeparationIsSymmetric — a signal absent from one class and universal
// in another separates them completely whichever way round the classes are
// listed. A signed subtraction would report a negative gap for half the table
// and sort those rows to the bottom as though they were useless.
func TestProbeSeparationIsSymmetric(t *testing.T) {
	e := newTabEval()
	e.probeTotal = map[tabs.Tab]int{tabs.Promotions: 50, tabs.Updates: 50}
	e.probeSignals = map[tabs.Tab]map[string]int{
		tabs.Promotions: {"sig": 0},
		tabs.Updates:    {"sig": 50},
	}
	_, _, forward := e.bestPairGap("sig", []tabs.Tab{tabs.Promotions, tabs.Updates})
	_, _, reverse := e.bestPairGap("sig", []tabs.Tab{tabs.Updates, tabs.Promotions})
	if forward != 1.0 || reverse != 1.0 {
		t.Errorf("gap = %.3f forward, %.3f reversed; want 1.0 both ways", forward, reverse)
	}
}

// TestMajorityTabRefusesToBreakATie. A sender split evenly between two tabs has
// no single answer, and inventing one would silently improve or damage whichever
// tab the map happened to yield first — a result that changes between runs.
func TestMajorityTabRefusesToBreakATie(t *testing.T) {
	if _, ok := majorityTab(map[tabs.Tab]int{tabs.Updates: 2, tabs.Promotions: 2}); ok {
		t.Error("an even split produced a majority tab")
	}
	if _, ok := majorityTab(map[tabs.Tab]int{}); ok {
		t.Error("an empty count produced a majority tab")
	}
	got, ok := majorityTab(map[tabs.Tab]int{tabs.Updates: 3, tabs.Promotions: 2})
	if !ok || got != tabs.Updates {
		t.Errorf("majorityTab = (%q, %v), want updates, true", got, ok)
	}
}

// TestTabEvalSenderMetricsAreNotMessageMetrics is the point of the sender-level
// table: one sender with many messages must not be able to carry a tab. Here
// Promotions is perfect on 40 messages from ONE sender and wrong on two other
// senders — excellent per-message, poor per-sender, and the report has to show
// both or the first number is misleading on its own.
func TestTabEvalSenderMetricsAreNotMessageMetrics(t *testing.T) {
	promo := []tabs.Tab{tabs.Promotions}
	updates := []tabs.Tab{tabs.Updates}
	marketing := kind.Facts{Subject: "New season, new styles", DKIMDomains: []string{"mcsv.net"}}

	e := newTabEval()
	for i := 0; i < 40; i++ {
		e.record(evalMsg("bulk@one.example", 1, promo, marketing))
	}
	e.record(evalMsg("a@two.example", 1, updates, marketing))
	e.record(evalMsg("b@three.example", 1, updates, marketing))

	// Per message: 40 of 42 correct.
	if e.correct != 40 || e.truth != 42 {
		t.Fatalf("per-message = %d/%d, want 40/42", e.correct, e.truth)
	}
	// Per sender: 1 of 3 correct. The classifier is right about ONE sender.
	right, total := 0, 0
	for s, tc := range e.senderTruth {
		truth, tOK := majorityTab(tc)
		pred, pOK := majorityTab(e.senderPred[s])
		if !tOK || !pOK {
			continue
		}
		total++
		if truth == pred {
			right++
		}
	}
	if total != 3 || right != 1 {
		t.Errorf("per-sender = %d/%d, want 1/3 — 95%% of messages and a third of senders are different claims",
			right, total)
	}
}
