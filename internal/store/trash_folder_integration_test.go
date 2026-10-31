// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Deleted mail has ONE definition (#631): trashed_at set, OR effectively in a
// trash-role mailbox.
//
// Before #631 every default Email/query tested trashed_at alone. trashed_at is
// written only by Mailroom's own delete and by the reaper, so mail another
// client (a phone, webmail) moved to Trash — live provenance in Trash,
// trashed_at NULL — appeared in All mail, Unread, label views and plain search
// while the Trash view listed it too. Production had 34 such messages.
//
// The fixture is one message of every kind the rule has to tell apart:
//
//	A  live in INBOX                                   → listed
//	B  live in Trash, trashed_at NULL (another client) → deleted
//	C  live in Trash, moved_mailbox_id = INBOX         → listed (restored in
//	   Mailroom; the provider copy is still in Trash because writeback is
//	   pending, or disabled on an ingest-only account — production today)
//	D  live in INBOX, trashed_at set (Mailroom's delete, before its move lands)
//	                                                   → deleted
//
// A and B share a thread in which B is the NEWER message, so a collapsed list
// that still saw B would pick B as the thread's representative. C is alone in
// its own thread, so Thread/get can be asked about it directly.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

type trashFolderFixture struct {
	user                   ulid.ULID
	inbox, trash           ulid.ULID
	a, b, c, d             ulid.ULID
	threadAB, thrdC, thrdD ulid.ULID
}

func seedTrashFolderFixture(t *testing.T, h *harness) trashFolderFixture {
	t.Helper()
	f := trashFolderFixture{user: NewULID(), inbox: NewULID(), trash: NewULID(),
		threadAB: NewULID(), thrdC: NewULID(), thrdD: NewULID()}
	u := f.user.String()
	mustExec(t, h, `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1,$2,$3)`,
		u, "sub-"+u, u+"@example.com")
	acct := NewULID().String()
	mustExec(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'A','mail.example.com','smtp.example.com',$3,$4)`,
		acct, u, []byte{0x01}, NewULID().String())
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		VALUES ($1,$2,$3,'INBOX','Inbox')`, f.inbox.String(), u, acct)
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		VALUES ($1,$2,$3,'Trash','Trash','\Trash')`, f.trash.String(), u, acct)
	for _, th := range []ulid.ULID{f.threadAB, f.thrdC, f.thrdD} {
		mustExec(t, h, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1,$2,'t')`, th.String(), u)
	}

	// msg inserts one unseen message (its own blob) received offsetMinutes from
	// now in thread th, with a live copy in mailbox mb, then applies extra (a
	// SET clause; $3 onwards are args) when non-empty.
	msg := func(th, mb ulid.ULID, offsetMinutes int, extra string, args ...any) ulid.ULID {
		t.Helper()
		id := NewULID()
		hash := make([]byte, 32)
		copy(hash, id[:])
		mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, u, hash, NewULID().String())
		mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,'m', now() + make_interval(mins => $5), 10)`,
			id.String(), u, hash, th.String(), offsetMinutes)
		mustExec(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`, NewULID().String(), u, id.String(), acct, mb.String(), testUID())
		if extra != "" {
			mustExec(t, h, `UPDATE app.messages SET `+extra+` WHERE user_id = $1 AND id = $2`,
				append([]any{u, id.String()}, args...)...)
		}
		return id
	}
	f.a = msg(f.threadAB, f.inbox, -10, "")
	f.b = msg(f.threadAB, f.trash, -1, "")
	f.c = msg(f.thrdC, f.trash, -5, "moved_mailbox_id = $3", f.inbox.String())
	f.d = msg(f.thrdD, f.inbox, -3, "trashed_at = now(), purge_after = now() + interval '30 days'")
	return f
}

func (f trashFolderFixture) name(id ulid.ULID) string {
	switch id {
	case f.a:
		return "A (live in INBOX)"
	case f.b:
		return "B (moved to Trash by another client, trashed_at NULL)"
	case f.c:
		return "C (restored in Mailroom, provider copy still in Trash)"
	case f.d:
		return "D (deleted in Mailroom, trashed_at set)"
	}
	return id.String()
}

func TestDeletedMailHasOneDefinition(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.appStore()
	f := seedTrashFolderFixture(t, h)

	run := func(label string, q EmailQuery) (map[ulid.ULID]bool, int) {
		t.Helper()
		var got map[ulid.ULID]bool
		var total int
		if err := st.UserTx(ctx, f.user, func(tx *UserTx) error {
			ids, n, err := tx.QueryEmails(ctx, q)
			if err != nil {
				return err
			}
			total = n
			got = make(map[ulid.ULID]bool, len(ids))
			for _, id := range ids {
				got[id] = true
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: QueryEmails: %v", label, err)
		}
		return got, total
	}
	has := func(label string, got map[ulid.ULID]bool, id ulid.ULID) {
		t.Helper()
		if !got[id] {
			t.Errorf("%s: missing %s", label, f.name(id))
		}
	}
	lacks := func(label string, got map[ulid.ULID]bool, id ulid.ULID) {
		t.Helper()
		if got[id] {
			t.Errorf("%s: returned %s, which is deleted", label, f.name(id))
		}
	}

	// All mail.
	got, total := run("all mail", EmailQuery{})
	lacks("all mail", got, f.b)
	has("all mail", got, f.c)
	has("all mail", got, f.a)
	lacks("all mail", got, f.d)
	if total != 2 {
		t.Errorf("all mail: total = %d, want 2 (A and C)", total)
	}

	// Unread: B is unseen, so only the trash predicate can keep it out.
	got, _ = run("unread", EmailQuery{NotKeyword: KeywordSeen})
	lacks("unread", got, f.b)
	has("unread", got, f.a)
	has("unread", got, f.c)

	// Search-shaped: the index named every message; Postgres must still drop
	// the deleted ones. This is what makes `-is:trashed` and plain text search
	// correct without touching the index's own trashed flag.
	got, _ = run("search candidates", EmailQuery{RestrictIDs: []ulid.ULID{f.a, f.b, f.c, f.d}})
	lacks("search candidates", got, f.b)
	has("search candidates", got, f.c)
	has("search candidates", got, f.a)
	lacks("search candidates", got, f.d)

	// The Inbox keeps the restored message: its move outranks the Trash copy.
	inbox := f.inbox
	got, _ = run("inbox", EmailQuery{InMailbox: &inbox})
	has("inbox", got, f.c)
	has("inbox", got, f.a)
	lacks("inbox", got, f.b)

	// The Trash view still lists B, and not the restored C.
	trash := f.trash
	got, _ = run("trash view", EmailQuery{InMailbox: &trash})
	has("trash view", got, f.b)
	if got[f.c] {
		t.Errorf("trash view: returned %s, which the user moved out of Trash", f.name(f.c))
	}

	// is:trashed is the exact complement over this fixture.
	got, total = run("is:trashed", EmailQuery{OnlyTrashed: true})
	has("is:trashed", got, f.b)
	has("is:trashed", got, f.d)
	if got[f.a] || got[f.c] {
		t.Errorf("is:trashed: returned a live message: %v", got)
	}
	if total != 2 {
		t.Errorf("is:trashed: total = %d, want 2 (B and D)", total)
	}
}

// A collapsed list picks each thread's first message among the ones it can
// see. B is the newer message in A's thread, so a list that still saw B would
// show the thread through B — a deleted message as the conversation's face —
// or count the thread with no visible message in it.
func TestDeletedMailCollapsedThreadShowsTheLiveMessage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.appStore()
	f := seedTrashFolderFixture(t, h)

	if err := st.UserTx(ctx, f.user, func(tx *UserTx) error {
		ids, total, err := tx.QueryEmails(ctx, EmailQuery{CollapseThreads: true})
		if err != nil {
			return err
		}
		got := make(map[ulid.ULID]bool, len(ids))
		for _, id := range ids {
			got[id] = true
		}
		if got[f.b] {
			t.Errorf("collapsed all mail: the A/B thread is represented by %s", f.name(f.b))
		}
		if !got[f.a] {
			t.Errorf("collapsed all mail: the A/B thread is missing its live message %s", f.name(f.a))
		}
		if !got[f.c] {
			t.Errorf("collapsed all mail: missing %s", f.name(f.c))
		}
		if len(ids) != 2 || total != 2 {
			t.Errorf("collapsed all mail: %d ids, total %d; want 2 and 2 (the A/B thread once, C once)", len(ids), total)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A rule's blast-radius preview — and induction, apply-rules and the kind sweep,
// which read the same candidates — must count the mail the list views show. Its
// doc promised "trashed messages are excluded" while testing trashed_at alone,
// so B was a candidate and in the denominator. The numerator and denominator
// share one predicate; this pins both.
func TestDeletedMailExcludedFromRulePreview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.appStore()
	f := seedTrashFolderFixture(t, h)

	var cands []RulePreviewCandidate
	var total int
	if err := st.UserTx(ctx, f.user, func(tx *UserTx) error {
		var err error
		if cands, err = tx.RulePreviewCandidates(ctx, 500); err != nil {
			return err
		}
		total, err = tx.CountPreviewableMessages(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := make(map[ulid.ULID]bool, len(cands))
	for _, c := range cands {
		got[c.ID] = true
	}
	for _, id := range []ulid.ULID{f.b, f.d} {
		if got[id] {
			t.Errorf("rule preview candidates: returned %s, which is deleted", f.name(id))
		}
	}
	for _, id := range []ulid.ULID{f.a, f.c} {
		if !got[id] {
			t.Errorf("rule preview candidates: missing %s", f.name(id))
		}
	}
	if total != 2 {
		t.Errorf("CountPreviewableMessages = %d, want 2 (A and C) — the denominator counts deleted mail", total)
	}
}

// Thread/get must not hand back what the list excludes: the reading pane
// renders a thread's emailIds, so B left in them would resurrect it. And it
// must keep what the list keeps: C, restored, stays in its thread.
func TestDeletedMailThreadMembers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.appStore()
	f := seedTrashFolderFixture(t, h)

	var views []ThreadView
	if err := st.UserTx(ctx, f.user, func(tx *UserTx) error {
		var err error
		views, err = tx.GetThreads(ctx, []ulid.ULID{f.threadAB, f.thrdC})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	byID := make(map[ulid.ULID][]ulid.ULID, len(views))
	for _, v := range views {
		byID[v.ID] = v.EmailIDs
	}
	if got := byID[f.threadAB]; len(got) != 1 || got[0] != f.a {
		t.Errorf("A/B thread emailIds = %v, want exactly [%s]: %s came back through Thread/get",
			got, f.a, f.name(f.b))
	}
	if got := byID[f.thrdC]; len(got) != 1 || got[0] != f.c {
		t.Errorf("C's thread emailIds = %v, want exactly [%s]: %s was dropped from its thread",
			got, f.c, f.name(f.c))
	}
}
