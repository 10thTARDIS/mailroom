// SPDX-License-Identifier: AGPL-3.0-only

package ml

import "testing"

// The holdout split is what the promotion gate believes, so it is worth pinning
// hard (#169).
//
// A per-label classifier will be used on mail from senders it has never seen —
// a sender it already knows is one an induced `alias =` rule handles for free
// and far more legibly. So "does this generalise?" means "does it work on an
// unseen sender?", and a split that lets one sender sit on both sides answers a
// different, much easier question and reports the answer as if it were the same
// one. That mistake is why the classifier's auto-apply was removed.

func ex(group string, positive bool) LabelExample {
	return LabelExample{Positive: positive, Group: group, Features: FeatureVector{}}
}

func groupsOf(es []LabelExample) map[string]bool {
	out := map[string]bool{}
	for _, e := range es {
		out[e.Group] = true
	}
	return out
}

// THE LOAD-BEARING PROPERTY. If this fails, the gate is measuring memorisation.
func TestGroupedSplitNeverPutsASenderOnBothSides(t *testing.T) {
	var examples []LabelExample
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com", "e.com"} {
		examples = append(examples, ex(d, true), ex(d, true), ex(d, false))
	}

	fit, holdout := splitLabelExamplesByGroup(examples, 0.25)
	if len(fit) == 0 || len(holdout) == 0 {
		t.Fatalf("split produced nothing: fit=%d holdout=%d", len(fit), len(holdout))
	}
	inFit, inHold := groupsOf(fit), groupsOf(holdout)
	for g := range inHold {
		if inFit[g] {
			t.Errorf("sender %q appears on BOTH sides — the model would be scored on senders it trained on", g)
		}
	}
}

// The old split is retained only to report the gap, and it demonstrably does
// leak. This is the contrast the report prints, so it must stay true — if the
// naive split ever stopped leaking, the two numbers would be measuring the same
// thing and the comparison would be noise.
func TestClassOnlySplitDoesLeakSenders(t *testing.T) {
	// INTERLEAVED, because that is how mail actually arrives — the training rows
	// come back in received_at order, so a sender's messages are scattered
	// through the list rather than contiguous.
	//
	// My first version of this fixture grouped each sender's messages together,
	// and the tail-cut happened to land exactly on a sender boundary — so the
	// class-only split produced a clean sender split by accident and the test
	// failed. That is worth keeping in mind about the OLD behaviour too: it does
	// not always leak, it leaks unpredictably depending on arrival order, which
	// is worse than leaking reliably.
	var examples []LabelExample
	for i := 0; i < 4; i++ {
		for _, d := range []string{"a.com", "b.com", "c.com", "d.com"} {
			examples = append(examples, ex(d, i%2 == 0))
		}
	}
	fit, holdout := splitLabelExamples(examples, 0.25)
	inFit, inHold := groupsOf(fit), groupsOf(holdout)
	leaked := false
	for g := range inHold {
		if inFit[g] {
			leaked = true
		}
	}
	if !leaked {
		t.Errorf("the class-only split no longer leaks senders; the naive/grouped comparison is now meaningless")
	}
}

// A label whose positives all come from ONE sender is not a model — it is a
// rule. It must be refused rather than scored, because scoring it would produce
// a number that reads as quality.
func TestSingleSenderLabelCannotBeEvaluated(t *testing.T) {
	examples := []LabelExample{
		ex("only.com", true), ex("only.com", true), ex("only.com", true),
		ex("other.com", false), ex("more.com", false),
	}
	fit, holdout := splitLabelExamplesByGroup(examples, 0.25)
	if fit != nil || holdout != nil {
		t.Errorf("a single-sender label was split anyway: fit=%d holdout=%d", len(fit), len(holdout))
	}
}

// Both sides must carry both classes, or AUC is undefined on one of them and
// would silently read as "no better".
func TestGroupedSplitKeepsBothClassesOnBothSides(t *testing.T) {
	var examples []LabelExample
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com", "e.com", "f.com"} {
		examples = append(examples, ex(d, true), ex(d, false))
	}
	fit, holdout := splitLabelExamplesByGroup(examples, 0.25)
	for _, side := range []struct {
		name string
		es   []LabelExample
	}{{"fit", fit}, {"holdout", holdout}} {
		if !hasBothClasses(side.es) {
			t.Errorf("%s carries only one class", side.name)
		}
	}
}

// Deterministic: the same evidence must produce the same split, or a run is not
// reproducible from its evidence and two passes could disagree about promotion
// for no reason anyone can see.
func TestGroupedSplitIsDeterministic(t *testing.T) {
	var examples []LabelExample
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com", "e.com"} {
		examples = append(examples, ex(d, true), ex(d, true), ex(d, false))
	}
	first, _ := splitLabelExamplesByGroup(examples, 0.25)
	for i := 0; i < 5; i++ {
		again, _ := splitLabelExamplesByGroup(examples, 0.25)
		if len(again) != len(first) {
			t.Fatalf("split %d differs in size: %d vs %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j].Group != first[j].Group || again[j].Positive != first[j].Positive {
				t.Fatalf("split %d differs at %d", i, j)
			}
		}
	}
}

// A message with no sender domain is its own group. Pooling them would invent a
// "no-sender" cohort that shares nothing, and holding that cohort out would look
// like a sender split while testing nothing.
func TestMessagesWithNoSenderAreNotPooled(t *testing.T) {
	keys := groupKeys([]LabelExample{ex("", true), ex("", true), ex("a.com", true)})
	if keys[0] == keys[1] {
		t.Errorf("two senderless messages share a group key; they would be held out as one cohort")
	}
	if keys[2] != "a.com" {
		t.Errorf("a real sender's key was rewritten: %q", keys[2])
	}
}

// The keys must be stable across calls. The first implementation derived them
// from the address of a loop variable, which Go may reuse — so identical input
// could produce different keys, and the split would stop being reproducible.
func TestGroupKeysAreStableAcrossCalls(t *testing.T) {
	in := []LabelExample{ex("", true), ex("a.com", true), ex("", false)}
	first := groupKeys(in)
	for i := 0; i < 3; i++ {
		again := groupKeys(in)
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("call %d: key %d changed from %q to %q", i, j, first[j], again[j])
			}
		}
	}
}
