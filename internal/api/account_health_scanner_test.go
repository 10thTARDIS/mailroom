// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"mailroom/internal/clamd"
)

// ⚠ THREE STATES THAT MUST NOT LOOK ALIKE (#395, ADR-027 §5c).
//
//	no scanner configured  -> the object is OMITTED
//	scanner down           -> reachable:false, stale:true
//	scanner fine but stale -> reachable:true,  stale:true
//
// Collapsing the first two would report an OUTAGE exactly like a deliberate
// choice not to scan. Collapsing the last two would lose the distinction between
// "nobody is answering" and "somebody is answering with year-old signatures" —
// and the second is the fail-open failure §5c is written about, because every
// scan still succeeds while the whole re-scan system is suspended.
type fakeScannerHealth struct {
	v   clamd.Version
	err error
}

func (f fakeScannerHealth) Version(context.Context) (clamd.Version, error) {
	return f.v, f.err
}

func TestScannerHealthDistinguishesAbsentFromDownFromStale(t *testing.T) {
	now := time.Now()

	// 1. No scanner installed at all: the field is nil, so the JSON omits it.
	a := &API{}
	if got := a.scannerHealth(context.Background()); got != nil {
		t.Errorf("no scanner configured produced %+v, want nil so the field is omitted", got)
	}

	// 2. Configured but not answering. ⚠ reachable false AND stale true: a
	// scanner nobody can reach is refreshing nothing, so its verdicts are
	// ageing whether or not we can measure by how much.
	down := &API{scanner: fakeScannerHealth{err: errors.New("connection refused")}}
	got := down.scannerHealth(context.Background())
	if got == nil {
		t.Fatal("an unreachable scanner produced nil — indistinguishable from having none")
	}
	if got.Reachable {
		t.Error("an unreachable scanner reported reachable")
	}
	if !got.Stale {
		t.Error("an unreachable scanner was not reported stale; nothing is refreshing verdicts")
	}

	// 3. Answering, with signatures from last week.
	old := &API{scanner: fakeScannerHealth{v: clamd.Version{
		Engine: "1.5.4", Database: 28110, Published: now.Add(-8 * 24 * time.Hour),
	}}}
	got = old.scannerHealth(context.Background())
	if !got.Reachable {
		t.Error("a scanner that answered reported unreachable")
	}
	if !got.Stale {
		t.Error("an 8-day-old database was not reported stale — this is §5c's silent suspension")
	}
	if got.DatabaseAgeSeconds == nil || *got.DatabaseAgeSeconds < 7*24*3600 {
		t.Errorf("DatabaseAgeSeconds = %v, want about 8 days", got.DatabaseAgeSeconds)
	}
	if got.DatabaseVersion != 28110 || got.EngineVersion != "1.5.4" {
		t.Errorf("version fields = %d/%q, want 28110/1.5.4", got.DatabaseVersion, got.EngineVersion)
	}

	// THE CONTROL. A fresh scanner is neither unreachable nor stale — without
	// this, every assertion above could pass because Stale was hardcoded true.
	fresh := &API{scanner: fakeScannerHealth{v: clamd.Version{
		Engine: "1.5.4", Database: 28110, Published: now.Add(-2 * time.Hour),
	}}}
	got = fresh.scannerHealth(context.Background())
	if !got.Reachable || got.Stale {
		t.Fatalf("a two-hour-old database reported reachable=%v stale=%v; the assertions above prove nothing",
			got.Reachable, got.Stale)
	}
}

// ⚠ AN UNKNOWN PUBLICATION TIME IS STALE AND ITS AGE IS null, NOT ZERO.
// A JSON zero would render in a panel as "0 seconds old" — the freshest possible
// database — for a daemon that declined to say. That is the fail-open bug
// rebuilt in the UI.
func TestScannerHealthReportsUnknownAgeAsNullAndStale(t *testing.T) {
	a := &API{scanner: fakeScannerHealth{v: clamd.Version{Engine: "1.5.4", Database: 28110}}}
	got := a.scannerHealth(context.Background())
	if got.DatabaseAgeSeconds != nil {
		t.Errorf("DatabaseAgeSeconds = %v, want nil; a zero renders as 'brand new'", *got.DatabaseAgeSeconds)
	}
	if !got.Stale {
		t.Error("a database of unknown age was reported fresh — 'we cannot tell' is not 'it is fine'")
	}
	if !got.Reachable {
		t.Error("the daemon answered, so it is reachable even though its age is unknown")
	}
}
