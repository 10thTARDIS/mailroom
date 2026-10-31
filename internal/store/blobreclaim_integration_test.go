// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The blob-reclaim half of #300, and the guard's third referent.
//
// The audit measured the defect rather than inferring it: blobs outnumbered
// messages, with a few app.blobs rows referenced by no
// message, no attachment and no model — two of them from ReplaceMessageBytes
// (their hashes are in app.superseded_content), the rest from RetireDraft and
// DeleteAccount. Each of those paths deleted message rows and reclaimed nothing.
//
// Every test here asserts on the ROW COUNT of a specific hash, never on a total:
// a single number cannot distinguish "nothing to reclaim" from "the reclaim is
// looking in the wrong place".

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/oklog/ulid/v2"
)

// blobRowExists reports whether the app.blobs row for a hash is still there,
// read as the SUPERUSER so RLS cannot be what makes it look gone.
func blobRowExists(t *testing.T, ctx context.Context, h *harness, userID string, hash []byte) bool {
	t.Helper()
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.blobs WHERE user_id = $1 AND content_hash = $2`,
		userID, hash).Scan(&n); err != nil {
		t.Fatalf("count blob rows: %v", err)
	}
	return n > 0
}

// TestRetireDraftReclaimsItsBlob is the highest-frequency leak: every autosave
// retires a revision.
func TestRetireDraftReclaimsItsBlob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var (
		v1Hash   []byte
		v2Hash   []byte
		reclaim  [][]byte
		v1, v2ID ulid.ULID
	)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		v1, v1Hash = makeDraft(t, ctx, tx, a, drafts, "revision one")
		v2ID, v2Hash = makeDraft(t, ctx, tx, a, drafts, "revision two")
		var e error
		reclaim, e = tx.RetireDraft(ctx, v1)
		return e
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	if len(reclaim) != 1 || !bytesEqual(reclaim[0], v1Hash) {
		t.Fatalf("RetireDraft reported %d reclaimed hashes (%v), want exactly the retired "+
			"revision's — the caller deletes the FILE from this list, so a miss here leaks "+
			"the bytes as well as the row", len(reclaim), hexAll(reclaim))
	}
	if blobRowExists(t, ctx, h, a.ID, v1Hash) {
		t.Error("the retired revision's app.blobs row survived. It is not inert litter: it " +
			"carries the dek_id its ciphertext is opened with and its size_stored is summed " +
			"into the user's storage quota (#300)")
	}
	// CONTROL: the SURVIVING revision's blob is untouched. Without this a reclaim
	// that deleted every blob of the user would satisfy the assertion above.
	if !blobRowExists(t, ctx, h, a.ID, v2Hash) {
		t.Errorf("retiring revision %s took revision %s's blob with it", v1, v2ID)
	}
}

// TestRetireDraftKeepsABlobAnAttachmentShares is the guard doing its job. The
// reclaim must never be "the message went, so the bytes go".
func TestRetireDraftKeepsABlobAnAttachmentShares(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var (
		id      ulid.ULID
		hash    []byte
		reclaim [][]byte
	)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, hash = makeDraft(t, ctx, tx, a, drafts, "a draft whose bytes are also an attachment")
		return nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A forwarded .eml on ANOTHER message whose bytes are this draft's — the
	// real sharing case (data-model §2), which has no foreign key to enforce it.
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.attachments (id, user_id, message_id, part_path, size_bytes, content_hash)
		VALUES ($1, $2, $3, '3', 42, $4)`,
		NewULID().String(), a.ID, a.MessageID, hash); err != nil {
		t.Fatalf("seed sharing attachment: %v", err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		reclaim, e = tx.RetireDraft(ctx, id)
		return e
	}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(reclaim) != 0 {
		t.Errorf("RetireDraft reclaimed %v while an attachment still references those bytes — "+
			"the caller would delete the FILE and the attachment would 404 forever", hexAll(reclaim))
	}
	if !blobRowExists(t, ctx, h, a.ID, hash) {
		t.Error("the shared blob row was deleted; the attachment referencing it is now unreadable")
	}
}

// seedModelArtifact registers a model artifact THE WAY THE REGISTRY DOES:
// app.blobs row and app.ml_models row in ONE UserTx (ml/registry.go snapshot).
//
// ⚠ THAT PAIRING IS THE WHOLE POINT, AND IT IS WHY THESE TESTS WERE RESEEDED
// (#316). The registry used to write the FILE and the ml_models row and no
// app.blobs row at all, so the guard's ml_models referent — added by #300 to
// protect an app.blobs row from reclaim — was protecting a row that never
// existed. The old fixture here got its blob row from the DRAFT path instead,
// which made this test pass against a state production could not produce: the
// mechanism it claimed to cover was never reachable. Seeding through the real
// pair is what makes the coverage real.
func seedModelArtifact(t *testing.T, ctx context.Context, tx *UserTx, hash []byte, kind string) {
	t.Helper()
	if err := tx.InsertBlobMeta(ctx, BlobMeta{
		ContentHash: hash, SizePlain: 128, SizeStored: 160,
		DEKID: NewULID(), EnvelopeVer: 1,
	}); err != nil {
		t.Fatalf("seed model artifact blob row: %v", err)
	}
	if _, err := tx.InsertModelVersion(ctx, kind, hash, false); err != nil {
		t.Fatalf("seed model version: %v", err)
	}
}

// TestReclaimGuardCountsModelArtifacts is the referent NEITHER earlier copy of
// this SQL had (#300). app.ml_models.artifact_hash is a blob reference with no
// foreign key, exactly like app.attachments.content_hash — so nothing stopped a
// reclaim from destroying a live model artifact's bytes.
//
// The collision it guards against is a draft whose bytes are byte-identical to a
// stored model artifact. Improbable, and #300 explicitly refused to accept
// improbability as a guarantee; this is what turns it into one.
func TestReclaimGuardCountsModelArtifacts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	body := "bytes that are also a model artifact " + NewULID().String()
	sum := sha256.Sum256([]byte(body))

	var (
		id      ulid.ULID
		reclaim [][]byte
	)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// The model artifact FIRST, as the registry writes it — then a draft that
		// happens to have the same bytes. InsertBlobMeta is ON CONFLICT DO NOTHING
		// and identity is the content hash, so both referents share the one row,
		// which is exactly the production shape.
		seedModelArtifact(t, ctx, tx, sum[:], "labeler")
		id, _ = makeDraft(t, ctx, tx, a, drafts, body) // hash == sha256(body)
		return nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		reclaim, e = tx.RetireDraft(ctx, id)
		return e
	}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(reclaim) != 0 {
		t.Errorf("the reclaim guard ignored app.ml_models.artifact_hash and reclaimed %v — "+
			"the caller deletes that FILE, and the next model load fails on bytes that are "+
			"gone with no row anywhere recording why", hexAll(reclaim))
	}
	if !blobRowExists(t, ctx, h, a.ID, sum[:]) {
		t.Error("the model artifact's blob row was deleted by a message-shaped reclaim")
	}
}

// TestPruneModelVersionsDoesNotReclaimBytesAMessageShares is the RECIPROCAL
// hazard, open the whole time the guarded one was being fixed (#316).
//
// PruneModelVersions asked one question — "does a surviving ml_models row still
// reference this hash?" — and Registry.Prune then deleted the FILE. Nothing
// asked whether a message or an attachment shared those bytes. So a model-shaped
// reclaim could destroy a live message's bytes and leave app.blobs and
// app.messages intact: the message lists, and opening it fails.
//
// Same improbability as the direction #300 guarded, and rejected for the same
// reason. Prune now goes through the one shared guard like everything else.
func TestPruneModelVersionsDoesNotReclaimBytesAMessageShares(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	body := "a model artifact whose bytes are also live mail " + NewULID().String()
	sum := sha256.Sum256([]byte(body))

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		seedModelArtifact(t, ctx, tx, sum[:], "labeler")
		makeDraft(t, ctx, tx, a, drafts, body) // the live message sharing those bytes
		return nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// keepN=0 with the only version inactive: the model row goes, so NO ml_models
	// row survives to reference the hash. The message is the only thing standing
	// between these bytes and deletion — which is the entire test.
	var (
		deleted int
		freed   [][]byte
	)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		deleted, freed, e = tx.PruneModelVersions(ctx, "labeler", 0)
		return e
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("prune deleted %d model rows, want 1 — the fixture did not exercise the path", deleted)
	}
	if len(freed) != 0 {
		t.Errorf("prune handed back %v for file deletion while a message still references those "+
			"bytes — the caller deletes that file and the message becomes unopenable, with "+
			"app.blobs and app.messages both still claiming it is there", hexAll(freed))
	}
	if !blobRowExists(t, ctx, h, a.ID, sum[:]) {
		t.Error("prune reclaimed the blob row of bytes a live message still references")
	}
}

// TestPruneModelVersionsReclaimsAnArtifactNothingElseReferences is the positive
// control for the test above: with no message sharing the bytes, prune MUST hand
// the hash back. Without it, a guard that returned nothing unconditionally would
// satisfy the negative test and leak every pruned artifact forever.
func TestPruneModelVersionsReclaimsAnArtifactNothingElseReferences(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	sum := sha256.Sum256([]byte("a model artifact nothing else references " + NewULID().String()))

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		seedModelArtifact(t, ctx, tx, sum[:], "labeler")
		return nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var freed [][]byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		_, freed, e = tx.PruneModelVersions(ctx, "labeler", 0)
		return e
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(freed) != 1 || !bytes.Equal(freed[0], sum[:]) {
		t.Fatalf("prune freed %v, want exactly the pruned artifact %s — a prune that reclaims "+
			"nothing leaks a file per version", hexAll(freed), hex.EncodeToString(sum[:]))
	}
	if blobRowExists(t, ctx, h, a.ID, sum[:]) {
		t.Error("prune reported the row reclaimed but it is still there")
	}
}

// TestReplaceMessageBytesReclaimsTheSupersededBlob covers the path that made two
// of the four live orphans, INCLUDING the guard that makes it safe.
func TestReplaceMessageBytesReclaimsTheSupersededBlob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	t.Run("the unsigned bytes are reclaimed", func(t *testing.T) {
		var (
			id       ulid.ULID
			oldHash  []byte
			reclaim  [][]byte
			newBytes = []byte("the multipart/signed rendering " + NewULID().String())
		)
		sum := sha256.Sum256(newBytes)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			id, oldHash = makeDraft(t, ctx, tx, a, drafts, "unsigned "+NewULID().String())
			var e error
			reclaim, e = tx.ReplaceMessageBytes(ctx, id, BlobMeta{
				ContentHash: sum[:], SizePlain: int64(len(newBytes)),
				SizeStored: int64(len(newBytes)) + 32, DEKID: NewULID(), EnvelopeVer: 1,
			}, MessageBytesProjection{})
			return e
		}); err != nil {
			t.Fatalf("tx: %v", err)
		}
		if len(reclaim) != 1 || !bytesEqual(reclaim[0], oldHash) {
			t.Fatalf("ReplaceMessageBytes reported %v, want the superseded hash. This is the "+
				"line whose comment used to say the blob was \"reclaimed by GC\"; there is no "+
				"GC (#300)", hexAll(reclaim))
		}
		if blobRowExists(t, ctx, h, a.ID, oldHash) {
			t.Error("the superseded app.blobs row survived — it keeps billing the user's " +
				"storage quota for bytes nothing can read")
		}
		// CONTROL: the superseded_content TOMBSTONE must SURVIVE the reclaim. It
		// maps hash -> message so ingest can recognise a stale IMAP copy (#260)
		// and RetireSentDraftCopy can cancel an APPEND naming those bytes (#268);
		// neither ever reads the bytes. Treating it as a referent would make these
		// blobs unreclaimable forever, and deleting it would resurrect #268.
		var tombstones int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.superseded_content WHERE user_id = $1 AND content_hash = $2`,
			a.ID, oldHash).Scan(&tombstones); err != nil {
			t.Fatal(err)
		}
		if tombstones != 1 {
			t.Errorf("the superseded_content tombstone for the reclaimed hash is gone (%d rows). "+
				"That is #260 and #268 both back", tombstones)
		}
	})

	t.Run("a queued APPEND naming those bytes blocks the reclaim", func(t *testing.T) {
		// ⚠ THE LOAD-BEARING HALF. Signing runs at SUBMIT, typically seconds after
		// the draft was saved and MINUTES before the writeback pass that files the
		// Drafts copy — so there is normally a pending APPEND naming the
		// PRE-SIGNATURE hash, and an APPEND payload names its bytes by content hash
		// and nothing else. Reclaiming under the plain guard would pull the bytes
		// out from under an intent that is still going to run, turning every signed
		// send into a dead-lettered "could not file the draft".
		var (
			id       ulid.ULID
			oldHash  []byte
			reclaim  [][]byte
			newBytes = []byte("signed, with an append still queued " + NewULID().String())
		)
		sum := sha256.Sum256(newBytes)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			id, oldHash = makeDraft(t, ctx, tx, a, drafts, "unsigned, pending append "+NewULID().String())
			if e := tx.EnqueueAppendIntent(ctx, id, mustULID(t, a.AccountID), "Drafts",
				[]string{`\Draft`}, nil, oldHash, "queued@example.com"); e != nil {
				return e
			}
			var e error
			reclaim, e = tx.ReplaceMessageBytes(ctx, id, BlobMeta{
				ContentHash: sum[:], SizePlain: int64(len(newBytes)),
				SizeStored: int64(len(newBytes)) + 32, DEKID: NewULID(), EnvelopeVer: 1,
			}, MessageBytesProjection{})
			return e
		}); err != nil {
			t.Fatalf("tx: %v", err)
		}
		if len(reclaim) != 0 {
			t.Errorf("the superseded bytes were reclaimed (%v) while a PENDING append names "+
				"them. The engine reads those bytes back out of the blob store when it "+
				"applies the intent, so this is a dead-lettered writeback on every signed "+
				"send (#268/#300)", hexAll(reclaim))
		}
		if !blobRowExists(t, ctx, h, a.ID, oldHash) {
			t.Error("the app.blobs row a queued APPEND still needs was deleted")
		}
	})
}

// TestDeleteAccountReclaimsTheBlobsOfItsOnlyMessages covers the path whose
// comment deferred the work to a reaper that cannot reach it.
func TestDeleteAccountReclaimsTheBlobsOfItsOnlyMessages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var (
		hash    []byte
		reclaim [][]byte
		found   bool
	)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, hash = makeDraft(t, ctx, tx, a, drafts, "lives only in this account "+NewULID().String())
		return nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// CONTROL ON THE PRECONDITION: DueForPurge — the "background purge reaper"
	// the old comment deferred to — cannot see this message, because it only ever
	// scans trashed_at IS NOT NULL. That is why the deferral was a leak and not a
	// schedule.
	var reachable int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.messages
		  WHERE user_id = $1 AND content_hash = $2 AND trashed_at IS NOT NULL`,
		a.ID, hash).Scan(&reachable); err != nil {
		t.Fatal(err)
	}
	if reachable != 0 {
		t.Fatalf("the fixture message is trashed, so a purge sweep WOULD reach it and this " +
			"test would not be exercising the leak")
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		found, reclaim, e = tx.DeleteAccount(ctx, mustULID(t, a.AccountID))
		return e
	}); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if !found {
		t.Fatal("delete account: found=false")
	}
	if !containsHash(reclaim, hash) {
		t.Errorf("DeleteAccount reclaimed %v, which does not include the blob of a message "+
			"that lived only in the deleted account. Nothing else ever will: the comment "+
			"deferring this to \"the background purge reaper\" named a sweep that only scans "+
			"trashed rows (#300)", hexAll(reclaim))
	}
	if blobRowExists(t, ctx, h, a.ID, hash) {
		t.Error("the orphaned app.blobs row survived the account delete")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsHash(hashes [][]byte, want []byte) bool {
	for _, h := range hashes {
		if bytesEqual(h, want) {
			return true
		}
	}
	return false
}

func hexAll(hashes [][]byte) []string {
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, hex.EncodeToString(h))
	}
	return out
}

// TestBlobRowCensusSeesAnOrphanAndSaysWhichKind is the report half of #300.
//
// The reclaim fix stops NEW orphans. It does nothing about the ones already on
// disk, and — more to the point — nothing tells anyone they are there. That was
// the actual defect: orphaned blobs accumulated for
// months because no query could see them.
//
// ⚠ A RECONCILIATION THAT CANNOT SEE AN ORPHAN IS WORSE THAN NONE, because it
// reports "clean" and is believed. So this seeds a known orphan, a known
// referenced row, and a known superseded orphan, and requires the census to
// tell all three apart. The referenced row is the negative control: without it
// a census that classified EVERYTHING as unreferenced would pass.
func TestBlobRowCensusSeesAnOrphanAndSaysWhichKind(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	// 1. A referenced row: a live draft. Must NOT be counted unreferenced.
	liveBody := "a message that still exists " + NewULID().String()
	liveSum := sha256.Sum256([]byte(liveBody))
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, _ = makeDraft(t, ctx, tx, a, drafts, liveBody)
		return nil
	}); err != nil {
		t.Fatalf("seed live draft: %v", err)
	}

	// 2. A plain orphan: a blob row nothing points at. This is what the three
	//    broken delete paths used to leave behind.
	orphanSum := sha256.Sum256([]byte("orphan bytes " + NewULID().String()))
	seedBareBlob(t, ctx, h, a.ID, orphanSum[:])

	// 3. A superseded orphan: the same, but recorded in superseded_content —
	//    the signing-rewrite producer, which is how two of the four live orphans
	//    were attributed to ReplaceMessageBytes rather than guessed at.
	supSum := sha256.Sum256([]byte("superseded bytes " + NewULID().String()))
	seedBareBlob(t, ctx, h, a.ID, supSum[:])
	var anchorMsg ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		anchorMsg, _ = makeDraft(t, ctx, tx, a, drafts, "anchor "+NewULID().String())
		return nil
	}); err != nil {
		t.Fatalf("seed anchor message: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.superseded_content (user_id, content_hash, message_id)
		VALUES ($1, $2, $3)`, a.ID, supSum[:], anchorMsg.String()); err != nil {
		t.Fatalf("seed superseded_content: %v", err)
	}

	var c BlobRowCensus
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		c, e = tx.BlobRowCensus(ctx)
		return e
	}); err != nil {
		t.Fatalf("census: %v", err)
	}

	// CONTROL ON THE FIXTURE. Every assertion below is a count, so an empty
	// census would satisfy the "no false positives" half in silence.
	if c.Rows < 4 {
		t.Fatalf("census saw %d blob rows, but this test seeded at least 4 — "+
			"it is not reading app.blobs for this user", c.Rows)
	}

	if c.Unreferenced != 2 {
		t.Errorf("Unreferenced = %d, want exactly 2 (the plain orphan and the superseded one). "+
			"Too few means the report cannot see a leak; too many means it would report live "+
			"mail as reclaimable.", c.Unreferenced)
	}
	if c.UnreferencedSuperseded != 1 {
		t.Errorf("UnreferencedSuperseded = %d, want 1 — attribution per producer path is the "+
			"half that makes an orphan count actionable (#300)", c.UnreferencedSuperseded)
	}
	// THE NEGATIVE CONTROL, stated as its own assertion: the referenced row must
	// be classified as reachable, or "unreferenced" means nothing.
	if c.ByMessage < 1 {
		t.Errorf("ByMessage = %d, want at least 1 — a census that counts no row as "+
			"message-referenced would classify every live message as an orphan", c.ByMessage)
	}
	for _, h := range c.UnreferencedHashes {
		if bytes.Equal(h, liveSum[:]) {
			t.Error("the census listed a LIVE draft's blob as unreferenced — acting on this " +
				"report would delete mail the user still has")
		}
	}
}

// seedBareBlob inserts an app.blobs row that nothing references: the exact
// residue the three broken delete paths left behind.
func seedBareBlob(t *testing.T, ctx context.Context, h *harness, userID string, hash []byte) {
	t.Helper()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1, $2, 10, 42, $3)`, userID, hash, NewULID().String()); err != nil {
		t.Fatalf("seed bare blob: %v", err)
	}
}

// TestBlobRowCensusCountsACachedProxiedImage is #418 item 3.
//
// ⚠ THE CENSUS AND THE RECLAIM GUARD MUST NAME THE SAME FOUR REFERENTS. The
// guard has checked app.proxy_image_cache since #55/ADR-026; the census did not,
// so every cached-image blob was reported as "unreferenced row (leaks unless
// reclaimed)" while being correctly referenced and correctly retained.
//
// A wrong NUMBER, never a wrong delete — blob-report is read-only and the guard
// was right all along. But this is an audit surface whose whole value is that
// its statements can be trusted, and it is #300's shape a third time: a referent
// added to the guard and not to the thing that counts.
func TestBlobRowCensusCountsACachedProxiedImage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// A blob row that ONLY a cached proxied image references.
	cachedSum := sha256.Sum256([]byte("a cached remote image " + NewULID().String()))
	seedBareBlob(t, ctx, h, a.ID, cachedSum[:])
	urlSum := sha256.Sum256([]byte("https://example.com/tracker.png " + NewULID().String()))
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.proxy_image_cache
		       (user_id, url_hash, content_hash, content_type, size_bytes)
		VALUES ($1, $2, $3, 'image/png', 1234)`,
		a.ID, urlSum[:], cachedSum[:]); err != nil {
		t.Fatalf("seed proxy_image_cache: %v", err)
	}

	// The control: a blob nothing at all references. Without it, a census that
	// simply stopped counting orphans would pass the assertion below.
	orphanSum := sha256.Sum256([]byte("a genuine orphan " + NewULID().String()))
	seedBareBlob(t, ctx, h, a.ID, orphanSum[:])

	var c BlobRowCensus
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		c, e = tx.BlobRowCensus(ctx)
		return e
	}); err != nil {
		t.Fatalf("census: %v", err)
	}

	if c.ByProxyCache != 1 {
		t.Errorf("ByProxyCache = %d, want 1", c.ByProxyCache)
	}
	for _, h := range c.UnreferencedHashes {
		if bytes.Equal(h, cachedSum[:]) {
			t.Error("a blob referenced ONLY by the proxy image cache was reported as an unreferenced leak")
		}
	}
	// The control must still be seen, or this test proves nothing.
	found := false
	for _, h := range c.UnreferencedHashes {
		if bytes.Equal(h, orphanSum[:]) {
			found = true
		}
	}
	if !found {
		t.Error("the genuinely unreferenced control blob was NOT reported — the census has stopped seeing orphans")
	}
}
