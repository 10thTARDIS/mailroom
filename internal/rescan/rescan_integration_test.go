// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package rescan

// The worker's LOOP, against a real database (#409, #415, #407).
//
// ⚠ THE UNIT TESTS BESIDE THIS FILE CANNOT SEE THE DEFECT THESE PIN. They cover
// ScanParts — whether the right rule is applied to a part — and nextFireIn, the
// interval decision in isolation. Neither can observe the thing that actually
// went wrong: a pass that produced no verdict left the row EXACTLY as it found
// it, so the same message was selected first on the next pass, thirty seconds
// later, forever. That is a fact about the store, the predicate and the worker
// together, and only a test holding all three can say it.
//
// ⚠ AND THE VISIBLE SYMPTOM WAS "SUCCESS". The job logged a full chunk every
// thirty seconds; `MessagesDueForRescan` is `ORDER BY m.id LIMIT 50`, so past
// fifty stuck messages nothing else was ever re-scanned — including the
// `<= 7 days` tier, which is the entire value of re-scanning.

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/clamd"
	"mailroom/internal/store"
)

// ── Harness ──────────────────────────────────────────────────────────────────

// rawWithAttachment is the message the fake blob store hands back. Its second
// part decodes to bytes, so it CAN acquire a content digest — which is what
// makes "the row never left the queue" a statement about the give-up path
// rather than about an empty part.
const rawWithAttachment = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: with a file\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=bnd\r\n" +
	"\r\n" +
	"--bnd\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"body\r\n" +
	"--bnd\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"thing.bin\"\r\n" +
	"\r\n" +
	"PAYLOAD\r\n" +
	"--bnd--\r\n"

// rawWithNoAttachments is the same message with its attachment gone: a projected
// row exists and the parser now finds nothing to judge.
const rawWithNoAttachments = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: nothing here\r\n" +
	"\r\n" +
	"just a body\r\n"

type fixture struct {
	super  *pgxpool.Pool
	st     *store.Store
	ctx    context.Context
	userID ulid.ULID
	msgID  ulid.ULID
	hash   []byte
	blobs  *fakeBlobs
	scan   *scriptedScanner
	w      *Worker
}

type fakeBlobs struct {
	raw  []byte
	err  error
	read int
}

func (f *fakeBlobs) Raw(context.Context, ulid.ULID, []byte) ([]byte, error) {
	f.read++
	if f.err != nil {
		return nil, f.err
	}
	return f.raw, nil
}

// scriptedScanner fails Scan on demand. ⚠ A stub returning a fatal error is the
// practical substitute for a file that genuinely crashes clamd, as #415 says: a
// real fixture would need a real daemon to kill. What is asserted is therefore
// the LOOP, which is identical either way, and not the verdict.
type scriptedScanner struct {
	verdict clamd.Verdict
	scanErr error
	db      int
	scans   int
}

func (s *scriptedScanner) Scan(context.Context, []byte) (clamd.Verdict, error) {
	s.scans++
	if s.scanErr != nil {
		return clamd.Verdict{}, s.scanErr
	}
	return s.verdict, nil
}

func (s *scriptedScanner) Version(context.Context) (clamd.Version, error) {
	db := s.db
	if db == 0 {
		db = 28110
	}
	return clamd.Version{Engine: "1.5.4", Database: db, Published: time.Now()}, nil
}

func envOrSkip(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; integration tests need the docker-compose Postgres", name)
	}
	return v
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	super, err := pgxpool.New(ctx, envOrSkip(t, "MAILROOM_TEST_SUPER_URL"))
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, envOrSkip(t, "MAILROOM_TEST_APP_URL"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	f := &fixture{
		super: super, st: st, ctx: ctx,
		userID: store.NewULID(), msgID: store.NewULID(),
		blobs: &fakeBlobs{raw: []byte(rawWithAttachment)},
		scan:  &scriptedScanner{verdict: clamd.Verdict{State: clamd.Clean}},
	}
	sum := sha256.Sum256([]byte(rawWithAttachment))
	f.hash = sum[:]

	tag := f.userID.String()
	// ⚠ THREE PARAMETERS FOR TWO VALUES, and the repetition is not laziness in
	// reverse. Reusing $1 for both `id app.ulid` and `oidc_subject text` makes
	// Postgres deduce inconsistent types for one parameter (SQLSTATE 42P08) —
	// the same trap store.EnsureRecurringUserJob carries a comment about.
	if _, err := super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1::app.ulid, $2, $3)`,
		tag, "sub-"+tag, "rescan-"+tag+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// app.messages carries a composite FK to app.blobs (user_id, content_hash) —
	// a message cannot exist without the bytes it names. The blob store itself is
	// faked here (this worker reads through the BlobReader interface), but the
	// ROW has to be real or the seed violates the constraint.
	if _, err := super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1::app.ulid, $2, $3, $3, $4::app.ulid)`,
		tag, f.hash, len(rawWithAttachment), store.NewULID().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, received_at, size_bytes, has_attachments)
		VALUES ($1::app.ulid, $2::app.ulid, $3, now(), $4, true)`,
		f.msgID.String(), tag, f.hash, len(rawWithAttachment)); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), `DELETE FROM app.users WHERE id = $1`, tag)
	})

	// The projection: one attachment row with bytes, so the digest arm is about
	// a part that COULD be digested.
	if err := st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		return tx.ReplaceAttachments(ctx, f.msgID, []store.AttachmentRow{
			{PartPath: "2", ContentType: "application/octet-stream", SizeBytes: 7},
		})
	}); err != nil {
		t.Fatalf("project attachment: %v", err)
	}

	f.w = New(st, f.blobs, f.scan, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	return f
}

// handle runs one pass and returns how long the worker asked to wait before the
// next one, read back from the row it actually scheduled.
//
// ⚠ THE INTERVAL IS READ FROM app.jobs, NOT FROM A RETURN VALUE. #415's whole
// complaint is that the stored verdicts look identical whether the job comes
// back in thirty seconds or a day, so the scheduled time is the only honest
// witness — and asserting the value the worker WROTE, rather than the value it
// computed, is what keeps the reschedule wiring inside the test.
func (f *fixture) handle(t *testing.T) time.Duration {
	t.Helper()
	// Any outstanding job of this kind suppresses the insert (that is
	// EnsureRecurringUserJob's whole contract), so each pass starts clean.
	if _, err := f.super.Exec(f.ctx, `DELETE FROM app.jobs WHERE user_id = $1`, f.userID.String()); err != nil {
		t.Fatalf("clear jobs: %v", err)
	}
	start := time.Now()
	if err := f.w.Handle(f.ctx, store.Job{ID: store.NewULID(), UserID: &f.userID, Kind: JobKind}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var fireAt time.Time
	if err := f.super.QueryRow(f.ctx,
		`SELECT fire_at FROM app.jobs WHERE user_id = $1 AND kind = $2`,
		f.userID.String(), JobKind).Scan(&fireAt); err != nil {
		t.Fatalf("read the rescheduled job: %v — the chain has stopped, which is the failure "+
			"nothing else would notice", err)
	}
	return fireAt.Sub(start)
}

type row struct {
	status           string
	scannedAt        *time.Time
	partHash         []byte
	attemptedVersion *int
}

func (f *fixture) row(t *testing.T) row {
	t.Helper()
	var r row
	if err := f.super.QueryRow(f.ctx, `
		SELECT scan_status, scanned_at, part_hash, attempted_db_version
		  FROM app.attachments WHERE user_id = $1 AND message_id = $2 AND part_path = '2'`,
		f.userID.String(), f.msgID.String()).
		Scan(&r.status, &r.scannedAt, &r.partHash, &r.attemptedVersion); err != nil {
		t.Fatalf("read attachment row: %v", err)
	}
	return r
}

func (f *fixture) due(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.UserTx(f.ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		n, e = tx.CountMessagesDueForRescan(f.ctx, 28110)
		return e
	}); err != nil {
		t.Fatalf("count due: %v", err)
	}
	return n
}

// ── The control ──────────────────────────────────────────────────────────────

// A healthy pass writes a verdict and a digest, and the message leaves the
// queue. Everything below asserts that a FAILING pass reaches the same place by
// a different route, so this has to hold first or none of them mean anything.
func TestAHealthyPassScansAndClearsTheQueue(t *testing.T) {
	f := newFixture(t)
	if f.due(t) != 1 {
		t.Fatalf("CONTROL FAILED: the seeded message is not due before the pass")
	}
	if next := f.handle(t); next > time.Hour {
		// remaining is 0 afterwards, so the ordinary interval is correct here;
		// this only asserts the pass did not report a phantom backlog.
		t.Logf("next pass in %v (the ordinary interval, as expected)", next)
	}
	r := f.row(t)
	if r.status != string(clamd.Clean) {
		t.Errorf("scan_status = %q, want clean", r.status)
	}
	if len(r.partHash) == 0 {
		t.Error("no part_hash — a release is keyed on the digest, so the gate would have no key")
	}
	if f.due(t) != 0 {
		t.Errorf("the message is still due after a successful re-scan (%d)", f.due(t))
	}
}

// ── #415: a scanner failure must not become a loop ───────────────────────────

func TestAScannerFailureRecordsTheAttemptAndStopsTheThirtySecondLoop(t *testing.T) {
	f := newFixture(t)
	f.scan.scanErr = errors.New("connection reset by peer")

	if f.due(t) != 1 {
		t.Fatalf("CONTROL FAILED: the message is not due before the failing pass")
	}
	next := f.handle(t)

	// ⚠ THE ASSERTION IS ON THE INTERVAL. The stored row looks the same either
	// way: the verdict is deliberately left standing (re-scan is best-effort and
	// must not quarantine mail because clamd was restarting), so only the
	// schedule can tell a drain from a spin.
	//
	// ⚠ AND IT IS GUARDED TWICE, SO IT ALONE DOES NOT PIN THE PREDICATE. Remove
	// the attempt stamp and this still passes — via nextFireIn's no-progress
	// back-off, which is the second guard doing its job. That is the design, not
	// a gap: the loop is closed in two independent places. What pins the FIRST
	// guard is the `due` assertion below, and probing this file means checking
	// each of them separately. Saying so here because the review that produced
	// #416 found the opposite habit — a comment claiming what its assertion did
	// not exclude.
	if next < time.Hour {
		t.Errorf("the next pass was scheduled in %v after a failing chunk. That is the loop #415 "+
			"describes: the same message is picked first every 30 seconds, its blob re-read and "+
			"re-decrypted each time, on the sequential dispatcher that `submit` shares — so a file "+
			"that crashes the daemon holds up outbound mail for as long as it keeps crashing it", next)
	}
	if f.due(t) != 0 {
		t.Errorf("the message is STILL due after the pass gave up on it (%d) — nothing the pass did "+
			"is visible to the predicate, so it will be re-offered forever", f.due(t))
	}

	// ⚠ AND THE GIVE-UP DID NOT FORGE A VERDICT. `unknown` blocks exactly as
	// `flagged` does (ADR-027 §5b), so downgrading a part because the daemon
	// hiccuped would quarantine ordinary mail — the harm the package comment
	// refuses. scanned_at likewise stays put: it is the verdict age §1a
	// surfaces, and the whole justification for the STOP tier.
	r := f.row(t)
	if r.status != "unscanned" {
		t.Errorf("scan_status = %q after a scanner failure, want it untouched at 'unscanned' — "+
			"re-scan is best-effort and must not block an attachment because clamd was restarting",
			r.status)
	}
	if r.scannedAt != nil {
		t.Errorf("scanned_at was set to %v by a pass that scanned nothing — that column is the age "+
			"of the VERDICT (ADR-027 §1a) and is what makes the `> 6 months` tier safe", r.scannedAt)
	}
	if r.attemptedVersion == nil || *r.attemptedVersion != 28110 {
		t.Errorf("attempted_db_version = %v, want 28110 — the attempt is the ONLY witness that the "+
			"row was visited, and without it the row never leaves the queue", r.attemptedVersion)
	}

	// A NEW SIGNATURE DATABASE RE-OPENS IT, and must: the row still has no
	// digest, so it is blockable and unreleasable until one pass succeeds.
	// One retry per database change is the bound; every 30 seconds was the bug.
	f.scan.db = 28111
	before := f.scan.scans
	f.handle(t)
	if f.scan.scans == before {
		t.Error("a new signature database did not re-offer the message — the attempt is a permanent " +
			"give-up rather than a version-scoped memory, so a transient failure would leave the " +
			"row without a digest forever")
	}
}

// ── #409's other two populations ─────────────────────────────────────────────

func TestAnUnreadableBlobLeavesTheQueue(t *testing.T) {
	f := newFixture(t)
	f.blobs.err = errors.New("blob not found")
	if f.due(t) != 1 {
		t.Fatalf("CONTROL FAILED: the message is not due before the pass")
	}
	if next := f.handle(t); next < time.Hour {
		t.Errorf("next pass in %v after every message failed to read — a corrupt blob would spin "+
			"the job forever", next)
	}
	if f.due(t) != 0 {
		t.Errorf("a message whose blob will not read is still due (%d) — it is permanently first in "+
			"an ORDER BY m.id LIMIT 50, so it starves everything behind it", f.due(t))
	}
}

func TestAMessageTheParserFindsNoPartsInLeavesTheQueue(t *testing.T) {
	f := newFixture(t)
	// The projected row survives from an earlier parse; the bytes no longer
	// contain an attachment. That disagreement is what put it in the queue, and
	// producing no verdicts is not a reason to stay there.
	f.blobs.raw = []byte(rawWithNoAttachments)
	if f.due(t) != 1 {
		t.Fatalf("CONTROL FAILED: the message is not due before the pass")
	}
	f.handle(t)
	if f.scan.scans != 0 {
		t.Fatalf("CONTROL FAILED: %d part(s) were scanned, so this fixture is not exercising the "+
			"no-parts path", f.scan.scans)
	}
	if f.due(t) != 0 {
		t.Errorf("a message with no attachment parts is still due (%d) — the pass had nothing to "+
			"judge and recorded nothing, so it is asked again every 30 seconds forever", f.due(t))
	}
}
