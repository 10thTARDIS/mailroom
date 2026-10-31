// SPDX-License-Identifier: AGPL-3.0-only

package clamd

import (
	"testing"
	"time"
)

// ⚠ THE FIRST STRING IS A REAL REPLY, captured from the deployed
// clamav/clamav:1.5 daemon on 2026-09-01 — not invented from documentation.
// The rest are the shapes clamd produces in degraded states, which matter more
// than the happy path here: this call exists to make a rotted scanner visible.
func TestParseVersionOnRealClamdOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resp       string
		wantEngine string
		wantDB     int
		wantPub    string // RFC3339, or "" for none
	}{
		{
			name: "the deployed daemon", resp: "ClamAV 1.5.4/28110/Tue Sep  1 06:26:54 2026",
			wantEngine: "1.5.4", wantDB: 28110, wantPub: "2026-09-01T06:26:54Z",
		},
		{
			// A double-digit day is a single space, so this and the case above
			// cover both spacings clamd emits.
			//
			// ⚠ They do NOT discriminate between the "_2" and "2" layouts:
			// probed, all four combinations parse, because time.Parse tolerates
			// the extra space. An earlier comment here claimed one of the two
			// would fail. Kept as format coverage, which is what it actually is.
			name: "double-digit day", resp: "ClamAV 1.5.4/28110/Wed Sep 30 06:26:54 2026",
			wantEngine: "1.5.4", wantDB: 28110, wantPub: "2026-09-30T06:26:54Z",
		},
		{
			// ⚠ WHAT clamd SAYS WITH NO DATABASE LOADED. This must parse, not
			// error: a daemon with no signatures at all is precisely the state
			// this call exists to surface, and refusing to parse it would hide
			// the worst case behind a transport-shaped error.
			name: "no database loaded", resp: "ClamAV 1.5.4",
			wantEngine: "1.5.4", wantDB: 0, wantPub: "",
		},
		{
			// ⚠ THE EXACT 44 BYTES THE DEPLOYED DAEMON RETURNED, captured off
			// the socket rather than read from `clamdscan --version` — which
			// might have been the CLI's own formatting rather than the
			// protocol's. It was not: the wire reply is the same string with a
			// single trailing NUL and NO newline. The `\n` here is extra
			// tolerance, not something clamd was observed to send.
			name: "wire framing", resp: "ClamAV 1.5.4/28110/Tue Sep  1 06:26:54 2026\x00",
			wantEngine: "1.5.4", wantDB: 28110, wantPub: "2026-09-01T06:26:54Z",
		},
		{
			// An unparsable database version leaves it at zero rather than
			// failing: zero is younger than every real version, so the re-scan
			// predicate treats those rows as due — the safe direction.
			name: "unparsable db version", resp: "ClamAV 1.5.4/main-63/Tue Sep  1 06:26:54 2026",
			wantEngine: "1.5.4", wantDB: 0, wantPub: "2026-09-01T06:26:54Z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVersion(tc.resp)
			if err != nil {
				t.Fatalf("parseVersion(%q): %v", tc.resp, err)
			}
			if got.Engine != tc.wantEngine {
				t.Errorf("Engine = %q, want %q", got.Engine, tc.wantEngine)
			}
			if got.Database != tc.wantDB {
				t.Errorf("Database = %d, want %d", got.Database, tc.wantDB)
			}
			pub := ""
			if !got.Published.IsZero() {
				pub = got.Published.Format(time.RFC3339)
			}
			if pub != tc.wantPub {
				t.Errorf("Published = %q, want %q", pub, tc.wantPub)
			}
		})
	}
}

// A reply that is not clamd's must be an error rather than a zero Version — the
// caller would otherwise read "engine unknown, database 0" as a rotted scanner
// and act on a parse failure as if it were a finding.
func TestParseVersionRejectsAForeignReply(t *testing.T) {
	for _, resp := range []string{"", "PONG", "stream: OK", "HTTP/1.1 200 OK"} {
		if v, err := parseVersion(resp); err == nil {
			t.Errorf("parseVersion(%q) = %+v, want an error", resp, v)
		}
	}
	// Positive control: the reply that IS clamd's still parses, or the loop
	// above proves only that parseVersion rejects everything.
	if _, err := parseVersion("ClamAV 1.5.4/28110/Tue Sep  1 06:26:54 2026"); err != nil {
		t.Fatalf("the control failed: %v — the rejections above prove nothing", err)
	}
}

// ⚠ THE FAIL-OPEN GUARD, AND THE REASON THIS TYPE HAS Stale() AT ALL.
//
// §5c gates every re-scan tier on "only if the database moved", so a freshclam
// that silently stops updating suspends the whole re-scan system with no signal:
// clamd keeps answering, scans keep succeeding, and the age shown to the user
// keeps advancing because a scan really did run — against a frozen database.
// Age is the only thing that can tell "has not moved" from "cannot move".
func TestStaleTreatsAFrozenDatabaseAsAFault(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	fresh := Version{Engine: "1.5.4", Database: 28110, Published: now.Add(-10 * time.Hour)}
	frozen := Version{Engine: "1.5.4", Database: 28110, Published: now.Add(-8 * 24 * time.Hour)}

	if fresh.Stale(now) {
		t.Error("a 10-hour-old database was called stale; the threshold would fire constantly")
	}
	if !frozen.Stale(now) {
		t.Error("an 8-day-old database was NOT called stale — this is the silent suspension §5c describes")
	}
	if age, ok := fresh.Age(now); !ok || age != 10*time.Hour {
		t.Errorf("Age = %v, %v; want 10h, true", age, ok)
	}
}

// ⚠ AN UNKNOWN PUBLICATION TIME IS STALE, NOT FRESH. A daemon that will not say
// how old its database is has not told us it is fine, and "no age" must never
// collapse into "age zero" — that is the fail-open bug rebuilt one level up.
func TestAnUnknownPublicationTimeIsStale(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	unknown := Version{Engine: "1.5.4", Database: 28110} // no Published

	if _, ok := unknown.Age(now); ok {
		t.Error("Age reported ok for a missing publication time")
	}
	if !unknown.Stale(now) {
		t.Error("a database of unknown age was treated as fresh — 'we cannot tell' is not 'it is fine'")
	}
	// Control: the same struct WITH a recent timestamp is not stale, so the
	// assertion above is about the missing field and not about the struct.
	fresh := unknown
	fresh.Published = now.Add(-time.Hour)
	if fresh.Stale(now) {
		t.Fatal("the control was stale too; this test proves nothing about the missing timestamp")
	}
}
