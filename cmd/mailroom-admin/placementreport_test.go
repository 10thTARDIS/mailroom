// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// The printer says STOPPED for a gated area, prints counts per area pair for
// the backfill, and prints "-" (never 0.000) for an area with nothing
// resolved: no evidence must not read as a measurement of zero.
func TestPrintPlacementReport(t *testing.T) {
	primary, updates := ulid.Make(), ulid.Make()
	rep := store.PlacementReport{
		Enabled: true,
		Filings: 7,
		Areas: []store.PlacementReportArea{
			{AreaID: primary, Name: "Primary", Gate: placement.Gate(placement.GateCounts{})},
			{AreaID: updates, Name: "Updates", ConfidentAddresses: 3,
				Counts: store.PlacementAreaCounts{Kept: 19, Corrected: 1, GateKept: 19, GateCorrected: 1,
					BackfillOpen: 4, BackfillKept: 30, BackfillCorrected: 2},
				Gate: placement.Gate(placement.GateCounts{Kept: 19, Corrected: 1})},
		},
		KeyReasons: map[placement.KeyKind]map[placement.Reason]int{
			placement.KeyAddress: {placement.Confident: 3, placement.Mixed: 1},
			placement.KeyDomain:  {},
		},
		Moves:   map[store.PlacementMove]int{{From: primary, To: updates}: 5, {To: updates}: 2},
		Abstain: map[placement.Reason]int{},
	}
	var buf bytes.Buffer
	printPlacementReport(&buf, rep, []string{"mx.example"})
	out := buf.String()
	for _, want := range []string{
		"learned placement: ON",
		"STOPPED (20 resolved",
		"would move 7 desk message(s)",
		"Primary              Updates                     5",
		"(needs sorting)      Updates                     2",
		"confident 3 · thin 0 · mixed 1",
		"DMARC pass from a trusted authserv-id (mx.example)",
		"sender not authenticated 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	// The backfill outcomes are their own row, ungated; Primary has none and
	// prints no such row.
	backfillRows := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  (backfill)") {
			backfillRows++
			if f := strings.Fields(l); len(f) < 5 || f[1] != "4" || f[2] != "30" || f[3] != "2" || !strings.HasSuffix(l, "not gated") {
				t.Errorf("backfill row = %q, want open 4, kept 30, corrected 2, not gated", l)
			}
		}
	}
	if backfillRows != 1 {
		t.Errorf("backfill rows = %d, want 1 (Updates only):\n%s", backfillRows, out)
	}
	if !math.IsNaN(rep.Areas[0].Gate.LCB) {
		t.Fatal("fixture: an empty gate must have a NaN bound")
	}
	primaryLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Primary ") {
			primaryLine = l
		}
	}
	if strings.Contains(primaryLine, "0.000") || !strings.Contains(primaryLine, " - ") {
		t.Errorf("an area with nothing resolved must print '-' for precision, got %q", primaryLine)
	}
}
