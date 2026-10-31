// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// keywordIntents is every keyword writeback intent queued for one message, as
// "op keyword".
func (h *agentHarness) keywordIntents(t *testing.T, id ulid.ULID) []string {
	t.Helper()
	rows, err := h.super.Query(context.Background(), `
		SELECT op || ' ' || COALESCE(payload->>'keyword', '')
		  FROM app.writeback_intents
		 WHERE message_id = $1 AND op IN ('+kw', '-kw')
		 ORDER BY created_at, id`, id.String())
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

// An IMAP system flag is not a keyword (#49 review, F1). `keywords/\Draft`
// used to become a raw keyword, written back as `UID STORE +FLAGS (\Draft)`
// and read by the next sync as is_draft — a draft made by a route the $draft
// checks never saw, needing only mail:move, which the user's next ordinary
// delete would then erase everywhere. Refused for keys AND sessions, at the
// patch, before any intent is queued.
//
// RED PROBE: drop the validJMAPKeyword checks in parseEmailPatch (and the
// store backstop in ApplyEmailUpdate) and the refusals below fail with the
// patch "updated" and a "+kw \Draft" intent queued.
func TestSystemFlagCannotBeSetAsAKeyword(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "organiser", "scopes": []string{"mail:read", "mail:move"}})

	refused := []map[string]any{
		{`keywords/\Draft`: true},
		{`keywords/\draft`: true},
		{`keywords/\Deleted`: true},
		{`keywords/\Seen`: nil},
		{"keywords": map[string]any{`\Draft`: true, "$seen": true}},
		{"keywords/has space": true},
		{"keywords/paren(": true},
	}
	for i, patch := range refused {
		// A key with mail:move — the scope the bypass needed.
		msg := agentMessage(t, h, a)
		name, res := h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{msg.String(): patch}})
		if name != "Email/set" {
			t.Fatalf("patch %d by a key: %s %v, want an Email/set answer with the object refused", i, name, res)
		}
		if se := setError(t, res, "notUpdated", msg.String()); se["type"] != setInvalidProps {
			t.Errorf("patch %d %v by a key: %v, want invalidProperties", i, patch, res)
		}
		if got := h.keywordIntents(t, msg); len(got) != 0 {
			t.Errorf("patch %d by a key queued keyword writeback %v", i, got)
		}
		if h.isDraft(t, msg) {
			t.Errorf("patch %d by a key made the message a draft", i)
		}

		// A browser session: the same chain was open to it.
		msg = agentMessage(t, h, a)
		res = call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{msg.String(): patch}})
		if se := setError(t, res, "notUpdated", msg.String()); se["type"] != setInvalidProps {
			t.Errorf("patch %d %v by a session: %v, want invalidProperties", i, patch, res)
		}
		if got := h.keywordIntents(t, msg); len(got) != 0 {
			t.Errorf("patch %d by a session queued keyword writeback %v", i, got)
		}
	}

	// Positive controls: an ordinary keyword is still set and written back, by
	// both — so the refusals above are about the keyword, not the route.
	msg := agentMessage(t, h, a)
	_, res := h.keyCall(t, tok, "Email/set", map[string]any{
		"update": map[string]any{msg.String(): map[string]any{"keywords/MyTag": true}}})
	if !mapHasKey(res, "updated", msg.String()) {
		t.Fatalf("a key could not set an ordinary keyword: %v", res)
	}
	if got := h.keywordIntents(t, msg); len(got) != 1 || got[0] != "+kw MyTag" {
		t.Fatalf("keyword intents after keywords/MyTag = %v, want [+kw MyTag]", got)
	}
	msg = agentMessage(t, h, a)
	res = call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{msg.String(): map[string]any{"keywords/$Project-7": true}}})
	if !mapHasKey(res, "updated", msg.String()) {
		t.Fatalf("a session could not set an ordinary keyword: %v", res)
	}
	if got := h.keywordIntents(t, msg); len(got) != 1 || got[0] != "+kw $Project-7" {
		t.Fatalf("keyword intents after keywords/$Project-7 = %v, want [+kw $Project-7]", got)
	}
}

// A key's whole-object `keywords` replacement leaves draft state alone (#49
// review, F2) instead of being refused: omitting $draft used to read as "clear
// it", a change no key may make, so every such patch from a key was forbidden.
//
// RED PROBE: drop the keyCaller skip in parseEmailPatch and the first call
// fails with "this change is not available to API keys".
func TestKeyKeywordsReplacementLeavesDraftAlone(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	addr := "kw-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "organiser", "scopes": []string{"mail:read", "mail:move"}})
	seen := func(id ulid.ULID) bool {
		var s bool
		if err := h.super.QueryRow(context.Background(), `SELECT is_seen FROM app.messages WHERE id=$1`, id.String()).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	msg := agentMessage(t, h, a)
	name, res := h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{
		msg.String(): map[string]any{"keywords": map[string]any{"$seen": true, "Replaced": true}}}})
	if name != "Email/set" || !mapHasKey(res, "updated", msg.String()) || !seen(msg) {
		t.Fatalf("a key's keywords replacement: %s %v (seen=%v), want updated", name, res, seen(msg))
	}
	if got := h.keywordIntents(t, msg); len(got) != 1 || got[0] != "+kw Replaced" {
		t.Errorf("keyword intents = %v, want [+kw Replaced]", got)
	}

	// Naming $draft does not make a received message a draft...
	msg = agentMessage(t, h, a)
	_, res = h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{
		msg.String(): map[string]any{"keywords": map[string]any{"$draft": true, "$seen": true}}}})
	if !mapHasKey(res, "updated", msg.String()) || h.isDraft(t, msg) {
		t.Fatalf("keywords {$draft, $seen} by a key: %v draft=%v, want updated and not a draft", res, h.isDraft(t, msg))
	}
	// ...and omitting it does not clear a real draft.
	draft := createDraftAs(t, h, a, addr, "still a draft")
	_, res = h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{
		draft.String(): map[string]any{"keywords": map[string]any{"$flagged": true}}}})
	if !mapHasKey(res, "updated", draft.String()) || !h.isDraft(t, draft) {
		t.Fatalf("keywords {$flagged} on a draft by a key: %v draft=%v, want updated and still a draft", res, h.isDraft(t, draft))
	}

	// A session's replacement still means what it says: omitting $draft
	// clears it (positive control that the skip is the key's alone).
	_ = call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{
		draft.String(): map[string]any{"keywords": map[string]any{"$seen": true}}}})
	if h.isDraft(t, draft) {
		t.Fatal("a session's keywords replacement without $draft left the draft flag: the skip is not key-only")
	}
}

// A held send shows the person deciding what it carries (#49 review, F4): a
// key can attach any blob of the user's, and a summary of only subject and
// recipients would let it ask "send this to alice?" about a payroll export.
//
// RED PROBE: drop Attachments from the held payload (and the summary) in
// holdSend and the assertions on attachments fail.
func TestHeldSendShowsItsAttachments(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	addr := "att-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	asker, _ := h.newKey(t, a.UserID, map[string]any{"name": "asker",
		"scopes": []string{"mail:read", "mail:draft", "mail:send"}, "acknowledgeSendAndDelete": true})

	// Any blob of the user's: here, the bytes of one of their own drafts.
	source := createDraftAs(t, h, a, addr, "the user's private words")
	got := call(t, h.api.emailGet, a.UserID, map[string]any{"ids": []string{source.String()}, "properties": []string{"blobId"}})
	blobID := asObj(asArr(got["list"])[0])["blobId"].(string)

	hold := func(args map[string]any) map[string]any {
		t.Helper()
		_, res := h.keyCall(t, asker, "Email/set", args)
		draft, ok := asObj(asObj(res["created"])["d"])["id"].(string)
		if !ok {
			t.Fatalf("key draft not created: %v", res)
		}
		_, res = h.keyCall(t, asker, "EmailSubmission/set", map[string]any{
			"create": map[string]any{"s": map[string]any{"emailId": draft}}})
		pid, _ := setError(t, res, "notCreated", "s")["pendingActionId"].(string)
		list := asArr(call(t, h.api.pendingGet, a.UserID, map[string]any{"ids": []string{pid}})["list"])
		if len(list) != 1 {
			t.Fatalf("pending request %q not listed", pid)
		}
		return asObj(list[0])
	}

	args := draftCreateArgs(addr, "quarterly numbers")
	asObj(asObj(args["create"])["d"])["attachments"] = []map[string]any{
		{"blobId": blobID, "type": "application/pdf", "name": "payroll.pdf"}}
	pp := hold(args)
	atts := asObj(pp["attachments"])
	if atts["count"] != float64(1) || len(asArr(atts["names"])) != 1 || asArr(atts["names"])[0] != "payroll.pdf" ||
		atts["totalSize"].(float64) <= 0 {
		t.Fatalf("held send's attachments = %v, want one named payroll.pdf with a size", pp["attachments"])
	}
	// The line counts the attachment; its NAME is key-written text, carried in
	// the structured attachments field above and kept out of the line (#852).
	if s := pp["summary"].(string); !strings.Contains(s, "1 attachment") || strings.Contains(s, "payroll.pdf") {
		t.Fatalf("held send's summary %q: want the attachment counted and not named", s)
	}

	// The same blob as an inline text part with a Content-ID: no display
	// classification may hide it from the approver (F4 follow-up).
	//
	// RED PROBE: return attachproj.Rows from PlannedSubmit.Attachments (the
	// first version of this fix) and this shows count 0.
	srcSize := asObj(asArr(call(t, h.api.emailGet, a.UserID, map[string]any{"ids": []string{source.String()},
		"properties": []string{"size"}})["list"])[0])["size"]
	args = draftCreateArgs(addr, "see inline")
	asObj(asObj(args["create"])["d"])["attachments"] = []map[string]any{
		{"blobId": blobID, "type": "text/plain", "name": "notes.txt", "disposition": "inline", "cid": "n1@example.com"}}
	pp = hold(args)
	atts = asObj(pp["attachments"])
	if atts["count"] != float64(1) || len(asArr(atts["names"])) != 1 || asArr(atts["names"])[0] != "notes.txt" ||
		atts["totalSize"] != srcSize {
		t.Fatalf("held send with an inline text attachment: attachments = %v (source size %v), want one notes.txt of that size",
			pp["attachments"], srcSize)
	}

	// Control: a send with none says none, rather than omitting the field.
	pp = hold(draftCreateArgs(addr, "plain"))
	if atts := asObj(pp["attachments"]); atts["count"] != float64(0) || strings.Contains(pp["summary"].(string), "attachment") {
		t.Fatalf("a send with no attachments: attachments=%v summary=%q", pp["attachments"], pp["summary"])
	}
}

// System keywords match in any casing (RFC 8621 §4.1.1; #49 review). Exact
// matching let `$JUNK`, bare `junk` and `$Draft` fall through to raw
// keywords, written back to the server and read by the next sync as is_junk
// (ingest folds case), around the refusal to set junk and the draft guard.
//
// RED PROBE: switch setKeyword back to the exact name (and drop the
// KeywordReserved backstop in applyEmailUpdate) and the $JUNK/junk cases are
// "updated" with a "+kw $JUNK" intent queued, and $Draft from a session too.
func TestSystemKeywordsMatchInAnyCase(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "organiser", "scopes": []string{"mail:read", "mail:move"}})
	junk := func(id ulid.ULID) bool {
		var j bool
		if err := h.super.QueryRow(ctx, `SELECT is_junk FROM app.messages WHERE id=$1`, id.String()).Scan(&j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	type outcome int
	const (
		objectRefused outcome = iota // notUpdated[id]
		methodRefused                // a method-level forbidden (a key's $draft)
	)
	for _, tc := range []struct {
		patch      string
		key, sess  outcome
		wantSesErr string
	}{
		{"keywords/$JUNK", objectRefused, objectRefused, setInvalidPatch},
		{"keywords/$Junk", objectRefused, objectRefused, setInvalidPatch},
		{"keywords/junk", objectRefused, objectRefused, setInvalidPatch},
		{"keywords/JUNK", objectRefused, objectRefused, setInvalidPatch},
		{"keywords/$NotJunk", objectRefused, objectRefused, setInvalidProps},
		{"keywords/nonjunk", objectRefused, objectRefused, setInvalidProps},
		{"keywords/$Draft", methodRefused, objectRefused, setForbidden},
		{"keywords/$DRAFT", methodRefused, objectRefused, setForbidden},
	} {
		msg := agentMessage(t, h, a)
		name, res := h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{msg.String(): map[string]any{tc.patch: true}}})
		switch tc.key {
		case methodRefused:
			wantForbidden(t, "a key's "+tc.patch, name, res)
		default:
			if name != "Email/set" || !mapHasKey(res, "notUpdated", msg.String()) {
				t.Errorf("%s: true by a key: %s %v, want the object refused", tc.patch, name, res)
			}
		}
		if got := h.keywordIntents(t, msg); len(got) != 0 || junk(msg) || h.isDraft(t, msg) {
			t.Errorf("%s by a key: intents %v junk=%v draft=%v, want none", tc.patch, got, junk(msg), h.isDraft(t, msg))
		}

		msg = agentMessage(t, h, a)
		res = call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{msg.String(): map[string]any{tc.patch: true}}})
		if se := setError(t, res, "notUpdated", msg.String()); se["type"] != tc.wantSesErr {
			t.Errorf("%s: true by a session: %v, want %s", tc.patch, res, tc.wantSesErr)
		}
		if got := h.keywordIntents(t, msg); len(got) != 0 || junk(msg) || h.isDraft(t, msg) {
			t.Errorf("%s by a session: intents %v junk=%v draft=%v, want none", tc.patch, got, junk(msg), h.isDraft(t, msg))
		}
	}

	// Positive controls. A mixed-case system keyword reaches its OWN field:
	// $Seen marks the message read and queues a flag, not a raw keyword.
	msg := agentMessage(t, h, a)
	_, res := h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{msg.String(): map[string]any{"keywords/$Seen": true}}})
	var seen bool
	_ = h.super.QueryRow(ctx, `SELECT is_seen FROM app.messages WHERE id=$1`, msg.String()).Scan(&seen)
	if !mapHasKey(res, "updated", msg.String()) || !seen || len(h.keywordIntents(t, msg)) != 0 {
		t.Fatalf("keywords/$Seen by a key: %v seen=%v intents=%v, want read with no raw keyword", res, seen, h.keywordIntents(t, msg))
	}
	// A mixed-case label atom (stored as "MailroomLabel-…", matched exactly)
	// still applies its label: only the system comparison is folded.
	created := call(t, h.api.labelSet, a.UserID, map[string]any{"create": map[string]any{"l": map[string]any{"name": "Case Test"}}})
	lid := asObj(asObj(created["created"])["l"])["id"].(string)
	got := call(t, h.api.labelGet, a.UserID, map[string]any{"ids": []string{lid}})
	atom, _ := asObj(asArr(got["list"])[0])["keyword"].(string)
	if atom == "" || atom == strings.ToLower(atom) {
		t.Fatalf("label atom %q: want a mixed-case atom for this control", atom)
	}
	msg = agentMessage(t, h, a)
	_, res = h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{msg.String(): map[string]any{"keywords/" + atom: true}}})
	var labelled int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`, msg.String(), lid).Scan(&labelled)
	if !mapHasKey(res, "updated", msg.String()) || labelled != 1 {
		t.Fatalf("keywords/%s by a key: %v labelled=%d, want the label applied", atom, res, labelled)
	}
}
