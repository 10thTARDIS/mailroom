// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the two membership bugs in mailbox provenance /
// reconciliation, against a real PostgreSQL with RLS enforced, connecting as
// mailroom_app:
//
//   - #94: MarkMailboxProvenanceStale used to retire a mailbox's live provenance
//     with NO change_log row, so every message dropped out of the folder the
//     instant it committed and no client was told. It now announces the loss.
//   - #87: a folder deleted on the server (gone from LIST) is reconciled and
//     destroyed — its messages lose that membership, the change stream carries it,
//     and the ON DELETE SET NULL landmine on canonical placements is neutralised.
//
// Both reuse the reaper fixture (reaper_integration_test.go).

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// markStale runs MarkMailboxProvenanceStale and returns the retired-row count.
func (f *reapFixture) markStale(t *testing.T, mailbox ulid.ULID) int64 {
	t.Helper()
	var n int64
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var err error
		n, err = tx.MarkMailboxProvenanceStale(context.Background(), mailbox)
		return err
	}); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	return n
}

// setMoved / setLocal place a canonical / local placement on a message.
func (f *reapFixture) setMoved(t *testing.T, msg, mailbox ulid.ULID) {
	t.Helper()
	if _, err := f.h.super.Exec(context.Background(),
		`UPDATE app.messages SET moved_mailbox_id = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String(), mailbox.String()); err != nil {
		t.Fatalf("set moved: %v", err)
	}
}

func (f *reapFixture) setLocal(t *testing.T, msg, mailbox ulid.ULID) {
	t.Helper()
	if _, err := f.h.super.Exec(context.Background(),
		`UPDATE app.messages SET local_mailbox_id = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String(), mailbox.String()); err != nil {
		t.Fatalf("set local: %v", err)
	}
}

// #94. Retiring a mailbox's live provenance is a JMAP-visible membership change
// for every message whose mailboxIds are DERIVED from it — so it must append one
// Email change_log row per such message, and NONE for a message whose membership
// is read from a canonical placement instead (where retiring provenance changes
// nothing a client can see).
func TestMarkMailboxProvenanceStaleAnnouncesMembershipLoss(t *testing.T) {
	f := newReapFixture(t)

	// only: lives only in the inbox — retiring it empties its membership.
	only := f.seedMessage(t, "only in the rolled folder")
	f.seedProvenance(t, only, f.inbox, 1, 42, "live")

	// shared: also lives in Archive — retiring the inbox row shrinks {inbox,archive}
	// to {archive}, still a visible change.
	shared := f.seedMessage(t, "also in Archive")
	f.seedProvenance(t, shared, f.inbox, 2, 42, "live")
	f.seedProvenance(t, shared, f.archive, 9, 90, "live")

	// placed: the user moved it (moved_mailbox_id) — its membership is read from that
	// column, not provenance, so retiring the inbox row is INVISIBLE and owes no row.
	placed := f.seedMessage(t, "user moved it elsewhere")
	f.seedProvenance(t, placed, f.inbox, 3, 42, "live")
	f.setMoved(t, placed, f.archive)

	b0, b1, b2 := f.emailChanges(t, only), f.emailChanges(t, shared), f.emailChanges(t, placed)

	retired := f.markStale(t, f.inbox)
	if retired != 3 {
		t.Fatalf("retired rows = %d, want 3 (one live inbox row per message)", retired)
	}

	if got := f.emailChanges(t, only) - b0; got != 1 {
		t.Errorf("`only` change_log rows = %d, want exactly 1 — it dropped out of the folder", got)
	}
	if got := f.emailChanges(t, shared) - b1; got != 1 {
		t.Errorf("`shared` change_log rows = %d, want exactly 1 — its mailboxIds shrank", got)
	}
	if got := f.emailChanges(t, placed) - b2; got != 0 {
		t.Errorf("`placed` change_log rows = %d, want 0 — a canonical placement hides the retirement", got)
	}

	// The rows really are retired, and the survivor's other location is untouched.
	if n := f.provenanceCount(t, only, "live"); n != 0 {
		t.Errorf("`only` still has %d live rows", n)
	}
	if got := f.liveMailboxes(t, shared); len(got) != 1 || got[0] != f.archive.String() {
		t.Errorf("`shared` live mailboxes = %v, want {Archive}", got)
	}
}

// insertMailbox adds an extra mailbox to the fixture (the fixture seeds only
// INBOX/Archive/Trash); ReconcileDeletedMailbox then destroys it.
func (f *reapFixture) insertMailbox(t *testing.T, name string) ulid.ULID {
	t.Helper()
	id := NewULID()
	if _, err := f.h.super.Exec(context.Background(),
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		 VALUES ($1, $2, $3, $4, $4)`,
		id.String(), f.userID.String(), f.accountID.String(), name); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	return id
}

func (f *reapFixture) mailboxExists(t *testing.T, id ulid.ULID) bool {
	t.Helper()
	var one int
	err := f.h.super.QueryRow(context.Background(),
		`SELECT 1 FROM app.mailboxes WHERE user_id = $1 AND id = $2`,
		f.userID.String(), id.String()).Scan(&one)
	return err == nil
}

func (f *reapFixture) mailboxDestroyChanges(t *testing.T, id ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Mailbox' AND object_id = $2 AND op = 'destroyed'`,
		f.userID.String(), id.String()).Scan(&n); err != nil {
		t.Fatalf("mailbox destroy changes: %v", err)
	}
	return n
}

func (f *reapFixture) reconcileDeleted(t *testing.T, mailbox ulid.ULID) DeletedMailboxResult {
	t.Helper()
	var res DeletedMailboxResult
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var err error
		res, err = tx.ReconcileDeletedMailbox(context.Background(), mailbox)
		return err
	}); err != nil {
		t.Fatalf("reconcile deleted mailbox: %v", err)
	}
	return res
}

// #87. A folder the server deleted is reconciled and destroyed. Its messages lose
// that membership honestly (change_log rows, soft-deletes where they are nowhere
// else), the Mailbox object is destroyed, and — THE LANDMINE — a canonical
// placement that named the folder is re-derived with a change_log row, never
// silently nulled by ON DELETE SET NULL.
func TestReconcileDeletedMailboxReconcilesAndDestroys(t *testing.T) {
	f := newReapFixture(t)
	receipts := f.insertMailbox(t, "Receipts")

	// only: physically only in the doomed folder → soft-deleted, retractably.
	only := f.seedMessage(t, "only in Receipts")
	f.seedProvenance(t, only, receipts, 1, 5, "live")

	// shared: also physically in Archive → keeps Archive, loses Receipts.
	shared := f.seedMessage(t, "in Receipts and Archive")
	f.seedProvenance(t, shared, receipts, 2, 5, "live")
	f.seedProvenance(t, shared, f.archive, 7, 90, "live")

	// moved: THE LANDMINE. The user moved it INTO Receipts (moved_mailbox_id), but
	// the bytes still physically sit in the inbox (writeback not landed). Deleting
	// the mailbox row would SET NULL its placement silently; reconciliation must
	// clear it WITH a change_log row and re-derive membership to {inbox}.
	moved := f.seedMessage(t, "user moved this into Receipts")
	f.seedProvenance(t, moved, f.inbox, 3, 42, "live")
	f.setMoved(t, moved, receipts)

	// draft: a local-only draft placed in the folder → nowhere once it is gone.
	draft := f.seedMessage(t, "draft in Receipts")
	f.setLocal(t, draft, receipts)

	b0, b1, b2, b3 := f.emailChanges(t, only), f.emailChanges(t, shared), f.emailChanges(t, moved), f.emailChanges(t, draft)

	res := f.reconcileDeleted(t, receipts)

	if res.ProvenanceDropped != 2 {
		t.Errorf("ProvenanceDropped = %d, want 2 (only + shared live in Receipts)", res.ProvenanceDropped)
	}
	if res.MembershipChanged != 4 {
		t.Errorf("MembershipChanged = %d, want 4 (every referencing message)", res.MembershipChanged)
	}
	if res.MessagesVanished != 2 {
		t.Errorf("MessagesVanished = %d, want 2 (only + draft, nowhere else)", res.MessagesVanished)
	}

	// The Mailbox object is destroyed, and its row is actually gone.
	if f.mailboxExists(t, receipts) {
		t.Error("the deleted folder's app.mailboxes row survived")
	}
	if got := f.mailboxDestroyChanges(t, receipts); got != 1 {
		t.Errorf("Mailbox destroyed change_log rows = %d, want 1", got)
	}

	// only: gone from everywhere, soft-deleted, announced.
	if s := f.msgState(t, only); !s.trashed || !s.vanished {
		t.Errorf("`only` not soft-deleted: %+v", s)
	}
	if got := f.emailChanges(t, only) - b0; got != 1 {
		t.Errorf("`only` change_log rows = %d, want 1", got)
	}

	// shared: still in Archive, not deleted, membership change announced.
	if s := f.msgState(t, shared); s.trashed {
		t.Error("`shared` was soft-deleted despite living in Archive")
	}
	if got := f.liveMailboxes(t, shared); len(got) != 1 || got[0] != f.archive.String() {
		t.Errorf("`shared` live mailboxes = %v, want {Archive}", got)
	}
	if got := f.emailChanges(t, shared) - b1; got != 1 {
		t.Errorf("`shared` change_log rows = %d, want 1", got)
	}

	// moved: THE LANDMINE ASSERTION. Its placement was re-derived (not silently
	// nulled): moved_mailbox_id is clear, membership fell back to {inbox}, it was
	// NOT soft-deleted, and — the part a silent SET NULL would have skipped — a
	// change_log row was written.
	if s := f.msgState(t, moved); s.moved != "" {
		t.Errorf("moved_mailbox_id = %s, want cleared", s.moved)
	}
	if s := f.msgState(t, moved); s.trashed {
		t.Error("`moved` was soft-deleted even though its bytes are in the inbox")
	}
	if got := f.liveMailboxes(t, moved); len(got) != 1 || got[0] != f.inbox.String() {
		t.Errorf("`moved` live mailboxes = %v, want {INBOX} (membership re-derived from provenance)", got)
	}
	if got := f.emailChanges(t, moved) - b2; got != 1 {
		t.Errorf("`moved` change_log rows = %d, want 1 — a silent SET NULL would have emitted ZERO", got)
	}

	// draft: local placement cleared, nowhere → soft-deleted, announced.
	if s := f.msgState(t, draft); !s.trashed || !s.vanished {
		t.Errorf("`draft` not soft-deleted: %+v", s)
	}
	if got := f.emailChanges(t, draft) - b3; got != 1 {
		t.Errorf("`draft` change_log rows = %d, want 1", got)
	}
}

// A deleted-folder reconciliation in one user's transaction cannot touch another
// user's mailbox: RLS, not a WHERE clause, is what enforces that.
func TestReconcileDeletedMailboxIsScopedByRLS(t *testing.T) {
	f := newReapFixture(t)
	receipts := f.insertMailbox(t, "Receipts")
	msg := f.seedMessage(t, "mine")
	f.seedProvenance(t, msg, receipts, 1, 5, "live")

	// As another user, the mailbox is invisible: nothing is dropped and the call
	// reports the mailbox as not found rather than reconciling my rows.
	err := f.st.UserTx(context.Background(), f.otherID, func(tx *UserTx) error {
		_, err := tx.ReconcileDeletedMailbox(context.Background(), receipts)
		return err
	})
	if err == nil {
		t.Fatal("another user reconciled a mailbox that is not theirs")
	}
	if !f.mailboxExists(t, receipts) {
		t.Fatal("RLS did not protect my mailbox from another user's reconcile")
	}
	if n := f.provenanceCount(t, msg, "live"); n != 1 {
		t.Fatal("another user's reconcile dropped my provenance")
	}
	if s := f.msgState(t, msg); s.trashed {
		t.Fatal("another user's reconcile soft-deleted my mail")
	}
}

// rolesFor runs MailboxRolesForAccount as the given user.
func (f *reapFixture) rolesFor(t *testing.T, user ulid.ULID) []MailboxRoleRow {
	t.Helper()
	var rows []MailboxRoleRow
	if err := f.st.UserTx(context.Background(), user, func(tx *UserTx) error {
		var err error
		rows, err = tx.MailboxRolesForAccount(context.Background(), f.accountID)
		return err
	}); err != nil {
		t.Fatalf("mailbox roles: %v", err)
	}
	return rows
}

// MailboxRolesForAccount backs `mailroom-admin mailbox-roles` (#351), which
// answers "what does this provider actually advertise" BEFORE real mail depends
// on the answer. A provider with no \Drafts produces no error at onboarding, no
// error at sync and no error at boot — it produces a failed save, once, later.
//
// Two properties carry the report, and neither is the obvious one:
//
//   - INBOX is role-carrying BY NAME. RFC 6154 defines no \Inbox, so servers
//     advertise nothing for it; deriving the role from SPECIAL-USE alone leaves
//     it role-less. That exact defect shipped once (#117) and dropped INBOX out
//     of the unified inbox.
//   - EVERY mailbox is returned, with the raw attribute, not just the ones that
//     resolved. "the folder does not exist" and "it exists but the server never
//     tagged it" are different problems with different fixes, and a list
//     filtered to roles cannot tell them apart.
func TestMailboxRolesForAccountReportsWhatTheServerAdvertises(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()

	// An untagged ordinary folder, and a folder carrying an attribute Mailroom
	// does not map. Both must appear with role "" — but for visibly different
	// reasons, which is the distinction the report exists to draw.
	if _, err := f.h.super.Exec(ctx,
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		 VALUES ($1, $2, $3, 'Receipts', 'Receipts', NULL),
		        ($4, $2, $3, 'Snoozed', 'Snoozed', '\Snoozed')`,
		NewULID().String(), f.userID.String(), f.accountID.String(), NewULID().String()); err != nil {
		t.Fatalf("seed extra mailboxes: %v", err)
	}

	rows := f.rolesFor(t, f.userID)
	got := map[string]MailboxRoleRow{}
	for _, r := range rows {
		got[r.IMAPName] = r
	}

	if len(rows) != 5 {
		t.Fatalf("returned %d mailboxes, want 5 — the report must list EVERY folder, "+
			"including the ones with no role, or a missing role cannot be told apart "+
			"from a missing folder: %+v", len(rows), rows)
	}

	// ⚠ THE ORDER IS A CONTRACT, NOT A CONVENIENCE. A server can advertise the
	// same SPECIAL-USE on two folders, and MailboxForRole — what the running
	// server actually resolves through — orders by imap_name and takes the FIRST
	// match. The caller relies on these rows arriving in that same order to name
	// the folder mail would really go to. Drop the ORDER BY and the report starts
	// disagreeing with the code it describes, silently.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].IMAPName > rows[i].IMAPName {
			t.Fatalf("rows are not ordered by imap_name (%q before %q); MailboxForRole "+
				"breaks same-role ties by that order, so a caller matching on the first "+
				"row would name a different folder than the server resolves",
				rows[i-1].IMAPName, rows[i].IMAPName)
		}
	}

	// ⚠ THE ONE THAT BREAKS SILENTLY. INBOX carries no SPECIAL-USE, so this
	// asserts BOTH halves: the role resolved, and it resolved from nothing.
	// Asserting only the role would still pass if a future change started
	// inventing a \Inbox attribute.
	if in := got["INBOX"]; in.Role != "inbox" || in.SpecialUse != "" {
		t.Errorf("INBOX = role %q, special_use %q; want role \"inbox\" from an EMPTY "+
			"attribute — RFC 6154 defines no backslash-Inbox, so a server sends none and the "+
			"role has to come from the reserved name (#117)", in.Role, in.SpecialUse)
	}

	for _, tc := range []struct{ name, wantSpecial, wantRole string }{
		{"Archive", `\Archive`, "archive"},
		{"Trash", `\Trash`, "trash"},
		{"Receipts", "", ""},
		// The attribute is REPORTED even though it maps to nothing. That is the
		// difference between "your server tagged this and we ignored it" and
		// "your server tagged nothing" — an operator can act on the first.
		{"Snoozed", `\Snoozed`, ""},
	} {
		r := got[tc.name]
		if r.Role != tc.wantRole || r.SpecialUse != tc.wantSpecial {
			t.Errorf("%s = role %q, special_use %q; want role %q, special_use %q",
				tc.name, r.Role, r.SpecialUse, tc.wantRole, tc.wantSpecial)
		}
	}
}

// RLS is tested, not assumed (CLAUDE.md): the report walks every user's accounts
// in turn, so a leak here would put one user's folder names in another's report.
func TestMailboxRolesForAccountIsScopedByRLS(t *testing.T) {
	f := newReapFixture(t)

	if rows := f.rolesFor(t, f.userID); len(rows) == 0 {
		t.Fatal("the owning user sees no mailboxes — the negative control below would " +
			"then pass for the wrong reason")
	}
	if rows := f.rolesFor(t, f.otherID); len(rows) != 0 {
		t.Errorf("a different user saw %d of this account's mailboxes: %+v", len(rows), rows)
	}
}

// liveCountsFor runs MailboxLiveCounts as the given user, keyed by IMAP name.
func (f *reapFixture) liveCountsFor(t *testing.T, user ulid.ULID) map[string]MailboxLiveCount {
	t.Helper()
	out := map[string]MailboxLiveCount{}
	if err := f.st.UserTx(context.Background(), user, func(tx *UserTx) error {
		rows, err := tx.MailboxLiveCounts(context.Background(), f.accountID)
		for _, r := range rows {
			out[r.IMAPName] = r
		}
		return err
	}); err != nil {
		t.Fatalf("mailbox live counts: %v", err)
	}
	return out
}

// MailboxLiveCounts backs consolidate-folders' "what Mailroom holds" column
// (#647), printed beside the server's STATUS MESSAGES. It must count exactly
// the rows a server UID corresponds to: LIVE provenance, one per UID — so a
// retired generation does not count, a second UID of the same message in the
// same folder DOES (the server counts it too), and a folder with no mail still
// appears, at 0, rather than vanishing from the report.
func TestMailboxLiveCountsCountsLiveUIDsPerFolder(t *testing.T) {
	f := newReapFixture(t)
	a := f.seedMessage(t, "a")
	b := f.seedMessage(t, "b")
	f.seedProvenance(t, a, f.inbox, 1, 7, "live")
	f.seedProvenance(t, b, f.inbox, 2, 7, "live")
	f.seedProvenance(t, b, f.inbox, 3, 7, "live") // a re-delivered copy: a second UID
	f.seedProvenance(t, a, f.inbox, 1, 6, "stale_uidvalidity")
	f.seedProvenance(t, a, f.archive, 9, 7, "live")

	got := f.liveCountsFor(t, f.userID)
	for _, tc := range []struct {
		name string
		want int64
		role string
	}{
		{"INBOX", 3, "inbox"},
		{"Archive", 1, "archive"},
		{"Trash", 0, "trash"},
	} {
		r, ok := got[tc.name]
		if !ok {
			t.Errorf("%s missing from the counts (an empty folder must still be reported): %+v", tc.name, got)
			continue
		}
		if r.LiveProvenance != tc.want || r.Role != tc.role {
			t.Errorf("%s = %d live, role %q; want %d, role %q", tc.name, r.LiveProvenance, r.Role, tc.want, tc.role)
		}
	}
}

func TestMailboxLiveCountsIsScopedByRLS(t *testing.T) {
	f := newReapFixture(t)
	f.seedProvenance(t, f.seedMessage(t, "x"), f.inbox, 1, 7, "live")
	if got := f.liveCountsFor(t, f.userID); got["INBOX"].LiveProvenance != 1 {
		t.Fatalf("the owning user sees %+v — the negative control below would then pass for the wrong reason", got)
	}
	if got := f.liveCountsFor(t, f.otherID); len(got) != 0 {
		t.Errorf("a different user saw this account's folders: %+v", got)
	}
}
