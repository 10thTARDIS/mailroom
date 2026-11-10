// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package rescan

// Re-scan when opened (ADR-056), against a real database with row-level security.
//
// ⚠ EVERY "NOTHING HAPPENED" ASSERTION HERE HAS A CONTROL BESIDE IT. "No scan was
// made" is also what a fixture that never worked looks like, so each negative
// test is paired with the same request on a message that SHOULD be re-scanned.

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/clamd"
	"mailroom/internal/store"
)

// age moves the fixture message to `days` old and gives its attachment a clean
// verdict made against an old signature version.
func (f *fixture) age(t *testing.T, days int, scannedVersion int) {
	t.Helper()
	if _, err := f.super.Exec(f.ctx, `
		UPDATE app.messages SET received_at = now() - make_interval(days => $3)
		 WHERE user_id = $1 AND id = $2`, f.userID.String(), f.msgID.String(), days); err != nil {
		t.Fatalf("age message: %v", err)
	}
	if _, err := f.super.Exec(f.ctx, `
		UPDATE app.attachments SET scan_status = 'clean', scanned_db_version = $3,
		       scanned_at = now() - interval '300 days', part_hash = decode(repeat('aa',32),'hex')
		 WHERE user_id = $1 AND message_id = $2`, f.userID.String(), f.msgID.String(), scannedVersion); err != nil {
		t.Fatalf("stale attachment: %v", err)
	}
}

type events struct {
	mu sync.Mutex
	n  map[string]int
}

func (e *events) observe(ev string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n == nil {
		e.n = map[string]int{}
	}
	e.n[ev]++
}

func (e *events) get(ev string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n[ev]
}

func (f *fixture) open(sc Scanner) (*OpenRescan, *events) {
	ev := &events{}
	o := NewOpenRescan(f.st, f.blobs, sc, slog.New(slog.NewTextHandler(io.Discard, nil)), ev.observe)
	return o, ev
}

func (f *fixture) openJobs(t *testing.T, user ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(f.ctx,
		`SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = $2`,
		user.String(), store.OpenRescanJobKind).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// blockingScanner never answers until its context ends, like a hung clamd.
type blockingScanner struct{ scriptedScanner }

func (b *blockingScanner) Scan(ctx context.Context, _ []byte) (clamd.Verdict, error) {
	b.scans++
	<-ctx.Done()
	return clamd.Verdict{}, errors.Join(errors.New("clamd: scan abandoned"), ctx.Err())
}

// A stale attachment on old mail is re-scanned on download, and the fresh verdict
// is what is stored.
func TestDownloadRescansAStaleOldAttachmentSynchronously(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	f.scan.verdict = clamd.Verdict{State: clamd.Flagged, Signature: "Test-Signature"}
	o, ev := f.open(f.scan)

	o.BeforeServe(f.ctx, f.userID, f.msgID, "")

	r := f.row(t)
	if r.status != "flagged" {
		t.Errorf("scan_status = %q, want flagged: the fresh verdict was not stored", r.status)
	}
	var ver *int
	_ = f.super.QueryRow(f.ctx, `SELECT scanned_db_version FROM app.attachments WHERE user_id=$1 AND message_id=$2`,
		f.userID.String(), f.msgID.String()).Scan(&ver)
	if ver == nil || *ver != 28110 {
		t.Errorf("scanned_db_version = %v, want 28110", ver)
	}
	if f.scan.scans != 1 || ev.get(EventSync) != 1 {
		t.Errorf("scans=%d sync=%d, want 1 and 1", f.scan.scans, ev.get(EventSync))
	}
	if f.openJobs(t, f.userID) != 0 {
		t.Error("a successful synchronous re-scan also queued a job")
	}
}

// The controls: nothing is scanned for a fresh verdict, or for mail in a newer
// band, and the same request on the stale old message DOES scan.
func TestNothingIsScannedForFreshOrNewerMail(t *testing.T) {
	f := newFixture(t)
	o, ev := f.open(f.scan)

	// Newer band (two months old, in a re-scanned tier): the existing tiers own it.
	f.age(t, 60, 100)
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	o.OnOpen(f.ctx, f.userID, []ulid.ULID{f.msgID})
	if f.scan.scans != 0 || f.openJobs(t, f.userID) != 0 {
		t.Errorf("newer-band mail was touched: scans=%d jobs=%d", f.scan.scans, f.openJobs(t, f.userID))
	}

	// Old but fresh: the verdict is at the current version.
	f.age(t, 400, 28110)
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	o.OnOpen(f.ctx, f.userID, []ulid.ULID{f.msgID})
	if f.scan.scans != 0 || f.openJobs(t, f.userID) != 0 {
		t.Errorf("a fresh verdict was re-scanned: scans=%d jobs=%d", f.scan.scans, f.openJobs(t, f.userID))
	}

	// CONTROL: the same calls on a stale old message do act.
	f.age(t, 400, 100)
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	if f.scan.scans != 1 {
		t.Fatalf("CONTROL FAILED: a stale old attachment was not re-scanned (scans=%d, events=%v)", f.scan.scans, ev.n)
	}
}

// A timeout fails OPEN: the request proceeds under the existing verdict (the row
// is untouched), a job is queued, and the breaker sends the next one straight to
// the queue.
func TestTimeoutFailsOpenAndQueuesAJob(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	hung := &blockingScanner{}
	o, ev := f.open(hung)
	o.WithSyncBudget(50 * time.Millisecond)

	start := time.Now()
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("BeforeServe took %v: the budget did not bound it", d)
	}

	if r := f.row(t); r.status != "clean" {
		t.Errorf("scan_status = %q after a timeout, want the existing clean verdict untouched", r.status)
	}
	if ev.get(EventTimeout) != 1 || ev.get(EventQueued) != 1 || f.openJobs(t, f.userID) != 1 {
		t.Errorf("timeout=%d queued=%d jobs=%d, want 1 each", ev.get(EventTimeout), ev.get(EventQueued), f.openJobs(t, f.userID))
	}
	// ONE slow part is not a scanner failure: the breaker stays shut and the next
	// call tries the scanner again (it would otherwise let a single crafted
	// attachment switch the synchronous path off for every user).
	scans := hung.scans
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	if hung.scans != scans+1 || ev.get(EventBusy) != 0 {
		t.Errorf("one timeout opened the breaker: scans %d->%d busy=%d", scans, hung.scans, ev.get(EventBusy))
	}
}

// The breaker opens on THREE scanner failures, and then the synchronous path is
// skipped (queued instead).
func TestBreakerOpensAfterThreeScannerFailures(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	f.scan.scanErr = errors.New("connection refused")
	o, ev := f.open(f.scan)

	for i := 0; i < BreakerThreshold-1; i++ {
		o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	}
	if o.breakerOpen() || ev.get(EventBusy) != 0 {
		t.Fatal("the breaker opened before the threshold")
	}
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	if !o.breakerOpen() {
		t.Fatal("three scanner failures did not open the breaker")
	}
	scans := f.scan.scans
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	if f.scan.scans != scans || ev.get(EventBusy) != 1 {
		t.Errorf("an open breaker still scanned: scans %d->%d busy=%d", scans, f.scan.scans, ev.get(EventBusy))
	}
}

// A hangs, B is FOUND. B must be written flagged (and so refused by the gate),
// A keeps its stored clean, the hang is one timeout, and the breaker stays shut.
// Before the fix the first Scan error abandoned the message: B was never
// refreshed and the breaker opened for every user.
func TestAHungPartDoesNotShieldAFlaggedOne(t *testing.T) {
	f := newFixture(t)
	f.twoParts(t)
	sc := &bytesScanner{hang: "AAAA", flag: "BBBB"}
	o, ev := f.open(sc)
	o.WithSyncBudget(400 * time.Millisecond)

	o.BeforeServe(f.ctx, f.userID, f.msgID, "2") // the person asked for the hung part

	got := f.statuses(t)
	if got["3"] != "flagged" {
		t.Errorf("part 3 = %q, want flagged: the hung part shielded it", got["3"])
	}
	if got["2"] != "clean" {
		t.Errorf("part 2 = %q, want its stored clean kept", got["2"])
	}
	if ev.get(EventTimeout) != 1 || ev.get(EventSync) != 1 || ev.get(EventQueued) != 1 {
		t.Errorf("timeout=%d sync=%d queued=%d, want 1 each", ev.get(EventTimeout), ev.get(EventSync), ev.get(EventQueued))
	}
	if o.breakerOpen() || len(o.failures) != 0 {
		t.Error("one slow part counted toward the breaker")
	}
}

// MEDIUM-2: no more than GlobalEnqueueCap jobs are created in a window across all
// users. CONTROL: the same opens are all enqueued when the cap is far above.
func TestGlobalEnqueueCapAcrossMessages(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	o, ev := f.open(f.scan)
	// Spend the window with other users' jobs, then a real open must be refused.
	for i := 0; i < GlobalEnqueueCap; i++ {
		if _, ok := o.reserveEnqueue(); !ok {
			t.Fatalf("CONTROL FAILED: reservation %d refused", i+1)
		}
	}
	o.OnOpen(f.ctx, f.userID, []ulid.ULID{f.msgID})
	if ev.get(EventGlobalCap) != 1 || f.openJobs(t, f.userID) != 0 || ev.get(EventQueued) != 0 {
		t.Errorf("global_cap=%d jobs=%d queued=%d: the cap did not stop the enqueue",
			ev.get(EventGlobalCap), f.openJobs(t, f.userID), ev.get(EventQueued))
	}
	// A refusal is not lost work: it persisted nothing and started no cooldown, so
	// the same message enqueues on a later open once the window has passed.
	base := time.Now()
	o.now = func() time.Time { return base.Add(GlobalCapWindow + time.Second) }
	o.OnOpen(f.ctx, f.userID, []ulid.ULID{f.msgID})
	if ev.get(EventQueued) != 1 || f.openJobs(t, f.userID) != 1 {
		t.Errorf("after the window: queued=%d jobs=%d, want the refused message enqueued (no cooldown was started)",
			ev.get(EventQueued), f.openJobs(t, f.userID))
	}
	// CONTROL: a fresh instance (empty window) enqueues the same open.
	if _, err := f.super.Exec(f.ctx, `DELETE FROM app.jobs WHERE user_id=$1 AND kind=$2`, f.userID.String(), store.OpenRescanJobKind); err != nil {
		t.Fatal(err)
	}
	o2, ev2 := f.open(f.scan)
	o2.OnOpen(f.ctx, f.userID, []ulid.ULID{f.msgID})
	if ev2.get(EventQueued) != 1 || f.openJobs(t, f.userID) != 1 {
		t.Errorf("CONTROL FAILED: queued=%d jobs=%d", ev2.get(EventQueued), f.openJobs(t, f.userID))
	}
}

// LOW-2: a transient unknown over a part with no verdict yet (unscanned) is dropped
// too, so a hiccup never turns a served file into a blocked one.
func TestTransientUnknownNeverOverwritesAnUnscannedPart(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	if _, err := f.super.Exec(f.ctx, `UPDATE app.attachments SET scan_status='unscanned' WHERE user_id=$1 AND message_id=$2`,
		f.userID.String(), f.msgID.String()); err != nil {
		t.Fatal(err)
	}
	f.scan.verdict = clamd.Verdict{State: clamd.Unknown, Reason: "hiccup", Transient: true}
	o, ev := f.open(f.scan)
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	if r := f.row(t); r.status != "unscanned" {
		t.Errorf("scan_status = %q, want unscanned kept (a blocking unknown was written)", r.status)
	}
	if ev.get(EventInconclusive) != 1 {
		t.Errorf("inconclusive = %d, want 1", ev.get(EventInconclusive))
	}
}

func TestScannerErrorFailsOpenAndQueuesAJob(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	f.scan.scanErr = errors.New("connection refused")
	o, ev := f.open(f.scan)

	o.BeforeServe(f.ctx, f.userID, f.msgID, "")

	if r := f.row(t); r.status != "clean" {
		t.Errorf("scan_status = %q, want clean untouched", r.status)
	}
	if ev.get(EventUnavailable) != 1 || f.openJobs(t, f.userID) != 1 {
		t.Errorf("unavailable=%d jobs=%d", ev.get(EventUnavailable), f.openJobs(t, f.userID))
	}
}

// clamd reports a per-file failure as `unknown`. Writing that over a stored
// `clean` would block ordinary mail on a bad second.
func TestAFreshUnknownNeverReplacesAStoredClean(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	f.scan.verdict = clamd.Verdict{State: clamd.Unknown, Reason: "scan failed but the daemon is alive", Transient: true}
	o, ev := f.open(f.scan)

	o.BeforeServe(f.ctx, f.userID, f.msgID, "")

	if r := f.row(t); r.status != "clean" {
		t.Fatalf("scan_status = %q: an inconclusive scan downgraded a clean verdict (it would now be blocked)", r.status)
	}
	if ev.get(EventInconclusive) != 1 || f.openJobs(t, f.userID) != 1 {
		t.Errorf("inconclusive=%d jobs=%d", ev.get(EventInconclusive), f.openJobs(t, f.userID))
	}
}

// Opening a message enqueues exactly one job, and repeated opens coalesce.
func TestOpeningAMessageEnqueuesOneDedupedJobAndTheJobRefreshes(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	o, ev := f.open(f.scan)

	for i := 0; i < 5; i++ {
		o.OnOpen(f.ctx, f.userID, []ulid.ULID{f.msgID})
	}
	if n := f.openJobs(t, f.userID); n != 1 {
		t.Fatalf("5 opens made %d jobs, want 1", n)
	}
	if ev.get(EventQueued) != 1 || ev.get(EventDeduped) != 4 {
		t.Errorf("queued=%d deduped=%d, want 1 and 4", ev.get(EventQueued), ev.get(EventDeduped))
	}
	var fireAt time.Time
	var payload string
	if err := f.super.QueryRow(f.ctx, `SELECT fire_at, payload::text FROM app.jobs WHERE user_id=$1 AND kind=$2`,
		f.userID.String(), store.OpenRescanJobKind).Scan(&fireAt, &payload); err != nil {
		t.Fatal(err)
	}
	if bad, known := store.PayloadKeysOutsideAllowlist(store.OpenRescanJobKind, []string{"message_id"}); len(bad) != 0 || !known {
		t.Errorf("payload keys: bad=%v known=%v", bad, known)
	}
	if time.Until(fireAt) > time.Second {
		t.Errorf("job fires in %v, want now", time.Until(fireAt))
	}

	// The job itself refreshes the verdict.
	f.scan.verdict = clamd.Verdict{State: clamd.Flagged, Signature: "X"}
	var jobID string
	_ = f.super.QueryRow(f.ctx, `SELECT id FROM app.jobs WHERE user_id=$1 AND kind=$2`, f.userID.String(), store.OpenRescanJobKind).Scan(&jobID)
	id, _ := ulid.Parse(jobID)
	job := store.Job{ID: id, UserID: &f.userID, Kind: store.OpenRescanJobKind, Payload: []byte(payload)}
	if err := o.Handle(f.ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if r := f.row(t); r.status != "flagged" || ev.get(EventJob) != 1 {
		t.Errorf("after the job: status=%q job events=%d", r.status, ev.get(EventJob))
	}
}

// THE DISPATCHER BOUND. A burst of opens across many old messages must create at
// most MaxQueuedPerMinute jobs, however many messages are opened, because the
// dispatcher is sequential and a long queue of re-scans delays `submit`.
func TestABurstOfOpensIsBoundedPerMinute(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	var ids []ulid.ULID
	ids = append(ids, f.msgID)
	for i := 0; i < MaxMessagesPerOpen-1; i++ {
		id := store.NewULID()
		// A distinct blob per message: (user_id, content_hash) is unique.
		sum := sha256.Sum256([]byte(id.String()))
		if _, err := f.super.Exec(f.ctx, `
			INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1::app.ulid, $2, 100, 100, $3::app.ulid)`,
			f.userID.String(), sum[:], store.NewULID().String()); err != nil {
			t.Fatalf("seed extra blob: %v", err)
		}
		if _, err := f.super.Exec(f.ctx, `
			INSERT INTO app.messages (id, user_id, content_hash, received_at, size_bytes, has_attachments)
			VALUES ($1::app.ulid, $2::app.ulid, $3, now() - interval '400 days', 100, true)`,
			id.String(), f.userID.String(), sum[:]); err != nil {
			t.Fatalf("seed extra message: %v", err)
		}
		if err := f.st.UserTx(f.ctx, f.userID, func(tx *store.UserTx) error {
			return tx.ReplaceAttachments(f.ctx, id, []store.AttachmentRow{{PartPath: "2", ContentType: "application/octet-stream", SizeBytes: 7}})
		}); err != nil {
			t.Fatalf("project: %v", err)
		}
		ids = append(ids, id)
	}
	o, ev := f.open(f.scan)

	// CONTROL: every one of them is stale and old, so without the bound each would queue.
	counts := map[ulid.ULID]store.OpenRescanCounts{}
	_ = f.st.UserTx(f.ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		counts, e = tx.OpenRescanCountsFor(f.ctx, ids, 28110)
		return e
	})
	stale := 0
	for _, c := range counts {
		if c.Stale > 0 {
			stale++
		}
	}
	if stale != len(ids) {
		t.Fatalf("CONTROL FAILED: %d of %d messages are stale and old", stale, len(ids))
	}

	o.OnOpen(f.ctx, f.userID, ids)
	if n := f.openJobs(t, f.userID); n != MaxQueuedPerMinute {
		t.Errorf("a burst of %d opens made %d jobs, want exactly the per-minute cap %d", len(ids), n, MaxQueuedPerMinute)
	}
	if ev.get(EventThrottled) != len(ids)-MaxQueuedPerMinute {
		t.Errorf("throttled = %d, want %d", ev.get(EventThrottled), len(ids)-MaxQueuedPerMinute)
	}
	// One body fetch never inspects more than MaxMessagesPerOpen.
	if len(ids) > MaxMessagesPerOpen {
		t.Errorf("fixture exceeds the per-open cap")
	}
}

// Ownership: another user's open can neither scan nor queue anything for a
// message that is not theirs, and a forged job naming it does nothing.
func TestAnotherUsersOpenReachesNothing(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	other := store.NewULID()
	if _, err := f.super.Exec(f.ctx,
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1::app.ulid, $2, $3)`,
		other.String(), "sub-"+other.String(), "other-"+other.String()+"@example.com"); err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	t.Cleanup(func() { _, _ = f.super.Exec(context.Background(), `DELETE FROM app.users WHERE id=$1`, other.String()) })
	o, _ := f.open(f.scan)

	// CONTROL: the owner's call scans.
	o.BeforeServe(f.ctx, f.userID, f.msgID, "")
	if f.scan.scans != 1 {
		t.Fatalf("CONTROL FAILED: the owner's open did not scan")
	}
	f.age(t, 400, 100)
	scans := f.scan.scans

	o.BeforeServe(f.ctx, other, f.msgID, "")
	o.OnOpen(f.ctx, other, []ulid.ULID{f.msgID})
	forged := store.Job{ID: store.NewULID(), UserID: &other, Kind: store.OpenRescanJobKind,
		Payload: []byte(`{"message_id":"` + f.msgID.String() + `"}`)}
	if err := o.Handle(f.ctx, forged); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if f.scan.scans != scans {
		t.Errorf("another user's request scanned this user's attachment (%d -> %d)", scans, f.scan.scans)
	}
	if f.openJobs(t, other) != 0 {
		t.Errorf("another user queued a job for a message that is not theirs")
	}
	if r := f.row(t); r.status != "clean" {
		t.Errorf("row changed to %q", r.status)
	}
}

// seqScanner answers each Scan from a list, in order (part order).
type seqScanner struct {
	scriptedScanner
	seq []clamd.Verdict
}

func (s *seqScanner) Scan(context.Context, []byte) (clamd.Verdict, error) {
	v := s.seq[s.scans%len(s.seq)]
	s.scans++
	return v, nil
}

const rawTwoAttachments = "From: a@example.com\r\nTo: b@example.com\r\nSubject: two\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=bnd\r\n\r\n--bnd\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
	"--bnd\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"a.bin\"\r\n\r\nAAAAAAA\r\n" +
	"--bnd\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"b.bin\"\r\n\r\nBBBBBBB\r\n" +
	"--bnd--\r\n"

func (f *fixture) twoParts(t *testing.T) {
	t.Helper()
	f.blobs.raw = []byte(rawTwoAttachments)
	if err := f.st.UserTx(f.ctx, f.userID, func(tx *store.UserTx) error {
		return tx.ReplaceAttachments(f.ctx, f.msgID, []store.AttachmentRow{
			{PartPath: "2", ContentType: "application/octet-stream", SizeBytes: 7},
			{PartPath: "3", ContentType: "application/octet-stream", SizeBytes: 7},
		})
	}); err != nil {
		t.Fatalf("project two parts: %v", err)
	}
	f.age(t, 400, 100)
}

func (f *fixture) statuses(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.super.Query(f.ctx, `SELECT part_path, scan_status FROM app.attachments WHERE user_id=$1 AND message_id=$2`,
		f.userID.String(), f.msgID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			t.Fatal(err)
		}
		out[p] = s
	}
	return out
}

// A flagged part is written even when another part of the same message fails
// transiently; the failed part keeps its stored clean.
func TestAFlaggedPartIsWrittenWhenAnotherPartFailsTransiently(t *testing.T) {
	f := newFixture(t)
	f.twoParts(t)
	sc := &seqScanner{seq: []clamd.Verdict{
		{State: clamd.Flagged, Signature: "Test-Signature"},
		{State: clamd.Unknown, Reason: "scan failed but the daemon is alive", Transient: true},
	}}
	o, ev := f.open(sc)

	o.BeforeServe(f.ctx, f.userID, f.msgID, "")

	got := f.statuses(t)
	if got["2"] != "flagged" {
		t.Errorf("part 2 = %q, want flagged: an infected part was not recorded because another part failed", got["2"])
	}
	if got["3"] != "clean" {
		t.Errorf("part 3 = %q, want its stored clean kept", got["3"])
	}
	if ev.get(EventInconclusive) != 1 || f.openJobs(t, f.userID) != 1 {
		t.Errorf("inconclusive=%d jobs=%d, want 1 each", ev.get(EventInconclusive), f.openJobs(t, f.userID))
	}
	// And the gate now refuses the flagged part (a Blocked state), not the clean one.
	var blocked map[string]bool
	_ = f.st.UserTx(f.ctx, f.userID, func(tx *store.UserTx) error {
		m, e := tx.AttachmentScanStates(f.ctx, []ulid.ULID{f.msgID})
		blocked = map[string]bool{}
		for _, st := range m[f.msgID] {
			blocked[st.PartPath] = st.Status == "flagged" || st.Status == "unknown"
		}
		return e
	})
	if !blocked["2"] || blocked["3"] {
		t.Errorf("blocked = %v, want only part 2", blocked)
	}
}

// A policy unknown (no `Transient`) is a real answer and overwrites as designed.
func TestAPolicyUnknownOverwritesAsDesigned(t *testing.T) {
	f := newFixture(t)
	f.age(t, 400, 100)
	f.scan.verdict = clamd.Verdict{State: clamd.Unknown, Signature: "Heuristics.Encrypted.Zip", Reason: "clamd could not fully examine this"}
	o, _ := f.open(f.scan)

	o.BeforeServe(f.ctx, f.userID, f.msgID, "")

	if r := f.row(t); r.status != "unknown" {
		t.Errorf("scan_status = %q, want the policy unknown written", r.status)
	}
}
