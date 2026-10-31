// SPDX-License-Identifier: AGPL-3.0-only

package areas

import (
	"strings"
	"testing"

	"mailroom/internal/tabs"
)

func TestDefaultHasFiveAreasAndValidates(t *testing.T) {
	cfg := Default()
	if len(cfg.Areas) != 5 {
		t.Fatalf("Default() has %d areas, want 5", len(cfg.Areas))
	}
	want := map[string]tabs.Tab{
		"Primary": tabs.Primary, "Social": tabs.Social, "Promotions": tabs.Promotions,
		"Updates": tabs.Updates, "Forums": tabs.Forums,
	}
	for _, a := range cfg.Areas {
		tab, ok := want[a.Name]
		if !ok {
			t.Errorf("unexpected area name %q", a.Name)
			continue
		}
		delete(want, a.Name)
		if len(a.Tabs) != 1 || a.Tabs[0] != tab {
			t.Errorf("area %q: Tabs = %v, want [%s]", a.Name, a.Tabs, tab)
		}
		if len(a.Labels) != 0 {
			t.Errorf("area %q: Labels = %v, want none", a.Name, a.Labels)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing areas: %v", want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}

// TestValidateCatchesEachRefusal is a table so every refusal in the doc
// comment has a line here holding it to that word, not just to "some error".
func TestValidateCatchesEachRefusal(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			name: "duplicate area name, case-insensitive",
			cfg: Config{Areas: []Area{
				{Name: "Family", Tabs: []tabs.Tab{tabs.Primary}},
				{Name: "family", Tabs: []tabs.Tab{tabs.Social}},
			}},
		},
		{
			name: "area with no feeds",
			cfg: Config{Areas: []Area{
				{Name: "Empty"},
			}},
		},
		{
			name: "invalid tab",
			cfg: Config{Areas: []Area{
				{Name: "Bad", Tabs: []tabs.Tab{"snacks"}},
			}},
		},
		{
			name: "a tab fed to two areas",
			cfg: Config{Areas: []Area{
				{Name: "A", Tabs: []tabs.Tab{tabs.Primary}},
				{Name: "B", Tabs: []tabs.Tab{tabs.Primary}},
			}},
		},
		{
			name: "a label fed to two areas, case-insensitive",
			cfg: Config{Areas: []Area{
				{Name: "A", Labels: []string{"Family"}},
				{Name: "B", Labels: []string{"family"}},
			}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cfg.Validate(); err == nil {
				t.Errorf("Validate() = nil, want an error for %q", c.name)
			}
		})
	}
}

func twoAreaFixture() Config {
	return Config{Areas: []Area{
		{Name: "Family", Labels: []string{"Family", "Friends"}},
		{Name: "Shopping", Tabs: []tabs.Tab{tabs.Promotions}, Labels: []string{"Deals"}},
	}}
}

func TestResolveLabelBeatsTab(t *testing.T) {
	cfg := twoAreaFixture()
	f := Facts{Labels: []string{"Family"}, Tab: tabs.Promotions, TabKnown: true}
	res := cfg.Resolve(f)
	if res.Area != "Family" || res.Reason != ByLabel {
		t.Errorf("Resolve() = %+v, want area Family by label (a label must beat a known tab)", res)
	}
}

func TestResolveTieEarlierAreaWinsAndContestedListsTheLoser(t *testing.T) {
	cfg := twoAreaFixture()
	// "Family" feeds the first area, "Deals" feeds the second — a message
	// carrying both labels ties, and the plan's own order must break it.
	f := Facts{Labels: []string{"Deals", "Family"}}
	res := cfg.Resolve(f)
	if res.Area != "Family" {
		t.Errorf("winner = %q, want Family (earlier in plan order)", res.Area)
	}
	if res.Reason != ByLabel {
		t.Errorf("Reason = %v, want ByLabel", res.Reason)
	}
	if len(res.Contested) != 1 || res.Contested[0] != "Shopping" {
		t.Errorf("Contested = %v, want [Shopping]", res.Contested)
	}
}

func TestResolveNoTieWhenLabelsAgree(t *testing.T) {
	cfg := twoAreaFixture()
	f := Facts{Labels: []string{"Family", "Friends"}}
	res := cfg.Resolve(f)
	if res.Area != "Family" || len(res.Contested) != 0 {
		t.Errorf("Resolve() = %+v, want Family with no contest (both labels feed the same area)", res)
	}
}

func TestResolveTabFallbackViaMappedOrClassified(t *testing.T) {
	cfg := twoAreaFixture()
	// No labels at all: the resolved tab (whichever source produced it —
	// Facts does not distinguish mapped from classified, by design) decides.
	res := cfg.Resolve(Facts{Tab: tabs.Promotions, TabKnown: true})
	if res.Area != "Shopping" || res.Reason != ByTab {
		t.Errorf("Resolve() = %+v, want Shopping by tab", res)
	}
}

func TestResolveUnsortedNoLabelNoVerdict(t *testing.T) {
	cfg := twoAreaFixture()
	res := cfg.Resolve(Facts{})
	if res.Area != "" || res.Reason != UnsortedNoLabelNoVerdict {
		t.Errorf("Resolve() = %+v, want unsorted/no-label-no-verdict", res)
	}
	if !res.Reason.Unsorted() {
		t.Error("Reason.Unsorted() = false, want true")
	}
}

func TestResolveUnsortedLabelsFeedNothing(t *testing.T) {
	cfg := twoAreaFixture()
	// A label that matches nothing in the plan, and (to prove label wins the
	// PRIORITY, not just the fallback) a known tab that ALSO feeds nothing.
	res := cfg.Resolve(Facts{Labels: []string{"Random"}, Tab: tabs.Social, TabKnown: true})
	if res.Area != "" || res.Reason != UnsortedLabelsFeedNothing {
		t.Errorf("Resolve() = %+v, want unsorted/labels-feed-nothing", res)
	}
}

func TestResolveUnsortedTabFeedsNothing(t *testing.T) {
	cfg := twoAreaFixture()
	// No labels, so this is unambiguous: nothing but the tab could have decided
	// it, and the tab feeds no area in this plan.
	res := cfg.Resolve(Facts{Tab: tabs.Updates, TabKnown: true})
	if res.Area != "" || res.Reason != UnsortedTabFeedsNothing {
		t.Errorf("Resolve() = %+v, want unsorted/tab-feeds-nothing", res)
	}
}

func TestResolveLabelMatchIsCaseInsensitive(t *testing.T) {
	cfg := twoAreaFixture()
	res := cfg.Resolve(Facts{Labels: []string{"  FAMILY  "}})
	if res.Area != "Family" || res.Reason != ByLabel {
		t.Errorf("Resolve() = %+v, want Family (case/whitespace must not matter)", res)
	}
}

// ---- ParsePlan ----

func TestParsePlanHappyPath(t *testing.T) {
	known := []string{"Family", "Friends", "Deals"}
	plan := strings.NewReader(`
# comment line, skipped
Family = label:Family, label:Friends

Shopping = tab:promotions, label:Deals
`)
	cfg, errs, err := ParsePlan(plan, known)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("ParsePlan: unexpected line errors: %v", errs)
	}
	want := Config{Areas: []Area{
		{Name: "Family", Labels: []string{"Family", "Friends"}},
		{Name: "Shopping", Tabs: []tabs.Tab{tabs.Promotions}, Labels: []string{"Deals"}},
	}}
	if !configsEqual(cfg, want) {
		t.Errorf("ParsePlan() = %+v, want %+v", cfg, want)
	}
}

// TestParsePlanNegativeControl is the required negative control (CLAUDE.md):
// a valid plan must yield ZERO LineErrors and a config equal to what was
// intended — proving the happy-path test above can actually distinguish
// success from a parser that silently drops every line.
func TestParsePlanNegativeControl(t *testing.T) {
	known := []string{"Deals"}
	plan := strings.NewReader("Shopping = tab:promotions, label:Deals\n")
	cfg, errs, err := ParsePlan(plan, known)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("ParsePlan: got %d line errors on a valid plan, want 0: %v", len(errs), errs)
	}
	want := Config{Areas: []Area{
		{Name: "Shopping", Tabs: []tabs.Tab{tabs.Promotions}, Labels: []string{"Deals"}},
	}}
	if !configsEqual(cfg, want) {
		t.Errorf("ParsePlan() = %+v, want %+v", cfg, want)
	}
}

func TestParsePlanRefusals(t *testing.T) {
	known := []string{"Deals"}
	cases := []struct {
		name string
		line string
	}{
		{"no equals", "Shopping tab:promotions"},
		{"empty name", " = tab:promotions"},
		{"duplicate name", "Shopping = tab:promotions\nShopping = tab:social"},
		{"no feeds", "Shopping ="},
		{"feed without prefix", "Shopping = promotions"},
		{"unknown tab", "Shopping = tab:snacks"},
		{"unknown label", "Shopping = label:Nope"},
		{"tab fed twice", "A = tab:promotions\nB = tab:promotions"},
		{"label fed twice", "A = label:Deals\nB = label:Deals"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs, err := ParsePlan(strings.NewReader(c.line), known)
			if err != nil {
				t.Fatalf("ParsePlan: unexpected top-level error: %v", err)
			}
			if len(errs) == 0 {
				t.Fatalf("ParsePlan(%q): got 0 line errors, want at least 1", c.line)
			}
		})
	}
}

func TestParsePlanRefusedLineDoesNotBlockALaterValidLine(t *testing.T) {
	known := []string{"Deals"}
	plan := strings.NewReader("Bad = tab:snacks\nGood = tab:promotions, label:Deals\n")
	cfg, errs, err := ParsePlan(plan, known)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly 1", errs)
	}
	if len(cfg.Areas) != 1 || cfg.Areas[0].Name != "Good" {
		t.Fatalf("cfg.Areas = %+v, want just Good", cfg.Areas)
	}
}

func configsEqual(a, b Config) bool {
	if len(a.Areas) != len(b.Areas) {
		return false
	}
	for i := range a.Areas {
		x, y := a.Areas[i], b.Areas[i]
		if x.Name != y.Name {
			return false
		}
		if !stringTabsEqual(x.Tabs, y.Tabs) {
			return false
		}
		if !stringsEqual(x.Labels, y.Labels) {
			return false
		}
	}
	return true
}

func stringTabsEqual(a, b []tabs.Tab) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
