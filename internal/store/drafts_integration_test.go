// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for locally authored mail (mailroom#22, spec §5): drafts,
// the LWW-preserve-loser conflict rule, the Sent-append dedup, and the
// role→mailbox resolution that the writeback `move` intent depends on.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedMailbox creates a mailbox with a SPECIAL-USE role for the fixture user's
// account, as onboarding's RFC 6154 detection would.
func seedMailbox(t *testing.T, ctx context.Context, h *harness, u fixtureUser, imapName, specialUse string) ulid.ULID {
	t.Helper()
	id := NewULID()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		VALUES ($1, $2, $3, $4, $4, $5)`,
		id.String(), u.ID, u.AccountID, imapName, specialUse); err != nil {
		t.Fatalf("seed mailbox %s: %v", imapName, err)
	}
	return id
}

// makeDraft writes a draft with the given bytes and returns its message id.
func makeDraft(t *testing.T, ctx context.Context, tx *UserTx, u fixtureUser, mailbox ulid.ULID, body string) (ulid.ULID, []byte) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	id, err := tx.CreateLocalMessage(ctx, LocalMessage{
		ContentHash: sum[:], SizePlain: int64(len(body)), SizeStored: int64(len(body)) + 32,
		DEKID:   NewULID(),
		Subject: "draft", Snippet: body,
		FromName: "You", FromAddress: "you@example.com",
		SentAt: time.Now(), SizeBytes: int64(len(body)),
		MessageIDHint: "draft-" + NewULID().String() + "@example.com",
		MailboxID:     mailbox, AccountID: mustULID(t, u.AccountID),
		Draft: true,
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return id, sum[:]
}

// A draft Mailroom just composed has no IMAP provenance yet — the server has not
// assigned it a UID. It must still appear in Drafts immediately, or the user
// saves a draft and watches it vanish until the next sync pass.
func TestLocalDraftIsVisibleInItsMailbox(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, hash := makeDraft(t, ctx, tx, a, drafts, "draft body one")

		views, err := tx.GetEmails(ctx, []ulid.ULID{id})
		if err != nil {
			return err
		}
		if len(views) != 1 {
			t.Fatal("the draft was not stored")
		}
		v := views[0]
		if len(v.MailboxIDs) != 1 || v.MailboxIDs[0] != drafts {
			t.Fatalf("mailboxIds = %v, want the Drafts mailbox %s", v.MailboxIDs, drafts)
		}
		var hasDraft bool
		for _, k := range v.Keywords {
			if k == KeywordDraft {
				hasDraft = true
			}
		}
		if !hasDraft {
			t.Errorf("keywords = %v, want $draft", v.Keywords)
		}

		// ...and Email/query inMailbox finds it, which is what the Drafts view is.
		ids, _, err := tx.QueryEmails(ctx, EmailQuery{InMailbox: &drafts})
		if err != nil {
			return err
		}
		var found bool
		for _, q := range ids {
			if q == id {
				found = true
			}
		}
		if !found {
			t.Error("Email/query inMailbox:Drafts did not return the local draft")
		}

		// The bytes are identity (ADR-001): re-saving the same bytes returns the
		// SAME message rather than violating UNIQUE (user_id, content_hash).
		again, err := tx.CreateLocalMessage(ctx, LocalMessage{
			ContentHash: hash, SizePlain: 14, SizeStored: 46, DEKID: NewULID(),
			SentAt: time.Now(), MailboxID: drafts, AccountID: mustULID(t, a.AccountID), Draft: true,
		})
		if err != nil {
			return err
		}
		if again != id {
			t.Error("re-storing identical bytes must dedup to the same canonical message (ADR-001)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// THE DRAFT CONFLICT RULE (spec §5): "last-writer-wins by IMAP INTERNALDATE/
// modseq, and the losing version is preserved as a second draft rather than
// destroyed — duplication over data loss."
//
// Editing a draft = a NEW message (new bytes -> new content hash) + retiring the
// version we based the edit on. What that CANNOT do is destroy a version we did
// not author: another client's concurrent edit is a different content hash, so it
// is a different row with a different UID, and RetireDraft never touches it.
func TestDraftReplaceRetiresOnlyOurVersion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var v1, phone, v2 ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// Our version, and — concurrently — the phone's edit, which sync pulled
		// in from the IMAP Drafts folder as its own canonical message.
		v1, _ = makeDraft(t, ctx, tx, a, drafts, "our version")
		phone, _ = makeDraft(t, ctx, tx, a, drafts, "the phone's version")

		// We edit: the new version is created, the old one retired.
		v2, _ = makeDraft(t, ctx, tx, a, drafts, "our version, edited")
		_, rerr := tx.RetireDraft(ctx, v1)
		return rerr
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.GetEmails(ctx, []ulid.ULID{v1, phone, v2})
		if err != nil {
			return err
		}
		got := map[ulid.ULID]bool{}
		for _, v := range views {
			got[v.ID] = true
		}
		if got[v1] {
			t.Error("the superseded version we authored should be gone")
		}
		if !got[v2] {
			t.Error("the new version should exist")
		}
		if !got[phone] {
			t.Fatal("THE LOSING VERSION WAS DESTROYED: a concurrent edit from another client must be PRESERVED as a second draft (spec §5)")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// Retiring a draft that lives on the server enqueues the EXPUNGE of exactly that
// copy, and appends the Email `destroyed` change. Retiring a non-draft is refused
// — that path is Trash, not expunge.
func TestRetireDraftEnqueuesExpungeAndRefusesNonDrafts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var draftID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		draftID, _ = makeDraft(t, ctx, tx, a, drafts, "on the server")
		// Give it a live IMAP location, as a completed APPEND round-trip would.
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: draftID, AccountID: mustULID(t, a.AccountID), MailboxID: drafts,
			UID: 41, UIDValidity: 7, RemoteFlags: []string{`\Draft`},
		}); err != nil {
			return err
		}

		before, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		if _, err := tx.RetireDraft(ctx, draftID); err != nil {
			return err
		}
		after, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		if after != before+1 {
			t.Errorf("retire appended %d Email change rows, want exactly 1", after-before)
		}

		// The seeded fixture message is NOT a draft: retiring it must be refused.
		if _, err := tx.RetireDraft(ctx, mustULID(t, a.MessageID)); err == nil {
			t.Error("retiring a non-draft must be refused (that path is Trash, not expunge)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	// THE INTENT MUST SURVIVE THE DELETE. app.writeback_intents.message_id is ON
	// DELETE CASCADE, so an expunge intent that referenced the retired message
	// would be deleted by the very DELETE that retires it — the copy would live
	// on the IMAP server forever. It therefore names its target by header
	// (mailbox + Message-ID), with a NULL message_id, and this assertion is what
	// keeps it that way.
	var payload []byte
	var messageID *string
	if err := h.super.QueryRow(ctx, `
		SELECT payload, message_id FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge' AND payload ? 'message_id_header'`,
		a.ID).Scan(&payload, &messageID); err != nil {
		t.Fatalf("the retired draft's IMAP copy was not queued for expunge: %v", err)
	}
	if messageID != nil {
		t.Fatal("the expunge intent must not reference the message row it outlives (ON DELETE CASCADE would take it with it)")
	}
	var p struct {
		Mailbox         string `json:"mailbox"`
		MessageIDHeader string `json:"message_id_header"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Mailbox != "Drafts" || p.MessageIDHeader == "" {
		t.Fatalf("expunge payload = %+v, want the Drafts mailbox and the draft's Message-ID", p)
	}
	_ = draftID
}

// The Sent APPEND (spec §4) and its dedup handles: an idempotency key derived
// from the CONTENT HASH (so a retried send enqueues one intent, not two) and a
// payload carrying the Message-ID the engine probes the server with.
func TestSentAppendIntentIsContentAddressedAndCarriesDedupID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	sent := seedMailbox(t, ctx, h, a, "Sent", `\Sent`)

	raw := []byte("Message-ID: <sent-1@example.com>\r\n\r\nhi\r\n")
	sum := sha256.Sum256(raw)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		target, found, err := tx.MailboxForRole(ctx, acct, "sent")
		if err != nil {
			return err
		}
		if !found || target.ID != sent || target.IMAPName != "Sent" {
			t.Fatalf("MailboxForRole(sent) = %+v found=%v, want the seeded Sent mailbox", target, found)
		}

		for i := 0; i < 2; i++ { // a retried send enqueues the SAME intent
			if err := tx.EnqueueAppendIntent(ctx, mustULID(t, a.MessageID), acct, target.IMAPName,
				[]string{`\Seen`}, raw, sum[:], "sent-1@example.com"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	var n int
	var payload []byte
	if err := h.super.QueryRow(ctx, `
		SELECT count(*), min(payload::text)::jsonb
		  FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND op = 'append'`,
		a.ID, acct.String()).Scan(&n, &payload); err != nil {
		t.Fatalf("read append intents: %v", err)
	}
	if n != 1 {
		t.Fatalf("append intents = %d, want exactly 1 (the idempotency key is content-addressed)", n)
	}
	var p struct {
		Mailbox         string   `json:"mailbox"`
		Flags           []string `json:"flags"`
		MessageIDHeader string   `json:"message_id_header"`
		ContentHash     string   `json:"content_hash"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Mailbox != "Sent" {
		t.Errorf("payload mailbox = %q, want Sent", p.Mailbox)
	}
	if p.MessageIDHeader != "sent-1@example.com" {
		t.Errorf("payload message_id_header = %q — without it the engine cannot skip a provider self-append", p.MessageIDHeader)
	}
	if p.ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("payload content_hash = %q, want %x", p.ContentHash, sum)
	}
	// THE POINT OF mailroom#89: the payload names the bytes, it does not carry
	// them. Before this change the raw body went into this same JSONB payload,
	// base64-expanded (~1.33x) — a 50 MiB attachment wrote ~67 MiB into
	// app.writeback_intents on every autosave. The engine now reads the bytes
	// back out of the blob store by content hash instead.
	var rawShapeCheck map[string]json.RawMessage
	if err := json.Unmarshal(payload, &rawShapeCheck); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if _, hasBody := rawShapeCheck["body"]; hasBody {
		t.Error("the append intent payload must not carry a body key at all — see mailroom#89")
	}
}

// REGRESSION (mailroom#89): the writeback_intents payload must carry the
// CONTENT HASH, never the raw bytes. Before this fix a large attachment's
// bytes were base64-encoded whole into this JSONB payload (~1.33x expansion),
// so a 50 MiB draft wrote ~67 MiB into app.writeback_intents on every
// autosave — and drafts autosave repeatedly. This proves the stored row stays
// small regardless of message size, and never contains the bytes.
func TestAppendIntentPayloadNeverCarriesTheBody(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	// Simulate a large attachment: 5 MiB of body. If the body were embedded
	// (the pre-fix shape), the stored payload would be several MB; the fix
	// keeps it a few hundred bytes regardless.
	big := make([]byte, 5*1024*1024)
	for i := range big {
		big[i] = byte(i)
	}
	sum := sha256.Sum256(big)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.EnqueueAppendIntent(ctx, mustULID(t, a.MessageID), acct, "Drafts", []string{`\Draft`},
			big, sum[:], "big-draft@example.com")
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	var payload []byte
	if err := h.super.QueryRow(ctx, `
		SELECT payload FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND op = 'append'
		 ORDER BY created_at DESC LIMIT 1`,
		a.ID, acct.String()).Scan(&payload); err != nil {
		t.Fatalf("read append intent: %v", err)
	}

	const wantSmallerThan = 4 * 1024 // a few hundred bytes of JSON, not megabytes
	if len(payload) >= wantSmallerThan {
		t.Fatalf("stored payload is %d bytes, want under %d — the body leaked into JSONB (mailroom#89)",
			len(payload), wantSmallerThan)
	}
	var p struct {
		Mailbox     string `json:"mailbox"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("payload content_hash = %q, want %x", p.ContentHash, sum)
	}
	if p.Mailbox != "Drafts" {
		t.Errorf("payload mailbox = %q, want Drafts", p.Mailbox)
	}
	_ = drafts
}

// The store-side half of the Sent dedup: a byte-identical copy the provider
// filed and sync has already ingested is visible here, so we skip the append
// entirely (ADR-001).
func TestContentHashInMailboxSeesTheProvidersCopy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	sent := seedMailbox(t, ctx, h, a, "Sent", `\Sent`)
	other := seedMailbox(t, ctx, h, a, "Archive", `\Archive`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		msg := mustULID(t, a.MessageID)
		var hash []byte
		if err := tx.tx.QueryRow(ctx,
			`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
			a.ID, msg.String()).Scan(&hash); err != nil {
			return err
		}

		in, err := tx.ContentHashInMailbox(ctx, hash, sent)
		if err != nil {
			return err
		}
		if in {
			t.Fatal("nothing is in Sent yet")
		}

		// The provider self-appended, and sync ingested it.
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: msg, AccountID: mustULID(t, a.AccountID), MailboxID: sent,
			UID: 9, UIDValidity: 1, RemoteFlags: []string{},
		}); err != nil {
			return err
		}
		in, err = tx.ContentHashInMailbox(ctx, hash, sent)
		if err != nil {
			return err
		}
		if !in {
			t.Fatal("the provider's own copy in Sent must be seen, or we append a second one")
		}
		// A copy in a DIFFERENT mailbox is not a Sent copy.
		if in, err := tx.ContentHashInMailbox(ctx, hash, other); err != nil {
			return err
		} else if in {
			t.Error("dedup must be scoped to the target mailbox")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// REGRESSION (a real bug this change fixes): ApplyEmailUpdate used to enqueue a
// `move` intent whose payload carried only {"target_role": "archive"} — but the
// writeback engine's contract is {"mailbox": <raw IMAP name>}, so EVERY archive
// and trash would have been dead-lettered as "move: payload missing destination
// mailbox" the moment writeback was wired up. The role must be resolved to the
// account's actual mailbox name at enqueue time.
func TestMoveIntentCarriesTheResolvedMailboxName(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	seedMailbox(t, ctx, h, a, "Archive", `\Archive`)

	msg := mustULID(t, a.MessageID)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, msg, EmailUpdate{MoveToRole: "archive"})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("message not found")
		}
		return nil
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	var payload []byte
	if err := h.super.QueryRow(ctx, `
		SELECT payload FROM app.writeback_intents
		 WHERE user_id = $1 AND message_id = $2 AND op = 'move'
		 ORDER BY created_at DESC LIMIT 1`,
		a.ID, msg.String()).Scan(&payload); err != nil {
		t.Fatalf("read move intent: %v", err)
	}
	var p struct {
		Mailbox    string `json:"mailbox"`
		TargetRole string `json:"target_role"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Mailbox != "Archive" {
		t.Fatalf("move payload mailbox = %q, want the account's Archive mailbox name — the engine cannot act on a role", p.Mailbox)
	}
	if p.TargetRole != "archive" {
		t.Errorf("the role should be kept for diagnostics, got %q", p.TargetRole)
	}
}

// An account with NO mailbox for the role still gets an intent — one the engine
// dead-letters with a surfaced reason. A user's "archive" is never silently
// dropped (spec §4a).
func TestMoveIntentWithNoSuchRoleIsSurfacedNotDropped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	// No \Archive mailbox is seeded for this account.

	msg := mustULID(t, a.MessageID)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msg, EmailUpdate{MoveToRole: "archive"})
		return err
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	var n int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND message_id = $2 AND op = 'move'`,
		a.ID, msg.String()).Scan(&n); err != nil {
		t.Fatalf("read move intents: %v", err)
	}
	if n == 0 {
		t.Fatal("the move was silently dropped; it must be enqueued so the engine can dead-letter it visibly")
	}
}

// MarkMessageSent moves a draft out of Drafts and into Sent, clears $draft, and
// appends exactly one Email change row.
func TestMarkMessageSent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)
	sent := seedMailbox(t, ctx, h, a, "Sent", `\Sent`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, _ := makeDraft(t, ctx, tx, a, drafts, "about to be sent")

		before, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		if err := tx.MarkMessageSent(ctx, id, sent); err != nil {
			return err
		}
		after, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		if after != before+1 {
			t.Errorf("mark-sent appended %d change rows, want 1", after-before)
		}

		views, err := tx.GetEmails(ctx, []ulid.ULID{id})
		if err != nil {
			return err
		}
		v := views[0]
		if len(v.MailboxIDs) != 1 || v.MailboxIDs[0] != sent {
			t.Errorf("mailboxIds = %v, want Sent", v.MailboxIDs)
		}
		for _, k := range v.Keywords {
			if k == KeywordDraft {
				t.Error("a sent message must not still be a $draft")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// THE HEADLINE for mailroom#146. Marking a message sent is not enough: its DRAFT
// copy is still sitting in the IMAP Drafts folder carrying \Draft, so membership
// puts the message back in Drafts and the next flag reconcile writes is_draft
// true again, undoing the send. The copy has to go.
func TestRetireSentDraftCopyExpungesTheDraftsCopyOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)
	sent := seedMailbox(t, ctx, h, a, "Sent", `\Sent`)

	var id ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, _ = makeDraft(t, ctx, tx, a, drafts, "sent, but still in drafts")
		// Both copies exist on the server: the draft we appended when it was
		// saved, and the Sent copy this send just filed.
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: id, AccountID: acct, MailboxID: drafts,
			UID: 11, UIDValidity: 7, RemoteFlags: []string{`\Draft`},
		}); err != nil {
			return err
		}
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: id, AccountID: acct, MailboxID: sent,
			UID: 12, UIDValidity: 9, RemoteFlags: []string{`\Seen`},
		}); err != nil {
			return err
		}

		before, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		n, _, err := tx.RetireSentDraftCopy(ctx, id, acct)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("retired %d draft copies, want 1", n)
		}
		after, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		// It runs inside MarkMessageSent's transaction, whose row already says
		// this Email changed. A second row for one mutation breaks the invariant.
		if after != before {
			t.Errorf("appended %d Email change rows, want 0 (MarkMessageSent already logged this update)", after-before)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	// The Drafts location is gone; the Sent one — the copy that IS the sent
	// message — is untouched.
	rows, err := h.super.Query(ctx, `
		SELECT mb.imap_name FROM app.message_provenance p
		  JOIN app.mailboxes mb ON mb.id = p.mailbox_id
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'`,
		a.ID, id.String())
	if err != nil {
		t.Fatal(err)
	}
	var live []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		live = append(live, name)
	}
	rows.Close()
	if len(live) != 1 || live[0] != "Sent" {
		t.Errorf("live locations = %v, want exactly [Sent] — the draft copy must not still put this message in Drafts", live)
	}

	// The expunge is addressed by header, with a NULL message_id, so the engine
	// SEARCHes for it rather than firing at a UID a UIDVALIDITY roll may have
	// reassigned to somebody else's mail.
	var payload []byte
	var messageID *string
	if err := h.super.QueryRow(ctx, `
		SELECT payload, message_id FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge'`, a.ID).Scan(&payload, &messageID); err != nil {
		t.Fatalf("no expunge was queued for the sent message's draft copy: %v", err)
	}
	if messageID != nil {
		t.Error("the expunge must name its target by header, not by message row")
	}
	var p struct {
		Mailbox         string `json:"mailbox"`
		MessageIDHeader string `json:"message_id_header"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Mailbox != "Drafts" {
		t.Errorf("expunge targets %q, want Drafts — the Sent copy must never be a target", p.Mailbox)
	}
	if p.MessageIDHeader == "" {
		t.Error("expunge payload carries no Message-ID; the engine would have nothing to search for")
	}
}

// THE REGRESSION mailroom#150 is about, and the case a real send actually hits.
// When a send completes, the draft's own APPEND has usually NOT run yet — so
// there is no copy on the server and no provenance row to find. The first
// version of this looked for live Drafts provenance, found none, and did
// nothing; the queued APPEND then ran a minute later and put the message back in
// Drafts. The cleanup must work from the ACCOUNT'S DRAFTS MAILBOX, not from what
// we happen to have observed.
func TestRetireSentDraftCopyWorksBeforeTheDraftAppendHasRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// Exactly the state a send sees: the draft exists locally, its APPEND is
		// queued, and NOTHING is on the server yet — no provenance at all.
		id, hash := makeDraft(t, ctx, tx, a, drafts, "sent before its append ran")
		if err := tx.EnqueueAppendIntent(ctx, id, acct, "Drafts", []string{`\Draft`}, nil, hash, "queued@example.com"); err != nil {
			return err
		}
		if _, _, err := tx.RetireSentDraftCopy(ctx, id, acct); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	// THE #150 PROPERTY, and it is the cancel that provides it: the queued Drafts
	// APPEND is gone, so the copy is never created and the message does not
	// reappear in Drafts a minute after it was sent.
	var appends int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'append' AND payload->>'mailbox' = 'Drafts'`,
		a.ID).Scan(&appends); err != nil {
		t.Fatal(err)
	}
	if appends != 0 {
		t.Errorf("%d Drafts appends still queued — the draft copy will be created after the send", appends)
	}

	// ⚠ AND NO EXPUNGE, which is a CHANGE (#268).
	//
	// This used to queue one anyway, on the reasoning that the APPEND might
	// already have been applied by a pass we raced, and that a search matching
	// nothing was a harmless no-op. The second half of that is what broke: an
	// expunge that finds nothing is now a FAILURE, because "found nothing" and
	// "deleted it" being the same green is how a real leftover draft went
	// unnoticed. Queueing one here would dead-letter a spurious "could not remove
	// the draft" on every ordinary send.
	//
	// The race it was covering is still covered — by asking a better question.
	// We know we cancelled the APPEND ourselves, so we know no copy exists; when
	// we CANNOT cancel it (already inflight or applied) the expunge is still
	// queued. See TestRetireSentDraftCopyStillExpungesWhenTheAppendAlreadyRan.
	var expunges int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge' AND payload->>'mailbox' = 'Drafts'`,
		a.ID).Scan(&expunges); err != nil {
		t.Fatal(err)
	}
	if expunges != 0 {
		t.Errorf("expunge intents = %d, want 0 — we cancelled the append ourselves, so there is no copy "+
			"to remove and asking the server to remove one can only fail", expunges)
	}
}

// The other half of the race, and the one that MUST still queue an expunge: the
// Drafts APPEND is no longer pending — a writeback pass already claimed and
// applied it — so the copy is on the server and cancelling is not an option.
func TestRetireSentDraftCopyStillExpungesWhenTheAppendAlreadyRan(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var msgID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, hash := makeDraft(t, ctx, tx, a, drafts, "its append already ran")
		msgID = id
		return tx.EnqueueAppendIntent(ctx, id, acct, "Drafts", []string{`\Draft`}, nil, hash, "ran@example.com")
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	// The writeback pass got there first.
	if _, err := h.super.Exec(ctx, `
		UPDATE app.writeback_intents SET state = 'applied'
		 WHERE user_id = $1 AND op = 'append'`, a.ID); err != nil {
		t.Fatal(err)
	}

	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		n, _, err := tx.RetireSentDraftCopy(ctx, msgID, acct)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("queued %d expunges, want 1: the copy IS on the server and nothing else will remove it", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	var expunges int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge' AND payload->>'mailbox' = 'Drafts'`,
		a.ID).Scan(&expunges); err != nil {
		t.Fatal(err)
	}
	if expunges != 1 {
		t.Errorf("expunge intents = %d, want 1", expunges)
	}
}

// ⚠ THE ROOT CAUSE OF #268, in one test.
//
// A SIGNED send rewrites the message's bytes at submit time
// (ReplaceMessageBytes), so by the time the send completes the row's
// content_hash is the SIGNED one — while the Drafts APPEND enqueued back at
// draft-save time still names the UNSIGNED bytes it is going to write.
//
// Matching only the current hash therefore cancelled NOTHING on exactly the path
// that matters. The append ran seconds later, wrote an unsigned draft to the
// server, and the expunge that was supposed to clean it up reported success
// without removing it. The operator was left with a stale draft that came back
// every time they deleted it.
func TestRetireSentDraftCopyCancelsTheAppendOfSupersededBytes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// 1. The draft is saved and its APPEND is queued, naming the UNSIGNED bytes.
		id, unsignedHash := makeDraft(t, ctx, tx, a, drafts, "please sign me")
		if err := tx.EnqueueAppendIntent(ctx, id, acct, "Drafts", []string{`\Draft`}, nil, unsignedHash, "signme@example.com"); err != nil {
			return err
		}

		// 2. Submit signs it: the SAME message now holds DIFFERENT bytes, and the
		//    old hash is recorded as superseded (#260).
		signed := []byte("the multipart/signed rendering of the same message")
		sum := sha256.Sum256(signed)
		if _, err := tx.ReplaceMessageBytes(ctx, id, BlobMeta{
			ContentHash: sum[:], SizePlain: int64(len(signed)),
			SizeStored: int64(len(signed)) + 32, DEKID: NewULID(), EnvelopeVer: 1,
		}, MessageBytesProjection{HasAttachments: true}); err != nil {
			return err
		}

		// 3. The send completes and cleans up the draft copy.
		_, _, err := tx.RetireSentDraftCopy(ctx, id, acct)
		return err
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	var appends int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'append' AND payload->>'mailbox' = 'Drafts'`,
		a.ID).Scan(&appends); err != nil {
		t.Fatal(err)
	}
	if appends != 0 {
		t.Fatalf("%d Drafts appends survived the cleanup — the append names the message's SUPERSEDED "+
			"bytes, and matching only its current hash is what put an unsigned draft on the server (#268)", appends)
	}
}

// ⚠ THE LOOP (#268): deleting a leftover draft inside Mailroom did nothing at
// all, and the message came back at the next sync.
//
// An expunge for this (account, mailbox, Message-ID) had already run and been
// marked applied. Its idempotency key then swallowed every later expunge for the
// same copy — silently, because a no-op enqueue is indistinguishable from a
// successful one. The user deletes the message, sync re-ingests the surviving
// IMAP copy, and the row reappears. Forever.
func TestExpungeIntentIsRevivedAfterAnEarlierOneAlreadyApplied(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var first ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		first, err = tx.EnqueueIntent(ctx, IntentRow{
			AccountID: acct, Op: IntentExpunge,
			Payload:        []byte(`{"mailbox":"Drafts","message_id_header":"loop@example.com"}`),
			IdempotencyKey: "expunge:" + acct.String() + ":Drafts:loop@example.com",
			Revive:         true,
		})
		return err
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	// It ran, found nothing (or thought it did), and retired.
	if _, err := h.super.Exec(ctx, `
		UPDATE app.writeback_intents SET state = 'applied', attempts = 1, applied_at = now()
		 WHERE user_id = $1 AND id = $2`, a.ID, first.String()); err != nil {
		t.Fatal(err)
	}

	// The user deletes the leftover. Sync recorded provenance for it, so
	// RetireDraft has a live target and enqueues the expunge of exactly that copy.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, _ := makeDraft(t, ctx, tx, a, drafts, "the copy sync brought back")
		if _, err := tx.tx.Exec(ctx,
			`UPDATE app.messages SET message_id_hint = 'loop@example.com' WHERE user_id = $1 AND id = $2`,
			aID.String(), id.String()); err != nil {
			return err
		}
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: id, AccountID: acct, MailboxID: drafts,
			UID: 4, UIDValidity: 1, RemoteFlags: []string{`\Draft`},
		}); err != nil {
			return err
		}
		_, rerr := tx.RetireDraft(ctx, id)
		return rerr
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	var state string
	var attempts int
	if err := h.super.QueryRow(ctx, `
		SELECT state, attempts FROM app.writeback_intents
		 WHERE user_id = $1 AND id = $2`, a.ID, first.String()).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("intent state = %q, want pending: an expunge that finished an hour ago must not "+
			"silence the one the user is asking for now — that is the loop they cannot escape", state)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0: a revived intent starts its retry budget over", attempts)
	}
}

// The other side of the same coin, and the reason Revive is not simply "always
// re-enqueue": a request that has NOT been answered yet must still collapse.
// Two saves in quick succession must not put two expunges of the same copy in
// the queue.
func TestExpungeIntentStillCollapsesWhileStillPending(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)

	row := IntentRow{
		AccountID: acct, Op: IntentExpunge,
		Payload:        []byte(`{"mailbox":"Drafts","message_id_header":"dup@example.com"}`),
		IdempotencyKey: "expunge:" + acct.String() + ":Drafts:dup@example.com",
		Revive:         true,
	}
	var firstID, secondID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if firstID, err = tx.EnqueueIntent(ctx, row); err != nil {
			return err
		}
		secondID, err = tx.EnqueueIntent(ctx, row)
		return err
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	if firstID != secondID {
		t.Errorf("enqueue returned %s then %s, want the same row: a duplicate of a request still "+
			"waiting to run is noise, not a new fact", firstID, secondID)
	}
	var n int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND op = 'expunge'`,
		a.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expunge intents = %d, want 1", n)
	}
}

// The sharp edge. The Sent APPEND names the SAME content hash as the draft's —
// they are the same bytes — so cancelling a pending Drafts append by hash alone
// would cancel the Sent append too, and the message would never be filed.
func TestRetireSentDraftCopyCancelsOnlyTheDraftsAppend(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, hash := makeDraft(t, ctx, tx, a, drafts, "queued for both folders")
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: id, AccountID: acct, MailboxID: drafts,
			UID: 21, UIDValidity: 7, RemoteFlags: []string{`\Draft`},
		}); err != nil {
			return err
		}
		// Both appends pending, same bytes, different mailboxes.
		if err := tx.EnqueueAppendIntent(ctx, id, acct, "Drafts", []string{`\Draft`}, nil, hash, "mid@example.com"); err != nil {
			return err
		}
		if err := tx.EnqueueAppendIntent(ctx, id, acct, "Sent", []string{`\Seen`}, nil, hash, "mid@example.com"); err != nil {
			return err
		}
		if _, _, err := tx.RetireSentDraftCopy(ctx, id, acct); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	rows, err := h.super.Query(ctx, `
		SELECT payload->>'mailbox' FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'append' ORDER BY 1`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mailboxes []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		mailboxes = append(mailboxes, m)
	}
	rows.Close()
	if len(mailboxes) != 1 || mailboxes[0] != "Sent" {
		t.Errorf("pending appends = %v, want exactly [Sent]: the Drafts append is cancelled, "+
			"and cancelling the Sent one would mean the message is never filed anywhere", mailboxes)
	}
}

// THE FOOTGUN mailroom#148 closes. RetireDraft expunges EVERY live copy of the
// message, and a submitted message has one in SENT. Retiring it would delete the
// user's record of a message they actually sent — off the server — to make a
// Drafts entry go away. (The DELETE is also refused outright by app.submissions'
// foreign key, which is how this surfaced: a raw SQLSTATE in the client.)
func TestRetireDraftRefusesASubmittedMessage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)
	sent := seedMailbox(t, ctx, h, a, "Sent", `\Sent`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, _ := makeDraft(t, ctx, tx, a, drafts, "sent but still draft-flagged")
		for _, mb := range []ulid.ULID{drafts, sent} {
			if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
				MessageID: id, AccountID: acct, MailboxID: mb,
				UID: 61, UIDValidity: 7, RemoteFlags: []string{`\Draft`},
			}); err != nil {
				return err
			}
		}
		identity, found, err := tx.DefaultIdentityForAccount(ctx, acct)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("fixture account has no default identity")
		}
		subID, _, err := tx.CreateSubmission(ctx, SubmissionRow{
			AccountID: acct, IdentityID: identity.ID, MessageID: id,
			FromAddress: "you@example.com", EnvelopeTo: []string{"friend@example.com"},
			SendAt: time.Now(),
		})
		if err != nil {
			return err
		}
		if err := tx.FinishSubmission(ctx, subID, SubmissionSent, "250 ok"); err != nil {
			return err
		}

		if _, err := tx.RetireDraft(ctx, id); !errors.Is(err, ErrDraftSubmitted) {
			t.Fatalf("RetireDraft on a submitted message = %v, want ErrDraftSubmitted — "+
				"retiring it would expunge the Sent copy too", err)
		}

		submitted, pending, err := tx.MessageSubmissionStatus(ctx, id)
		if err != nil {
			return err
		}
		if !submitted || pending {
			t.Errorf("MessageSubmissionStatus = submitted:%v pending:%v, want true,false", submitted, pending)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	// Nothing was expunged and nothing was deleted.
	var intents, messages int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND op = 'expunge'`,
		a.ID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 0 {
		t.Errorf("%d expunge intents queued for a submitted message — the Sent copy was at risk", intents)
	}
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.messages WHERE user_id = $1`, a.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages == 0 {
		t.Error("the submitted message was deleted")
	}
}

// The repair query for #146's wreckage names messages by the SHAPE the bug left
// behind — sent, but still live in a Drafts folder — because nothing marks them.
func TestStrandedSentDraftIDsFindsOnlyTheWreckage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)
	sent := seedMailbox(t, ctx, h, a, "Sent", `\Sent`)

	var stranded, healthyDraft, healthySent ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		identity, found, err := tx.DefaultIdentityForAccount(ctx, acct)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("fixture account has no default identity")
		}
		submit := func(id ulid.ULID) error {
			subID, _, err := tx.CreateSubmission(ctx, SubmissionRow{
				AccountID: acct, IdentityID: identity.ID, MessageID: id,
				FromAddress: "you@example.com", EnvelopeTo: []string{"friend@example.com"},
				SendAt: time.Now(),
			})
			if err != nil {
				return err
			}
			return tx.FinishSubmission(ctx, subID, SubmissionSent, "250 ok")
		}

		// (1) The wreckage: sent, still live in Drafts.
		stranded, _ = makeDraft(t, ctx, tx, a, drafts, "stranded")
		for _, mb := range []ulid.ULID{drafts, sent} {
			if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
				MessageID: stranded, AccountID: acct, MailboxID: mb, UID: 71, UIDValidity: 7,
				RemoteFlags: []string{`\Draft`},
			}); err != nil {
				return err
			}
		}
		if err := submit(stranded); err != nil {
			return err
		}

		// (2) A real draft in Drafts that was never sent.
		healthyDraft, _ = makeDraft(t, ctx, tx, a, drafts, "still a draft")
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: healthyDraft, AccountID: acct, MailboxID: drafts, UID: 72, UIDValidity: 7,
			RemoteFlags: []string{`\Draft`},
		}); err != nil {
			return err
		}

		// (3) A correctly-sent message: Sent only. What every send looks like now.
		healthySent, _ = makeDraft(t, ctx, tx, a, drafts, "properly sent")
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: healthySent, AccountID: acct, MailboxID: sent, UID: 73, UIDValidity: 9,
			RemoteFlags: []string{`\Seen`},
		}); err != nil {
			return err
		}
		return submit(healthySent)
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	var got []StrandedSentDraft
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		got, e = tx.StrandedSentDraftIDs(ctx, 100)
		return e
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].MessageID != stranded {
		t.Fatalf("stranded = %v, want exactly [%v] (not the live draft %v, not the clean send %v)",
			got, stranded, healthyDraft, healthySent)
	}
	// The account comes back with it: the repair resolves the Drafts mailbox by
	// role, which needs the account rather than a provenance row.
	if got[0].AccountID != acct {
		t.Errorf("stranded account = %v, want %v", got[0].AccountID, acct)
	}
}

// With no Message-ID header there is no safe way to name the copy, so nothing is
// expunged and the local location stays: duplication over data loss, the same
// call RetireDraft makes.
func TestRetireSentDraftCopyRefusesToGuessWithoutAMessageID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, _ := makeDraft(t, ctx, tx, a, drafts, "no message-id")
		if _, err := tx.tx.Exec(ctx,
			`UPDATE app.messages SET message_id_hint = NULL WHERE user_id = $1 AND id = $2`,
			a.ID, id.String()); err != nil {
			return err
		}
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: id, AccountID: acct, MailboxID: drafts,
			UID: 31, UIDValidity: 7, RemoteFlags: []string{`\Draft`},
		}); err != nil {
			return err
		}
		n, _, err := tx.RetireSentDraftCopy(ctx, id, acct)
		if err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("retired %d copies with no Message-ID to name them by, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	var intents, provenance int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND op = 'expunge'`,
		a.ID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.message_provenance WHERE user_id = $1 AND state = 'live'`,
		a.ID).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	if intents != 0 {
		t.Errorf("%d expunge intents queued with no Message-ID to search for", intents)
	}
	if provenance == 0 {
		t.Error("the local location was dropped even though the server copy was left alone")
	}
}

// ⚠ WHAT THIS PINS (#522), AND WHY IT HAD TO BE BUILT SYNTHETICALLY.
//
// RetireDraft hard-deletes the local row and expunges every live copy the
// target query returns — no Trash, no tombstone, no undo. That query filtered on
// `state = 'live'` ALONE, so it returned every live copy in EVERY mailbox.
//
// The reason that is not merely theoretical: app.messages.is_draft is merged
// from the SERVER's \Draft flag at ingest, not set only by Mailroom's own
// compose path. So a message the provider had flagged \Draft anywhere — mail
// imported from an archive that passed through a dozen clients, say — was
// destroyable in one step, in every folder at once.
//
// MEASURED FIRST, and the measurement is why this test looks like this: on a
// real instance NO messages had is_draft set and there was no
// Drafts mailbox at all (INBOX, \Archive, \Junk, \Sent only). Positive control:
// the same join without the is_draft filter returned rows, so that zero was
// the corpus and not a broken query. The hazard is entirely LATENT there, which
// means no fixture drawn from real data could exercise it — the two-mailbox
// situation has to be constructed here.
//
// The Archive copy surviving is the whole assertion. Before the role filter it
// was expunged too, and that is mail the user never asked to destroy.
func TestRetireDraftExpungesOnlyTheDraftsCopyNotEveryFolder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)
	archive := seedMailbox(t, ctx, h, a, "Archive", `\Archive`)

	var id ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, _ = makeDraft(t, ctx, tx, a, drafts, "a draft that also sits in Archive")
		// The same message, live in two places: the Drafts copy Mailroom filed,
		// and a copy in Archive carrying the server's \Draft flag — which is how
		// is_draft comes to be true for mail Mailroom never composed.
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: id, AccountID: acct, MailboxID: drafts,
			UID: 21, UIDValidity: 3, RemoteFlags: []string{`\Draft`},
		}); err != nil {
			return err
		}
		if _, err := tx.UpsertProvenance(ctx, ProvenanceRow{
			MessageID: id, AccountID: acct, MailboxID: archive,
			UID: 22, UIDValidity: 3, RemoteFlags: []string{`\Draft`, `\Seen`},
		}); err != nil {
			return err
		}
		_, err := tx.RetireDraft(ctx, id)
		return err
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	// Exactly one expunge was enqueued, and it names Drafts.
	rows, err := h.super.Query(ctx, `
		SELECT payload->>'mailbox' FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge' ORDER BY 1`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var mb string
		if err := rows.Scan(&mb); err != nil {
			t.Fatal(err)
		}
		got = append(got, mb)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != "Drafts" {
		t.Errorf("expunge intents target %v, want exactly [Drafts]. An Archive entry here "+
			"means RetireDraft is destroying a copy the user never asked to delete, in a "+
			"folder that is not Drafts, with no Trash step and no undo", got)
	}
}

// TestShadowedAppendKeepsItsBytesAlive pins #557's REAL mechanism.
//
// ⚠ THE ISSUE NAMED THE WRONG ONE, and the wrong one cannot bite. #557 says
// RetireDraft's pending-only cancelPendingAppend leaves a suppressed APPEND
// behind while the blob is reclaimed. But RetireDraft deletes the message row
// in that same transaction, and app.writeback_intents.message_id is ON DELETE
// CASCADE, so the APPEND goes with it whatever its state. A probe written
// against that path passed green against unfixed code — which is how this one
// was found.
//
// The path that actually loses the bytes is the blob reclaim's own guard.
// hashesWithLiveWriteback answers "is any not-yet-applied APPEND still naming
// these bytes?" and it enumerated states — 'pending' and 'inflight' — written
// before migration 00056 added 'suppressed'. So on a shadowed account
// (ADR-028), signing a draft at submit time asks the guard about the
// pre-signature bytes, is told nothing needs them, and reclaims them out from
// under a ledger row that will be replayed by --apply. The engine then resolves
// the body by content hash, gets blob.ErrNotFound, and dead-letters: one
// writeback failure per signed send during the run.
//
// That is precisely the "could not file the draft" outcome hashesWithLiveWriteback's
// own doc says it exists to prevent — reintroduced for shadowed accounts only.
func TestShadowedAppendKeepsItsBytesAlive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	// Ingest-only mode for this account, as Account/set does it.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.accounts SET writeback_enabled = false WHERE user_id = $1 AND id = $2`,
		a.ID, a.AccountID); err != nil {
		t.Fatalf("shadow the account: %v", err)
	}

	var reclaimed [][]byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, unsignedHash := makeDraft(t, ctx, tx, a, drafts, "please sign me")
		if err := tx.EnqueueAppendIntent(ctx, id, acct, "Drafts",
			[]string{`\Draft`}, nil, unsignedHash, "signme@example.com"); err != nil {
			return err
		}

		// ARMING CONTROL, inside the transaction so a failure stops the test
		// before it can assert anything: the APPEND must really be suppressed.
		// If it were 'pending' the ORIGINAL guard would hold it correctly and
		// this test would pass green while exercising nothing.
		var suppressed int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM app.writeback_intents
			 WHERE user_id = $1 AND op = 'append' AND state = 'suppressed'`,
			a.ID).Scan(&suppressed); err != nil {
			return err
		}
		if suppressed != 1 {
			t.Fatalf("%d suppressed appends, want 1 — the account is not actually shadowed, so "+
				"this test would exercise the ordinary pending path and prove nothing", suppressed)
		}

		// Submit signs it: the same message now holds DIFFERENT bytes, and the
		// pre-signature hash becomes a reclaim candidate.
		signed := []byte("the multipart/signed rendering of the same message")
		sum := sha256.Sum256(signed)
		var err error
		reclaimed, err = tx.ReplaceMessageBytes(ctx, id, BlobMeta{
			ContentHash: sum[:], SizePlain: int64(len(signed)),
			SizeStored: int64(len(signed)) + 32, DEKID: NewULID(), EnvelopeVer: 1,
		}, MessageBytesProjection{HasAttachments: false})
		return err
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	if len(reclaimed) != 0 {
		t.Errorf("ReplaceMessageBytes reclaimed %d blob(s) while a SUPPRESSED append still names "+
			"those bytes — the ledger row survives the shadow run naming bytes that no longer "+
			"exist, and --apply dead-letters it with blob.ErrNotFound, one failure per signed "+
			"send (#557)", len(reclaimed))
	}
}

// TestUnreferencedDraftBytesAreStillReclaimed is the NEGATIVE CONTROL for the
// test above, and without it that one proves nothing.
//
// "Nothing was reclaimed" is only evidence that the guard held if a reclaim
// would otherwise have happened here. Identical fixture, identical call, with
// NO append intent naming the bytes — the superseded blob must be reclaimed. If
// this goes red, the harness cannot observe a reclaim at all and the assertion
// above is vacuous rather than passing.
//
// It also pins the other direction of the fix: widening the guard to count
// 'suppressed' must not turn the reclaim off in general, which would silently
// convert a leak-free path into a leaking one.
func TestUnreferencedDraftBytesAreStillReclaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	if _, err := h.super.Exec(ctx,
		`UPDATE app.accounts SET writeback_enabled = false WHERE user_id = $1 AND id = $2`,
		a.ID, a.AccountID); err != nil {
		t.Fatalf("shadow the account: %v", err)
	}

	var reclaimed [][]byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, _ := makeDraft(t, ctx, tx, a, drafts, "nothing queued for these bytes")
		// No EnqueueAppendIntent: nothing names the pre-signature bytes.
		signed := []byte("the signed rendering, with no queued append behind it")
		sum := sha256.Sum256(signed)
		var err error
		reclaimed, err = tx.ReplaceMessageBytes(ctx, id, BlobMeta{
			ContentHash: sum[:], SizePlain: int64(len(signed)),
			SizeStored: int64(len(signed)) + 32, DEKID: NewULID(), EnvelopeVer: 1,
		}, MessageBytesProjection{HasAttachments: false})
		return err
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed %d blob(s) with nothing naming the superseded bytes, want 1 — this "+
			"harness cannot observe a reclaim happening, so TestShadowedAppendKeepsItsBytesAlive's "+
			"\"nothing was reclaimed\" assertion proves nothing", len(reclaimed))
	}
}

// ⚠ THE ROOT CAUSE OF #603, in one test.
//
// RetireDraft decided what to expunge from app.message_provenance alone — copies
// SYNC HAS SEEN. But provenance is written when a sync pass observes the UID, not
// when the APPEND applies. In practice the provenance lagged the applied append by
// tens of seconds, against an autosave that supersedes
// every 15.
//
// A draft replaced inside that window hit three closed doors at once: nothing to
// cancel (the append had run), nothing to expunge (no provenance yet), and then
// the append intent — the last trace that a copy existed — was deleted by the
// message's own ON DELETE CASCADE. The copy stayed on the provider, and the next
// sync ingested it as a BRAND-NEW draft. That is the second draft users
// found sitting beside a refused send.
func TestRetireDraftExpungesACopyWhoseAppendRanBeforeSyncSawIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var msgID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, hash := makeDraft(t, ctx, tx, a, drafts, "superseded inside the provenance gap")
		msgID = id
		return tx.EnqueueAppendIntent(ctx, id, acct, "Drafts", []string{`\Draft`}, nil, hash, "gap@example.com")
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	// The writeback pass ran — the copy IS on the server — but no sync pass has
	// observed it yet, so there is deliberately NO provenance row. That absence
	// is the whole point of this test; adding one would test the old path.
	if _, err := h.super.Exec(ctx, `
		UPDATE app.writeback_intents SET state = 'applied'
		 WHERE user_id = $1 AND op = 'append'`, a.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, rerr := tx.RetireDraft(ctx, msgID)
		return rerr
	}); err != nil {
		t.Fatalf("RetireDraft: %v", err)
	}

	var expunges int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge' AND payload->>'mailbox' = 'Drafts'`,
		a.ID).Scan(&expunges); err != nil {
		t.Fatal(err)
	}
	if expunges != 1 {
		t.Errorf("expunge intents = %d, want 1: the copy is on the server, its provenance "+
			"has simply not been recorded yet, and nothing else will ever remove it", expunges)
	}
}

// ⚠ #691: AN INFLIGHT APPEND HAS NOT NECESSARILY READ ITS BYTES YET.
//
// ClaimPendingIntents flips a whole BATCH to 'inflight' before the engine applies
// any of it (writeback.ApplyPending), and the engine reads an APPEND's body from
// the blob store only when it reaches that intent (writeback.appendBody). So
// "inflight" means "claimed", not "on the server".
//
// RetireDraft counts an inflight APPEND as a copy that exists and queues the
// expunge for it — correct, because the APPEND may well land. But it then
// reclaimed the very bytes that APPEND still had to read. When the retire won
// the race, the engine got blob.ErrNotFound, dead-lettered an intent row the
// retire had already cascaded away, and wrote nothing; the expunge it was
// paired with then searched Drafts five times for a copy that never existed and
// dead-lettered "no copy of <…> found". That is the production trace in #691:
// an expunge for a revision with no APPEND row, created the same second as a
// send, DEAD after 5 attempts.
//
// Keeping the bytes makes the expunge's premise true: the APPEND completes, and
// the expunge — enqueued after the claim, so applied in a LATER pass — removes it.
func TestRetireDraftKeepsTheBytesAnInflightAppendHasNotReadYet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var msgID ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, hash := makeDraft(t, ctx, tx, a, drafts, "claimed but not yet appended")
		msgID = id
		return tx.EnqueueAppendIntent(ctx, id, acct, "Drafts", []string{`\Draft`}, nil, hash, "inflight@example.com")
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	// The pass claims the batch, exactly as ApplyPending does, and has not yet
	// reached this intent.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		claimed, err := tx.ClaimPendingIntents(ctx, acct, 64)
		if err != nil {
			return err
		}
		// The shared fixture seeds an unrelated +flag intent too; what matters is
		// that THIS append is now inflight.
		for _, c := range claimed {
			if c.Op == IntentAppend {
				return nil
			}
		}
		t.Fatalf("claimed %d intents and none is the draft's append — the fixture is not the race", len(claimed))
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	var reclaim [][]byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var rerr error
		reclaim, rerr = tx.RetireDraft(ctx, msgID)
		return rerr
	}); err != nil {
		t.Fatalf("RetireDraft: %v", err)
	}

	var expunges int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'expunge' AND payload->>'mailbox' = 'Drafts'`,
		a.ID).Scan(&expunges); err != nil {
		t.Fatal(err)
	}
	// Arming control: the expunge must still be queued, or the bytes being
	// kept would be a leak with nothing to show for it.
	if expunges != 1 {
		t.Fatalf("expunge intents = %d, want 1: an inflight APPEND may land, so its copy is owed a removal", expunges)
	}
	if len(reclaim) != 0 {
		t.Errorf("RetireDraft reclaimed %d blob(s) that a claimed, still-running APPEND has yet to read — "+
			"the APPEND dies on blob.ErrNotFound, nothing reaches the server, and the expunge queued "+
			"for it dead-letters \"no copy found\" after every retry (#691)", len(reclaim))
	}
}

// ⚠ THE CONTROL, and it is the one that stops the fix above from being worse
// than the bug.
//
// When the APPEND has NOT run, RetireDraft cancels it and no copy is ever
// created — so asking the server to remove one would dead-letter a spurious
// "could not remove the draft" on every ordinary discard. An empty SEARCH is a
// failure in the engine, deliberately (#268), so a spurious expunge is not
// harmless noise; it is a red intent the operator has to triage.
func TestRetireDraftQueuesNoExpungeWhenTheAppendNeverRan(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	var msgID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, hash := makeDraft(t, ctx, tx, a, drafts, "never left the queue")
		msgID = id
		return tx.EnqueueAppendIntent(ctx, id, acct, "Drafts", []string{`\Draft`}, nil, hash, "queued@example.com")
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	// Left 'pending' on purpose: the writeback pass never got to it.

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, rerr := tx.RetireDraft(ctx, msgID)
		return rerr
	}); err != nil {
		t.Fatalf("RetireDraft: %v", err)
	}

	var expunges, appends int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE op = 'expunge'), count(*) FILTER (WHERE op = 'append')
		  FROM app.writeback_intents WHERE user_id = $1`, a.ID).Scan(&expunges, &appends); err != nil {
		t.Fatal(err)
	}
	if expunges != 0 {
		t.Errorf("expunge intents = %d, want 0: we stopped the write ourselves, so there is "+
			"nothing on the server to remove and the engine would dead-letter this", expunges)
	}
	if appends != 0 {
		t.Errorf("append intents = %d, want 0: the pending append must be cancelled", appends)
	}
}
