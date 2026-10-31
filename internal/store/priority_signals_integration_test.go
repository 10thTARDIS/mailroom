// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// #842: replies from any client become one `reply` event per original, the
// reverse direction (original arriving after the reply) is caught, user B's
// sent mail can never record feedback on user A's messages, Triage/open's
// store half is once per message, and a re-score of an unchanged value is not a
// change.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"
)

// seedThreaded inserts a non-draft message with threading headers, as the
// superuser. threadID may be "" (no thread).
func seedThreaded(t *testing.T, h *harness, userID, from, hint, inReplyTo string, refs []string, threadID string) string {
	t.Helper()
	ctx := context.Background()
	id := NewULID().String()
	hash := sha256.Sum256([]byte("threaded-" + id))
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, 10, 20, $3)`,
		userID, hash[:], NewULID().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if refs == nil {
		refs = []string{}
	}
	var thread any
	if threadID != "" {
		thread = threadID
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, from_address,
		        received_at, size_bytes, message_id_hint, in_reply_to, references_ids)
		VALUES ($1, $2, $3, $4, 'Re: seed', $5, now(), 10, NULLIF($6, ''), NULLIF($7, ''), $8)`,
		id, userID, hash[:], thread, from, hint, inReplyTo, refs); err != nil {
		t.Fatalf("seed threaded message: %v", err)
	}
	return id
}

// sentMailbox creates a \Sent mailbox on the user's account and returns its id.
func sentMailbox(t *testing.T, h *harness, u fixtureUser) string {
	t.Helper()
	id := NewULID().String()
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		VALUES ($1, $2, $3, $4, 'Sent', '\Sent')`,
		id, u.ID, u.AccountID, "Sent-"+id); err != nil {
		t.Fatalf("seed sent mailbox: %v", err)
	}
	return id
}

// fileAt gives msgID a provenance row in mailboxID — where the user's server
// filed it.
func fileAt(t *testing.T, h *harness, u fixtureUser, msgID, mailboxID string) {
	t.Helper()
	var uid int64
	if err := h.super.QueryRow(context.Background(),
		`SELECT COALESCE(max(uid), 100) + 1 FROM app.message_provenance WHERE mailbox_id = $1`, mailboxID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		NewULID().String(), u.ID, msgID, u.AccountID, mailboxID, uid); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
}

// seedSent seeds a message From the user, filed in their Sent mailbox.
func seedSent(t *testing.T, h *harness, u fixtureUser, sent, hint, inReplyTo string, refs []string, threadID string) string {
	t.Helper()
	id := seedThreaded(t, h, u.ID, u.Tag+"@example.com", hint, inReplyTo, refs, threadID)
	fileAt(t, h, u, id, sent)
	return id
}

func replyRowsOn(t *testing.T, h *harness, msgID string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.feedback_events WHERE message_id = $1 AND action = 'reply'`,
		msgID).Scan(&n); err != nil {
		t.Fatalf("count reply rows: %v", err)
	}
	return n
}

// A reply written in ANOTHER client and ingested from Sent records exactly one
// reply event on the original; a rerun, and a second copy of the reply, record
// none.
func TestReplyFromAnyClientRecordedOncePerOriginal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	own := a.Tag + "@example.com"
	origHint := "seed-" + a.Tag + "@example.com" // the seeded inbound message, stored bracketed

	box := sentMailbox(t, h, a)
	sent := seedSent(t, h, a, box, "r1-"+a.Tag+"@client.test", origHint, nil, "")
	sentCopy := seedSent(t, h, a, box, "r1copy-"+a.Tag+"@client.test", origHint, nil, "")
	_ = own

	record := func(id string) (ReplyResolution, bool) {
		var res ReplyResolution
		var ok bool
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var e error
			res, ok, e = tx.RecordRepliesForSent(ctx, mustULID(t, id), "ingest")
			return e
		}); err != nil {
			t.Fatalf("RecordRepliesForSent: %v", err)
		}
		return res, ok
	}

	if res, ok := record(sent); res != ReplyResolved || !ok {
		t.Fatalf("first record: res=%v inserted=%v, want resolved+inserted", res, ok)
	}
	if n := replyRowsOn(t, h, a.MessageID); n != 1 {
		t.Fatalf("reply rows after first record = %d, want 1", n)
	}
	if res, ok := record(sent); res != ReplyResolved || ok {
		t.Errorf("rerun: res=%v inserted=%v, want resolved and NOT inserted", res, ok)
	}
	if res, ok := record(sentCopy); res != ReplyResolved || ok {
		t.Errorf("second copy: res=%v inserted=%v, want resolved and NOT inserted", res, ok)
	}
	if n := replyRowsOn(t, h, a.MessageID); n != 1 {
		t.Errorf("reply rows after rerun and second copy = %d, want exactly 1", n)
	}
}

// The resolution skips the user's own mail, and reports each non-match per case.
func TestReplyTargetResolution(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	own := a.Tag + "@example.com"
	origHint := "seed-" + a.Tag + "@example.com"

	// My follow-up to my own earlier reply, whose References still name the
	// inbound original: resolves THROUGH my own message to the original.
	box := sentMailbox(t, h, a)
	first := "first-" + a.Tag + "@client.test"
	seedSent(t, h, a, box, first, origHint, nil, "")
	followUp := seedSent(t, h, a, box, "", first, []string{"<" + origHint + ">", first}, "")
	// A reply whose only parent is my own mail.
	ownOnly := seedSent(t, h, a, box, "", first, nil, "")
	// From my address, but filed in the INBOX and never sent: a spoof.
	spoof := seedThreaded(t, h, a.ID, own, "", origHint, nil, "")
	fileAt(t, h, a, spoof, a.MailboxID)
	// Someone else's reply (not mine) to the original.
	notOwn := seedThreaded(t, h, a.ID, "stranger@elsewhere.test", "", origHint, nil, "")
	// A reply to mail I never received.
	missing := seedSent(t, h, a, box, "", "nowhere@nothing.test", nil, "")
	// No headers at all.
	bare := seedSent(t, h, a, box, "", "", nil, "")

	cases := []struct {
		name string
		id   string
		want ReplyResolution
	}{
		{"follow-up through own mail", followUp, ReplyResolved},
		{"parent is own mail only", ownOnly, ReplyParentOwnOnly},
		{"not the user's message", notOwn, ReplyNotOwn},
		{"parent not stored", missing, ReplyParentMissing},
		{"no threading headers", bare, ReplyNoHeaders},
		{"own address, never sent (spoof)", spoof, ReplyUnverified},
	}
	for _, c := range cases {
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			orig, res, err := tx.ReplyTarget(ctx, mustULID(t, c.id))
			if err != nil {
				return err
			}
			if res != c.want {
				t.Errorf("%s: resolution %v, want %v", c.name, res, c.want)
			}
			if res == ReplyResolved && orig.String() != a.MessageID {
				t.Errorf("%s: resolved to %s, want the inbound original %s", c.name, orig, a.MessageID)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// The original arriving AFTER the reply (an initial sync that reaches Sent
// first) is caught at its own ingest, through the thread.
//
// NEGATIVE CONTROL: the same call for a message that did not join the thread
// writes nothing — so the 1 above comes from the thread lookup, not from the
// helper writing on any inbound message.
func TestReplyRecordedWhenOriginalArrivesLater(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	own := a.Tag + "@example.com"

	thread := NewULID().String()
	if _, err := h.super.Exec(ctx, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, 'late')`, thread, a.ID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	lateHint := "late-" + a.Tag + "@elsewhere.test"
	seedSent(t, h, a, sentMailbox(t, h, a), "", lateHint, nil, thread)
	_ = own
	late := seedThreaded(t, h, a.ID, "late@elsewhere.test", lateHint, "", nil, thread)

	var written int
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		written, e = tx.RecordReplySignalsAtIngest(ctx, mustULID(t, late), mustULID(t, thread), false, "late@elsewhere.test", lateHint, mustULID(t, a.MailboxID))
		return e
	}); err != nil {
		t.Fatalf("not-joined ingest: %v", err)
	}
	if written != 0 || replyRowsOn(t, h, late) != 0 {
		t.Fatalf("control: a message that joined no thread recorded %d replies", written)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		written, e = tx.RecordReplySignalsAtIngest(ctx, mustULID(t, late), mustULID(t, thread), true, "late@elsewhere.test", lateHint, mustULID(t, a.MailboxID))
		return e
	}); err != nil {
		t.Fatalf("joined ingest: %v", err)
	}
	if written != 1 || replyRowsOn(t, h, late) != 1 {
		t.Errorf("the late original got %d reply rows (helper said %d), want 1", replyRowsOn(t, h, late), written)
	}
}

// RLS: user B's sent mail naming A's message records nothing on A's message —
// neither by resolution (B cannot see it) nor by a direct record call.
//
// NEGATIVE CONTROL: the identical sent message in A's own mailbox DOES record,
// so the zero for B is isolation, not a probe that cannot see a write.
func TestReplySignalCannotCrossUsers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aOrigHint := "seed-" + a.Tag + "@example.com"

	// The fixture seeds an `open` event on A's message, so compare before/after.
	onA := func() int {
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.feedback_events WHERE message_id = $1 AND action IN ('reply','open')`,
			a.MessageID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := onA()

	bSent := seedSent(t, h, b, sentMailbox(t, h, b), "", aOrigHint, nil, "")
	if err := s.UserTx(ctx, mustULID(t, b.ID), func(tx *UserTx) error {
		res, ok, err := tx.RecordRepliesForSent(ctx, mustULID(t, bSent), "ingest")
		if err != nil {
			return err
		}
		if res != ReplyParentMissing || ok {
			t.Errorf("B's reply naming A's message: res=%v inserted=%v, want parent-not-stored and nothing written", res, ok)
		}
		// Naming A's message id directly is refused by RLS too.
		ok, err = tx.RecordReplyFeedback(ctx, mustULID(t, a.MessageID), mustULID(t, bSent), "backfill")
		if err != nil {
			return err
		}
		if ok {
			t.Error("B recorded a reply on A's message by id")
		}
		_, found, err := tx.RecordOpenFeedback(ctx, mustULID(t, a.MessageID))
		if err != nil {
			return err
		}
		if found {
			t.Error("B's RecordOpenFeedback found A's message")
		}
		return nil
	}); err != nil {
		t.Fatalf("B tx: %v", err)
	}
	if after := onA(); after != before {
		t.Fatalf("B's actions put %d reply/open rows on A's message", after-before)
	}

	aSent := seedSent(t, h, a, sentMailbox(t, h, a), "", aOrigHint, nil, "")
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		_, ok, err := tx.RecordRepliesForSent(ctx, mustULID(t, aSent), "ingest")
		if err == nil && !ok {
			t.Error("control: A's own identical reply did not record — the probe cannot observe a write")
		}
		return err
	}); err != nil {
		t.Fatalf("A tx: %v", err)
	}
}

func TestOpenFeedbackOncePerMessage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	// A fresh message: the fixture's own message already carries an open event.
	fresh := seedThreaded(t, h, a.ID, "friend@example.com", "fresh-"+a.Tag+"@example.com", "", nil, "")
	msg := mustULID(t, fresh)
	for i, want := range []bool{true, false} {
		if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
			rec, found, err := tx.RecordOpenFeedback(ctx, msg)
			if err != nil {
				return err
			}
			if !found || rec != want {
				t.Errorf("call %d: recorded=%v found=%v, want recorded=%v", i+1, rec, found, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.feedback_events WHERE message_id = $1 AND action = 'open' AND context->>'source' = 'client'`,
		fresh).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("open rows = %d, want 1", n)
	}
	if _, found, err := func() (bool, bool, error) {
		var r, f bool
		err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
			var e error
			r, f, e = tx.RecordOpenFeedback(ctx, ulid.Make())
			return e
		})
		return r, f, err
	}(); err != nil || found {
		t.Errorf("an unknown id: found=%v err=%v, want not found", found, err)
	}
}

// Re-scoring a message to the SAME score is not a change, for a score that is
// not exactly representable as a float32 (0.1) — in RescoreTriage and in the
// dry run's TriageScoreWouldChange alike, so `priority-rescore --apply` writes
// what the dry run counted and a second run writes nothing. (A probe that cast
// RescoreTriage's parameter to double precision stayed green: the parameter is
// inferred as real from its VALUES position. TriageScoreWouldChange has no such
// position, so its explicit ::real is what this test guards there.)
func TestRescoreOfAnUnchangedScoreIsNotAChange(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	msg := mustULID(t, a.MessageID)
	const score = 0.1
	seq := func() int64 {
		var v int64
		if err := h.super.QueryRow(ctx,
			`SELECT last_seq FROM app.change_seq WHERE user_id = $1 AND object_type = 'Email'`, a.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		_, err := tx.RescoreTriage(ctx, msg, score, "priority:0", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := seq()
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		would, err := tx.TriageScoreWouldChange(ctx, msg, score, "priority:0", nil)
		if err != nil {
			return err
		}
		if would {
			t.Error("TriageScoreWouldChange reports a change for the identical score")
		}
		changed, err := tx.RescoreTriage(ctx, msg, score, "priority:0", nil)
		if err != nil {
			return err
		}
		if changed {
			t.Error("RescoreTriage reports a change for the identical score")
		}
		// And a genuinely different score IS a change.
		would, err = tx.TriageScoreWouldChange(ctx, msg, 0.2, "priority:0", nil)
		if err != nil {
			return err
		}
		if !would {
			t.Error("TriageScoreWouldChange missed a real change")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := seq(); after != before {
		t.Errorf("an unchanged re-score appended %d Email change rows", after-before)
	}
}

// SECURITY (#842 review): a message whose From header names the user, arriving
// in the INBOX, is not the user's reply — anyone can write that header. It
// records nothing, at ingest and in the reverse (thread) direction.
//
// NEGATIVE CONTROL: the identical message arriving into the user's Sent
// mailbox records exactly one — so the zero is the provenance rule, not a
// resolution that never fires.
func TestSpoofedOwnReplyInInboxRecordsNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	own := a.Tag + "@example.com"
	origHint := "seed-" + a.Tag + "@example.com"

	spoof := seedThreaded(t, h, a.ID, own, "spoof-"+a.Tag+"@attacker.test", origHint, nil, "")
	ingest := func(id, mailbox string) int {
		var n int
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var e error
			n, e = tx.RecordReplySignalsAtIngest(ctx, mustULID(t, id), ulid.ULID{}, false, own, "", mustULID(t, mailbox))
			return e
		}); err != nil {
			t.Fatalf("ingest seam: %v", err)
		}
		return n
	}
	if n := ingest(spoof, a.MailboxID); n != 0 || replyRowsOn(t, h, a.MessageID) != 0 {
		t.Fatalf("a spoofed own-address message in the INBOX recorded %d reply rows", replyRowsOn(t, h, a.MessageID))
	}
	// Not counted by the backfill's population either.
	fileAt(t, h, a, spoof, a.MailboxID)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		ids, unverified, err := tx.OwnThreadedMessages(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id.String() == spoof {
				t.Error("OwnThreadedMessages includes the INBOX spoof")
			}
		}
		if unverified != 1 {
			t.Errorf("unverified own count = %d, want 1", unverified)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	box := sentMailbox(t, h, a)
	genuine := seedThreaded(t, h, a.ID, own, "genuine-"+a.Tag+"@client.test", origHint, nil, "")
	if n := ingest(genuine, box); n != 1 || replyRowsOn(t, h, a.MessageID) != 1 {
		t.Fatalf("control: the same message arriving in Sent recorded %d (rows %d), want 1", n, replyRowsOn(t, h, a.MessageID))
	}
}

// A reply sent THROUGH MAILROOM is verifiable by its submission even before
// any Sent copy exists.
func TestMailroomSubmissionCountsAsSent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	own := a.Tag + "@example.com"
	sent := seedThreaded(t, h, a.ID, own, "", "seed-"+a.Tag+"@example.com", nil, "")

	record := func() (ReplyResolution, bool) {
		var res ReplyResolution
		var ok bool
		if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
			var e error
			res, ok, e = tx.RecordRepliesForSent(ctx, mustULID(t, sent), "backfill")
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return res, ok
	}
	// Control first: with no submission and no Sent copy it is unverified.
	if res, ok := record(); res != ReplyUnverified || ok {
		t.Fatalf("before the submission: %v inserted=%v, want unverified", res, ok)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id, from_address, envelope_to, state)
		VALUES ($1, $2, $3, $4, $5, $6, ARRAY['friend@example.com']::citext[], 'sent')`,
		NewULID().String(), a.ID, a.AccountID, a.IdentityID, sent, own); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	if res, ok := record(); res != ReplyResolved || !ok {
		t.Fatalf("with a sent submission: %v inserted=%v, want resolved and recorded", res, ok)
	}
	if n := replyRowsOn(t, h, a.MessageID); n != 1 {
		t.Errorf("reply rows = %d, want 1", n)
	}
}

// seedKey inserts an API key for the user and returns its id.
func seedKey(t *testing.T, h *harness, u fixtureUser) string {
	t.Helper()
	id := NewULID().String()
	hash := sha256.Sum256([]byte("key-" + id))
	if _, err := h.super.Exec(context.Background(), `INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes)
	      VALUES ($1, $2, 'ai tool', $3, ARRAY['mail:read'])`, id, u.ID, hash[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return id
}

// seedSentSubmission records a 'sent' submission of msgID, by keyID when it is
// not empty and by a person otherwise.
func seedSentSubmission(t *testing.T, h *harness, u fixtureUser, msgID, keyID string) {
	t.Helper()
	var key any
	if keyID != "" {
		key = keyID
	}
	if _, err := h.super.Exec(context.Background(), `INSERT INTO app.submissions
	        (id, user_id, account_id, identity_id, message_id, from_address, envelope_to, state, api_key_id)
	      VALUES ($1, $2, $3, $4, $5, $6, ARRAY['friend@example.net']::citext[], 'sent', $7)`,
		NewULID().String(), u.ID, u.AccountID, u.IdentityID, msgID, u.Tag+"@example.com", key); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
}

// #867: a reply an AI-tool key wrote or sent records NO reply signal,
// through any of the three sources, while the same reply by a person does.
// Four ways a reply can be a key's: a draft the key wrote, a submission the
// key made, and a Sent copy that landed on its OWN row (a provider rewrote the
// body, so no content hash matched) and is recognised by its Message-ID.
//
// NEGATIVE CONTROL: the identical reply by a person records exactly one row per
// original on every source, so each zero is the key guard, not a dead probe.
func TestKeySentReplyRecordsNoSignal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	box := sentMailbox(t, h, a)
	keyID := seedKey(t, h, a)

	type kase struct {
		name    string
		byKey   bool
		makeFor func(origHint, tag string) string // returns the sent message id
	}
	cases := []kase{
		{"person", false, func(origHint, tag string) string {
			return seedSent(t, h, a, box, "p-"+tag+"@client.test", origHint, nil, "")
		}},
		{"person submission", false, func(origHint, tag string) string {
			id := seedSent(t, h, a, box, "ps-"+tag+"@client.test", origHint, nil, "")
			seedSentSubmission(t, h, a, id, "")
			return id
		}},
		{"key draft", true, func(origHint, tag string) string {
			id := seedSent(t, h, a, box, "kd-"+tag+"@client.test", origHint, nil, "")
			if _, err := h.super.Exec(ctx, `INSERT INTO app.api_key_drafts (user_id, message_id, key_id) VALUES ($1, $2, $3)`,
				a.ID, id, keyID); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{"key submission", true, func(origHint, tag string) string {
			id := seedSent(t, h, a, box, "ks-"+tag+"@client.test", origHint, nil, "")
			seedSentSubmission(t, h, a, id, keyID)
			return id
		}},
		{"key Sent copy on its own row", true, func(origHint, tag string) string {
			hint := "kc-" + tag + "@client.test"
			draft := seedSent(t, h, a, box, hint, origHint, nil, "")
			seedSentSubmission(t, h, a, draft, keyID)
			return seedSent(t, h, a, box, hint, origHint, nil, "") // the re-filed copy, same Message-ID
		}},
	}
	for _, c := range cases {
		for _, source := range []string{"submission", "ingest", "backfill"} {
			tag := NewULID().String()
			origHint := "orig-" + tag + "@other.test"
			orig := seedThreaded(t, h, a.ID, "friend@other.test", origHint, "", nil, "")
			fileAt(t, h, a, orig, a.MailboxID)
			sent := c.makeFor(origHint, tag)

			var ok bool
			if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
				var e error
				if source == "ingest" {
					_, ok, e = tx.RecordRepliesForSent(ctx, mustULID(t, sent), source)
				} else {
					ok, e = tx.RecordReplyFeedback(ctx, mustULID(t, orig), mustULID(t, sent), source)
				}
				return e
			}); err != nil {
				t.Fatalf("%s/%s: %v", c.name, source, err)
			}
			want := 1
			if c.byKey {
				want = 0
			}
			if got := replyRowsOn(t, h, orig); got != want || ok != !c.byKey {
				t.Errorf("%s via %s: %d reply rows (inserted=%v), want %d", c.name, source, got, ok, want)
			}
		}
	}
}
