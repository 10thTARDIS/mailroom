// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// The backfill report says it is EXACT, prints a line per key with the
// sender address (operator-only), every reason including the zero ones, an
// exclude that matched nothing as 0, and REPORT ONLY when not applying. It
// has no subject to print: the plan carries none, which is the guarantee.
func TestPrintPlacementBackfill(t *testing.T) {
	primary, updates := ulid.Make(), ulid.Make()
	key := placement.Key{Kind: placement.KeyAddress, Value: "alerts@bank.example"}
	plan := store.PlacementBackfillPlan{
		Enabled: true, Exact: true, Filings: 4,
		Scanned: 10, Held: 4, NoOp: 1, OffDesk: 1,
		Abstain: map[placement.Reason]int{placement.Unauthenticated: 2},
		Moves: []store.PlacementBackfillMove{
			{MessageID: ulid.Make(), Key: key, AreaID: updates, NowIn: primary},
			{MessageID: ulid.Make(), Key: key, AreaID: updates},
		},
		Pairs: map[store.PlacementMove]int{{From: primary, To: updates}: 1, {To: updates}: 1},
		Keys: []store.PlacementBackfillKey{{Key: key, AreaID: updates, Moves: 2,
			NowIn:   map[ulid.ULID]int{primary: 1, {}: 1},
			Skipped: map[placement.Reason]int{placement.Unauthenticated: 2}}},
		Excluded:  map[string]int{"nobody.example": 0},
		AreaNames: map[ulid.ULID]string{primary: "Primary", updates: "Updates"},
	}
	if plan.Accounted() != plan.Scanned {
		t.Fatalf("fixture: partition %d != scanned %d", plan.Accounted(), plan.Scanned)
	}
	var buf bytes.Buffer
	printPlacementBackfill(&buf, plan, nil, []string{"mx.example"}, false)
	out := buf.String()
	for _, want := range []string{
		"checks: EXACT",
		"learned placement: ON",
		"move 2",
		fmt.Sprintf("  %-42s %6d", "sender not authenticated", 2),
		fmt.Sprintf("  %-42s %6d", "ambiguous From", 0),
		fmt.Sprintf("  %-42s %6d", "message bytes unreadable", 0),
		"accounted for 10 of 10 scanned",
		fmt.Sprintf("  %-42s %6d message(s)", "nobody.example", 0),
		"alerts@bank.example",
		"(needs sorting) 1, Primary 1",
		"unauthenticated 2",
		"REPORT ONLY: nothing was written.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	plan.Enabled = false
	printPlacementBackfill(&buf, plan, nil, nil, true)
	if out := buf.String(); !strings.Contains(out, "--apply is refused") || strings.Contains(out, "REPORT ONLY") {
		t.Errorf("an OFF switch must say --apply is refused:\n%s", out)
	}
}

func TestValidateExcludes(t *testing.T) {
	for _, ok := range []string{"a@b.example", "B.Example", "@b.example"} {
		if err := validateExcludes([]string{ok}); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "localhost", "a@", "@", "a b.example", "a.example,b.example"} {
		if err := validateExcludes([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
