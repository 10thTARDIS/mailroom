// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"mailroom/internal/tabs"
)

// The plan parser is the only thing standing between a typed file and a write to
// JMAP-visible state, so its refusals matter more than its acceptances: a line it
// mis-parses becomes a label pointed at the wrong tab, which silently moves a
// whole category of mail.

func TestLabelTabPlanParsesAndRefuses(t *testing.T) {
	plan := strings.Join([]string{
		"# an example mapping (#219)",
		"",
		"Promotions\tpromotions",
		"Events\tpromotions",
		"Reading\tupdates",
		"Retired\t", // empty tab: unmaps, deliberately allowed
		"NoTabColumn",
		"Bogus\tinbox",
		"\tupdates",
		"Promotions\tupdates", // duplicate label
	}, "\n")

	entries, errs, err := parseLabelTabPlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("accepted %d entries, want 4: %+v", len(entries), entries)
	}
	want := map[string]tabs.Tab{
		"Promotions": tabs.Promotions,
		"Events":     tabs.Promotions,
		"Reading":    tabs.Updates,
		"Retired":    "", // unmapped
	}
	for _, e := range entries {
		w, ok := want[e.Label]
		if !ok {
			t.Errorf("unexpected entry %q", e.Label)
			continue
		}
		if e.Tab != w {
			t.Errorf("%s -> %q, want %q", e.Label, e.Tab, w)
		}
	}
	if len(errs) != 4 {
		t.Fatalf("refused %d lines, want 4: %+v", len(errs), errs)
	}
	reasons := strings.Join(func() []string {
		out := make([]string, len(errs))
		for i, e := range errs {
			out[i] = e.Reason
		}
		return out
	}(), " | ")
	for _, must := range []string{"no tab between", "not a category tab", "missing label name", "already mapped"} {
		if !strings.Contains(reasons, must) {
			t.Errorf("no refusal mentioning %q; got: %s", must, reasons)
		}
	}
}

// TestLabelTabPlanRefusesADuplicateRatherThanPickingOne. Two lines for one label
// is a plan whose outcome depends on which line runs last — the operator would
// get a mapping they can read two ways and the tool would pick silently.
func TestLabelTabPlanRefusesADuplicateRatherThanPickingOne(t *testing.T) {
	entries, errs, err := parseLabelTabPlan(strings.NewReader("News\tupdates\nNews\tpromotions\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || entries[0].Tab != tabs.Updates {
		t.Errorf("entries = %+v, want only the first line kept", entries)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want the duplicate refused", errs)
	}
}

// TestLabelTabPlanKeepsWhitespaceOutOfLabelNames — but only at the edges. A
// label name legitimately contains spaces, so trimming the whole field is right
// and collapsing internal runs would be wrong: "Big Project" must not become
// "BigProject" and fail to match a real label.
func TestLabelTabPlanKeepsInternalSpacesInLabelNames(t *testing.T) {
	entries, errs, err := parseLabelTabPlan(strings.NewReader("  Big  Project \t updates \n"))
	if err != nil || len(errs) != 0 {
		t.Fatalf("parse: %v %+v", err, errs)
	}
	if len(entries) != 1 || entries[0].Label != "Big  Project" {
		t.Fatalf("label = %q, want %q", entries[0].Label, "Big  Project")
	}
	if entries[0].Tab != tabs.Updates {
		t.Errorf("tab = %q, want updates", entries[0].Tab)
	}
}

// TestLabelTabPlanSkipsCommentsAndBlanks. The mapping is product intent rather
// than a derivation, so the file is the right place to record WHY a label points
// where it does — which only works if comments survive parsing.
func TestLabelTabPlanSkipsCommentsAndBlanks(t *testing.T) {
	entries, errs, err := parseLabelTabPlan(strings.NewReader(
		"# why: event mail is marketing\n\n   \nEvents\tpromotions\n"))
	if err != nil || len(errs) != 0 {
		t.Fatalf("parse: %v %+v", err, errs)
	}
	if len(entries) != 1 || entries[0].Label != "Events" {
		t.Errorf("entries = %+v, want just Events", entries)
	}
}
