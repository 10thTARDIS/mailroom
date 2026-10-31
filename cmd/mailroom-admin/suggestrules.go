// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// `suggest-rules` persists what `suggest-report` only measured (#242).
//
// suggest-report answered "could rule induction reach unlabelled mail at
// all", and the answer — once the collateral-filter defect (#248) was found
// and removed — was yes: most of the unlabelled mail was reachable. That is a
// report: it writes nothing, because there was nothing yet worth writing.
// This command is the other half. It takes exactly the candidates
// suggest-report counted as USABLE and turns them into `app.rules` rows
// carrying a single `suggestLabel` action, so `Email.suggestedLabels` (#176's
// abstention surface) has something to show instead of staying empty
// forever.
//
// COLLATERAL IS NOT A QUALITY BAR HERE — that was #248's defect, and it is
// deliberately not reintroduced. `accept-rules` bounds itself on Collateral —
// the count of NON-POSITIVE messages a candidate also matches — because for a
// rule about to auto-apply a label, collateral is a blast radius and the safe
// default is zero. That reasoning does not transfer to a suggestion: a
// candidate that reaches unlabelled mail HAS Collateral > 0 by definition
// (every unlabelled message is non-positive), so REJECTING on collateral <=
// anything forces this command's result to be empty on every corpus, forever —
// precisely the bug suggest-report shipped with (see suggestreport.go's
// candidateMatches comment for the full account). So the BAR — suggestFilter —
// is Conflicts (never acceptable: the user filed that mail under a different
// label BY HAND) and Covers (not enough of the user's own labelling to trust),
// and nothing else.
//
// RUN `accept-rules` FIRST, THEN THIS FOR THE REMAINDER (#294). The two
// commands are not alternatives and their order is not free. Measured on a
// live mailbox: `suggest-rules` alone proposed many more suggestions than it did
// after `accept-rules` had created its rules first. The candidates in the gap
// were at collateral 0, matching nothing beyond mail the
// user had already filed under that label themselves. A SUGGESTION ASKS; A RULE
// FILES, so for those candidates the wrong order silently downgraded an answer into a
// question AND inflated the needs-a-category queue with mail that had a safe
// answer available. Nothing in either command's output revealed the dependency,
// and both numbers looked reasonable. `--max-collateral` (default 0, the same
// name, default and meaning as accept-rules' flag, so the two bars cannot
// drift) now routes those candidates to the command that files them — see
// belongsToAcceptRules for why that is the INVERSE of #248 rather than its
// return.
//
// THE ROUTING RUNS AFTER THE SUPERSESSION PASS, NOT BEFORE IT, and that ordering
// was itself a correction found on the live instance: measured before it, this
// command reported "N belong to accept-rules — run it first" at the same moment
// accept-rules reported "nothing to do", because the rules had already been
// accepted and every one of those candidates was superseded. A candidate an existing rule covers is
// counted as COVERED whatever its collateral. suggestSkips.partition carries the
// full account, including why the stage-alignment argument that put it earlier
// was reasonable and lost anyway.
//
// PRIORITY SORTS AFTER REAL RULES (see suggestRulePriority): a suggestion is
// induction's second-guess about a message, and it must never evaluate ahead
// of a rule the user actually wrote or explicitly accepted via accept-rules.
//
// DRY RUN BY DEFAULT, same as accept-rules and for the same reason: the
// report IS the deliverable, and "it ran and changed nothing" must not look
// like "it ran and created forty rules" from the outside.
//
// #242's FOLLOW-UP — `--backfill`. Creating the rules alone leaves the
// surface empty until new mail arrives, because rules fire at ingest, and
// "shipped, wired, tested, and produces no rows on real mail" is exactly the
// failure #242 exists to fix. `--backfill` writes app.label_suggestions rows
// for mail ALREADY in the mailbox, through tx.PutRuleSuggestion — the same
// store funnel ingest itself uses for a matched suggestLabel action, never a
// second write path (applyrules.go's doctrine: "a front door onto the SAME
// store functions").
//
// ⚠ ITS COUNT IS NOT EXPECTED TO EQUAL suggest-report's REACHED, and this
// comment (plus the flag help and the printed banner) said the opposite until
// the first live run. That was a FALSE INVARIANT PRINTED IN A REPORT, which is
// worse than no invariant at all: it sends the next reader hunting a defect
// that is not there. The two commands measure deliberately different
// populations, and this one's is strictly SMALLER, by two exclusions
// suggest-report does not make:
//
//	(a) candidates an ACCEPTED rule already covers are dropped (the
//	    supersession pass below) — suggest-report counts every candidate's
//	    reach, accepted or not;
//	(b) messages that already carry ANY label are dropped (the #242 follow-up
//	    guard, in store.PutRuleSuggestion) — suggest-report's "unlabelled"
//	    means "no HAND label", which counts mail a RULE has already filed.
//
// On a real mailbox the two differ. THE ONLY THING WORTH
// COMPARING IS THE INEQUALITY: for the same corpus and --min-covers,
// suggest-rules <= suggest-report, because no filter here can add reach. A
// suggest-rules count LARGER than suggest-report's REACHED would be a real
// defect. A gap is not.
//
// AND THE SKIPPED COUNT IS TWO POPULATIONS, REPORTED SEPARATELY (#291). Exclusion
// (b) above reads as one number, and for one release it was printed as one:
// "N already-categorised message(s) matched and are skipped". Decomposed
// against label provenance on a real corpus most were HAND-filed and a minority
// RULE-filed, and only the rule-filed ones are what v1.34.1's guard prevents — so the single
// figure overstated its own fix roughly fourfold. The 123 are the candidates'
// OWN POSITIVES: the hand-filed mail each rule was induced FROM, which the
// pre-v1.34.1 filter (len(UserLabelIDs) == 0) already excluded and which was
// never a target under any version. One number could not tell "the guard is
// doing 42 messages of work" from "the guard is doing 165", and the second
// reading is the one a reader takes. Both halves are now printed, in the
// per-label table and in both summaries. The store guard itself is unchanged and
// correct: HasAnyLabel is the right test for WHETHER TO WRITE; this is purely
// about how the skip is ATTRIBUTED.

// suggestRulePriority is the default --priority for rules this command
// creates.
//
// Ordinary rules default to priority 100 (db/schema.sql's column default,
// and what accept-rules uses for its own creates), and rules evaluate
// LOWEST priority first (store.RulesForEval orders "priority, id" ascending).
// A suggestion has to sort AFTER every rule a human wrote or accepted — it
// is induction's second-guess, not a first opinion — so the default here
// must be numerically LARGER than 100, not the same number repeated. 200
// clears every ordinary rule's default with headroom left in (100, 200) for
// an operator who wants to place something between "my rules" and "the
// machine's guesses".
const suggestRulePriority = 200

// suggestSkipReason names why an induced candidate was not kept, so the
// report can give per-signal counts (CLAUDE.md: "report per-signal counts,
// never one total") instead of a single number that cannot tell "nothing
// cleared the bar" from "everything did".
type suggestSkipReason int

const (
	suggestKept suggestSkipReason = iota
	suggestSkipConflict
	suggestSkipCovers
)

// suggestFilter decides whether an induced candidate clears suggest-rules'
// bar, and names why not when it does not.
//
// COLLATERAL IS DELIBERATELY ABSENT FROM THIS FUNCTION, and that absence is
// the whole point — it is the exact defect #248 found and fixed in
// suggest-report. Collateral counts the NON-POSITIVE messages a candidate
// matches; every unlabelled message is non-positive, so a candidate reaching
// unlabelled mail has Collateral > 0 BY DEFINITION. Filtering on it here
// would force this command to propose nothing, on every corpus, forever.
// TestSuggestFilterKeepsHighCollateralCandidate pins the inverse: a
// Collateral > 0 candidate must NOT be rejected by this function.
func suggestFilter(c rules.Candidate, minCovers int) suggestSkipReason {
	// Conflicts are never acceptable at any threshold — mirroring
	// accept-rules exactly, for the exact same reason: a conflict is mail
	// the user filed under a DIFFERENT label by hand, so the rule would
	// contradict them rather than merely surprise them.
	if c.Conflicts > 0 {
		return suggestSkipConflict
	}
	if c.Covers < minCovers {
		return suggestSkipCovers
	}
	return suggestKept
}

// belongsToAcceptRules reports whether a candidate that has already cleared the
// bar above is one `accept-rules` would take, and which this command must
// therefore NOT offer as a question (#294).
//
// THE ORDERING DEFECT, MEASURED. On a live mailbox: `suggest-rules`
// alone proposed many more suggestion rules than it did after `accept-rules`
// had run first. Every one of the candidates in the gap sat at collateral
// 0 — matching nothing beyond mail the user had already filed under that label
// themselves.
//
// THAT IS A DEFECT, NOT MERE REDUNDANCY, because the two commands do different
// things to a message. A SUGGESTION ASKS; A RULE FILES. For a zero-collateral
// candidate — backed by the user's own filings and reaching nothing else — filing
// is strictly better: the label lands at ingest and the user is never
// interrupted. Proposing it as a suggestion instead downgrades an answer into a
// question, and since an open suggestion is one arm of the needs-a-category
// predicate, it INFLATES the review queue with mail that had a safe answer
// available. Running the two commands in the wrong order did that 35 times, and
// neither command's output said the order mattered.
//
// ⚠ THIS IS NOT #248 COMING BACK, AND THE DIFFERENCE IS THE WHOLE ARGUMENT.
// #248's defect was `collateral <= 0` used as an ACCEPTANCE test in
// suggest-report: keep a candidate only if it matches no non-positive message.
// Every unlabelled message is non-positive, so that test says "keep only the
// candidates that can suggest nothing", and it forces REACHED to 0 on every
// corpus, forever — a tautology about the filter that read as a finding about
// induction. The same test appears here with the opposite sign and the opposite
// consequence:
//
//	#248's bug  : KEEP if collateral <= 0 — keeps only the candidates that reach
//	              nothing, so the output is empty by construction, always.
//	this (#294) : SKIP if collateral <= 0 — hands the candidates that reach
//	              nothing-to-ask-about to the command that FILES them, and keeps
//	              exactly the population #248 restored: collateral > 0, i.e. the
//	              candidates that DO reach unlabelled mail.
//
// So a bound of 0 cannot empty this command the way #248's did; it removes only
// candidates with nothing to suggest. Kept deliberately SEPARATE from
// suggestFilter — the quality bar, where collateral must never appear — because
// this is a ROUTING decision made after that bar is cleared, and because
// TestSuggestFilterKeepsHighCollateralCandidate and
// TestZeroCollateralCandidatesCannotReachUnlabelledMail pin #248's fix on that
// bar and must keep passing untouched. They do.
//
// ⚠ RAISING --max-collateral ABOVE 0 IS THE DANGEROUS DIRECTION, and it is the
// same danger accept-rules' flag carries: at N, this hands over candidates that
// would newly touch up to N messages, and accept-rules at the same bound files
// them without asking. Same name, same default, same meaning in both commands,
// so the two bars cannot drift into disagreeing about which command owns a
// candidate.
//
// TAKES A COLLATERAL, NOT A Candidate, because it is applied late — after dedup
// and after supersession (see suggestSkips.partition) — where what survives is a
// `suggestion` rather than an induced candidate. One number decides this, and a
// signature naming that number cannot be handed the wrong one.
func belongsToAcceptRules(collateral, maxCollateral int) bool {
	return collateral <= maxCollateral
}

// suggestSkips tallies what became of every induced candidate. FIVE OUTCOMES,
// FIVE NUMBERS (CLAUDE.md: per-signal counts, never one total), filled at the
// three stages they can honestly be decided at: `propose` per candidate, `dedup`
// once a label's set is known, `partition` once the whole set is.
//
// TOGETHER WITH THE SURVIVORS THEY PARTITION THE INDUCED SET — every candidate
// lands in exactly one of the six, and TestPipelineAccountsForEveryInducedCandidate
// asserts the equality. That is not decoration: this file has now twice shipped a
// number that meant something other than what its reader took it for, and both
// times the tell was a population sitting in no bucket or the wrong one.
//
// A named type with methods rather than counters incremented inline, because the
// ORDER of the tests is load-bearing and testable only if each decision is one
// function. Two orderings matter, and both were argued from a live run:
//
//	conflicts/covers BEFORE acceptable — so a conflicting candidate at collateral
//	0 is counted as a CONFLICT, never as one accept-rules would take. accept-rules
//	refuses conflicts too, so counting it there would advise an action that does
//	nothing.
//
//	superseded BEFORE acceptable — same principle, and it cost a release to learn;
//	see partition.
type suggestSkips struct {
	conflicts  int
	covers     int
	superseded int
	acceptable int
	// duplicates is the FIFTH bucket, and the ONE THAT IS NOT A REFUSAL. A
	// candidate dropped here is strictly narrower than one being kept for the same
	// label — the SAME decision expressed twice — so the population it would have
	// suggested for IS offered, under the broader form. It is counted, and worded,
	// as folded-in rather than withheld.
	//
	// ⚠ ITS SEVERITY IS GENUINELY LOWER THAN THE OTHER FOUR, and saying so is part
	// of reporting it honestly. Until this counter existed these candidates were in
	// NO bucket: measured live at 3 (visible candidates went 120 → 117 across the
	// two builds, the three being accept-able candidates that were also
	// dedup-dropped, counted pre-dedup in `acceptable` before #294's reorder moved
	// the routing after it). But unlike the 48-against-0 that reorder fixed, NO
	// PRINTED NUMBER CONTRADICTED THEM: a reader would have had to sum the buckets
	// and compare against an induced total the report never printed. It could not
	// cause a wrong action, only an unaccountable one. It is here because a known
	// gap and an unmeasured gap are different things, and because the report now
	// claims the six numbers account for everything — a claim that has to be true.
	duplicates int
}

// propose turns one label's induced candidates into suggestions, applying the BAR
// and tallying what it drops.
//
// The candidate → suggestion construction lives HERE, not at the call site, so the
// pipeline the partition test drives is the pipeline the command runs. A candidate
// lost between induction and the kept list would otherwise be invisible to any
// test that did the construction itself.
//
// #294's routing is deliberately NOT here; see partition.
func (s *suggestSkips) propose(l store.LabelView, induced []rules.Candidate, minCovers int) []suggestion {
	out := make([]suggestion, 0, len(induced))
	for _, c := range induced {
		if !s.route(c, minCovers) {
			continue
		}
		out = append(out, suggestion{
			label: l, conditions: c.Conditions,
			positives: c.Positives, covers: c.Covers, collateral: c.Collateral,
		})
	}
	return out
}

// route applies the BAR — and only the bar — to one induced candidate, tallying
// the reason it was dropped and reporting whether it survives.
func (s *suggestSkips) route(c rules.Candidate, minCovers int) bool {
	switch suggestFilter(c, minCovers) {
	case suggestSkipConflict:
		s.conflicts++
		return false
	case suggestSkipCovers:
		s.covers++
		return false
	}
	return true
}

// dedup drops any proposal strictly narrower than one already kept for the same
// label, and counts what it dropped.
//
// Identical reasoning, and the identical primitives (conjuncts / isSuperset), to
// accept-rules' own dedup pass: a rule that is "the kept one plus another
// restriction" can never fire on anything the broader one misses, so keeping both
// would make the suggestion list longer than the number of decisions in it.
//
// ⚠ THE COUNT IS WORDED DIFFERENTLY FROM THE OTHER FOUR ON PURPOSE. These are not
// proposals withheld — the mail they would have suggested for is suggested, by the
// broader form kept above them. "Not offered" would be false about them, which is
// the whole reason they need a bucket of their own rather than a share of one of
// the others.
//
// Broadest first (covers descending), then skip anything whose conjuncts are a
// superset of one already kept: conjunct-set containment rather than tree
// equality, because that is exactly the "same rule plus another restriction"
// relationship.
func (s *suggestSkips) dedup(kept []suggestion) []suggestion {
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].covers > kept[j].covers })
	out := kept[:0]
	for _, k := range kept {
		redundant := false
		for _, d := range out {
			if d.label.ID == k.label.ID && isSuperset(conjuncts(k.conditions), conjuncts(d.conditions)) {
				redundant = true
				break
			}
		}
		if redundant {
			s.duplicates++
			continue
		}
		out = append(out, k)
	}
	return out
}

// partition applies the two tests that can only be decided once the whole kept
// set is known — supersession against the user's own rules, then #294's routing —
// IN THAT ORDER, and returns what survives both.
//
// ⚠ THE ORDER IS THE FIX, AND IT REPLACED A REASONABLE-SOUNDING ARGUMENT THAT
// MEASUREMENT KILLED. #294's routing first shipped inside `route`, at the same
// stage accept-rules applies its own collateral bound (accept.go's single
// conflicts/collateral/covers test, before its dedup and supersession passes).
// That was chosen for STAGE ALIGNMENT: the same test, on the same inputs, at the
// same point in both commands, so the two bars could not drift. It is a genuinely
// good reason and it lost anyway.
//
// It lost to the STEADY STATE. On a live instance, an hour after
// `accept-rules --apply` had been run, the two commands said:
//
//	suggest-rules : N belong to `accept-rules` … ⚠ run `accept-rules --apply` first
//	accept-rules  : every proposal that met the bar is already covered by a rule
//	                you have; nothing to do
//
// N against 0. Every accept-able candidate was by then superseded by a real
// rule, and the tally never saw it, because it ran before the supersession pass
// that would have reclassified them. The bound was not merely loose; in the
// NORMAL state of a worked mailbox it was maximally loose, and the ⚠ instructed
// the reader to take an action that would do nothing. A plausible number meaning
// something other than what its reader takes it for is the exact defect class
// #291 and #294 are the fix for, so shipping it inside that fix was not an option.
//
// READER ACCURACY BEATS IMPLEMENTATION SYMMETRY. A candidate an existing rule
// already covers belongs in "already covered by a rule you have" whatever its
// collateral: that is what is true about it, and it is already the bucket with
// the right name. What stage accept-rules happens to test at is an implementation
// detail of the other command; what the operator should do next is not.
//
// `acceptable` remains an UPPER BOUND — accept-rules runs its own induction and
// its own dedup, so it may pick a different representative of a cluster and
// create fewer — but the bound is now tight enough to act on rather than a
// restatement of the whole population.
func (s *suggestSkips) partition(kept []suggestion, saved []savedRule, maxCollateral int) []suggestion {
	fresh := kept[:0]
	for _, k := range kept {
		// SUPERSESSION FIRST. A message an accepted addLabel rule already covers
		// gets that label at ingest and never reaches the unlabelled pool at all,
		// so suggesting it would be a second opinion about mail that already has a
		// first one — and telling the operator to run accept-rules for it would be
		// advice about work that is already done.
		if coveredBySaved(k, saved) {
			s.superseded++
			continue
		}
		if belongsToAcceptRules(k.collateral, maxCollateral) {
			s.acceptable++
			continue
		}
		fresh = append(fresh, k)
	}
	return fresh
}

// coveredBySaved reports whether a rule the user already has would catch
// everything this suggestion would.
//
// REUSES accept-rules' supersession primitives (conjuncts / isSuperset) rather
// than a second implementation of "does an existing rule already catch this
// population" wearing a different name (mailroom#195). Same label, and the
// suggestion's conjuncts a superset of the rule's — i.e. the suggestion is that
// rule plus zero or more extra restrictions, so it can never fire on anything the
// rule misses.
func coveredBySaved(s suggestion, saved []savedRule) bool {
	sc := conjuncts(s.conditions)
	for i := range saved {
		if saved[i].labels[s.label.ID] && isSuperset(sc, saved[i].conjuncts) {
			return true
		}
	}
	return false
}

// suggestion is one induced suggestLabel candidate that cleared the bar and
// survived both dedup passes below: the label it would suggest, the
// conditions it would test, and the evidence numbers the report renders.
//
// Deliberately its own type rather than accept.go's proposal: that struct
// carries no Positives, and Positives IS the number this command's whole
// report is built to show — EvidenceConfidence is a function of it, not of
// Covers or Collateral.
type suggestion struct {
	label      store.LabelView
	conditions rules.Condition
	positives  int
	covers     int
	collateral int
}

func runSuggestRules(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("suggest-rules", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		limit     = fs.Int("limit", 5000, "how many messages to scan as evidence")
		minCovers = fs.Int("min-covers", 2, "propose a suggestion only if it explains at least this many of your own labellings")
		// RUN `accept-rules` FIRST, THEN THIS FOR THE REMAINDER (#294). Same
		// name, same default and same meaning as accept-rules' own flag, on
		// purpose: a candidate at or below this bound is one accept-rules would
		// FILE, and filing beats asking whenever the candidate reaches nothing
		// the user has not already filed themselves. This is NOT #248's
		// collateral filter returning — see belongsToAcceptRules for why the
		// inverted test has the inverted consequence.
		maxCollateral = fs.Int("max-collateral", 0, "leave candidates at or below this collateral to `accept-rules`, "+
			"which FILES them instead of asking about them (#294); run accept-rules FIRST, then this for the remainder")
		apply    = fs.Bool("apply", false, "actually create the suggestion rules, and (with --backfill) write suggestion rows (default: report only)")
		priority = fs.Int("priority", suggestRulePriority, "priority for created suggestion rules (see suggestRulePriority: must sort after real rules)")
		backfill = fs.Bool("backfill", false, "also write app.label_suggestions rows for EXISTING UNCATEGORISED mail (#242 follow-up); "+
			"its count is a SUBSET of suggest-report's REACHED and is never expected to equal it — this command additionally "+
			"drops candidates an accepted rule already covers, and messages that already carry any label")
		maxMut = fs.Int("max-mutations", 1000, "stop --backfill after writing this many suggestion rows (apply-rules' bound, same reasoning)")
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

	var (
		candidates []store.RulePreviewCandidate
		total      int
		labels     []store.LabelView
		saved      []savedRule
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if candidates, e = tx.RulePreviewCandidates(ctx, *limit); e != nil {
			return e
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return e
		}
		if labels, e = tx.ListLabels(ctx); e != nil {
			return e
		}
		// REUSED, not reimplemented: the same function accept.go's supersession
		// pass loads from, so "does an existing rule already cover this" is one
		// piece of logic answering both commands rather than two that could drift
		// (mailroom#195's lesson).
		saved, e = savedRules(ctx, tx)
		return e
	}); err != nil {
		return err
	}

	w := splitWorkload(candidates)

	// A BOUNDED SCAN MUST REPORT ITS BOUND (#155) — a lower bound reads as a
	// total.
	complete := len(candidates) >= total
	fmt.Printf("evidence : %d of %d messages scanned%s\n",
		len(candidates), total,
		map[bool]string{true: "", false: "  (TRUNCATED — raise --limit)"}[complete])
	fmt.Printf("workload : %d uncategorised, %d filed by hand, %d filed by a RULE (categorised, so not a target)\n",
		w.uncategorised, w.handFiled, w.ruleFiled)
	fmt.Printf("proposing: conflicts 0, covers >= %d, collateral > %d\n", *minCovers, *maxCollateral)
	fmt.Printf("           collateral is NOT a quality bar here (#248): a candidate that reaches\n")
	fmt.Printf("           unlabelled mail HAS collateral, so rejecting on it empties this report by\n")
	fmt.Printf("           construction. The bound only ROUTES candidates at or below it away.\n")
	// THE ORDER IS NOT FREE, AND UNTIL #294 NOTHING SAID SO. Printed before the
	// numbers, because the numbers are wrong-by-omission if the operator has not
	// run accept-rules yet: 48 proposals became 13 once it had.
	fmt.Printf("order    : run `accept-rules --apply` FIRST, then this for the remainder (#294). A\n")
	fmt.Printf("           suggestion ASKS; a rule FILES. Candidates accept-rules would take are\n")
	fmt.Printf("           counted below and left to it rather than turned into questions.\n\n")

	names := map[string]string{}
	for _, l := range labels {
		names[l.ID.String()] = l.Name
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })

	var (
		kept  []suggestion
		skips suggestSkips
	)
	for _, l := range labels {
		target := l.ID.String()
		examples := make([]rules.Example, 0, len(candidates))
		positives := 0
		for _, c := range candidates {
			ex := rules.Example{Facts: factsFromCandidate(c)}
			for _, lid := range c.UserLabelIDs {
				if lid.String() == target {
					ex.Positive = true
					continue
				}
				if n, ok := names[lid.String()]; ok {
					ex.OtherUserLabels = append(ex.OtherUserLabels, n)
				}
			}
			if ex.Positive {
				positives++
			}
			examples = append(examples, ex)
		}
		if positives == 0 {
			continue
		}
		kept = append(kept, skips.propose(l, rules.Induce(examples), *minCovers)...)
	}

	// THE PIPELINE, IN ONE PLACE AND IN ORDER: bar (above, per candidate) → dedup
	// → supersession → #294's routing. Every stage tallies what it drops, and the
	// five tallies plus the survivors account for every candidate induction
	// proposed. Both of this release's defects were populations that had no bucket
	// or the wrong one, so the accounting is now the property, not a side effect.
	kept = skips.dedup(kept)

	// DROP ANYTHING AN ACCEPTED RULE ALREADY COVERS, AND THEN ANYTHING
	// `accept-rules` WOULD TAKE — in that order, which is the whole of #294's
	// correction. See suggestSkips.partition for the live run that settled it: with
	// the routing done earlier, this command claimed 48 candidates belonged to
	// accept-rules at the same moment accept-rules said it had nothing to do.
	kept = skips.partition(kept, saved, *maxCollateral)

	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].label.Name != kept[j].label.Name {
			return kept[i].label.Name < kept[j].label.Name
		}
		return kept[i].covers > kept[j].covers
	})

	// candidateMatches, THROUGH THE REAL EVALUATOR — never a second matcher —
	// is what both the reach numbers below AND --backfill's writes are
	// computed from, so a suggestion this report claims would reach a message
	// is EXACTLY the suggestion --backfill would write for it (#155's rule for
	// Rule/preview, applied here).
	scan := scanBackfill(kept, candidates)
	reachedMsgs := map[ulid.ULID]bool{}
	for _, t := range scan.targets {
		reachedMsgs[t.msgID] = true
	}
	covered := len(reachedMsgs)
	abstain := w.uncategorised - covered

	for _, s := range kept {
		name := suggestRuleName(s.label.Name, s.conditions)
		conf := rules.EvidenceConfidence(s.positives)
		fmt.Printf("  %-14s positives %-4d collateral %-4d confidence %.2f\n",
			truncate(s.label.Name, 14), s.positives, s.collateral, conf)
		fmt.Printf("                 rule: %s\n", name)
		fmt.Printf("                 cond: %s\n", describe(s.conditions))
	}
	if len(kept) > 0 {
		fmt.Println()
	}

	fmt.Printf("%d suggestion rule(s) meet the bar.\n", len(kept))
	// FOUR REASONS, FOUR NUMBERS, PRINTED WHETHER OR NOT THEY ARE ZERO. These used
	// to share one parenthetical and the accept-rules bucket did not exist at all
	// (#294). Each zero is a different finding — nothing conflicted, nothing was
	// too thin, nothing is already covered, or (the one worth reading) accept-rules
	// has already been run and there is nothing left for it to take.
	fmt.Println("not offered, by reason:")
	fmt.Printf("  %-5d already covered by a rule you have\n", skips.superseded)
	fmt.Printf("  %-5d belong to `accept-rules` (collateral <= %d: it would FILE them, not ask)\n",
		skips.acceptable, *maxCollateral)
	fmt.Printf("  %-5d dropped for conflicts (mail you filed under a DIFFERENT label by hand)\n", skips.conflicts)
	fmt.Printf("  %-5d dropped for insufficient covers (fewer than %d of your own labellings)\n",
		skips.covers, *minCovers)
	// THE FIFTH NUMBER, AND THE ONLY ONE THAT IS NOT A REFUSAL — so it sits below
	// the four with its own framing rather than inside a list headed "not offered",
	// which would be false about it. The mail these would have suggested for IS
	// suggested, by the broader form kept above. Printed even at zero, like the
	// four, because a bucket that appears only when non-empty is one a reader
	// cannot tell from a bucket that does not exist.
	fmt.Printf("plus %d narrower duplicate(s) of a rule offered above — the SAME decision expressed\n",
		skips.duplicates)
	fmt.Printf("     twice (broader form kept), folded in rather than withheld.\n")
	// AND THE ACCOUNTING CLAIM, which is checkable from the numbers on this screen.
	// It is here because the alternative — a reader summing six numbers against a
	// total the report never prints — is exactly how three candidates sat in no
	// bucket at all until someone diffed two builds.
	fmt.Printf("     these five and the %d above account for every candidate induction proposed.\n", len(kept))
	// ⚠ ONLY WHEN THERE IS SOMETHING TO DO. This block advises an ACTION, so it
	// must not appear when the action would do nothing — which is precisely what it
	// did on a live instance when the count was measured before the
	// supersession pass (a positive count here against "nothing to do" there). Now that
	// superseded candidates are counted as superseded, an acceptable count of 0 is
	// the normal steady state and this stays silent.
	if skips.acceptable > 0 {
		fmt.Printf("  ⚠ run `accept-rules --apply` first, then re-run this: those %d are mail your own\n", skips.acceptable)
		fmt.Printf("    filings already answer, and asking about them instead would add %d question(s)\n", skips.acceptable)
		fmt.Printf("    to the needs-a-category queue. Candidates a rule of yours ALREADY covers are\n")
		fmt.Printf("    counted on the line above, not here, so this is what is genuinely left for it.\n")
		// A BOUND, REPORTED AS ONE (CLAUDE.md) — but a tight one now, and no longer
		// the load-bearing caveat it was when it stood in for the supersession pass.
		fmt.Printf("    Still an upper bound: accept-rules runs its own induction and dedup, so it may\n")
		fmt.Printf("    pick a different form of the same rule and create slightly fewer.\n")
	}

	// PER LABEL BEFORE THE TOTALS, and printed whether or not --backfill was
	// passed: the reach claim is what an operator judges these rules on, and it
	// must be visible BEFORE deciding to write anything. THREE columns because one
	// number cannot tell "this label reaches nothing" from "everything it reaches
	// is already filed" (the #242 follow-up), and two cannot tell "already filed BY
	// A RULE, which the guard prevented" from "already filed BY THE USER, which was
	// never a target and is this candidate's own evidence" (#291).
	scan.printPerLabel(kept)

	pct := 0
	if w.uncategorised > 0 {
		pct = 100 * covered / w.uncategorised
	}
	fmt.Printf("\n%d of %d uncategorised messages would receive at least one suggestion (%d%%).\n",
		covered, w.uncategorised, pct)
	// THE ABSTENTION COUNT — on its own line, never folded into the total
	// above. A bounded/partial result must report its bound (CLAUDE.md); zero
	// suggestions offered and zero uncategorised mail look identical in a single
	// number, and only the explicit remainder tells them apart.
	fmt.Printf("%d uncategorised messages would receive NOTHING from these suggestions.\n", abstain)
	// AND THE SKIPPED POPULATION — AS TWO NUMBERS, NEVER ONE (#291). These are
	// messages a kept suggestion genuinely MATCHES and is deliberately not written
	// for, because they already carry a label. Printed as a single figure it read
	// as the size of what v1.34.1's guard prevents, and it is not: on a real
	// corpus it decomposed into mostly hand-filed mail (never a target under any
	// version, and the very mail these candidates were induced FROM) and a smaller
	// rule-filed part (the population the guard actually catches). One number
	// overstated the fix several times over.
	// Reporting zero in the RULE row on a corpus with rule-filed mail is still the
	// signal that the guard, or this measurement of it, has regressed.
	scan.printFiledSkips()

	if !*apply {
		fmt.Printf("\nDRY RUN — would create %d suggestion rule(s). Re-run with --apply.\n", len(kept))
	} else {
		created, skipped := 0, 0
		for _, s := range kept {
			name := suggestRuleName(s.label.Name, s.conditions)
			err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				_, e := tx.CreateRule(ctx, store.RuleRow{
					Name:       name,
					Priority:   *priority,
					Conditions: s.conditions,
					Actions: []rules.Action{{
						Type:     rules.ActionSuggestLabel,
						Label:    s.label.ID.String(),
						Evidence: s.positives,
					}},
					Enabled: true,
					// System-suggested for the same reason accept-rules marks its own
					// creates: this rule was derived from the user's labelling, not
					// written by them, and a rules list that cannot tell the two apart
					// is one nobody can audit.
					SystemSuggested: true,
				})
				return e
			})
			switch {
			case err == nil:
				created++
				fmt.Printf("  created  %s\n", name)
			case errors.Is(err, store.ErrRuleNameTaken):
				// Almost always a re-run: the name is deterministic from label +
				// conditions (suggestRuleName), so the same candidate collides with
				// itself rather than duplicating — the idempotence accept-rules also
				// relies on.
				skipped++
				fmt.Printf("  exists   %s\n", name)
			default:
				return fmt.Errorf("creating %q: %w", name, err)
			}
		}
		fmt.Printf("\ncreated %d, already present %d\n", created, skipped)
	}

	if *backfill {
		return runSuggestBackfill(ctx, d, userID, scan, w.uncategorised, *apply, *maxMut)
	}
	return nil
}

// workload is the candidate scan split into the THREE populations this command
// has to keep apart. It is a named type with a unit test (TestSplitWorkload)
// rather than three counters inline, because getting the split wrong is exactly
// the defect the #242 follow-up fixed and a report is only as honest as its
// denominator.
type workload struct {
	// uncategorised: NO label at all. This is the suggestion target population and
	// the needs-a-category population — one definition, and it is the one
	// EmailQuery.NeedsCategory uses (NOT EXISTS over app.message_labels, no
	// applied_by test).
	uncategorised int
	// handFiled: at least one label the USER applied. This is the induction
	// EVIDENCE population — the only labels rules.Induce is allowed to learn from.
	handFiled int
	// ruleFiled: labelled, but only by automation. ⚠ THIS ROW IS THE WHOLE BUG.
	// It is categorised (so not a target) yet contributes no evidence, and the
	// original code had no bucket for it: `len(UserLabelIDs) == 0` swept it in with
	// the uncategorised, and a fair share of proposed suggestions on a real
	// mailbox landed here. It gets its own printed number so that never again
	// hides inside another.
	ruleFiled int
}

func splitWorkload(candidates []store.RulePreviewCandidate) workload {
	var w workload
	for _, c := range candidates {
		switch filingOf(c) {
		case filedByHand:
			w.handFiled++
		case filedByRule:
			w.ruleFiled++
		default:
			w.uncategorised++
		}
	}
	return w
}

// filing is WHO filed a scanned message, which is the distinction this whole
// command turns on twice: it decides what counts as induction evidence, and
// (#291) how a skipped match is attributed.
type filing int

const (
	// filedNever: no label of any kind. The suggestion TARGET population, and
	// the needs-a-category population.
	filedNever filing = iota
	// filedByHand: at least one label the USER applied. Induction evidence, and
	// never a suggestion target at any version of this command.
	filedByHand
	// filedByRule: labelled, but only by automation. Categorised, so not a
	// target, yet contributing no evidence — the bucket whose absence was the
	// #242 follow-up defect.
	filedByRule
)

// filingOf classifies one scanned message.
//
// ONE FUNCTION, TWO CALLERS — splitWorkload's denominator and scanBackfill's skip
// attribution — rather than the same two-line switch written twice
// (mailroom#195's lesson). They MUST agree: the workload table says how many messages
// were filed by hand, and the skip table says how many of those a suggestion
// matched. Two copies of the test could drift and the two tables would then
// disagree about the same mailbox with nothing to say which was right.
//
// HAND WINS OVER RULE when a message carries both, and that is deliberate: the
// hand label is what makes the message evidence, and what makes it never a
// target. The store can produce that shape (attachLabelFacts sets HasAnyLabel for
// every message_labels row and appends to UserLabelIDs only for
// applied_by='user'), so the precedence is a real decision, not a formality.
func filingOf(c store.RulePreviewCandidate) filing {
	switch {
	case len(c.UserLabelIDs) > 0:
		return filedByHand
	case c.HasAnyLabel:
		return filedByRule
	default:
		return filedNever
	}
}

// backfillTarget is one (message, label) pair `--backfill` would write an
// app.label_suggestions row for.
type backfillTarget struct {
	msgID    ulid.ULID
	labelID  ulid.ULID
	cond     rules.Condition
	evidence int
}

// backfillScan is one pass of the kept suggestions over the candidate scan: what
// --backfill would WRITE, and what it deliberately would NOT.
//
// The second half is not bookkeeping. "It ran and changed nothing" and "it ran
// and changed the wrong rows" are the two failures this project pays for, and a
// report that prints only the rows it would write cannot distinguish "no
// suggestion matches anything" from "every match is already filed" — which are
// opposite findings with opposite responses.
type backfillScan struct {
	// targets is every (message, label) row --backfill would write.
	targets []backfillTarget
	// rows counts targets per label — the per-label ROWS column. Computed here
	// rather than re-derived by the printer, so the table and the writes come from
	// one traversal.
	rows map[ulid.ULID]int
	// skippedHand and skippedRule count, PER LABEL, the messages a kept candidate
	// for that label matched and was NOT written for because the message already
	// carries a label. Together they are the report-side twin of
	// store.PutRuleSuggestion's guard — same population, measured from the same
	// scan — and SEPARATELY they are the #291 fix.
	//
	// ⚠ TWO MAPS, NOT ONE, BECAUSE THE TWO POPULATIONS MEAN OPPOSITE THINGS.
	// skippedHand is mail the user filed themselves: it is the EVIDENCE these very
	// candidates were induced from, it was excluded before v1.34.1 and after, and
	// no version of this command would ever have written it. skippedRule is mail
	// automation filed: it is exactly what v1.34.1's guard prevents, and before
	// that guard those rows WOULD have been written and would have dragged
	// categorised mail back into the needs-a-category queue. Merged, the pair read
	// as the second thing — 165 where the truth was 42 — which overstates the fix
	// fourfold and makes the guard look like it is doing four times the work it is.
	skippedHand map[ulid.ULID]int
	skippedRule map[ulid.ULID]int
	// skippedHandMsgs / skippedRuleMsgs are the DISTINCT message counts behind the
	// two maps — a message matched by three labels is one message that stays out of
	// the queue, not three. The per-label numbers do not sum to these, which is why
	// both forms exist.
	skippedHandMsgs int
	skippedRuleMsgs int
}

// scanBackfill computes, from the FINAL kept suggestions and the full candidate
// scan, every (message, label) pair a suggestion reaches — through
// candidateMatches (suggestreport.go), the SAME matcher suggest-report's REACHED
// figure is computed from, so the two are comparable rather than two measurements
// that merely sound alike. (Comparable, NOT equal: see this file's header for the
// two exclusions that make this one a strict subset.)
//
// ⚠ ONLY MESSAGES WITH NO LABEL AT ALL QUALIFY — HasAnyLabel, never
// len(UserLabelIDs) == 0. That is the #242 follow-up. The original filter asked
// "did the USER file this?", which is the right question for induction EVIDENCE
// and the wrong one for the suggestion TARGET population: it counted mail an
// accepted rule had already filed as a target, and on a real mailbox a fair share of
// proposed suggestions landed there. Since a suggestion row is itself one of
// the two arms of the needs-a-category predicate, writing them would have dragged
// already-categorised messages back into the review queue.
// store.PutRuleSuggestion refuses those writes; this function is what makes the
// REPORT agree with the writes instead of over-promising.
// TestScanBackfillSkipsEveryFiledMessage pins both halves.
//
// `kept` must already be sorted so the STRONGEST candidate for a label comes
// first (this file sorts by covers descending within each label, right
// before this is called): the first match per (message, label) wins, so the
// evidence a written row carries is the same evidence the report table
// above shows for that label.
func scanBackfill(kept []suggestion, candidates []store.RulePreviewCandidate) backfillScan {
	out := backfillScan{
		rows:        map[ulid.ULID]int{},
		skippedHand: map[ulid.ULID]int{},
		skippedRule: map[ulid.ULID]int{},
	}
	for _, c := range candidates {
		facts := factsFromCandidate(c)
		// WHO filed it, decided ONCE per message through the same function
		// splitWorkload's denominator uses (#291): the skip table and the workload
		// table have to be talking about the same three populations.
		filed := filingOf(c)
		claimed := map[ulid.ULID]bool{}
		for _, s := range kept {
			if claimed[s.label.ID] {
				continue
			}
			if !candidateMatches(s.conditions, s.label.Name, facts) {
				continue
			}
			claimed[s.label.ID] = true
			// THE MATCH IS COUNTED EITHER WAY; only the destination differs. The
			// filed test sits HERE and not at the top of the loop precisely so a
			// skipped message is measured rather than never looked at — a filter
			// that skips before matching can only ever report zero.
			//
			// AND THE TWO SKIP DESTINATIONS ARE NOT ONE (#291): whether the label
			// came from the user or from a rule is the difference between "this was
			// never a target" and "this is what the v1.34.1 guard prevents".
			switch filed {
			case filedByHand:
				out.skippedHand[s.label.ID]++
			case filedByRule:
				out.skippedRule[s.label.ID]++
			default:
				out.rows[s.label.ID]++
				out.targets = append(out.targets, backfillTarget{
					msgID: c.ID, labelID: s.label.ID, cond: s.conditions, evidence: s.positives,
				})
			}
		}
		if len(claimed) > 0 {
			switch filed {
			case filedByHand:
				out.skippedHandMsgs++
			case filedByRule:
				out.skippedRuleMsgs++
			}
		}
	}
	return out
}

// printPerLabel renders the per-label reach table: what each kept suggestion
// would write, and what it matched but declined because the mail is already
// filed. Labels with neither are omitted — a table of zeros is noise, and every
// kept suggestion's own line is printed above regardless.
func (s backfillScan) printPerLabel(kept []suggestion) {
	name := map[ulid.ULID]string{}
	var ids []ulid.ULID
	seen := map[ulid.ULID]bool{}
	for _, k := range kept {
		name[k.label.ID] = k.label.Name
		empty := s.rows[k.label.ID] == 0 && s.skippedHand[k.label.ID] == 0 && s.skippedRule[k.label.ID] == 0
		if seen[k.label.ID] || empty {
			continue
		}
		seen[k.label.ID] = true
		ids = append(ids, k.label.ID)
	}
	if len(ids) == 0 {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return name[ids[i]] < name[ids[j]] })
	// THREE COLUMNS, NOT TWO (#291). The old SKIPPED column merged mail the user
	// filed by hand with mail a rule filed, and only the second is a message the
	// v1.34.1 guard kept out of the queue; the first is the candidate's own
	// evidence and was never going to be written by any version of this command.
	fmt.Printf("\n%-24s %8s %10s %10s\n", "LABEL", "ROWS", "SKIP hand", "SKIP rule")
	for _, id := range ids {
		fmt.Printf("%-24s %8d %10d %10d\n",
			truncate(name[id], 24), s.rows[id], s.skippedHand[id], s.skippedRule[id])
	}
	fmt.Printf("  ROWS      = written.\n")
	fmt.Printf("  SKIP hand = YOU filed it — never a target: this is the evidence the candidate was\n")
	fmt.Printf("              induced FROM, excluded before v1.34.1 and after.\n")
	fmt.Printf("  SKIP rule = a RULE filed it — this is the population v1.34.1's guard prevents from\n")
	fmt.Printf("              re-entering the needs-a-category queue.\n")
}

// printFiledSkips renders the two distinct-message skip totals, in both the main
// report and the --backfill banner.
//
// ONE FUNCTION FOR BOTH SURFACES so the two cannot drift into printing different
// decompositions of the same scan — the single merged figure they used to share
// is the #291 defect, and a fix applied to one surface only would leave the other
// still overstating the guard's work fourfold.
// ⚠ THE MERGED TOTAL IS NOT PRINTED AT ALL, not even as a headline above the
// split. That is deliberate and it is the fix: "165 already-categorised
// message(s) matched and are skipped" is read as the size of what the v1.34.1
// guard prevents, and no decomposition printed underneath un-reads it — the
// figure has already been taken by then. Per-case counts, never one total
// (CLAUDE.md). The sum is trivially available to anyone who actually wants it.
func (s backfillScan) printFiledSkips() {
	fmt.Printf("already-categorised message(s) a suggestion MATCHED and is not written for — two\n")
	fmt.Printf("populations, deliberately never one number (#291):\n")
	fmt.Printf("  %d you filed BY HAND — never a target, at any version: this is the mail these very\n", s.skippedHandMsgs)
	fmt.Printf("    candidates were induced FROM (their own positives), excluded before v1.34.1 and after.\n")
	fmt.Printf("  %d filed BY A RULE — and this one IS what v1.34.1's guard prevents: before it, these\n", s.skippedRuleMsgs)
	fmt.Printf("    would have been written and would have re-entered the needs-a-category queue.\n")
}

// runSuggestBackfill writes app.label_suggestions rows for the UNCATEGORISED
// mail `scan` identifies — the #242 follow-up. The targets and the reach numbers
// already printed above are computed from the SAME kept candidates through the
// SAME matcher, so this is not a second measurement of the mailbox: it is the
// first one, written down.
func runSuggestBackfill(
	ctx context.Context, d *deps, userID ulid.ULID,
	scan backfillScan, uncategorised int,
	apply bool, maxMut int,
) error {
	fmt.Println()
	fmt.Println("BACKFILL (#242 follow-up) — writing suggestions for mail already in the mailbox,")
	fmt.Println("not just mail that arrives from here on.")
	// ⚠ THE CLAIM THIS BANNER USED TO MAKE — "should match suggest-report's
	// REACHED" — WAS FALSE, and printing a false invariant is worse than printing
	// none: the next reader chases a defect that does not exist. See this file's
	// header for the two exclusions; only the inequality is meaningful.
	fmt.Println("Its count is a SUBSET of suggest-report's REACHED, never equal to it: this command")
	fmt.Println("also drops candidates an accepted rule already covers, and messages that already")
	fmt.Println("carry ANY label (suggest-report counts mail a RULE filed as unlabelled). For the")
	fmt.Println("same corpus and --min-covers, expect suggest-rules <= suggest-report; the reverse")
	fmt.Println("would be a defect, a gap would not.")
	// THE ORDER, REPEATED HERE (#294) rather than left several screens up: this is
	// the surface that WRITES, and a --backfill run made before `accept-rules` fills
	// the needs-a-category queue with questions that mail the user already filed
	// answers — a large share of them on a real mailbox.
	fmt.Println("Run `accept-rules --apply` FIRST and this command second: a suggestion ASKS, a rule")
	fmt.Println("FILES, and candidates accept-rules would take are left to it (--max-collateral).")

	msgs := map[ulid.ULID]bool{}
	for _, t := range scan.targets {
		msgs[t.msgID] = true
	}
	abstain := uncategorised - len(msgs)
	fmt.Printf("\n%d suggestion row(s) over %d uncategorised message(s) (%d messages reached).\n",
		len(scan.targets), uncategorised, len(msgs))
	// AGAIN ON ITS OWN LINE, deliberately repeated from the report above: this
	// is what a --backfill run leaves BEHIND — how much of the abstention
	// queue this pass did NOT touch — and it must not be inferred by
	// subtraction from a table several screens up (#242 requires it explicit).
	fmt.Printf("%d uncategorised message(s) would receive NOTHING (abstention; unchanged by this run).\n", abstain)
	// Repeated for the same reason, and it is the number that says this run will
	// not INFLATE the needs-a-category queue — SPLIT, because merged it claims
	// credit the guard has not earned (#291). The per-label breakdown is the two
	// SKIP columns in the table above.
	scan.printFiledSkips()

	if !apply {
		fmt.Printf("\nDRY RUN — would write %d suggestion row(s). Re-run with --apply --backfill.\n", len(scan.targets))
		return nil
	}

	written, deferred := 0, 0
	for _, t := range scan.targets {
		if written >= maxMut {
			deferred++
			continue
		}
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			return tx.PutRuleSuggestion(ctx, t.msgID, t.labelID, t.cond, t.evidence)
		}); err != nil {
			return fmt.Errorf("suggest-rules backfill: message %s label %s: %w", t.msgID, t.labelID, err)
		}
		written++
	}
	// HONEST ABOUT WHAT THIS NUMBER IS: calls that returned without error, not
	// rows proven present. store.PutRuleSuggestion silently skips a message that
	// gained a label between the scan above and this write, so a label applied
	// mid-run makes this over-count by that many. The gap is bounded by concurrent
	// labelling during one operator run — normally zero — and it errs in the safe
	// direction (claiming a write that did not happen, never hiding one that did).
	// Removing it entirely would mean a per-row read-back, which is not worth a
	// round trip per message to restate what the scan already reported per label.
	fmt.Printf("\n%d suggestion row(s) written.\n", written)
	if deferred > 0 {
		// A BOUNDED RESULT MUST REPORT ITS BOUND (CLAUDE.md) — a partial
		// backfill that read like a complete one is the exact failure mode
		// apply-rules' own --max-mutations note exists to avoid.
		fmt.Printf("STOPPED AT THE --max-mutations BOUND of %d. This is a PARTIAL backfill (%d row(s)\n"+
			"deferred); re-run to continue, or raise the bound. A re-run is idempotent: PutRuleSuggestion\n"+
			"upserts on (user_id, message_id, label_id), so it converges rather than duplicating.\n",
			maxMut, deferred)
	}
	return nil
}

// suggestRuleName builds a stable, deterministic name for a suggestLabel
// rule, so a re-run collides with itself (store.ErrRuleNameTaken) rather
// than duplicating. TestSuggestRuleNameDeterministic pins the "same inputs,
// same name" half of that; ErrRuleNameTaken handling above pins the rest.
//
// DELIBERATELY A DIFFERENT PREFIX FROM accept.go's ruleName, not the same
// scheme reused. accept.go's own comment records what happens when two
// generators can produce the same name for related-but-different rules: "a
// name is only a proxy for identity while one generator owns it", and this
// command and accept-rules are two generators writing to the same UNIQUE
// (user_id, name) namespace. If they rendered "Shop: alias equals ..."
// identically, then accept-rules creating that exact rule after suggest-rules
// already had would fail with ErrRuleNameTaken against a rule of the WRONG
// action type — reported as "exists" when nothing resembling what
// accept-rules would have created exists at all. The "Suggested: " prefix
// keeps the two namespaces disjoint by construction, not by convention.
func suggestRuleName(label string, c rules.Condition) string {
	name := "Suggested: " + label + ": " + describe(c)
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}
