// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// sampleMeasure is the numbers of the section-4/5 integration fixtures
// (internal/ingest/placement_measure_integration_test.go), so the printer is
// read against figures worked out by hand rather than invented ones.
func sampleMeasure() store.PlacementMeasure {
	u, r, p := ulid.Make(), ulid.Make(), ulid.Make()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	left := func(events int, keys placement.KeyTally, areas map[ulid.ULID]placement.AreaPrecision, tot placement.AreaPrecision, served, untaught, mt, mu int) placement.LeftAloneResult {
		return placement.LeftAloneResult{Events: events, EventsByArea: map[ulid.ULID]int{u: 7, r: 3}, Keys: keys, Areas: areas, Total: tot,
			Served: served, Untaught: untaught, MovedAnyTaught: mt, MovedAnyUntaught: mu}
	}
	prec := func(k, c int) placement.AreaPrecision {
		return placement.AreaPrecision{Messages: k + c, Kept: k, Corrected: c, Gate: placement.Gate(placement.GateCounts{Kept: k, Corrected: c})}
	}
	keys := placement.KeyTally{
		Confident: map[ulid.ULID]placement.KeyCounts{u: {Addresses: 2}, r: {Addresses: 1}},
		New:       map[ulid.ULID]placement.KeyCounts{u: {Addresses: 1}, r: {Addresses: 1}},
		Reasons: map[placement.KeyKind]map[placement.Reason]int{
			placement.KeyAddress: {placement.Confident: 3, placement.Thin: 1, placement.Mixed: 1},
			placement.KeyDomain:  {placement.Thin: 4, placement.Mixed: 1},
		},
	}
	arch := placement.ArchiveResult{
		Events: 7, Untaught: 3,
		Keys: placement.KeyTally{
			Confident: map[ulid.ULID]placement.KeyCounts{r: {Addresses: 1}, p: {Addresses: 1}},
			Reasons: map[placement.KeyKind]map[placement.Reason]int{
				placement.KeyAddress: {placement.Confident: 2, placement.Mixed: 1, placement.Thin: 1},
				placement.KeyDomain:  {placement.Thin: 3, placement.Mixed: 1},
			},
		},
		Areas: map[ulid.ULID]placement.ArchiveAreaRow{
			r: {Messages: 5, ArchivedUnread: 1, Opened: 2, KeptUnopened: 1, ArchivedOther: 1,
				Strict:  placement.Gate(placement.GateCounts{Kept: 1, Corrected: 4}),
				Engaged: placement.Gate(placement.GateCounts{Kept: 1, Corrected: 2})},
		},
	}
	return store.PlacementMeasure{
		Now: now, Split: now.AddDate(0, 0, -45), WindowStart: now.AddDate(0, 0, -365),
		Options:   store.PlacementMeasureOptions{WindowDays: 365, SplitDays: 45},
		AreaNames: map[ulid.ULID]string{u: "Updates", r: "Promotions", p: "Primary"}, Primary: p, PrimaryName: "Primary",
		Abstentions: store.AbstentionTally{Days: 30, Since: now.AddDate(0, 0, -29), FirstRecorded: now.AddDate(0, 0, -9), DaysCovered: 10,
			ByReason: map[string]int{"none": 41, "thin": 12, "mixed": 5, "unauthenticated": 3}},
		Auth: store.AuthRefusals{Exact: true, Population: 10, Read: 10,
			Confident: store.AuthBucket{Messages: 8, OK: 2, AuthOnly: 4, AuthAlso: 1, OtherOnly: 1, AuthOnlyKeys: 2, CoveredKeys: 1, CoveredMessages: 2},
			Thin:      store.AuthBucket{Messages: 2, OK: 1, AuthOnly: 1, AuthOnlyKeys: 1}},
		RecentDays:     30,
		RecencyAddress: placement.Recency{Keys: 6, Mixed: 4, MixedRecent: 4, Rows: []placement.RecencyRow{{N: 2, Keys: 2, Recent: 1}, {N: 3, Keys: 1, Recent: 1}, {N: 5, Keys: 1, Recent: 1}}},
		RecencyDomain:  placement.Recency{Keys: 5, Mixed: 5, MixedRecent: 4, Rows: []placement.RecencyRow{{N: 2, Keys: 2, Recent: 1}, {N: 3, Keys: 1, Recent: 1}, {N: 5, Keys: 1, Recent: 1}}},
		LeftAlone:      left(10, keys, map[ulid.ULID]placement.AreaPrecision{u: prec(2, 1), r: prec(1, 1)}, prec(3, 2), 1, 4, 3, 2),
		LeftAloneAny:   left(13, keys, map[ulid.ULID]placement.AreaPrecision{u: prec(3, 2), r: prec(1, 1)}, prec(4, 3), 1, 2, 4, 1),
		LeftAloneCover: store.LeftAloneCoverage{DeskPlaced: 13, Opened: 10, EarliestOpen: now.AddDate(0, 0, -70), UnobservableMoved: 1,
			ExcludedRule: 1, ExcludedLearned: 1, Outcomes: 10},
		UserEvidence:    3,
		Archived:        arch,
		ArchivedCover:   store.ArchivedCoverage{EventsNoArea: 1, PrimaryOutcomes: 9},
		ListID:          placement.ListIDCeiling{Filings: 17, Unfilled: 1, MixedAddresses: 5, MultiList: 4, Separated: 2, Partial: 1, NotHelped: 1, SubKeysConfident: 4},
		ListIDAddresses: 5, ListIDCensus: store.RecipientCensus{Total: 60, Filled: 41},
	}
}

// The printer shows every section with counts only, names the bound wherever a
// number is one (an unfinished tally window, a capped scan, an unfilled column),
// prints "-" rather than 0.000 for nothing measured, and prints the split day.
func TestPrintPlacementMeasure(t *testing.T) {
	m := sampleMeasure()
	var buf bytes.Buffer
	printPlacementMeasure(&buf, m, []string{"mx.example"})
	out := buf.String()
	for _, want := range []string{
		"split day D = 2026-10-09",
		"recorded since 2026-11-14: 10 of 30 days covered (counts below are a LOWER BOUND on the window)",
		"none                  41",
		"held back, total      61",
		"needs a DMARC pass from a trusted authserv-id (mx.example)",
		"population: 10 message(s)",
		"confident          8       2         4          1          1          2           1             2",
		"last 5 filings agree:    1 key(s)",
		"events before D: 13 desk-placed message(s) old enough to judge, 10 of them opened",
		"OPENED and not moved (the signal): 10 event(s)",
		"merely NOT MOVED (contrast",
		"not judged (5/20)",
		"filed by hand into ANY area: taught keys 3 of 5 later message(s), untaught keys 2 of 4",
		"5 later message(s) in Primary",
		"Primary                1/0          0       0       0      0       0        - /",
		"0.200 / 0.036",
		"a List-Id key would make the address clean (every list in one area): 2",
		"(address + List-Id) keys that would reach 2 agreeing filings: 4",
		"41 of 60 messages are filled",
		"REPORT ONLY: nothing was written.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "@") && strings.Contains(strings.ReplaceAll(out, "@example", ""), "@") {
		t.Errorf("the report must hold counts only, but it contains an address-like token:\n%s", out)
	}
	t.Logf("\n%s", out)

	// A capped scan and a lower-bound mode say so.
	m.Auth.Exact, m.Auth.Truncated = false, true
	buf.Reset()
	printPlacementMeasure(&buf, m, nil)
	out = buf.String()
	for _, want := range []string{"LOWER BOUND: stored failure flag only", "CAPPED at --auth-scan-max"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	// An empty tally says it is empty rather than printing a table of zeros as a finding.
	m.Abstentions = store.AbstentionTally{Days: 30, Since: m.Now.AddDate(0, 0, -29), ByReason: map[string]int{}}
	buf.Reset()
	printPlacementMeasure(&buf, m, nil)
	if !strings.Contains(buf.String(), "nothing recorded yet") {
		t.Errorf("an empty tally must say so:\n%s", buf.String())
	}
}
