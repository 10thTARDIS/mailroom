// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package mailsync's live end-to-end coverage for FOLDER DELETION (#87).
//
// A folder deleted in another IMAP client stops appearing in LIST, so nothing
// ever selects it again, nothing reaps it, and its provenance rows live forever —
// every message it held stays "in" a mailbox that no longer exists. This is the
// same class of bug as the expunge reaper (#86), one level up: a whole folder
// leaving the server rather than a single message.
//
// It is proved against a REAL Dovecot, not a double, for the same reason the
// reaper is: the destructive conclusion ("this folder is gone, drop its
// membership") is drawn from what a real server's LIST does and does not return,
// and getting it wrong destroys mail. The folders are created and deleted on the
// server exactly as an operator or another client would (adminClient, from
// uidvalidity_integration_test.go — Mailroom's own SyncSession deliberately cannot
// manage mailboxes).
//
// Run via scripts/mailsync-test.sh. Without a live Dovecot + PostgreSQL, every
// test here skips (newLiveFixture).
package mailsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// createFolder makes a fresh (empty) folder on the server, tolerating a leftover
// of the same name from an earlier run by deleting it first.
func (f *liveFixture) createFolder(t *testing.T, name string) {
	t.Helper()
	c := f.adminClient(t)
	_ = c.Delete(name).Wait() // ignore "no such mailbox"
	if err := c.Create(name, nil).Wait(); err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
}

// deleteFolder deletes a folder on the server, as another IMAP client would.
func (f *liveFixture) deleteFolder(t *testing.T, name string) {
	t.Helper()
	c := f.adminClient(t)
	if err := c.Delete(name).Wait(); err != nil {
		t.Fatalf("delete %q: %v", name, err)
	}
}

// mailboxRowExists reports whether Mailroom still holds an app.mailboxes row for
// the IMAP name.
func (f *liveFixture) mailboxRowExists(t *testing.T, imapName string) bool {
	t.Helper()
	var one int
	err := f.super.QueryRow(context.Background(),
		`SELECT 1 FROM app.mailboxes WHERE user_id = $1 AND imap_name = $2`,
		f.userID.String(), imapName).Scan(&one)
	return err == nil
}

// mailboxDestroyChanges counts the Mailbox `destroyed` change_log rows for an id.
func (f *liveFixture) mailboxDestroyChanges(t *testing.T, id ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Mailbox' AND object_id = $2 AND op = 'destroyed'`,
		f.userID.String(), id.String()).Scan(&n); err != nil {
		t.Fatalf("mailbox destroy changes: %v", err)
	}
	return n
}

func (f *liveFixture) movedMailbox(t *testing.T, msg ulid.ULID) string {
	t.Helper()
	var moved string
	if err := f.super.QueryRow(context.Background(),
		`SELECT COALESCE(moved_mailbox_id, '') FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&moved); err != nil {
		t.Fatalf("moved mailbox: %v", err)
	}
	return moved
}

// THE BUG THIS ISSUE EXISTS FOR (parts a + b). A folder deleted on the server
// leaves its messages: one that lived ONLY there is soft-deleted, one that also
// lived in Archive keeps Archive — and both membership changes reach /changes and
// the index. The folder itself is destroyed.
func TestLiveDeletedFolderIsReconciledAndAnnounced(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond)) // always due
	const folder = "Receipts"
	f.createFolder(t, folder)

	f.appendMsg(t, folder, "only-in-receipts")
	// Same bytes in two folders dedup to ONE canonical message with two locations.
	f.appendMsg(t, folder, "in-two-folders")
	f.appendMsg(t, f.archive, "in-two-folders")

	f.syncOnce(t)
	receiptsID := f.mailboxID(t, folder)
	archiveID := f.mailboxID(t, f.archive)
	only, ok := f.messageIDBySubject(t, "only-in-receipts")
	if !ok {
		t.Fatal("only-in-receipts was never ingested")
	}
	shared, ok := f.messageIDBySubject(t, "in-two-folders")
	if !ok {
		t.Fatal("in-two-folders was never ingested")
	}
	if got := f.mailboxIDs(t, only); len(got) != 1 || got[0] != receiptsID.String() {
		t.Fatalf("precondition: only-in-receipts is not in {Receipts}: %v", got)
	}
	if got := f.mailboxIDs(t, shared); len(got) != 2 {
		t.Fatalf("precondition: in-two-folders is not in two folders: %v", got)
	}
	beforeOnly, beforeShared := f.emailChanges(t, only), f.emailChanges(t, shared)

	// THE DELETION. Another client deletes the folder.
	f.deleteFolder(t, folder)

	sum := f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("sync errors: %v", sum.Errors)
	}
	if sum.MailboxesDeleted != 1 {
		t.Fatalf("MailboxesDeleted = %d, want 1: %+v", sum.MailboxesDeleted, sum)
	}

	// The message that lived only in the deleted folder: gone from everywhere,
	// soft-deleted (ADR-009), and announced.
	if got := f.mailboxIDs(t, only); len(got) != 0 {
		t.Errorf("only-in-receipts is STILL in %v — the whole point of #87", got)
	}
	if !f.trashed(t, only) {
		t.Error("only-in-receipts was not soft-deleted, though it is in no folder at all")
	}
	if !f.trashState(t, only).vanished {
		t.Error("no vanished_at: the soft-delete is not retractable")
	}
	if got := f.emailChanges(t, only) - beforeOnly; got != 1 {
		t.Errorf("Email change_log rows for only-in-receipts = %d, want 1", got)
	}

	// The message that also lived in Archive: keeps Archive, is NOT deleted, and its
	// mailboxIds change is announced.
	if got := f.mailboxIDs(t, shared); len(got) != 1 || got[0] != archiveID.String() {
		t.Errorf("in-two-folders mailboxIds = %v, want {Archive}", got)
	}
	if f.trashed(t, shared) {
		t.Fatal("SOFT-DELETED A MESSAGE THAT IS SITTING IN ARCHIVE")
	}
	if got := f.emailChanges(t, shared) - beforeShared; got != 1 {
		t.Errorf("Email change_log rows for in-two-folders = %d, want 1", got)
	}
	// Email/query agrees with Email/get.
	if got := f.inMailbox(t, archiveID); len(got) != 1 || got[0] != shared.String() {
		t.Errorf("inMailbox(Archive) = %v, want only the shared message", got)
	}

	// The Mailbox object is destroyed: its row is gone and /changes carries it.
	if f.mailboxRowExists(t, folder) {
		t.Error("the deleted folder's app.mailboxes row survived")
	}
	if got := f.mailboxDestroyChanges(t, receiptsID); got != 1 {
		t.Errorf("Mailbox destroyed change_log rows = %d, want 1", got)
	}
}

// A FAILED LIST RECONCILES NOTHING (part c). A LIST that fails (here: a network
// error) is not "every folder was deleted" — it is "we could not ask". The folder
// we hold, and its message, are untouched. This is the folder-level twin of "a
// failed sync reaps nothing".
func TestLiveDeletedFolderFailedListReconcilesNothing(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	const folder = "Ephemeral"
	f.createFolder(t, folder)
	t.Cleanup(func() { f.deleteFolder(t, folder) })

	f.appendMsg(t, folder, "must-survive-a-failed-list")
	f.syncOnce(t)
	msg, ok := f.messageIDBySubject(t, "must-survive-a-failed-list")
	if !ok {
		t.Fatal("message was never ingested")
	}
	folderID := f.mailboxID(t, folder)

	// The LIST fails outright — the one failure mode "the folder is gone" must never
	// be confused with.
	broken := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &brokenList{SyncSession: s}, nil
		}, WithReapInterval(time.Nanosecond))

	if _, err := broken.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	}); err == nil {
		t.Fatal("a sync whose LIST failed succeeded; the test proves nothing")
	}

	if !f.mailboxRowExists(t, folder) {
		t.Fatal("DESTROYED A FOLDER BECAUSE THE LIST FAILED")
	}
	if f.trashed(t, msg) {
		t.Fatal("SOFT-DELETED MAIL BECAUSE THE LIST FAILED")
	}
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != folderID.String() {
		t.Fatalf("membership changed across a failed LIST: %v", got)
	}
	if got := f.mailboxDestroyChanges(t, folderID); got != 0 {
		t.Errorf("emitted %d Mailbox destroyed rows across a failed LIST, want 0", got)
	}
}

// THE LANDMINE (part d). A message the user MOVED into a folder (moved_mailbox_id)
// whose folder is then deleted must not have its placement SILENTLY nulled by
// ON DELETE SET NULL. Reconciliation clears it explicitly, re-derives membership
// from where the bytes actually are, and — the part a silent cascade would skip —
// writes a change_log row.
func TestLiveDeletedFolderDoesNotSilentlyNullCanonicalPlacements(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	ctx := context.Background()
	const folder = "Receipts"
	f.createFolder(t, folder)

	// The message physically lives in the inbox; the user moves it into Receipts in
	// Mailroom (Email/set), which sets moved_mailbox_id — writeback has not moved the
	// bytes, so its provenance still says INBOX.
	f.appendMsg(t, f.inbox, "moved-into-receipts")
	f.syncOnce(t)
	inboxID := f.mailboxID(t, f.inbox)
	receiptsID := f.mailboxID(t, folder)
	msg, ok := f.messageIDBySubject(t, "moved-into-receipts")
	if !ok {
		t.Fatal("message was never ingested")
	}

	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{MoveToMailboxID: &receiptsID})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("message not found for Email/set")
		}
		return nil
	}); err != nil {
		t.Fatalf("Email/set move: %v", err)
	}
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != receiptsID.String() {
		t.Fatalf("precondition: after the move mailboxIds = %v, want {Receipts}", got)
	}
	if f.movedMailbox(t, msg) != receiptsID.String() {
		t.Fatal("precondition: moved_mailbox_id was not set")
	}
	beforeChanges := f.emailChanges(t, msg)

	// The destination folder is deleted on the server.
	f.deleteFolder(t, folder)

	sum := f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("sync errors: %v", sum.Errors)
	}
	if sum.MailboxesDeleted != 1 {
		t.Fatalf("MailboxesDeleted = %d, want 1", sum.MailboxesDeleted)
	}

	// The placement was RE-DERIVED, not silently nulled:
	if m := f.movedMailbox(t, msg); m != "" {
		t.Errorf("moved_mailbox_id = %s, want cleared", m)
	}
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != inboxID.String() {
		t.Errorf("mailboxIds = %v, want {INBOX} — membership fell back to where the bytes actually are", got)
	}
	if f.trashed(t, msg) {
		t.Fatal("SOFT-DELETED A MESSAGE WHOSE BYTES ARE STILL IN THE INBOX")
	}
	// The proof that the cascade did NOT do this silently: a change_log row exists.
	if got := f.emailChanges(t, msg) - beforeChanges; got < 1 {
		t.Errorf("Email change_log rows = %d, want >= 1 — a silent ON DELETE SET NULL would have written ZERO, undoing #84 invisibly", got)
	}
	if f.mailboxRowExists(t, folder) {
		t.Error("the deleted folder's row survived")
	}
}

// brokenList is a real IMAP session whose LIST fails — the failure that must never
// be read as "every folder was deleted".
type brokenList struct {
	sync.SyncSession
}

func (b *brokenList) ListMailboxes(ctx context.Context) ([]sync.Mailbox, error) {
	return nil, errors.New("injected fault: cannot LIST mailboxes")
}

var _ sync.SyncSession = (*brokenList)(nil)
