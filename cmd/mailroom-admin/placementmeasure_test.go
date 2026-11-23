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
			Served: served, Untaught: untaught, MovedAnyTaught: mt, UntaughtMoved: mu}
	}
	prec := func(k, c, agree, unres int) placement.AreaPrecision {
		return placement.AreaPrecision{Kept: k, Corrected: c, Agreement: agree, Unresolved: unres,
			Gate: placement.Gate(placement.GateCounts{Kept: k, Corrected: c})}
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
		Events: 7,
		Keys: placement.KeyTally{
			Confident: map[ulid.ULID]placement.KeyCounts{r: {Addresses: 1}, p: {Addresses: 1}},
			Reasons: map[placement.KeyKind]map[placement.Reason]int{
				placement.KeyAddress: {placement.Confident: 2, placement.Mixed: 1, placement.Thin: 1},
				placement.KeyDomain:  {placement.Thin: 3, placement.Mixed: 1},
			},
		},
		Areas: map[ulid.ULID]placement.ArchiveAreaRow{
			r: {Messages: 6, ArchivedUnread: 1, Opened: 1, SeenNoOpen: 1, KeptUnopened: 1, ArchivedOther: 1, RefiledOut: 1,
				Strict:  placement.Gate(placement.GateCounts{Kept: 2, Corrected: 4}),
				Engaged: placement.Gate(placement.GateCounts{Kept: 2, Corrected: 1})},
		},
		Untaught: placement.ArchiveAreaRow{Messages: 5, KeptUnopened: 3, Opened: 1, RefiledOut: 1,
			Strict:  placement.Gate(placement.GateCounts{Kept: 1, Corrected: 4}),
			Engaged: placement.Gate(placement.GateCounts{Kept: 1, Corrected: 1})},
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
		LeftAlone:      left(10, keys, map[ulid.ULID]placement.AreaPrecision{u: prec(1, 3, 1, 1), r: prec(1, 1, 0, 1)}, prec(2, 4, 1, 2), 1, 4, 3, 2),
		LeftAloneAny:   left(13, keys, map[ulid.ULID]placement.AreaPrecision{u: prec(2, 3, 2, 1), r: prec(1, 1, 0, 1)}, prec(3, 4, 2, 2), 1, 2, 4, 1),
		LeftAloneCover: store.LeftAloneCoverage{DeskPlaced: 13, Opened: 10, EarliestOpen: now.AddDate(0, 0, -70), UnobservableMoved: 1, UnobservableAfterD: 1,
			ExcludedRule: 1, ExcludedLearned: 1, ExcludedOther: 2, Outcomes: 10},
		UserEvidence: 3, PlacedRecent: 20, WindowMessages: 60, BulkImported: 9,
		Archived:        arch,
		ArchivedCover:   store.ArchivedCoverage{EventsNoArea: 1, PrimaryOutcomes: 9},
		ListID:          placement.ListIDCeiling{Filings: 17, Unfilled: 1, MixedAddresses: 5, MultiList: 5, Undetermined: 1, Separated: 1, Partial: 2, NotHelped: 1, SubKeysConfident: 3},
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
		"events judged from mail placed at least 7 days before D: 13 desk-placed message(s), 10 of them opened in the window",
		"OPENED and not moved (the signal): 10 event(s)",
		"merely NOT MOVED (contrast",
		"not judged (6/20)",
		"WOULD CHANGE: 2 later message(s)",
		"filed by hand into ANY area: taught senders 3 of 9 later message(s), untaught senders 2 of 4",
		"6 later message(s) in Primary (or filed out of it) from taught senders and 5 from senders the signal did not teach",
		"(base rate)",
		"placed by learned sorting in the same 30 days: 20  ->  held back 75.3% of the 81 it considered",
		"9 of them reached Mailroom more than a day after they were received",
		"UNDETERMINED",
		"1 message(s) the user moved on or after D would have been events",
		"Primary                   1/0      0       0       0      0      0       0        0        - / -",
		"a List-Id key would make the address clean (every list in one area): 1",
		"(address + List-Id) keys that would reach 2 agreeing filings: 3",
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

// Area names are typed by the user, so they must reach the terminal without
// escape sequences or bidi overrides, in both reports.
func TestAreaNamesAreSafeOnTheTerminal(t *testing.T) {
	m := sampleMeasure()
	evil := "Work\u001b[2J\u202eevil"
	for id := range m.AreaNames {
		m.AreaNames[id] = evil
	}
	m.PrimaryName = evil
	var buf bytes.Buffer
	printPlacementMeasure(&buf, m, nil)
	rep := store.PlacementReport{Enabled: true, Areas: []store.PlacementReportArea{{AreaID: ulid.Make(), Name: evil,
		Gate: placement.Gate(placement.GateCounts{})}},
		KeyReasons: map[placement.KeyKind]map[placement.Reason]int{placement.KeyAddress: {}, placement.KeyDomain: {}},
		Moves:      map[store.PlacementMove]int{}, Abstain: map[placement.Reason]int{}}
	printPlacementReport(&buf, rep, nil)
	out := buf.String()
	if strings.ContainsAny(out, "\u001b\u202e") {
		t.Errorf("an area name reached the output raw:\n%q", out)
	}
	if !strings.Contains(out, "Work\ufffd[2J\ufffdevil") {
		t.Errorf("the sanitised name should still be readable, with U+FFFD in place of the unsafe runes")
	}
	// Negative control: the same check on an unsanitised print must fail.
	if !strings.ContainsAny(evil, "\u001b\u202e") {
		t.Fatal("fixture: the name carries no unsafe rune")
	}
}
