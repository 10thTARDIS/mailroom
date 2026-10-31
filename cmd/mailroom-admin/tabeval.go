// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/kind"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// `mailroom-admin tab-eval` — the tab classifier's ACCEPTANCE TEST, replayed
// over a real corpus (mailroom#219).
//
// It replays every message, decides its tab from the function facts alone, and
// scores that against the operator's own label→tab mapping. Read-only: it writes
// nothing, and it is the first fair version of this test the epic has run,
// because the facts are sender-independent by construction — there is no
// training set for a sender to leak out of.
//
// WHAT THIS REPORT REFUSES TO DO, and why each refusal is load-bearing:
//
//  1. **No aggregate score.** Per-tab precision and recall, separately, always.
//     #219 requires it because Forums is EXPECTED to score 0 recall against
//     today's mapping — all list traffic is mapped elsewhere — and an aggregate
//     would let that drag a total that reads as "the classifier is bad".
//  2. **No silent exclusions.** Ground truth is only mail that is labelled AND
//     whose label is mapped. Everything left out is counted and named, because a
//     bounded result that does not report its bound reads as a total (#155), and
//     because zero rows can be a design finding rather than an empty result
//     (#176). If ground truth is empty the report says so loudly and stops
//     rather than printing a wall of 0.000 that looks like a failed classifier.
//  3. **No tab-level number without its per-rule decomposition.** "Updates
//     precision 0.8" means something completely different if most of it arrives
//     via `bulk_apparatus` — the residue rule that only knows the mail is
//     machine-sent — than via the transactional lexicon. Reporting only the tab
//     would hide the single number that says whether the Promotions/Updates
//     boundary is actually being separated.
//  4. **No precision without its sender count.** A rule that scores 1.000 across
//     three senders has demonstrated nothing. This matters most for
//     `social_platform`, the one rule that is a curated name list and therefore
//     the one that could have been fitted to this corpus.
//  5. **No message-level metric without its sender-level twin.** Every message
//     metric is repeated macro-averaged over senders, each sender counting once.
//     A mailbox where one sender is 45 messages can look excellent per-message
//     and be carried entirely by that sender. For a classifier that never
//     trains, this — not a holdout — is what "senders held out" has to mean:
//     there is no training set to hold anything out of, so the honest question
//     is whether the result survives one sender ceasing to dominate.
func runTabEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tab-eval", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
		listIDs   = fs.Int("list-ids", 40, "how many distinct List-Ids to print in the reconciliation list")
		probe     = fs.String("probe", "", "for the population ONE rule decided, report candidate-signal rates split by the TRUE tab (e.g. bulk_apparatus)")
		samples   = fs.Int("samples", 8, "example subjects per true tab under --probe")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	ev := newTabEval()
	ev.probeRule = strings.TrimSpace(*probe)
	ev.probeSampleN = *samples
	after := ulid.ULID{}
	for {
		var page []store.MessageTabFacts
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageTabFactsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("scan message tab facts: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			after = m.ID
			ev.record(m)
		}
		if len(page) < *batch {
			break
		}
	}

	ev.print(*listIDs)
	return nil
}

// tabEval accumulates the replay. Separated from the I/O so the scoring is
// testable without a database — the same split as functionSignalStats.
type tabEval struct {
	scanned int
	// The excluded populations, counted separately because they have different
	// meanings and different fixes.
	unlabelled      int
	labelUnmapped   int
	ambiguousLabels int

	truth   int // messages in ground truth
	correct int

	// Per-tab confusion over ground-truth messages.
	truthByTab map[tabs.Tab]int
	predByTab  map[tabs.Tab]int
	tpByTab    map[tabs.Tab]int
	confusion  map[[2]tabs.Tab]int

	// Per-rule: how often it fired at all, over how many distinct senders, and
	// how it scored where there is ground truth to score against.
	ruleFired   map[string]int
	ruleSenders map[string]map[string]bool
	ruleScored  map[string]int
	ruleCorrect map[string]int
	ruleTab     map[string]tabs.Tab

	// Per sender, the ground-truth and predicted tab counts, for the sender-level
	// metrics.
	senderTruth map[string]map[tabs.Tab]int
	senderPred  map[string]map[tabs.Tab]int

	// The List-Id reconciliation population: mail the RFC calls list traffic,
	// against where the operator's mapping actually puts it.
	listIDTotal int
	listIDByTab map[tabs.Tab]int
	listIDNames map[string]int

	// --probe: for the population ONE rule decided, the rate of each candidate
	// signal split by the message's TRUE tab. See probeReport below for why the
	// split is the whole point.
	probeRule    string
	probeSampleN int
	probeTotal   map[tabs.Tab]int
	probeSignals map[tabs.Tab]map[string]int
	probeDomains map[tabs.Tab]map[string]int
	probeSamples map[tabs.Tab][]string
}

func newTabEval() *tabEval {
	return &tabEval{
		truthByTab:   map[tabs.Tab]int{},
		predByTab:    map[tabs.Tab]int{},
		tpByTab:      map[tabs.Tab]int{},
		confusion:    map[[2]tabs.Tab]int{},
		ruleFired:    map[string]int{},
		ruleSenders:  map[string]map[string]bool{},
		ruleScored:   map[string]int{},
		ruleCorrect:  map[string]int{},
		ruleTab:      map[string]tabs.Tab{},
		senderTruth:  map[string]map[tabs.Tab]int{},
		senderPred:   map[string]map[tabs.Tab]int{},
		listIDByTab:  map[tabs.Tab]int{},
		listIDNames:  map[string]int{},
		probeTotal:   map[tabs.Tab]int{},
		probeSignals: map[tabs.Tab]map[string]int{},
		probeDomains: map[tabs.Tab]map[string]int{},
		probeSamples: map[tabs.Tab][]string{},
	}
}

// candidateSignals names every cheap fact that MIGHT separate the tabs a rule
// currently lumps together. Nothing here is new parsing — it is all already
// computable from the stored row, which is the point: if a discriminator exists
// among these, it costs one rule to exploit; if none does, the answer is not
// "try harder with the same inputs".
func candidateSignals(f kind.Facts) []string {
	var out []string
	add := func(name string, on bool) {
		if on {
			out = append(out, name)
		}
	}
	add("bulk_stream_subdomain", kind.HasBulkStreamShape(f.FromDomain))
	switch kind.ESPClassOf(f) {
	case kind.ESPMarketing:
		add("esp_marketing", true)
	case kind.ESPTransactional:
		add("esp_transactional", true)
	case kind.ESPNewsletter:
		add("esp_newsletter", true)
	case kind.ESPMixed:
		add("esp_mixed", true)
	default:
		add("esp_unrecognised", true)
	}
	for _, c := range kind.SubjectClasses(f.Subject) {
		add("subject_"+string(c), true)
	}
	add("one_click", strings.TrimSpace(f.ListUnsubscribePost) != "")
	add("list_unsubscribe", strings.TrimSpace(f.ListUnsubscribe) != "")
	add("feedback_id", strings.TrimSpace(f.FeedbackID) != "")
	add("precedence", strings.TrimSpace(f.Precedence) != "")
	add("return_path", strings.TrimSpace(f.ReturnPath) != "")
	add("dkim_signed", len(f.DKIMDomains) > 0)
	add("dkim_multi_domain", len(f.DKIMDomains) > 1)
	// DKIM alignment: does the signing domain differ from the From domain? An
	// unaligned d= means somebody else sent it, which is what the ESP table tries
	// to name and usually cannot (see internal/kind/esp.go's header).
	if len(f.DKIMDomains) > 0 && f.FromDomain != "" {
		aligned := false
		for _, d := range f.DKIMDomains {
			if d == strings.ToLower(f.FromDomain) || strings.HasSuffix(strings.ToLower(f.FromDomain), "."+d) {
				aligned = true
			}
		}
		add("dkim_unaligned", !aligned)
	}
	add("noreply_sender", strings.Contains(strings.ToLower(f.FromAddress), "noreply") ||
		strings.Contains(strings.ToLower(f.FromAddress), "no-reply") ||
		strings.Contains(strings.ToLower(f.FromAddress), "donotreply"))
	return out
}

func (e *tabEval) record(m store.MessageTabFacts) {
	e.scanned++
	sender := strings.ToLower(strings.TrimSpace(m.Facts.FromAddress))

	v := tabs.Classify(m.Facts)
	e.ruleFired[v.Rule]++
	e.ruleTab[v.Rule] = v.Tab
	if e.ruleSenders[v.Rule] == nil {
		e.ruleSenders[v.Rule] = map[string]bool{}
	}
	e.ruleSenders[v.Rule][sender] = true

	// The List-Id reconciliation population is measured over EVERY message, not
	// only ground truth: the question is "which of my mail is RFC list
	// traffic", and answering it only for mail they have already labelled would
	// under-report the thing they are being asked to reconsider.
	if id := strings.TrimSpace(m.Facts.ListID); id != "" {
		e.listIDTotal++
		e.listIDNames[normaliseListID(id)]++
		if truth, _, ok := tabs.FromLabels(m.Mapped); ok {
			e.listIDByTab[truth]++
		}
	}

	truth, contested, ok := tabs.FromLabels(m.Mapped)
	if !ok {
		if m.Labels == 0 {
			e.unlabelled++
		} else {
			e.labelUnmapped++
		}
		return
	}
	// AMBIGUOUS GROUND TRUTH IS EXCLUDED, not resolved. Scoring against a message
	// that is simultaneously two tabs is meaningless — every answer is both right
	// and wrong — and silently including it would corrupt exactly the per-tab
	// precision figures this report exists to produce (#219). Expected to be zero
	// since #223; counted anyway, because "expected to be zero" is how a
	// regression hides.
	if len(contested) > 0 {
		e.ambiguousLabels++
		return
	}

	e.truth++
	e.truthByTab[truth]++
	e.predByTab[v.Tab]++
	e.confusion[[2]tabs.Tab{truth, v.Tab}]++
	e.ruleScored[v.Rule]++
	if v.Tab == truth {
		e.correct++
		e.tpByTab[truth]++
		e.ruleCorrect[v.Rule]++
	}

	if e.senderTruth[sender] == nil {
		e.senderTruth[sender] = map[tabs.Tab]int{}
		e.senderPred[sender] = map[tabs.Tab]int{}
	}
	e.senderTruth[sender][truth]++
	e.senderPred[sender][v.Tab]++

	// --probe: accumulate candidate-signal rates for the rule under examination,
	// keyed by the message's TRUE tab.
	if e.probeRule != "" && v.Rule == e.probeRule {
		e.probeTotal[truth]++
		if e.probeSignals[truth] == nil {
			e.probeSignals[truth] = map[string]int{}
			e.probeDomains[truth] = map[string]int{}
		}
		for _, s := range candidateSignals(m.Facts) {
			e.probeSignals[truth][s]++
		}
		if d := kind.SendingDomain(m.Facts); d != "" {
			e.probeDomains[truth][d]++
		} else if m.Facts.FromDomain != "" {
			e.probeDomains[truth]["(from) "+m.Facts.FromDomain]++
		}
		if len(e.probeSamples[truth]) < e.probeSampleN {
			e.probeSamples[truth] = append(e.probeSamples[truth],
				fmt.Sprintf("%-38s %s", trunc(m.Facts.FromAddress, 38), trunc(m.Facts.Subject, 60)))
		}
	}
}

// normaliseListID reduces a List-Id header to its bracketed identifier where
// there is one, so `Foo Discussion <foo.example.com>` and `<foo.example.com>`
// count as one list rather than two.
func normaliseListID(v string) string {
	if l := strings.Index(v, "<"); l >= 0 {
		if r := strings.Index(v[l:], ">"); r > 0 {
			return strings.ToLower(v[l+1 : l+r])
		}
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func (e *tabEval) print(listIDLimit int) {
	fmt.Println("=== tab-eval (#219): the tab classifier replayed over this corpus ===")
	fmt.Println("Read-only. Ground truth is the operator's own label→tab mapping.")
	fmt.Println()

	fmt.Println("CORPUS, and what is NOT scored")
	fmt.Printf("  messages scanned                              : %6d\n", e.scanned)
	fmt.Printf("  GROUND TRUTH (labelled AND its label mapped)  : %6d   <- everything below is scored on this\n", e.truth)
	fmt.Printf("  excluded: unlabelled                          : %6d\n", e.unlabelled)
	fmt.Printf("  excluded: labelled, but no label maps to a tab: %6d\n", e.labelUnmapped)
	fmt.Printf("  excluded: labels span more than one tab       : %6d\n", e.ambiguousLabels)
	fmt.Println()

	if e.truth == 0 {
		fmt.Println("⚠ GROUND TRUTH IS EMPTY — NOTHING BELOW WOULD MEAN ANYTHING, so it is not printed.")
		fmt.Println()
		fmt.Println("  This is almost certainly not a classifier failure. Ground truth needs labels")
		fmt.Println("  that are MAPPED to a tab, and the label→tab mapping is per-user DATA (#219,")
		fmt.Println("  ADR-021) — no migration seeds it, deliberately, because it would hard-code one")
		fmt.Println("  operator's label names into the schema.")
		fmt.Println()
		fmt.Println("  Set it with Label/set (the `tab` property), then re-run. `mailroom-admin tabs`")
		fmt.Println("  shows which labels are still unmapped.")
		if e.labelUnmapped > 0 {
			fmt.Printf("\n  %d labelled messages are waiting on exactly that.\n", e.labelUnmapped)
		}
		return
	}

	acc := ratio(e.correct, e.truth)
	fmt.Printf("OVERALL AGREEMENT WITH THE MAPPING: %d/%d (%.1f%%)\n", e.correct, e.truth, 100*acc)
	fmt.Println("  Reported once, as context. It is NOT the acceptance criterion — a single")
	fmt.Println("  number cannot distinguish a tab that works from a tab that is empty, and")
	fmt.Println("  Forums is expected to drag it down for a reason that is not a defect.")
	fmt.Println()

	fmt.Println("PER TAB, OVER MESSAGES")
	fmt.Printf("  %-12s %6s %6s %6s %6s %6s %10s %8s\n",
		"TAB", "TRUTH", "PRED", "TP", "FP", "FN", "PRECISION", "RECALL")
	for _, t := range tabs.All {
		tp := e.tpByTab[t]
		fp := e.predByTab[t] - tp
		fn := e.truthByTab[t] - tp
		fmt.Printf("  %-12s %6d %6d %6d %6d %6d %10s %8s\n",
			t, e.truthByTab[t], e.predByTab[t], tp, fp, fn,
			metric(tp, e.predByTab[t]), metric(tp, e.truthByTab[t]))
	}
	fmt.Println()
	fmt.Println("  FORUMS RECALL 0 IS EXPECTED, not a broken tab: List-Id is the strongest")
	fmt.Println("  function signal there is, and the operator's mapping currently routes every")
	fmt.Println("  labelled list message to Updates or Promotions. See the reconciliation list")
	fmt.Println("  at the end — that disagreement is the report's OUTPUT, not its error.")
	fmt.Println()

	e.printSenderLevel()
	e.printConfusion()
	e.printRules()
	e.printListIDReconciliation(listIDLimit)
	e.printProbe()
}

// printSenderLevel repeats the per-tab metrics with each SENDER counting once.
//
// For a classifier that never trains there is no holdout to construct, so this is
// what sender-independence has to be measured as: a sender's true tab is the tab
// most of its ground-truth mail maps to, its predicted tab is the classifier's
// majority verdict, and the metrics are computed over senders. A tab that looks
// strong per-message and weak here is being carried by one high-volume sender.
func (e *tabEval) printSenderLevel() {
	truthByTab := map[tabs.Tab]int{}
	predByTab := map[tabs.Tab]int{}
	tpByTab := map[tabs.Tab]int{}
	var senders, split int

	for s, tc := range e.senderTruth {
		truth, tOK := majorityTab(tc)
		pred, pOK := majorityTab(e.senderPred[s])
		if !tOK || !pOK {
			// A sender whose own mail is split evenly across tabs has no single
			// answer; counted and excluded rather than broken arbitrarily.
			split++
			continue
		}
		senders++
		truthByTab[truth]++
		predByTab[pred]++
		if truth == pred {
			tpByTab[truth]++
		}
	}

	fmt.Printf("PER TAB, OVER SENDERS (each sender counts once; %d senders, %d excluded as evenly split)\n",
		senders, split)
	fmt.Printf("  %-12s %6s %6s %6s %10s %8s\n", "TAB", "TRUTH", "PRED", "TP", "PRECISION", "RECALL")
	for _, t := range tabs.All {
		fmt.Printf("  %-12s %6d %6d %6d %10s %8s\n",
			t, truthByTab[t], predByTab[t], tpByTab[t],
			metric(tpByTab[t], predByTab[t]), metric(tpByTab[t], truthByTab[t]))
	}
	fmt.Println("  A tab that is strong above and weak here was carried by one high-volume sender.")
	fmt.Println()
}

func (e *tabEval) printConfusion() {
	fmt.Println("CONFUSION (rows = the mapping's answer, columns = the classifier's)")
	fmt.Printf("  %-12s", "TRUTH \\ PRED")
	for _, t := range tabs.All {
		fmt.Printf(" %10s", t)
	}
	fmt.Println()
	for _, truth := range tabs.All {
		fmt.Printf("  %-12s", truth)
		for _, pred := range tabs.All {
			fmt.Printf(" %10d", e.confusion[[2]tabs.Tab{truth, pred}])
		}
		fmt.Println()
	}
	pu := e.confusion[[2]tabs.Tab{tabs.Promotions, tabs.Updates}]
	up := e.confusion[[2]tabs.Tab{tabs.Updates, tabs.Promotions}]
	fmt.Printf("\n  THE CELL TO READ: promotions↔updates, %d + %d = %d messages. #219 names that\n", pu, up, pu+up)
	fmt.Println("  boundary as the hard one — both sides are bulk with an unsubscribe header —")
	fmt.Println("  so it is where the classifier earns or loses its keep.")
	fmt.Println()
}

func (e *tabEval) printRules() {
	fmt.Println("PER RULE — which rule decided, and whether it was right")
	fmt.Printf("  %-24s %-12s %6s %8s %7s %10s\n",
		"RULE", "TAB", "FIRED", "SENDERS", "SCORED", "PRECISION")
	names := make([]string, 0, len(e.ruleFired))
	for n := range e.ruleFired {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if e.ruleFired[names[i]] != e.ruleFired[names[j]] {
			return e.ruleFired[names[i]] > e.ruleFired[names[j]]
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		fmt.Printf("  %-24s %-12s %6d %8d %7d %10s\n",
			n, e.ruleTab[n], e.ruleFired[n], len(e.ruleSenders[n]),
			e.ruleScored[n], metric(e.ruleCorrect[n], e.ruleScored[n]))
	}
	fmt.Println()
	fmt.Println("  FIRED counts every message; SCORED counts only those with ground truth, and")
	fmt.Println("  PRECISION is over SCORED. Read the two columns together: a rule with a high")
	fmt.Println("  precision over a handful of senders has demonstrated very little, and that")
	fmt.Printf("  caveat applies hardest to %q, the only rule here that is a curated\n", tabs.RuleSocialPlatform)
	fmt.Println("  name list rather than a header semantic — the one rule that could have been")
	fmt.Println("  fitted to this corpus.")
	fmt.Println()
	if n := e.ruleScored[tabs.RuleBulkApparatus]; n > 0 {
		fmt.Printf("  %q decided %d of the %d scored messages (%.1f%%) — that is the residue\n",
			tabs.RuleBulkApparatus, n, e.truth, 100*ratio(n, e.truth))
		fmt.Println("  rule, which knows only that the mail is machine-sent bulk. Updates precision")
		fmt.Println("  is worth very little if most of it arrives this way.")
		fmt.Println()
	}
	if n := e.ruleFired[tabs.RuleNone]; n > 0 {
		fmt.Printf("  ⚠ %q fired on %d messages. The rule list is supposed to be TOTAL —\n",
			tabs.RuleNone, n)
		fmt.Println("  its last two rules partition every message — so this means someone narrowed")
		fmt.Println("  one of them and mail is now defaulting silently into Primary.")
		fmt.Println()
	}
}

func (e *tabEval) printListIDReconciliation(limit int) {
	fmt.Println("LIST-ID RECONCILIATION — the classifier's disagreement, as the reader's decision")
	fmt.Printf("  %d messages carry a List-Id header (RFC 2919), so the classifier calls them Forums.\n", e.listIDTotal)
	if e.listIDTotal == 0 {
		fmt.Println("  None in this corpus, so there is nothing to reconcile.")
		return
	}
	fmt.Println("  Where the mapping puts the labelled ones today:")
	for _, t := range tabs.All {
		if n := e.listIDByTab[t]; n > 0 {
			fmt.Printf("    %-12s %6d\n", t, n)
		}
	}
	fmt.Println()
	fmt.Println("  NEITHER SIDE IS WRONG. The mapping says where this mail should go today; the")
	fmt.Println("  classifier says what it IS per RFC 2919. This list is the input for deciding")
	fmt.Println("  whether to point some of those labels at Forums — it is not an error list.")
	fmt.Println()

	names := make([]string, 0, len(e.listIDNames))
	for n := range e.listIDNames {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if e.listIDNames[names[i]] != e.listIDNames[names[j]] {
			return e.listIDNames[names[i]] > e.listIDNames[names[j]]
		}
		return names[i] < names[j]
	})
	shown := names
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	fmt.Printf("  DISTINCT LISTS (%d total, showing %d):\n", len(names), len(shown))
	for _, n := range shown {
		fmt.Printf("    %-56s %5d\n", n, e.listIDNames[n])
	}
	if len(shown) < len(names) {
		fmt.Printf("    … %d more not shown; raise -list-ids to see them.\n", len(names)-len(shown))
	}
}

// probeMinClass is the class size below which a separation is flagged rather
// than trusted. Not a filter — a small class is still shown, because hiding it
// would be its own kind of dishonesty — but a 41-point gap against ten messages
// is a hypothesis, not a finding.
const probeMinClass = 20

// bestPairGap returns the two true tabs a signal separates best, and by how
// much.
//
// THE NUMBER IS UNCHANGED FROM THE FIRST VERSION, AND THAT IS THE POINT — a
// max-minus-min over the classes IS the largest pairwise difference, so the
// original computation was arithmetically fine. What it did wrong was print
// that number with nothing saying WHICH TWO CLASSES it belonged to.
//
// On a real corpus the probe population is lopsided: two large classes and a
// tiny social one. `bulk_stream_subdomain` printed a headline gap, which a reader
// naturally took to mean the two tabs the rule is failing to separate — while
// its actual promotions-vs-updates separation was small, in the wrong
// direction. The headline was real; it described social vs updates, and a
// handful of messages were setting the top of the table.
//
// So this returns the PAIR as well as the gap, and the caller names it and
// flags a thin class. An unlabelled aggregate is precisely what this report
// refuses everywhere else; it had one of its own.
func (e *tabEval) bestPairGap(signal string, present []tabs.Tab) (tabs.Tab, tabs.Tab, float64) {
	var bestA, bestB tabs.Tab
	best := -1.0
	for i := 0; i < len(present); i++ {
		for j := i + 1; j < len(present); j++ {
			a, b := present[i], present[j]
			ra := ratio(e.probeSignals[a][signal], e.probeTotal[a])
			rb := ratio(e.probeSignals[b][signal], e.probeTotal[b])
			gap := ra - rb
			if gap < 0 {
				gap = -gap
			}
			// Ties break toward the pair with more evidence behind it, so the
			// named pair is the one a reader can actually act on.
			if gap > best ||
				(gap == best && e.probeTotal[a]+e.probeTotal[b] > e.probeTotal[bestA]+e.probeTotal[bestB]) {
				best, bestA, bestB = gap, a, b
			}
		}
	}
	if best < 0 {
		return "", "", 0
	}
	return bestA, bestB, best
}

// printProbe answers one question: among the mail a single rule currently lumps
// together, is there any cheap signal that separates the tabs it is getting
// wrong?
//
// THE SPLIT IS THE WHOLE POINT, and it is the lesson #169 paid for. A signal's
// rate on one class means nothing on its own — the promotional lexicon fires on
// 25% of Promotions and looked like signal until someone measured that it fires
// on the negatives too. What makes a discriminator is the GAP between its rate
// on the classes being confused. So every row below is two rates and their
// difference, and a signal whose rates match is reported as no use rather than
// quietly omitted.
func (e *tabEval) printProbe() {
	if e.probeRule == "" {
		return
	}
	fmt.Printf("\n=== PROBE: what separates the tabs that %q lumps together ===\n", e.probeRule)
	total := 0
	for _, n := range e.probeTotal {
		total += n
	}
	if total == 0 {
		fmt.Printf("  No scored message was decided by a rule named %q.\n", e.probeRule)
		fmt.Println("  Either the name is wrong or that rule never fires on mail with ground truth —")
		fmt.Println("  both are findings, and neither is an empty result.")
		return
	}

	present := make([]tabs.Tab, 0, len(tabs.All))
	for _, t := range tabs.All {
		if e.probeTotal[t] > 0 {
			present = append(present, t)
		}
	}
	fmt.Printf("  population: %d scored messages, by TRUE tab —", total)
	for _, t := range present {
		fmt.Printf("  %s %d", t, e.probeTotal[t])
	}
	fmt.Println()

	// Every signal seen anywhere in the population.
	seen := map[string]bool{}
	for _, t := range present {
		for s := range e.probeSignals[t] {
			seen[s] = true
		}
	}
	names := make([]string, 0, len(seen))
	for s := range seen {
		names = append(names, s)
	}

	sort.Slice(names, func(i, j int) bool {
		_, _, gi := e.bestPairGap(names[i], present)
		_, _, gj := e.bestPairGap(names[j], present)
		if gi != gj {
			return gi > gj
		}
		return names[i] < names[j]
	})

	fmt.Printf("\n  %-24s", "CANDIDATE SIGNAL")
	for _, t := range present {
		fmt.Printf(" %8s(%d)", t, e.probeTotal[t])
	}
	fmt.Printf("   %s\n", "BEST SEPARATION (which pair)")
	for _, s := range names {
		fmt.Printf("  %-24s", s)
		for _, t := range present {
			fmt.Printf(" %11.1f%%", 100*ratio(e.probeSignals[t][s], e.probeTotal[t]))
		}
		a, b, gap := e.bestPairGap(s, present)
		note := ""
		if e.probeTotal[a] < probeMinClass || e.probeTotal[b] < probeMinClass {
			note = fmt.Sprintf("  ⚠ n<%d", probeMinClass)
		}
		fmt.Printf("   %5.1f%%  %s vs %s%s\n", 100*gap, a, b, note)
	}
	fmt.Println("\n  SEPARATION IS PER PAIR, and the pair is named, because the classes here are very")
	fmt.Println("  different sizes. A max-minus-min over all three would let the smallest class set")
	fmt.Println("  the headline for every row — which is the same mistake as quoting a precision")
	fmt.Println("  without its sample size, and this report already refuses that elsewhere.")
	fmt.Println("  A separation near zero means the signal fires equally on both sides: not a")
	fmt.Println("  discriminator, however often it fires. Only a large one is worth a rule.")

	for _, t := range present {
		fmt.Printf("\n  TOP SENDING DOMAINS where the truth is %s (d= or Return-Path):\n", t)
		doms := make([]string, 0, len(e.probeDomains[t]))
		for d := range e.probeDomains[t] {
			doms = append(doms, d)
		}
		sort.Slice(doms, func(i, j int) bool {
			if e.probeDomains[t][doms[i]] != e.probeDomains[t][doms[j]] {
				return e.probeDomains[t][doms[i]] > e.probeDomains[t][doms[j]]
			}
			return doms[i] < doms[j]
		})
		for i, d := range doms {
			if i >= 10 {
				fmt.Printf("    … %d more\n", len(doms)-10)
				break
			}
			fmt.Printf("    %-50s %4d\n", d, e.probeDomains[t][d])
		}
		if len(e.probeSamples[t]) > 0 {
			fmt.Printf("  EXAMPLES (%s):\n", t)
			for _, s := range e.probeSamples[t] {
				fmt.Printf("    %s\n", s)
			}
		}
	}
}

// majorityTab returns the tab holding a strict majority of the counts — strict,
// so an evenly split sender reports no answer rather than an arbitrary one.
func majorityTab(counts map[tabs.Tab]int) (tabs.Tab, bool) {
	best, bestN, total, tie := tabs.Tab(""), 0, 0, false
	for _, t := range tabs.All {
		n := counts[t]
		total += n
		switch {
		case n > bestN:
			best, bestN, tie = t, n, false
		case n == bestN && n > 0:
			tie = true
		}
	}
	if total == 0 || tie {
		return "", false
	}
	return best, true
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// metric renders a rate, or an em dash when the denominator is zero. A rate over
// nothing is not 0.000 — printing it as a number invites reading an absent
// measurement as a bad one.
func metric(n, d int) string {
	if d == 0 {
		return "—"
	}
	return fmt.Sprintf("%.3f", ratio(n, d))
}
