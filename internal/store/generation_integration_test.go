// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the UIDVALIDITY roll (reaper.go
// ReconcileRetiredGeneration, mailroom#88) against a real PostgreSQL with RLS
// enforced, connecting as mailroom_app.
//
// Every test here is written around ONE question, the same one #86 was written
// around and the only one that matters: WHEN IS MAILROOM ALLOWED TO CONCLUDE THAT
// A MESSAGE IS GONE? A roll is followed by a re-fetch, and during that re-fetch
// every message in the mailbox looks exactly like a message that has vanished —
// nothing but stale rows. So the cases that must NOT conclude anything (an
// incomplete re-fetch, a message that came back, a message the user trashed, a
// message with a canonical placement, a message that lives in another folder too)
// get more attention here than the case that must.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// reconcile runs ReconcileRetiredGeneration and fails the test on error.
func (f *reapFixture) reconcile(t *testing.T, obs GenerationObservation) GenerationResult {
	t.Helper()
	var res GenerationResult
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var err error
		res, err = tx.ReconcileRetiredGeneration(context.Background(), obs)
		return err
	}); err != nil {
		t.Fatalf("reconcile retired generation: %v", err)
	}
	return res
}

// reconcileErr runs ReconcileRetiredGeneration and returns the error (or nil).
func (f *reapFixture) reconcileErr(obs GenerationObservation) error {
	return f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		_, err := tx.ReconcileRetiredGeneration(context.Background(), obs)
		return err
	})
}

// provenanceCount counts a message's provenance rows in a given state.
func (f *reapFixture) provenanceCount(t *testing.T, msg ulid.ULID, state string) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.message_provenance
		  WHERE user_id = $1 AND message_id = $2 AND state = $3::app.provenance_state`,
		f.userID.String(), msg.String(), state).Scan(&n); err != nil {
		t.Fatalf("provenance count: %v", err)
	}
	return n
}

// THE HEADLINE (#88). A mailbox is recreated on the server. A message that is no
// longer in it must not silently cease to exist: it is soft-deleted, retractably,
// and the change stream carries exactly one row saying so.
func TestReconcileRetiredGenerationSoftDeletesTheOrphan(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "gone when the mailbox came back")
	// The old generation, retired by MarkMailboxProvenanceStale.
	f.seedProvenance(t, msg, f.inbox, 7, 42, "stale_uidvalidity")
	before := f.emailChanges(t, msg)

	// The new generation is empty: the mailbox was recreated with nothing in it.
	// UIDNEXT 1 is the server corroborating that (no UID has ever been issued).
	res := f.reconcile(t, GenerationObservation{
		MailboxID: f.inbox, UIDValidity: 43, ObservedUIDs: nil, UIDBound: 1,
	})

	if res.ProvenanceDropped != 1 || res.MembershipChanged != 1 || res.MessagesVanished != 1 {
		t.Fatalf("result = %+v, want 1 dropped / 1 changed / 1 vanished", res)
	}
	s := f.msgState(t, msg)
	if !s.trashed || !s.vanished || s.purgeAfter == nil {
		t.Fatalf("the orphan was not soft-deleted: %+v — a message that vanishes with no trace is the bug", s)
	}
	if got := f.emailChanges(t, msg) - before; got != 1 {
		t.Errorf("Email change_log rows = %d, want exactly 1 (the invariant)", got)
	}
	if n := f.provenanceCount(t, msg, "stale_uidvalidity"); n != 0 {
		t.Errorf("%d retired provenance rows survived the reconcile", n)
	}
}

// A message that IS still there after the roll — re-fetched under the new
// generation, deduped onto the same canonical message by content hash — is
// untouched. Nothing is soft-deleted and NOT ONE change_log row is emitted by the
// reconcile: its effective membership never moved, and a change row would be a lie.
func TestReconcileRetiredGenerationLeavesTheSurvivorAlone(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "still there after the roll")
	f.seedProvenance(t, msg, f.inbox, 7, 42, "stale_uidvalidity") // old generation
	f.seedProvenance(t, msg, f.inbox, 3, 43, "live")              // re-fetched under the new one
	before := f.emailChanges(t, msg)

	res := f.reconcile(t, GenerationObservation{
		MailboxID: f.inbox, UIDValidity: 43, ObservedUIDs: []int64{3}, UIDBound: 4,
	})

	if res.ProvenanceDropped != 1 {
		t.Errorf("ProvenanceDropped = %d, want 1 (the retired coordinate)", res.ProvenanceDropped)
	}
	if res.MembershipChanged != 0 || res.MessagesVanished != 0 {
		t.Fatalf("result = %+v: a message the server still has was treated as a membership change", res)
	}
	if got := f.emailChanges(t, msg) - before; got != 0 {
		t.Errorf("Email change_log rows = %d, want 0 — nothing about this message changed", got)
	}
	if s := f.msgState(t, msg); s.trashed {
		t.Fatal("SOFT-DELETED A MESSAGE THAT IS SITTING IN THE RECREATED MAILBOX")
	}
	if got := f.liveMailboxes(t, msg); len(got) != 1 || got[0] != f.inbox.String() {
		t.Errorf("live mailboxes = %v, want {INBOX}", got)
	}
}

// AN INCOMPLETE RE-FETCH CONCLUDES NOTHING. This is the mail-destroying case: the
// FETCH returned without error but silently did not bring back everything the
// server holds. Our provenance then looks exactly as it would if those messages
// had been deleted. The completeness proof is what tells the two apart, and it
// must refuse.
func TestReconcileRetiredGenerationRefusesAnIncompleteRefetch(t *testing.T) {
	f := newReapFixture(t)
	fetched := f.seedMessage(t, "the re-fetch brought this one back")
	dropped := f.seedMessage(t, "the re-fetch silently missed this one")
	f.seedProvenance(t, fetched, f.inbox, 7, 42, "stale_uidvalidity")
	f.seedProvenance(t, dropped, f.inbox, 8, 42, "stale_uidvalidity")
	f.seedProvenance(t, fetched, f.inbox, 3, 43, "live") // only ONE of the two landed

	// The server says the new generation holds UID 3 AND UID 4. We have no live
	// row for 4 — so the re-fetch is not complete, and nothing it failed to bring
	// back can be concluded absent.
	err := f.reconcileErr(GenerationObservation{
		MailboxID: f.inbox, UIDValidity: 43, ObservedUIDs: []int64{3, 4}, UIDBound: 5,
	})
	if err == nil {
		t.Fatal("RECONCILED ON AN INCOMPLETE RE-FETCH — this soft-deletes mail that is still on the server")
	}

	if s := f.msgState(t, dropped); s.trashed || s.vanished {
		t.Fatalf("SOFT-DELETED A MESSAGE BECAUSE OUR OWN FETCH MISSED IT: %+v", s)
	}
	if s := f.msgState(t, fetched); s.trashed {
		t.Fatalf("soft-deleted a re-fetched message: %+v", s)
	}
	// The whole transaction rolled back: the retired rows are still retired, so
	// the roll is still pending and the next pass retries it.
	if n := f.provenanceCount(t, dropped, "stale_uidvalidity"); n != 1 {
		t.Errorf("retired rows for the missed message = %d, want 1 (still pending)", n)
	}
	if n := f.provenanceCount(t, fetched, "stale_uidvalidity"); n != 1 {
		t.Errorf("retired rows for the fetched message = %d, want 1 (nothing was committed)", n)
	}
	if got := f.emailChanges(t, dropped); got != 0 {
		t.Errorf("a refused reconcile still wrote %d change_log rows", got)
	}
}

// A message that ALSO lives in another folder is not "gone" — it left one folder,
// which is a membership change (one change_log row) and nothing more. This is the
// other-client-moved-it-out-of-the-rolled-mailbox case.
func TestReconcileRetiredGenerationMovedMessageIsNotDeleted(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "moved to Archive before the roll")
	f.seedProvenance(t, msg, f.inbox, 7, 42, "stale_uidvalidity")
	f.seedProvenance(t, msg, f.archive, 2, 90, "live")
	before := f.emailChanges(t, msg)

	res := f.reconcile(t, GenerationObservation{
		MailboxID: f.inbox, UIDValidity: 43, ObservedUIDs: nil, UIDBound: 1,
	})

	if res.MembershipChanged != 1 {
		t.Errorf("MembershipChanged = %d, want 1 (it left the inbox)", res.MembershipChanged)
	}
	if res.MessagesVanished != 0 {
		t.Fatalf("SOFT-DELETED A MESSAGE THAT IS SITTING IN ARCHIVE: %+v", res)
	}
	if s := f.msgState(t, msg); s.trashed {
		t.Fatal("SOFT-DELETED A MESSAGE THAT IS SITTING IN ARCHIVE")
	}
	if got := f.emailChanges(t, msg) - before; got != 1 {
		t.Errorf("Email change_log rows = %d, want exactly 1", got)
	}
	if got := f.liveMailboxes(t, msg); len(got) != 1 || got[0] != f.archive.String() {
		t.Errorf("live mailboxes = %v, want {Archive}", got)
	}
}

// THE USER'S OWN DELETE IS NOT OURS TO TOUCH. A message the user trashed keeps its
// original trashed_at/purge_after (re-stamping would silently extend a 30-day
// window the user already started) and never acquires vanished_at (which would
// make the IMAP Trash sync resurrect it — see RetractVanished).
func TestReconcileRetiredGenerationDoesNotDisturbAUserTrashedMessage(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()

	// (a) Trashed WITH a canonical placement — what Email/set actually does when
	// the account has a Trash folder (jmap_email.go ApplyEmailUpdate).
	placed := f.seedMessage(t, "user trashed it, placed in Trash")
	f.seedProvenance(t, placed, f.inbox, 7, 42, "stale_uidvalidity")
	if _, err := f.h.super.Exec(ctx,
		`UPDATE app.messages
		    SET trashed_at = now(), purge_after = now() + interval '30 days', moved_mailbox_id = $3
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), placed.String(), f.trash.String()); err != nil {
		t.Fatalf("user trash: %v", err)
	}

	// (b) Trashed WITHOUT a placement (no Trash folder in the account to place it
	// in): the message carries trashed_at and nothing else, which is the shape the
	// soft-delete guard has to defend against on its own.
	bare := f.seedMessage(t, "user trashed it, nowhere to place it")
	f.seedProvenance(t, bare, f.inbox, 8, 42, "stale_uidvalidity")
	if _, err := f.h.super.Exec(ctx,
		`UPDATE app.messages SET trashed_at = now(), purge_after = now() + interval '30 days'
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), bare.String()); err != nil {
		t.Fatalf("user trash: %v", err)
	}

	beforePlaced, beforeBare := f.msgState(t, placed), f.msgState(t, bare)
	changesPlaced, changesBare := f.emailChanges(t, placed), f.emailChanges(t, bare)

	res := f.reconcile(t, GenerationObservation{
		MailboxID: f.inbox, UIDValidity: 43, ObservedUIDs: nil, UIDBound: 1,
	})
	if res.MessagesVanished != 0 {
		t.Fatalf("RE-TRASHED A MESSAGE THE USER DELETED: %+v", res)
	}

	for _, c := range []struct {
		name    string
		id      ulid.ULID
		before  msgState
		changes int
		// A placed message reads its membership from moved_mailbox_id, so dropping
		// a provenance row changes nothing a client can see — and says nothing.
		wantChanges int
	}{
		{"placed", placed, beforePlaced, changesPlaced, 0},
		{"bare", bare, beforeBare, changesBare, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := f.msgState(t, c.id)
			if !s.trashed {
				t.Fatal("the user's delete was undone")
			}
			if s.vanished {
				t.Fatal("STAMPED vanished_at ON A MESSAGE THE USER DELETED — the IMAP Trash sync will now resurrect it")
			}
			if s.purgeAfter == nil || !s.purgeAfter.Equal(*c.before.purgeAfter) {
				t.Errorf("purge_after moved from %v to %v — the user's 30-day window was silently restarted",
					c.before.purgeAfter, s.purgeAfter)
			}
			if got := f.emailChanges(t, c.id) - c.changes; got != c.wantChanges {
				t.Errorf("Email change_log rows = %d, want %d", got, c.wantChanges)
			}
		})
	}
}

// Fail closed on an observation that cannot justify what the re-fetch covered.
func TestReconcileRetiredGenerationRefusesUnjustifiableObservations(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "safe")
	f.seedProvenance(t, msg, f.inbox, 7, 42, "stale_uidvalidity")

	cases := map[string]GenerationObservation{
		"no bound":         {MailboxID: f.inbox, UIDValidity: 43, UIDBound: 0},
		"negative bound":   {MailboxID: f.inbox, UIDValidity: 43, UIDBound: -1},
		"zero uidvalidity": {MailboxID: f.inbox, UIDValidity: 0, UIDBound: 1},
		"no mailbox":       {UIDValidity: 43, UIDBound: 1},
	}
	for name, obs := range cases {
		t.Run(name, func(t *testing.T) {
			if err := f.reconcileErr(obs); err == nil {
				t.Fatal("accepted an observation it cannot justify")
			}
			if n := f.provenanceCount(t, msg, "stale_uidvalidity"); n != 1 {
				t.Fatal("a refused observation destroyed provenance")
			}
			if s := f.msgState(t, msg); s.trashed {
				t.Fatal("a refused observation soft-deleted mail")
			}
		})
	}
}

// A reconcile in one user's transaction cannot touch another user's rows: RLS,
// not a WHERE clause, is what enforces that.
func TestReconcileRetiredGenerationIsScopedByRLS(t *testing.T) {
	f := newReapFixture(t)
	mine := f.seedMessage(t, "mine")
	f.seedProvenance(t, mine, f.inbox, 7, 42, "stale_uidvalidity")

	var res GenerationResult
	if err := f.st.UserTx(context.Background(), f.otherID, func(tx *UserTx) error {
		var err error
		res, err = tx.ReconcileRetiredGeneration(context.Background(), GenerationObservation{
			MailboxID: f.inbox, UIDValidity: 43, UIDBound: 1,
		})
		return err
	}); err != nil {
		t.Fatalf("reconcile as other user: %v", err)
	}
	if res.ProvenanceDropped != 0 {
		t.Fatalf("another user's reconcile dropped %d of my provenance rows", res.ProvenanceDropped)
	}
	if n := f.provenanceCount(t, mine, "stale_uidvalidity"); n != 1 {
		t.Fatal("RLS did not protect my provenance from another user's reconcile")
	}
	if s := f.msgState(t, mine); s.trashed {
		t.Fatal("another user's reconcile soft-deleted my mail")
	}
}

// THE FULL LOOP, AND THE REASON THE RETIRED ROWS ARE DELETED RATHER THAN KEPT.
// A mailbox is recreated (roll), its orphan is soft-deleted — and then the mailbox
// is RESTORED FROM BACKUP and its UIDVALIDITY reverts to the value it had before.
// The server serves the old coordinate again, which is proof the soft-delete was
// wrong: because the retired row is GONE, re-ingesting that coordinate INSERTs a
// fresh live row, which is a NEW live location, which retracts the soft-delete and
// owes a change_log row. The message comes back.
func TestReconcileThenUIDValidityRevertBringsTheMessageBack(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "the mailbox was restored from backup")
	f.seedProvenance(t, msg, f.inbox, 7, 42, "stale_uidvalidity")

	f.reconcile(t, GenerationObservation{
		MailboxID: f.inbox, UIDValidity: 43, ObservedUIDs: nil, UIDBound: 1,
	})
	if s := f.msgState(t, msg); !s.trashed || !s.vanished {
		t.Fatalf("precondition: the orphan was not soft-deleted: %+v", s)
	}

	// The backup restore puts UIDVALIDITY 42 back, and the poll re-ingests UID 7.
	var nowLive, retracted bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		if nowLive, err = tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: msg, AccountID: f.accountID, MailboxID: f.inbox,
			UID: 7, UIDValidity: 42, RemoteFlags: []string{},
		}); err != nil {
			return err
		}
		if !nowLive {
			return nil
		}
		retracted, err = tx.RetractVanished(ctx, msg)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if !nowLive {
		t.Fatal("re-ingesting the restored coordinate did not report a new live location — the retraction never fires and the message stays trashed forever")
	}
	if !retracted {
		t.Fatal("the soft-delete was not retracted")
	}
	if s := f.msgState(t, msg); s.trashed || s.vanished || s.purgeAfter != nil {
		t.Errorf("state = %+v, want fully un-deleted", s)
	}
	if got := f.liveMailboxes(t, msg); len(got) != 1 || got[0] != f.inbox.String() {
		t.Errorf("live mailboxes = %v, want {INBOX}", got)
	}
}
