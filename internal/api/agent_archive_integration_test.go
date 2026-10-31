// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// otherAccountMessage seeds a second account for f's user with one message in
// it, and returns the account, its inbox and the message.
func otherAccountMessage(t *testing.T, h *agentHarness, f apiFixture) (acct, mbox, msg ulid.ULID) {
	t.Helper()
	acct, mbox, msg = store.NewULID(), store.NewULID(), store.NewULID()
	hash := sha256.Sum256([]byte("other-" + msg.String()))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		  VALUES ($1,$2,'second','mail.example.com','smtp.example.com',$3,$4)`,
			[]any{acct.String(), f.UserID.String(), []byte{1}, store.NewULID().String()}},
		{`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		  VALUES ($1,$2,$3,'INBOX','Inbox 2','\Inbox')`, []any{mbox.String(), f.UserID.String(), acct.String()}},
		{`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,1,1,$3)`,
			[]any{f.UserID.String(), hash[:], store.NewULID().String()}},
		{`INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
		  VALUES ($1,$2,$3,'other account', now(), 1)`, []any{msg.String(), f.UserID.String(), hash[:]}},
		{`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		  VALUES ($1,$2,$3,$4,$5,$6,1)`,
			[]any{store.NewULID().String(), f.UserID.String(), msg.String(), acct.String(), mbox.String(), testUID()}},
	} {
		if _, err := h.super.Exec(context.Background(), q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return acct, mbox, msg
}

// A restricted key's Send & archive list stays inside its accounts (#49
// review): the restriction is the application's, not RLS's, and
// archiveOnSuccess was checked against nothing, so a key restricted to one
// account could archive up to 100 messages of another as a side effect of a
// send. Checked when the send is created, when it is held, and again when an
// approved send runs.
//
// RED PROBE: remove the keyMayArchive calls in createSubmission and holdSend
// and the other-account archive is accepted (a submission recording it, a
// held request), and the approval after the move runs.
func TestRestrictedKeyArchivesOnlyInsideItsAccounts(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	addr := "arc-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	acct2, mbox2, outside := otherAccountMessage(t, h, a)
	inside := agentMessage(t, h, a)
	scopes := []string{"mail:read", "mail:draft", "mail:send", "mail:move"}
	restricted := map[string]any{"scopes": scopes, "acknowledgeSendAndDelete": true,
		"accountIds": []string{a.AccountID.String()}}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range restricted {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	submissions := func() int {
		var n int
		_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.submissions WHERE user_id=$1`, a.UserID.String()).Scan(&n)
		return n
	}
	draftBy := func(tok, subject string) string {
		t.Helper()
		_, res := h.keyCall(t, tok, "Email/set", draftCreateArgs(addr, subject))
		id, ok := asObj(asObj(res["created"])["d"])["id"].(string)
		if !ok {
			t.Fatalf("key draft not created: %v", res)
		}
		return id
	}
	submit := func(tok, draft string, archive ulid.ULID) map[string]any {
		_, res := h.keyCall(t, tok, "EmailSubmission/set", map[string]any{"create": map[string]any{
			"s": map[string]any{"emailId": draft, "archiveOnSuccess": []string{archive.String()}}}})
		return res
	}

	// Immediate sends.
	now, _ := h.newKey(t, a.UserID, with(map[string]any{"name": "now", "confirmScopes": []string{}}))
	before := submissions()
	res := submit(now, draftBy(now, "out"), outside)
	if se := setError(t, res, "notCreated", "s"); se["type"] != setForbidden || len(asArr(se["properties"])) != 1 ||
		asArr(se["properties"])[0] != "archiveOnSuccess" {
		t.Fatalf("a restricted key archiving another account's message: %v, want forbidden on archiveOnSuccess", res)
	}
	if submissions() != before {
		t.Fatal("a refused send still recorded a submission, whose archive would run once sent")
	}
	// Positive control: its own account's message is accepted and recorded
	// for archiving once sent (the archive itself is the send worker's,
	// internal/compose's send_archive_integration_test.go).
	res = submit(now, draftBy(now, "in"), inside)
	if _, ok := asObj(res["created"])["s"]; !ok {
		t.Fatalf("a restricted key archiving its own account's message: %v, want created", res)
	}
	var recorded int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.submissions WHERE user_id=$1 AND archive_on_success = ARRAY[$2]::app.ulid[]`,
		a.UserID.String(), inside.String()).Scan(&recorded)
	if recorded != 1 {
		t.Fatalf("submissions recording the inside archive = %d, want 1", recorded)
	}

	// Held sends: refused at hold time...
	ask, _ := h.newKey(t, a.UserID, with(map[string]any{"name": "ask"}))
	res = submit(ask, draftBy(ask, "held out"), outside)
	if se := setError(t, res, "notCreated", "s"); se["type"] != setForbidden {
		t.Fatalf("a held send archiving another account's message: %v, want forbidden at hold time", res)
	}
	// ...shown to the approver when allowed...
	moving := agentMessage(t, h, a)
	res = submit(ask, draftBy(ask, "held in"), moving)
	pid, _ := setError(t, res, "notCreated", "s")["pendingActionId"].(string)
	list := asArr(call(t, h.api.pendingGet, a.UserID, map[string]any{"ids": []string{pid}})["list"])
	if len(list) != 1 {
		t.Fatalf("held send %q not listed: %v", pid, res)
	}
	pp := asObj(list[0])
	if arc := asObj(pp["archive"]); arc["count"] != float64(1) || len(asArr(arc["subjects"])) != 1 ||
		!strings.Contains(pp["summary"].(string), "archive 1 message") {
		t.Fatalf("held send's archive = %v, summary %q: want 1 message shown", pp["archive"], pp["summary"])
	}
	// ...and checked again when the approved send runs: the message has moved
	// to the other account since.
	if _, err := h.super.Exec(ctx, `UPDATE app.message_provenance SET account_id=$2, mailbox_id=$3 WHERE message_id=$1`,
		moving.String(), acct2.String(), mbox2.String()); err != nil {
		t.Fatal(err)
	}
	before = submissions()
	call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid: map[string]any{"state": "approved"}}})
	if submissions() != before {
		t.Fatal("an approved send ran although its archive list had left the key's accounts")
	}
}
