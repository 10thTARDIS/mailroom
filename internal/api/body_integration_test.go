// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the three bugs the web client hit against a real server
// (mailroom#82, #83, #84). They run against a real Postgres with RLS enforced
// AND a real encrypted blob store — the body tests in particular store actual
// RFC 5322 bytes, encrypt them, and read them back out through Email/get,
// because the bug they cover (an Email with no body at all) is invisible to any
// test that hands the API a pre-parsed double.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// seedRawMessage stores raw RFC 5322 bytes in the real blob store (encrypted
// under the same DEK the API will read them with) and inserts the canonical
// message row that points at them, live in the fixture's Inbox.
func seedRawMessage(t *testing.T, h *composeHarness, f apiFixture, raw []byte) ulid.ULID {
	t.Helper()
	ctx := context.Background()

	dek, dekID, err := fakeKeys{}.MailDEK(ctx, f.UserID)
	if err != nil {
		t.Fatalf("dek: %v", err)
	}
	hash, err := h.blobs.Put(ctx, f.UserID, dek, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	stored, err := h.blobs.StoredSize(ctx, f.UserID, hash)
	if err != nil {
		t.Fatalf("blob size: %v", err)
	}

	msgID := store.NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed raw message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1,$2,$3,$4,$5)`,
		f.UserID.String(), hash[:], len(raw), stored, dekID.String())
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
	        from_name, from_address, received_at, size_bytes, has_attachments)
	      VALUES ($1,$2,$3,$4,'Quarterly report','the plain version','Alice','alice@example.com',
	              now(), $5, true)`,
		msgID.String(), f.UserID.String(), hash[:], f.ThreadID.String(), len(raw))
	// ⚠ THE UID MUST BE UNIQUE PER CALL. It used to be the literal 42, which was
	// fine while every test seeded exactly one raw message — and became a
	// `provenance_live_uq` violation the moment a test needed two (#396's
	// content-duplication test seeds the same attachment on two messages, which
	// is the whole point of it). The failure is a seeding error rather than an
	// assertion, so it looks like a broken test rather than the constraint doing
	// its job.
	nextSeededUID++
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
	      VALUES ($1,$2,$3,$4,$5,$6,1)`,
		store.NewULID().String(), f.UserID.String(), msgID.String(),
		f.AccountID.String(), f.InboxID.String(), nextSeededUID)
	return msgID
}

// nextSeededUID hands out distinct IMAP uids to seedRawMessage. Tests run
// sequentially within a package (`go test -p 1`, and no t.Parallel here), so a
// plain counter is sufficient and a mutex would only add noise.
var nextSeededUID = 41

// seedMailbox adds one more mailbox to a fixture's account.
func seedMailbox(t *testing.T, super *pgxpool.Pool, f apiFixture, imapName, specialUse string) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	var su any
	if specialUse != "" {
		su = specialUse
	}
	if _, err := super.Exec(context.Background(),
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		 VALUES ($1,$2,$3,$4,$4,$5)`,
		id.String(), f.UserID.String(), f.AccountID.String(), imapName, su); err != nil {
		t.Fatalf("seed mailbox %q: %v", imapName, err)
	}
	return id
}

// mailroom#82 — THE BLOCKER. A message stored as real MIME bytes must come back
// out of Email/get with a body: bodyStructure, textBody/htmlBody, attachments,
// bodyValues, and the full recipient lists a reply-all needs. Before this, the
// Email object had none of them and no client could display a message at all.
func TestEmailGetReturnsTheBodyFromTheBlob(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(alternativeWithAttachment))

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{msgID.String()},
		"properties": []string{
			"id", "subject", "from", "to", "cc", "replyTo",
			"bodyStructure", "textBody", "htmlBody", "attachments", "bodyValues",
		},
		"fetchTextBodyValues": true,
		"fetchHTMLBodyValues": true,
	})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get = %#v", res)
	}
	obj := list[0].(map[string]any)

	// The body itself.
	values, _ := obj["bodyValues"].(map[string]any)
	textPart := obj["textBody"].([]any)[0].(map[string]any)
	htmlPart := obj["htmlBody"].([]any)[0].(map[string]any)
	tv, ok := values[textPart["partId"].(string)].(map[string]any)
	if !ok || tv["value"] != "the plain version" {
		t.Errorf("text body value = %#v (bodyValues=%#v)", tv, values)
	}
	hv, ok := values[htmlPart["partId"].(string)].(map[string]any)
	if !ok || hv["value"] != "<p>the html version</p>" {
		t.Errorf("html body value = %#v", hv)
	}
	if htmlPart["type"] != "text/html" || textPart["type"] != "text/plain" {
		t.Errorf("body part types: text=%v html=%v", textPart["type"], htmlPart["type"])
	}

	// The attachment.
	atts := obj["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments = %#v", obj["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["name"] != "report.pdf" || att["type"] != "application/pdf" {
		t.Errorf("attachment = %#v", att)
	}

	// The structure, as a tree.
	bs, _ := obj["bodyStructure"].(map[string]any)
	if bs["type"] != "multipart/mixed" {
		t.Fatalf("bodyStructure = %#v", bs)
	}
	if subs, _ := bs["subParts"].([]any); len(subs) != 2 {
		t.Errorf("bodyStructure subParts = %#v", bs["subParts"])
	}

	// Reply-all: the recipients are exposed.
	to := obj["to"].([]any)
	if len(to) != 2 || to[0].(map[string]any)["email"] != "bob@example.com" {
		t.Errorf("to = %#v", obj["to"])
	}
	if cc := obj["cc"].([]any); len(cc) != 1 || cc[0].(map[string]any)["email"] != "dave@example.com" {
		t.Errorf("cc = %#v", obj["cc"])
	}

	// Cross-user: none of this is reachable for someone else's message id.
	_, b := seedTwo(t, h.super)
	res = call(t, h.api.emailGet, b.UserID, map[string]any{
		"ids":                 []string{msgID.String()},
		"properties":          []string{"id", "bodyValues"},
		"fetchAllBodyValues":  true,
		"fetchTextBodyValues": true,
	})
	if n := len(res["list"].([]any)); n != 0 {
		t.Errorf("another user read the body of A's message (%d objects)", n)
	}
}

// countingRaw is a RawSource that counts blob reads — the only way to assert the
// thing that actually matters about Email/get's cost.
type countingRaw struct {
	inner RawSource
	reads atomic.Int64
}

func (c *countingRaw) ReadRaw(ctx context.Context, userID ulid.ULID, hash []byte) ([]byte, error) {
	c.reads.Add(1)
	return c.inner.ReadRaw(ctx, userID, hash)
}

// The LIST path must not decrypt and parse every message. An Email/get that
// names the properties a list view shows touches NO blob; one that asks for a
// body property reads exactly the messages it asked about.
func TestEmailGetReadsBlobsOnlyForBodyProperties(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(alternativeWithAttachment))

	counter := &countingRaw{inner: h.api.raw}
	h.api = New(h.store, WithRawSource(counter))

	listProps := []string{"id", "threadId", "mailboxIds", "keywords", "subject",
		"from", "receivedAt", "preview", "hasAttachment", "size"}
	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{msgID.String(), a.MessageID.String()}, "properties": listProps,
	})
	if n := len(res["list"].([]any)); n != 2 {
		t.Fatalf("Email/get returned %d objects", n)
	}
	if got := counter.reads.Load(); got != 0 {
		t.Fatalf("a list-view Email/get read %d blobs; it must read none", got)
	}

	// ...and the reading pane pays for exactly what it opened.
	call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":                 []string{msgID.String()},
		"properties":          []string{"id", "textBody", "bodyValues"},
		"fetchTextBodyValues": true,
	})
	if got := counter.reads.Load(); got != 1 {
		t.Errorf("blob reads = %d, want exactly 1", got)
	}
}

// mailroom#83 — Email/query's hasKeyword could not see a LABEL's keyword atom:
// Email/get reported `keywords: {"MailroomLabel-x": true}` while
// Email/query {"hasKeyword": "MailroomLabel-x"} answered total 0. The read path
// and the write path disagreed about what a keyword is.
func TestEmailQueryKeywordSeesLabels(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// The seed applies the label to the message, so Email/get shows the atom.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{a.MessageID.String()}, "properties": []string{"id", "keywords"},
	})
	kw := got["list"].([]any)[0].(map[string]any)["keywords"].(map[string]any)
	if kw[a.LabelAtom] != true {
		t.Fatalf("Email/get keywords = %#v, want the label atom", kw)
	}

	// ...and Email/query must agree with it.
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"hasKeyword": a.LabelAtom}, "calculateTotal": true,
	})
	ids := res["ids"].([]any)
	if len(ids) != 1 || ids[0] != a.MessageID.String() {
		t.Fatalf("hasKeyword(%q) = %#v (total %v), want the labeled message",
			a.LabelAtom, ids, res["total"])
	}

	// notKeyword is the exact negation.
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"notKeyword": a.LabelAtom},
	})
	if n := len(res["ids"].([]any)); n != 0 {
		t.Errorf("notKeyword(%q) returned %d ids, want 0", a.LabelAtom, n)
	}

	// An unknown atom matches nothing (and does not error).
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"hasKeyword": "MailroomLabel-nonexistent"},
	})
	if n := len(res["ids"].([]any)); n != 0 {
		t.Errorf("hasKeyword(unknown) returned %d ids, want 0", n)
	}

	// Removing the label through Email/set removes it from the query too: the
	// two paths stay in agreement after a write.
	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/" + a.LabelAtom: nil},
		},
	})
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"hasKeyword": a.LabelAtom},
	})
	if n := len(res["ids"].([]any)); n != 0 {
		t.Errorf("after removing the label, hasKeyword still returns %d ids", n)
	}
}

// mailroom#84 — archiving a message left it in the inbox. The canonical move is
// applied in the same transaction as the Email/set, so the message leaves the
// inbox AT ONCE, while provenance (which the writeback engine needs to find the
// message on the IMAP server) is left exactly as it was.
func TestArchiveLeavesTheInboxImmediately(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	archiveID := seedMailbox(t, h.super, a, "Archive", `\Archive`)

	inInbox := func() int {
		res := call(t, h.api.emailQuery, a.UserID, map[string]any{
			"filter": map[string]any{"inMailbox": a.InboxID.String()},
		})
		return len(res["ids"].([]any))
	}
	if inInbox() != 1 {
		t.Fatalf("the seeded message is not in the inbox to begin with")
	}

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"mailboxIds/" + archiveID.String(): true},
		},
	})
	if _, ok := res["updated"].(map[string]any)[a.MessageID.String()]; !ok {
		t.Fatalf("archive was not applied: %#v", res)
	}

	if n := inInbox(); n != 0 {
		t.Errorf("the archived message is STILL in the inbox (%d) — it bounced back", n)
	}
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inMailbox": archiveID.String()},
	})
	if ids := res["ids"].([]any); len(ids) != 1 || ids[0] != a.MessageID.String() {
		t.Errorf("Email/query inMailbox=Archive = %#v, want the archived message", ids)
	}

	// mailboxIds reflects the move, not the source folder's provenance.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{a.MessageID.String()}, "properties": []string{"id", "mailboxIds"},
	})
	mb := got["list"].([]any)[0].(map[string]any)["mailboxIds"].(map[string]any)
	if len(mb) != 1 || mb[archiveID.String()] != true {
		t.Errorf("mailboxIds = %#v, want exactly the Archive mailbox", mb)
	}

	// The Mailbox counts agree with the query (otherwise the inbox keeps a badge
	// for a message that is no longer in it).
	counts := call(t, h.api.mailboxGet, a.UserID, map[string]any{
		"ids": []string{a.InboxID.String(), archiveID.String()},
	})
	for _, o := range counts["list"].([]any) {
		m := o.(map[string]any)
		want := float64(0)
		if m["id"] == archiveID.String() {
			want = 1
		}
		if m["totalEmails"] != want {
			t.Errorf("mailbox %v totalEmails = %v, want %v", m["name"], m["totalEmails"], want)
		}
	}

	// The IMAP side is untouched and still actionable: provenance still says
	// INBOX (which is where the bytes physically are — the writeback engine
	// needs that to move them), and the move intent names the real folder.
	var live int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.message_provenance
		  WHERE user_id=$1 AND message_id=$2 AND mailbox_id=$3 AND state='live'`,
		a.UserID.String(), a.MessageID.String(), a.InboxID.String()).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Errorf("live provenance rows in the source mailbox = %d, want 1: "+
			"the writeback engine finds the message by its (mailbox, uid) coordinate", live)
	}
	var payload string
	if err := h.super.QueryRow(ctx,
		`SELECT payload::text FROM app.writeback_intents
		  WHERE user_id=$1 AND message_id=$2 AND op='move'`,
		a.UserID.String(), a.MessageID.String()).Scan(&payload); err != nil {
		t.Fatalf("no move intent was enqueued: %v", err)
	}
	if !bytes.Contains([]byte(payload), []byte(`"mailbox": "Archive"`)) &&
		!bytes.Contains([]byte(payload), []byte(`"mailbox":"Archive"`)) {
		t.Errorf("move intent payload = %s, want it to name the Archive mailbox", payload)
	}

	// Re-asserting the same placement is a no-op, not a second IMAP move.
	before := h.countRows(t, "writeback_intents", a.UserID, a.MessageID)
	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"mailboxIds/" + archiveID.String(): true},
		},
	})
	if after := h.countRows(t, "writeback_intents", a.UserID, a.MessageID); after != before {
		t.Errorf("re-archiving enqueued another intent (%d -> %d)", before, after)
	}
}

// A trashed message leaves every other view at once and is found in the Trash —
// where a soft-deleted message (ADR-009) has to be, since Email/set told the
// client it was `destroyed`.
func TestTrashedMessageLeavesTheInboxAndIsFoundInTrash(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	call(t, h.api.emailSet, a.UserID, map[string]any{
		"destroy": []string{a.MessageID.String()},
	})

	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inMailbox": a.InboxID.String()},
	})
	if n := len(res["ids"].([]any)); n != 0 {
		t.Errorf("the trashed message is still in the inbox (%d)", n)
	}
	// An unfiltered query is every view that is not a folder (search, all mail):
	// a message the client was told is destroyed must not surface there either.
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{})
	if n := len(res["ids"].([]any)); n != 0 {
		t.Errorf("the trashed message still shows in an unfiltered query (%d)", n)
	}
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inMailbox": a.TrashID.String()},
	})
	if ids := res["ids"].([]any); len(ids) != 1 || ids[0] != a.MessageID.String() {
		t.Fatalf("Email/query inMailbox=Trash = %#v, want the trashed message", ids)
	}

	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{a.MessageID.String()}, "properties": []string{"id", "mailboxIds"},
	})
	mb := got["list"].([]any)[0].(map[string]any)["mailboxIds"].(map[string]any)
	if len(mb) != 1 || mb[a.TrashID.String()] != true {
		t.Errorf("mailboxIds = %#v, want exactly the Trash mailbox", mb)
	}

	// Restoring it puts it back in a real mailbox (Archive by default) and out
	// of the Trash.
	archiveID := seedMailbox(t, h.super, a, "Archive", `\Archive`)
	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"mailboxIds/" + a.TrashID.String(): nil},
		},
	})
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inMailbox": archiveID.String()},
	})
	if ids := res["ids"].([]any); len(ids) != 1 {
		t.Errorf("after restore, Email/query inMailbox=Archive = %#v, want the message", ids)
	}
}

// ── #419: one unreadable message must cost one message ──────────────────────

// failingRaw makes ReadRaw fail for chosen content hashes, the way a corrupt or
// missing blob does. Same shape as countingRaw above, for the same reason: the
// interesting states of the blob store are a few lines of decorator and a great
// deal of filesystem surgery.
//
// ⚠ THE ERROR IS THE ONE blob.Get ACTUALLY RETURNS ON A BAD BLOB — AEAD
// verification — because that is what makes this the #419 case rather than a
// generic I/O hiccup. blob.Get verifies the tag and the content hash as it
// drains, so this is what "the bytes are corrupt or tampered with" looks like
// from here.
type failingRaw struct {
	inner RawSource
	fail  map[string]bool
}

func (f *failingRaw) ReadRaw(ctx context.Context, userID ulid.ULID, hash []byte) ([]byte, error) {
	if f.fail[hex.EncodeToString(hash)] {
		return nil, errors.New("blob: AEAD verification failed")
	}
	return f.inner.ReadRaw(ctx, userID, hash)
}

func TestOneUnreadableMessageDoesNotHideTheRestOfTheBatch(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	const n = 4
	ids := make([]string, 0, n)
	hashes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		raw := "From: alice@example.com\r\nTo: you@example.com\r\n" +
			"Subject: message " + string(rune('A'+i)) + "\r\n" +
			"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\nContent-Type: text/plain\r\n\r\n" +
			"BODY-MARKER-" + string(rune('A'+i)) + "\r\n"
		id := seedRawMessage(t, h, a, []byte(raw))
		ids = append(ids, id.String())
		var ch []byte
		if err := h.super.QueryRow(context.Background(),
			`SELECT content_hash FROM app.messages WHERE id = $1`, id.String()).Scan(&ch); err != nil {
			t.Fatalf("read content_hash: %v", err)
		}
		hashes = append(hashes, hex.EncodeToString(ch))
	}

	get := func() []any {
		t.Helper()
		res := call(t, h.api.emailGet, a.UserID, map[string]any{
			"ids":                 ids,
			"properties":          []string{"id", "subject", "textBody", "bodyValues", "bodyUnavailable"},
			"fetchTextBodyValues": true,
		})
		return res["list"].([]any)
	}

	// ── CONTROL 3 (the one #419 asks for first): with NO corruption, all N ───
	// come back with bodies. Without this the test passes trivially against a
	// handler that returns nothing at all.
	list := get()
	if len(list) != n {
		t.Fatalf("CONTROL FAILED: %d of %d messages returned with nothing corrupted", len(list), n)
	}
	for i, o := range list {
		if !strings.Contains(mustJSONString(t, o), "BODY-MARKER-") {
			t.Fatalf("CONTROL FAILED: message %d came back with no body before any corruption: %v",
				i, mustJSONString(t, o))
		}
	}

	// ── Corrupt exactly one blob. ───────────────────────────────────────────
	broken := hashes[1]
	h.api.raw = &failingRaw{inner: h.api.raw, fail: map[string]bool{broken: true}}

	list = get()
	if len(list) != n {
		t.Fatalf("Email/get returned %d of %d messages after ONE blob went bad — one unreadable "+
			"message is still costing the whole batch, which on a decades-old imported mailbox "+
			"means a reading pane that will not load", len(list), n)
	}

	// ── 1. The others still carry their bodies. ─────────────────────────────
	var marked, withBody int
	for _, o := range list {
		obj := o.(map[string]any)
		if unavailable, _ := obj["bodyUnavailable"].(bool); unavailable {
			marked++
			// ⚠ 2. AND IT IS IDENTIFIABLE, NOT SILENTLY BLANK. A message whose
			// body simply vanished would be worse than one that says it could
			// not be read — the same "an absence is not an answer" rule the
			// scan verdicts and tier counts follow.
			if strings.Contains(mustJSONString(t, obj), "BODY-MARKER-") {
				t.Errorf("a message marked unavailable still carried body text: %v", mustJSONString(t, obj))
			}
			continue
		}
		if strings.Contains(mustJSONString(t, obj), "BODY-MARKER-") {
			withBody++
		}
	}
	if withBody != n-1 {
		t.Errorf("%d of %d readable messages came back with bodies, want %d — the failure is not "+
			"confined to the message that caused it", withBody, n-1, n-1)
	}
	if marked != 1 {
		t.Errorf("%d messages were marked bodyUnavailable, want exactly 1 — the client cannot tell "+
			"WHICH message is at fault, which was the other half of the old failure", marked)
	}
}
