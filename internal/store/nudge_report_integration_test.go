// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// NudgeMessagesAfter (#58): each direction/ground-truth flag per message,
// exactly, and another user's rows never visible.
func TestNudgeMessagesAfterFlagsAndIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	sent := NewULID().String()
	junkBox := NewULID().String()
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1,$2,$3,'Sent Items','Sent','\Sent'), ($4,$2,$3,'Spam','Spam','\Junk')`,
		sent, a.ID, a.AccountID, junkBox)
	thread := NewULID().String()
	exec(`INSERT INTO app.threads (id, user_id) VALUES ($1,$2)`, thread, a.ID)

	n := 0
	msg := func(extraCols, extraVals string, args ...any) string {
		t.Helper()
		n++
		id := NewULID().String()
		hash := sha256.Sum256([]byte(id))
		exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,1,1,$3)`,
			a.ID, hash[:], NewULID().String())
		all := append([]any{id, a.ID, hash[:], thread}, args...)
		exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, from_address, received_at, size_bytes`+extraCols+`)
		      VALUES ($1,$2,$3,$4,'Someone@Example.org', now(), 1`+extraVals+`)`, all...)
		return id
	}
	prov := func(msgID, mailbox string, state string) {
		exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		      VALUES ($1,$2,$3,$4,$5,$6,1,$7::app.provenance_state)`,
			NewULID().String(), a.ID, msgID, a.AccountID, mailbox, int64(9000+len(msgID)+n*7), state)
	}

	plain := msg(`, message_id_hint, in_reply_to, is_answered`, `, $5, $6, true`, "<p@x>", "<parent@x>")
	inSent := msg(``, ``)
	prov(inSent, sent, "live")
	inSentStale := msg(``, ``) // provenance no longer live: still sent
	prov(inSentStale, sent, "stale_uidvalidity")
	localSent := msg(`, local_mailbox_id`, `, $5`, sent)
	viaSub := msg(``, ``)
	exec(`INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id, from_address, envelope_to, state)
	      VALUES ($1,$2,$3,$4,$5,'x@example.com',ARRAY['y@example.org']::citext[],'sent')`,
		NewULID().String(), a.ID, a.AccountID, a.IdentityID, viaSub)
	scheduled := msg(``, ``)
	exec(`INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id, from_address, envelope_to, state)
	      VALUES ($1,$2,$3,$4,$5,'x@example.com',ARRAY['y@example.org']::citext[],'scheduled')`,
		NewULID().String(), a.ID, a.AccountID, a.IdentityID, scheduled)
	junkFlag := msg(`, is_junk`, `, true`)
	inJunk := msg(``, ``)
	prov(inJunk, junkBox, "live")
	trashed := msg(`, trashed_at`, `, now()`)
	draft := msg(`, is_draft`, `, true`)

	st := h.appStore()
	got := map[string]NudgeMessage{}
	var after ulid.ULID
	pages := 0
	for {
		var page []NudgeMessage
		if err := st.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
			var e error
			page, e = tx.NudgeMessagesAfter(ctx, after, 3)
			return e
		}); err != nil {
			t.Fatalf("walk: %v", err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		for _, r := range page {
			got[r.ID.String()] = r
			after = r.ID
		}
	}
	if _, ok := got[draft]; ok {
		t.Fatal("a draft was returned")
	}
	// 9 seeded non-draft here + the fixture's own seed message.
	if len(got) != 10 || pages != 4 {
		t.Fatalf("walk returned %d messages in %d pages, want 10 in 4", len(got), pages)
	}
	type flags struct{ sent, sub, junk, trash, answered bool }
	want := map[string]flags{
		plain:       {answered: true},
		inSent:      {sent: true},
		inSentStale: {sent: true},
		localSent:   {sent: true},
		viaSub:      {sub: true},
		scheduled:   {},
		junkFlag:    {junk: true},
		inJunk:      {junk: true},
		trashed:     {trash: true},
	}
	for id, w := range want {
		r := got[id]
		f := flags{r.InSentMailbox, r.ViaSubmission, r.IsJunk, r.IsTrashed, r.IsAnswered}
		if f != w {
			t.Errorf("message %s flags = %+v, want %+v", id, f, w)
		}
	}
	p := got[plain]
	if p.FromAddress != "someone@example.org" || p.MessageIDHint != "<p@x>" || p.InReplyTo != "<parent@x>" ||
		p.ThreadID.String() != thread || len(p.ContentHash) != 32 {
		t.Errorf("plain row = %+v", p)
	}

	var ids NudgeIdentities
	if err := st.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		var e error
		ids, e = tx.NudgeIdentities(ctx)
		return e
	}); err != nil {
		t.Fatalf("identities: %v", err)
	}
	if ids.SentMailboxes != 1 || len(ids.Addresses) != 1 || ids.Addresses[0] != strings.ToLower(a.Tag)+"@example.com" {
		t.Errorf("identities = %+v", ids)
	}

	// RLS negative: B walks its own mail only — none of A's ids, and B's
	// Sent count does not see A's Sent mailbox.
	var bRows []NudgeMessage
	var bIDs NudgeIdentities
	if err := st.UserTx(ctx, mustULID(t, b.ID), func(tx *UserTx) error {
		var e error
		if bRows, e = tx.NudgeMessagesAfter(ctx, ulid.ULID{}, 1000); e != nil {
			return e
		}
		bIDs, e = tx.NudgeIdentities(ctx)
		return e
	}); err != nil {
		t.Fatalf("walk b: %v", err)
	}
	for _, r := range bRows {
		if _, leaked := got[r.ID.String()]; leaked {
			t.Fatalf("user B saw user A's message %s", r.ID)
		}
	}
	if len(bRows) != 1 || bIDs.SentMailboxes != 0 {
		t.Fatalf("user B: %d rows (want its 1 seed message), %d sent mailboxes (want 0)", len(bRows), bIDs.SentMailboxes)
	}
}
