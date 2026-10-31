// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The re-scan predicate must TERMINATE (#409, #407, #415, ADR-027 §5c).
//
// ⚠ THE SIBLING FILE PINS THAT DUE ROWS ARE FOUND. THIS ONE PINS THAT THEY
// LEAVE, and nothing asserted that before — which is how v1.61.1 shipped a fixed
// point. rescan_integration_test.go asserts a row with no digest IS due; nothing
// asserted that a row which CANNOT acquire a digest eventually is not. So a part
// that decoded to zero bytes matched the digest arm on every pass forever.
//
// ⚠ AND THE COST WAS NOT WASTED WORK. MessagesDueForRescan is
// `ORDER BY m.id LIMIT 50` over ingest-ordered ULIDs, so fifty permanently-due
// messages at the head of the queue starve every message behind them — including
// the `<= 7 days` tier, which is the entire value of re-scanning — while the job
// logs a full chunk every thirty seconds.
//
// Each assertion below is preceded by the control that says the fixture could
// have failed, because "not due" is the answer a broken fixture also gives.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// rescanFixture is one user, one message, and a helper to ask what is due.
type rescanFixture struct {
	h     *harness
	ctx   context.Context
	s     *Store
	user  fixtureUser
	msgID string
}

func newRescanFixture(t *testing.T) *rescanFixture {
	t.Helper()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	f := &rescanFixture{h: h, ctx: context.Background(), s: h.appStore(), user: a, msgID: a.MessageID}

	// ⚠ THE SHARED FIXTURE ALREADY WROTE AN ATTACHMENT ROW, digest and all
	// (fixtures_test.go seeds part '2' with a part_hash so the RLS release
	// assertions have something unambiguous to act on). Inheriting it would make
	// every assertion here depend on a choice made for a different test — and
	// ReplaceAttachments deliberately does NOT reset scan state on conflict, so
	// a "freshly projected" row below would silently arrive pre-digested. It
	// cost one wrong expectation before this reset existed.
	//
	// So the scan state is returned to the never-scanned baseline explicitly.
	// The tests then state their own preconditions rather than borrowing them.
	if _, err := h.super.Exec(f.ctx, `
		UPDATE app.attachments
		   SET scan_status='unscanned', scanned_at=NULL, scanned_db_version=NULL,
		       scanned_engine_version=NULL, part_hash=NULL,
		       attempted_at=NULL, attempted_db_version=NULL
		 WHERE user_id=$1 AND message_id=$2`, a.ID, a.MessageID); err != nil {
		t.Fatalf("reset the fixture's scan state: %v", err)
	}
	return f
}

// due asks the predicate the job uses, through BOTH queries, and fails if they
// disagree.
//
// ⚠ THE TWO MUST AGREE OR "remaining" IS A NUMBER ABOUT A DIFFERENT POPULATION.
// The job selects a chunk with one and reports what is left with the other; a
// drift between them is how a stalled loop reports success. They are checked
// together here so no assertion below can be satisfied by only one of them.
func (f *rescanFixture) due(t *testing.T, version int) int {
	t.Helper()
	var n int
	var targets []RescanTarget
	if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		var e error
		if n, e = tx.CountMessagesDueForRescan(f.ctx, version); e != nil {
			return e
		}
		targets, e = tx.MessagesDueForRescan(f.ctx, version, 10)
		return e
	}); err != nil {
		t.Fatalf("count/select due: %v", err)
	}
	if len(targets) != n {
		t.Fatalf("the COUNT says %d message(s) are due and the SELECT returns %d — the two halves of "+
			"one predicate have drifted, so the job's 'remaining' describes a different population "+
			"than the work it does", n, len(targets))
	}
	return n
}

// project writes one attachment row with the given decoded size.
func (f *rescanFixture) project(t *testing.T, partPath string, size int64) {
	t.Helper()
	if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		return tx.ReplaceAttachments(f.ctx, mustULID(t, f.msgID), []AttachmentRow{
			{PartPath: partPath, ContentType: "application/octet-stream", SizeBytes: size},
		})
	}); err != nil {
		t.Fatalf("project: %v", err)
	}
}

// TestTheDigestArmNeverSelectsAPartThatCannotBeDigested is #409's headline case.
//
// A zero-byte part reaches attachscan.HasNoBytes, so PartHash returns nil BY
// DESIGN — hashing nothing yields one identity every empty part in the mailbox
// would share, and a single release against it would open the gate on all of
// them. The row's part_hash therefore stays NULL on every pass, ingest and
// re-scan alike, and the digest arm deliberately bypasses the tier gate. Nothing
// could ever remove it.
//
// ⚠ THE FIXTURE MUST ISOLATE THE DIGEST ARM, AND THE FIRST VERSION OF THIS TEST
// DID NOT. A freshly projected row is due through the STALENESS arm as well —
// scanned_db_version is NULL and a minutes-old message sits in the `<= 7 days`
// tier, which has no minimum interval — so "still due" said nothing about the
// digest clause at all. The row is therefore stamped at the current version
// first, which switches staleness off and leaves the digest arm as the only
// thing that can select it.
func TestTheDigestArmNeverSelectsAPartThatCannotBeDigested(t *testing.T) {
	f := newRescanFixture(t)
	const version = 28110

	// Stamp a verdict at the CURRENT version with NO digest, directly in SQL.
	// ⚠ Deliberately not through SetScanVerdicts: that also records the attempt,
	// which would exclude the row through the predicate's leading factor and
	// make this test pass without the size clause existing at all.
	stampNoDigest := func(size int64) {
		t.Helper()
		f.project(t, "2", size)
		if _, err := f.h.super.Exec(f.ctx, `
			UPDATE app.attachments
			   SET scan_status='unknown', scanned_at=now(),
			       scanned_db_version=$3, scanned_engine_version='1.5.4',
			       part_hash=NULL, attempted_at=NULL, attempted_db_version=NULL
			 WHERE user_id=$1 AND message_id=$2 AND part_path='2'`,
			f.user.ID, f.msgID, version); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	// ── CONTROL: a part WITH bytes and no digest is due, on the digest arm. ──
	// Nothing is stale and no tier is asking, so this can only be the digest
	// arm. Without it, "not due" below could mean the whole predicate is dead.
	stampNoDigest(11)
	if got := f.due(t, version); got != 1 {
		t.Fatalf("CONTROL FAILED: a stamped row with no digest is not due (%d), so the digest arm is "+
			"not firing at all and nothing below can distinguish a working exclusion from a dead "+
			"predicate", got)
	}

	// ── The same row, with a decoded size of zero: NOT due. ─────────────────
	// Nothing else changed — same message, same age, same verdict, same absent
	// digest, same absent attempt — so only the size clause can produce this.
	stampNoDigest(0)
	if got := f.due(t, version); got != 0 {
		t.Errorf("a part that decoded to ZERO BYTES is still due through the digest arm (%d). It can "+
			"never acquire a content digest, so it is a fixed point: the job re-reads, re-decrypts "+
			"and re-parses its message every 30 seconds forever, and past 50 such rows the "+
			"ORDER BY m.id LIMIT 50 means NOTHING ELSE IS EVER RE-SCANNED while the log says 50 done",
			got)
	}

	// ── ⚠ AND THE ROW WAS NOT EXILED FROM RE-SCANNING ALTOGETHER. ────────────
	// A fix that simply dropped zero-byte parts from the predicate would pass
	// the assertion above and be wrong: the part still deserves a VERDICT, which
	// ages like any other and is re-made when the signatures move. What is
	// permanently excluded is the demand for a digest it cannot produce, not the
	// row. This is the one-sided-fix control.
	if got := f.due(t, version+1); got != 1 {
		t.Errorf("a zero-byte part is not due when the signature database moves (%d) — its verdict "+
			"can still be re-made and still ages, and excluding the ROW rather than the digest "+
			"demand would silently stop that forever", got)
	}
}

// TestAGiveUpRecordedAsAnAttemptLeavesTheRescanQueue is the other half: a row
// that COULD have a digest, on a message the pass could not process at all.
//
// ⚠ THIS IS #415'S CIRCUIT BREAKER. A file that crashes clamd, a blob that will
// not read, a message the parser now finds no parts in — each leaves the row
// exactly as it found it, so it was re-offered thirty seconds later, forever,
// on a dispatcher `submit` shares.
func TestAGiveUpRecordedAsAnAttemptLeavesTheRescanQueue(t *testing.T) {
	f := newRescanFixture(t)
	const version = 28110
	f.project(t, "2", 11)

	// ── CONTROL: due before anything is recorded. ────────────────────────────
	if got := f.due(t, version); got != 1 {
		t.Fatalf("CONTROL FAILED: the row is not due before the attempt is recorded (%d)", got)
	}

	// ── Record a give-up. No verdict, no digest — only the attempt. ──────────
	before := f.readRow(t, "2")
	if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		return tx.RecordScanAttempt(f.ctx, mustULID(t, f.msgID), version, time.Now())
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if got := f.due(t, version); got != 0 {
		t.Errorf("a row whose give-up was recorded is STILL due (%d) — the pass changed nothing that "+
			"the predicate can see, so the same message is picked first on the next pass in 30 "+
			"seconds, forever", got)
	}

	// ── ⚠ AND THE ATTEMPT DID NOT PRETEND TO BE A VERDICT. ───────────────────
	// scanned_at is what ADR-027 §1a surfaces as the age of the verdict, and it
	// is the entire justification for the `> 6 months` tier never being
	// re-scanned. If a give-up refreshed it, the operator would be told a
	// verdict is fresh when nothing was scanned — in the one surface that
	// exists to stop that being said.
	after := f.readRow(t, "2")
	if after.status != before.status {
		t.Errorf("recording an attempt changed scan_status from %q to %q — a pass that scanned nothing "+
			"must not move a verdict", before.status, after.status)
	}
	if !sameNullableTime(before.scannedAt, after.scannedAt) {
		t.Errorf("recording an attempt moved scanned_at (%v -> %v) — that column is the age of the "+
			"VERDICT (ADR-027 §1a) and refreshing it here claims freshness nothing produced",
			before.scannedAt, after.scannedAt)
	}
	if !sameNullableInt(before.scannedVersion, after.scannedVersion) {
		t.Errorf("recording an attempt moved scanned_db_version (%v -> %v) — the row would then look "+
			"scanned against signatures that never saw it", before.scannedVersion, after.scannedVersion)
	}

	// ── A NEW SIGNATURE DATABASE RE-OPENS IT, and must. ──────────────────────
	// The attempt is a memory scoped to a version, not a permanent give-up: a
	// blob that would not read today may read tomorrow, and a release is keyed
	// on the digest that is still missing. One retry per database change is the
	// bound; every 30 seconds was the defect.
	if got := f.due(t, version+1); got != 1 {
		t.Errorf("a recorded attempt survived a signature-database change (%d due, want 1) — the row "+
			"has no digest and can have one, so it is permanently blockable and unreleasable", got)
	}
}

// TestAScannedRowLeavesTheQueueWithoutWaitingForItsTier pins #407 at the
// predicate: a verdict written WITH a stamp is not immediately due again.
//
// ⚠ THE `<= 7 days` TIER HAS NO MINIMUM INTERVAL, so freshly ingested mail is
// due the instant it lands unless the write stamped a version. Before this,
// ingest never did — so every new message was re-scanned once, redundantly,
// on the sequential dispatcher that `submit` shares.
func TestAScannedRowLeavesTheQueueWithoutWaitingForItsTier(t *testing.T) {
	f := newRescanFixture(t)
	const version = 28110
	f.project(t, "2", 11)
	if got := f.due(t, version); got != 1 {
		t.Fatalf("CONTROL FAILED: a fresh unscanned row is not due (%d)", got)
	}

	hash := sha256.Sum256([]byte("part-2-bytes"))
	if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		return tx.SetScanVerdicts(f.ctx, mustULID(t, f.msgID),
			[]ScanVerdict{{PartPath: "2", Status: "clean", PartHash: hash[:]}},
			&ScanStamp{At: time.Now(), DBVersion: version, Engine: "1.5.4"})
	}); err != nil {
		t.Fatalf("set verdicts: %v", err)
	}
	if got := f.due(t, version); got != 0 {
		t.Errorf("a message scanned seconds ago is due for re-scan again (%d) — clamd looks at the same "+
			"bytes twice inside a day, on a dispatcher that outbound mail shares", got)
	}

	// ⚠ THE STAMPED PASS ALSO RECORDED THE ATTEMPT. Asserted rather than
	// assumed, because the predicate's termination rests on EVERY pass moving
	// attempted_* — a verdict write that skipped it would leave the successful
	// path as the one hole in the argument.
	row := f.readRow(t, "2")
	if row.attemptedVersion == nil || *row.attemptedVersion != version {
		t.Errorf("a verdict write left attempted_db_version = %v, want %d — the successful path is the "+
			"one that must NOT be an exception to 'every pass records the attempt'",
			row.attemptedVersion, version)
	}

	// And the ordinary staleness arm still works, so none of the above was
	// achieved by making the predicate match nothing.
	if got := f.due(t, version+1); got != 1 {
		t.Errorf("a row stamped at an older signature version is not due (%d) — the version arm has "+
			"been disabled rather than the redundancy removed", got)
	}
}

// attRow is the subset of app.attachments these assertions read.
type attRow struct {
	status           string
	scannedAt        *time.Time
	scannedVersion   *int
	attemptedAt      *time.Time
	attemptedVersion *int
}

func (f *rescanFixture) readRow(t *testing.T, partPath string) attRow {
	t.Helper()
	var r attRow
	if err := f.h.super.QueryRow(f.ctx, `
		SELECT scan_status, scanned_at, scanned_db_version, attempted_at, attempted_db_version
		  FROM app.attachments
		 WHERE user_id = $1 AND message_id = $2 AND part_path = $3`,
		f.user.ID, f.msgID, partPath).
		Scan(&r.status, &r.scannedAt, &r.scannedVersion, &r.attemptedAt, &r.attemptedVersion); err != nil {
		t.Fatalf("read attachment row: %v", err)
	}
	return r
}

func sameNullableTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func sameNullableInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestRescanBacklogReportsWhatTheJobItselfWouldDo is the audit's own test.
//
// ⚠ THE REPORT COULD NOT SEE THE STALL IT EXISTS TO SHOW. `attachment-report`
// computed "due for re-scan" from stamped versions alone and never learned about
// the digest arm, so it printed `due for re-scan: 0` while the job had work
// forever. v1.61.1 extracted a shared constant so the SELECT and the COUNT could
// not drift and MISSED THE THIRD CONSUMER. This asserts the report's number is
// the job's number, and that the two no-digest populations stay apart.
func TestRescanBacklogReportsWhatTheJobItselfWouldDo(t *testing.T) {
	f := newRescanFixture(t)
	const version = 28110

	// A part that can be digested and one that cannot, on the same message.
	if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		return tx.ReplaceAttachments(f.ctx, mustULID(t, f.msgID), []AttachmentRow{
			{PartPath: "2", ContentType: "application/pdf", SizeBytes: 11},
			{PartPath: "3", ContentType: "text/plain", SizeBytes: 0},
		})
	}); err != nil {
		t.Fatalf("project: %v", err)
	}

	var bl RescanBacklog
	read := func() {
		t.Helper()
		if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
			var e error
			bl, e = tx.RescanBacklogFor(f.ctx, version)
			return e
		}); err != nil {
			t.Fatalf("backlog: %v", err)
		}
	}
	read()
	if bl.DueMessages != f.due(t, version) {
		t.Errorf("the report says %d message(s) due and the job's own count says %d — the audit is "+
			"computing the backlog a second way, which is exactly how it went blind",
			bl.DueMessages, f.due(t, version))
	}
	// ⚠ TWO POPULATIONS, NEVER ONE. "no digest yet" is a backfill that has not
	// arrived; "no digest possible" is a part that is DONE and empty. One number
	// cannot tell a permanent fixed point from pending work.
	if bl.NoDigest != 2 || bl.NoDigestPossible != 1 {
		t.Errorf("noDigest/noDigestPossible = %d/%d, want 2/1 — the empty part and the not-yet-digested "+
			"part are opposite findings and must not collapse into one number",
			bl.NoDigest, bl.NoDigestPossible)
	}
	if bl.AttemptedAtCurrent != 0 {
		t.Errorf("attemptedAtCurrent = %d before any pass, want 0", bl.AttemptedAtCurrent)
	}

	if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		return tx.RecordScanAttempt(f.ctx, mustULID(t, f.msgID), version, time.Now())
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	read()
	// ⚠ THE DENOMINATOR THAT SEPARATES "CAUGHT UP" FROM "STOPPED". With
	// DueMessages at 0, a zero here means nothing was ever looked at — the shape
	// of a re-scan chain that has quietly stopped rescheduling.
	if bl.AttemptedAtCurrent != 2 {
		t.Errorf("attemptedAtCurrent = %d after a pass over both parts, want 2 — without it, "+
			"'0 due' cannot be told apart from 'the job is not running'", bl.AttemptedAtCurrent)
	}
	if bl.DueMessages != 0 {
		t.Errorf("DueMessages = %d after every part was visited, want 0", bl.DueMessages)
	}
}

// TestAnUnknownVerdictIsRetriedEvenInTheStopTier is #412's second item.
//
// ⚠ THE `> 6 months` TIER'S ARGUMENT IS ABOUT `clean`, AND IT WAS APPLIED TO
// BOTH. §5c stops re-scanning old mail because a verdict AGES and the detection
// gap is short. `unknown` is not an ageing answer — it means nothing looked —
// and §5b makes it BLOCK. On mail over six months old those two combined into a
// permanent quarantine on ordinary mail: a password-protected PDF, or a payslip
// that hit a transient scan failure during a first sync, was never re-judged for
// the life of the mailbox.
func TestAnUnknownVerdictIsRetriedEvenInTheStopTier(t *testing.T) {
	f := newRescanFixture(t)
	const version = 28110
	f.project(t, "2", 11)

	// Age the message past every re-scan tier, and stamp it at the CURRENT
	// version with a digest so neither the staleness arm nor the digest arm can
	// select it. Only the verdict itself is left.
	if _, err := f.h.super.Exec(f.ctx,
		`UPDATE app.messages SET received_at = now() - interval '400 days' WHERE id = $1`,
		f.msgID); err != nil {
		t.Fatalf("age the message: %v", err)
	}
	hash := sha256.Sum256([]byte("part-2-bytes"))
	stamp := func(status string) {
		t.Helper()
		if _, err := f.h.super.Exec(f.ctx, `
			UPDATE app.attachments
			   SET scan_status=$4, scanned_at=now(), scanned_db_version=$3,
			       scanned_engine_version='1.5.4', part_hash=$5,
			       attempted_at=NULL, attempted_db_version=NULL
			 WHERE user_id=$1 AND message_id=$2 AND part_path='2'`,
			f.user.ID, f.msgID, version, status, hash[:]); err != nil {
			t.Fatalf("stamp %s: %v", status, err)
		}
	}

	// ── CONTROL: a `clean` verdict in the STOP tier is NOT due. ─────────────
	// This is the tier doing its job, and without it "due" below could be any
	// of the other two arms firing.
	stamp("clean")
	if got := f.due(t, version); got != 0 {
		t.Fatalf("CONTROL FAILED: a 400-day-old CLEAN attachment, stamped at the current version "+
			"with a digest, is due (%d) — the STOP tier is not holding, so the assertion below "+
			"cannot show anything about the verdict", got)
	}

	// ── The same row, same age, same stamp — but `unknown`. ─────────────────
	stamp("unknown")
	if got := f.due(t, version); got != 1 {
		t.Errorf("a 400-day-old UNKNOWN attachment is not due (%d). `unknown` means NOTHING LOOKED, "+
			"and §5b makes it block exactly as `flagged` does — so on mail past the STOP tier it "+
			"is a PERMANENT quarantine on ordinary mail: an encrypted archive, a password-protected "+
			"payslip, or a transient scan failure during a first sync, withheld for the life of "+
			"the mailbox with no automatic recovery", got)
	}

	// ⚠ AND IT IS STILL BOUNDED. The attempt gate applies, so a part that can
	// never resolve is re-tried once per signature-database change rather than
	// continuously — the difference between a bounded cost and the loop #409
	// was about.
	if err := f.s.UserTx(f.ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		return tx.RecordScanAttempt(f.ctx, mustULID(t, f.msgID), version, time.Now())
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if got := f.due(t, version); got != 0 {
		t.Errorf("an unknown row is still due after a pass visited it at this version (%d) — the "+
			"new arm has escaped the attempt gate and is a fixed point", got)
	}
}

// TestAZeroByteUnknownIsNotRetriedByTheUnresolvedArm pins the interaction
// between #412's new arm and #409's fixed point.
//
// ⚠ THIS IS THE ASSERTION #409'S OWN TEST PRODUCED WHEN #412 WAS WRITTEN. The
// unresolved-verdict arm was first written as a bare `scan_status = 'unknown'`,
// and TestTheDigestArmNeverSelectsAPartThatCannotBeDigested went red — because
// attachscan records a zero-byte part as `unknown`, so every empty
// `ATT00001.txt` in an old mailbox came back into the queue once per signature
// update, forever. Not the loop #409 fixed (the attempt gate still bounds it),
// but the waste that gate was added to stop.
//
// The two clauses ask ONE question — could a later pass change this answer — so
// they carry the same `size_bytes > 0`. Written as its own test because the
// interaction is between two arms added by two different issues, and neither
// issue's test names it.
func TestAZeroByteUnknownIsNotRetriedByTheUnresolvedArm(t *testing.T) {
	f := newRescanFixture(t)
	const version = 28110

	// A part with BYTES and an `unknown` verdict, stamped current with a digest,
	// on a message too old for any tier. Only the unresolved arm can select it.
	set := func(size int64) {
		t.Helper()
		f.project(t, "2", size)
		hash := sha256.Sum256([]byte("part-2-bytes"))
		if _, err := f.h.super.Exec(f.ctx, `
			UPDATE app.messages SET received_at = now() - interval '400 days' WHERE id = $1`,
			f.msgID); err != nil {
			t.Fatalf("age: %v", err)
		}
		if _, err := f.h.super.Exec(f.ctx, `
			UPDATE app.attachments
			   SET scan_status='unknown', scanned_at=now(), scanned_db_version=$3,
			       scanned_engine_version='1.5.4', part_hash=$4,
			       attempted_at=NULL, attempted_db_version=NULL
			 WHERE user_id=$1 AND message_id=$2 AND part_path='2'`,
			f.user.ID, f.msgID, version, hash[:]); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	// ── CONTROL: an unknown part WITH bytes is due, past every tier. ────────
	// That is #412's fix. Without it, "not due" below would also be what a
	// missing unresolved arm looks like.
	set(11)
	if got := f.due(t, version); got != 1 {
		t.Fatalf("CONTROL FAILED: an unknown part with bytes is not due past the STOP tier (%d), so "+
			"the assertion below cannot distinguish the size clause from a missing arm", got)
	}

	// ── The same row with NO bytes: not due. ────────────────────────────────
	set(0)
	if got := f.due(t, version); got != 0 {
		t.Errorf("a ZERO-BYTE unknown part is due (%d). attachscan records an empty part as "+
			"`unknown`, so a bare status predicate drags every empty ATT00001.txt back into the "+
			"queue once per signature update forever — and no future signature database can "+
			"resolve a part with nothing in it", got)
	}
}

// TestAppendChangesIsIndistinguishableFromTheOneAtATimePath pins #417's fix.
//
// ⚠ THE POINT IS NOT SPEED, IT IS A LOCK. Every AppendChange takes the
// per-(user, type) `change_seq` row lock and holds it until commit, so a loop of
// N calls serialises every other Email writer for that user across all of them.
// A quarantine flag propagates to every copy of the same content, and on a real
// mailbox a newsletter logo sits on ten thousand messages — inside the ingest
// transaction.
//
// ⚠ WHAT MUST NOT CHANGE IS WHAT A CLIENT SEES. The sequences have to stay
// strictly increasing and contiguous per (user, type), because a `/changes`
// cursor is exactly that number. A bulk path that allocated them differently
// would be a sync bug that no test of the flag itself would catch.
func TestAppendChangesIsIndistinguishableFromTheOneAtATimePath(t *testing.T) {
	f := newRescanFixture(t)
	aID := mustULID(t, f.user.ID)

	before := func() int64 {
		t.Helper()
		var seq int64
		if err := f.s.UserTx(f.ctx, aID, func(tx *UserTx) error {
			var e error
			seq, e = tx.ChangeState(f.ctx, ObjectEmail)
			return e
		}); err != nil {
			t.Fatalf("change state: %v", err)
		}
		return seq
	}

	start := before()
	ids := []ulid.ULID{NewULID(), NewULID(), NewULID()}
	if err := f.s.UserTx(f.ctx, aID, func(tx *UserTx) error {
		return tx.AppendChanges(f.ctx, ObjectEmail, ids, OpUpdated)
	}); err != nil {
		t.Fatalf("append changes: %v", err)
	}

	// ── The state advanced by exactly the number of objects. ────────────────
	if got := before(); got != start+int64(len(ids)) {
		t.Errorf("change state went %d -> %d for %d objects, want +%d — a client's /changes cursor "+
			"is this number, so an allocation that skips or reuses sequences is a sync defect",
			start, got, len(ids), len(ids))
	}

	// ── Every id got its own row, with a distinct, contiguous sequence. ─────
	rows, err := f.h.super.Query(f.ctx, `
		SELECT object_id, seq FROM app.change_log
		 WHERE user_id = $1 AND object_type = $2 AND seq > $3
		 ORDER BY seq`, f.user.ID, ObjectEmail, start)
	if err != nil {
		t.Fatalf("read change rows: %v", err)
	}
	defer rows.Close()
	var gotIDs []string
	var seqs []int64
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotIDs = append(gotIDs, id)
		seqs = append(seqs, seq)
	}
	if len(gotIDs) != len(ids) {
		t.Fatalf("%d change rows for %d objects, want one each — a bulk insert that dropped rows "+
			"would leave those messages invisible to every client's next sync", len(gotIDs), len(ids))
	}
	for i, id := range ids {
		if gotIDs[i] != id.String() {
			t.Errorf("row %d names %s, want %s — the sequence block and the id list are not aligned, "+
				"so clients would be told the wrong messages changed", i, gotIDs[i], id)
		}
		if want := start + int64(i) + 1; seqs[i] != want {
			t.Errorf("row %d has seq %d, want %d — the block is not contiguous", i, seqs[i], want)
		}
	}
}
