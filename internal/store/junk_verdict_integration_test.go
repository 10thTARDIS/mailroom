// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// #576: "Not junk" must take the provider's junk KEYWORD off the copy before the
// move carries it into the inbox, and the engine must be handed those intents
// in that order. The end-to-end proof against a real Dovecot is
// internal/mailsync's TestLiveNotJunkSurvivesTheNextSync; these pin the
// store-side decisions one at a time.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// junkFixture is fixture user a with the seeded message moved into a \Junk
// folder, carrying the given server flags, with the given keyword probe.
type junkFixture struct {
	h     *harness
	u     fixtureUser
	uid   ulid.ULID
	acct  ulid.ULID
	msg   ulid.ULID
	inbox ulid.ULID
	junk  ulid.ULID
}

func newJunkFixture(t *testing.T, flags []string, isJunk, accepts bool) *junkFixture {
	t.Helper()
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	f := &junkFixture{h: h, u: a, uid: mustULID(t, a.ID), acct: mustULID(t, a.AccountID),
		msg: mustULID(t, a.MessageID), inbox: mustULID(t, a.MailboxID)}
	f.junk = seedMailbox(t, ctx, h, a, "Junk", `\Junk`)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("junk fixture: %v", err)
		}
	}
	exec(`UPDATE app.mailboxes SET accepts_keywords = $2 WHERE user_id = $1`, a.ID, accepts)
	exec(`UPDATE app.message_provenance SET mailbox_id = $2, remote_flags = $3
	       WHERE user_id = $1`, a.ID, f.junk.String(), flags)
	exec(`UPDATE app.messages SET is_junk = $2 WHERE user_id = $1 AND id = $3`, a.ID, isJunk, a.MessageID)
	// seedUser leaves a pending intent of its own; take it out of the queue so
	// a claim shows exactly what the update under test enqueued.
	exec(`DELETE FROM app.writeback_intents WHERE user_id = $1`, a.ID)
	return f
}

// apply runs one Email/set update for the fixture message.
func (f *junkFixture) apply(t *testing.T, up EmailUpdate) {
	t.Helper()
	ctx := context.Background()
	if err := f.h.appStore().UserTx(ctx, f.uid, func(tx *UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, f.msg, up)
		if err == nil && !found {
			t.Fatal("message not found")
		}
		return err
	}); err != nil {
		t.Fatalf("ApplyEmailUpdate: %v", err)
	}
}

// claimed is what the engine would be handed, in the order it would apply it:
// "op arg" per intent.
func (f *junkFixture) claimed(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	if err := f.h.appStore().UserTx(ctx, f.uid, func(tx *UserTx) error {
		ins, err := tx.ClaimPendingIntents(ctx, f.acct, 50)
		if err != nil {
			return err
		}
		for _, in := range ins {
			var p map[string]string
			if err := json.Unmarshal(in.Payload, &p); err != nil {
				return err
			}
			arg := p["keyword"]
			if in.Op == IntentMove {
				arg = p["mailbox"]
			}
			out = append(out, string(in.Op)+" "+arg)
		}
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return out
}

func (f *junkFixture) emailChanges(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2`,
		f.u.ID, f.u.MessageID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *junkFixture) junkCol(t *testing.T) bool {
	t.Helper()
	var j bool
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT is_junk FROM app.messages WHERE id = $1`, f.u.MessageID).Scan(&j); err != nil {
		t.Fatal(err)
	}
	return j
}

// notJunk is exactly what the API's parseEmailPatch builds for
// `keywords/$junk: false` with no destination named (internal/api/email.go).
func notJunk() EmailUpdate {
	f := false
	return EmailUpdate{Junk: &f, MoveToRole: "inbox"}
}

func equalSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// THE HEADLINE (#576). A rescued message's verdict keyword comes off, $NotJunk
// goes on, and both are handed to the engine BEFORE the move — because the move
// carries whatever keywords the copy still has into the inbox.
//
// NEGATIVE CONTROLS: (1) delete the enqueueJunkVerdictIntents call in ApplyEmailUpdate and
// the sequence loses its two keyword ops; (2) move it below enqueueMoveIntents
// and the move comes first.
func TestNotJunkTakesTheVerdictKeywordOffBeforeTheMove(t *testing.T) {
	f := newJunkFixture(t, []string{`\Seen`, "$Junk"}, true, true)
	before := f.emailChanges(t)
	f.apply(t, notJunk())

	want := []string{"-kw $Junk", "+kw $NotJunk", "move INBOX"}
	if got := f.claimed(t); !equalSeq(got, want) {
		t.Fatalf("claimed intents = %v, want %v (keyword ops first, then the move)", got, want)
	}
	if f.junkCol(t) {
		t.Error("is_junk still true after Not junk")
	}
	if got := f.emailChanges(t) - before; got != 1 {
		t.Errorf("Email change rows appended = %d, want exactly 1", got)
	}
}

// Removal follows what the server REPORTED, spelling included, and needs no
// keyword probe — a server that stored `Junk` can be asked to drop it. The
// $NotJunk add DOES need the probe: a mailbox that refuses new keywords would
// fail the STORE, retry and dead-letter, for feedback that is only a courtesy.
//
// NEGATIVE CONTROL: drop the HAVING bool_and(accepts_keywords) and `+kw $NotJunk` appears.
func TestNotJunkHonoursTheKeywordProbeAndTheServersSpelling(t *testing.T) {
	f := newJunkFixture(t, []string{"Junk", "$junk"}, true, false)
	f.apply(t, notJunk())
	want := []string{"-kw $junk", "-kw Junk", "move INBOX"}
	if got := f.claimed(t); !equalSeq(got, want) {
		t.Fatalf("claimed intents = %v, want %v", got, want)
	}
}

// A PLACEMENT-ONLY provider (measured: Migadu) carries no verdict keyword, so
// there is nothing to remove and no removal STORE at all — only the courtesy
// $NotJunk where keywords are accepted, ahead of the move that is the real
// signal there.
func TestNotJunkOnAPlacementOnlyProviderRemovesNothing(t *testing.T) {
	f := newJunkFixture(t, []string{`\Seen`}, false, true)
	f.apply(t, notJunk())
	want := []string{"+kw $NotJunk", "move INBOX"}
	if got := f.claimed(t); !equalSeq(got, want) {
		t.Fatalf("claimed intents = %v, want %v", got, want)
	}
}

// A message already rescued — not junk, already canonically in the inbox — is
// a true no-op: no change row, no intent. The verdict intents ride on a change;
// they never create one.
func TestNotJunkTwiceIsANoOp(t *testing.T) {
	f := newJunkFixture(t, []string{"$Junk"}, true, true)
	f.apply(t, notJunk())
	_ = f.claimed(t) // drain the first action's intents
	before := f.emailChanges(t)
	f.apply(t, notJunk())
	if got := f.claimed(t); len(got) != 0 {
		t.Errorf("a repeated Not junk enqueued %v, want nothing", got)
	}
	if got := f.emailChanges(t) - before; got != 0 {
		t.Errorf("a repeated Not junk appended %d change rows, want 0", got)
	}
}

// THE REVERSE, HALF-SYMMETRIC ON PURPOSE. A move into Junk withdraws a $NotJunk
// the copy carries, so folder and keyword do not contradict each other — and it
// does NOT add $Junk, nor touch is_junk: the verdict is the provider's (spec
// §6), and a $Junk added on the way in would ride every later ordinary move
// back out, recreating #576 on providers that never had it.
//
// NEGATIVE CONTROL: drop the intoJunk branch and `-kw $NotJunk` disappears.
func TestMoveIntoJunkWithdrawsNotJunkAndNeverAddsJunk(t *testing.T) {
	f := newJunkFixture(t, []string{"$NotJunk"}, false, true)
	// The copy lives in the inbox for this one.
	if _, err := f.h.super.Exec(context.Background(),
		`UPDATE app.message_provenance SET mailbox_id = $2 WHERE user_id = $1`,
		f.u.ID, f.inbox.String()); err != nil {
		t.Fatal(err)
	}
	before := f.emailChanges(t)
	junk := f.junk
	f.apply(t, EmailUpdate{MoveToRole: "junk", MoveToMailboxID: &junk})
	want := []string{"-kw $NotJunk", "move Junk"}
	if got := f.claimed(t); !equalSeq(got, want) {
		t.Fatalf("claimed intents = %v, want %v", got, want)
	}
	if f.junkCol(t) {
		t.Error("a move into Junk set is_junk: the verdict is the provider's")
	}
	if got := f.emailChanges(t) - before; got != 1 {
		t.Errorf("Email change rows appended = %d, want exactly 1", got)
	}

	// With nothing to withdraw it is the plain move it always was.
	g := newJunkFixture(t, []string{`\Seen`}, false, true)
	if _, err := g.h.super.Exec(context.Background(),
		`UPDATE app.message_provenance SET mailbox_id = $2 WHERE user_id = $1`,
		g.u.ID, g.inbox.String()); err != nil {
		t.Fatal(err)
	}
	gj := g.junk
	g.apply(t, EmailUpdate{MoveToRole: "junk", MoveToMailboxID: &gj})
	if got := g.claimed(t); !equalSeq(got, []string{"move Junk"}) {
		t.Fatalf("claimed intents = %v, want just the move", got)
	}
}

// RLS NEGATIVE for the two reads the verdict intents are built from: another
// user naming a's message sees no carried token and no accepting account, so
// no intent could ever be derived from a's provenance. The owner's read is the
// positive half — without it, empty would prove nothing.
func TestJunkVerdictReadsAreUserScoped(t *testing.T) {
	f := newJunkFixture(t, []string{"$Junk"}, true, true)
	_, b := seedTwoUsers(t, f.h.super)
	ctx := context.Background()
	read := func(user ulid.ULID) (int, int) {
		var tokens, accts int
		if err := f.h.appStore().UserTx(ctx, user, func(tx *UserTx) error {
			c, err := tx.liveKeywordTokens(ctx, f.msg, junkVerdictTokens)
			if err != nil {
				return err
			}
			a, err := tx.keywordAcceptingAccounts(ctx, f.msg)
			tokens, accts = len(c), len(a)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return tokens, accts
	}
	if tok, acc := read(f.uid); tok != 1 || acc != 1 {
		t.Fatalf("owner reads tokens=%d accounts=%d, want 1 and 1", tok, acc)
	}
	if tok, acc := read(mustULID(t, b.ID)); tok != 0 || acc != 0 {
		t.Fatalf("another user reads tokens=%d accounts=%d of a's message — RLS leak", tok, acc)
	}
}

// THE TIE-BREAK, observed where it can actually fail. Intents from one
// transaction share created_at (now() is the transaction's start), so the order
// rests entirely on the id. Inserted here with the LATER id first — heap order
// opposite to id order — so a claim that fell back on heap order would hand
// them over backwards. The Not-junk test above cannot show this on its own: the
// app inserts in id order, so heap order happens to agree with it there.
//
// NEGATIVE CONTROL: remove `, id COLLATE "C"` from ClaimPendingIntents' ORDER BY and this
// claims the later id first.
func TestClaimOrdersTiesByID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	if _, err := h.super.Exec(ctx, `DELETE FROM app.writeback_intents WHERE user_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	early := ulid.MustNew(ulid.Timestamp(time.Now()), ulid.DefaultEntropy())
	late := ulid.MustNew(ulid.Timestamp(time.Now().Add(time.Second)), ulid.DefaultEntropy())
	tx, err := h.super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, id := range []ulid.ULID{late, early} { // LATER id inserted FIRST
		if _, err := tx.Exec(ctx, `
			INSERT INTO app.writeback_intents (id, user_id, message_id, account_id, op, payload, idempotency_key)
			VALUES ($1, $2, $3, $4, '+kw', '{"keyword":"x"}', $5)`,
			id.String(), a.ID, a.MessageID, a.AccountID, "tie-"+id.String()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var got []ulid.ULID
	if err := h.appStore().UserTx(ctx, mustULID(t, a.ID), func(utx *UserTx) error {
		ins, err := utx.ClaimPendingIntents(ctx, mustULID(t, a.AccountID), 50)
		for _, in := range ins {
			got = append(got, in.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != early || got[1] != late {
		t.Fatalf("claim order = %v, want [%s %s] (id order within one created_at)", got, early, late)
	}
}

// ApplyStoredFlagDelta records an acknowledged STORE on the live row it names,
// leaves a row that is no longer live alone, and — the RLS negative — cannot
// see another user's provenance at all.
func TestApplyStoredFlagDelta(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	if _, err := h.super.Exec(ctx,
		`UPDATE app.message_provenance SET remote_flags = $2 WHERE user_id = $1`, a.ID, []string{`\Seen`, "$Junk"}); err != nil {
		t.Fatal(err)
	}
	var provA string
	if err := h.super.QueryRow(ctx, `SELECT id FROM app.message_provenance WHERE user_id = $1`, a.ID).Scan(&provA); err != nil {
		t.Fatal(err)
	}
	flagsOf := func() []string {
		var fl []string
		if err := h.super.QueryRow(ctx, `SELECT remote_flags FROM app.message_provenance WHERE id = $1`, provA).Scan(&fl); err != nil {
			t.Fatal(err)
		}
		return fl
	}
	changeRows := func() int {
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	changesBefore := changeRows()
	call := func(user string, add, remove []string) bool {
		var changed bool
		if err := h.appStore().UserTx(ctx, mustULID(t, user), func(tx *UserTx) error {
			var err error
			changed, err = tx.ApplyStoredFlagDelta(ctx, mustULID(t, provA), add, remove)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return changed
	}

	// RLS: user b names a's row and changes nothing.
	if call(b.ID, []string{"$NotJunk"}, []string{"$Junk"}) {
		t.Fatal("another user's call reported a change to a's provenance")
	}
	if got := flagsOf(); !equalSeq(got, []string{`\Seen`, "$Junk"}) {
		t.Fatalf("a's flags after b's call = %v — RLS let b write a's provenance", got)
	}

	if !call(a.ID, []string{"$NotJunk"}, []string{"$junk"}) {
		t.Fatal("the owner's call reported no change")
	}
	if got := flagsOf(); !equalSeq(got, []string{`\Seen`, "$NotJunk"}) {
		t.Fatalf("flags = %v, want [\\Seen $NotJunk]", got)
	}
	if call(a.ID, []string{"$NotJunk"}, nil) {
		t.Error("re-recording the same STORE reported a change")
	}

	// A retired row is history, not a server copy: left alone.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.message_provenance SET state = 'stale_uidvalidity' WHERE id = $1`, provA); err != nil {
		t.Fatal(err)
	}
	if call(a.ID, nil, []string{"$NotJunk"}) {
		t.Error("a stale row was rewritten")
	}
	if n := changeRows() - changesBefore; n != 0 {
		t.Errorf("recording server flags appended %d change rows, want 0 (not JMAP-visible)", n)
	}
}
