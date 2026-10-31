// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// A Drafts copy whose header EXPUNGE is already queued is not new mail (#686).
//
// The production timeline, from a real database: a draft revision's
// APPEND reached the provider; the next autosave retired that revision and
// queued `expunge {mailbox: Drafts, message_id_header: M}`; and the SAME sync
// pass whose writeback phase had run just before the retire then polled Drafts,
// found the provider's copy of M, matched it to nothing — its local row had been
// deleted a second earlier — and ingested it as a brand-new draft in a new
// thread. Nothing tied that row to the expunge, so when the expunge removed the
// copy a few seconds later the row stayed, in Drafts, until the hourly reaper.
//
// These tests drive the real Ingest entry point against the real store, with
// the retire done the way Email/set does it (create the new revision and retire
// the old one in ONE transaction). The IMAP side is represented by the one fact
// ingest can see: an APPEND intent the writeback engine has marked 'applied'.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// draftsFixture adds role-bearing Drafts and Sent mailboxes to the ingest
// fixture. The base fixture's INBOX/Archive carry no special_use.
func (f *fixture) draftsFixture(t *testing.T) (drafts, sent ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	drafts, sent = ulid.Make(), ulid.Make()
	for _, m := range []struct {
		id         ulid.ULID
		name, role string
	}{{drafts, "Drafts", `\Drafts`}, {sent, "Sent", `\Sent`}} {
		if _, err := f.super.Exec(ctx, `
			INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
			VALUES ($1, $2, $3, $4, $4, $5)`,
			m.id.String(), f.userID.String(), f.accountID.String(), m.name, m.role); err != nil {
			t.Fatalf("seed %s: %v", m.name, err)
		}
	}
	return drafts, sent
}

// composeRevision records one locally composed draft revision and queues its
// Drafts APPEND — compose.Composer.record's two writes, without the builder.
func (f *fixture) composeRevision(t *testing.T, tx *store.UserTx, drafts ulid.ULID, subject, msgID string) (ulid.ULID, []byte) {
	t.Helper()
	ctx := context.Background()
	raw := rawMessage(subject, msgID, nil)
	sum := sha256.Sum256(raw)
	id, err := tx.CreateLocalMessage(ctx, store.LocalMessage{
		ContentHash: sum[:], SizePlain: int64(len(raw)), SizeStored: int64(len(raw)),
		DEKID: f.dekID, Subject: subject, FromAddress: "alice@example.com",
		SentAt: time.Now().UTC(), SizeBytes: int64(len(raw)),
		MessageIDHint: msgID, MailboxID: drafts, AccountID: f.accountID,
		Draft: true, Seen: true,
	})
	if err != nil {
		t.Fatalf("create %s: %v", subject, err)
	}
	if err := tx.EnqueueAppendIntent(ctx, id, f.accountID, "Drafts",
		[]string{`\Draft`, `\Seen`}, raw, sum[:], msgID); err != nil {
		t.Fatalf("append intent %s: %v", subject, err)
	}
	return id, raw
}

// markAppendsApplied is the writeback pass that put every queued APPEND on the
// server. It is what makes RetireDraft owe an expunge (#603).
func (f *fixture) markAppendsApplied(t *testing.T) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(), `
		UPDATE app.writeback_intents SET state = 'applied', applied_at = now()
		 WHERE user_id = $1 AND op = 'append' AND state = 'pending'`, f.userID.String()); err != nil {
		t.Fatal(err)
	}
}

// supersede is one autosave: rev2 created and rev1 retired in the same
// transaction, which is what internal/api's Email/set does.
func (f *fixture) supersede(t *testing.T, drafts, rev1 ulid.ULID, subject, msgID string) ulid.ULID {
	t.Helper()
	var rev2 ulid.ULID
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		rev2, _ = f.composeRevision(t, tx, drafts, subject, msgID)
		_, err := tx.RetireDraft(context.Background(), rev1)
		return err
	}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	return rev2
}

func (f *fixture) changeRows(t *testing.T) int {
	t.Helper()
	return f.scalar(t, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, f.userID.String())
}

func (f *fixture) messagesWithMessageID(t *testing.T, msgID string) int {
	t.Helper()
	return f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id = $1 AND message_id_hint = $2`,
		f.userID.String(), msgID)
}

// THE REPRODUCTION. rev1 is on the server; rev2 retired it and queued the
// expunge of M; a sync pass reads the provider's copy of M before the expunge
// has run.
func TestIngestDefersADraftsCopyWhoseExpungeIsQueued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	drafts, _ := f.draftsFixture(t)

	const m1, m2 = "rev1-686@example.com", "rev2-686@example.com"
	var rev1 ulid.ULID
	var rev1Raw []byte
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		rev1, rev1Raw = f.composeRevision(t, tx, drafts, "Draft rev1", m1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.markAppendsApplied(t)
	rev2 := f.supersede(t, drafts, rev1, "Draft rev2", m2)

	// PREMISE: the expunge of M is queued and has not run. Without it this test
	// is about a different situation (#603: no expunge owed or queued at all).
	if got := f.scalar(t, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge' AND state = 'pending'
		   AND payload->>'mailbox' = 'Drafts' AND payload->>'message_id_header' = $2`,
		f.userID.String(), m1); got != 1 {
		t.Fatalf("pending expunge intents for rev1 = %d, want 1 — the premise of #686 does not hold", got)
	}
	before := f.changeRows(t)

	// The sync pass reads the provider's copy of rev1: uid 437, as in production.
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, rev1Raw, f.prov(drafts, 437, []string{`\Draft`, `\Seen`}))
	if err != nil {
		t.Fatal(err)
	}

	if res.Created || f.messagesWithMessageID(t, m1) != 0 {
		t.Errorf("the provider's copy of a RETIRED revision became a new local draft (created=%v) — "+
			"this is #686: its expunge is already queued, so it is not new mail", res.Created)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id = $1`, f.userID.String()); got != 1 {
		t.Errorf("messages = %d, want 1 (rev2 only)", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE user_id = $1 AND uid = 437`,
		f.userID.String()); got != 0 {
		t.Errorf("provenance rows at uid 437 = %d, want 0 — a copy being removed is not a location", got)
	}
	// NONE FOR A NO-OP. Nothing JMAP-visible changed, so nothing may be appended.
	if got := f.changeRows(t); got != before {
		t.Errorf("change_log grew by %d rows for an ingest that changed nothing", got-before)
	}
	// The live revision is untouched.
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id = $1 AND id = $2 AND is_draft`,
		f.userID.String(), rev2.String()); got != 1 {
		t.Error("the live revision is gone or no longer a draft")
	}
}

// CONTROL — DUPLICATION OVER DATA LOSS. Once the expunge has given up (the
// provider kept the copy through every retry), the copy is real mail on the
// server and must be ingested, or it would be a permanent gap the #93 backfill
// re-fetches forever.
func TestIngestStillTakesADraftsCopyWhoseExpungeDeadLettered(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	drafts, _ := f.draftsFixture(t)

	const m1, m2 = "rev1-dead-686@example.com", "rev2-dead-686@example.com"
	var rev1 ulid.ULID
	var rev1Raw []byte
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		rev1, rev1Raw = f.composeRevision(t, tx, drafts, "Draft rev1", m1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.markAppendsApplied(t)
	f.supersede(t, drafts, rev1, "Draft rev2", m2)
	if _, err := f.super.Exec(ctx, `
		UPDATE app.writeback_intents SET state = 'dead', dead_reason = 'server kept it'
		 WHERE user_id = $1 AND op = 'expunge'`, f.userID.String()); err != nil {
		t.Fatal(err)
	}

	before := f.changeRows(t)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, rev1Raw, f.prov(drafts, 438, []string{`\Draft`}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || f.messagesWithMessageID(t, m1) != 1 {
		t.Fatalf("a copy the expunge could NOT remove was swallowed (created=%v) — ingest is now losing "+
			"a message that really is on the server", res.Created)
	}
	if got := f.changeRows(t); got == before {
		t.Error("a new message was created with no change_log row")
	}
}

// CONTROL — THE LIVE REVISION. A Drafts copy carrying the live revision's own
// Message-ID is that revision's location and must attach to it as before. It
// is also the proof that the guard keys on the RETIRED Message-ID, not on
// "some expunge is pending in Drafts".
func TestIngestAttachesTheLiveRevisionsCopyWhileAnExpungeIsQueued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	drafts, _ := f.draftsFixture(t)

	const m1, m2 = "rev1-live-686@example.com", "rev2-live-686@example.com"
	var rev1 ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		rev1, _ = f.composeRevision(t, tx, drafts, "Draft rev1", m1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.markAppendsApplied(t)
	rev2 := f.supersede(t, drafts, rev1, "Draft rev2", m2)
	rev2Raw := rawMessage("Draft rev2", m2, nil)

	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, rev2Raw, f.prov(drafts, 439, []string{`\Draft`, `\Seen`}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.MessageID != rev2 {
		t.Fatalf("the live revision's own copy resolved to %s (created=%v), want rev2 %s",
			res.MessageID, res.Created, rev2)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE user_id = $1 AND message_id = $2 AND uid = 439`,
		f.userID.String(), rev2.String()); got != 1 {
		t.Errorf("rev2's Drafts copy recorded %d provenance rows, want 1", got)
	}
}

// THE SAME WINDOW ON THE SEND PATH. RetireSentDraftCopy queues an expunge by the
// SENT message's Message-ID when its Drafts APPEND has already run. A sync pass
// reading that Drafts copy before the expunge lands used to attach it to the
// sent message — content-hash dedup — which puts the sent message back in
// Drafts and merges \Draft back onto it (is_draft = true): #146's symptom,
// reopened for the length of the window.
func TestIngestDoesNotPullASentMessageBackIntoDraftsWhileItsExpungeIsQueued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	drafts, sent := f.draftsFixture(t)

	const ms = "sent-686@example.com"
	var msg ulid.ULID
	var raw []byte
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		msg, raw = f.composeRevision(t, tx, drafts, "Sent one", ms)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.markAppendsApplied(t)
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		if err := tx.MarkMessageSent(ctx, msg, sent); err != nil {
			return err
		}
		n, _, err := tx.RetireSentDraftCopy(ctx, msg, f.accountID)
		if err == nil && n != 1 {
			t.Fatalf("premise: RetireSentDraftCopy queued %d expunges, want 1", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := f.changeRows(t)

	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(drafts, 440, []string{`\Draft`, `\Seen`}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Fatal("the sent message's Drafts copy created a second message")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id = $1 AND id = $2 AND is_draft`,
		f.userID.String(), msg.String()); got != 0 {
		t.Error("the SENT message is a draft again: its Drafts copy was attached while the expunge " +
			"removing that copy was still queued")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE user_id = $1 AND mailbox_id = $2`,
		f.userID.String(), drafts.String()); got != 0 {
		t.Errorf("the sent message has %d Drafts location(s), want 0", got)
	}
	if got := f.changeRows(t); got != before {
		t.Errorf("change_log grew by %d rows for an ingest that changed nothing", got-before)
	}
}
