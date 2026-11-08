// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the JMAP conformance fixes the third-party client job
// found. Each asserts the behaviour with the real store under RLS, in the shape
// a stock client sees it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

func (h *agentHarness) sessionAs(t *testing.T, token string) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/jmap/session", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("session: HTTP %d %q", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func accountReadOnly(t *testing.T, session map[string]any) bool {
	t.Helper()
	accts := asObj(session["accounts"])
	if len(accts) != 1 {
		t.Fatalf("accounts = %v", accts)
	}
	for _, a := range accts {
		return asObj(a)["isReadOnly"] == true
	}
	return false
}

func TestSessionUrlsAreAbsoluteAndTemplatesSurvive(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "ro", "scopes": []string{"mail:read"}})

	check := func(label string, s map[string]any, origin string) {
		t.Helper()
		want := map[string]string{
			"apiUrl":         origin + "/jmap",
			"downloadUrl":    origin + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
			"uploadUrl":      origin + "/jmap/upload/{accountId}/",
			"eventSourceUrl": origin + "/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
			"previewUrl":     origin + "/jmap/preview/{accountId}/{blobId}/{name}",
		}
		for k, v := range want {
			if s[k] != v {
				t.Errorf("%s: %s = %v, want %s", label, k, s[k], v)
			}
		}
	}
	// No public origin configured: the request's own (httptest says example.com).
	check("request origin", h.sessionAs(t, tok), "http://example.com")

	// Configured: that origin, whatever the request's Host was.
	h.api.publicOrigin = "https://mail.example.com"
	check("configured origin", h.sessionAs(t, tok), "https://mail.example.com")

	// And the same for a signed-in session.
	rec := httptest.NewRecorder()
	auth.DevAuth(http.HandlerFunc(h.api.SessionHandler), a.UserID).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/session", nil))
	var sess map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	check("session user", sess, "https://mail.example.com")
}

func TestSessionIsReadOnlyFollowsTheKeyScopes(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ro, _ := h.newKey(t, a.UserID, map[string]any{"name": "ro", "scopes": []string{"mail:read", "labels:read", "search:read"}})
	draft, _ := h.newKey(t, a.UserID, map[string]any{"name": "draft", "scopes": []string{"mail:read", "mail:draft"}})
	mv, _ := h.newKey(t, a.UserID, map[string]any{"name": "mv", "scopes": []string{"mail:read", "mail:move"}})

	if !accountReadOnly(t, h.sessionAs(t, ro)) {
		t.Error("a key with only read scopes: isReadOnly = false")
	}
	if accountReadOnly(t, h.sessionAs(t, draft)) {
		t.Error("a key that can write drafts is read-only")
	}
	if accountReadOnly(t, h.sessionAs(t, mv)) {
		t.Error("a key that can move mail is read-only")
	}
	rec := httptest.NewRecorder()
	auth.DevAuth(http.HandlerFunc(h.api.SessionHandler), a.UserID).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/session", nil))
	var sess map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if accountReadOnly(t, sess) {
		t.Error("a signed-in session is read-only")
	}
}

// seedMoreMail adds n inbox messages to the fixture user.
func seedMoreMail(t *testing.T, h *composeHarness, f apiFixture, n int) []ulid.ULID {
	t.Helper()
	var out []ulid.ULID
	for range n {
		out = append(out, seedRawMessage(t, h, f, []byte("From: A <a@example.com>\r\nSubject: s"+
			store.NewULID().String()+"\r\n\r\nbody\r\n")))
	}
	return out
}

func TestEmailQueryNegativePosition(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedMoreMail(t, h, a, 3) // four messages in all

	q := func(args map[string]any) map[string]any {
		args["calculateTotal"] = true
		return call(t, h.api.emailQuery, a.UserID, args)
	}
	for _, c := range []struct {
		name         string
		position     int
		limit        int
		wantPosition int
		wantIDs      int
	}{
		{"-1 is the last", -1, 5, 3, 1},
		{"-2 is the last two", -2, 5, 2, 2},
		{"-4 is the start", -4, 10, 0, 4},
		{"beyond the start clamps to 0", -100, 10, 0, 4},
		{"limit still applies", -3, 1, 1, 1},
	} {
		res := q(map[string]any{"position": c.position, "limit": c.limit})
		if int(res["position"].(float64)) != c.wantPosition || len(asArr(res["ids"])) != c.wantIDs || res["total"] != float64(4) {
			t.Errorf("%s: position %v ids %d total %v, want %d/%d/4", c.name, res["position"], len(asArr(res["ids"])), res["total"], c.wantPosition, c.wantIDs)
		}
	}
	// The resolved window is the SAME one the equivalent positive position gives.
	neg := q(map[string]any{"position": -2, "limit": 5})
	pos := q(map[string]any{"position": 2, "limit": 5})
	if !slices.Equal(toStrings(neg["ids"]), toStrings(pos["ids"])) {
		t.Errorf("position -2 = %v, position 2 = %v", neg["ids"], pos["ids"])
	}
	// Collapse-threads takes a different statement; it must resolve too.
	res := q(map[string]any{"position": -1, "limit": 5, "collapseThreads": true})
	if res["position"] != float64(0) || len(asArr(res["ids"])) != 1 {
		t.Errorf("collapseThreads position -1: %v", res)
	}
}

func TestGetMalformedIDsAreNotFound(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	for name, fn := range map[string]methodFn{
		"Email/get": h.api.emailGet, "Thread/get": h.api.threadGet, "Mailbox/get": h.api.mailboxGet,
		"Label/get": h.api.methods["Label/get"], "Identity/get": h.api.methods["Identity/get"],
	} {
		args := map[string]any{"ids": []string{"not-an-id", a.MessageID.String()}}
		if name != "Email/get" {
			args = map[string]any{"ids": []string{"not-an-id"}}
		}
		res := call(t, fn, a.UserID, args)
		nf := toStrings(res["notFound"])
		if len(nf) != 1 || nf[0] != "not-an-id" {
			t.Errorf("%s: notFound = %v, want exactly [not-an-id]", name, nf)
		}
	}
	// The valid id next to it still comes back in list, not notFound.
	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{"not-an-id", a.MessageID.String()}, "properties": []string{"id"}})
	if len(asArr(res["list"])) != 1 {
		t.Errorf("list = %v", res["list"])
	}
}

func TestEmailMessageIDProperties(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	// The fixture message has a Message-ID hint with brackets and nothing else.
	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{a.MessageID.String()}, "properties": []string{"id", "messageId", "inReplyTo", "references"}})
	m := asObj(asArr(res["list"])[0])
	if ids := toStrings(m["messageId"]); len(ids) != 1 || strings.ContainsAny(ids[0], "<>") {
		t.Errorf("messageId = %v, want one id without brackets", m["messageId"])
	}
	for _, k := range []string{"inReplyTo", "references"} {
		if v, present := m[k]; !present || v != nil {
			t.Errorf("%s = %#v (present %v), want null", k, v, present)
		}
	}
	// A reply: both the chain and the parent come back bracket-free.
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET in_reply_to = '<p@example.net>', references_ids = ARRAY['<r1@example.net>','<p@example.net>'] WHERE id = $1`,
		a.MessageID.String()); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{a.MessageID.String()}, "properties": []string{"inReplyTo", "references"}})
	m = asObj(asArr(res["list"])[0])
	if got := toStrings(m["inReplyTo"]); !slices.Equal(got, []string{"p@example.net"}) {
		t.Errorf("inReplyTo = %v", m["inReplyTo"])
	}
	if got := toStrings(m["references"]); !slices.Equal(got, []string{"r1@example.net", "p@example.net"}) {
		t.Errorf("references = %v", m["references"])
	}
}

// bulkMail inserts n bare messages (and the threads and blobs they need) for the
// user, straight into the tables, to cross a size bound cheaply.
func bulkMail(t *testing.T, h *composeHarness, f apiFixture, n int) {
	t.Helper()
	ids, threads := make([]string, n), make([]string, n)
	for i := range ids {
		ids[i], threads[i] = store.NewULID().String(), store.NewULID().String()
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(context.Background(), sql, args...); err != nil {
			t.Fatalf("bulk seed: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      SELECT $1, sha256(convert_to(i, 'UTF8')), 1, 1, $3 FROM unnest($2::text[]) AS i`,
		f.UserID.String(), ids, store.NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject, message_count, last_received_at)
	      SELECT i, $1, 'bulk', 1, now() FROM unnest($2::text[]) AS i`, f.UserID.String(), threads)
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
		 SELECT m.id, $1, sha256(convert_to(m.id, 'UTF8')), t.id, 'bulk', now(), 1
		   FROM unnest($2::text[]) WITH ORDINALITY AS m(id, n)
		   JOIN unnest($3::text[]) WITH ORDINALITY AS t(id, n) USING (n)`,
		f.UserID.String(), ids, threads); err != nil {
		t.Fatalf("bulk messages: %v", err)
	}
}

func TestGetWithNullIDs(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedMoreMail(t, h, a, 2)

	// Within the bound: every object.
	res := call(t, h.api.emailGet, a.UserID, map[string]any{"ids": nil, "properties": []string{"id"}})
	if len(asArr(res["list"])) != 3 || len(asArr(res["notFound"])) != 0 {
		t.Errorf("Email/get ids:null returned %d (want 3): %v", len(asArr(res["list"])), res["notFound"])
	}
	res = call(t, h.api.threadGet, a.UserID, map[string]any{"ids": nil})
	if len(asArr(res["list"])) < 1 {
		t.Errorf("Thread/get ids:null returned nothing: %v", res)
	}
	// Deleted mail is not an object.
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET trashed_at = now() WHERE id = $1`, a.MessageID.String()); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.emailGet, a.UserID, map[string]any{"ids": nil, "properties": []string{"id"}})
	if len(asArr(res["list"])) != 2 {
		t.Errorf("Email/get ids:null counted deleted mail: %d", len(asArr(res["list"])))
	}

	// Over the bound: requestTooLarge, from the number the session advertises.
	bulkMail(t, h, a, maxObjectsInGet)
	for name, fn := range map[string]methodFn{"Email/get": h.api.emailGet, "Thread/get": h.api.threadGet} {
		_, mErr := fn(context.Background(), a.UserID, mustJSON(t, map[string]any{"ids": nil}))
		if mErr == nil || mErr.Type != errRequestTooLarge {
			t.Errorf("%s ids:null over maxObjectsInGet: %+v, want requestTooLarge", name, mErr)
		}
		// Explicit ids are not affected.
		raw, mErr := fn(context.Background(), a.UserID, mustJSON(t, map[string]any{"ids": []string{a.ThreadID.String()}}))
		if mErr != nil {
			t.Errorf("%s with explicit ids: %+v %s", name, mErr, raw)
		}
	}
	core := coreCapability()
	if core["maxObjectsInGet"] != maxObjectsInGet {
		t.Errorf("advertised maxObjectsInGet = %v, enforced %d", core["maxObjectsInGet"], maxObjectsInGet)
	}
}

func TestEmailQueryCanCalculateChangesIsFalseForKeys(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "ro", "scopes": []string{"mail:read"}})
	filter := map[string]any{"filter": map[string]any{"inMailbox": a.InboxID.String()}}

	// Positive control: the signed-in session is told true for a stable filter,
	// so the false below is the key rule and not a filter that is never stable.
	if res := call(t, h.api.emailQuery, a.UserID, filter); res["canCalculateChanges"] != true {
		t.Fatalf("session: canCalculateChanges = %v, want true", res["canCalculateChanges"])
	}
	name, res := h.keyCall(t, tok, "Email/query", filter)
	if name != "Email/query" || res["canCalculateChanges"] != false {
		t.Errorf("key: %s canCalculateChanges = %v, want false", name, res["canCalculateChanges"])
	}
	// And the method it would have used is indeed closed to the key.
	name, res = h.keyCall(t, tok, "Email/queryChanges", map[string]any{
		"filter": filter["filter"], "sinceQueryState": "s:1"})
	wantForbidden(t, "Email/queryChanges by a key", name, res)
}

func TestEmailQueryInMailboxOtherThan(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	// A second account with its own mailbox and one message in it.
	acct2, box2, m2 := store.NewULID(), store.NewULID(), seedMoreMail(t, h.composeHarness, a, 1)[0]
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		  VALUES ($1,$2,'second','mail.example.com','smtp.example.com',$3,$4)`,
			[]any{acct2.String(), a.UserID.String(), []byte{1}, store.NewULID().String()}},
		{`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		  VALUES ($1,$2,$3,'INBOX','Inbox 2','\Inbox')`, []any{box2.String(), a.UserID.String(), acct2.String()}},
		// seedRawMessage filed m2 in the first account's inbox: re-file it.
		{`UPDATE app.message_provenance SET account_id = $2, mailbox_id = $3 WHERE message_id = $1`,
			[]any{m2.String(), acct2.String(), box2.String()}},
	} {
		if _, err := h.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	scopes := []string{"mail:read"}
	all, _ := h.newKey(t, a.UserID, map[string]any{"name": "all", "scopes": scopes})
	one, _ := h.newKey(t, a.UserID, map[string]any{"name": "one", "scopes": scopes,
		"accountIds": []string{a.AccountID.String()}})

	query := func(tok string, other ...string) []string {
		t.Helper()
		name, res := h.keyCall(t, tok, "Email/query", map[string]any{"filter": map[string]any{"inMailboxOtherThan": other}})
		if name != "Email/query" {
			t.Fatalf("Email/query: %s %v", name, res)
		}
		return toStrings(res["ids"])
	}
	inbox1, mb2 := a.InboxID.String(), box2.String()

	// Positive controls: the filter selects by mailbox, and an unrestricted key
	// sees the second account's message through it.
	if ids := query(all, inbox1); !slices.Equal(ids, []string{m2.String()}) {
		t.Fatalf("other than inbox 1 (unrestricted key) = %v, want only the second account's message", ids)
	}
	if ids := query(all, mb2); slices.Contains(ids, m2.String()) || !slices.Contains(ids, a.MessageID.String()) {
		t.Fatalf("other than inbox 2 = %v", ids)
	}
	// An empty list excludes nothing.
	if ids := query(all); len(ids) != 2 {
		t.Errorf("other than nothing = %v, want both messages", ids)
	}

	// THE POINT: a key confined to the first account learns nothing about the
	// second through this filter, however the filter is aimed.
	if ids := query(one, inbox1); len(ids) != 0 {
		t.Errorf("restricted key, other than inbox 1 = %v; it saw the other account's mail", ids)
	}
	if ids := query(one, mb2); slices.Contains(ids, m2.String()) {
		t.Errorf("restricted key, other than inbox 2 = %v; it saw the other account's mail", ids)
	}
	_, res := h.keyCall(t, one, "Email/query", map[string]any{
		"filter": map[string]any{"inMailboxOtherThan": []string{inbox1}}, "calculateTotal": true})
	if res["total"] != float64(0) {
		t.Errorf("restricted key total = %v; the count leaks the other account's mail", res["total"])
	}

	// The membership is the EFFECTIVE one, as inMailbox reads it: a message the
	// user moved is in the mailbox it was moved to, not where provenance says.
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET moved_mailbox_id = $2 WHERE id = $1`,
		a.MessageID.String(), box2.String()); err != nil {
		t.Fatal(err)
	}
	if ids := query(all, inbox1); !slices.Contains(ids, a.MessageID.String()) {
		t.Errorf("a message moved out of inbox 1 is still 'only in inbox 1': %v", ids)
	}
	if ids := query(all, mb2); slices.Contains(ids, a.MessageID.String()) {
		t.Errorf("a message moved into inbox 2 is 'in a mailbox other than inbox 2': %v", ids)
	}
	// A bad id is an argument error, like inMailbox's.
	name, res := h.keyCall(t, all, "Email/query", map[string]any{"filter": map[string]any{"inMailboxOtherThan": []string{"nope"}}})
	if name != "error" || res["type"] != errInvalidArguments {
		t.Errorf("malformed mailbox id: %s %v", name, res)
	}
}
