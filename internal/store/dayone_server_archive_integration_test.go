// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// DAY-ONE ARCHIVES, ON THE SERVER (#934), against the day-one fixture
// (setup_integration_test.go) plus four more older INBOX messages: after its
// run, seven older INBOX messages are archived in Mailroom only, 26 older
// folder-A/B messages are archived in Mailroom but live in other server
// folders, and three in-window INBOX messages were never touched.
//
// ⚠ EVERY "NOT MOVED" ASSERTION HAS A "MOVED" TWIN. "No intent" is trivially
// true of a function that moves nothing, so the same test proves the three
// remaining candidates DO get one. The exclusions each have their own message
// and their own count, and each is shown to be a candidate BEFORE it is
// excluded, so a count of 1 is a message leaving the set, not one that was
// never in it. What turns red, by probe:
//   - `desk_archived_at IS NOT NULL` gone: the scan offers in-window mail and
//     the by-id refusals fail.
//   - the INBOX EXISTS gone: InInbox becomes 33.
//   - the outside-INBOX NOT EXISTS gone: the two-copy message is moved, with
//     its folder copy (a move intent takes every live copy in the account).
//   - the flagged/pinned clauses gone: those messages are moved.
//   - the setup source gone: 0 setup->Archive intents.
//   - the LabelBySetup arm in ApplyEmailUpdate gone: the learned placement is
//     marked kept.

import (
	"context"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"
)

func (f *setupFixture) dayOneReport(t *testing.T) DayOneServerArchiveReport {
	t.Helper()
	var r DayOneServerArchiveReport
	if err := f.s.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var e error
		r, e = tx.DayOneServerArchiveReport(context.Background())
		return e
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	return r
}

func (f *setupFixture) dayOneCandidates(t *testing.T, limit int) []ulid.ULID {
	t.Helper()
	var ids []ulid.ULID
	if err := f.s.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var e error
		ids, e = tx.DayOneServerArchiveCandidates(context.Background(), limit)
		return e
	}); err != nil {
		t.Fatalf("candidates: %v", err)
	}
	return ids
}

func (f *setupFixture) archiveDayOne(t *testing.T, id ulid.ULID) bool {
	t.Helper()
	var ok bool
	if err := f.s.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var e error
		ok, e = tx.ArchiveDayOneOnServer(context.Background(), id)
		return e
	}); err != nil {
		t.Fatalf("archive %s: %v", id, err)
	}
	return ok
}

func (f *setupFixture) dayOneRun(t *testing.T, limit, batch int) (moved, skipped int) {
	t.Helper()
	moved, skipped, err := f.s.ArchiveDayOneOnServerRun(context.Background(), f.userID, limit, batch)
	if err != nil {
		t.Fatalf("run(limit %d, batch %d): %v", limit, batch, err)
	}
	return moved, skipped
}

func (f *setupFixture) moveIntents(t *testing.T, id ulid.ULID) (all, setupToArchive int) {
	t.Helper()
	f.scan(t, &all, `SELECT count(*) FROM app.writeback_intents
		WHERE user_id=$1 AND message_id=$2 AND op='move'`, f.u.ID, id.String())
	f.scan(t, &setupToArchive, `SELECT count(*) FROM app.writeback_intents
		WHERE user_id=$1 AND message_id=$2 AND op='move' AND source='setup'
		  AND payload->>'mailbox'='Archive' AND payload->>'target_role'='archive'`, f.u.ID, id.String())
	return all, setupToArchive
}

func TestDayOneServerArchive(t *testing.T) {
	f := newSetupFixture(t)

	// Before any setup run nothing is day-one archived, so nothing at all.
	if r := f.dayOneReport(t); r.DeskArchived != 0 || r.InInbox != 0 || r.Candidates != 0 {
		t.Fatalf("before setup: report %+v, want all zero", r)
	}

	// Four more older INBOX messages, so there are enough candidates to
	// exclude one per reason and still move three.
	var extra []ulid.ULID
	for i := 0; i < 4; i++ {
		id := f.seedMessage(t, fmt.Sprintf("inbox-old-extra%d", i), "extra@inbox.example", setupOldDays, false, "")
		f.provenance(t, id, f.inboxID)
		extra = append(extra, id)
	}
	f.applyRun(t, 5)
	inboxOld := append([]ulid.ULID{f.inboxOlderTabbed}, f.inboxOlderPlain...)
	inboxOld = append(inboxOld, extra...)
	inFolders := len(f.messagesIn[f.folderA]) + len(f.messagesIn[f.folderB])

	r := f.dayOneReport(t)
	if r.InInbox != len(inboxOld) || r.DeskArchived != len(inboxOld)+inFolders {
		t.Fatalf("after setup: in inbox %d of %d day-one archived; want %d of %d",
			r.InInbox, r.DeskArchived, len(inboxOld), len(inboxOld)+inFolders)
	}
	if r.Candidates != 7 || r.Movable != 7 {
		t.Fatalf("after setup: candidates %d movable %d, want 7 and 7", r.Candidates, r.Movable)
	}
	if len(r.Accounts) != 1 || r.Accounts[0].Candidates != 7 || r.Accounts[0].ArchiveMailbox != "Archive" ||
		!r.Accounts[0].WritebackLive {
		t.Fatalf("after setup: accounts %+v, want one live account, 7 candidates, -> Archive", r.Accounts)
	}

	// One exclusion per reason, each on a message that was a candidate.
	snoozed, twoCopies := f.inboxOlderPlain[1], f.inboxOlderPlain[0]
	flagged, pinned := extra[2], extra[3]
	mustExec(t, f.h, `UPDATE app.messages SET snoozed_until = now() + interval '1 day' WHERE id=$1`, snoozed.String())
	f.provenance(t, twoCopies, f.folderC)
	mustExec(t, f.h, `UPDATE app.messages SET is_flagged = true WHERE id=$1`, flagged.String())
	mustExec(t, f.h, `UPDATE app.messages SET pinned_at = now() WHERE id=$1`, pinned.String())
	movers := []ulid.ULID{f.inboxOlderTabbed, extra[0], extra[1]}

	r = f.dayOneReport(t)
	if r.Candidates != 3 || r.Movable != 3 || r.Snoozed != 1 || r.OutsideInbox != 1 || r.Flagged != 1 || r.Pinned != 1 {
		t.Fatalf("with exclusions: candidates %d movable %d snoozed %d outside %d flagged %d pinned %d; "+
			"want 3, 3, 1, 1, 1, 1", r.Candidates, r.Movable, r.Snoozed, r.OutsideInbox, r.Flagged, r.Pinned)
	}

	// The by-id re-check refuses everything that does not qualify.
	refused := append([]ulid.ULID{snoozed, twoCopies, flagged, pinned}, f.inboxInWindow...)
	for _, id := range refused {
		if f.archiveDayOne(t, id) {
			t.Errorf("message %s does not qualify but ArchiveDayOneOnServer moved it", id)
		}
	}

	// An open learned placement on a mover: setup moving it is not the user
	// keeping it.
	mustExec(t, f.h, `UPDATE app.message_labels SET applied_by = 'learned'
		WHERE message_id=$1 AND label_id=$2`, f.inboxOlderTabbed.String(), f.updatesLabel.String())
	var learnedLabels int
	f.scan(t, &learnedLabels, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND applied_by='learned'`,
		f.inboxOlderTabbed.String())
	if learnedLabels != 1 {
		t.Fatalf("fixture: the tabbed message carries %d learned label(s), want 1 (its area label)", learnedLabels)
	}
	mustExec(t, f.h, `INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id)
		VALUES ($1,$2,'address','news@inbox.example',$3)`, f.u.ID, f.inboxOlderTabbed.String(), f.updatesArea.String())

	var changesBefore int
	f.scan(t, &changesBefore, `SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Email'`, f.u.ID)

	// The run honours its limit across batches, then finishes the rest, then
	// has nothing left.
	if moved, skipped := f.dayOneRun(t, 2, 1); moved != 2 || skipped != 0 {
		t.Fatalf("run(limit 2, batch 1): moved %d skipped %d, want 2 and 0", moved, skipped)
	}
	if r = f.dayOneReport(t); r.Candidates != 1 {
		t.Fatalf("after the limited run: %d candidate(s), want 1", r.Candidates)
	}
	if moved, _ := f.dayOneRun(t, 0, 50); moved != 1 {
		t.Fatalf("run(no limit): moved %d, want 1", moved)
	}
	if moved, _ := f.dayOneRun(t, 0, 50); moved != 0 {
		t.Fatalf("rerun: moved %d, want 0", moved)
	}

	// The movers: placed in Archive, one setup-sourced move intent each, one
	// Email change row each.
	for _, id := range movers {
		var moved *string
		f.scan(t, &moved, `SELECT moved_mailbox_id FROM app.messages WHERE id=$1`, id.String())
		if moved == nil || *moved != f.archiveID.String() {
			t.Errorf("message %s: moved_mailbox_id %v, want Archive %s", id, moved, f.archiveID)
		}
		if n, ok := f.moveIntents(t, id); n != 1 || ok != 1 {
			t.Errorf("message %s: %d move intent(s), %d setup->Archive; want 1 and 1", id, n, ok)
		}
	}
	var changesAfter int
	f.scan(t, &changesAfter, `SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Email'`, f.u.ID)
	if changesAfter-changesBefore != len(movers) {
		t.Errorf("Email change rows: +%d, want +%d (one per moved message)", changesAfter-changesBefore, len(movers))
	}
	var outcome string
	f.scan(t, &outcome, `SELECT outcome FROM app.learned_placements WHERE message_id=$1`, f.inboxOlderTabbed.String())
	if outcome != "open" {
		t.Errorf("learned placement on a setup-moved message: outcome %q, want open", outcome)
	}

	// Nothing else moved.
	untouched := append([]ulid.ULID{}, refused...)
	untouched = append(untouched, f.messagesIn[f.folderA]...)
	untouched = append(untouched, f.messagesIn[f.folderB]...)
	for _, id := range untouched {
		if n, _ := f.moveIntents(t, id); n != 0 {
			t.Errorf("message %s got %d move intent(s); it must not move", id, n)
		}
	}

	r = f.dayOneReport(t)
	if r.Candidates != 0 || r.PlacedSince != len(movers) || r.SetupMoves["pending"] != len(movers) {
		t.Fatalf("after apply: candidates %d placed %d setup moves %v; want 0, %d, pending %d",
			r.Candidates, r.PlacedSince, r.SetupMoves, len(movers), len(movers))
	}
}

// TestDayOneServerArchiveNeedsAnArchiveFolder: an account with no Archive
// mailbox has its candidates reported as blocked and offered to nobody. Before
// and after on the same fixture, so "blocked" is shown to differ from "none".
func TestDayOneServerArchiveNeedsAnArchiveFolder(t *testing.T) {
	f := newSetupFixture(t)
	f.applyRun(t, 5)
	if r := f.dayOneReport(t); r.Movable != 3 {
		t.Fatalf("with an Archive folder: movable %d, want 3", r.Movable)
	}

	mustExec(t, f.h, `UPDATE app.mailboxes SET special_use = NULL WHERE id=$1`, f.archiveID.String())
	r := f.dayOneReport(t)
	if r.Candidates != 3 || r.Movable != 0 {
		t.Fatalf("without one: candidates %d movable %d, want 3 and 0", r.Candidates, r.Movable)
	}
	if len(r.Accounts) != 1 || r.Accounts[0].ArchiveMailbox != "" || r.Accounts[0].Movable() {
		t.Fatalf("without one: accounts %+v, want no Archive mailbox and not movable", r.Accounts)
	}
	if got := f.dayOneCandidates(t, 100); len(got) != 0 {
		t.Fatalf("without one: %d candidate(s) offered, want 0", len(got))
	}
	if moved, _ := f.dayOneRun(t, 0, 50); moved != 0 {
		t.Fatalf("without one: run moved %d, want 0", moved)
	}
}

// TestDayOneServerArchiveNeedsLiveWriteback: with writeback off — for the
// account, or for the whole instance — a move would be born 'suppressed',
// which is terminal, so nothing is movable and nothing is enqueued.
func TestDayOneServerArchiveNeedsLiveWriteback(t *testing.T) {
	f := newSetupFixture(t)
	f.applyRun(t, 5)
	if r := f.dayOneReport(t); r.Movable != 3 {
		t.Fatalf("writeback live: movable %d, want 3", r.Movable)
	}

	mustExec(t, f.h, `UPDATE app.accounts SET writeback_enabled = false WHERE id=$1`, f.u.AccountID)
	r := f.dayOneReport(t)
	if r.Movable != 0 || r.Accounts[0].WritebackLive || r.Accounts[0].WritebackOff != "account writeback is off" {
		t.Fatalf("account writeback off: movable %d, account %+v", r.Movable, r.Accounts[0])
	}
	if moved, _ := f.dayOneRun(t, 0, 50); moved != 0 {
		t.Fatalf("account writeback off: run moved %d, want 0", moved)
	}

	mustExec(t, f.h, `UPDATE app.accounts SET writeback_enabled = true WHERE id=$1`, f.u.AccountID)
	f.s.SetWritebackDisabled(true)
	r = f.dayOneReport(t)
	if r.Movable != 0 || r.Accounts[0].WritebackOff != "instance is ingest-only" {
		t.Fatalf("instance ingest-only: movable %d, account %+v", r.Movable, r.Accounts[0])
	}
	if moved, _ := f.dayOneRun(t, 0, 50); moved != 0 {
		t.Fatalf("instance ingest-only: run moved %d, want 0", moved)
	}

	var intents int
	f.scan(t, &intents, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1 AND op='move' AND source='setup'`, f.u.ID)
	if intents != 0 {
		t.Fatalf("%d setup move intent(s) enqueued with writeback off, want 0", intents)
	}
}
