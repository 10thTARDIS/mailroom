// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"reflect"
	"sort"
	"testing"
)

// featuresFromFacts emits the exact expected sparse key set from hand-built
// facts — the contract the model and the (future) trainer both depend on.
func TestFeaturesFromFacts(t *testing.T) {
	// Built via assignments (not a keyed literal) so the body stays alignment-free.
	// The values carry cases the hashing must handle: mixed-case address/domain/
	// alias (keys must lowercase), a subject with a duplicate token ("budget"),
	// seen_count 8 (-> bucket 6-10) and toCc 2 (-> bucket 2).
	var facts MessageFacts
	facts.SenderAddress = "VIP@Example.com"
	facts.SenderDomain = "Example.com"
	facts.HasCorresponded = true
	facts.OutboundCount = 8
	facts.IsListMail = true
	facts.HasListUnsubscribe = true
	facts.Subject = "Re: Q3 Budget — budget!!"
	facts.ToCcCount = 2
	facts.IsReplyInThread = true
	facts.HourOfDayUTC = 9
	facts.HasAttachments = true
	facts.AddressedAlias = "Me+Sales@Example.com"
	facts.RemoteFlagged = true
	got := featuresFromFacts(facts)

	// The EXACT expected key set (all presence-1.0). "budget" is deduped though it
	// appears twice in the subject; everything is lowercased.
	wantKeys := []string{
		"from:vip@example.com",
		"dom:example.com",
		"corr:1",
		"sent:6-10",
		"list:1",
		"unsub:1",
		"subtok:re",
		"subtok:q3",
		"subtok:budget",
		"tocc:2",
		"reply:1",
		"hour:9",
		"attach:1",
		"alias:me+sales@example.com",
		"flagged:1",
	}
	want := FeatureVector{}
	for _, k := range wantKeys {
		want[k] = 1.0
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("feature set mismatch\n got: %v\nwant: %v", sortedKeys(got), sortedKeys(want))
	}
}

// The empty/zero facts still emit the three CATEGORICAL features (they always
// have a value) and nothing else — no stray boolean keys.
func TestFeaturesFromFactsEmpty(t *testing.T) {
	got := featuresFromFacts(MessageFacts{})
	want := FeatureVector{
		"sent:0": 1.0,
		"tocc:0": 1.0,
		"hour:0": 1.0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("empty facts feature set = %v, want %v", sortedKeys(got), sortedKeys(want))
	}
}

func TestCountBucket(t *testing.T) {
	cases := map[int]string{-3: "0", 0: "0", 1: "1", 2: "2", 3: "3-5", 5: "3-5",
		6: "6-10", 10: "6-10", 11: "11-25", 25: "11-25", 26: "26+", 1000: "26+"}
	for in, want := range cases {
		if got := countBucket(in); got != want {
			t.Errorf("countBucket(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestClampHour(t *testing.T) {
	for in, want := range map[int]int{-1: 0, 0: 0, 9: 9, 23: 23, 24: 23, 99: 23} {
		if got := clampHour(in); got != want {
			t.Errorf("clampHour(%d) = %d, want %d", in, got, want)
		}
	}
}

func sortedKeys(fv FeatureVector) []string {
	ks := make([]string, 0, len(fv))
	for k := range fv {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
