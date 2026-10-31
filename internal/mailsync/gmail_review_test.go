// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"errors"
	"strings"
	"testing"
	"time"

	"mailroom/internal/ingest"
	"mailroom/internal/sync/gmailtest"
)

// Regression tests for the independent review of #907 (#52). Each one was
// written to fail against the code it reviewed (d322138).

// H1: A MISSING X-GM-LABELS IS NO READING. For each way the item can go
// missing on the wire — omitted, split into a second response, truncated
// with the connection dropped — the label sweep must hand ingest NO reading
// for that message, never an empty one (which would strip every label and
// the INBOX placement, and have the next sweep re-add them as new filings).
func TestGmailMissingLabelsAreNoReading(t *testing.T) {
	for _, fault := range []string{gmailtest.LabelsOmitted, gmailtest.LabelsSplit, gmailtest.LabelsTruncated} {
		t.Run(fault, func(t *testing.T) {
			r := newGmailRig(t, 0, time.Hour)
			r.add(3, 100, func(int) []string { return []string{`\Inbox`, "Work"} })
			r.primeAllMail()
			r.pass()
			before := len(r.ing.readings[2])
			if before == 0 {
				t.Fatal("setup: uid 2 was never read with its labels")
			}

			r.g.SetLabelFault(allMail, 2, fault)
			r.clock = r.clock.Add(2 * time.Hour) // the sweep is due
			_, _ = r.passErr()                   // a truncated response fails the pass; that is fine

			for _, got := range r.ing.readings[2][before:] {
				if len(got) == 0 {
					t.Fatalf("a %s X-GM-LABELS reached ingest as an EMPTY label set (%#v): it would strip uid 2's labels", fault, got)
				}
			}
			if _, changed := r.ing.changes[2]; changed {
				t.Errorf("a %s X-GM-LABELS changed uid 2's labels", fault)
			}
			// The other messages still carry their real readings.
			if got := r.ing.labels[1]; len(got) != 2 {
				t.Errorf("uid 1 labels = %q", got)
			}
		})
	}
}

// H2 (1, 3): THE PAUSES ARE ASKED ABOUT BEFORE ANY DOWNLOAD. With the scanner
// unavailable, a Gmail pass downloads nothing (the generic engine learns from
// the first failed ingest — after a chunk has been drained off the wire).
func TestGmailScannerPauseDownloadsNothing(t *testing.T) {
	r := newGmailRig(t, 500, 0)
	r.add(10, 100, nil)
	r.primeAllMail()
	r.ing.preflight = &ingest.ScannerUnavailableError{Cause: errors.New("connection refused")}
	for i := 0; i < 3; i++ {
		if _, err := r.passErr(); !errors.Is(err, ingest.ErrScannerUnavailable) {
			t.Fatalf("pass %d: err = %v, want the scanner pause", i, err)
		}
	}
	if b, n := r.g.BodyBytesServed(); b != 0 || n != 0 {
		t.Errorf("a paused Gmail pass downloaded %d bodies (%d bytes)", n, b)
	}
	if r.st.metered() != 0 {
		t.Errorf("metered %d bytes for a pass that downloaded nothing", r.st.metered())
	}
}

// H2 (1, 2): EVERY DOWNLOADED BYTE IS METERED, EVEN WHEN INGEST FAILS. A
// message ingest can never store (a poison message) fails its chunk on every
// pass, and the drain downloads the rest of the chunk unseen. Before the fix
// none of it was metered, so every pass re-downloaded the chunk: unbounded.
// Now the meter covers everything served, and repeated passes stop at the
// budget.
func TestGmailFailedIngestIsMeteredAndBounded(t *testing.T) {
	r := newGmailRig(t, 500, 0)
	r.add(10, 100, nil)
	r.primeAllMail()
	r.ing.poison[9] = true // in the first history chunk (uids 7-10), not the first message served
	for i := 0; i < 8; i++ {
		_, _ = r.passErr()
	}
	served, _ := r.g.BodyBytesServed()
	if served > 500 {
		t.Errorf("eight failing passes downloaded %d bytes against a 500-byte budget", served)
	}
	if m := r.st.metered(); m < served {
		t.Errorf("metered %d bytes but the server served %d: downloads escaped the budget", m, served)
	}
}

// H2 (4): a label that decodes to a NUL ("&AAA-") cannot block its message:
// it is sanitised away before anything is stored.
func TestGmailNulLabelSanitised(t *testing.T) {
	r := newGmailRig(t, 0, 0)
	r.add(1, 100, nil)
	r.g.SetLabelFault(allMail, 1, "")
	r.primeAllMail()
	r.gmailWire(1, `"&AAA-x" Work`)
	r.pass()
	got := r.ing.labels[1]
	if len(got) != 2 || got[0] != "x" || got[1] != "Work" {
		t.Errorf("labels = %q, want [x Work]", got)
	}
	for _, l := range got {
		if strings.ContainsRune(l, 0) {
			t.Errorf("a NUL survived into label %q", l)
		}
	}
}

// M1: A GAP REFETCH IS SIZED AND STAYS INSIDE THE BUDGET.
func TestGmailGapRefetchStaysInBudget(t *testing.T) {
	r := newGmailRig(t, 100_000, 0)
	r.add(10, 100, nil)
	r.primeAllMail()
	r.pass() // imports everything: 1000 bytes metered
	served0, _ := r.g.BodyBytesServed()

	// The budget now leaves 300; six gaps (600 bytes) open up.
	r.req.Gmail.DailyBudgetBytes = 1300
	row := r.st.rows[allMail]
	for u := int64(1); u <= 6; u++ {
		delete(row.prov, u)
	}
	row.lastReapedAt = nil
	r.clock = r.clock.Add(2 * time.Hour)
	_, _ = r.passErr()
	served, _ := r.g.BodyBytesServed()
	if got := served - served0; got > 300 {
		t.Errorf("the gap refetch downloaded %d bytes with 300 left in the budget", got)
	}
	if got := served - served0; got == 0 {
		t.Error("no gap was refetched at all; the budget assertion above proves nothing")
	}
}

// M2: THE METER IS A ROLLING 24 HOURS. A pass at 23:30 and another at 00:30
// are inside one 24-hour window, so together they stay inside one budget.
func TestGmailBudgetRollsAcrossMidnight(t *testing.T) {
	r := newGmailRig(t, 500, 0)
	r.clock = time.Date(2026, 10, 27, 23, 30, 0, 0, time.UTC)
	r.add(10, 100, nil)
	r.primeAllMail()
	r.pass()
	r.clock = r.clock.Add(time.Hour) // 00:30, the next calendar day
	r.pass()
	if served, _ := r.g.BodyBytesServed(); served > 500 {
		t.Errorf("an hour apart across midnight, two passes downloaded %d bytes against a 500-byte budget", served)
	}
}

// LOW: label views other than INBOX/Sent/Drafts are marked not synced.
func TestGmailLabelViewsMarkedNotSynced(t *testing.T) {
	r := newGmailRig(t, 0, 0)
	r.primeAllMail()
	r.pass()
	for name, want := range map[string]bool{
		"Work": true, "Receipts": true, "Été": true, "[Gmail]/Important": true, "[Gmail]/Starred": true,
		"INBOX": false, "[Gmail]/Sent Mail": false, "[Gmail]/Drafts": false, allMail: false,
		"[Gmail]/Spam": false, "[Gmail]/Trash": false,
	} {
		row, ok := r.st.rows[name]
		if !ok {
			t.Errorf("%s: no row", name)
			continue
		}
		if r.st.scopeOff[row.id] != want {
			t.Errorf("%s: sync scope off = %v, want %v", name, r.st.scopeOff[row.id], want)
		}
	}
}

// LOW: AN INTERRUPTED LABEL SWEEP RESUMES WHERE IT STOPPED instead of
// restarting from the floor on the next pass.
func TestGmailLabelSweepResumes(t *testing.T) {
	old := gmailSweepChunk
	gmailSweepChunk = 2
	t.Cleanup(func() { gmailSweepChunk = old })
	r := newGmailRig(t, 0, time.Hour)
	r.add(6, 100, func(int) []string { return []string{"A"} })
	r.primeAllMail()
	r.pass()

	r.g.SetLabelFault(allMail, 4, gmailtest.LabelsTruncated) // the sweep's second chunk (3:4) dies
	r.clock = r.clock.Add(2 * time.Hour)
	_, _ = r.passErr()
	if next := r.st.sweepNext[r.ing.allMail]; next != 3 {
		t.Fatalf("sweep progress = %d, want 3 (chunk 1:2 done)", next)
	}
	r.g.SetLabelFault(allMail, 4, "")
	r.g.ResetCounters()
	r.clock = r.clock.Add(time.Minute) // not due by interval: only the resume runs it
	r.pass()
	for _, f := range r.g.Fetches() {
		if strings.Contains(f, " 1:2 ") {
			t.Errorf("the sweep restarted from the floor: %s", f)
		}
	}
	if _, ok := r.st.sweepNext[r.ing.allMail]; ok {
		t.Error("a finished sweep left progress behind")
	}
}

// gmailWire pins uid's X-GM-LABELS wire form.
func (r *gmailRig) gmailWire(uid uint32, wire string) {
	r.g.SetWireLabels(allMail, uid, wire)
}
