// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

// A SCANNER OUTAGE MUST PAUSE THE ACCOUNT — INCLUDING THE REAPER (#394, ADR-027 §4c).
//
// This is the mail-loss half of the malware-scanning increment, and it is the
// one place where getting it half-right is worse than not doing it at all.
//
// A refusal has two jobs. WITHHOLDING CURSORS makes it a retry: the mail stays
// on the provider and the next pass fetches it again. SKIPPING THE REAPER is
// what stops "we could not scan this" from becoming "the user deleted this".
// The first happens for free — syncMailbox withholds cursors on ANY error — so
// a wrong implementation looks correct in every test that only checks cursors.
//
// ⚠ THE DEFECT HAS A SHAPE, AND THE TESTS BELOW ARE BUILT AROUND IT. Without
// the sentinel branch in SyncAccountWith, an ingest failure in one mailbox is
// isolated and the pass CONTINUES: the account's other mailboxes are polled,
// then reaped, then folder-reconciled. "We could not scan the INBOX" becomes
// "mail was deleted from Archive". So the fixture has two mailboxes — one where
// the scan fails, one with something reapable — and
// TestAGenericIngestFailureStillLetsTheReaperRunElsewhere is the negative
// control proving that this fixture DOES reap when the sentinel is absent.
// Without that control, the assertion "nothing was reaped" could pass because
// nothing was ever reapable.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mailroom/internal/clamd"
	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

func archiveBox() sync.Mailbox { return sync.Mailbox{Name: "Archive", Delimiter: '/'} }

// scannerFixture builds the two-mailbox account described above.
//
//	INBOX    the server holds UID 1, which we have never ingested — so this
//	         mailbox FETCHES, and therefore this is where an ingest error lands.
//	Archive  the server holds UID 1; we hold live provenance for 1 AND 2, so
//	         UID 2 has vanished and is reapable. Its cursor is already at 2, so
//	         it fetches nothing and cannot fail.
//
// INBOX is listed first, so the failure happens before Archive is polled at all
// — which is what makes "did Archive still get reaped?" the question.
func scannerFixture(t *testing.T) (*fakeSession, *fakeStore, *fakeIngester, *Engine) {
	t.Helper()
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox(), archiveBox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1},
				msgs:   []sync.Message{msg(1, 0, "an unscannable message")},
			},
			"Archive": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 1},
				msgs:   []sync.Message{msg(1, 0, "archived")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 0, lastReapedAt: hoursAgo(2)})
	st.seedRow("Archive",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2, lastReapedAt: hoursAgo(2)}, 1, 2)
	st.reapResult = store.ReapResult{ProvenanceDropped: 1, MembershipChanged: 1, MessagesVanished: 1}
	st.retireCount = 1
	return sess, st, ing, reapEngine(sess, st, ing)
}

// THE CONTROL THAT MAKES THE FIXTURE MEAN SOMETHING. With a healthy ingester,
// this account reaps: Archive's vanished UID 2 is dropped. If this ever stops
// being true, the two tests below are asserting the absence of something that
// was never going to happen.
func TestTheScannerFixtureDoesReapWhenNothingFails(t *testing.T) {
	sess, st, ing, e := scannerFixture(t)
	_ = sess
	_ = ing

	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatalf("healthy pass failed: %v", err)
	}
	if len(st.reapCalls) == 0 {
		t.Fatal("the control reaped nothing — the two tests below would prove nothing")
	}
	if sum.MessagesIngested != 1 {
		t.Errorf("MessagesIngested = %d, want 1 (INBOX's new UID)", sum.MessagesIngested)
	}
	if _, ok := st.cursorCalls[st.rows["INBOX"].id]; !ok {
		t.Error("INBOX cursors were not advanced on a healthy pass")
	}
}

// ⚠ THE NEGATIVE CONTROL. An ORDINARY ingest failure in INBOX is isolated —
// that mailbox's cursors are withheld and the pass carries on — so Archive is
// still polled and STILL REAPED.
//
// This is not a bug: a transient IMAP or database fault genuinely is
// per-mailbox, and halting the whole account on one would be its own outage.
// The point of the test is that it establishes the DEFAULT behaviour, so the
// scanner test below is measuring the sentinel and not the fixture. Delete the
// sentinel branch from SyncAccountWith and the scanner test becomes this test.
func TestAGenericIngestFailureStillLetsTheReaperRunElsewhere(t *testing.T) {
	_, st, ing, e := scannerFixture(t)
	ing.err = errors.New("a transient fault that is nobody's pause signal")

	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatalf("a per-mailbox fault must not fail the pass: %v", err)
	}
	if len(sum.Errors) == 0 {
		t.Error("the INBOX failure was not reported at all")
	}
	if len(st.reapCalls) == 0 {
		t.Fatal("nothing was reaped — this control must show the reaper RUNNING, or the scanner test proves nothing")
	}
	if _, ok := st.cursorCalls[st.rows["INBOX"].id]; ok {
		t.Error("INBOX cursors advanced across an ingest failure")
	}
}

// THE PROPERTY. A scanner outage stops the whole pass: no cursors anywhere, and
// crucially NOTHING REAPED in the mailbox that synced perfectly well.
func TestAScannerOutagePausesTheAccountAndSkipsTheReaper(t *testing.T) {
	_, st, ing, e := scannerFixture(t)
	ing.err = &ingest.ScannerUnavailableError{Cause: clamd.ErrDaemonUnreachable, Consecutive: 1}

	sum, err := e.SyncAccount(context.Background(), testReq())

	// (a) The pass FAILS. A nil return would have the runner record SUCCESS and
	// hide a scanner that has been down for a week.
	//
	// ⚠ Errorf, NOT Fatalf, and deliberately: a Fatal here would short-circuit
	// the reaper assertions below, so the run that proves the sentinel matters
	// would only ever report "returned nil" and never exercise (b) at all. The
	// probe (delete the sentinel branch, watch this go red) has to reach every
	// assertion it claims to make.
	if err == nil {
		t.Errorf("SyncAccount returned nil across a scanner outage — the pause would be recorded as success; summary=%+v", sum)
	} else if !errors.Is(err, ingest.ErrScannerUnavailable) {
		t.Errorf("error = %v, want it to wrap ingest.ErrScannerUnavailable", err)
	}

	// (b) ⚠ THE HALF THAT IS EASY TO MISS. Archive polled cleanly and had a
	// vanished UID sitting there for the reaper — the control above proves the
	// reaper takes it. It must not have been taken here.
	if len(st.reapCalls) != 0 {
		t.Errorf("THE REAPER RAN DURING A SCANNER OUTAGE: %+v — 'we could not scan the INBOX' just became "+
			"'mail was deleted from Archive', which ADR-027 §4c calls worse than having no scanner at all", st.reapCalls)
	}
	if sum.MessagesVanished != 0 || sum.ProvenanceReaped != 0 {
		t.Errorf("summary reports reaping during a scanner outage: vanished=%d provenance=%d",
			sum.MessagesVanished, sum.ProvenanceReaped)
	}
	// The folder-deletion reconciler and the placement retirement are the other
	// two destructive phases behind the same early return.
	if len(st.deleteCalls) != 0 {
		t.Errorf("folders were reconciled away during a scanner outage: %v", st.deleteCalls)
	}
	if st.retireCalls != 0 {
		t.Errorf("canonical placements were retired during a scanner outage: %d calls", st.retireCalls)
	}

	// (c) Cursors are withheld EVERYWHERE, which is what makes the refusal a
	// retry rather than a skip. Archive never got as far as being polled.
	if len(st.cursorCalls) != 0 {
		t.Errorf("cursors advanced during a scanner outage: %v — the refused mail would never be asked for again", st.cursorCalls)
	}
}

// The sentinel must be the SCANNER's, not the quota's. Account health is built
// from err.Error() (syncrunner.recordHealth), so a collision here sends the
// operator to look at disk usage while a container is down.
func TestTheScannerPauseIsNotTheQuotaPause(t *testing.T) {
	_, _, ing, e := scannerFixture(t)
	ing.err = &ingest.ScannerUnavailableError{Cause: clamd.ErrDaemonUnreachable, Consecutive: 2}

	_, err := e.SyncAccount(context.Background(), testReq())
	if err == nil {
		t.Fatal("no error")
	}
	if errors.Is(err, ingest.ErrQuotaExhausted) {
		t.Error("a scanner outage matches ingest.ErrQuotaExhausted — account health would read 'over the storage quota'")
	}
	// The transport reason survives the wrapping, so the operator's log names
	// what actually failed rather than a category.
	if !errors.Is(err, clamd.ErrDaemonUnreachable) {
		t.Error("the clamd cause did not survive wrapping; the operator sees a category, not a reason")
	}

	health := err.Error()
	if !strings.Contains(health, "scanner") {
		t.Errorf("account health string %q does not name the scanner", health)
	}
	if strings.Contains(health, "quota") {
		t.Errorf("account health string %q mentions quota", health)
	}
	// It must also say the mail is safe. An operator reading this at 3am needs
	// to know whether anything was lost before they start looking for it.
	if !strings.Contains(health, "left on the provider") {
		t.Errorf("account health string %q does not say the mail is still on the provider", health)
	}
}

// A guard on the fixture's own timing assumption: the reaper is only due
// because lastReapedAt is old. If reapEngine's interval or the fake's clock
// changed, TestTheScannerFixtureDoesReapWhenNothingFails would go red rather
// than silently proving nothing — but stating it here makes the dependency
// visible at the point it matters.
func TestTheScannerFixtureIsReapDue(t *testing.T) {
	_, st, _, _ := scannerFixture(t)
	last := st.rows["Archive"].lastReapedAt
	if last == nil || time.Since(*last) < time.Hour {
		t.Fatalf("Archive lastReapedAt = %v; the fixture's reaper is not due, so its controls are vacuous", last)
	}
}
