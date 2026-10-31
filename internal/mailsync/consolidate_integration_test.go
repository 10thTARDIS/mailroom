// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package mailsync's live proof that FOLDER CONSOLIDATION ON THE PROVIDER
// (#647, `mailroom-admin consolidate-folders`) leaves Mailroom's copy intact.
//
// Consolidation writes nothing to Mailroom's database: it moves mail between
// folders and deletes the emptied folders over IMAP, exactly as another client
// would, and relies on the ordinary sync engine to follow. A UID MOVE gives
// every moved message a NEW UID in the destination, so to the engine it is new
// mail there plus a UID gone from a folder that is itself then gone. That is
// only harmless if the engine deduplicates the re-downloaded bodies by content
// hash onto the messages it already has (#206, ADR-018) AND the folder-deletion
// and reaper paths do not soft-delete a message on the way. This is the proof,
// with the real engine, a real Dovecot and a real PostgreSQL.
//
// Run via scripts/mailsync-test.sh.
package mailsync

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/consolidate"
	"mailroom/internal/sync"
)

func TestLiveConsolidationKeepsMailroomsMessagesWhole(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond)) // reap every pass
	ctx := context.Background()

	// An IMAP user of this test's own, so the per-sender folders it creates are
	// invisible to every other test sharing the harness account.
	f.cfg.Username = "consolidate-" + strings.ToLower(ulid.Make().String())
	sess, err := sync.Dial(ctx, f.cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	f.sess = sess

	c := f.adminClient(t)
	for _, mb := range []string{"INBOX/a", "INBOX/b", "INBOX/c", "Archive/2025/01", "Old", "Trash/x"} {
		if err := c.Create(mb, nil).Wait(); err != nil {
			t.Fatalf("create %q: %v", mb, err)
		}
	}
	moved := map[string]string{} // subject -> destination
	f.appendMsg(t, "INBOX", "already-in-inbox")
	for mb, n := range map[string]int{"INBOX/a": 3, "INBOX/b": 2, "Archive/2025/01": 2} {
		dest := "INBOX"
		if strings.HasPrefix(mb, "Archive/") {
			dest = "Archive"
		}
		for i := 0; i < n; i++ {
			subj := strings.NewReplacer("/", "-").Replace(mb) + "-" + string(rune('a'+i))
			f.appendMsg(t, mb, subj)
			moved[subj] = dest
		}
	}

	first := f.syncOnce(t)
	if len(first.Errors) != 0 {
		t.Fatalf("first sync: %v", first.Errors)
	}
	before := f.corpus(t)
	t.Logf("before consolidation: %d messages, %d distinct content hashes, %d live provenance rows; first sync ingested %d, fetched %d bodies",
		before.messages, before.hashes, before.liveProvenance, first.MessagesIngested, first.BodiesFetched)
	if before.messages != 8 || before.hashes != 8 {
		t.Fatalf("precondition: want 8 messages with 8 hashes, have %+v", before)
	}
	ids := map[string]ulid.ULID{}
	for subj := range moved {
		id, ok := f.messageIDBySubject(t, subj)
		if !ok {
			t.Fatalf("precondition: %q was never ingested", subj)
		}
		ids[subj] = id
	}

	// Consolidate, exactly as the operator will: the inbox run, then the archive flatten.
	fa, err := sync.DialFolderAdmin(ctx, f.cfg)
	if err != nil {
		t.Fatalf("DialFolderAdmin: %v", err)
	}
	t.Cleanup(func() { _ = fa.Close() })
	for _, o := range []consolidate.Options{
		{From: "INBOX/*", Into: "INBOX", DeleteEmpty: []string{"INBOX/*", "Old", "Trash/*"}},
		{From: "Archive/*", Into: "Archive", DeleteEmpty: []string{"Archive/*"}},
	} {
		p, err := consolidate.BuildPlan(ctx, fa, nil, o)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		var out strings.Builder
		if _, err := consolidate.Apply(ctx, fa, p, &out); err != nil {
			t.Fatalf("apply: %v\n%s", err, out.String())
		}
		t.Logf("consolidate --from %q:\n%s", o.From, out.String())
	}

	second := f.syncOnce(t)
	if len(second.Errors) != 0 {
		t.Fatalf("second sync: %v", second.Errors)
	}
	after := f.corpus(t)
	t.Logf("after consolidation + one sync: %d messages, %d distinct content hashes, %d live provenance rows; "+
		"the sync ingested %d NEW messages, deduped %d, fetched %d bodies (%d bytes), reaped %d provenance rows, "+
		"reconciled %d deleted folders, vanished %d messages",
		after.messages, after.hashes, after.liveProvenance, second.MessagesIngested, second.FlagUpdates,
		second.BodiesFetched, second.BodyBytesFetched, second.ProvenanceReaped, second.MailboxesDeleted, second.MessagesVanished)

	if after.messages != before.messages {
		t.Errorf("message count %d -> %d: the moved mail was ingested as NEW messages", before.messages, after.messages)
	}
	if after.duplicateHashes != 0 {
		t.Errorf("%d content hashes are held by more than one message: the #206 dedup did not catch moved mail", after.duplicateHashes)
	}
	if after.vanished != 0 || after.trashed != 0 {
		t.Errorf("%d messages soft-deleted (vanished_at), %d trashed: a MOVE was read as a deletion", after.vanished, after.trashed)
	}
	if after.liveProvenance != before.liveProvenance {
		t.Errorf("live provenance %d -> %d: want one live location per message, as before", before.liveProvenance, after.liveProvenance)
	}
	inboxID, archiveID := f.mailboxID(t, "INBOX"), f.mailboxID(t, "Archive")
	for subj, dest := range moved {
		id, ok := f.messageIDBySubject(t, subj)
		if !ok || id != ids[subj] {
			t.Errorf("%q is message %v after consolidation, was %v — its identity did not survive", subj, id, ids[subj])
			continue
		}
		want := inboxID
		if dest == "Archive" {
			want = archiveID
		}
		if got := f.mailboxIDs(t, id); len(got) != 1 || got[0] != want.String() {
			t.Errorf("%q is in %v, want exactly its destination %s (%s)", subj, got, dest, want)
		}
	}
}

type corpusCounts struct {
	messages, hashes, duplicateHashes, vanished, trashed, liveProvenance int
}

// corpus reads the user's whole message table as ground truth (superuser).
func (f *liveFixture) corpus(t *testing.T) corpusCounts {
	t.Helper()
	var c corpusCounts
	if err := f.super.QueryRow(context.Background(), `
		SELECT count(*), count(DISTINCT content_hash),
		       count(*) FILTER (WHERE vanished_at IS NOT NULL),
		       count(*) FILTER (WHERE trashed_at IS NOT NULL)
		  FROM app.messages WHERE user_id = $1`, f.userID.String()).
		Scan(&c.messages, &c.hashes, &c.vanished, &c.trashed); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	if err := f.super.QueryRow(context.Background(), `
		SELECT count(*) FROM (SELECT content_hash FROM app.messages WHERE user_id = $1
		                      GROUP BY content_hash HAVING count(*) > 1) d`, f.userID.String()).
		Scan(&c.duplicateHashes); err != nil {
		t.Fatalf("duplicate hashes: %v", err)
	}
	c.liveProvenance = f.liveProvenanceCount(t)
	return c
}
