// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"mailroom/internal/rules"
)

// candidateMatches is the single funnel every number in `suggest-report` passes
// through, and its failure mode is SILENT: a mis-built probe rule makes
// Evaluate return nothing for every message, the report prints zeros, and the
// zeros read as "induction reaches nothing" rather than "the measurement is
// broken". So it gets a positive case AND a negative control — the control is
// what proves a zero here can mean something.
func TestCandidateMatchesFiresAndRefuses(t *testing.T) {
	cond := rules.Condition{All: []rules.Condition{
		{Field: "fromDomain", Op: rules.OpEquals, Value: "shop.example"},
	}}

	hit := rules.MessageFacts{
		From:       "deals@shop.example",
		FromDomain: "shop.example",
		Subject:    "Sale",
		Headers:    map[string]string{},
	}
	if !candidateMatches(cond, "Promotions", hit) {
		t.Fatal("a message the condition plainly matches did not fire — the probe rule is mis-built, " +
			"and every count in suggest-report would be a silent zero")
	}

	// NEGATIVE CONTROL: the same probe must refuse a message it should not
	// match. Without this, a function that returned true unconditionally would
	// pass the assertion above.
	miss := rules.MessageFacts{
		From:       "hello@other.example",
		FromDomain: "other.example",
		Subject:    "Sale",
		Headers:    map[string]string{},
	}
	if candidateMatches(cond, "Promotions", miss) {
		t.Error("the probe fired on a message from a different domain; it is not discriminating at all")
	}
}

// THE TAUTOLOGY THAT MADE THIS REPORT'S FIRST REAL RUN MEANINGLESS, pinned so
// nobody reintroduces it.
//
// ⚠ THE VERSION OF THIS TEST THAT SHIPPED UNTIL #298 COULD NOT FAIL. It read:
//
//	const collateralOfThisCandidate = 1
//	if collateralOfThisCandidate <= 0 { t.Fatal(...) }
//
// `1 <= 0` is a compile-time constant, so the assertion was unreachable. It read
// no Candidate field and called nothing in the production path, and it was cited
// in #295 as evidence that suggest-report was safe. Re-adding the
// `|| c.Collateral > 0` term to the report left `go test ./cmd/mailroom-admin/...`
// entirely green.
//
// What the property actually is: Collateral counts the NON-POSITIVE messages a
// candidate matches, and every unlabelled message is non-positive. So a
// candidate that reaches unlabelled mail has Collateral > 0 BY DEFINITION —
// filtering on `Collateral <= 0` selects exactly the candidates that can produce
// no suggestion, forcing REACHED to 0 for every label on every corpus, forever.
// The first real run printed "0 reached", which read as a finding about induction
// and was a fact about the filter. The true number was most of the candidates.
//
// Stated as something falsifiable: the bar must not read Collateral AT ALL.
// Varying it across its whole range must not change a single verdict.
func TestSuggestFilterIgnoresCollateralEntirely(t *testing.T) {
	// Two candidates that straddle every OTHER term in the bar, so this cannot
	// pass by testing only cases the filter already rejects for another reason.
	bases := []struct {
		name string
		c    rules.Candidate
		want suggestSkipReason
	}{
		{"kept", rules.Candidate{Conflicts: 0, Covers: 5, Positives: 5}, suggestKept},
		{"conflicting", rules.Candidate{Conflicts: 1, Covers: 5, Positives: 5}, suggestSkipConflict},
		{"thin", rules.Candidate{Conflicts: 0, Covers: 1, Positives: 1}, suggestSkipCovers},
	}
	// 0 is the value the reintroduced bug would key on; the rest span the range
	// a real corpus produces.
	for _, collateral := range []int{0, 1, 2, 50, 1000} {
		for _, b := range bases {
			c := b.c
			c.Collateral = collateral
			if got := suggestFilter(c, 2); got != b.want {
				t.Errorf("suggestFilter(%s, Collateral=%d) = %v, want %v — the bar has started "+
					"reading Collateral. A candidate that reaches unlabelled mail HAS collateral, "+
					"so any term over it excludes precisely the candidates capable of producing a "+
					"suggestion and makes the report vacuous (#248, #298).",
					b.name, collateral, got, b.want)
			}
		}
	}
}

// AND THE BAR HAS TO BE THE ONE THAT WAS JUST TESTED.
//
// The test above pins suggestFilter. That is only worth something to
// `suggest-report` if suggest-report actually calls it — and until #298 it did
// not: it carried an inline `c.Conflicts > 0 || c.Covers < *minCovers`, a second
// copy of the same predicate. runSuggestReport needs a database and has no test
// callers, so nothing tied the copy to its original in either direction.
//
// This is a source-level check for the same reason
// TestEveryIframeCarriesTheMessageSandbox is one: the alternative is a test that
// cannot run without Postgres, and the property is about which function the code
// calls, which the source answers directly.
func TestSuggestReportUsesTheSharedFilter(t *testing.T) {
	// COMMENTS ARE STRIPPED FIRST, and finding out why is the reason this note
	// exists: the first version of this test grepped the raw file and went red on
	// its own explanatory prose, because the comment above the call site QUOTES
	// the inline bar it replaced. A test that cannot tell code from a comment
	// about code would fail every time someone documented the defect properly.
	// Re-printing the parsed AST drops comments and normalises spacing, so what
	// is scanned below is what the compiler sees.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "suggestreport.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		t.Fatal(err)
	}
	body := buf.String()

	// CONTROL ON THE STRIPPING ITSELF. If the parse or the re-print produced
	// something empty or unrecognisable, every assertion below would pass
	// vacuously — the exact failure mode this whole issue is about. This name
	// exists in the file for an unrelated reason and must survive the round trip.
	if !strings.Contains(body, "func runSuggestReport") {
		t.Fatalf("the parsed re-print of suggestreport.go does not contain runSuggestReport "+
			"(%d bytes) — the scan below is not looking at the source it thinks it is", len(body))
	}

	if !strings.Contains(body, "suggestFilter(c, *minCovers) != suggestKept") {
		t.Error("suggestreport.go no longer applies suggestFilter to its induced candidates. " +
			"Its bar must be the shared one, or the tests above stop covering this command " +
			"(#298 item 3).")
	}

	// THE NEGATIVE HALF, and it is the half that matters: calling suggestFilter
	// somewhere does not stop a second, hand-written bar sitting next to it. The
	// reintroduced #248 bug would look exactly like that.
	for _, banned := range []string{"c.Collateral", "c.Conflicts >", "c.Covers <"} {
		if strings.Contains(body, banned) {
			t.Errorf("suggestreport.go tests %q directly. Candidate acceptance belongs to "+
				"suggestFilter alone — a second bar here is how the original defect (#248) got "+
				"in, and how it would get back in.", banned)
		}
	}
}
