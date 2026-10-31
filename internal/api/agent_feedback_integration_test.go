// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// #867: what an AI-tool key does with mail still happens, but never
// becomes an app.feedback_events row — the priority model learns from the
// USER's actions only. Every call below goes through the real bearer gate, so
// the key identity reaches the emitters exactly as in production.

import (
	"context"
	"testing"

	"mailroom/internal/store"
)

func feedbackRows(t *testing.T, h *agentHarness, msgID string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.feedback_events WHERE message_id = $1`, msgID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// NEGATIVE CONTROL: the same three actions through a SESSION each record a row
// (and the key's actions are asserted to have happened), so a zero for the key
// is the guard and not a seam that cannot record.
func TestKeyEmailSetEmitsNoFeedback(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	archiveID := store.NewULID()
	if _, err := h.super.Exec(ctx, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1,$2,$3,'Archive','Archive','\Archive')`, archiveID.String(), a.UserID.String(), a.AccountID.String()); err != nil {
		t.Fatal(err)
	}
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "mover",
		"scopes": []string{"mail:read", "mail:move", "mail:delete"}, "confirmScopes": []string{}})

	patches := []struct {
		name  string
		patch map[string]any
	}{
		{"flag", map[string]any{"keywords/$flagged": true}},
		{"trash", map[string]any{"mailboxIds/" + a.TrashID.String(): true}},
		{"archive unread", map[string]any{"mailboxIds/" + archiveID.String(): true}},
	}
	for _, p := range patches {
		viaKey := agentMessage(t, h, a).String()
		name, res := h.keyCall(t, tok, "Email/set", map[string]any{
			"update": map[string]any{viaKey: p.patch}})
		if name != "Email/set" || !mapHasKey(res, "updated", viaKey) {
			t.Fatalf("%s through the key did not happen: %s %v", p.name, name, res)
		}
		if n := feedbackRows(t, h, viaKey); n != 0 {
			t.Errorf("%s through a key recorded %d feedback rows, want 0", p.name, n)
		}

		viaSession := agentMessage(t, h, a).String()
		if _, mErr := h.api.emailSet(context.Background(), a.UserID,
			mustJSON(t, map[string]any{"update": map[string]any{viaSession: p.patch}})); mErr != nil {
			t.Fatalf("%s through the session: %+v", p.name, mErr)
		}
		if n := feedbackRows(t, h, viaSession); n != 1 {
			t.Errorf("control: %s through a session recorded %d feedback rows, want 1", p.name, n)
		}
	}
}

// A reply a key sends records no reply signal on the message it answers; the
// identical reply sent by the user's session does.
func TestKeySubmissionEmitsNoReplyFeedback(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	addr := "fb-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "sender",
		"scopes":                   []string{"mail:read", "mail:draft", "mail:send"},
		"acknowledgeSendAndDelete": true, "confirmScopes": []string{}})

	// The inbound original: a.MessageID, with a Message-ID the drafts answer.
	hint := "orig-" + a.UserID.String() + "@other.test"
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET message_id_hint = $2 WHERE id = $1`,
		a.MessageID.String(), hint); err != nil {
		t.Fatal(err)
	}
	replyArgs := func(subject string) map[string]any {
		args := draftCreateArgs(addr, subject)
		d := args["create"].(map[string]any)["d"].(map[string]any)
		d["inReplyTo"] = []string{hint}
		return args
	}

	// Through the key.
	_, res := h.keyCall(t, tok, "Email/set", replyArgs("key reply"))
	draft, ok := asObj(asObj(res["created"])["d"])["id"].(string)
	if !ok {
		t.Fatalf("key draft not created: %v", res)
	}
	name, res := h.keyCall(t, tok, "EmailSubmission/set", map[string]any{"create": map[string]any{
		"s": map[string]any{"emailId": draft}}})
	if name != "EmailSubmission/set" || !mapHasKey(res, "created", "s") {
		t.Fatalf("key send did not happen: %s %v", name, res)
	}
	if n := feedbackRows(t, h, a.MessageID.String()); n != 0 {
		t.Errorf("a reply sent by a key recorded %d feedback rows on the original, want 0", n)
	}

	// Control: the user's own reply to the same message records one.
	sres := call(t, h.api.emailSet, a.UserID, replyArgs("user reply"))
	userDraft, ok := asObj(asObj(sres["created"])["d"])["id"].(string)
	if !ok {
		t.Fatalf("session draft not created: %v", sres)
	}
	if _, mErr := invokeAs(t, &rlsHarness{composeHarness: h.composeHarness}, "EmailSubmission/set", a.UserID,
		map[string]any{"create": map[string]any{"s": map[string]any{"emailId": userDraft}}}); mErr != nil {
		t.Fatalf("session send: %+v", mErr)
	}
	if n := feedbackRows(t, h, a.MessageID.String()); n != 1 {
		t.Errorf("control: a reply sent by the user recorded %d feedback rows, want 1", n)
	}
}
