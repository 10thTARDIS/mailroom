// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// The plan file format and the two plan builders, tested where they can be
// falsified: no database, no file, just text in and values out.
//
// A REFUSED LINE MUST BE REPORTED, NOT SKIPPED. That is the property worth
// pinning hardest: in a plan file whose typo silently meant "leave this folder
// alone", an operator would apply a plan they did not write and the report would
// look correct (areas.ParsePlan makes the same argument for its own format).

func TestParseDayOnePlan(t *testing.T) {
	in := `
# the header is a comment
Receipts    | labelArchive
Newsletters | labelArchive | Reading
Invoices    | labelArchive | -      # label only, deliberately no area
To do       | bringToDesk
Old stuff   | leave

`
	lines, lineErrs, err := parseDayOnePlan(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lineErrs) != 0 {
		t.Fatalf("well-formed plan produced %d line error(s): %+v", len(lineErrs), lineErrs)
	}
	want := []dayOnePlanLine{
		{Folder: "Receipts", Action: "labelArchive"},
		{Folder: "Newsletters", Action: "labelArchive", Area: "Reading", HasArea: true},
		{Folder: "Invoices", Action: "labelArchive", Area: "", HasArea: true},
		{Folder: "To do", Action: "bringToDesk"},
		{Folder: "Old stuff", Action: "leave"},
	}
	if len(lines) != len(want) {
		t.Fatalf("parsed %d line(s), want %d: %+v", len(lines), len(want), lines)
	}
	for i, w := range want {
		got := lines[i]
		if got.Folder != w.Folder || got.Action != w.Action || got.Area != w.Area || got.HasArea != w.HasArea {
			t.Errorf("line %d = %+v, want folder=%q action=%q area=%q hasArea=%v",
				i, got, w.Folder, w.Action, w.Area, w.HasArea)
		}
	}
	// THE TWO AREA CASES ARE DIFFERENT AND MUST STAY DIFFERENT: an omitted third
	// field means "an area named after the folder", and `-` means "no area at
	// all". A single "empty means default" rule could not express the second.
	if lines[0].HasArea {
		t.Error("an omitted third field must not read as an explicit area choice")
	}
	if !lines[2].HasArea || lines[2].Area != "" {
		t.Errorf(`"-" must read as an explicit "no area" choice, got %+v`, lines[2])
	}
}

func TestParseDayOnePlanRefusesRatherThanSkips(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // a substring of the reason
	}{
		{"no action at all", "Receipts", "want `FOLDER"},
		{"an unknown action", "Receipts | archiveEverything", "action must be"},
		{"a typo in the action", "Receipts | labelarchive", "action must be"},
		{"too many fields", "Receipts | labelArchive | Money | extra", "want `FOLDER"},
		{"no folder name", " | labelArchive", "no folder name"},
		{"an empty third field is ambiguous", "Receipts | labelArchive |", "ambiguous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines, lineErrs, err := parseDayOnePlan(strings.NewReader(tc.line + "\n"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(lines) != 0 {
				t.Errorf("a refused line still produced a plan line: %+v", lines)
			}
			if len(lineErrs) != 1 {
				t.Fatalf("refused %d line(s), want 1: %+v", len(lineErrs), lineErrs)
			}
			if !strings.Contains(lineErrs[0].Reason, tc.want) {
				t.Errorf("reason = %q, want it to contain %q", lineErrs[0].Reason, tc.want)
			}
			if lineErrs[0].Raw != tc.line {
				t.Errorf("Raw = %q, want the line verbatim %q", lineErrs[0].Raw, tc.line)
			}
		})
	}
}

// censusFixture is a small census to build plans against: one plain folder with
// no existing area, one with one, and INBOX.
func censusFixture() (rows []store.FolderCensus, existingArea ulid.ULID) {
	existingArea = store.NewULID()
	return []store.FolderCensus{
		{MailboxID: store.NewULID(), Name: "Receipts"},
		{MailboxID: store.NewULID(), Name: "Reading", ExistingAreaID: &existingArea},
		{MailboxID: store.NewULID(), Name: "Inbox", Role: "inbox"},
	}, existingArea
}

func TestDefaultDayOnePlan(t *testing.T) {
	census, existingArea := censusFixture()
	plan := defaultDayOnePlan(census, 30)
	if plan.WindowDays != 30 {
		t.Errorf("WindowDays = %d, want 30", plan.WindowDays)
	}
	if len(plan.Folders) != 3 {
		t.Fatalf("plan has %d folder(s), want 3 (every census row)", len(plan.Folders))
	}
	for _, f := range plan.Folders {
		if f.Action != store.SetupActionLabelArchive {
			t.Errorf("folder %s: action = %q, want labelArchive (the default plan labels and archives everything)",
				f.MailboxID, f.Action)
		}
	}

	receipts := plan.Folders[0]
	if receipts.LabelName != "Receipts" {
		t.Errorf("the label defaults to the folder's own name, got %q", receipts.LabelName)
	}
	if receipts.AreaName != "Receipts" || receipts.AreaID != nil {
		t.Errorf("a folder with no matching area gets a NEW area of its own name, got %+v", receipts)
	}

	reading := plan.Folders[1]
	if reading.AreaID == nil || *reading.AreaID != existingArea {
		t.Errorf("a folder whose name matches an EXISTING area feeds that area, got %+v", reading)
	}
	if reading.AreaName != "" {
		t.Errorf("naming both an existing area and a new area name is ambiguous, got %+v", reading)
	}

	// INBOX gets NO folder label and NO area: its older mail is labelled with
	// each message's own resolved area label instead, per message.
	inbox := plan.Folders[2]
	if inbox.LabelName != "" || inbox.AreaName != "" || inbox.AreaID != nil {
		t.Errorf("INBOX must carry no folder label and no area, got %+v", inbox)
	}
}

func TestDayOnePlanFromCensus(t *testing.T) {
	census, existingArea := censusFixture()
	lines := []dayOnePlanLine{
		{Folder: "receipts", Action: store.SetupActionLabelArchive}, // case-insensitive match
		{Folder: "Reading", Action: store.SetupActionLabelArchive, HasArea: true, Area: ""},
		{Folder: "Inbox", Action: store.SetupActionLeave},
	}
	plan, unmatched := dayOnePlanFromCensus(census, lines, 14)
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	if plan.WindowDays != 14 {
		t.Errorf("WindowDays = %d, want 14", plan.WindowDays)
	}
	if len(plan.Folders) != 3 {
		t.Fatalf("plan has %d folder(s), want 3", len(plan.Folders))
	}
	if plan.Folders[0].MailboxID != census[0].MailboxID {
		t.Errorf("a lower-case folder name must match the census row: %+v", plan.Folders[0])
	}
	if plan.Folders[0].AreaName != "Receipts" {
		t.Errorf("an omitted area defaults to the folder name, got %q", plan.Folders[0].AreaName)
	}
	// `-`: an EXPLICIT no-area choice must win over the existing area of the same
	// name, or the file could not express "label only".
	if plan.Folders[1].AreaName != "" || plan.Folders[1].AreaID != nil {
		t.Errorf(`an explicit "-" must mean no area even when one of that name exists, got %+v`, plan.Folders[1])
	}
	_ = existingArea
	// A `leave` line carries no label and no area — there is nothing to name.
	if plan.Folders[2].LabelName != "" || plan.Folders[2].AreaName != "" {
		t.Errorf("a leave line must carry no label or area, got %+v", plan.Folders[2])
	}
}

// A plan line naming a folder the census does not have is REPORTED, and the
// caller refuses the whole run. Silently dropping it would apply a plan the
// operator did not write — and the most likely cause is that they named a
// special-use folder, which the census excludes on purpose.
func TestDayOnePlanFromCensusReportsUnmatchedFolders(t *testing.T) {
	census, _ := censusFixture()
	lines := []dayOnePlanLine{
		{Folder: "Receipts", Action: store.SetupActionLabelArchive},
		{Folder: "Sent", Action: store.SetupActionLabelArchive},
		{Folder: "Recipts", Action: store.SetupActionLabelArchive}, // a typo
	}
	plan, unmatched := dayOnePlanFromCensus(census, lines, 30)
	if len(plan.Folders) != 1 {
		t.Errorf("plan has %d folder(s), want 1 (only the matched one)", len(plan.Folders))
	}
	if len(unmatched) != 2 {
		t.Fatalf("unmatched = %v, want 2 entries", unmatched)
	}
	joined := strings.Join(unmatched, ",")
	for _, want := range []string{"Sent", "Recipts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("unmatched %v does not name %q", unmatched, want)
		}
	}
}

// The summary line is the sentence the whole feature is judged by, and the
// "nothing moves on the server" clause must be in it EVEN WHEN moves is 0 — a
// promise that appears only when it is being broken is not checkable.
func TestDayOneSummaryLineAlwaysSaysNothingMoves(t *testing.T) {
	line := dayOneSummaryLine(store.SetupCounts{Labels: 4404, Archived: 4276, Moves: 0, Leave: 55, EstimatedSeconds: 90})
	for _, want := range []string{"4,404", "4,276", "nothing moves on the server", "leave 55", "2 minute"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q does not contain %q", line, want)
		}
	}
	withMoves := dayOneSummaryLine(store.SetupCounts{Moves: 12})
	if !strings.Contains(withMoves, "nothing moves on the server") {
		t.Errorf("summary %q drops the promise when there ARE moves; it must always state it, "+
			"because the moves it names are the explicit bringToDesk exception", withMoves)
	}
}

func TestCommaDayOne(t *testing.T) {
	for in, want := range map[int]string{0: "0", 55: "55", 999: "999", 1000: "1,000", 4404: "4,404", 1234567: "1,234,567"} {
		if got := commaDayOne(in); got != want {
			t.Errorf("commaDayOne(%d) = %q, want %q", in, got, want)
		}
	}
}
