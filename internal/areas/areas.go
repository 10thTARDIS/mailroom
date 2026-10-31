// SPDX-License-Identifier: AGPL-3.0-only

// Package areas is the resolution layer for mailroom#442: a partition of
// EVERY message into exactly one "area", fed by category tabs (internal/tabs)
// and/or by labels (by name).
//
// WHY THIS IS A THIRD AXIS, NOT A REPLACEMENT FOR EITHER OF THE OTHER TWO.
// Labels are topic (spec §6); tabs are function, decided sender-independently
// from header facts (internal/tabs' doc comment). An area is neither — it is
// the operator's OWN grouping of the two, stated as data rather than derived,
// because "which of my tabs and labels belong together" is a question only
// the operator can answer and a compiled default would be wrong for the
// second user (the same reasoning untangle-labels and map-label-tabs already
// rest on: the plan lives in a file the operator supplies, never in the
// binary).
//
// A LABEL OR TAB MAY FEED AT MOST ONE AREA (Validate enforces it), so an area
// plan is a genuine PARTITION of the two vocabularies, not an overlapping
// tagging scheme. Resolve then answers, for one message, which single area it
// falls into and — the point of shipping the report before the object
// (CLAUDE.md's "build the report before the fix") — WHY, so an operator can
// see the plan's blind spots before mailroom#442 ever writes an areas object
// that anything depends on.
//
// This package holds no state and touches no database, exactly like
// internal/tabs: internal/store assembles the Facts a resolution needs
// (store.AreaReportRow, mailroom#442 increment 0) and
// cmd/mailroom-admin/areareport.go is the only caller today.
package areas

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"mailroom/internal/tabs"
)

// Area is one named partition: fed by zero or more category tabs and zero or
// more labels (by name, case-insensitive — see normalizeLabel). An area with
// neither is meaningless and Validate refuses it.
type Area struct {
	Name   string
	Tabs   []tabs.Tab
	Labels []string
}

// Config is an ordered set of areas. ORDER IS LOAD-BEARING: it is the tie
// break Resolve uses when a message's labels feed more than one area (the
// first one in this order wins), exactly the role tabs.Precedence() plays for
// the tab layer's own tie-break — stated as data (plan order) here rather
// than as a second precedence table, because area order is the operator's
// own arrangement and has no protocol fact to derive it from.
type Config struct {
	Areas []Area
}

// Default returns one area per category tab, named with an initial capital
// (Primary, Social, Promotions, Updates, Forums) and fed by that tab alone.
// It is the plan `mailroom-admin area-report` uses with no --plan flag, and
// it always validates: every tab in tabs.All feeds exactly one area, no
// labels are claimed, so a first run needs no plan file at all.
func Default() Config {
	out := make([]Area, len(tabs.All))
	for i, t := range tabs.All {
		out[i] = Area{Name: capitalize(string(t)), Tabs: []tabs.Tab{t}}
	}
	return Config{Areas: out}
}

// capitalize upper-cases the first byte. Tab names are lower-case ASCII
// (internal/tabs), so this is not a general title-caser — it exists only to
// turn "primary" into "Primary" for Default()'s area names.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// normalizeLabel is how a label name is compared EVERYWHERE in this package:
// case-insensitive and trimmed, because a label plan written by hand will not
// reliably match a stored name byte-for-byte, and a plan that silently fed
// nothing because of a case mismatch is exactly the kind of failure the
// report-before-the-fix posture exists to catch.
func normalizeLabel(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Validate refuses a Config that cannot be a partition:
//
//   - two areas with the same name (case-insensitive) — Resolve would return
//     an ambiguous winner and the report table would have two identical rows;
//   - an area that feeds nothing (no tabs, no labels) — dead weight in the
//     plan that can never receive a message;
//   - a tab outside the closed vocabulary (tabs.All);
//   - a tab claimed by two areas — the "at most one area" invariant, without
//     which a message's tab-fed area would depend on map iteration order;
//   - a label (case-insensitive) claimed by two areas, same reason.
func (c Config) Validate() error {
	seenName := map[string]bool{}
	seenTab := map[tabs.Tab]string{}
	seenLabel := map[string]string{}

	for _, a := range c.Areas {
		key := strings.ToLower(a.Name)
		if seenName[key] {
			return fmt.Errorf("areas: duplicate area name %q", a.Name)
		}
		seenName[key] = true

		if len(a.Tabs) == 0 && len(a.Labels) == 0 {
			return fmt.Errorf("areas: area %q feeds nothing (no tabs, no labels)", a.Name)
		}

		for _, t := range a.Tabs {
			if !t.Valid() {
				return fmt.Errorf("areas: area %q: %q is not a category tab", a.Name, t)
			}
			if prev, dup := seenTab[t]; dup {
				return fmt.Errorf("areas: tab %q feeds both %q and %q", t, prev, a.Name)
			}
			seenTab[t] = a.Name
		}

		for _, l := range a.Labels {
			lk := normalizeLabel(l)
			if prev, dup := seenLabel[lk]; dup {
				return fmt.Errorf("areas: label %q feeds both %q and %q", l, prev, a.Name)
			}
			seenLabel[lk] = a.Name
		}
	}
	return nil
}

// Facts is one message's resolvable input: its labels (any case/spacing —
// normalized inside Resolve) and its RESOLVED tab, computed exactly as
// store.ResolveTabs computes it (mapped label tab by tabs.Precedence(), else
// the stored classifier verdict) — Resolve does not re-derive that
// precedence, it only consumes the answer, so there is exactly one
// implementation of "which tab does this message have" (store.AreaReportRow's
// ResolvedTab method).
type Facts struct {
	Labels   []string
	Tab      tabs.Tab
	TabKnown bool
}

// Reason is why a message resolved the way it did.
type Reason int

const (
	// ByLabel: the winning area was fed by one of the message's own labels.
	ByLabel Reason = iota
	// ByTab: no label decided it, but the message's resolved tab feeds an area.
	ByTab
	// UnsortedNoLabelNoVerdict: no labels at all, and no resolved tab either —
	// nothing has looked at this message yet. The tab classifier's workload,
	// same population `mailroom-admin tabs` already names for the tab layer.
	UnsortedNoLabelNoVerdict
	// UnsortedLabelsFeedNothing: the message IS labelled, but none of its
	// labels feeds an area. A mapping gap the operator closes with one edit to
	// the plan, not a classifier workload — the opposite fix from the reason
	// above, and the same split MessageTabMapping draws between "unlabelled"
	// and "labelled but unmapped" (internal/store/category_tabs.go).
	UnsortedLabelsFeedNothing
	// UnsortedTabFeedsNothing: the message has no labels, but its resolved tab
	// is known and no area claims that tab — the plan needs one more area, or
	// an existing one needs to add the tab.
	UnsortedTabFeedsNothing
)

// String renders the reason as the sentence fragment the report prints.
func (r Reason) String() string {
	switch r {
	case ByLabel:
		return "by label"
	case ByTab:
		return "by tab"
	case UnsortedNoLabelNoVerdict:
		return "no label and no verdict"
	case UnsortedLabelsFeedNothing:
		return "labelled, but no label feeds an area"
	case UnsortedTabFeedsNothing:
		return "tab feeds no area"
	default:
		return fmt.Sprintf("areas.Reason(%d)", int(r))
	}
}

// Unsorted reports whether this reason means the message landed in "Needs
// sorting" rather than a real area.
func (r Reason) Unsorted() bool {
	switch r {
	case UnsortedNoLabelNoVerdict, UnsortedLabelsFeedNothing, UnsortedTabFeedsNothing:
		return true
	default:
		return false
	}
}

// Resolution is the outcome of resolving one message. Area is "" exactly when
// Reason.Unsorted() is true. Contested carries the OTHER areas the message's
// labels also fed, in plan order — empty when the labels agreed, or when the
// winner was decided by tab rather than by label (a message never has two
// tabs, so there is nothing to contest there).
type Resolution struct {
	Area      string
	Reason    Reason
	Contested []string
}

// Resolve answers which area f's message falls into.
//
// THE ORDER, MATCHING THE SPEC EXACTLY:
//
//  1. Labels first. The first area (in c's order) fed by any of the message's
//     labels wins. Every OTHER area also fed by one of the message's labels is
//     a lost tie, recorded in Contested — the identical shape and reasoning
//     as tabs.FromLabels' own contested return: a tie rule nobody measures is
//     a rule nobody can correct.
//  2. Else the resolved tab. If f.TabKnown, the area (if any) that claims
//     f.Tab wins.
//  3. Else "Needs sorting", and the THREE reasons above are chosen by
//     whether the message carries labels at all — not by whether a
//     non-feeding tab was also known. A labelled message that reaches this
//     branch has a label-plan gap; that fix (edit the plan) is more specific
//     and more likely correct than blaming the tab, so it takes priority
//     whenever hasLabels is true. This mirrors MessageTabMapping's own
//     Labels-decides-first split for exactly the same reason.
func (c Config) Resolve(f Facts) Resolution {
	hasLabels := len(f.Labels) > 0

	if hasLabels {
		normLabels := make(map[string]bool, len(f.Labels))
		for _, l := range f.Labels {
			normLabels[normalizeLabel(l)] = true
		}
		var matched []string
		for _, a := range c.Areas {
			for _, l := range a.Labels {
				if normLabels[normalizeLabel(l)] {
					matched = append(matched, a.Name)
					break
				}
			}
		}
		if len(matched) > 0 {
			return Resolution{Area: matched[0], Reason: ByLabel, Contested: matched[1:]}
		}
	}

	if f.TabKnown {
		for _, a := range c.Areas {
			for _, t := range a.Tabs {
				if t == f.Tab {
					return Resolution{Area: a.Name, Reason: ByTab}
				}
			}
		}
	}

	switch {
	case hasLabels:
		return Resolution{Reason: UnsortedLabelsFeedNothing}
	case f.TabKnown:
		return Resolution{Reason: UnsortedTabFeedsNothing}
	default:
		return Resolution{Reason: UnsortedNoLabelNoVerdict}
	}
}

// LineError is one refused line of a plan file — the same shape as
// cmd/mailroom-admin/maplabeltabs.go's labelTabLineError, repeated here rather
// than shared because the two plan formats parse different things and a
// shared type would tempt someone to make them parse the same thing.
type LineError struct {
	Line   int
	Raw    string
	Reason string
}

// ParsePlan reads an area plan, one area per line:
//
//	Name = feed, feed, ...
//
// where a feed is `tab:<tab>` (one of tabs.All) or `label:<label name>`.
// Label names may contain spaces, so feeds are split on commas ALONE and each
// one is trimmed on both sides afterward. Blank lines and `#` comments are
// skipped, matching map-label-tabs' plan format and for the same reason: the
// operator's own vocabulary belongs in a file they supply, never compiled
// into the binary (CLAUDE.md's portability rule), and the file is worth
// letting them annotate.
//
// Example plan:
//
//	# Everything that should read as "people", regardless of tab.
//	Family = label:Family, label:Friends
//	# Bulk retail, whether or not it was ever labelled.
//	Shopping = tab:promotions, label:Deals
//	# Leave social and forums as their own areas; updates falls through to
//	# "needs sorting" until this plan gives it a home.
//	Social = tab:social
//	Forums = tab:forums
//
// Refusals are PER LINE and parsing continues past one — a plan with forty
// good lines and one typo must not be discarded wholesale, the same posture
// as parseLabelTabPlan. knownLabels bounds `label:` feeds against the
// account's real labels (case-insensitive) so a mistyped label name fails
// loudly instead of silently feeding nothing forever.
func ParsePlan(r io.Reader, knownLabels []string) (Config, []LineError, error) {
	known := make(map[string]bool, len(knownLabels))
	for _, l := range knownLabels {
		known[normalizeLabel(l)] = true
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var cfg Config
	var errs []LineError
	seenName := map[string]int{}
	seenTab := map[tabs.Tab]int{}
	seenLabel := map[string]int{}

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		trimmed := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		area, reason := parseAreaLine(trimmed, known, seenName, seenTab, seenLabel, lineNo)
		if reason != "" {
			errs = append(errs, LineError{Line: lineNo, Raw: raw, Reason: reason})
			continue
		}
		seenName[strings.ToLower(area.Name)] = lineNo
		cfg.Areas = append(cfg.Areas, area)
	}
	if err := sc.Err(); err != nil {
		return Config{}, errs, fmt.Errorf("areas: read plan: %w", err)
	}
	return cfg, errs, nil
}

// parseAreaLine parses one non-blank, non-comment line into an Area, or
// returns the reason it was refused (empty string on success).
//
// seenTab/seenLabel are committed only on a SUCCESSFUL parse of the whole
// line, so a line refused for one reason (say, an unknown tab) does not also
// consume a label that a later, otherwise-valid line wanted — a refusal must
// not have side effects beyond itself.
func parseAreaLine(
	line string,
	known map[string]bool,
	seenName map[string]int,
	seenTab map[tabs.Tab]int,
	seenLabel map[string]int,
	lineNo int,
) (Area, string) {
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return Area{}, "no '=' between the area name and its feeds"
	}
	name := strings.TrimSpace(line[:eq])
	rest := strings.TrimSpace(line[eq+1:])
	if name == "" {
		return Area{}, "empty area name"
	}
	if prev, dup := seenName[strings.ToLower(name)]; dup {
		return Area{}, fmt.Sprintf("area %q already defined on line %d", name, prev)
	}
	if rest == "" {
		return Area{}, fmt.Sprintf("area %q has no feeds", name)
	}

	area := Area{Name: name}
	for _, p := range strings.Split(rest, ",") {
		feed := strings.TrimSpace(p)
		if feed == "" {
			return Area{}, fmt.Sprintf("area %q: empty feed", name)
		}
		switch {
		case strings.HasPrefix(feed, "tab:"):
			tabName := strings.TrimSpace(strings.TrimPrefix(feed, "tab:"))
			t, ok := tabs.Parse(tabName)
			if !ok {
				return Area{}, fmt.Sprintf("%q is not a category tab (want one of %v)", tabName, tabs.All)
			}
			if prev, dup := seenTab[t]; dup {
				return Area{}, fmt.Sprintf("tab %q already feeds the area defined on line %d", t, prev)
			}
			area.Tabs = append(area.Tabs, t)
		case strings.HasPrefix(feed, "label:"):
			labelName := strings.TrimSpace(strings.TrimPrefix(feed, "label:"))
			if labelName == "" {
				return Area{}, fmt.Sprintf("area %q: empty label name", name)
			}
			key := normalizeLabel(labelName)
			if !known[key] {
				return Area{}, fmt.Sprintf("no label named %q on this account", labelName)
			}
			if prev, dup := seenLabel[key]; dup {
				return Area{}, fmt.Sprintf("label %q already feeds the area defined on line %d", labelName, prev)
			}
			area.Labels = append(area.Labels, labelName)
		default:
			return Area{}, fmt.Sprintf("feed %q has neither a tab: nor a label: prefix", feed)
		}
	}

	for _, t := range area.Tabs {
		seenTab[t] = lineNo
	}
	for _, l := range area.Labels {
		seenLabel[normalizeLabel(l)] = lineNo
	}
	return area, ""
}
