// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// TestSuggestFilterKeepsHighCollateralCandidate pins the INVERSE of the #248
// defect suggest-report shipped with and later fixed: a candidate that
// reaches unlabelled mail has Collateral > 0 BY DEFINITION (Collateral counts
// the non-positive messages a candidate matches, and every unlabelled message
// is non-positive), so this command's filter must not reject on it. Filtering
// on collateral here — the way accept-rules filters, correctly, for its own
// very different purpose — would force suggestFilter to keep nothing, on
// every corpus, forever.
func TestSuggestFilterKeepsHighCollateralCandidate(t *testing.T) {
	c := rules.Candidate{Conflicts: 0, Covers: 5, Positives: 5, Collateral: 50}
	if got := suggestFilter(c, 2); got != suggestKept {
		t.Fatalf("suggestFilter(%+v) = %v, want suggestKept — a candidate reaching unlabelled mail "+
			"must not be rejected for having reached it (#248)", c, got)
	}
}

// TestSuggestFilterDropsConflicts is the one bar this command shares with
// accept-rules without exception: a conflict is mail the user filed under a
// DIFFERENT label by hand, so a high Covers count must not rescue it.
func TestSuggestFilterDropsConflicts(t *testing.T) {
	c := rules.Candidate{Conflicts: 1, Covers: 10, Positives: 10, Collateral: 0}
	if got := suggestFilter(c, 2); got != suggestSkipConflict {
		t.Fatalf("suggestFilter(%+v) = %v, want suggestSkipConflict", c, got)
	}
}

// TestSuggestFilterDropsInsufficientCovers checks the other bar: too little
// of the user's own labelling explains the candidate.
func TestSuggestFilterDropsInsufficientCovers(t *testing.T) {
	c := rules.Candidate{Conflicts: 0, Covers: 1, Positives: 1, Collateral: 0}
	if got := suggestFilter(c, 2); got != suggestSkipCovers {
		t.Fatalf("suggestFilter(%+v) with --min-covers=2 = %v, want suggestSkipCovers", c, got)
	}
}

// TestBelongsToAcceptRulesRoutesOnlyTheSafeOnes is #294's predicate at its
// boundary — and the boundary is where the whole argument lives, because the
// SAME arithmetic with the opposite sign is the #248 defect.
//
// #248: keep a candidate only if collateral <= 0. Every unlabelled message is
// non-positive and therefore collateral, so that keeps only candidates that reach
// nothing, and the report is empty on every corpus forever.
//
// #294 (this): SKIP a candidate if collateral <= 0, and keep the rest. The
// survivors are exactly the collateral > 0 population #248 restored — the ones
// that DO reach unlabelled mail — so the bound cannot empty this command. What it
// removes is candidates with nothing to ask about, which `accept-rules` should
// file instead.
//
// The two cases below are the two halves of that claim, and neither is redundant:
// without the second, a predicate returning true always would pass.
func TestBelongsToAcceptRulesRoutesOnlyTheSafeOnes(t *testing.T) {
	if !belongsToAcceptRules(0, 0) {
		t.Error("belongsToAcceptRules(collateral 0, bound 0) = false — a candidate matching nothing " +
			"beyond mail the user already filed under that label is one accept-rules FILES; asking " +
			"about it instead adds a question to the needs-a-category queue that the user's own " +
			"filings answer")
	}
	// ⚠ THE #248 SIDE. A candidate with real collateral is the only kind that can
	// produce a suggestion at all, so it must survive this test — otherwise the
	// bound would empty the command exactly the way #248's did.
	if belongsToAcceptRules(50, 0) {
		t.Error("belongsToAcceptRules(collateral 50, bound 0) = true — a candidate that REACHES " +
			"unlabelled mail was routed away, which is #248 with extra steps: the population that " +
			"survives this bound must be precisely the population that has something to suggest")
	}
	// The bound is a bound, not a synonym for zero: at 5 a candidate touching 3
	// extra messages is accept-rules' business, at 0 it is this command's. Pinned
	// because the flag would otherwise be free to be ignored and every test above
	// would still pass.
	if belongsToAcceptRules(3, 0) {
		t.Error("belongsToAcceptRules(collateral 3, bound 0) = true — 3 is above the bound")
	}
	if !belongsToAcceptRules(3, 5) {
		t.Error("belongsToAcceptRules(collateral 3, bound 5) = false — --max-collateral is not being " +
			"read; the predicate is hard-wired to 0")
	}
}

// TestSuggestSkipsRouteAppliesTheBarAndNothingElse pins the per-candidate stage:
// conflicts and covers, tallied separately, and NOTHING about collateral.
//
// ⚠ A ZERO-COLLATERAL CANDIDATE MUST SURVIVE THIS FUNCTION. That is not an
// oversight, it is the #294 correction: whether a candidate belongs to
// `accept-rules` cannot be decided here, because a candidate an existing rule
// ALREADY COVERS belongs in the "already covered" bucket instead, and supersession
// is only knowable once the whole set is in hand. Deciding it early is what made
// this command report "48 belong to accept-rules — run it first" while
// accept-rules reported "nothing to do" on the same mailbox in the same hour.
// suggestSkips.partition owns that decision now, and this asserts route has given
// it up.
//
// ⚠ THE FIXTURE'S COUNTS ARE ALL DIFFERENT (kept 7, conflicts 2, covers 1) ON
// PURPOSE. With any two equal, a tally that incremented the wrong field — the
// single most likely way this goes wrong — would still satisfy every assertion.
// This is the same reason the #291 fixture is 3-to-1.
//
// AND THE CONFLICT ROWS SIT AT COLLATERAL 0 deliberately: they are the rows that
// would land in the wrong bucket if any collateral test ever crept back in front
// of the bar. accept-rules refuses conflicts too, so counting them as its work
// would advise an action that does nothing with them.
func TestSuggestSkipsRouteAppliesTheBarAndNothingElse(t *testing.T) {
	const minCovers = 2
	cases := []struct {
		what string
		c    rules.Candidate
		keep bool
	}{
		{"reaches unlabelled mail", rules.Candidate{Covers: 5, Positives: 5, Collateral: 50}, true},
		{"reaches unlabelled mail", rules.Candidate{Covers: 4, Positives: 4, Collateral: 9}, true},
		{"reaches unlabelled mail", rules.Candidate{Covers: 3, Positives: 3, Collateral: 2}, true},
		// COLLATERAL 1 IS THE NARROWEST THING THIS COMMAND STILL SUGGESTS FOR: one
		// unlabelled message. If any bound ever swallowed this, #248 would be back.
		{"reaches exactly one unlabelled message", rules.Candidate{Covers: 2, Positives: 2, Collateral: 1}, true},
		// ⚠ COLLATERAL 0, AND STILL KEPT HERE. partition decides these, after
		// supersession has had first refusal on them.
		{"accept-rules' own, decided later", rules.Candidate{Covers: 9, Positives: 9, Collateral: 0}, true},
		{"accept-rules' own, decided later", rules.Candidate{Covers: 4, Positives: 4, Collateral: 0}, true},
		{"accept-rules' own, decided later", rules.Candidate{Covers: 2, Positives: 2, Collateral: 0}, true},
		{"conflicting, and safe-looking", rules.Candidate{Covers: 10, Positives: 10, Collateral: 0, Conflicts: 1}, false},
		{"conflicting, and safe-looking", rules.Candidate{Covers: 8, Positives: 8, Collateral: 0, Conflicts: 3}, false},
		{"too little evidence", rules.Candidate{Covers: 1, Positives: 1, Collateral: 7}, false},
	}

	var (
		skips suggestSkips
		kept  int
	)
	for _, tc := range cases {
		if got := skips.route(tc.c, minCovers); got != tc.keep {
			t.Errorf("route(%s: %+v) = %v, want %v", tc.what, tc.c, got, tc.keep)
		} else if got {
			kept++
		}
	}

	if kept != 7 {
		t.Errorf("kept %d candidate(s), want 7 — every candidate clearing the bar must reach "+
			"partition, INCLUDING the zero-collateral ones: routing them away here is what let "+
			"this command claim 48 candidates for accept-rules while accept-rules had nothing "+
			"to do (#294)", kept)
	}
	if skips.conflicts != 2 {
		t.Errorf("skips.conflicts = %d, want 2 — a CONFLICTING candidate at collateral 0 must be "+
			"counted as a conflict, not as one accept-rules would take: accept-rules refuses "+
			"conflicts too, so counting it there tells the operator to run a command that will "+
			"not touch it", skips.conflicts)
	}
	if skips.covers != 1 {
		t.Errorf("skips.covers = %d, want 1", skips.covers)
	}
	// THE LATER BUCKETS MUST STILL BE EMPTY. Route touching any of them is the
	// regression this whole reordering exists to prevent, and it would otherwise be
	// invisible: the totals would still add up.
	if skips.acceptable != 0 || skips.superseded != 0 || skips.duplicates != 0 {
		t.Errorf("after route only: acceptable = %d, superseded = %d, duplicates = %d, want 0, 0 and "+
			"0 — none can be decided per candidate, and deciding any of them here is the #294 "+
			"ordering defect", skips.acceptable, skips.superseded, skips.duplicates)
	}
	// AND THE NUMBERS MUST ACCOUNT FOR EVERY CANDIDATE. A reason silently dropped
	// on the floor is a report whose parts do not add up to its whole, which is
	// how "nothing to do" and "the thing I measured is always empty" become
	// indistinguishable (CLAUDE.md).
	if got := kept + skips.conflicts + skips.covers; got != len(cases) {
		t.Errorf("kept + the two bar reasons = %d over %d candidates — the bar is not a partition "+
			"and some candidates are unaccounted for", got, len(cases))
	}
}

// TestPartitionCountsSupersededBeforeAcceptable is the ordering fix, and it is
// pinned by a candidate that qualifies for BOTH buckets.
//
// THE DEFECT, MEASURED LIVE. #294's routing first ran inside `route`, per
// candidate, before the supersession pass. An hour after the operator had run
// `accept-rules --apply`, the two commands said:
//
//	suggest-rules : 48 belong to `accept-rules` … ⚠ run `accept-rules --apply` first
//	accept-rules  : every proposal that met the bar is already covered by a rule
//	                you have; nothing to do
//
// 48 against 0. Every one of the 48 was by then superseded by a real rule, and the
// early tally could not see it. That is not a loose bound — in the steady state of
// a worked mailbox it is maximally loose, and the ⚠ it printed instructed the
// reader to take an action that would do nothing. A plausible number meaning
// something other than what its reader takes it for is precisely the defect class
// #291 and #294 exist to fix.
//
// ⚠ SO THE FIXTURE'S FIRST CASE IS ZERO-COLLATERAL AND SUPERSEDED AT ONCE. Under
// the old order it lands in `acceptable`; under the new one in `superseded`. A
// fixture without such a candidate passes against BOTH implementations and pins
// nothing — the counts alone are what tell the two apart, which is why they are
// deliberately unequal (superseded 3, acceptable 1, kept 2).
func TestPartitionCountsSupersededBeforeAcceptable(t *testing.T) {
	domain := func(v string) rules.Condition {
		return rules.Condition{All: []rules.Condition{
			{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: v},
		}}
	}
	// The saved rule's test PLUS one more restriction: strictly narrower, so it can
	// never fire on anything the saved rule misses.
	narrower := func(v string) rules.Condition {
		return rules.Condition{All: []rules.Condition{
			{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: v},
			{Field: rules.FieldListUnsub, Op: rules.OpExists},
		}}
	}
	label := func(n string) store.LabelView { return store.LabelView{ID: ulid.Make(), Name: n} }

	var (
		safeAndCovered = label("SafeAndCovered")
		coveredWide    = label("CoveredWide")
		coveredNarrow  = label("CoveredNarrow")
		acceptable     = label("Acceptable")
		freshWide      = label("FreshWide")
		freshOtherRule = label("FreshOtherRule")
		someoneElse    = label("SomeoneElse")
	)

	saved := []savedRule{{
		name:    "Shop → filed",
		enabled: true,
		labels: map[ulid.ULID]bool{
			safeAndCovered.ID: true, coveredWide.ID: true, coveredNarrow.ID: true,
		},
		conjuncts: conjuncts(domain("shop.example")),
	}, {
		// DISCRIMINATION CONTROL: identical conditions, a DIFFERENT label. It must
		// not supersede anything below, or "covered" would mean "some rule somewhere
		// tests this" rather than "this label is already being applied".
		name:      "News → someone else's label",
		enabled:   true,
		labels:    map[ulid.ULID]bool{someoneElse.ID: true},
		conjuncts: conjuncts(domain("news.example")),
	}}

	in := []suggestion{
		// ⚠ THE CASE THE WHOLE TEST IS FOR: collateral 0 (so accept-rules' bound
		// claims it) AND already covered by a saved rule (so it is not accept-rules'
		// work at all — accept-rules would report it as already covered).
		{label: safeAndCovered, conditions: domain("shop.example"), positives: 6, covers: 6, collateral: 0},
		{label: coveredWide, conditions: domain("shop.example"), positives: 5, covers: 5, collateral: 40},
		{label: coveredNarrow, conditions: narrower("shop.example"), positives: 4, covers: 4, collateral: 12},
		{label: acceptable, conditions: domain("promo.example"), positives: 3, covers: 3, collateral: 0},
		{label: freshWide, conditions: domain("deals.example"), positives: 7, covers: 7, collateral: 30},
		{label: freshOtherRule, conditions: domain("news.example"), positives: 2, covers: 2, collateral: 5},
	}
	total := len(in)

	var skips suggestSkips
	// partition reuses the input's backing array, so nothing may read `in` after
	// this line — the survivors are identified from the returned slice alone.
	fresh := skips.partition(in, saved, 0)

	if skips.superseded != 3 {
		t.Errorf("skips.superseded = %d, want 3 — a candidate a rule of yours already covers belongs "+
			"here WHATEVER its collateral. Counting the zero-collateral one as accept-rules' work "+
			"instead is the live 48-against-0: this command says 'run accept-rules first' and "+
			"accept-rules says 'nothing to do'", skips.superseded)
	}
	if skips.acceptable != 1 {
		t.Errorf("skips.acceptable = %d, want 1 — only the candidate that is BOTH safe and not "+
			"already covered is genuinely left for accept-rules; anything else here advises work "+
			"that is already done", skips.acceptable)
	}
	if len(fresh) != 2 {
		t.Errorf("partition kept %d suggestion(s), want 2", len(fresh))
	}
	survived := map[string]bool{}
	for _, s := range fresh {
		survived[s.label.Name] = true
	}
	// The negative control: a saved rule with the same conditions for a DIFFERENT
	// label must not have superseded this one.
	if !survived[freshOtherRule.Name] {
		t.Errorf("%q was dropped, but the only rule with its conditions applies a different label — "+
			"supersession is ignoring the label, so it would silence suggestions for labels no rule "+
			"applies at all", freshOtherRule.Name)
	}
	if !survived[freshWide.Name] {
		t.Errorf("%q was dropped though nothing covers it and its collateral is 30", freshWide.Name)
	}
	// And the two that must NOT be here, checked by name rather than inferred from
	// the length: a miscount could make the length right for the wrong reason.
	if survived[safeAndCovered.Name] || survived[acceptable.Name] {
		t.Errorf("a superseded or accept-able suggestion survived partition: %v", survived)
	}
	// THE FOUR BUCKETS STILL PARTITION THE SET. Reordering two tests is exactly the
	// kind of edit that loses a candidate between them, and a lost candidate is
	// invisible in every printed number.
	if got := len(fresh) + skips.superseded + skips.acceptable; got != total {
		t.Errorf("kept + superseded + acceptable = %d over %d suggestions — partition is not a "+
			"partition and some candidates are unaccounted for", got, total)
	}
	if skips.conflicts != 0 || skips.covers != 0 {
		t.Errorf("partition touched the bar counters (conflicts %d, covers %d) — they belong to "+
			"route, and a candidate counted twice is a report whose reasons overlap",
			skips.conflicts, skips.covers)
	}
}

// TestPipelineAccountsForEveryInducedCandidate drives all three tallying stages —
// propose (the bar), dedup, partition — and asserts that the five skip buckets
// plus the survivors PARTITION the induced candidate set.
//
// WHY THE EQUALITY IS THE POINT. Both defects this release fixes were populations
// in the wrong bucket or in none: #291 merged two skip populations into one
// number, #294 counted superseded candidates as work waiting for another command.
// And this test exists because a third instance survived the fix for the first
// two — dedup-dropped candidates were counted nowhere, measured at 3 when the
// operator diffed visible candidates across two builds (120 → 117). Its severity
// was lower than the other two (no printed number contradicted it; a reader would
// have had to sum the buckets against a total the report never printed) but a
// known gap and an unmeasured one are different things. An equality that has to
// hold catches the whole class, including the next one nobody has thought of.
//
// ⚠ ALL SIX COUNTS ARE DIFFERENT (kept 6, conflicts 1, covers 2, duplicates 3,
// superseded 4, acceptable 5) ON PURPOSE. The equality alone is a totals-only
// assertion: it holds just as well if a candidate is counted in the WRONG bucket,
// which is defect #294 exactly. Only per-bucket assertions over unequal counts can
// tell a miscount from a misfiling, so both are here.
func TestPipelineAccountsForEveryInducedCandidate(t *testing.T) {
	const minCovers = 2

	dom := func(v string) rules.Condition {
		return rules.Condition{All: []rules.Condition{
			{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: v},
		}}
	}
	// Two DIFFERENT ways to be strictly narrower than dom(v), so the duplicate
	// bucket is not exercised by one shape repeated.
	domUnsub := func(v string) rules.Condition {
		return rules.Condition{All: []rules.Condition{
			{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: v},
			{Field: rules.FieldListUnsub, Op: rules.OpExists},
		}}
	}
	domSubject := func(v string) rules.Condition {
		return rules.Condition{All: []rules.Condition{
			{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: v},
			{Field: rules.FieldSubject, Op: rules.OpContains, Value: "Sale"},
		}}
	}
	cand := func(c rules.Condition, covers, collateral, conflicts int) rules.Candidate {
		return rules.Candidate{
			Conditions: c, Covers: covers, Positives: covers,
			Collateral: collateral, Conflicts: conflicts,
		}
	}

	alpha := store.LabelView{ID: ulid.Make(), Name: "Alpha"}
	beta := store.LabelView{ID: ulid.Make(), Name: "Beta"}
	gamma := store.LabelView{ID: ulid.Make(), Name: "Gamma"}

	// Saved rules for Beta only, one per condition, so each superseded candidate is
	// covered by its own rule rather than all four by one — and so nothing in Alpha
	// or Gamma can be superseded by accident.
	var saved []savedRule
	for _, d := range []string{"b1.example", "b2.example", "b3.example", "b4.example"} {
		saved = append(saved, savedRule{
			name:      "Beta ← " + d,
			enabled:   true,
			labels:    map[ulid.ULID]bool{beta.ID: true},
			conjuncts: conjuncts(dom(d)),
		})
	}

	groups := []struct {
		label   store.LabelView
		induced []rules.Candidate
	}{{
		label: alpha,
		induced: []rules.Candidate{
			cand(dom("a1.example"), 9, 20, 0),       // KEPT
			cand(domUnsub("a1.example"), 5, 4, 0),   // DUPLICATE of the above
			cand(domSubject("a1.example"), 4, 3, 0), // DUPLICATE, by a different conjunct
			cand(dom("a2.example"), 8, 15, 0),       // KEPT
			cand(domUnsub("a2.example"), 3, 2, 0),   // DUPLICATE
			cand(dom("a3.example"), 7, 6, 1),        // CONFLICT
			cand(dom("a4.example"), 1, 5, 0),        // COVERS
			cand(dom("a5.example"), 1, 5, 0),        // COVERS
		},
	}, {
		label: beta,
		induced: []rules.Candidate{
			// ⚠ COLLATERAL 0 AND SUPERSEDED AT ONCE — the #294 ordering case, carried
			// here too so the partition cannot be satisfied by counting it as
			// acceptable.
			cand(dom("b1.example"), 6, 0, 0),  // SUPERSEDED
			cand(dom("b2.example"), 5, 11, 0), // SUPERSEDED
			cand(dom("b3.example"), 4, 12, 0), // SUPERSEDED
			cand(dom("b4.example"), 3, 13, 0), // SUPERSEDED
		},
	}, {
		label: gamma,
		induced: []rules.Candidate{
			cand(dom("c1.example"), 6, 0, 0),  // ACCEPTABLE
			cand(dom("c2.example"), 5, 0, 0),  // ACCEPTABLE
			cand(dom("c3.example"), 4, 0, 0),  // ACCEPTABLE
			cand(dom("c4.example"), 3, 0, 0),  // ACCEPTABLE
			cand(dom("c5.example"), 2, 0, 0),  // ACCEPTABLE
			cand(dom("c6.example"), 9, 8, 0),  // KEPT
			cand(dom("c7.example"), 8, 9, 0),  // KEPT
			cand(dom("c8.example"), 7, 7, 0),  // KEPT
			cand(dom("c9.example"), 6, 10, 0), // KEPT
		},
	}}

	total := 0
	var skips suggestSkips
	var proposed []suggestion
	for _, g := range groups {
		total += len(g.induced)
		proposed = append(proposed, skips.propose(g.label, g.induced, minCovers)...)
	}
	fresh := skips.partition(skips.dedup(proposed), saved, 0)

	for _, b := range []struct {
		what      string
		got, want int
		why       string
	}{
		{"kept", len(fresh), 6, "the suggestions actually offered"},
		{"conflicts", skips.conflicts, 1, "mail filed under a different label by hand"},
		{"covers", skips.covers, 2, "too little of the user's own labelling"},
		{"duplicates", skips.duplicates, 3,
			"narrower forms of a rule offered above — the bucket that did not exist until it " +
				"was measured on a real corpus"},
		{"superseded", skips.superseded, 4, "a rule the user already has covers them"},
		{"acceptable", skips.acceptable, 5, "accept-rules would file them"},
	} {
		if b.got != b.want {
			t.Errorf("%s = %d, want %d (%s)", b.what, b.got, b.want, b.why)
		}
	}

	// ⚠ THE EQUALITY. Every candidate induction proposed must land in exactly one
	// of the six, and the report now PRINTS this claim, so it has to be true. A
	// candidate in none of them is a number nobody can reconcile; a candidate in
	// two is a reason counted twice.
	if got := len(fresh) + skips.conflicts + skips.covers + skips.duplicates +
		skips.superseded + skips.acceptable; got != total {
		t.Errorf("the six outcomes account for %d of %d induced candidate(s) — the pipeline is not a "+
			"partition. %d candidate(s) are in no bucket, which is how three dedup-dropped ones "+
			"went unreported until two builds were diffed against each other",
			got, total, total-got)
	}
}

// TestSuggestRuleNameDeterministic pins the idempotence property the file
// comment promises: same label and conditions must always render the same
// name, because that is the ONLY thing that makes a re-run collide with
// itself (store.ErrRuleNameTaken) instead of creating a duplicate rule.
//
// It also checks the name is DISTINCT from accept.go's ruleName for the same
// inputs, which is the property that keeps the two commands' generated rules
// out of each other's way in the shared UNIQUE (user_id, name) namespace (see
// the file comment on suggestRuleName).
func TestSuggestRuleNameDeterministic(t *testing.T) {
	cond := rules.Condition{All: []rules.Condition{
		{Field: rules.FieldAlias, Op: rules.OpEquals, Value: "shop@example.com"},
		{Field: rules.FieldListUnsub, Op: rules.OpExists},
	}}

	first := suggestRuleName("Shop", cond)
	second := suggestRuleName("Shop", cond)
	if first != second {
		t.Fatalf("suggestRuleName is not deterministic: %q != %q for identical inputs; "+
			"a re-run would create a duplicate rule instead of colliding on the name", first, second)
	}

	if other := ruleName("Shop", cond); first == other {
		t.Fatalf("suggestRuleName(%q) collided with accept.go's ruleName(%q) — "+
			"the two commands would fight over the same rule name for different action types", first, other)
	}
}

// shopDomainSuggestion and saleSubjectSuggestion are two kept suggestions for
// two DIFFERENT labels that both fire on the same fixture mail below. Two
// labels, not one, because the distinction between the per-label skip counters
// (skippedHand / skippedRule) and the DISTINCT message counts (skippedHandMsgs /
// skippedRuleMsgs) is only observable when a single message is matched by more
// than one of them — with one label the two forms are equal for every input and a
// test using one could not tell them apart.
func shopDomainSuggestion(label store.LabelView) suggestion {
	return suggestion{
		label: label,
		conditions: rules.Condition{All: []rules.Condition{
			{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: "shop.example"},
		}},
		positives: 4,
	}
}

func saleSubjectSuggestion(label store.LabelView) suggestion {
	return suggestion{
		label: label,
		conditions: rules.Condition{All: []rules.Condition{
			{Field: rules.FieldSubject, Op: rules.OpContains, Value: "Sale"},
		}},
		positives: 7,
	}
}

// TestScanBackfillSkipsEveryFiledMessage is the population pin for the #242
// follow-up, and it is named in scanBackfill's own doc comment.
//
// THE DEFECT IT GUARDS. The original filter asked `len(UserLabelIDs) == 0` —
// "did the USER file this?" — which is the right question for induction EVIDENCE
// and the wrong one for the suggestion TARGET population. A message an accepted
// rule had already filed answered "no user label", counted as a target, and got a
// suggestion written on top of a category it already had; because a suggestion
// row is one of the two arms of the needs-a-category predicate, that DRAGS an
// already-categorised message back into the review queue. On a real
// mailbox a fair share of proposed suggestions landed there.
//
// So the fixture mixes all three populations and every one of them MATCHES the
// kept suggestions. That is deliberate: a filed message that did not match would
// be excluded by the matcher rather than by the guard, and the test would pass
// against a scanBackfill with no guard at all.
//
// ⚠ AND THE SKIPS ARE ASSERTED AS COUNTS, not merely as absences from `targets`.
// That is the second half of the property, and the one that is easy to lose:
// scanBackfill tests the message's filing AFTER the match, on purpose, so a
// skipped message is measured rather than never looked at. Moving that test to
// the top of the loop — an obvious-looking "why match something we are going to
// skip" tidy-up — leaves `targets` exactly right and silently zeroes every skip
// counter, which is a report that cannot tell "no suggestion matches anything"
// from "every match is already filed". Those are opposite findings.
func TestScanBackfillSkipsEveryFiledMessage(t *testing.T) {
	promotions := store.LabelView{ID: ulid.Make(), Name: "Promotions"}
	receipts := store.LabelView{ID: ulid.Make(), Name: "Receipts"}
	kept := []suggestion{shopDomainSuggestion(promotions), saleSubjectSuggestion(receipts)}

	var (
		uncat1   = ulid.Make()
		uncat2   = ulid.Make()
		ruleFile = ulid.Make()
		handFile = ulid.Make()
		nomatch  = ulid.Make()
	)
	candidates := []store.RulePreviewCandidate{
		// UNCATEGORISED — no label of any kind. The target population.
		{ID: uncat1, From: "deals@shop.example", Subject: "Spring Sale"},
		{ID: uncat2, From: "offers@shop.example", Subject: "Autumn Sale"},
		// RULE-FILED — ⚠ THE WHOLE BUG. Categorised, but by automation, so it
		// contributes NO UserLabelIDs. The old filter read this as uncategorised.
		{ID: ruleFile, From: "deals@shop.example", Subject: "Winter Sale", HasAnyLabel: true},
		// HAND-FILED. Both fields set, because that is the only shape the store can
		// produce: attachLabelFacts sets HasAnyLabel for EVERY message_labels row and
		// appends to UserLabelIDs only for applied_by='user', so
		// `len(UserLabelIDs) > 0` implies HasAnyLabel. A fixture with UserLabelIDs
		// set and HasAnyLabel false would be testing a candidate no query can return.
		{ID: handFile, From: "deals@shop.example", Subject: "Summer Sale",
			HasAnyLabel: true, UserLabelIDs: []ulid.ULID{ulid.Make()}},
		// DISCRIMINATION CONTROL: uncategorised and matched by NEITHER suggestion.
		// Without it, a scanBackfill that matched everything would still satisfy
		// every assertion below.
		{ID: nomatch, From: "hello@other.example", Subject: "Hello"},
	}

	scan := scanBackfill(kept, candidates)

	targeted := map[ulid.ULID]int{}
	var pairs []string
	for _, tg := range scan.targets {
		targeted[tg.msgID]++
		// Rendered as strings: a ULID is a [16]byte, so %+v on the target struct
		// prints byte arrays and a failure here would be unreadable at exactly the
		// moment somebody needs to read it.
		pairs = append(pairs, tg.msgID.String()+"→"+tg.labelID.String())
	}
	// TARGETS: the two uncategorised messages, once per label.
	if len(scan.targets) != 4 {
		t.Fatalf("scanBackfill produced %d target(s), want 4 — two uncategorised messages × two labels:\n  %s",
			len(scan.targets), strings.Join(pairs, "\n  "))
	}
	for _, want := range []ulid.ULID{uncat1, uncat2} {
		if targeted[want] != 2 {
			t.Errorf("uncategorised message %s got %d target(s), want 2 (one per label)", want, targeted[want])
		}
	}
	// NEGATIVE CONTROL, checked against the ids rather than inferred from a
	// length: a fixture bug could make the count right for the wrong reason. These
	// are what actually prove a filed message was excluded BECAUSE it is filed.
	if n := targeted[ruleFile]; n != 0 {
		t.Errorf("a message a RULE had already filed produced %d backfill target(s), want 0 — "+
			"this is the 36-of-160 defect: the suggestion would pull categorised mail back into "+
			"the needs-a-category queue", n)
	}
	if n := targeted[handFile]; n != 0 {
		t.Errorf("a message the user filed BY HAND produced %d backfill target(s), want 0", n)
	}
	if n := targeted[nomatch]; n != 0 {
		t.Errorf("a message neither suggestion matches produced %d target(s), want 0 — the matcher "+
			"is not discriminating, so every other assertion here is vacuous", n)
	}

	// ROWS: the per-label write counts, from the same traversal as targets.
	for _, l := range []store.LabelView{promotions, receipts} {
		if got := scan.rows[l.ID]; got != 2 {
			t.Errorf("rows[%s] = %d, want 2", l.Name, got)
		}
	}

	// ⚠ SKIPPED — THE ASSERTION THE ORDER OF THE TESTS INSIDE scanBackfill EXISTS
	// FOR. Both filed messages match both suggestions, so each label declined two:
	// one hand-filed and one rule-filed (#291 split them; before that they shared a
	// counter). Asserted per bucket, because the bucket that matters for the guard
	// is the RULE one and a merged number hides it.
	for _, l := range []store.LabelView{promotions, receipts} {
		if got := scan.skippedHand[l.ID]; got != 1 {
			t.Errorf("skippedHand[%s] = %d, want 1 — a filed message a suggestion MATCHED must be "+
				"COUNTED as skipped, not filtered out before the match and thus invisible; a "+
				"report that always prints 0 here cannot distinguish 'nothing matched' from "+
				"'everything matched was already filed'", l.Name, got)
		}
		if got := scan.skippedRule[l.ID]; got != 1 {
			t.Errorf("skippedRule[%s] = %d, want 1 — this is the bucket v1.34.1's guard actually "+
				"acts on, and it is the one that must never be inferred from a merged total", l.Name, got)
		}
	}

	// AND THE DISTINCT MESSAGE COUNTS, which are NOT the sums of the per-label
	// numbers. This fixture is built so each pair is 1 and 2 — both forms are
	// asserted, because a distinct count that had collapsed into the (message,
	// label) pair count would still look plausible on its own.
	if scan.skippedHandMsgs != 1 || scan.skippedRuleMsgs != 1 {
		t.Errorf("skippedHandMsgs = %d, skippedRuleMsgs = %d, want 1 and 1 — a message matched by "+
			"two labels is ONE message that stays out of the needs-a-category queue, not two",
			scan.skippedHandMsgs, scan.skippedRuleMsgs)
	}
	if sum := scan.skippedHand[promotions.ID] + scan.skippedHand[receipts.ID]; sum != 2 {
		t.Errorf("the per-label hand skips total %d, want 2 — they count (message, label) PAIRS, and "+
			"if they had become a distinct-message count there would be nothing left measuring which "+
			"label declined what", sum)
	}
	if sum := scan.skippedRule[promotions.ID] + scan.skippedRule[receipts.ID]; sum != 2 {
		t.Errorf("the per-label rule skips total %d, want 2", sum)
	}
}

// TestScanBackfillSplitsHandFiledFromRuleFiledSkips is #291, and the fixture's
// ASYMMETRY is the entire test.
//
// THE DEFECT. The report printed one number for both filed populations: "165
// already-categorised message(s) matched and are skipped". A reader takes that as
// the size of what v1.34.1's guard prevents. It is not. Decomposed by label
// provenance on a real corpus, most of the filed mail (123 of 165 in the fixture) was HAND-filed — the candidates'
// own positives, the mail each rule was induced FROM, excluded before v1.34.1 by
// `len(UserLabelIDs) == 0` and excluded after it by HasAnyLabel — and only the rest (42 in the fixture) were
// rule-filed, i.e. messages that would otherwise have re-entered the queue. The
// single figure overstated the fix roughly fourfold.
//
// ⚠ SO THE TWO BUCKETS ARE GIVEN DIFFERENT COUNTS ON PURPOSE: three hand-filed
// messages against one rule-filed one. A test built with equal numbers, or one
// asserting only their total, PASSES AGAINST THE MERGED BEHAVIOUR THIS FIXES —
// which is exactly how the defect survived a suite that already covered "filed
// mail is skipped". Only the inequality can tell the two implementations apart.
func TestScanBackfillSplitsHandFiledFromRuleFiledSkips(t *testing.T) {
	promotions := store.LabelView{ID: ulid.Make(), Name: "Promotions"}
	kept := []suggestion{shopDomainSuggestion(promotions)}

	hand := func() store.RulePreviewCandidate {
		return store.RulePreviewCandidate{
			ID: ulid.Make(), From: "deals@shop.example", Subject: "Sale",
			HasAnyLabel: true, UserLabelIDs: []ulid.ULID{ulid.Make()},
		}
	}
	candidates := []store.RulePreviewCandidate{
		hand(), hand(), hand(),
		// Exactly ONE filed only by a rule.
		{ID: ulid.Make(), From: "deals@shop.example", Subject: "Sale", HasAnyLabel: true},
		// One uncategorised, so `rows` is non-zero and the fixture cannot be
		// satisfied by a scanBackfill that simply skips everything.
		{ID: ulid.Make(), From: "deals@shop.example", Subject: "Sale"},
	}

	scan := scanBackfill(kept, candidates)

	if scan.skippedHandMsgs != 3 {
		t.Errorf("skippedHandMsgs = %d, want 3 — mail the USER filed is the candidate's own evidence "+
			"and was never a target under any version; counting it as what the guard prevents is "+
			"the #291 defect", scan.skippedHandMsgs)
	}
	if scan.skippedRuleMsgs != 1 {
		t.Errorf("skippedRuleMsgs = %d, want 1 — this is the ONLY message v1.34.1's guard keeps out "+
			"of the needs-a-category queue here", scan.skippedRuleMsgs)
	}
	// THE DISCRIMINATION ASSERTION, stated on its own so a future edit that merges
	// the buckets fails HERE with the reason attached rather than in an arithmetic
	// mismatch several lines up.
	if scan.skippedHandMsgs == scan.skippedRuleMsgs {
		t.Errorf("the two skip buckets are equal (%d) on a fixture built 3-to-1 — they are being fed "+
			"from one counter, which is the merged report #291 fixes", scan.skippedHandMsgs)
	}
	// And per label, same split, same reasoning.
	if got, want := scan.skippedHand[promotions.ID], 3; got != want {
		t.Errorf("skippedHand[Promotions] = %d, want %d", got, want)
	}
	if got, want := scan.skippedRule[promotions.ID], 1; got != want {
		t.Errorf("skippedRule[Promotions] = %d, want %d", got, want)
	}
	// THE TOTAL IS UNCHANGED BY THE SPLIT — the buckets partition the old number
	// rather than re-measuring it, so the guard's behaviour is provably untouched
	// and only the attribution moved.
	if got := scan.skippedHandMsgs + scan.skippedRuleMsgs; got != 4 {
		t.Errorf("the two buckets total %d skipped message(s), want 4 — the split must PARTITION the "+
			"population the old single counter measured, not change it", got)
	}
	if scan.rows[promotions.ID] != 1 || len(scan.targets) != 1 {
		t.Errorf("rows[Promotions] = %d, targets = %d, want 1 and 1 — the uncategorised message must "+
			"still be written; a scan that skipped everything would satisfy the assertions above",
			scan.rows[promotions.ID], len(scan.targets))
	}
}

// TestScanBackfillOneRowPerLabel checks the other half of the contract: a
// message can be claimed by more than one label (ambiguity is real and
// suggest-report counts it — see its "ambiguous" metric), but never twice by
// the SAME label, even if more than one kept candidate for that label
// matches it. Two rows for one (message, label) pair would make the
// per-label ROWS count in the backfill report double whichever message hit
// this, and PutRuleSuggestion would receive two calls that just upsert over
// each other — harmless in the database, but wrong in the report above it.
func TestScanBackfillOneRowPerLabel(t *testing.T) {
	// Two candidates for the SAME label, both matching the same message: a
	// narrow one (alias) and a broader one (domain) that a real dedup pass
	// would usually have pruned — but this function must not rely on that
	// having happened upstream.
	narrow := rules.Condition{All: []rules.Condition{
		{Field: rules.FieldAlias, Op: rules.OpEquals, Value: "deals@shop.example"},
	}}
	broad := rules.Condition{All: []rules.Condition{
		{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: "shop.example"},
	}}
	label := store.LabelView{ID: ulid.Make(), Name: "Promotions"}
	kept := []suggestion{
		{label: label, conditions: narrow, positives: 6},
		{label: label, conditions: broad, positives: 3},
	}

	msgID := ulid.Make()
	candidates := []store.RulePreviewCandidate{
		{ID: msgID, From: "deals@shop.example", Alias: "deals@shop.example", Subject: "Sale"},
	}

	scan := scanBackfill(kept, candidates)
	if len(scan.targets) != 1 {
		t.Fatalf("scanBackfill returned %d target(s) for one message and one label, want 1", len(scan.targets))
	}
	got := scan.targets[0]
	if got.msgID != msgID {
		t.Errorf("target message = %s, want %s", got.msgID, msgID)
	}
	if got.labelID != label.ID {
		t.Errorf("target label = %s, want %s", got.labelID, label.ID)
	}
	if got.evidence != 6 {
		t.Errorf("evidence = %d, want 6 — the FIRST (strongest) kept candidate for the label should win", got.evidence)
	}
	if scan.rows[label.ID] != 1 {
		t.Errorf("rows[%s] = %d, want 1 — the per-label count must agree with the targets it was "+
			"computed alongside", label.Name, scan.rows[label.ID])
	}
	if scan.skippedHandMsgs != 0 || scan.skippedRuleMsgs != 0 ||
		scan.skippedHand[label.ID] != 0 || scan.skippedRule[label.ID] != 0 {
		t.Errorf("skipped hand/rule msgs = %d/%d, per-label %s = %d/%d, want all 0 — nothing in this "+
			"fixture is filed", scan.skippedHandMsgs, scan.skippedRuleMsgs, label.Name,
			scan.skippedHand[label.ID], scan.skippedRule[label.ID])
	}
}

// TestSplitWorkloadSeparatesRuleFiledFromUncategorised pins the three-way split,
// and the middle bucket IS the bug.
//
// `ruleFiled` — labelled, but only by automation — has no evidence to give the
// inducer and no need of a category, and before the #242 follow-up there was no
// bucket for it: `len(UserLabelIDs) == 0` swept it in with the uncategorised, so
// the denominator every percentage in this report is divided by counted mail
// that was already filed. A report is only as honest as its denominator, which
// is why this is a named type with a test rather than three counters inline.
func TestSplitWorkloadSeparatesRuleFiledFromUncategorised(t *testing.T) {
	candidates := []store.RulePreviewCandidate{
		// Two with no label at all.
		{ID: ulid.Make(), From: "a@example.com"},
		{ID: ulid.Make(), From: "b@example.com"},
		// One filed ONLY by a rule: HasAnyLabel with no user labels. The store can
		// and does return exactly this shape (attachLabelFacts sets the flag for
		// every row and the ids only for applied_by='user').
		{ID: ulid.Make(), From: "c@example.com", HasAnyLabel: true},
		// Three the user filed by hand — both fields set, as the store sets them.
		{ID: ulid.Make(), From: "d@example.com", HasAnyLabel: true, UserLabelIDs: []ulid.ULID{ulid.Make()}},
		{ID: ulid.Make(), From: "e@example.com", HasAnyLabel: true, UserLabelIDs: []ulid.ULID{ulid.Make()}},
		{ID: ulid.Make(), From: "f@example.com", HasAnyLabel: true, UserLabelIDs: []ulid.ULID{ulid.Make()}},
	}

	w := splitWorkload(candidates)
	want := workload{uncategorised: 2, handFiled: 3, ruleFiled: 1}
	if w != want {
		t.Fatalf("splitWorkload = %+v, want %+v", w, want)
	}
	// SPELLED OUT SEPARATELY, because the struct comparison above would also be
	// satisfied by a fixture that never exercised the ruleFiled arm, and this is
	// the one arm whose absence shipped a defect. A rule-filed message counted as
	// uncategorised inflates the target population and the denominator with it.
	if w.ruleFiled != 1 {
		t.Errorf("ruleFiled = %d, want 1 — a message filed only by a rule fell into another bucket", w.ruleFiled)
	}
	if w.uncategorised != 2 {
		t.Errorf("uncategorised = %d, want 2 — a message a RULE filed is CATEGORISED and must not "+
			"be counted here; that conflation is the #242 follow-up defect", w.uncategorised)
	}
	// The three buckets must partition the input: anything that fell out entirely
	// would be invisible in every printed number.
	if got := w.uncategorised + w.handFiled + w.ruleFiled; got != len(candidates) {
		t.Errorf("the three buckets total %d over %d candidates — the split is not a partition",
			got, len(candidates))
	}
}

// TestSuggestRulesClaimsNoEqualityWithSuggestReport is a test about PRINTED
// TEXT, which is unusual and deliberate.
//
// The banner and the --backfill flag help both used to promise that this
// command's count "should match suggest-report's REACHED". It never can: this
// command additionally drops candidates an accepted rule already covers
// and messages that already carry any label, so the two figures differ on a real
// mailbox. A FALSE INVARIANT PRINTED IN A REPORT is worse
// than no invariant — it sends the next reader hunting a defect that does not
// exist, which is exactly what happened. The claim is part of the deliverable
// here, so it is asserted like any other output.
//
// It matches on PHRASES rather than pinning paragraphs, so rewording the
// explanation does not go red while restoring the equality claim does.
func TestSuggestRulesClaimsNoEqualityWithSuggestReport(t *testing.T) {
	// The banner. `d` is nil and safe: with apply=false, runSuggestBackfill
	// returns at the DRY RUN line without touching the deps. If that ever stops
	// being true this panics rather than passing quietly, which is the failure
	// mode to prefer.
	banner := captureStdout(t, func() {
		if err := runSuggestBackfill(context.Background(), nil, ulid.ULID{}, emptyScan(), 0, false, 0); err != nil {
			t.Fatalf("runSuggestBackfill dry run: %v", err)
		}
	})

	// The flag help. `-h` is answered by flag.ContinueOnError before any user id
	// is parsed or any database is opened, so this needs no fixture at all.
	help := captureStderr(t, func() {
		if err := runSuggestRules(context.Background(), []string{"-h"}); err == nil {
			t.Fatal("runSuggestRules -h returned no error; expected flag.ErrHelp")
		}
	})

	for _, surface := range []struct{ what, text string }{
		{"--backfill banner", banner},
		{"--backfill flag help", help},
	} {
		lower := strings.ToLower(surface.text)
		// The forbidden claims, listed literally. "never equal to it" is fine and
		// must stay fine, so these are whole phrases and not the word "equal".
		for _, claim := range []string{
			"should match", "should agree", "should equal", "should be the same",
			"must match", "must equal", "matches suggest-report", "equals suggest-report",
			"same as suggest-report", "agree with suggest-report",
		} {
			if strings.Contains(lower, claim) {
				t.Errorf("the %s claims %q. It is FALSE: suggest-rules additionally drops "+
					"candidates an accepted rule already covers and messages that already carry "+
					"any label, so its count is strictly smaller (160 vs 270 live). Printing the "+
					"claim sends the next reader after a defect that is not there.\n\n%s",
					surface.what, claim, surface.text)
			}
		}
		// And the framing that replaced it must actually be there — otherwise this
		// test passes just as happily against a surface that says nothing at all.
		if !strings.Contains(lower, "subset") {
			t.Errorf("the %s does not describe its count as a SUBSET of suggest-report's; the "+
				"relationship between the two numbers is the thing an operator needs and its "+
				"absence is how the false claim got believed in the first place.\n\n%s",
				surface.what, surface.text)
		}
		if !strings.Contains(lower, "suggest-report") {
			t.Errorf("the %s never names suggest-report, so it cannot be framing anything relative "+
				"to it.\n\n%s", surface.what, surface.text)
		}
	}

	// The banner alone carries the direction of the inequality, which is the only
	// comparison that means anything: a LARGER count here than suggest-report's
	// would be a real defect, a smaller one is expected.
	if !strings.Contains(banner, "suggest-rules <= suggest-report") {
		t.Errorf("the banner does not state the inequality it replaced the equality claim with; "+
			"without a direction, a reader has no way to tell a real defect from the expected "+
			"gap.\n\n%s", banner)
	}
}

// emptyScan is a backfillScan with its maps made and nothing in them — the shape
// runSuggestBackfill needs to reach its DRY RUN line without a database.
func emptyScan() backfillScan {
	return backfillScan{
		rows:        map[ulid.ULID]int{},
		skippedHand: map[ulid.ULID]int{},
		skippedRule: map[ulid.ULID]int{},
	}
}

// TestBackfillBannerReportsTheTwoFiledPopulationsSeparately is #291 at the
// SURFACE, and it is a test about printed text for the same reason the equality
// test below is: the report IS the deliverable here, and this defect was entirely
// in the reporting — the guard it describes was already correct.
//
// THE FIXTURE USES THE SHAPE THAT EXPOSED IT. 123 hand-filed and 42 rule-filed stand
// behind the single "165 already-categorised
// message(s) matched and are skipped" line. So the assertion is not only that
// both parts appear, but that the MERGED figure does NOT: 165 is the number that
// made the fix look four times larger than it is, and a surface that prints it
// alone has regressed however well the split is computed underneath.
func TestBackfillBannerReportsTheTwoFiledPopulationsSeparately(t *testing.T) {
	scan := emptyScan()
	scan.skippedHandMsgs = 123
	scan.skippedRuleMsgs = 42

	banner := captureStdout(t, func() {
		if err := runSuggestBackfill(context.Background(), nil, ulid.ULID{}, scan, 0, false, 0); err != nil {
			t.Fatalf("runSuggestBackfill dry run: %v", err)
		}
	})

	for _, want := range []string{"123", "42"} {
		if !strings.Contains(banner, want) {
			t.Errorf("the --backfill banner never prints %q. The two filed populations are reported "+
				"separately or not at all: one is the candidates' own evidence and was never a "+
				"target, the other is what v1.34.1's guard prevents (#291).\n\n%s", want, banner)
		}
	}
	if strings.Contains(banner, "165") {
		t.Errorf("the --backfill banner prints the MERGED total 165. That is the #291 defect verbatim: "+
			"a reader takes it as the size of what the guard prevents, and the real figure is 42 — "+
			"the other 123 are hand-filed mail no version of this command would have written.\n\n%s",
			banner)
	}
	// The words, not only the digits: two bare numbers on a line an operator cannot
	// attribute are no better than one.
	lower := strings.ToLower(banner)
	for _, want := range []string{"by hand", "by a rule"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the --backfill banner never says %q, so the two numbers it prints cannot be "+
				"attributed to a population.\n\n%s", want, banner)
		}
	}
}

// TestSuggestRulesStatesTheCommandOrder pins #294's OTHER half. The bound is
// worthless if the operator does not know there is an order — the whole defect was
// that running the commands the wrong way round silently turned 35 auto-filing
// rules into 35 questions, and NEITHER command's output mentioned the other.
//
// Two surfaces, because they are reached by different operators: the flag help by
// someone reading `-h`, the banner by someone about to write rows. The main
// report's own `order :` block is printed after openDeps and so cannot be captured
// without a database — it is covered by the integration lane's absence rather than
// by this test, which is a real gap and is recorded as one.
func TestSuggestRulesStatesTheCommandOrder(t *testing.T) {
	banner := captureStdout(t, func() {
		if err := runSuggestBackfill(context.Background(), nil, ulid.ULID{}, emptyScan(), 0, false, 0); err != nil {
			t.Fatalf("runSuggestBackfill dry run: %v", err)
		}
	})
	help := captureStderr(t, func() {
		if err := runSuggestRules(context.Background(), []string{"-h"}); err == nil {
			t.Fatal("runSuggestRules -h returned no error; expected flag.ErrHelp")
		}
	})

	for _, surface := range []struct{ what, text string }{
		{"--backfill banner", banner},
		{"flag help", help},
	} {
		lower := strings.ToLower(surface.text)
		if !strings.Contains(lower, "accept-rules") {
			t.Errorf("the %s never names `accept-rules`, so nothing tells an operator that running "+
				"it first is what keeps 35 auto-filable candidates from becoming questions "+
				"(#294).\n\n%s", surface.what, surface.text)
		}
		if !strings.Contains(lower, "first") {
			t.Errorf("the %s names accept-rules but not the ORDER. Both commands produce plausible "+
				"numbers in either order, which is precisely why the dependency has to be stated "+
				"rather than inferred (#294).\n\n%s", surface.what, surface.text)
		}
	}
}

// captureStdout / captureStderr run fn with the given stream redirected through
// a pipe and return everything written to it.
//
// These commands' output IS their deliverable — the report is the thing that
// ships, and this project has twice paid for a report that stated something the
// code did not do — so the text is worth asserting on, and asserting on it means
// capturing it. Nothing here runs in parallel, so swapping the process-wide
// stream is safe.
func captureStdout(t *testing.T, fn func()) string { return capture(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { return capture(t, &os.Stderr, fn) }

func capture(t *testing.T, stream **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := *stream
	*stream = w
	// Drained in a goroutine: a pipe's buffer is finite, and a report longer than
	// it would otherwise block the writer forever rather than fail.
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	func() {
		defer func() {
			*stream = orig
			_ = w.Close()
		}()
		fn()
	}()
	out := <-done
	_ = r.Close()
	return out
}
