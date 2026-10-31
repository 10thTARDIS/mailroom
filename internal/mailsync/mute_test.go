// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"testing"

	"mailroom/internal/sync"
)

// A pass that ingests one reply to a muted conversation and one ordinary
// message reports both as ingested and ONE as muted — the number syncrunner
// subtracts from the new-mail push (spec §6). The ingestor's own verdict
// (Result.Muted) is the only input; the engine's job is to carry it into the
// summary without losing or inventing any.
func TestMutedArrivalsAreCountedSeparately(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, PermanentFlagsAllowNew: true},
				msgs:   []sync.Message{msg(1, 0, "ordinary"), msg(2, 0, "muted reply")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	ing.mutedBodies = map[string]bool{"muted reply": true}

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 0, 2, 0)
	if sum.MessagesMuted != 1 {
		t.Errorf("MessagesMuted = %d, want 1 (the reply to the muted conversation)", sum.MessagesMuted)
	}
}

// The CONDSTORE flag pass's body fallback (#523) is another place new mail
// enters: a UID it finds no live provenance for is re-fetched with a body and
// ingested, and if that turns out to be a muted reply it is counted as one.
func TestMutedArrivalFromTheFlagPassFallbackIsCounted(t *testing.T) {
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, HighestModSeq: 12},
				msgs: []sync.Message{
					msg(1, 12, "one"),
					msg(3, 12, "muted reply"), // no provenance: the fallback fetches it
				},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	ing.mutedBodies = map[string]bool{"muted reply": true}
	st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3, highestModSeq: 10}, 1)

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", sum.Errors)
	}
	if len(ing.calls) != 1 || ing.calls[0].UID != 3 || sum.MessagesIngested != 1 {
		t.Fatalf("Ingest calls = %+v, MessagesIngested = %d — want exactly UID 3, once: the fixture did not exercise the fallback",
			ing.calls, sum.MessagesIngested)
	}
	if sum.MessagesMuted != 1 {
		t.Errorf("MessagesMuted = %d, want 1 (the muted reply the fallback ingested)", sum.MessagesMuted)
	}
}

// The gap backfill (#93) is a third place new mail enters, and a muted reply a
// truncated FETCH lost and the backfill recovered is just as muted as one the
// poll brought in. Without this, a recovered muted reply would push.
func TestMutedArrivalRecoveredByTheGapBackfillIsCounted(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 3},
				msgs:   []sync.Message{msg(1, 0, "a"), msg(2, 0, "muted reply"), msg(3, 0, "c")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	ing.mutedBodies = map[string]bool{"muted reply": true}
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3}, 1, 3)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if sum.GapsBackfilled != 1 || sum.MessagesIngested != 1 {
		t.Fatalf("GapsBackfilled=%d MessagesIngested=%d, want 1/1 — the fixture did not exercise the backfill", sum.GapsBackfilled, sum.MessagesIngested)
	}
	if sum.MessagesMuted != 1 {
		t.Errorf("MessagesMuted = %d, want 1 (the recovered muted reply)", sum.MessagesMuted)
	}
}
