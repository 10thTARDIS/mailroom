// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mcp/mcptest"
)

// AN API KEY'S OWN DRAFTS THROUGH MCP (#872, ADR-045): the scope-aware
// wording, repeats collapsing, draft_update and draft_discard, and Markdown
// bodies — end to end, through the real bearer gate, key gate and store.

func newDraftFixture(t *testing.T) (*agentHarness, apiFixture, string) {
	t.Helper()
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	addr := "me-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	return h, a, addr
}

// keyDrafts is how many rows of app.api_key_drafts the key has.
func (h *agentHarness) keyDrafts(t *testing.T, keyID string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.api_key_drafts WHERE key_id = $1`, keyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// liveDraftsOf is how many messages of the user are drafts not in Trash.
func (h *agentHarness) liveDraftsOf(t *testing.T, user ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND is_draft AND trashed_at IS NULL`,
		user.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func emailID(t *testing.T, out map[string]any) string {
	t.Helper()
	id, _ := out["emailId"].(string)
	if id == "" {
		t.Fatalf("no emailId in %v", out)
	}
	return id
}

func TestMCPDraftWordingFollowsTheKey(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	srv := h.mcpServer(t, a.UserID)
	args := map[string]any{"from": addr, "to": []string{"bob@example.net"}, "subject": "Lunch", "body": "Thursday?"}

	for _, c := range []struct {
		name     string
		key      map[string]any
		wantSend bool
	}{
		{"no send", map[string]any{"scopes": presetAssistant}, false},
		{"send, approval", map[string]any{"scopes": append(slices.Clone(presetAssistant), "mail:send"),
			"confirmScopes": []string{"mail:send"}}, true},
		{"send, direct", map[string]any{"scopes": append(slices.Clone(presetAssistant), "mail:send"),
			"confirmScopes": []string{}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.key["name"] = c.name
			token, _ := h.newKey(t, a.UserID, c.key)
			s := mcpConnect(t, srv, token)
			tools, err := s.Tools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			listed, _ := json.Marshal(tools)
			if strings.Contains(string(listed), "send_draft") != c.wantSend {
				t.Fatalf("tools/list mentions send_draft = %v, want %v", !c.wantSend, c.wantSend)
			}
			for _, tl := range tools {
				if tl.Name == "draft_new" || tl.Name == "draft_reply" || tl.Name == "draft_update" {
					if strings.Contains(tl.Description, "This key cannot send email.") == c.wantSend {
						t.Errorf("%s: cannot-send sentence present = %v: %s", tl.Name, !c.wantSend, tl.Description)
					}
				}
			}
			res := mcpCall(t, s, "draft_new", args)
			mustOK(t, "draft_new", res)
			if strings.Contains(res.Text, "send_draft") != c.wantSend ||
				strings.Contains(res.Text, "This key cannot send email. The draft is in Drafts; the account owner sends it from Mailroom.") == c.wantSend {
				t.Fatalf("draft_new result for %s: %s", c.name, res.Text)
			}
			if c.name == "send, approval" && !strings.Contains(res.Text, "approve") {
				t.Errorf("an approval key's result should say the send needs approval: %s", res.Text)
			}
			if !c.wantSend {
				// Called anyway, the hidden send_draft names what is missing.
				r := mcpCall(t, s, "send_draft", map[string]any{"emailId": emailID(t, res.Object())})
				want := "send_draft is not available: this key does not have permission to send email (mail:send)"
				if !r.IsError || !strings.Contains(r.Text, want) {
					t.Fatalf("send_draft without mail:send: %+v", r)
				}
			}
		})
	}
}

func TestMCPRepeatDraftsCollapse(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	ctx := context.Background()
	srv := h.mcpServer(t, a.UserID)
	token, keyID := h.newKey(t, a.UserID, map[string]any{"name": "loop", "scopes": presetAssistant, "ratePerMinute": 600})
	s := mcpConnect(t, srv, token)
	args := func(body string) map[string]any {
		return map[string]any{"from": addr, "to": []string{"Bob <bob@example.net>"}, "subject": "Lunch", "body": body}
	}

	first := mustOK(t, "draft_new", mcpCall(t, s, "draft_new", args("Thursday?")))
	again := mcpCall(t, s, "draft_new", args("Thursday?"))
	second := mustOK(t, "draft_new", again)
	if second["status"] != "already_saved" || second["emailId"] != first["emailId"] || h.keyDrafts(t, keyID) != 1 {
		t.Fatalf("an identical repeat: first %v, second %v, %d drafts; want already_saved with the first id and one draft",
			first, second, h.keyDrafts(t, keyID))
	}
	if !strings.Contains(again.Text, "nothing new was written") {
		t.Errorf("the repeat's summary must say nothing was written: %s", again.Text)
	}

	// A different body is a different draft.
	other := mustOK(t, "draft_new", mcpCall(t, s, "draft_new", args("Friday?")))
	if other["status"] != "draft" || other["emailId"] == first["emailId"] || h.keyDrafts(t, keyID) != 2 {
		t.Fatalf("a different body collapsed: %v (%d drafts)", other, h.keyDrafts(t, keyID))
	}

	// Older than the window: written again.
	if _, err := h.super.Exec(ctx, `UPDATE app.api_key_drafts SET created_at = now() - interval '11 minutes' WHERE message_id = $1`,
		first["emailId"]); err != nil {
		t.Fatal(err)
	}
	late := mustOK(t, "draft_new", mcpCall(t, s, "draft_new", args("Thursday?")))
	if late["status"] != "draft" || late["emailId"] == first["emailId"] || h.keyDrafts(t, keyID) != 3 {
		t.Fatalf("a repeat after the window collapsed: %v", late)
	}

	// A discarded draft is not reused.
	mustOK(t, "draft_discard", mcpCall(t, s, "draft_discard", map[string]any{"emailId": late["emailId"]}))
	afterDiscard := mustOK(t, "draft_new", mcpCall(t, s, "draft_new", args("Thursday?")))
	if afterDiscard["status"] != "draft" || afterDiscard["emailId"] == late["emailId"] {
		t.Fatalf("a repeat of a discarded draft reused it: %v", afterDiscard)
	}

	// Another key's identical draft is its own.
	token2, key2 := h.newKey(t, a.UserID, map[string]any{"name": "other", "scopes": presetAssistant})
	theirs := mustOK(t, "draft_new", mcpCall(t, mcpConnect(t, srv, token2), "draft_new", args("Thursday?")))
	if theirs["status"] != "draft" || h.keyDrafts(t, key2) != 1 {
		t.Fatalf("another key's identical draft collapsed into this key's: %v", theirs)
	}
}

func TestMCPRepeatOfASentDraftIsNotReused(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "sender", "acknowledgeSendAndDelete": false,
		"scopes": append(slices.Clone(presetAssistant), "mail:send"), "confirmScopes": []string{}})
	s := mcpConnect(t, h.mcpServer(t, a.UserID), token)
	args := map[string]any{"from": addr, "to": []string{"bob@example.net"}, "subject": "Sent", "body": "once"}
	first := mustOK(t, "draft_new", mcpCall(t, s, "draft_new", args))
	mustOK(t, "send_draft", mcpCall(t, s, "send_draft", map[string]any{"emailId": first["emailId"]}))
	again := mustOK(t, "draft_new", mcpCall(t, s, "draft_new", args))
	if again["status"] != "draft" || again["emailId"] == first["emailId"] {
		t.Fatalf("a repeat of a submitted draft reused it: %v", again)
	}
}

func TestMCPRepeatDraftRace(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	srv := h.mcpServer(t, a.UserID)
	token, keyID := h.newKey(t, a.UserID, map[string]any{"name": "racer", "scopes": presetAssistant, "ratePerMinute": 600})
	const rounds, racers = 4, 3
	sessions := make([]*mcptest.Session, racers)
	for i := range sessions {
		sessions[i] = mcpConnect(t, srv, token)
	}
	for round := range rounds {
		var wg sync.WaitGroup
		ids := make([]string, racers)
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := sessions[i].Call(context.Background(), "draft_new", map[string]any{"from": addr,
					"to": []string{"bob@example.net"}, "subject": "Race", "body": fmt.Sprintf("round %d", round)})
				if err == nil && !res.IsError {
					ids[i], _ = res.Object()["emailId"].(string)
				}
			}()
		}
		wg.Wait()
		for i := range ids {
			if ids[i] == "" || ids[i] != ids[0] {
				t.Fatalf("round %d: concurrent identical drafts got ids %v, want one draft", round, ids)
			}
		}
	}
	if n := h.keyDrafts(t, keyID); n != rounds {
		t.Fatalf("%d drafts after %d rounds of %d identical concurrent calls, want %d", n, rounds, racers, rounds)
	}
}

func TestMCPDraftUpdateAndDiscard(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	ctx := context.Background()
	srv := h.mcpServer(t, a.UserID)
	token, keyID := h.newKey(t, a.UserID, map[string]any{"name": "writer", "scopes": presetAssistant, "ratePerMinute": 600})
	s := mcpConnect(t, srv, token)

	// A reply draft, so the threading has something to keep.
	msg := seedRawMessage(t, h.composeHarness, a, []byte("From: Alice <alice@example.com>\r\nTo: "+addr+
		"\r\nSubject: Plans\r\nMessage-ID: <p1@example.com>\r\n\r\nWhen?\r\n"))
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET message_id_hint='<p1@example.com>' WHERE id=$1`, msg.String()); err != nil {
		t.Fatal(err)
	}
	reply := emailID(t, mustOK(t, "draft_reply", mcpCall(t, s, "draft_reply", map[string]any{"emailId": msg.String(), "body": "Soon."})))
	changesBefore := h.emailChanges(t, a.UserID)

	updated := mustOK(t, "draft_update", mcpCall(t, s, "draft_update", map[string]any{"emailId": reply,
		"to": []string{"alice@example.com"}, "subject": "Re: Plans", "body": "Tomorrow at **noon**."}))
	newID := emailID(t, updated)
	if newID == reply || updated["replacedEmailId"] != reply {
		t.Fatalf("draft_update: %v", updated)
	}
	if exists, tr := h.trashed(t, ulid.MustParse(reply)); !exists || !tr {
		t.Fatalf("the old version: exists=%v trashed=%v, want in Trash", exists, tr)
	}
	var owner string
	_ = h.super.QueryRow(ctx, `SELECT key_id FROM app.api_key_drafts WHERE message_id=$1`, newID).Scan(&owner)
	if owner != keyID {
		t.Fatalf("the new version's provenance: key %q, want %q", owner, keyID)
	}
	got := call(t, h.api.emailGet, a.UserID, map[string]any{"ids": []string{newID}, "properties": []string{"inReplyTo", "subject"}})
	if r := asObj(asArr(got["list"])[0]); !strInArr(r["inReplyTo"], "p1@example.com") {
		t.Fatalf("the reply threading was not kept: %v", r)
	}
	changes := h.emailChanges(t, a.UserID)[len(changesBefore):]
	if !slices.Contains(changes, newID+":created") || !slices.Contains(changes, reply+":updated") {
		t.Fatalf("change_log rows of the update: %v, want the new version created and the old updated", changes)
	}
	// The old version cannot be sent, changed or discarded any more.
	if r := mcpCall(t, s, "draft_update", map[string]any{"emailId": reply, "to": []string{"a@example.com"},
		"subject": "x", "body": "y"}); !r.IsError || !strings.Contains(r.Text, "no longer an unsent draft") {
		t.Fatalf("updating a replaced version: %+v", r)
	}

	discarded := mustOK(t, "draft_discard", mcpCall(t, s, "draft_discard", map[string]any{"emailId": newID}))
	if discarded["status"] != "discarded" {
		t.Fatalf("draft_discard: %v", discarded)
	}
	if exists, tr := h.trashed(t, ulid.MustParse(newID)); !exists || !tr {
		t.Fatalf("discarded draft: exists=%v trashed=%v, want in Trash", exists, tr)
	}
	if ch := h.emailChanges(t, a.UserID); ch[len(ch)-1] != newID+":updated" {
		t.Fatalf("discard wrote no change_log row: %v", ch[len(ch)-1])
	}
}

func TestMCPDraftUpdateAndDiscardRefusals(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	srv := h.mcpServer(t, a.UserID)
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "mine", "acknowledgeSendAndDelete": true,
		"scopes": append(slices.Clone(presetAssistant), "mail:send", "mail:delete"), "confirmScopes": []string{"mail:send"},
		"ratePerMinute": 600})
	s := mcpConnect(t, srv, token)
	newDraft := func(sess *mcptest.Session, body string) string {
		res, err := sess.Call(context.Background(), "draft_new", map[string]any{"from": addr,
			"to": []string{"bob@example.net"}, "subject": "S", "body": body})
		if err != nil || res.IsError {
			t.Fatalf("draft_new: %v %+v", err, res)
		}
		return res.Object()["emailId"].(string)
	}

	// The owner's own draft (a session create).
	created := call(t, h.api.emailSet, a.UserID, map[string]any{"create": map[string]any{"u": map[string]any{
		"from": []any{map[string]any{"email": addr}}, "to": []any{map[string]any{"email": "x@example.net"}},
		"subject": "mine", "textBody": []any{map[string]any{"partId": "1", "type": "text/plain"}},
		"bodyValues": map[string]any{"1": map[string]any{"value": "the owner's words"}}}}})
	userDraft := asObj(asObj(created["created"])["u"])["id"].(string)

	// Another key's draft.
	token2, _ := h.newKey(t, a.UserID, map[string]any{"name": "theirs", "scopes": presetAssistant})
	otherDraft := newDraft(mcpConnect(t, srv, token2), "theirs")

	// A draft whose send waits for approval.
	pendingDraft := newDraft(s, "pending")
	if out := mustOK(t, "send_draft", mcpCall(t, s, "send_draft", map[string]any{"emailId": pendingDraft})); out["status"] != "pending_approval" {
		t.Fatalf("send_draft in approval mode: %v", out)
	}

	// A submitted draft: a key with direct sends.
	token3, _ := h.newKey(t, a.UserID, map[string]any{"name": "direct",
		"scopes": append(slices.Clone(presetAssistant), "mail:send"), "confirmScopes": []string{}})
	s3 := mcpConnect(t, srv, token3)
	sentDraft := newDraft(s3, "sent")
	mustOK(t, "send_draft", mcpCall(t, s3, "send_draft", map[string]any{"emailId": sentDraft}))

	for _, c := range []struct {
		name, id, want string
		sess           *mcptest.Session
	}{
		{"the owner's draft", userDraft, "not written by this API key", s},
		{"another key's draft", otherDraft, "not written by this API key", s},
		{"a draft waiting for approval", pendingDraft, "waiting for the account owner's approval", s},
		{"a submitted draft", sentDraft, "already been submitted", s3},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := h.liveDraftsOf(t, a.UserID)
			for _, tool := range []string{"draft_update", "draft_discard"} {
				args := map[string]any{"emailId": c.id}
				if tool == "draft_update" {
					args = map[string]any{"emailId": c.id, "to": []string{"z@example.net"}, "subject": "hijack", "body": "replaced"}
				}
				res, err := c.sess.Call(context.Background(), tool, args)
				if err != nil {
					t.Fatal(err)
				}
				if !res.IsError || !strings.Contains(res.Text, c.want) {
					t.Fatalf("%s of %s: %+v, want a refusal saying %q", tool, c.name, res, c.want)
				}
				if exists, tr := h.trashed(t, ulid.MustParse(c.id)); !exists || tr {
					t.Fatalf("%s of %s touched it: exists=%v trashed=%v", tool, c.name, exists, tr)
				}
			}
			if after := h.liveDraftsOf(t, a.UserID); after != before {
				t.Fatalf("a refused update still wrote a draft: %d -> %d live drafts", before, after)
			}
		})
	}
}

func TestMCPMarkdownDraftBody(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "md", "scopes": presetAssistant})
	s := mcpConnect(t, h.mcpServer(t, a.UserID), token)
	body := "Hi **Bob**,\nsee [this](javascript:alert(1)) and <script>alert(2)</script>\nThanks"
	id := emailID(t, mustOK(t, "draft_new", mcpCall(t, s, "draft_new", map[string]any{"from": addr,
		"to": []string{"bob@example.net"}, "subject": "md", "body": body})))
	storedText, storedHTML := alternativeParts(t, storedBytes(t, h.composeHarness, h.blobs, a.UserID, id))
	if strings.ReplaceAll(storedText, "\r\n", "\n") != body {
		t.Errorf("stored text/plain = %q, want the body exactly %q", storedText, body)
	}
	got := call(t, h.api.emailGet, a.UserID, map[string]any{"ids": []string{id},
		"properties": []string{"textBody", "htmlBody", "bodyValues"}, "fetchAllBodyValues": true})
	e := asObj(asArr(got["list"])[0])
	values := asObj(e["bodyValues"])
	part := func(list string) string {
		parts := asArr(e[list])
		if len(parts) != 1 {
			t.Fatalf("%s: %v", list, parts)
		}
		return asObj(values[asObj(parts[0])["partId"].(string)])["value"].(string)
	}
	text := strings.ReplaceAll(part("textBody"), "\r\n", "\n")
	if strings.TrimRight(text, "\n") != body {
		t.Errorf("text/plain = %q, want the body exactly %q", text, body)
	}
	html := part("htmlBody")
	for _, w := range []string{"<strong>Bob</strong>", "<br"} {
		if !strings.Contains(html, w) {
			t.Errorf("text/html lacks %q: %s", w, html)
		}
	}
	if strings.Contains(strings.ToLower(html), "javascript:") || strings.Contains(strings.ToLower(html), "<script") {
		t.Errorf("text/html carries active content: %s", html)
	}
	// The stored HTML part itself — what a recipient gets — not only the
	// read path's sanitised view of it.
	lower := strings.ToLower(storedHTML)
	if !strings.Contains(storedHTML, "<strong>Bob</strong>") || strings.Contains(lower, "javascript:") ||
		strings.Contains(lower, "<script") {
		t.Errorf("the stored text/html part: %s", storedHTML)
	}
}

// alternativeParts is the decoded text/plain and text/html parts of a
// multipart/alternative message; it fails the test for any other shape.
func alternativeParts(t *testing.T, raw []byte) (text, html string) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("the draft is %q, want multipart/alternative:\n%s", mt, raw)
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(p) // quoted-printable is decoded by the reader
		if err != nil {
			t.Fatal(err)
		}
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		switch ct {
		case "text/plain":
			text = string(b)
		case "text/html":
			html = string(b)
		}
	}
	return text, html
}

// emailChanges is the user's Email change_log rows, oldest first, as
// "id:op".
func (h *agentHarness) emailChanges(t *testing.T, user ulid.ULID) []string {
	t.Helper()
	rows, err := h.super.Query(context.Background(), `SELECT object_id || ':' || op FROM app.change_log
		WHERE user_id = $1 AND object_type = 'Email' ORDER BY seq`, user.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// A version the key replaced or discarded is in Trash, still draft-flagged and
// still the key's: it must not be sendable, or a confused agent holding the
// old id would send words it took back.
func TestMCPReplacedDraftIsNotSendable(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "sender",
		"scopes": append(slices.Clone(presetAssistant), "mail:send"), "confirmScopes": []string{}, "ratePerMinute": 600})
	s := mcpConnect(t, h.mcpServer(t, a.UserID), token)
	old := emailID(t, mustOK(t, "draft_new", mcpCall(t, s, "draft_new", map[string]any{"from": addr,
		"to": []string{"bob@example.net"}, "subject": "v1", "body": "first words"})))
	cur := emailID(t, mustOK(t, "draft_update", mcpCall(t, s, "draft_update", map[string]any{"emailId": old,
		"to": []string{"bob@example.net"}, "subject": "v2", "body": "second words"})))
	if r := mcpCall(t, s, "send_draft", map[string]any{"emailId": old}); !r.IsError || !strings.Contains(r.Text, "replaced or discarded") {
		t.Fatalf("send_draft of the replaced version: %+v", r)
	}
	gone := emailID(t, mustOK(t, "draft_new", mcpCall(t, s, "draft_new", map[string]any{"from": addr,
		"to": []string{"bob@example.net"}, "subject": "v3", "body": "thrown away"})))
	mustOK(t, "draft_discard", mcpCall(t, s, "draft_discard", map[string]any{"emailId": gone}))
	if r := mcpCall(t, s, "send_draft", map[string]any{"emailId": gone}); !r.IsError || !strings.Contains(r.Text, "replaced or discarded") {
		t.Fatalf("send_draft of a discarded draft: %+v", r)
	}
	// Positive control: the current version sends.
	if out := mustOK(t, "send_draft", mcpCall(t, s, "send_draft", map[string]any{"emailId": cur})); out["status"] != "submitted" {
		t.Fatalf("send_draft of the current version: %v", out)
	}
}

// A draft the key withdrew after asking to send it is refused at APPROVAL,
// with a reason the owner can read, rather than approved and then failing as
// "approved, but refused when run" (#874 review). draft_discard itself is
// refused while the send waits, so the race this closes — a discard that
// checked before the hold committed — is reproduced by moving the draft to
// Trash directly.
func TestMCPApprovalOfAWithdrawnDraftIsRefused(t *testing.T) {
	h, a, addr := newDraftFixture(t)
	ctx := context.Background()
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "asks",
		"scopes": append(slices.Clone(presetAssistant), "mail:send"), "confirmScopes": []string{"mail:send"}})
	s := mcpConnect(t, h.mcpServer(t, a.UserID), token)
	draft := emailID(t, mustOK(t, "draft_new", mcpCall(t, s, "draft_new", map[string]any{"from": addr,
		"to": []string{"bob@example.net"}, "subject": "S", "body": "b"})))
	out := mustOK(t, "send_draft", mcpCall(t, s, "send_draft", map[string]any{"emailId": draft}))
	pid, _ := out["pendingId"].(string)
	if out["status"] != "pending_approval" || pid == "" {
		t.Fatalf("send_draft in approval mode: %v", out)
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET trashed_at = now(), moved_mailbox_id = $2 WHERE id = $1`,
		draft, a.TrashID.String()); err != nil {
		t.Fatal(err)
	}

	res := call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid: map[string]any{"state": "approved"}}})
	se := asObj(asObj(res["notUpdated"])[pid])
	desc, _ := se["description"].(string)
	if se["type"] != setDraftChanged || !strings.Contains(desc, "discarded or replaced") {
		t.Fatalf("approving a send of a withdrawn draft: %v, want draftChanged naming the discard", res)
	}
	var state string
	_ = h.super.QueryRow(ctx, `SELECT state FROM app.api_key_pending WHERE id = $1`, pid).Scan(&state)
	var subs int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.submissions WHERE user_id = $1`, a.UserID.String()).Scan(&subs)
	if state != "failed" || subs != 0 {
		t.Fatalf("after the refused approval: state %q, %d submissions; want failed and none", state, subs)
	}
}
