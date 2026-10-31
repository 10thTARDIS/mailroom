// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"strings"
	"testing"
)

// RECEIVE-ONLY (ADR-046 §12): a send on an account whose provider refuses
// SMTP AUTH (5.7.139) is refused up front with accountCannotSend and the
// reason, nothing durable is written, and Account/getHealth shows sendBlocked.
// The control: once the mark is cleared, the identical send is accepted and
// getHealth drops the section.
//
// PROBE: delete the SendBlocked refusal in submission.go and the first
// assertion goes red.
func TestReceiveOnlyAccountRefusesSendAndShowsInHealth(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	ctx := context.Background()
	if _, err := h.super.Exec(ctx, `INSERT INTO app.account_sync_status (account_id, user_id, send_blocked_code, send_blocked_at)
		VALUES ($1, $2, 'smtp_auth_disabled', now())`, a.AccountID.String(), a.UserID.String()); err != nil {
		t.Fatal(err)
	}
	emailID := composeDraft(t, h, a.UserID, "me@example.com", "blocked-send")
	res := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
	})
	e := setError(t, res, "notCreated", "s1")
	if e["type"] != setAccountCannotSend {
		t.Fatalf("SetError = %v, want %q", e, setAccountCannotSend)
	}
	if d, _ := e["description"].(string); !strings.Contains(d, "5.7.139") {
		t.Errorf("description %q does not explain the refusal", d)
	}
	if got := submissionRowStates(t, h, a.UserID, emailID); len(got) != 0 {
		t.Errorf("a refused send left submission rows %v", got)
	}

	health := call(t, h.api.accountGetHealth, a.UserID, map[string]any{})
	var entry map[string]any
	for _, x := range health["list"].([]any) {
		if m := x.(map[string]any); m["id"] == a.AccountID.String() {
			entry = m
		}
	}
	sb, ok := entry["sendBlocked"].(map[string]any)
	if !ok || sb["code"] != "smtp_auth_disabled" || sb["since"] == "" || !strings.Contains(sb["message"].(string), "administrator") {
		t.Fatalf("getHealth sendBlocked = %v", entry["sendBlocked"])
	}

	if _, err := h.super.Exec(ctx, `UPDATE app.account_sync_status SET send_blocked_code = NULL, send_blocked_at = NULL
		WHERE account_id = $1`, a.AccountID.String()); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
	})
	if !mapHasKey(res, "created", "s1") {
		t.Fatalf("after clearing, the send was refused: %v", res)
	}
	health = call(t, h.api.accountGetHealth, a.UserID, map[string]any{})
	for _, x := range health["list"].([]any) {
		if m := x.(map[string]any); m["id"] == a.AccountID.String() {
			if _, present := m["sendBlocked"]; present {
				t.Errorf("sendBlocked still reported after clearing: %v", m["sendBlocked"])
			}
		}
	}
}
