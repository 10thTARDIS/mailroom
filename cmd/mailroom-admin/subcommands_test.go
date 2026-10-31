// SPDX-License-Identifier: AGPL-3.0-only

package main

// mailroom#350: the dispatcher's vocabulary used to live in three places.
//
// Two of them are now one — `subcommands` dispatches AND renders the usage line,
// so "the usage line lists a command that does not dispatch" is no longer
// expressible. The third is the package doc comment, which cannot be derived
// because it carries each command's FLAGS, and which had silently drifted to 33
// of 51 entries before anyone compared it.
//
// So this test guards the half that cannot be made structural. It checks
// MEMBERSHIP ONLY, never prose — asserting anything about the descriptions would
// turn a good comment into a test failure and make the comments worse.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docSubcommandLine matches a subcommand entry in the package doc comment:
// "//", a tab, then the name at the very start. The leading-[a-z] anchor is what
// excludes both the CONTINUATION lines (which begin with spaces after the tab)
// and the "Configuration" block's MAILROOM_* names, which are uppercase.
var docSubcommandLine = regexp.MustCompile(`^//\t([a-z][a-z0-9-]*)[ \t]`)

// docSubcommands reads the names out of main.go's package doc comment.
func docSubcommands(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Only the doc comment, which is everything above `package main`.
	doc, _, found := strings.Cut(string(src), "\npackage main")
	if !found {
		t.Fatal("cannot find `package main` in main.go — the doc comment cannot be isolated")
	}

	out := map[string]bool{}
	for _, line := range strings.Split(doc, "\n") {
		if m := docSubcommandLine.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	// ⚠ THE EMPTY-SIDE TRAP, WHICH THIS ISSUE ITSELF WARNS ABOUT. A comparison
	// against a list the extractor failed to populate prints a clean "everything
	// is missing" and means nothing — it happened once while writing this, when
	// the pattern used `\t` inside a grep -E that does not expand it. If the
	// doc-comment format ever changes so this regexp stops matching, fail here
	// rather than reporting 51 phantom omissions.
	if len(out) == 0 {
		t.Fatal("extracted ZERO subcommands from the package doc comment: the format changed " +
			"and this test would otherwise report every command as undocumented")
	}
	return out
}

// TestPackageDocDocumentsEverySubcommand fails in BOTH directions: a command
// that dispatches and is undocumented, and a documented name that no longer
// dispatches.
//
// ⚠ The second direction matters as much as the first. The export archive's
// manifest.json tells a reader to run `pgp-export-wrapped` to retrieve the
// private key WITHOUT WHICH THEIR ENCRYPTED MAIL IS PERMANENTLY UNREADABLE. A
// rename that updated the map and not the doc leaves the documentation pointing
// at a command that no longer exists — which is worse than an undocumented one,
// because it reads as authoritative.
func TestPackageDocDocumentsEverySubcommand(t *testing.T) {
	documented := docSubcommands(t)

	if len(subcommands) == 0 {
		t.Fatal("the subcommands map is empty — the other side of this comparison is meaningless")
	}

	var undocumented, stale []string
	for name := range subcommands {
		if !documented[name] {
			undocumented = append(undocumented, name)
		}
	}
	for name := range documented {
		if _, ok := subcommands[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(stale)

	if len(undocumented) > 0 {
		t.Errorf("%d subcommand(s) dispatch but are NOT in main.go's package doc comment: %s",
			len(undocumented), strings.Join(undocumented, " "))
	}
	if len(stale) > 0 {
		t.Errorf("%d name(s) are documented but NO LONGER dispatch: %s",
			len(stale), strings.Join(stale, " "))
	}
}

// TestSubcommandListIsDerivedFromTheMap pins the half that is now structural
// rather than tested: the usage line and the unknown-subcommand error are
// rendered from `subcommands`, so they cannot name a command that does not
// dispatch. If someone reintroduces a hand-maintained constant, this fails.
func TestSubcommandListIsDerivedFromTheMap(t *testing.T) {
	got := subcommandList()
	if got == "" {
		t.Fatal("subcommandList() is empty")
	}
	names := strings.Split(got, "|")
	if len(names) != len(subcommands) {
		t.Fatalf("subcommandList() has %d names, the map has %d — it is not derived from the map",
			len(names), len(subcommands))
	}
	for _, n := range names {
		if _, ok := subcommands[n]; !ok {
			t.Errorf("usage line offers %q, which does not dispatch", n)
		}
	}
	// Sorted, so the usage line is stable rather than map-iteration order.
	if !sort.StringsAreSorted(names) {
		t.Error("subcommandList() is not sorted; its output would vary between runs")
	}
}
