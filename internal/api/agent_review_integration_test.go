// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// The fixes from PR #841's security review (#49).

func (h *agentHarness) isDraft(t *testing.T, id ulid.ULID) bool {
	t.Helper()
	var d bool
	if err := h.super.QueryRow(context.Background(), `SELECT is_draft FROM app.messages WHERE id=$1`, id.String()).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// 1. Nothing becomes a draft by patch, and a destroy only ever erases a draft
// that lives in a Drafts folder.
func TestDraftFlagCannotArmAPermanentDelete(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	addr := "dr-" + a.UserID.String()[20:] + "@example.com"
	_, sentID := seedSendable(t, h.super, a, addr, "")

	// (a) A key with EVERY scope is refused any $draft change.
	all, _ := h.newKey(t, a.UserID, map[string]any{"name": "everything", "scopes": allScopes,
		"acknowledgeSendAndDelete": true, "confirmScopes": []string{}})
	name, res := h.keyCall(t, all, "Email/set", map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"keywords/$draft": true}}})
	wantForbidden(t, "a key setting $draft", name, res)

	// (b) A SESSION cannot make received mail a draft either.
	res = call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"keywords/$draft": true}}})
	if setError(t, res, "notUpdated", a.MessageID.String())["type"] != setForbidden || h.isDraft(t, a.MessageID) {
		t.Fatalf("a session made a received message a draft: %v", res)
	}
	// ...nor a Sent one, which would then pass the send's draft check.
	sent := agentMessage(t, h, a)
	if _, err := h.super.Exec(ctx, `UPDATE app.message_provenance SET mailbox_id=$2 WHERE message_id=$1`,
		sent.String(), sentID.String()); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{sent.String(): map[string]any{"keywords/$draft": true}}})
	if !mapHasKey(res, "notUpdated", sent.String()) || h.isDraft(t, sent) {
		t.Fatalf("a Sent message was made a draft: %v", res)
	}
	res = call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": sent.String()}}})
	if !mapHasKey(res, "notCreated", "s") {
		t.Fatalf("an old Sent message was submitted again: %v", res)
	}
	// Clearing the flag on a real draft stays allowed (positive control for b).
	real := createDraftAs(t, h, a, addr, "keep")
	res = call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{real.String(): map[string]any{"keywords/$draft": false}}})
	if !mapHasKey(res, "updated", real.String()) {
		t.Fatalf("clearing $draft on a draft was refused: %v", res)
	}

	// (c) A received message flagged draft BY OTHER MEANS (direct DB setup) is
	// not erased by a session's destroy: it goes to Trash.
	victim := agentMessage(t, h, a)
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET is_draft = true WHERE id=$1`, victim.String()); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.emailSet, a.UserID, map[string]any{"destroy": []string{victim.String()}})
	if exists, tr := h.trashed(t, victim); !exists || !tr || !strInArr(res["destroyed"], victim.String()) {
		t.Fatalf("destroy of a draft-flagged Inbox message: exists=%v trashed=%v res=%v — want kept, in Trash",
			exists, tr, res)
	}
	// Positive control: a genuine composed draft IS still retired.
	gone := createDraftAs(t, h, a, addr, "discard me")
	call(t, h.api.emailSet, a.UserID, map[string]any{"destroy": []string{gone.String()}})
	if exists, _ := h.trashed(t, gone); exists {
		t.Fatal("a genuine unsent draft was not retired: the check above proves nothing")
	}
}

// 2 (the store half is in internal/auth): see TestBearerGateLimitsFailuresPerAddress.

// 3. Reads are charged by volume.
func TestAgentKeyReadsAreChargedByVolume(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "bulk-read", "scopes": []string{"mail:read"},
		"dailyBudget": 5, "ratePerMinute": 600})
	ids := make([]string, 120) // ids need not exist: the cost is what was asked for
	for i := range ids {
		ids[i] = store.NewULID().String()
	}
	if name, res := h.keyCall(t, tok, "Email/get", map[string]any{"ids": ids, "properties": []string{"id"}}); name != "Email/get" {
		t.Fatalf("first 120-id get (3 units of 5): %s %v", name, res)
	}
	name, res := h.keyCall(t, tok, "Email/get", map[string]any{"ids": ids, "properties": []string{"id"}})
	if name != "error" || res["type"] != errOverQuota {
		t.Fatalf("second 120-id get: %s %v, want overQuota (6 units of 5)", name, res)
	}
	// A key's query always runs with a limit, charged up front (#49 review,
	// F3): the default when it sends none, the maximum when it asks for more.
	//
	// RED PROBE: make emailQueryNeeds charge readCost again and drop the
	// keyQueryLimit clamp in emailQueryWith, and the limit-less query below
	// returns all 60+ ids, and the over-limit one is admitted on a budget of 5.
	for range 60 {
		agentMessage(t, h, a)
	}
	tok2, key2 := h.newKey(t, a.UserID, map[string]any{"name": "lister", "scopes": []string{"mail:read"}})
	name, res = h.keyCall(t, tok2, "Email/query", map[string]any{})
	if name != "Email/query" || len(asArr(res["ids"])) != KeyQueryDefaultLimit || res["limit"] != float64(KeyQueryDefaultLimit) {
		t.Fatalf("a limit-less query by a key: %s ids=%d limit=%v, want %d ids and limit %d",
			name, len(asArr(res["ids"])), res["limit"], KeyQueryDefaultLimit, KeyQueryDefaultLimit)
	}
	spent := func(key string) int {
		var n int
		_ = h.super.QueryRow(context.Background(), `SELECT COALESCE(sum(cost), 0) FROM app.api_key_events WHERE key_id=$1`, key).Scan(&n)
		return n
	}
	if got := spent(key2); got != 1 {
		t.Fatalf("a default-limit query cost %d, want 1 unit", got)
	}
	// Positive control for the cap: an explicit small limit is honoured and
	// not echoed back as a server limit.
	_, res = h.keyCall(t, tok2, "Email/query", map[string]any{"limit": 7})
	if len(asArr(res["ids"])) != 7 || res["limit"] != nil {
		t.Fatalf("limit 7 by a key: ids=%d limit=%v, want 7 and no server limit", len(asArr(res["ids"])), res["limit"])
	}
	// More than the maximum is clamped and paid for before it runs: on a
	// budget of 5 it is refused outright rather than topped up afterwards.
	tok3, key3 := h.newKey(t, a.UserID, map[string]any{"name": "greedy", "scopes": []string{"mail:read"},
		"dailyBudget": 5, "ratePerMinute": 600})
	name, res = h.keyCall(t, tok3, "Email/query", map[string]any{"limit": 100000})
	if name != "error" || res["type"] != errOverQuota {
		t.Fatalf("a 100000-limit query on a budget of 5: %s %v, want overQuota up front", name, res)
	}
	tok4, _ := h.newKey(t, a.UserID, map[string]any{"name": "big", "scopes": []string{"mail:read"}})
	_, res = h.keyCall(t, tok4, "Email/query", map[string]any{"limit": 100000})
	if res["limit"] != float64(KeyQueryMaxLimit) {
		t.Fatalf("a 100000-limit query: limit=%v, want the server's %d", res["limit"], KeyQueryMaxLimit)
	}

	// Thread/get is charged by the email ids it returns, not per thread: every
	// agentMessage above shares the fixture's thread, so one thread is 60+
	// emails, 2+ units.
	//
	// RED PROBE: drop the Thread/get branch in readAnswerCost and this costs 1.
	tok5, key5 := h.newKey(t, a.UserID, map[string]any{"name": "threads", "scopes": []string{"mail:read"}})
	if name, res := h.keyCall(t, tok5, "Thread/get", map[string]any{"ids": []string{a.ThreadID.String()}}); name != "Thread/get" {
		t.Fatalf("Thread/get: %s %v", name, res)
	}
	if got := spent(key5); got < 2 {
		t.Fatalf("a Thread/get returning 60+ email ids cost %d, want at least 2 units", got)
	}
	_, _ = keyID, key3
}

// 4. A key creates submissions only from its own drafts, and never changes or
// cancels one.
func TestAgentKeySubmitsOnlyItsOwnDrafts(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	addr := "own-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "sender",
		"scopes": []string{"mail:read", "mail:draft", "mail:send"}, "confirmScopes": []string{}})
	usersDraft := createDraftAs(t, h, a, addr, "the user's own words")
	_, res := h.keyCall(t, tok, "EmailSubmission/set", map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": usersDraft.String()}}})
	if setError(t, res, "notCreated", "s")["type"] != setForbidden {
		t.Fatalf("a key sent the user's draft: %v", res)
	}
	// Positive control: its own draft goes.
	_, res = h.keyCall(t, tok, "Email/set", draftCreateArgs(addr, "mine"))
	own := asObj(asObj(res["created"])["d"])["id"].(string)
	_, res = h.keyCall(t, tok, "EmailSubmission/set", map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": own}}})
	sub, ok := asObj(res["created"])["s"].(map[string]any)
	if !ok {
		t.Fatalf("a key's own draft was not sent: %v", res)
	}
	// The user's scheduled sends (and the key's own) cannot be changed or cancelled.
	for _, args := range []map[string]any{
		{"update": map[string]any{sub["id"].(string): map[string]any{"undoStatus": "canceled"}}},
		{"destroy": []string{sub["id"].(string)}},
	} {
		name, res := h.keyCall(t, tok, "EmailSubmission/set", args)
		wantForbidden(t, "a key changing a submission", name, res)
	}
}

// 5. An approval that never ran is failed by the sweep.
func TestStuckApprovalIsFailedBySweep(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "stuck", "scopes": []string{"mail:read", "mail:delete"}})
	victim := agentMessage(t, h, a)
	_, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{victim.String()}})
	stuck := setError(t, res, "notDestroyed", victim.String())["pendingActionId"].(string)
	other := agentMessage(t, h, a)
	_, res = h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{other.String()}})
	ran := setError(t, res, "notDestroyed", other.String())["pendingActionId"].(string)

	// One approval runs normally: ran_at is set with its audit row.
	call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{ran: map[string]any{"state": "approved"}}})
	// The other is approved and then "the process stops": decided, never ran.
	if _, err := h.super.Exec(ctx, `UPDATE app.api_key_pending SET state='approved', decided_at = now() - interval '11 minutes'
		WHERE id=$1`, stuck); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.api_key_pending SET decided_at = now() - interval '11 minutes' WHERE id=$1`, ran); err != nil {
		t.Fatal(err)
	}
	if _, err := h.api.SweepPendingApprovals(ctx); err != nil {
		t.Fatal(err)
	}
	state := func(id string) string {
		var s string
		_ = h.super.QueryRow(ctx, `SELECT state FROM app.api_key_pending WHERE id=$1`, id).Scan(&s)
		return s
	}
	if state(stuck) != store.PendingStateFailed {
		t.Errorf("an approval that never ran is %q after the sweep, want failed", state(stuck))
	}
	if state(ran) != store.PendingStateApproved {
		t.Errorf("an approval that ran is %q after the sweep, want approved", state(ran))
	}
	if _, tr := h.trashed(t, victim); tr {
		t.Error("the stuck request ran after all")
	}
	_ = keyID
}

// 6. A restricted key's session names none of the user's addresses.
func TestRestrictedKeySessionNamesNoAccount(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	var email string
	_ = h.super.QueryRow(context.Background(), `SELECT email FROM app.users WHERE id=$1`, a.UserID.String()).Scan(&email)
	session := func(tok string) string {
		r := httptest.NewRequest(http.MethodGet, "/jmap/session", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("session: %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	open, _ := h.newKey(t, a.UserID, map[string]any{"name": "open", "scopes": []string{"mail:read"}})
	if !strings.Contains(session(open), email) {
		t.Fatalf("an unrestricted key's session lacks the login address %q: the check below proves nothing", email)
	}
	restricted, _ := h.newKey(t, a.UserID, map[string]any{"name": "restricted", "scopes": []string{"mail:read"},
		"accountIds": []string{a.AccountID.String()}})
	if body := session(restricted); strings.Contains(body, email) {
		t.Errorf("a restricted key's session names the login address: %s", body)
	}
}

// 7. The account restriction follows where a message IS, not where it was.
func TestAccountRestrictionFollowsAMove(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	acct2, mbox2 := store.NewULID(), store.NewULID()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		  VALUES ($1,$2,'second','mail.example.com','smtp.example.com',$3,$4)`,
			[]any{acct2.String(), a.UserID.String(), []byte{1}, store.NewULID().String()}},
		{`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		  VALUES ($1,$2,$3,'Archive','Archive 2','\Archive')`, []any{mbox2.String(), a.UserID.String(), acct2.String()}},
	} {
		if _, err := h.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	old, _ := h.newKey(t, a.UserID, map[string]any{"name": "old", "scopes": []string{"mail:read"},
		"accountIds": []string{a.AccountID.String()}})
	now, _ := h.newKey(t, a.UserID, map[string]any{"name": "new", "scopes": []string{"mail:read"},
		"accountIds": []string{acct2.String()}})
	sees := func(tok string) bool {
		_, res := h.keyCall(t, tok, "Email/get", map[string]any{"ids": []string{a.MessageID.String()},
			"properties": []string{"id"}})
		return len(asArr(res["list"])) == 1
	}
	if !sees(old) || sees(now) {
		t.Fatal("before the move: the account-1 key must see it and the account-2 key must not")
	}
	res := call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{
		a.MessageID.String(): map[string]any{"mailboxIds": map[string]any{mbox2.String(): true}}}})
	if !mapHasKey(res, "updated", a.MessageID.String()) {
		t.Fatalf("move: %v", res)
	}
	var moved *string
	_ = h.super.QueryRow(ctx, `SELECT moved_mailbox_id FROM app.messages WHERE id=$1`, a.MessageID.String()).Scan(&moved)
	if moved == nil || *moved != mbox2.String() {
		t.Fatalf("the move did not set moved_mailbox_id (%v): this test is not testing a move", moved)
	}
	if sees(old) {
		t.Error("after a move into account 2, a key restricted to account 1 still sees the message")
	}
	if !sees(now) {
		t.Error("after a move into account 2, a key restricted to account 2 does not see it")
	}
	_ = sha256.Sum256
	_ = time.Now
}

// An approval whose request is already marked as run is not run again (#49
// review, F5): the admission refuses when MarkAPIKeyPendingRan changes no row.
//
// RED PROBE: ignore MarkAPIKeyPendingRan's ran result in admitKeyCall and the
// message is trashed.
func TestApprovalAlreadyRunIsNotRunAgain(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "twice", "scopes": []string{"mail:read", "mail:delete"}})
	victim := agentMessage(t, h, a)
	_, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{victim.String()}})
	pid := setError(t, res, "notDestroyed", victim.String())["pendingActionId"].(string)
	// The request carries a run already (a replay, or a run racing this one).
	if _, err := h.super.Exec(ctx, `UPDATE app.api_key_pending SET ran_at = now() WHERE id=$1`, pid); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid: map[string]any{"state": "approved"}}})
	if _, tr := h.trashed(t, victim); tr {
		t.Fatalf("an approval already marked as run ran again: %v", res)
	}
	// Positive control: a fresh approval of the same kind does run.
	other := agentMessage(t, h, a)
	_, res = h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{other.String()}})
	pid2 := setError(t, res, "notDestroyed", other.String())["pendingActionId"].(string)
	call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid2: map[string]any{"state": "approved"}}})
	if _, tr := h.trashed(t, other); !tr {
		t.Fatal("a fresh approval did not run: the refusal above proves nothing")
	}
}
