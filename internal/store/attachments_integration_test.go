//go:build integration

// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// projectedRows reads the projection back for one message, ordered by part path.
func projectedRows(t *testing.T, ctx context.Context, h *harness, userID string, msgID ulid.ULID) []AttachmentRow {
	t.Helper()
	rows, err := h.super.Query(ctx, `
		SELECT part_path, coalesce(filename, ''), content_type, size_bytes
		  FROM app.attachments
		 WHERE user_id = $1 AND message_id = $2
		 ORDER BY part_path`, userID, msgID.String())
	if err != nil {
		t.Fatalf("read attachments: %v", err)
	}
	defer rows.Close()
	var out []AttachmentRow
	for rows.Next() {
		var r AttachmentRow
		if err := rows.Scan(&r.PartPath, &r.Filename, &r.ContentType, &r.SizeBytes); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func scanStatusOf(t *testing.T, ctx context.Context, h *harness, userID string, msgID ulid.ULID, path string) string {
	t.Helper()
	var s string
	if err := h.super.QueryRow(ctx, `
		SELECT scan_status FROM app.attachments
		 WHERE user_id = $1 AND message_id = $2 AND part_path = $3`,
		userID, msgID.String(), path).Scan(&s); err != nil {
		t.Fatalf("read scan_status: %v", err)
	}
	return s
}

// ReplaceAttachments has to converge, not accumulate: ingest can re-run over
// the same message (a redelivery, a repaired sync) and the backfill runs over
// mail that may already be projected.
func TestReplaceAttachmentsIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	rows := []AttachmentRow{
		{PartPath: "2", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: 11},
		{PartPath: "3", Filename: "b.png", ContentType: "image/png", SizeBytes: 22},
	}
	for i := 0; i < 3; i++ {
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			return tx.ReplaceAttachments(ctx, msgID, rows)
		}); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	got := projectedRows(t, ctx, h, a.ID, msgID)
	if len(got) != 2 {
		t.Fatalf("after 3 identical passes: %d rows, want 2 — the upsert is accumulating", len(got))
	}
	if got[0].PartPath != "2" || got[0].Filename != "a.pdf" || got[0].SizeBytes != 11 {
		t.Errorf("row 0 = %+v", got[0])
	}
}

// ⚠ THE ONE THAT MATTERS MOST. A re-ingest must never reset a scan verdict to
// 'unscanned'. Re-scanning is expensive and, worse, a silently-reset verdict
// reads as "not yet checked" rather than as "we lost the answer" — so a
// quarantined attachment would quietly become downloadable again.
func TestReplaceAttachmentsDoesNotResetAScanVerdict(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	rows := []AttachmentRow{{PartPath: "2", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: 11}}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, rows)
	}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := scanStatusOf(t, ctx, h, a.ID, msgID, "2"); got != "unscanned" {
		t.Fatalf("initial scan_status = %q, want %q", got, "unscanned")
	}

	// A scanner records a verdict.
	if _, err := h.super.Exec(ctx, `
		UPDATE app.attachments SET scan_status = 'flagged'
		 WHERE user_id = $1 AND message_id = $2 AND part_path = '2'`,
		a.ID, msgID.String()); err != nil {
		t.Fatalf("set verdict: %v", err)
	}

	// Re-ingest the same message. The descriptive columns may be rewritten;
	// the verdict must not be.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, rows)
	}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := scanStatusOf(t, ctx, h, a.ID, msgID, "2"); got != "flagged" {
		t.Errorf("scan_status = %q after re-ingest, want %q — a verdict was silently discarded", got, "flagged")
	}
}

// ⚠ Signing rewrites a message's bytes at Submit (ADR-001's one exception) and
// ADDS A PART, so the part list legitimately changes for locally-authored mail.
// Rows for parts that no longer exist must go, or the projection describes a
// message that does not exist — #377 in a new table.
func TestReplaceAttachmentsPrunesPartsThatAreGone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	before := []AttachmentRow{
		{PartPath: "2", ContentType: "application/pdf", SizeBytes: 10},
		{PartPath: "3", ContentType: "image/png", SizeBytes: 20},
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, before)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The rewrite drops part 3 and adds a signature at 4.
	after := []AttachmentRow{
		{PartPath: "2", ContentType: "application/pdf", SizeBytes: 10},
		{PartPath: "4", ContentType: "application/pgp-signature", SizeBytes: 5},
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, after)
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got := projectedRows(t, ctx, h, a.ID, msgID)
	if len(got) != 2 {
		t.Fatalf("%d rows, want 2", len(got))
	}
	if got[0].PartPath != "2" || got[1].PartPath != "4" {
		t.Errorf("paths = %q,%q — want 2,4; a stale row survived the rewrite",
			got[0].PartPath, got[1].PartPath)
	}
}

// An empty list means "this message has no attachment parts", which after a
// rewrite is a real state. It must clear the projection rather than be treated
// as "nothing to do".
func TestReplaceAttachmentsWithNoRowsClearsTheProjection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, []AttachmentRow{
			{PartPath: "2", ContentType: "application/pdf", SizeBytes: 10},
		})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, nil)
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := projectedRows(t, ctx, h, a.ID, msgID); len(got) != 0 {
		t.Errorf("%d rows survived a clear, want 0", len(got))
	}
}

// The backfill's candidate query must find messages whose flag is set and whose
// rows are missing, and must STOP finding them once projected — otherwise the
// backfill loops forever or reports work it already did.
func TestMessagesMissingAttachmentRowsTracksCoverage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	// ⚠ THE FIXTURE ALREADY PROJECTS THIS MESSAGE. seedTwoUsers inserts an
	// app.attachments row (fixtures_test.go), which is exactly what the query
	// is supposed to exclude — so without this delete the test asserts that a
	// COVERED message is missing, and fails against correct code. It cost a
	// debugging pass to notice, which is why it is stated rather than just
	// fixed.
	if _, err := h.super.Exec(ctx,
		`DELETE FROM app.attachments WHERE user_id = $1 AND message_id = $2`,
		a.ID, msgID.String()); err != nil {
		t.Fatalf("clear fixture rows: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`UPDATE app.messages SET has_attachments = true WHERE user_id = $1 AND id = $2`,
		a.ID, msgID.String()); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	var missing []ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		missing, e = tx.MessagesMissingAttachmentRows(ctx, ulid.ULID{}, 100)
		return e
	}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(missing) != 1 || missing[0] != msgID {
		t.Fatalf("missing = %v, want exactly the flagged message %s", missing, msgID)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, []AttachmentRow{
			{PartPath: "2", ContentType: "application/pdf", SizeBytes: 10},
		})
	}); err != nil {
		t.Fatalf("project: %v", err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		missing, e = tx.MessagesMissingAttachmentRows(ctx, ulid.ULID{}, 100)
		return e
	}); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("still %d missing after projecting — the backfill would never converge", len(missing))
	}
}

// ⚠ RLS, on a table nothing has ever written. Every other user table is covered
// by the isolation suite; this one had no rows to isolate until now. A projected
// attachment names a filename and a size — it must not be visible across users.
func TestAttachmentsAreIsolatedBetweenUsers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	msgID := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, []AttachmentRow{
			{PartPath: "2", Filename: "private.pdf", ContentType: "application/pdf", SizeBytes: 10},
		})
	}); err != nil {
		t.Fatalf("project as a: %v", err)
	}

	// B must see nothing, and must not be able to prune A's rows either.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, nil)
	}); err != nil {
		t.Fatalf("b clear (should be a no-op, not an error): %v", err)
	}
	if got := projectedRows(t, ctx, h, a.ID, msgID); len(got) != 1 {
		t.Errorf("A's rows = %d after B tried to clear them, want 1 — RLS did not hold", len(got))
	}
}

// ⚠ A RE-SCAN MUST NOT REVOKE A HUMAN'S RELEASE (#394, #396, ADR-027 §5, §8).
//
// §7c's stability table says a `malicious` verdict essentially never reverts, so
// a re-scan WILL re-flag an attachment somebody already looked at and released —
// resurrecting the quarantine on a cadence and turning one false positive into a
// recurring fight with the software.
//
// ⚠ WHAT THIS TEST ASSERTS CHANGED IN #396, AND SO DID THE MECHANISM. §5
// modelled `released` as a value of `scan_status`, protected by an
// `IS DISTINCT FROM 'released'` clause every writer had to remember; the old
// version of this test asserted the column still read `released` afterwards.
// Migration 00044 moved the release to app.attachment_releases, keyed on the
// part's content digest, which makes the protection structural: nothing a
// scanner writes can reach that table. So the verdict below DOES move back to
// `flagged` — that is correct and is the improvement — while the release, and
// therefore the open gate, survives.
//
// The second half is the negative control: a part that was never released must
// end up quarantined by the same call. Without it, "the release survived" could
// be true because nothing was writing anything.
func TestARescanDoesNotRevokeARelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	rows := []AttachmentRow{
		{PartPath: "2", Filename: "released.pdf", ContentType: "application/pdf", SizeBytes: 11},
		{PartPath: "3", Filename: "still-flagged.pdf", ContentType: "application/pdf", SizeBytes: 12},
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, rows)
	}); err != nil {
		t.Fatalf("project: %v", err)
	}

	// Both parts are flagged, each with its own content digest, and the user
	// releases only part 2.
	hash2 := sha256.Sum256([]byte("part-2-bytes"))
	hash3 := sha256.Sum256([]byte("part-3-bytes"))
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.SetScanVerdicts(ctx, msgID, []ScanVerdict{
			{PartPath: "2", Status: "flagged", PartHash: hash2[:]},
			{PartPath: "3", Status: "flagged", PartHash: hash3[:]},
		}, nil)
	}); err != nil {
		t.Fatalf("seed verdicts: %v", err)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, _, err := tx.ReleaseAttachment(ctx, msgID, "2")
		return err
	}); err != nil {
		t.Fatalf("release part 2: %v", err)
	}

	// A later re-scan re-flags BOTH — which is what a real re-scan does.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.SetScanVerdicts(ctx, msgID, []ScanVerdict{
			{PartPath: "2", Status: "flagged", Signature: "Win.Test.EICAR_HDB-1", PartHash: hash2[:]},
			{PartPath: "3", Status: "flagged", Signature: "Win.Test.EICAR_HDB-1", PartHash: hash3[:]},
		}, nil)
	}); err != nil {
		t.Fatalf("re-scan: %v", err)
	}

	var released2, released3 bool
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		st2, _, err := tx.AttachmentScanState(ctx, msgID, "2")
		if err != nil {
			return err
		}
		st3, _, err := tx.AttachmentScanState(ctx, msgID, "3")
		released2, released3 = st2.Released, st3.Released
		return err
	}); err != nil {
		t.Fatalf("read states: %v", err)
	}
	if !released2 {
		t.Errorf("the release on part 2 did not survive a re-scan — that is how one false positive " +
			"becomes a fight on every signature update")
	}
	// THE CONTROL. Same call, same transaction, a part nobody released: it must
	// still be quarantined, or the assertion above proves nothing.
	if released3 {
		t.Fatalf("part 3 reads as released and nobody released it — the release lookup is not " +
			"keyed on anything, so the assertion above is vacuous")
	}
	// And the VERDICT itself was refreshed rather than erased, which is the half
	// the old column-based model could not do.
	if got := scanStatusOf(t, ctx, h, a.ID, msgID, "2"); got != "flagged" {
		t.Errorf("the released part's verdict is %q, want flagged — the release must leave the "+
			"scanner's answer intact and keep it current", got)
	}
}

// ⚠ A CHANGE ROW ON EVERY WRITE WOULD BE A WEEKLY FULL RESYNC (#396, ADR-027 §9).
//
// The verdict is JMAP-visible from increment 5, so the load-bearing invariant
// applies — but re-scan rewrites every row of every due message on every pass,
// and §5c's tiers keep the whole corpus in an actively re-scanned band. Applied
// literally, that is hundreds of Email change rows a week for verdicts that
// never moved, forcing every client to refetch: exactly the noise §9 refuses for
// verdict age and DB version.
//
// So: a transition appends one row, and a rewrite of the same value appends
// none. Both halves are asserted here, and the first is the control for the
// second.
func TestScanVerdictChangeRowsOnlyOnTransitions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, []AttachmentRow{
			{PartPath: "2", ContentType: "application/pdf", SizeBytes: 11},
		})
	}); err != nil {
		t.Fatalf("project: %v", err)
	}
	countChanges := func() int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.change_log
			  WHERE user_id=$1 AND object_type='Email' AND object_id=$2`,
			a.ID, msgID.String()).Scan(&n); err != nil {
			t.Fatalf("count changes: %v", err)
		}
		return n
	}
	before := countChanges()

	hash := sha256.Sum256([]byte("part-2-bytes"))
	scan := func(status string) {
		t.Helper()
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			return tx.SetScanVerdicts(ctx, msgID, []ScanVerdict{
				{PartPath: "2", Status: status, PartHash: hash[:]},
			}, nil)
		}); err != nil {
			t.Fatalf("scan %s: %v", status, err)
		}
	}

	// unscanned -> clean is a transition. THE CONTROL: if this appends nothing,
	// the second assertion is measuring a mechanism that never fires.
	scan("clean")
	afterFirst := countChanges()
	if afterFirst != before+1 {
		t.Fatalf("a verdict TRANSITION appended %d change rows, want exactly 1 — clients would "+
			"never learn the attachment's state moved", afterFirst-before)
	}

	// Three more passes at the SAME verdict, as the weekly re-scan does.
	scan("clean")
	scan("clean")
	scan("clean")
	if got := countChanges(); got != afterFirst {
		t.Errorf("re-writing an UNCHANGED verdict appended %d change rows — at a real corpus's "+
			"re-scan cadence that is a full client resync every week for nothing", got-afterFirst)
	}
}

// The four states a scan can actually record all round-trip. `scan_status` has
// no CHECK constraint, so nothing but this test stands between a typo'd state
// and a column full of values no reader recognises.
func TestSetScanVerdictsRoundTripsEveryScannerState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	want := map[string]string{"2": "clean", "3": "flagged", "4": "unknown"}
	var rows []AttachmentRow
	var verdicts []ScanVerdict
	for path, status := range want {
		rows = append(rows, AttachmentRow{PartPath: path, ContentType: "application/octet-stream", SizeBytes: 1})
		verdicts = append(verdicts, ScanVerdict{PartPath: path, Status: status})
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if err := tx.ReplaceAttachments(ctx, msgID, rows); err != nil {
			return err
		}
		return tx.SetScanVerdicts(ctx, msgID, verdicts, nil)
	}); err != nil {
		t.Fatalf("project and scan: %v", err)
	}
	for path, w := range want {
		if got := scanStatusOf(t, ctx, h, a.ID, msgID, path); got != w {
			t.Errorf("part %s = %q, want %q", path, got, w)
		}
	}

	// And the aggregate behind "is my mailbox clean" (§5d) reports them PER
	// STATE. A single total cannot distinguish "0 flagged" from "0 flagged
	// because nothing has been scanned", which are opposite answers.
	var counts map[string]int
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		counts, err = tx.CountAttachmentsByScanStatus(ctx)
		return err
	}); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	for _, st := range []string{"clean", "flagged", "unknown"} {
		if counts[st] != 1 {
			t.Errorf("aggregate[%s] = %d, want 1 (full aggregate: %v)", st, counts[st], counts)
		}
	}
}

// ⚠ THE GENERATED SQL HAS TO RUN (#395, ADR-027 §5c). tierCaseSQL builds a CASE
// expression from RescanTiers at call time, so a malformed interval or a quoting
// slip is a runtime error in an operator's report rather than a compile failure.
// This is the test that makes that impossible to ship.
//
// It also pins the three properties the report depends on and a unit test
// cannot: that a tier with no mail in it is still EMITTED (a vanished row reads
// as "not applicable" rather than "zero"), that NULL-version rows are counted as
// NeverScanned rather than silently dropped, and that the oldest verdict wins.
func TestAttachmentVerdictCoverageBucketsByMailAge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// Three messages at chosen ages, each with one attachment. The fixture's
	// own message is left alone and lands in whichever tier its received_at
	// implies, so the assertions below are on DELTAS, not absolutes.
	type seeded struct {
		age  time.Duration
		part string
		ver  *int
	}
	v100, v200 := 100, 200
	rows := []seeded{
		{age: 2 * 24 * time.Hour, part: "2", ver: &v200},  // <= 7 days
		{age: 20 * 24 * time.Hour, part: "3", ver: &v100}, // <= 1 month, behind
		{age: 400 * 24 * time.Hour, part: "4", ver: nil},  // > 6 months, never scanned
	}
	for i, r := range rows {
		msgID := NewULID()
		// A slice, not the [32]byte sha256 returns: pgx has no encode plan for
		// a fixed-size array against the app.sha256 domain.
		sum := sha256.Sum256([]byte(msgID.String()))
		hash := sum[:]
		// ⚠ THE BLOB FIRST. app.messages carries a composite FK
		// (user_id, content_hash) -> app.blobs, so a message cannot exist
		// without the bytes it names — the same rule seedUser follows in
		// fixtures_test.go, and the reason this loop mirrors it rather than
		// inventing a shorter INSERT.
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1, $2, 10, 20, $3)`,
			a.ID, hash, NewULID().String()); err != nil {
			t.Fatalf("seed blob %d: %v", i, err)
		}
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.messages (id, user_id, content_hash, received_at, size_bytes, has_attachments)
			VALUES ($1, $2, $4, now() - $3::interval, 10, true)`,
			msgID.String(), a.ID, fmt.Sprintf("%d seconds", int64(r.age/time.Second)),
			hash); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.attachments (id, user_id, message_id, part_path, content_type, size_bytes,
			                             scan_status, scanned_at, scanned_db_version)
			VALUES ($1, $2, $3, $4, 'application/octet-stream', 1,
			        CASE WHEN $5::int IS NULL THEN 'unscanned' ELSE 'clean' END,
			        CASE WHEN $5::int IS NULL THEN NULL ELSE now() - ($6::int * interval '1 day') END,
			        $5)`,
			NewULID().String(), a.ID, msgID.String(), r.part, r.ver, i+1); err != nil {
			t.Fatalf("seed attachment %d: %v", i, err)
		}
	}

	var cov AttachmentVerdictCoverage
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		cov, e = tx.AttachmentVerdictCoverageFor(ctx)
		return e
	}); err != nil {
		t.Fatalf("coverage: %v", err)
	}

	// ⚠ EVERY TIER IS PRESENT, including ones with nothing in them. This is the
	// property that keeps an unvalidated boundary visible instead of letting it
	// disappear from the report on a corpus that cannot exercise it.
	//
	// ⚠ Errorf, not Fatalf: a Fatal here short-circuits every assertion below,
	// so the run that proves this test covers NeverScanned would only ever
	// report the tier count. Probing a test means reaching all of it. The map
	// lookups that follow are safe on a short list — a missing key yields the
	// zero TierCount, which is what they assert against anyway.
	if len(cov.Tiers) != len(RescanTiers) {
		t.Errorf("got %d tiers, want all %d emitted: %+v", len(cov.Tiers), len(RescanTiers), cov.Tiers)
	}
	byTier := map[string]TierCount{}
	for _, tc := range cov.Tiers {
		byTier[tc.Tier] = tc
	}
	for _, want := range []string{"<= 7 days", "<= 1 month", "> 6 months"} {
		if byTier[want].Attachments == 0 {
			t.Errorf("tier %q has 0 attachments, want the one seeded into it: %+v", want, cov.Tiers)
		}
	}

	// The NULL-version row is NeverScanned, not missing. This is the count the
	// predicate specified in ADR-027 §13 would have skipped entirely.
	if cov.NeverScanned < 1 {
		t.Errorf("NeverScanned = %d, want at least the one seeded with no stamped version", cov.NeverScanned)
	}
	if cov.StampedVersions[100] != 1 || cov.StampedVersions[200] != 1 {
		t.Errorf("StampedVersions = %v, want one row each at 100 and 200", cov.StampedVersions)
	}
	// Oldest verdict is the oldest, not the newest or the first seen.
	if cov.OldestVerdict == nil {
		t.Fatal("OldestVerdict is nil despite two scanned rows")
	}
	if age := time.Since(*cov.OldestVerdict); age < 24*time.Hour {
		t.Errorf("OldestVerdict is %v old, want the ~2-day-old one", age.Truncate(time.Hour))
	}
}

// The census is per user, like everything else under RLS. A second user's
// attachments must not appear in the first user's tier counts — otherwise the
// operator's "due for re-scan" number is the whole instance's, and the job that
// acts on it would be sized wrongly.
func TestAttachmentVerdictCoverageIsPerUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	// Give B an attachment with a distinctive stamped version.
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.attachments (id, user_id, message_id, part_path, content_type, size_bytes,
		                             scan_status, scanned_at, scanned_db_version)
		VALUES ($1, $2, $3, '9', 'application/octet-stream', 1, 'clean', now(), 999999)`,
		NewULID().String(), b.ID, b.MessageID); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	var cov AttachmentVerdictCoverage
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		var e error
		cov, e = tx.AttachmentVerdictCoverageFor(ctx)
		return e
	}); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if n, ok := cov.StampedVersions[999999]; ok {
		t.Errorf("user A's census contains %d of user B's attachments — RLS did not scope it", n)
	}

	// Positive control: B's own census DOES see it, so the assertion above is
	// about isolation rather than about the row never having been written.
	var bcov AttachmentVerdictCoverage
	if err := s.UserTx(ctx, mustULID(t, b.ID), func(tx *UserTx) error {
		var e error
		bcov, e = tx.AttachmentVerdictCoverageFor(ctx)
		return e
	}); err != nil {
		t.Fatalf("coverage b: %v", err)
	}
	if bcov.StampedVersions[999999] != 1 {
		t.Fatalf("user B cannot see their own attachment (%v) — the isolation assertion above proves nothing",
			bcov.StampedVersions)
	}
}

// TestAVerdictThatMovedUnderAConcurrentCommitStillAppendsAChangeRow reproduces
// the one residual #418 recorded against the old CTE and could not reproduce.
//
// The CTE read the previous status with a SELECT and updated the row in the same
// statement. Under READ COMMITTED both halves take the statement's snapshot, but
// only the UPDATE re-evaluates when it has to wait: if another transaction
// commits a change to the same row between the snapshot and the row lock,
// EPQ re-fetches the NEW tuple for the UPDATE while the SELECT half still
// reports the value it saw under the ORIGINAL snapshot.
//
// So the damaging sequence is: ingest commits clean→flagged; an in-flight
// re-scan writing `clean` observes old=clean, calls it a rewrite rather than a
// transition, and appends NO change row. The row is correct; the client is not.
// It keeps a "quarantined" badge on a message that is no longer flagged, until
// some later transition happens to sync it.
//
// PostgreSQL 18's `RETURNING old.scan_status` reads the tuple the UPDATE
// actually replaced — the EPQ-refetched one — which is why the CTE is gone.
//
// ⚠ THE CONTROL IS THAT SESSION B ACTUALLY BLOCKS. A test where the writer never
// waits on the lock exercises the ordinary path and would pass against the old
// CTE too, proving nothing. If the wait is never observed, this fails rather
// than reporting a pass it did not earn.
func TestAVerdictThatMovedUnderAConcurrentCommitStillAppendsAChangeRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, []AttachmentRow{
			{PartPath: "2", ContentType: "application/pdf", SizeBytes: 11},
		})
	}); err != nil {
		t.Fatalf("project: %v", err)
	}
	hash := sha256.Sum256([]byte("racing-part-bytes"))
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.SetScanVerdicts(ctx, msgID, []ScanVerdict{
			{PartPath: "2", Status: "clean", PartHash: hash[:]},
		}, nil)
	}); err != nil {
		t.Fatalf("seed clean: %v", err)
	}

	countChanges := func() int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.change_log
			  WHERE user_id=$1 AND object_type='Email' AND object_id=$2`,
			a.ID, msgID.String()).Scan(&n); err != nil {
			t.Fatalf("count changes: %v", err)
		}
		return n
	}
	before := countChanges()

	// Session A: the ingest that flags the row, committed only after B is waiting.
	txA, err := h.super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if _, err := txA.Exec(ctx,
		`UPDATE app.attachments SET scan_status='flagged'
		  WHERE user_id=$1 AND message_id=$2 AND part_path='2'`,
		a.ID, msgID.String()); err != nil {
		t.Fatalf("A flags the row: %v", err)
	}

	// Session B: the re-scan writing `clean`, which will block on A's row lock.
	done := make(chan error, 1)
	go func() {
		done <- s.UserTx(ctx, aID, func(tx *UserTx) error {
			return tx.SetScanVerdicts(ctx, msgID, []ScanVerdict{
				{PartPath: "2", Status: "clean", PartHash: hash[:]},
			}, nil)
		})
	}()

	// Wait until B is genuinely blocked on a lock rather than merely slow.
	deadline := time.Now().Add(15 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&n); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if n > 0 {
			blocked = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the writer finished WITHOUT ever waiting on A's row lock (err=%v) — "+
				"this test did not exercise the concurrent path and its verdict is worthless", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("never observed a blocked lock within 15s: the race this test exists for " +
			"was not set up, so a pass here would mean nothing")
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("concurrent verdict write: %v", err)
	}

	// The row really moved flagged→clean, so exactly one change row is owed.
	if got := countChanges(); got != before+1 {
		t.Errorf("a flagged→clean move under a concurrent commit appended %d change rows, want 1.\n"+
			"The verdict is stored correctly but no client will ever hear about it: the "+
			"quarantine badge stays until some later transition happens to sync it.",
			got-before)
	}
	if got := scanStatusOf(t, ctx, h, a.ID, msgID, "2"); got != "clean" {
		t.Errorf("stored scan_status = %q, want clean — the UPDATE itself lost the race", got)
	}
}
