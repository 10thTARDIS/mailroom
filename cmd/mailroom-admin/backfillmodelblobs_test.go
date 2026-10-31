// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"
)

// The v1.34.7 report printed each user's detail lines BEFORE that user's summary
// row, so on a multi-user instance every user's hashes rendered directly beneath
// the PREVIOUS user's row. On a real instance that put several model
// artifacts under a user showing zero models.
//
// The numbers were right; the attribution was not readable. `blob-report` had
// the order right already, and this is the test that keeps both honest.
func TestUserBlockPutsDetailsUnderTheirOwnUser(t *testing.T) {
	const (
		quiet = "01QUIETUSER0000000000000000" // no models at all
		owner = "01OWNERUSER0000000000000000" // the one the hashes belong to
	)
	details := []string{
		"  missing    2ff3abfd (1 version(s), 7110 plain / 7170 stored)",
		"  missing    9c4d9573 (1 version(s), 7110 plain / 7170 stored)",
	}

	out := userBlock(quiet, userTotals{}, nil) +
		userBlock(owner, userTotals{models: 9, hashes: 5, missing: 5}, details)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	idx := func(prefix string) int {
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		t.Fatalf("no line starting %q in:\n%s", prefix, out)
		return -1
	}

	quietRow, ownerRow := idx(quiet), idx(owner)
	if quietRow > ownerRow {
		t.Fatalf("users rendered out of order (%d, %d) — the fixture does not exercise the bug",
			quietRow, ownerRow)
	}

	// THE ASSERTION: every detail line falls after the row of the user it
	// belongs to, and therefore never lands under the quiet user above it.
	for _, d := range details {
		at := idx(d)
		if at < ownerRow {
			t.Errorf("detail line %d renders at line %d, ABOVE its own user's row at %d — it "+
				"reads as belonging to %s, which has no models at all", at, at, ownerRow, quiet)
		}
	}

	// A user with no details owns exactly one line, so nothing can drift into
	// the gap between the two rows.
	if ownerRow-quietRow != 1 {
		t.Errorf("the quiet user's block is %d lines, want 1 — anything between the two rows is "+
			"content with no visible owner", ownerRow-quietRow)
	}
}

// TestUserBlockCountsMatchTheHeaderColumns pins each number to the column the
// header names. The report's whole argument is that these are SEPARATE signals
// (#177): "missing" and "no-file" are different problems, and a transposition
// here would silently swap one for the other in every run.
func TestUserBlockCountsMatchTheHeaderColumns(t *testing.T) {
	// Deliberately all-distinct values: equal ones would let a transposition pass.
	u := userTotals{models: 1, hashes: 2, haveRow: 3, missing: 4, noFile: 5, unreadable: 6, written: 7}
	fields := strings.Fields(userBlock("01USER00000000000000000000", u, nil))

	want := []string{"01USER00000000000000000000", "1", "2", "3", "4", "5", "6", "7"}
	if len(fields) != len(want) {
		t.Fatalf("row has %d fields, want %d: %q", len(fields), len(want), fields)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("column %d = %q, want %q (header order: USER MODELS HASHES HAVE-ROW "+
				"MISSING NO-FILE UNREADABLE WRITTEN)", i, fields[i], want[i])
		}
	}
}
