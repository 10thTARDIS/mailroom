// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for EXPUNGE/VANISHED reconciliation (reaper.go, #86)
// against a real PostgreSQL with RLS enforced, connecting as mailroom_app —
// exactly the role and policies production runs under.
//
// The tests are written around one question: when is Mailroom allowed to
// conclude that a message is GONE? So the cases that must NOT reap (a UID above
// the observation bound, another generation, another mailbox, another user, a
// message with a canonical placement) get at least as much attention as the ones
// that must.

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

type reapFixture struct {
	h         *harness
	st        *Store
	userID    ulid.ULID
	otherID   ulid.ULID
	accountID ulid.ULID
	inbox     ulid.ULID
	archive   ulid.ULID
	trash     ulid.ULID
}

func newReapFixture(t *testing.T) *reapFixture {
	t.Helper()
	h := newHarness(t)
	f := &reapFixture{
		h: h, st: h.appStore(),
		userID: NewULID(), otherID: NewULID(), accountID: NewULID(),
		inbox: NewULID(), archive: NewULID(), trash: NewULID(),
	}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, u := range []ulid.ULID{f.userID, f.otherID} {
		exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
			u.String(), "sub-"+u.String(), u.String()+"@example.com")
	}
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		f.accountID.String(), f.userID.String(), NewULID().String())
	for _, m := range []struct {
		id   ulid.ULID
		name string
		role string
	}{{f.inbox, "INBOX", ""}, {f.archive, "Archive", `\Archive`}, {f.trash, "Trash", `\Trash`}} {
		exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		      VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''))`,
			m.id.String(), f.userID.String(), f.accountID.String(), m.name, m.role)
	}
	return f
}

// seedMessage inserts a canonical message (plus its blob) and returns its id.
func (f *reapFixture) seedMessage(t *testing.T, subject string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	threadID := NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), hash[:], NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
	      VALUES ($1, $2, $3, $4, $5, now(), 10)`,
		id.String(), f.userID.String(), hash[:], threadID.String(), subject)
	return id
}

// seedProvenance places a message in a mailbox at a (uidvalidity, uid).
func (f *reapFixture) seedProvenance(t *testing.T, msg, mailbox ulid.ULID, uid, uidValidity int64, state string) {
	t.Helper()
	if _, err := f.h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::app.provenance_state)`,
		NewULID().String(), f.userID.String(), msg.String(), f.accountID.String(),
		mailbox.String(), uid, uidValidity, state); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
}

// liveMailboxes returns the mailbox ids a message currently has live provenance
// in, read as the superuser (assertions look at ground truth, not through RLS).
func (f *reapFixture) liveMailboxes(t *testing.T, msg ulid.ULID) []string {
	t.Helper()
	rows, err := f.h.super.Query(context.Background(),
		`SELECT mailbox_id FROM app.message_provenance
		  WHERE user_id = $1 AND message_id = $2 AND state = 'live' ORDER BY mailbox_id`,
		f.userID.String(), msg.String())
	if err != nil {
		t.Fatalf("live mailboxes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// emailChanges counts the Email change_log rows for a message.
func (f *reapFixture) emailChanges(t *testing.T, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("change count: %v", err)
	}
	return n
}

type msgState struct {
	trashed    bool
	purgeAfter *time.Time
	vanished   bool
	moved      string
}

func (f *reapFixture) msgState(t *testing.T, msg ulid.ULID) msgState {
	t.Helper()
	var s msgState
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT trashed_at IS NOT NULL, purge_after, vanished_at IS NOT NULL,
		        COALESCE(moved_mailbox_id, '')
		   FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&s.trashed, &s.purgeAfter, &s.vanished, &s.moved); err != nil {
		t.Fatalf("message state: %v", err)
	}
	return s
}

func (f *reapFixture) reap(t *testing.T, obs ReapObservation) ReapResult {
	t.Helper()
	var res ReapResult
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var err error
		res, err = tx.ReapMailbox(context.Background(), obs)
		return err
	}); err != nil {
		t.Fatalf("reap: %v", err)
	}
	return res
}

// THE HEADLINE. A message the server no longer has leaves its Mailroom folder,
// gets its change_log row, and — being nowhere else — is soft-deleted.
func TestReapMailboxDropsVanishedAndSoftDeletes(t *testing.T) {
	f := newReapFixture(t)
	kept := f.seedMessage(t, "still there")
	gone := f.seedMessage(t, "deleted on the phone")
	f.seedProvenance(t, kept, f.inbox, 1, 42, "live")
	f.seedProvenance(t, gone, f.inbox, 2, 42, "live")

	before := f.emailChanges(t, gone)
	res := f.reap(t, ReapObservation{
		MailboxID: f.inbox, UIDValidity: 42,
		ObservedUIDs: []int64{1}, // the server no longer lists UID 2
		UIDBound:     3,          // its UIDNEXT
	})

	if res.ProvenanceDropped != 1 || res.MembershipChanged != 1 || res.MessagesVanished != 1 {
		t.Fatalf("result = %+v, want 1/1/1", res)
	}
	if got := f.liveMailboxes(t, gone); len(got) != 0 {
		t.Errorf("expunged message still has live provenance in %v", got)
	}
	if got := f.liveMailboxes(t, kept); len(got) != 1 || got[0] != f.inbox.String() {
		t.Errorf("the message the server still has was reaped: %v", got)
	}
	// Soft-deleted, never hard-deleted (ADR-009), and the inference is recorded
	// as retractable.
	st := f.msgState(t, gone)
	if !st.trashed || !st.vanished || st.purgeAfter == nil {
		t.Errorf("vanished message state = %+v, want trashed + vanished + purge_after", st)
	}
	if st.purgeAfter.Before(time.Now().Add(29 * 24 * time.Hour)) {
		t.Errorf("purge_after = %v, want ~30 days out", st.purgeAfter)
	}
	// Exactly one Email change_log row — the invariant, in the same transaction.
	if got := f.emailChanges(t, gone) - before; got != 1 {
		t.Errorf("Email change_log rows appended = %d, want exactly 1", got)
	}
	if got := f.emailChanges(t, kept); got != 0 {
		t.Errorf("appended %d change rows for a message that did not move", got)
	}
}

// THE RACE BOUND. A provenance row written between the server's answer and this
// transaction sits ABOVE the observed UID space and must survive — otherwise a
// message that arrives during the reap is deleted the moment it lands.
func TestReapMailboxNeverReapsAboveTheBound(t *testing.T) {
	f := newReapFixture(t)
	arrivedDuringTheReap := f.seedMessage(t, "landed mid-reap")
	f.seedProvenance(t, arrivedDuringTheReap, f.inbox, 7, 42, "live")

	res := f.reap(t, ReapObservation{
		MailboxID: f.inbox, UIDValidity: 42,
		ObservedUIDs: []int64{1, 2},
		UIDBound:     5, // the server's UIDNEXT when it answered; UID 7 did not exist yet
	})
	if res.ProvenanceDropped != 0 {
		t.Fatalf("reaped %d rows above the observation bound", res.ProvenanceDropped)
	}
	if got := f.liveMailboxes(t, arrivedDuringTheReap); len(got) != 1 {
		t.Fatal("a message ingested concurrently with the reap was deleted by it")
	}
	if f.msgState(t, arrivedDuringTheReap).trashed {
		t.Fatal("soft-deleted a message the observation never covered")
	}
}

// A UID is meaningless outside its generation. Rows in another UIDVALIDITY, or
// already retired to 'stale_uidvalidity', are not in this observation's space.
func TestReapMailboxIgnoresOtherGenerationsAndMailboxes(t *testing.T) {
	f := newReapFixture(t)
	oldGen := f.seedMessage(t, "old generation")
	stale := f.seedMessage(t, "already retired")
	elsewhere := f.seedMessage(t, "another folder")
	f.seedProvenance(t, oldGen, f.inbox, 3, 41, "live") // different uidvalidity
	f.seedProvenance(t, stale, f.inbox, 4, 42, "stale_uidvalidity")
	f.seedProvenance(t, elsewhere, f.archive, 3, 42, "live") // different mailbox

	res := f.reap(t, ReapObservation{
		MailboxID: f.inbox, UIDValidity: 42, ObservedUIDs: nil, UIDBound: 100,
	})
	if res.ProvenanceDropped != 0 {
		t.Fatalf("dropped %d rows outside the observed coordinate space", res.ProvenanceDropped)
	}
	for name, m := range map[string]ulid.ULID{"old generation": oldGen, "stale": stale, "elsewhere": elsewhere} {
		if len(f.liveMailboxes(t, m)) == 0 && name != "stale" {
			t.Errorf("%s lost its provenance", name)
		}
		if f.msgState(t, m).trashed {
			t.Errorf("%s was soft-deleted", name)
		}
	}
}

// A message that MOVED to another folder is not a message that VANISHED. Its
// INBOX row goes; its Archive row stays; it is emphatically not trashed.
func TestReapMailboxMovedMessageIsNotDeleted(t *testing.T) {
	f := newReapFixture(t)
	moved := f.seedMessage(t, "archived on the phone")
	f.seedProvenance(t, moved, f.inbox, 5, 42, "live")   // stale: the phone moved it out
	f.seedProvenance(t, moved, f.archive, 9, 77, "live") // where it actually is now

	res := f.reap(t, ReapObservation{
		MailboxID: f.inbox, UIDValidity: 42, ObservedUIDs: []int64{}, UIDBound: 6,
	})
	if res.ProvenanceDropped != 1 || res.MembershipChanged != 1 {
		t.Fatalf("result = %+v, want 1 dropped / 1 membership change", res)
	}
	if res.MessagesVanished != 0 {
		t.Fatal("soft-deleted a message that is sitting in Archive")
	}
	if got := f.liveMailboxes(t, moved); len(got) != 1 || got[0] != f.archive.String() {
		t.Errorf("membership = %v, want exactly {Archive}", got)
	}
	if f.msgState(t, moved).trashed {
		t.Fatal("trashed a message that merely moved folders")
	}
	if got := f.emailChanges(t, moved); got != 1 {
		t.Errorf("Email change rows = %d, want 1 (the membership change)", got)
	}
}

// A message with a CANONICAL PLACEMENT reads its membership from that column,
// not from provenance — so dropping its stale source row changes nothing a
// client can see, and must NOT produce a change_log row, and must NOT trash it
// even when it is the last provenance the message had.
func TestReapMailboxCanonicalPlacementIsInvisibleToTheReaper(t *testing.T) {
	f := newReapFixture(t)
	archived := f.seedMessage(t, "user archived it here")
	f.seedProvenance(t, archived, f.inbox, 8, 42, "live")
	if _, err := f.h.super.Exec(context.Background(),
		`UPDATE app.messages SET moved_mailbox_id = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), archived.String(), f.archive.String()); err != nil {
		t.Fatalf("set placement: %v", err)
	}

	res := f.reap(t, ReapObservation{
		MailboxID: f.inbox, UIDValidity: 42, ObservedUIDs: []int64{}, UIDBound: 9,
	})
	if res.ProvenanceDropped != 1 {
		t.Fatalf("dropped = %d, want 1", res.ProvenanceDropped)
	}
	if res.MembershipChanged != 0 || res.MessagesVanished != 0 {
		t.Fatalf("result = %+v: the placement overrides provenance, so nothing visible changed", res)
	}
	if f.msgState(t, archived).trashed {
		t.Fatal("soft-deleted a message the user had explicitly placed in Archive")
	}
	if got := f.emailChanges(t, archived); got != 0 {
		t.Errorf("appended %d change rows for an invisible mutation", got)
	}
}

// RLS. A reap run as one user cannot touch another user's provenance, even for
// the same mailbox id — the policy, not the WHERE clause, is what proves it.
func TestReapMailboxIsUserScoped(t *testing.T) {
	f := newReapFixture(t)
	mine := f.seedMessage(t, "mine")
	f.seedProvenance(t, mine, f.inbox, 1, 42, "live")

	// The other user cannot see, and therefore cannot reap, this mailbox at all.
	var res ReapResult
	if err := f.st.UserTx(context.Background(), f.otherID, func(tx *UserTx) error {
		var err error
		res, err = tx.ReapMailbox(context.Background(), ReapObservation{
			MailboxID: f.inbox, UIDValidity: 42, ObservedUIDs: []int64{}, UIDBound: 100,
		})
		return err
	}); err != nil {
		t.Fatalf("reap as other user: %v", err)
	}
	if res.ProvenanceDropped != 0 {
		t.Fatalf("another user's reap dropped %d of my provenance rows", res.ProvenanceDropped)
	}
	if got := f.liveMailboxes(t, mine); len(got) != 1 {
		t.Fatal("RLS did not protect my provenance from another user's reap")
	}
}

// Fail closed on an observation that cannot justify itself.
func TestReapMailboxRefusesUnjustifiableObservations(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "safe")
	f.seedProvenance(t, msg, f.inbox, 1, 42, "live")

	cases := map[string]ReapObservation{
		"zero uidvalidity": {MailboxID: f.inbox, UIDValidity: 0, UIDBound: 100},
		"no mailbox":       {UIDValidity: 42, UIDBound: 100},
	}
	for name, obs := range cases {
		t.Run(name, func(t *testing.T) {
			err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
				_, err := tx.ReapMailbox(context.Background(), obs)
				return err
			})
			if err == nil {
				t.Fatal("accepted an observation it cannot justify")
			}
			if got := f.liveMailboxes(t, msg); len(got) != 1 {
				t.Fatal("provenance was destroyed by a refused observation")
			}
		})
	}

	// A zero bound is not an error, just a no-op: nothing can be below UID 1.
	res := f.reap(t, ReapObservation{MailboxID: f.inbox, UIDValidity: 42, UIDBound: 1})
	if res.ProvenanceDropped != 0 || len(f.liveMailboxes(t, msg)) != 1 {
		t.Fatal("an empty UID space reaped something")
	}
}

// The reaper's soft-delete is RETRACTABLE; the user's is not. This is the whole
// reason vanished_at exists.
func TestRetractVanished(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()

	reaped := f.seedMessage(t, "reaper thought it was gone")
	f.seedProvenance(t, reaped, f.inbox, 1, 42, "live")
	f.reap(t, ReapObservation{MailboxID: f.inbox, UIDValidity: 42, UIDBound: 2})
	if s := f.msgState(t, reaped); !s.trashed || !s.vanished {
		t.Fatalf("precondition: %+v", s)
	}

	userTrashed := f.seedMessage(t, "user deleted it")
	if _, err := f.h.super.Exec(ctx,
		`UPDATE app.messages SET trashed_at = now(), purge_after = now() + interval '30 days'
		  WHERE user_id = $1 AND id = $2`, f.userID.String(), userTrashed.String()); err != nil {
		t.Fatalf("user trash: %v", err)
	}

	var retractedReaped, retractedUser bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		if retractedReaped, err = tx.RetractVanished(ctx, reaped); err != nil {
			return err
		}
		retractedUser, err = tx.RetractVanished(ctx, userTrashed)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if !retractedReaped {
		t.Error("the reaper's own conclusion was not retractable")
	}
	if s := f.msgState(t, reaped); s.trashed || s.vanished || s.purgeAfter != nil {
		t.Errorf("retracted message state = %+v, want fully un-deleted", s)
	}
	if retractedUser {
		t.Fatal("RESURRECTED A MESSAGE THE USER DELETED — vanished_at must gate this")
	}
	if s := f.msgState(t, userTrashed); !s.trashed {
		t.Fatal("the user's trash was undone")
	}
}

// UpsertProvenance's conflict path resets state to 'live': a row retired by a
// UIDVALIDITY roll can come back when the server hands us the coordinate again.
//
// And it reports that as a NEW LIVE LOCATION (#88). A stale row is invisible to
// every client — membership and the flag merge both filter state='live' — so
// reviving one puts the message back in a folder it was not in a moment ago. That
// is a JMAP-visible mutation and it owes a change_log row; reporting false here
// (as this did before) meant the message silently reappeared, with no /changes row
// and no search re-index, and any standing reaper soft-delete was never retracted.
func TestUpsertProvenanceRevivingAStaleRowIsANewLiveLocation(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "restored from backup")
	f.seedProvenance(t, msg, f.inbox, 3, 42, "stale_uidvalidity")

	var revived, repeat bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		if revived, err = tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: msg, AccountID: f.accountID, MailboxID: f.inbox,
			UID: 3, UIDValidity: 42, RemoteFlags: []string{`\Seen`},
		}); err != nil {
			return err
		}
		// A repeat sight of the now-live row is an ordinary poll: the LOCATION did
		// not change, so it must report false or every poll would spam the change
		// stream.
		repeat, err = tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: msg, AccountID: f.accountID, MailboxID: f.inbox,
			UID: 3, UIDValidity: 42, RemoteFlags: []string{`\Seen`, `\Flagged`},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !revived {
		t.Error("reviving a retired row was not reported as a new live location — the message reappears with no change_log row")
	}
	if repeat {
		t.Error("re-seeing an already-live row reported a new live location — every poll would emit a change")
	}
	if got := f.liveMailboxes(t, msg); len(got) != 1 || got[0] != f.inbox.String() {
		t.Fatalf("stale row was not revived to 'live': %v", got)
	}
}

// RETIRING THE CANONICAL PLACEMENT (#84 -> #86). The override may only be cleared
// once the reaper has proved it redundant, and clearing it must be invisible.
func TestRetireCanonicalPlacements(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()

	// (a) Landed: the writeback move applied, Archive provenance exists, and the
	//     reaper has removed the INBOX row. The override has done its job.
	landed := f.seedMessage(t, "archived and settled")
	f.seedProvenance(t, landed, f.archive, 9, 77, "live")

	// (b) In flight: the move has not landed yet — the source row is still there.
	//     Clearing the override HERE is exactly what would bounce the message back
	//     into the inbox, so it must not happen.
	inFlight := f.seedMessage(t, "archive still in flight")
	f.seedProvenance(t, inFlight, f.inbox, 4, 42, "live")
	f.seedProvenance(t, inFlight, f.archive, 10, 77, "live")

	// (c) Unconfirmed: no provenance at the destination at all (e.g. Archive is
	//     sync_scope='off'). The override is the ONLY thing keeping the message
	//     visible; retiring it would make the message vanish from every folder.
	unconfirmed := f.seedMessage(t, "moved into an unsynced folder")

	for _, m := range []ulid.ULID{landed, inFlight, unconfirmed} {
		if _, err := f.h.super.Exec(ctx,
			`UPDATE app.messages SET moved_mailbox_id = $3 WHERE user_id = $1 AND id = $2`,
			f.userID.String(), m.String(), f.archive.String()); err != nil {
			t.Fatalf("set placement: %v", err)
		}
	}

	// Membership BEFORE, through the real JMAP derivation (GetEmails, which is
	// what a client actually reads) — not a hand-rolled query that could agree
	// with the reaper and disagree with the API.
	membership := func(m ulid.ULID) []string {
		t.Helper()
		var out []string
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			views, err := tx.GetEmails(ctx, []ulid.ULID{m})
			if err != nil {
				return err
			}
			for _, v := range views {
				for _, mb := range v.MailboxIDs {
					out = append(out, mb.String())
				}
			}
			sort.Strings(out)
			return nil
		}); err != nil {
			t.Fatalf("membership: %v", err)
		}
		return out
	}
	beforeLanded := membership(landed)
	beforeInFlight := membership(inFlight)
	beforeUnconfirmed := membership(unconfirmed)

	var n int64
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		n, err = tx.RetireCanonicalPlacements(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("retired %d placements, want exactly 1 (only the landed one)", n)
	}
	if got := f.msgState(t, landed).moved; got != "" {
		t.Errorf("landed message kept its override (%s)", got)
	}
	if got := f.msgState(t, inFlight).moved; got != f.archive.String() {
		t.Error("RETIRED AN OVERRIDE WHILE THE SOURCE PROVENANCE STILL EXISTS — the message would bounce back to the inbox")
	}
	if got := f.msgState(t, unconfirmed).moved; got != f.archive.String() {
		t.Error("retired an override with no provenance at the destination — the message would be in no folder at all")
	}

	// AND THE POINT: retirement is membership-neutral. Every message reads
	// EXACTLY as it did before — that is what makes it impossible to flicker.
	for name, pair := range map[string][2][]string{
		"landed":      {beforeLanded, membership(landed)},
		"in flight":   {beforeInFlight, membership(inFlight)},
		"unconfirmed": {beforeUnconfirmed, membership(unconfirmed)},
	} {
		before, after := pair[0], pair[1]
		if len(before) != len(after) {
			t.Fatalf("%s: membership changed across retirement: %v -> %v", name, before, after)
		}
		for i := range before {
			if before[i] != after[i] {
				t.Fatalf("%s: membership changed across retirement: %v -> %v", name, before, after)
			}
		}
	}
	// The landed message still reads as being in Archive — now from provenance
	// rather than the override.
	if got := membership(landed); len(got) != 1 || got[0] != f.archive.String() {
		t.Errorf("landed message membership after retirement = %v, want {Archive}", got)
	}
	if got := f.emailChanges(t, landed); got != 0 {
		t.Errorf("retirement appended %d change_log rows for a provably invisible mutation", got)
	}
}

// StampMailboxReaped advances the cadence cursor and nothing else.
func TestStampMailboxReaped(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.StampMailboxReaped(ctx, f.inbox, nil)
	}); err != nil {
		t.Fatal(err)
	}
	var stamped *time.Time
	if err := f.h.super.QueryRow(ctx,
		`SELECT last_reaped_at FROM app.mailboxes WHERE id = $1`, f.inbox.String()).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped == nil {
		t.Fatal("last_reaped_at not advanced")
	}
	// And it comes back out through UpsertMailbox, which is how the engine reads
	// the cadence.
	var state MailboxState
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		state, err = tx.UpsertMailbox(ctx, MailboxUpsert{
			AccountID: f.accountID, IMAPName: "INBOX", DisplayName: "INBOX",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if state.LastReapedAt == nil {
		t.Fatal("UpsertMailbox did not return the reap cursor")
	}
}

// ---- #620: the skip-unchanged baselines --------------------------------------

// mailboxState reads a mailbox back the way the engine does — through
// UpsertMailbox, under RLS — so the assertions see what the next pass would see.
func (f *reapFixture) mailboxState(t *testing.T, imapName string) MailboxState {
	t.Helper()
	var st MailboxState
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var err error
		st, err = tx.UpsertMailbox(context.Background(), MailboxUpsert{
			AccountID: f.accountID, IMAPName: imapName, DisplayName: imapName,
		})
		return err
	}); err != nil {
		t.Fatalf("read mailbox state: %v", err)
	}
	return st
}

// The POLL-TIME baseline: UpdateMailboxCursors writes UIDNEXT and MESSAGES from
// the same SELECT as the existing cursors, in the same statement, and they come
// back out through UpsertMailbox. A migrated row starts at NULL (unknown), and a
// SELECT that carried no UIDNEXT stores NULL rather than a 0 that could match.
func TestMailboxPollBaselineRoundTrips(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()

	if st := f.mailboxState(t, "INBOX"); st.PollUIDNext != nil || st.PollMessages != nil ||
		st.Observed != nil || st.LastObservedFullAt != nil {
		t.Fatalf("a fresh row has baselines %+v; every one must start unknown (NULL)", st)
	}

	// Written and read back inside ONE transaction, as the engine's poll does.
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		if err := tx.UpdateMailboxCursors(ctx, f.inbox, MailboxCursors{
			UIDValidity: 7, LastSeenUID: 41, HighestModSeq: 900, UIDNext: 42, Messages: 40,
		}); err != nil {
			return err
		}
		st, err := tx.UpsertMailbox(ctx, MailboxUpsert{AccountID: f.accountID, IMAPName: "INBOX", DisplayName: "INBOX"})
		if err != nil {
			return err
		}
		if st.PollUIDNext == nil || *st.PollUIDNext != 42 || st.PollMessages == nil || *st.PollMessages != 40 {
			t.Errorf("poll baseline = (%s, %s), want (42, 40)", p64(st.PollUIDNext), p64(st.PollMessages))
		}
		if st.UIDValidity != 7 || st.LastSeenUID != 41 || st.HighestModSeq != 900 {
			t.Errorf("existing cursors = %+v, disturbed by the new columns", st)
		}
		// A cursor write is not an observation.
		if st.Observed != nil || st.LastObservedFullAt != nil {
			t.Errorf("UpdateMailboxCursors wrote an observation baseline: %+v", st.Observed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// No UIDNEXT from the SELECT: NULL, not 0. An empty mailbox's MESSAGES 0 is
	// a real zero and is kept.
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.UpdateMailboxCursors(ctx, f.inbox, MailboxCursors{UIDValidity: 7, UIDNext: 0, Messages: 0})
	}); err != nil {
		t.Fatal(err)
	}
	if st := f.mailboxState(t, "INBOX"); st.PollUIDNext != nil || st.PollMessages == nil || *st.PollMessages != 0 {
		t.Errorf("after a UIDNEXT-less SELECT: poll baseline = (%s, %s), want (NULL, 0)", p64(st.PollUIDNext), p64(st.PollMessages))
	}
}

// The OBSERVATION-TIME baseline is written only when an observation actually
// ran. A skipped observation (StampMailboxUnchanged) advances last_reaped_at and
// NOTHING else — in particular not last_observed_full_at, or the 24-hour
// backstop could be re-stamped away forever.
func TestMailboxObservationBaselineOnlyFromARealObservation(t *testing.T) {
	f := newReapFixture(t)
	ctx := context.Background()
	stamp := func(fn func(tx *UserTx) error) {
		t.Helper()
		if err := f.st.UserTx(ctx, f.userID, fn); err != nil {
			t.Fatal(err)
		}
	}
	lastReaped := func() time.Time {
		t.Helper()
		var at *time.Time
		if err := f.h.super.QueryRow(ctx, `SELECT last_reaped_at FROM app.mailboxes WHERE id = $1`,
			f.inbox.String()).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at == nil {
			t.Fatal("last_reaped_at is NULL")
		}
		return *at
	}

	// 1. A skipped observation on a never-observed mailbox: cadence only.
	stamp(func(tx *UserTx) error { return tx.StampMailboxUnchanged(ctx, f.inbox) })
	lastReaped()
	if st := f.mailboxState(t, "INBOX"); st.Observed != nil || st.LastObservedFullAt != nil {
		t.Fatalf("a SKIPPED observation wrote a baseline %+v / %v", st.Observed, st.LastObservedFullAt)
	}

	// 2. A real observation that reaped, in the reap's own transaction (the
	// adapter's shape): the tuple and the real-observation time land together.
	seen := ObservedStatus{UIDValidity: 7, UIDNext: 42, Messages: 40, HighestModSeq: 900}
	stamp(func(tx *UserTx) error {
		if _, err := tx.ReapMailbox(ctx, ReapObservation{
			MailboxID: f.inbox, UIDValidity: 7, ObservedUIDs: []int64{}, UIDBound: 42,
		}); err != nil {
			return err
		}
		return tx.StampMailboxReaped(ctx, f.inbox, &seen)
	})
	st := f.mailboxState(t, "INBOX")
	if st.Observed == nil || *st.Observed != seen {
		t.Fatalf("observed baseline = %+v, want %+v", st.Observed, seen)
	}
	if st.LastObservedFullAt == nil {
		t.Fatal("a real observation did not record last_observed_full_at")
	}

	// 3. A later skip leaves the baseline AND its time exactly as they were. The
	// time is backdated first so "unchanged" cannot pass by now() coinciding.
	old := time.Now().Add(-20 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := f.h.super.Exec(ctx, `UPDATE app.mailboxes SET last_observed_full_at = $2, last_reaped_at = $2 WHERE id = $1`,
		f.inbox.String(), old); err != nil {
		t.Fatal(err)
	}
	stamp(func(tx *UserTx) error { return tx.StampMailboxUnchanged(ctx, f.inbox) })
	st = f.mailboxState(t, "INBOX")
	if st.LastObservedFullAt == nil || !st.LastObservedFullAt.Equal(old) {
		t.Errorf("last_observed_full_at = %v after a SKIP, want it left at %v — the 24 h backstop would never fire",
			st.LastObservedFullAt, old)
	}
	if st.Observed == nil || *st.Observed != seen {
		t.Errorf("observed baseline = %+v after a SKIP, want %+v untouched", st.Observed, seen)
	}
	if !lastReaped().After(old) {
		t.Error("a skip did not advance last_reaped_at — the folder would be due again every pass")
	}

	// 4. A real observation that establishes NO baseline clears the old one, so
	// the next due observation runs for real.
	stamp(func(tx *UserTx) error { return tx.StampMailboxReaped(ctx, f.inbox, nil) })
	if st := f.mailboxState(t, "INBOX"); st.Observed != nil || st.LastObservedFullAt == nil {
		t.Errorf("after a baseline-less observation: observed = %+v, full-at = %v; want nil, set",
			st.Observed, st.LastObservedFullAt)
	}

	// 5. Atomic with the reap: a reap transaction that rolls back leaves no
	// baseline behind.
	errRollback := errors.New("roll back")
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		if err := tx.StampMailboxReaped(ctx, f.inbox, &seen); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatalf("UserTx = %v, want the rollback sentinel", err)
	}
	if st := f.mailboxState(t, "INBOX"); st.Observed != nil {
		t.Errorf("a rolled-back observation left a baseline %+v", st.Observed)
	}
}

// p64 renders a nullable bigint for a failure message: its value, or NULL.
func p64(p *int64) string {
	if p == nil {
		return "NULL"
	}
	return strconv.FormatInt(*p, 10)
}
