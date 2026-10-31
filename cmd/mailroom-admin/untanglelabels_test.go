// SPDX-License-Identifier: AGPL-3.0-only

package main

// The plan parser is the part of untangle-labels that has to run with no
// database at all: every line it gets wrong is a message the database layer
// will never even be asked about, so these pin the exact set of things a
// plan file is allowed to look like (mailroom#219).

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestParseUntanglePlanSkipsCommentsAndBlankLines(t *testing.T) {
	id1, id2 := ulid.Make(), ulid.Make()
	plan := "# a comment\n" +
		"\n" +
		id1.String() + "\tPersonal\n" +
		"   \n" + // whitespace-only line
		"  # indented comment\n" +
		id2.String() + "\tReceipts\n"

	entries, errs, err := parseUntanglePlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected line errors: %+v", errs)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(entries), entries)
	}
	if entries[0].MessageID != id1 || entries[0].KeepLabel != "Personal" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].MessageID != id2 || entries[1].KeepLabel != "Receipts" {
		t.Errorf("entry 1 = %+v", entries[1])
	}
}

// CRLF is what a plan file edited on Windows will carry, and it must parse
// identically to a bare-\n file — no line error, no mangled label text.
func TestParseUntanglePlanHandlesCRLF(t *testing.T) {
	id := ulid.Make()
	plan := "# comment\r\n" +
		"\r\n" +
		id.String() + "\tNewsletters\r\n"

	entries, errs, err := parseUntanglePlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected line errors: %+v", errs)
	}
	if len(entries) != 1 || entries[0].MessageID != id || entries[0].KeepLabel != "Newsletters" {
		t.Fatalf("entries = %+v", entries)
	}
}

// The format is TAB-delimited specifically so a label name may contain
// spaces. A line delimited by a space instead of a tab must be refused, not
// guessed at by splitting on whitespace.
func TestParseUntanglePlanRequiresATabNotSpaces(t *testing.T) {
	id := ulid.Make()
	plan := id.String() + " Personal\n"

	entries, errs, err := parseUntanglePlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none (no tab present)", entries)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one", errs)
	}
	if !strings.Contains(errs[0].Reason, "tab") {
		t.Errorf("reason = %q, want it to mention the missing tab", errs[0].Reason)
	}
}

// A label name is free to contain spaces of its own; the tab is what marks
// the boundary, not whitespace in general.
func TestParseUntanglePlanAllowsSpacesInTheLabelName(t *testing.T) {
	id := ulid.Make()
	plan := id.String() + "\tNeeds Review\n"

	entries, errs, err := parseUntanglePlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected line errors: %+v", errs)
	}
	if len(entries) != 1 || entries[0].KeepLabel != "Needs Review" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestParseUntanglePlanRejectsMalformedULID(t *testing.T) {
	plan := "not-a-valid-ulid\tPersonal\n"

	entries, errs, err := parseUntanglePlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one", errs)
	}
	if !strings.Contains(errs[0].Reason, "not a valid ULID") {
		t.Errorf("reason = %q, want it to name the ULID problem", errs[0].Reason)
	}
}

func TestParseUntanglePlanRejectsMissingLabelColumn(t *testing.T) {
	id := ulid.Make()
	plan := id.String() + "\t\n" // tab present, nothing after it

	entries, errs, err := parseUntanglePlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one", errs)
	}
	if !strings.Contains(errs[0].Reason, "label column") {
		t.Errorf("reason = %q, want it to name the missing label column", errs[0].Reason)
	}
}

// A message id repeated across lines is refused WHOLESALE — every occurrence
// — because the plan disagrees with itself about that message and picking
// one line over the other would be exactly the guessing this command refuses
// to do.
func TestParseUntanglePlanRejectsDuplicateMessageIDs(t *testing.T) {
	id := ulid.Make()
	other := ulid.Make()
	plan := id.String() + "\tPersonal\n" +
		other.String() + "\tReceipts\n" +
		id.String() + "\tNewsletters\n"

	entries, errs, err := parseUntanglePlan(strings.NewReader(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || entries[0].MessageID != other {
		t.Fatalf("entries = %+v, want only the non-duplicated id", entries)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %+v, want one per duplicated occurrence", errs)
	}
	for _, e := range errs {
		if !strings.Contains(e.Reason, "appears on lines") {
			t.Errorf("reason = %q, want it to explain the duplicate", e.Reason)
		}
	}
	// Both the original occurrence (line 1) and the repeat (line 3) are
	// reported — losing either would hide which lines are wrong.
	if errs[0].Line != 1 || errs[1].Line != 3 {
		t.Errorf("errs line numbers = %d, %d, want 1 and 3", errs[0].Line, errs[1].Line)
	}
}
