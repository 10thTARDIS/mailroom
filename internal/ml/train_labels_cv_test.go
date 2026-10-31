// SPDX-License-Identifier: AGPL-3.0-only

package ml

import "testing"

// Fold assignment is what the CV numbers mean, so it is pinned as hard as the
// single split (#169).
//
// The original assignment gave folds only to senders that contribute
// positives; a negative from any other domain trained always and scored
// never. That meant every scored holdout negative shared a domain with the
// holdout positives — the CV measured within-domain discrimination, not "is
// this unseen sender's mail X" — and a fold whose positive domains had no
// other mail held zero negatives and was silently skipped.

// Every example must land in exactly one fold's holdout. Without this the
// pooled out-of-fold decision metrics double-count some examples and never see
// others, and "each example judged once by a model that never saw its sender"
// stops being true.
func TestLabelCVFoldsPartitionEveryExample(t *testing.T) {
	var examples []LabelExample
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com", "e.com"} {
		examples = append(examples, ex(d, true), ex(d, false))
	}
	for _, d := range []string{"n1.com", "n2.com", "n3.com", "n4.com", "n5.com", "n6.com", "n7.com"} {
		examples = append(examples, ex(d, false))
	}
	// Senderless examples are their own singleton groups; they must fold too.
	examples = append(examples, ex("", false), ex("", false))

	const folds = 5
	keys := groupKeys(examples)
	foldOf, posGroups := labelCVFolds(examples, keys, folds)
	if posGroups != 5 {
		t.Fatalf("posGroups = %d, want 5", posGroups)
	}

	seen := make([]int, len(examples))
	for f := 0; f < folds; f++ {
		for i := range examples {
			fold, known := foldOf[keys[i]]
			if !known {
				t.Fatalf("example %d (group %q) has no fold — it would train always and score never", i, keys[i])
			}
			if fold == f {
				seen[i]++
			}
		}
	}
	for i, n := range seen {
		if n != 1 {
			t.Errorf("example %d appears in %d holdouts, want exactly 1", i, n)
		}
	}
}

// Each fold's holdout must contain CROSS-DOMAIN negatives — negatives from
// domains with no positives at all. If the only negatives a holdout can hold
// are those sharing a domain with its positives, the CV asks "which of this
// domain's mail is X" when the serving question is "is this unseen domain's
// mail X".
func TestLabelCVFoldsGiveEveryFoldCrossDomainNegatives(t *testing.T) {
	var examples []LabelExample
	posDomains := []string{"a.com", "b.com", "c.com"}
	for _, d := range posDomains {
		examples = append(examples, ex(d, true), ex(d, true))
	}
	negDomains := []string{"n1.com", "n2.com", "n3.com", "n4.com", "n5.com", "n6.com"}
	for _, d := range negDomains {
		examples = append(examples, ex(d, false))
	}

	const folds = 3
	keys := groupKeys(examples)
	foldOf, _ := labelCVFolds(examples, keys, folds)

	crossDomainNegs := make([]int, folds)
	for i, e := range examples {
		if e.Positive {
			continue
		}
		isPosDomain := false
		for _, d := range posDomains {
			if e.Group == d {
				isPosDomain = true
			}
		}
		if !isPosDomain {
			crossDomainNegs[foldOf[keys[i]]]++
		}
	}
	for f, n := range crossDomainNegs {
		if n == 0 {
			t.Errorf("fold %d holds no cross-domain negatives — it can only measure within-domain discrimination", f)
		}
	}
}

// A positive-bearing domain's negatives follow their domain (the whole point
// of grouping), and the same evidence yields the same folds.
func TestLabelCVFoldsAreGroupedAndDeterministic(t *testing.T) {
	var examples []LabelExample
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com"} {
		examples = append(examples, ex(d, true), ex(d, false))
	}
	examples = append(examples, ex("n1.com", false), ex("n2.com", false))

	keys := groupKeys(examples)
	first, _ := labelCVFolds(examples, keys, 2)
	// Same-domain examples share a fold by construction (one key, one entry) —
	// assert the map is keyed by group, then that repeated calls agree.
	for i := range examples {
		if _, ok := first[keys[i]]; !ok {
			t.Fatalf("group %q unassigned", keys[i])
		}
	}
	for run := 0; run < 3; run++ {
		again, _ := labelCVFolds(examples, keys, 2)
		for g, f := range first {
			if again[g] != f {
				t.Fatalf("run %d: group %q moved from fold %d to %d", run, g, f, again[g])
			}
		}
	}
}
