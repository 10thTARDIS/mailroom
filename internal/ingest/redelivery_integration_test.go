// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// RE-DELIVERY DEDUP, end to end (#206, ADR-018).
//
// dedup_test.go proves DedupHash agrees on two byte-strings. That is not the same claim
// as "ingest stops creating a second message", and the gap between those two is where the
// bug would survive a green suite: the column could go unwritten, the lookup could go
// unqueried, or the fix could be wired into one ingest path and not the other. So these
// tests go through the real Ingestor against a real database, and assert on ROW COUNTS.
//
// The shape reproduced here is the one measured in a real mailbox: the same
// message delivered again with a different `Received:` line and nothing else changed.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// reDelivered returns the same message as rawMessage would, prefixed with a Received:
// line — i.e. what a second delivery of one message actually looks like. Different bytes,
// different content_hash, same message.
func reDelivered(subject, msgID, receivedStamp string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Received: from relay.example ([10.0.0.1]) by mx.example; %s\r\n", receivedStamp)
	b.Write(rawMessage(subject, msgID, nil))
	return []byte(b.String())
}

func TestIngestTreatsAReDeliveryAsTheSameMessage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := reDelivered("Sample Game is available now", "redeliv@example.com", "Wed, 30 Jul 2026 16:50:56 +0000")
	second := reDelivered("Sample Game is available now", "redeliv@example.com", "Wed, 30 Jul 2026 16:50:57 +0000")

	// The premise of the whole issue: these are NOT the same bytes, so ADR-001's
	// identity cannot see that they are the same message. If this ever stops being
	// true the test below is testing nothing.
	if sha256.Sum256(first) == sha256.Sum256(second) {
		t.Fatal("the two deliveries have identical bytes; this test no longer reproduces #206")
	}

	res1, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, first, f.prov(f.mailboxA, 693, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res1.Created {
		t.Fatal("the first delivery should create a message")
	}

	res2, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, second, f.prov(f.mailboxA, 694, nil))
	if err != nil {
		t.Fatal(err)
	}

	// THE ASSERTION THE OPERATOR CARES ABOUT: one message, not two.
	if res2.Created {
		t.Error("a re-delivery created a second message — #206 is not fixed")
	}
	if res2.MessageID != res1.MessageID {
		t.Errorf("re-delivery resolved to %s, want the first message %s", res2.MessageID, res1.MessageID)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 1 {
		t.Errorf("message count = %d, want 1", got)
	}

	// NOTHING IS DISCARDED. The second delivery really happened and is recorded as
	// another provenance row, so the message is still known to exist at UID 694 and
	// writeback still targets both coordinates.
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE message_id=$1`,
		res1.MessageID.String()); got != 2 {
		t.Errorf("provenance count = %d, want 2 — the second delivery must still be recorded", got)
	}

	// And exactly one Email was created, not two.
	if got := f.changeCount(t, "Email", "created"); got != 1 {
		t.Errorf("Email created changes = %d, want 1", got)
	}
}

// The FIRST check must still win. Identical bytes are certain; an equal dedup hash is an
// inference. If the order ever inverted, the cheap unique-index path would stop being
// used and the certain answer would come from the less certain question.
func TestIngestStillDedupsIdenticalBytesFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	raw := reDelivered("Same bytes twice", "samebytes@example.com", "Wed, 30 Jul 2026 16:50:56 +0000")

	res1, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 1, nil))
	if err != nil {
		t.Fatal(err)
	}
	res2, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxB, 2, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created || res2.MessageID != res1.MessageID {
		t.Error("identical bytes must still dedup to the same message")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 1 {
		t.Errorf("message count = %d, want 1", got)
	}
}

// THE NEGATIVE CONTROL, and the one that matters most: ADR-001's stated hazard. A
// mailing-list copy shares the Message-ID but has a rewritten body. It must remain its
// own message, or this feature has silently started losing mail.
func TestIngestKeepsAListCopyWithARewrittenFooterSeparate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	direct := reDelivered("Weekly digest", "listcopy@example.com", "Wed, 30 Jul 2026 16:50:56 +0000")
	listCopy := append([]byte(nil), direct...)
	listCopy = append(listCopy, []byte("\r\n--\r\nYou are subscribed to example-list.\r\n")...)

	res1, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, direct, f.prov(f.mailboxA, 10, nil))
	if err != nil {
		t.Fatal(err)
	}
	res2, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, listCopy, f.prov(f.mailboxA, 11, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Created {
		t.Error("a list copy with a rewritten footer was merged away — ADR-001's hazard has returned")
	}
	if res2.MessageID == res1.MessageID {
		t.Error("the two copies resolved to one message id")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 2 {
		t.Errorf("message count = %d, want 2 — the copies differ in body and must stay separate", got)
	}
}

// A genuinely different message from the same sender with the same subject must not
// collapse either — the Message-ID differs, which is enough.
func TestIngestKeepsDistinctMessagesSeparate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := reDelivered("Your receipt", "one@example.com", "Wed, 30 Jul 2026 16:50:56 +0000")
	b := reDelivered("Your receipt", "two@example.com", "Wed, 30 Jul 2026 16:50:56 +0000")

	if _, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, a, f.prov(f.mailboxA, 20, nil)); err != nil {
		t.Fatal(err)
	}
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, b, f.prov(f.mailboxA, 21, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Error("two different messages collapsed into one")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 2 {
		t.Errorf("message count = %d, want 2", got)
	}
}

// The column has to actually be WRITTEN, or the lookup above is querying a table of
// NULLs and the whole mechanism silently does nothing for the next delivery.
func TestIngestRecordsTheDedupHash(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	raw := reDelivered("Recorded", "recorded@example.com", "Wed, 30 Jul 2026 16:50:56 +0000")

	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 30, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND dedup_hash IS NOT NULL`,
		res.MessageID.String()); got != 1 {
		t.Fatal("dedup_hash was not written on insert")
	}
	// And it is the value DedupHash computes, not some other 32 bytes.
	want := DedupHash(raw)
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND dedup_hash=$2`,
		res.MessageID.String(), want[:]); got != 1 {
		t.Error("the stored dedup_hash is not ingest.DedupHash of the raw bytes")
	}
}
