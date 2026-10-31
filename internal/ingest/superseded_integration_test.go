// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// Ingest must recognise bytes a message USED to hold (#260).
//
// This is the WIRING test for the fix. internal/api proves the tombstone is
// written and that the store lookup answers correctly; none of that matters if
// findExisting never asks the question. A control that is recorded but never
// consulted is this project's most expensive recurring defect, so the consulting
// gets its own test, through the real Ingest entry point.
//
// The scenario is the one observed on the instance: compose-time signing
// replaces a draft's bytes at submit, and a sync pass later reads the stale
// copy still sitting in the IMAP Drafts folder. Here the byte replacement is
// done directly through the store rather than by standing up the whole send
// path — the question under test is what INGEST does with the old bytes.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func TestIngestDoesNotResurrectSupersededBytes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	original := rawMessage("A draft that will be signed", "superseded@example.com", nil)
	replacement := rawMessage("A draft that will be signed (signed form)", "superseded@example.com", nil)

	// Premise: the two renderings are genuinely different bytes, or the fix is
	// not being exercised at all.
	origSum := sha256.Sum256(original)
	replSum := sha256.Sum256(replacement)
	if origSum == replSum {
		t.Fatal("the two renderings have identical bytes; this test no longer reproduces #260")
	}

	// 1. ⚠ THE MESSAGE IS CREATED THE WAY COMPOSE CREATES IT, and the choice of
	//    producer is the whole test.
	//
	//    An earlier version of this test created it with f.ing.Ingest, and it
	//    passed for the wrong reason: Ingest records a dedup_hash, so the stale
	//    copy below was caught by #206's body-hash check and the superseded
	//    lookup was never needed. Deleting the fix left the test GREEN — measured,
	//    not assumed.
	//
	//    CreateLocalMessage does NOT set dedup_hash (a locally composed draft has
	//    no re-delivery to detect), which is exactly why production had nothing
	//    to fall back on and grew a stray row. Using the real producer is the
	//    difference between reproducing the bug and reproducing a belief about it.
	var messageID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		id, err := tx.CreateLocalMessage(ctx, store.LocalMessage{
			ContentHash: origSum[:],
			SizePlain:   int64(len(original)),
			SizeStored:  int64(len(original)),
			DEKID:       f.dekID,
			Subject:     "A draft that will be signed",
			FromAddress: "me@example.com",
			SentAt:      time.Now().UTC(),
			SizeBytes:   int64(len(original)),
			// The Message-ID must match the bytes, or threading and the count
			// assertion below would be comparing different messages.
			MessageIDHint: "superseded@example.com",
			MailboxID:     f.mailboxA,
			AccountID:     f.accountID,
			Draft:         true,
		})
		messageID = id
		return err
	}); err != nil {
		t.Fatalf("create local draft: %v", err)
	}

	// CONTROL ON THE FIXTURE: no dedup_hash, or this test is back to passing for
	// the wrong reason.
	if got := f.scalar(t,
		`SELECT count(*) FROM app.messages WHERE user_id=$1 AND id=$2 AND dedup_hash IS NULL`,
		f.userID.String(), messageID.String()); got != 1 {
		t.Fatal("the composed draft has a dedup_hash — #206's check would catch the stale " +
			"copy and this test would not exercise the superseded lookup at all")
	}

	res := struct{ MessageID ulid.ULID }{messageID}

	// 2. Its bytes are replaced in place — what signing does at submit. This
	//    records the tombstone as a side effect, in the same transaction.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, rerr := tx.ReplaceMessageBytes(ctx, messageID, store.BlobMeta{
			ContentHash: replSum[:],
			SizePlain:   int64(len(replacement)),
			SizeStored:  int64(len(replacement)),
			DEKID:       f.dekID,
			EnvelopeVer: 1,
		}, store.MessageBytesProjection{})
		return rerr
	}); err != nil {
		t.Fatalf("replace bytes: %v", err)
	}

	// CONTROL: the message really did move to the new hash. Without this the
	// assertion below could pass because nothing happened at all.
	if got := f.scalar(t,
		`SELECT count(*) FROM app.messages WHERE user_id=$1 AND content_hash=$2`,
		f.userID.String(), replSum[:]); got != 1 {
		t.Fatalf("the message did not take the replacement hash (matches=%d)", got)
	}

	// 3. ⚠ THE STALE COPY ARRIVES. This is the sync pass reading IMAP Drafts
	//    after the bytes moved but before the expunge landed — seven seconds
	//    was the real-world margin.
	late, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, original, f.prov(f.mailboxB, 802, nil))
	if err != nil {
		t.Fatal(err)
	}

	if late.Created {
		t.Error("the superseded bytes created a SECOND message — this is #260: a stray, " +
			"unsigned copy of a message the user has already sent")
	}
	if late.MessageID != res.MessageID {
		t.Errorf("the stale copy resolved to %s, want the original message %s",
			late.MessageID, res.MessageID)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 1 {
		t.Errorf("message count = %d, want 1", got)
	}

	// NOTHING IS DISCARDED. The delivery still happened, so it is recorded as a
	// provenance row against the real message — which is what lets the expunge
	// machinery find and remove that IMAP copy later. Dropping it on the floor
	// would leave an orphan in the folder that nothing knows about.
	//
	// Exactly ONE, and the count is a fact about the fixture rather than a
	// guess: CreateLocalMessage deliberately creates no provenance row (a
	// composed draft has none until its own APPEND round-trips), so this stale
	// delivery is the first.
	if got := f.scalar(t,
		`SELECT count(*) FROM app.message_provenance WHERE message_id=$1`,
		late.MessageID.String()); got != 1 {
		t.Errorf("provenance rows = %d, want 1 — the stale delivery must still be recorded", got)
	}
}

// TestIngestStillCreatesGenuinelyNewMessages is the negative control on the
// whole mechanism. A findExisting that answered "already have it" too eagerly
// would satisfy the test above while silently DISCARDING new mail — a far worse
// outcome than the duplicate it replaces, and one that would be invisible.
func TestIngestStillCreatesGenuinelyNewMessages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := rawMessage("First", "new-one@example.com", nil)
	second := rawMessage("Second, unrelated", "new-two@example.com", nil)

	a, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, first, f.prov(f.mailboxA, 901, nil))
	if err != nil {
		t.Fatal(err)
	}
	// Supersede the first message's bytes, so the tombstone table is non-empty
	// and the lookup is genuinely being consulted during the second ingest.
	replacement := rawMessage("First (replaced)", "new-one@example.com", nil)
	replSum := sha256.Sum256(replacement)
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, rerr := tx.ReplaceMessageBytes(ctx, a.MessageID, store.BlobMeta{
			ContentHash: replSum[:], SizePlain: int64(len(replacement)),
			SizeStored: int64(len(replacement)), DEKID: f.dekID, EnvelopeVer: 1,
		}, store.MessageBytesProjection{})
		return rerr
	}); err != nil {
		t.Fatalf("replace bytes: %v", err)
	}

	b, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, second, f.prov(f.mailboxA, 902, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Created {
		t.Fatal("an unrelated new message was swallowed as a duplicate — ingest is losing mail")
	}
	if b.MessageID == a.MessageID {
		t.Error("two different messages collapsed into one row")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 2 {
		t.Errorf("message count = %d, want 2", got)
	}
}
