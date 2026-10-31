// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package sync_test

import (
	"context"
	"testing"
	"time"

	mrsync "mailroom/internal/sync"
)

// THE #740 FINDING, AGAINST THE REAL SERVER, BOTH HALVES. An EXAMINE's
// PERMANENTFLAGS cannot say whether a mailbox stores new keywords — Dovecot
// sends it empty, because nothing is permanent in a read-only session — while a
// read-write SELECT of the same server carries `\*`. And only the read-write
// SELECTs a session performs for its writes reach WritableKeywordProbes.
//
// The EXAMINE half is the reason the fix exists, kept as an assertion so a
// server or library that one day answers differently is noticed rather than
// silently making the old path look right.
func TestLiveKeywordProbeNeedsAReadWriteSelect(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	examined, err := sess.Select(ctx, mailboxInbox, mrsync.SelectOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("EXAMINE: %v", err)
	}
	if examined.PermanentFlagsAllowNew {
		t.Errorf("EXAMINE reported PERMANENTFLAGS \\* (%v) — the premise of #740 no longer holds on this server", examined.PermanentFlags)
	}
	prober, ok := sess.(interface{ WritableKeywordProbes() map[string]bool })
	if !ok {
		t.Fatal("the real session does not expose WritableKeywordProbes")
	}
	if got := prober.WritableKeywordProbes(); len(got) != 0 {
		t.Errorf("after an EXAMINE only, probes = %v, want none: EXAMINE cannot answer", got)
	}

	// A write: StoreFlags SELECTs read-write on the way.
	uid := appendTestMessage(t, ctx, sess, mailboxDrafts, "Keyword probe test message", nil)
	if err := sess.StoreFlags(ctx, mailboxDrafts, uid, []mrsync.Flag{mrsync.FlagFlagged}, nil); err != nil {
		t.Fatalf("StoreFlags: %v", err)
	}
	got := prober.WritableKeywordProbes()
	if accepts, seen := got[mailboxDrafts]; !seen || !accepts {
		t.Errorf("after a write in %s, probes = %v, want %s: true (Dovecot stores new keywords)", mailboxDrafts, got, mailboxDrafts)
	}
	if _, seen := got[mailboxInbox]; seen {
		t.Errorf("probes = %v name %s, which was only ever EXAMINEd", got, mailboxInbox)
	}
}
