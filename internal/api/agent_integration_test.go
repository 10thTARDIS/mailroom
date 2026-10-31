// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/compose"
	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// Agent access end to end (#49, ADR-042): every request below goes through the
// REAL bearer gate with the REAL store resolver in front of the REAL API
// handler, exactly as cmd/mailroom mounts them.

type fakeAlerts struct {
	mu   sync.Mutex
	sent []fakeAlert
}

type fakeAlert struct {
	user             ulid.ULID
	title, body, url string
	urgent           bool
}

func (f *fakeAlerts) NotifyAgentAlert(_ context.Context, userID ulid.ULID, title, body, url string, urgent bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, fakeAlert{userID, title, body, url, urgent})
	return nil
}

// waitFor polls for an alert (they are sent asynchronously).
func (f *fakeAlerts) waitFor(t *testing.T, match func(fakeAlert) bool) fakeAlert {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, a := range f.sent {
			if match(a) {
				f.mu.Unlock()
				return a
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no matching alert was pushed; got %+v", f.sent)
	return fakeAlert{}
}

type agentHarness struct {
	*composeHarness
	alerts  *fakeAlerts
	handler http.Handler
	clock   time.Time
	mu      sync.Mutex
}

func newAgentHarness(t *testing.T) *agentHarness {
	t.Helper()
	ch := newComposeHarness(t)
	h := &agentHarness{composeHarness: ch, alerts: &fakeAlerts{}}
	ch.api = New(ch.store, WithComposer(compose.NewComposer(ch.blobs, fakeKeys{}, time.Millisecond)),
		WithAlertNotifier(h.alerts))
	ch.api.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.clock.IsZero() {
			// Midday UTC: off hours never fire unless a test asks for them.
			n := time.Now().UTC()
			return time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, time.UTC)
		}
		return h.clock
	}
	gate := &auth.BearerGate{Resolve: ch.store.ResolveAPIKeyToken,
		Limiter: auth.NewFailureLimiter(1000, time.Millisecond)}
	h.handler = gate.Wrap(ch.api.Handler(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(599) // the session path: never expected here
	}))
	return h
}

func (h *agentHarness) setClock(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = at
}

// newKey creates a key as the user's SESSION and returns its token and id.
func (h *agentHarness) newKey(t *testing.T, user ulid.ULID, settings map[string]any) (token, id string) {
	t.Helper()
	res := call(t, h.api.apiKeySet, user, map[string]any{"create": map[string]any{"k": settings}})
	created, _ := res["created"].(map[string]any)
	k, ok := created["k"].(map[string]any)
	if !ok {
		t.Fatalf("key not created: %v", res)
	}
	return k["token"].(string), k["id"].(string)
}

var allUsing = func() []string {
	var out []string
	for urn := range capabilityObjects() {
		out = append(out, urn)
	}
	return out
}()

// keyCall makes one method call with a bearer token and returns the response
// name and arguments.
func (h *agentHarness) keyCall(t *testing.T, token, method string, args any) (string, map[string]any) {
	t.Helper()
	body := mustJSON(t, map[string]any{"using": allUsing, "methodCalls": []any{[]any{method, args, "c0"}}})
	r := httptest.NewRequest(http.MethodPost, "/jmap", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.RemoteAddr = "192.0.2.44:4444"
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %q", method, w.Code, w.Body.String())
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.MethodResponses) != 1 {
		t.Fatalf("%s: bad response %q", method, w.Body.String())
	}
	var name string
	var out map[string]any
	_ = json.Unmarshal(resp.MethodResponses[0][0], &name)
	_ = json.Unmarshal(resp.MethodResponses[0][1], &out)
	return name, out
}

func (h *agentHarness) rawStatus(token string, both bool) (int, string, string) {
	r := httptest.NewRequest(http.MethodPost, "/jmap",
		strings.NewReader(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"0"]]}`))
	r.Header.Set("Authorization", "Bearer "+token)
	if both {
		r.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: "x"})
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header().Get("WWW-Authenticate")
}

func wantForbidden(t *testing.T, what, name string, res map[string]any) {
	t.Helper()
	if name != "error" || res["type"] != errForbidden {
		t.Fatalf("%s: got %s %v, want a forbidden method error", what, name, res)
	}
}

func (h *agentHarness) trashed(t *testing.T, id ulid.ULID) (exists, trashed bool) {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*), bool_or(trashed_at IS NOT NULL) FROM app.messages WHERE id = $1`, id.String()).
		Scan(&n, &trashed); err != nil && n != 0 {
		t.Fatal(err)
	}
	return n == 1, trashed
}

func (h *agentHarness) events(t *testing.T, keyID string) []string {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT outcome || COALESCE(':' || anomaly, '') FROM app.api_key_events WHERE key_id = $1 ORDER BY id`, keyID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// ---- auth ----------------------------------------------------------------------

func TestAgentKeyAuthenticationAndRLS(t *testing.T) {
	h := newAgentHarness(t)
	a, b := seedTwo(t, h.super)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	token, keyID := h.newKey(t, a.UserID, map[string]any{"name": "reader", "scopes": []string{"mail:read"}})

	// Positive control, and RLS through the key equals the user's: A's message
	// is there, B's is notFound.
	name, res := h.keyCall(t, token, "Email/get", map[string]any{
		"ids": []string{a.MessageID.String(), b.MessageID.String()}, "properties": []string{"subject"}})
	if name != "Email/get" || len(asArr(res["list"])) != 1 || !strInArr(res["notFound"], b.MessageID.String()) {
		t.Fatalf("key Email/get: %s %v — want A's message only", name, res)
	}
	if lst := asArr(res["list"]); asObj(lst[0])["id"] != a.MessageID.String() {
		t.Fatalf("key Email/get returned the wrong message: %v", lst)
	}

	unknown, _, _, _ := auth.NewAPIToken()
	codeU, bodyU, wwwU := h.rawStatus(unknown, false)
	codeM, bodyM, wwwM := h.rawStatus("mr_nope", false)
	codeB, bodyB, wwwB := h.rawStatus(token, true) // a live key PLUS a session cookie
	call(t, h.api.apiKeySet, a.UserID, map[string]any{"destroy": []string{keyID}})
	codeR, bodyR, wwwR := h.rawStatus(token, false) // revoked
	for what, got := range map[string][3]any{
		"unknown": {codeU, bodyU, wwwU}, "malformed": {codeM, bodyM, wwwM},
		"cookie+bearer": {codeB, bodyB, wwwB}, "revoked": {codeR, bodyR, wwwR},
	} {
		if got[0] != http.StatusUnauthorized || got[1] != bodyU || got[2] != `Bearer realm="mailroom"` {
			t.Errorf("%s: %v, want the uniform 401 (%d %q)", what, got, http.StatusUnauthorized, bodyU)
		}
	}

	// THE TOKEN IS NOWHERE: not in any column of the key row (only its hash),
	// not in the audit, not in the log.
	var row, audits, events string
	sum := sha256.Sum256([]byte(token))
	var stored []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT row_to_json(k)::text, token_hash FROM app.api_keys k WHERE id = $1`, keyID).Scan(&row, &stored); err != nil {
		t.Fatal(err)
	}
	_ = h.super.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(row_to_json(l)::text, ' '), '') FROM app.audit_log l WHERE user_id = $1`,
		a.UserID.String()).Scan(&audits)
	_ = h.super.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(row_to_json(e)::text, ' '), '') FROM app.api_key_events e WHERE user_id = $1`,
		a.UserID.String()).Scan(&events)
	if !bytes.Equal(stored, sum[:]) {
		t.Fatal("token_hash is not SHA-256 of the token")
	}
	if !strings.Contains(audits, "api_key.create") || !strings.Contains(events, "Email/get") {
		t.Fatalf("the audit is empty, so the leak check below would prove nothing: %q / %q", audits, events)
	}
	body := token[3:]
	for where, text := range map[string]string{"key row": row, "audit_log": audits, "api_key_events": events,
		"logs": logs.String()} {
		if strings.Contains(text, body) {
			t.Errorf("the token appears in %s", where)
		}
	}
}

// ---- scopes --------------------------------------------------------------------

func TestAgentKeyScopesAreEnforced(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	reader, _ := h.newKey(t, a.UserID, map[string]any{"name": "reader", "scopes": []string{"mail:read", "contacts:read"}})

	name, res := h.keyCall(t, reader, "Email/set", map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"keywords/$seen": true}}})
	wantForbidden(t, "read-only Email/set", name, res)
	name, res = h.keyCall(t, reader, "EmailSubmission/set", map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": a.MessageID.String()}}})
	wantForbidden(t, "read-only EmailSubmission/set", name, res)
	name, res = h.keyCall(t, reader, "Contact/set", map[string]any{"update": map[string]any{}})
	wantForbidden(t, "read-only Contact/set", name, res)
	for _, m := range []string{"ApiKey/get", "ApiKey/set", "ApiKey/getEvents", "PendingAction/set",
		"UserKey/get", "UserKey/generate", "UserKey/export", "Identity/set", "Account/get"} {
		name, res := h.keyCall(t, reader, m, map[string]any{})
		wantForbidden(t, m, name, res)
	}
	var seen bool
	_ = h.super.QueryRow(context.Background(), `SELECT is_seen FROM app.messages WHERE id=$1`, a.MessageID.String()).Scan(&seen)
	if seen {
		t.Fatal("a refused Email/set changed the message")
	}

	// A mixed Email/set needs EVERY scope: move alone cannot also delete.
	mover, _ := h.newKey(t, a.UserID, map[string]any{"name": "mover", "scopes": []string{"mail:read", "mail:move"}})
	second := agentMessage(t, h, a)
	name, res = h.keyCall(t, mover, "Email/set", map[string]any{
		"update":  map[string]any{a.MessageID.String(): map[string]any{"keywords/$seen": true}},
		"destroy": []string{second.String()}})
	wantForbidden(t, "move-only key with a destroy", name, res)
	// A delete written as a patch is still a delete.
	name, res = h.keyCall(t, mover, "Email/set", map[string]any{
		"update": map[string]any{second.String(): map[string]any{"mailboxIds/" + a.TrashID.String(): true}}})
	wantForbidden(t, "move-only key trashing by patch", name, res)
	if _, tr := h.trashed(t, second); tr {
		t.Fatal("a refused call trashed the message")
	}
	// Positive control: the move itself is allowed.
	if name, res := h.keyCall(t, mover, "Email/set", map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"keywords/$seen": true}}}); name != "Email/set" ||
		!mapHasKey(res, "updated", a.MessageID.String()) {
		t.Fatalf("mover's own move: %s %v", name, res)
	}

	// Destroy by a key is ALWAYS a soft delete — even of an unsent draft, which
	// a session's destroy retires for good.
	deleter, _ := h.newKey(t, a.UserID, map[string]any{"name": "deleter",
		"scopes": []string{"mail:read", "mail:move", "mail:delete"}, "confirmScopes": []string{}})
	seedSendable(t, h.super, a, "me-"+a.UserID.String()[20:]+"@example.com", "")
	draft := createDraftAs(t, h, a, "me-"+a.UserID.String()[20:]+"@example.com", "a draft")
	name, res = h.keyCall(t, deleter, "Email/set", map[string]any{"destroy": []string{second.String(), draft.String()}})
	if name != "Email/set" || len(asArr(res["destroyed"])) != 2 {
		t.Fatalf("key destroy: %s %v", name, res)
	}
	for _, id := range []ulid.ULID{second, draft} {
		if exists, tr := h.trashed(t, id); !exists || !tr {
			t.Errorf("message %s after a key's destroy: exists=%v trashed=%v, want kept in Trash", id, exists, tr)
		}
	}
}

// createDraftAs creates a draft through the session API and returns its id.
func createDraftAs(t *testing.T, h *agentHarness, f apiFixture, from, subject string) ulid.ULID {
	t.Helper()
	res := call(t, h.api.emailSet, f.UserID, draftCreateArgs(from, subject))
	created, _ := res["created"].(map[string]any)
	d, ok := created["d"].(map[string]any)
	if !ok {
		t.Fatalf("draft not created: %v", res)
	}
	id, err := ulid.Parse(d["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func draftCreateArgs(from, subject string) map[string]any {
	return map[string]any{"create": map[string]any{"d": map[string]any{
		"from": []map[string]any{{"email": from}}, "to": []map[string]any{{"email": "alice@example.net"}},
		"subject": subject, "bodyValues": map[string]any{"1": map[string]any{"value": "hello"}},
		"textBody": []map[string]any{{"partId": "1"}},
	}}}
}

// ---- the account restriction ---------------------------------------------------

func TestAgentKeyAccountRestriction(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	acct2, mbox2, m2 := store.NewULID(), store.NewULID(), store.NewULID()
	hash := sha256.Sum256([]byte("second-" + m2.String()))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		  VALUES ($1,$2,'second','mail.example.com','smtp.example.com',$3,$4)`,
			[]any{acct2.String(), a.UserID.String(), []byte{1}, store.NewULID().String()}},
		{`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		  VALUES ($1,$2,$3,'INBOX','Inbox 2','\Inbox')`, []any{mbox2.String(), a.UserID.String(), acct2.String()}},
		{`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,1,1,$3)`,
			[]any{a.UserID.String(), hash[:], store.NewULID().String()}},
		{`INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
		  VALUES ($1,$2,$3,'other account', now(), 1)`, []any{m2.String(), a.UserID.String(), hash[:]}},
		{`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		  VALUES ($1,$2,$3,$4,$5,$6,1)`,
			[]any{store.NewULID().String(), a.UserID.String(), m2.String(), acct2.String(), mbox2.String(), testUID()}},
	} {
		if _, err := h.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	scopes := []string{"mail:read", "mail:move", "mail:delete", "labels:read"}
	all, _ := h.newKey(t, a.UserID, map[string]any{"name": "all", "scopes": scopes, "confirmScopes": []string{}})
	one, _ := h.newKey(t, a.UserID, map[string]any{"name": "one", "scopes": scopes, "confirmScopes": []string{},
		"accountIds": []string{a.AccountID.String()}})

	queryIDs := func(tok string) []string {
		name, res := h.keyCall(t, tok, "Email/query", map[string]any{})
		if name != "Email/query" {
			t.Fatalf("Email/query: %s %v", name, res)
		}
		var ids []string
		for _, v := range asArr(res["ids"]) {
			ids = append(ids, v.(string))
		}
		return ids
	}
	// Positive control: an unrestricted key finds both accounts' mail.
	if ids := queryIDs(all); !slices.Contains(ids, a.MessageID.String()) || !slices.Contains(ids, m2.String()) {
		t.Fatalf("unrestricted key query: %v", ids)
	}
	if ids := queryIDs(one); !slices.Contains(ids, a.MessageID.String()) || slices.Contains(ids, m2.String()) {
		t.Errorf("restricted key query: %v, want account 1 only", ids)
	}
	_, res := h.keyCall(t, one, "Email/get", map[string]any{"ids": []string{m2.String()}, "properties": []string{"subject"}})
	if len(asArr(res["list"])) != 0 || !strInArr(res["notFound"], m2.String()) {
		t.Errorf("restricted key Email/get of the other account's mail: %v", res)
	}
	_, res = h.keyCall(t, one, "Email/set", map[string]any{
		"update": map[string]any{m2.String(): map[string]any{"keywords/$flagged": true}}})
	if !mapHasKey(res, "notUpdated", m2.String()) {
		t.Errorf("restricted key updated the other account's mail: %v", res)
	}
	_, res = h.keyCall(t, one, "Email/set", map[string]any{"destroy": []string{m2.String()}})
	if !mapHasKey(res, "notDestroyed", m2.String()) {
		t.Errorf("restricted key deleted the other account's mail: %v", res)
	}
	_, res = h.keyCall(t, one, "Email/set", map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"mailboxIds": map[string]any{mbox2.String(): true}}}})
	if !mapHasKey(res, "notUpdated", a.MessageID.String()) {
		t.Errorf("restricted key moved mail into the other account: %v", res)
	}
	if exists, tr := h.trashed(t, m2); !exists || tr {
		t.Fatal("the other account's message was changed")
	}
	_, res = h.keyCall(t, one, "Mailbox/get", map[string]any{})
	for _, mb := range asArr(res["list"]) {
		if asObj(mb)["id"] == mbox2.String() {
			t.Errorf("restricted key listed the other account's mailbox")
		}
	}
	if len(asArr(res["list"])) == 0 {
		t.Error("restricted key listed no mailbox at all: the check above proves nothing")
	}
	name, res := h.keyCall(t, one, "Thread/get", map[string]any{"ids": []string{a.ThreadID.String()}})
	wantForbidden(t, "restricted key Thread/get", name, res)
	name, res = h.keyCall(t, one, "Email/changes", map[string]any{"sinceState": "0"})
	wantForbidden(t, "restricted key Email/changes", name, res)

	// Label restriction is refused at creation, clearly.
	res = call(t, h.api.apiKeySet, a.UserID, map[string]any{"create": map[string]any{"k": map[string]any{
		"name": "labelled", "scopes": []string{"mail:read"}, "labelIds": []string{a.LabelID.String()}}}})
	if !mapHasKey(res, "notCreated", "k") {
		t.Errorf("a label-restricted key was created: %v", res)
	}
}

// ---- limits ----------------------------------------------------------------------

type ledgerSender struct{ n int }

func (s *ledgerSender) Send(context.Context, provider.SendRequest) (provider.SendResult, error) {
	s.n++
	return provider.SendResult{}, nil
}

func TestAgentKeyLimitsAndTheLedger(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "slow", "scopes": []string{"mail:read"},
		"ratePerMinute": 3, "dailyBudget": 1000})
	for i := range 3 {
		if name, res := h.keyCall(t, tok, "Mailbox/get", map[string]any{}); name != "Mailbox/get" {
			t.Fatalf("call %d: %s %v", i+1, name, res)
		}
	}
	name, res := h.keyCall(t, tok, "Mailbox/get", map[string]any{})
	if name != "error" || res["type"] != errRateLimit {
		t.Fatalf("4th call in a minute: %s %v, want rateLimit", name, res)
	}
	if ev := h.events(t, keyID); ev[len(ev)-1] != store.KeyOutcomeRateLimited {
		t.Errorf("rate limit not audited: %v", ev)
	}

	budget, _ := h.newKey(t, a.UserID, map[string]any{"name": "budget", "scopes": []string{"mail:read", "mail:move"},
		"ratePerMinute": 600, "dailyBudget": 4})
	ids := []string{a.MessageID.String(), agentMessage(t, h, a).String(),
		agentMessage(t, h, a).String()}
	upd := map[string]any{}
	for _, id := range ids {
		upd[id] = map[string]any{"keywords/$flagged": true}
	}
	if name, res := h.keyCall(t, budget, "Email/set", map[string]any{"update": upd}); name != "Email/set" {
		t.Fatalf("3-object set within budget: %s %v", name, res)
	}
	// 3 spent of 4: a 2-object set is over, a read (cost 1) is not.
	name, res = h.keyCall(t, budget, "Email/set", map[string]any{"update": map[string]any{
		ids[0]: map[string]any{"keywords/$seen": true}, ids[1]: map[string]any{"keywords/$seen": true}}})
	if name != "error" || res["type"] != errOverQuota {
		t.Fatalf("over-budget set: %s %v, want overQuota", name, res)
	}
	if name, _ := h.keyCall(t, budget, "Mailbox/get", map[string]any{}); name != "Mailbox/get" {
		t.Fatalf("the last unit of budget was not usable")
	}

	// A key's send draws from the outbound ledger like any send.
	addr := "send-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	sender, _ := h.newKey(t, a.UserID, map[string]any{"name": "sender",
		"scopes": []string{"mail:read", "mail:draft", "mail:send"}, "confirmScopes": []string{}})
	_, res = h.keyCall(t, sender, "Email/set", draftCreateArgs(addr, "from the agent"))
	draft := asObj(asObj(res["created"])["d"])["id"].(string)
	_, res = h.keyCall(t, sender, "EmailSubmission/set", map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": draft}}})
	sub, ok := asObj(res["created"])["s"].(map[string]any)
	if !ok {
		t.Fatalf("key submission not created: %v", res)
	}
	subID, _ := ulid.Parse(sub["id"].(string))
	time.Sleep(20 * time.Millisecond) // past the 1 ms undo window
	ls := &ledgerSender{}
	w := compose.NewSendWorker(h.store, h.blobs, fakeKeys{}, ls, nil)
	if err := w.Send(context.Background(), a.UserID, subID, 1, 3); err != nil {
		t.Fatalf("send worker: %v", err)
	}
	var spent int
	if err := h.super.QueryRow(context.Background(),
		`SELECT COALESCE(sum(sent_count),0) FROM app.outbound_ledger WHERE user_id = $1`, a.UserID.String()).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if ls.n != 1 || spent != 1 {
		t.Fatalf("a key's send: provider calls %d, ledger %d — want 1 and 1", ls.n, spent)
	}
}

// ---- anomalies ---------------------------------------------------------------------

func TestAgentKeyMassDeleteStopsAndSuspends(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "cleaner",
		"scopes": []string{"mail:read", "mail:delete"}, "confirmScopes": []string{}, "ratePerMinute": 600})
	var ids []string
	for range MassDeleteThreshold + 1 {
		ids = append(ids, agentMessage(t, h, a).String())
	}
	if name, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": ids[:MassDeleteThreshold]}); name != "Email/set" ||
		len(asArr(res["destroyed"])) != MassDeleteThreshold {
		t.Fatalf("up to the threshold: %s %v", name, res)
	}
	name, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": ids[MassDeleteThreshold:]})
	wantForbidden(t, "one past the threshold", name, res)
	last, _ := ulid.Parse(ids[MassDeleteThreshold])
	if _, tr := h.trashed(t, last); tr {
		t.Fatal("the call past the threshold deleted anyway")
	}
	ev := h.events(t, keyID)
	if !slices.Contains(ev, "anomaly:mass_delete") {
		t.Errorf("no mass_delete anomaly in the audit: %v", ev)
	}
	var suspended bool
	_ = h.super.QueryRow(context.Background(), `SELECT suspended_at IS NOT NULL FROM app.api_keys WHERE id=$1`, keyID).Scan(&suspended)
	if !suspended {
		t.Error("the key was not suspended")
	}
	h.alerts.waitFor(t, func(al fakeAlert) bool {
		return al.user == a.UserID && al.urgent && strings.Contains(al.body, "delete") &&
			al.url == "/#/settings/api-keys/"+keyID
	})
	if code, _, _ := h.rawStatus(tok, false); code != http.StatusUnauthorized {
		t.Errorf("a suspended key still authenticates: %d", code)
	}
}

func TestAgentKeyOffHoursIsRecordedNotRefused(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "night", "scopes": []string{"mail:read", "mail:move"}})
	n := time.Now().UTC()
	h.setClock(time.Date(n.Year(), n.Month(), n.Day(), 3, 0, 0, 0, time.UTC))
	for range 2 {
		if name, res := h.keyCall(t, tok, "Email/set", map[string]any{
			"update": map[string]any{a.MessageID.String(): map[string]any{"keywords/$seen": true}}}); name != "Email/set" {
			t.Fatalf("off-hours write was refused: %s %v", name, res)
		}
	}
	got := 0
	for _, e := range h.events(t, keyID) {
		if e == "anomaly:off_hours" {
			got++
		}
	}
	if got != 1 {
		t.Errorf("%d off_hours anomalies for two calls, want exactly 1 (one per cooldown)", got)
	}
	h.alerts.waitFor(t, func(al fakeAlert) bool {
		return al.urgent && strings.Contains(al.body, "off hours") && al.url == "/#/settings/api-keys/"+keyID
	})
}

// ---- pending approvals ------------------------------------------------------------

func TestAgentKeyPendingApprovals(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	addr := "ask-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	asker, askerID := h.newKey(t, a.UserID, map[string]any{"name": "asker",
		"scopes": []string{"mail:read", "mail:draft", "mail:send", "mail:delete"}, "acknowledgeSendAndDelete": true})
	other, _ := h.newKey(t, a.UserID, map[string]any{"name": "other", "scopes": []string{"mail:read"}})

	// Defaults: both destructive scopes ask first.
	got := call(t, h.api.apiKeyGet, a.UserID, map[string]any{"ids": []string{askerID}})
	if cs := asArr(asObj(asArr(got["list"])[0])["confirmScopes"]); len(cs) != 2 {
		t.Fatalf("confirmScopes default = %v, want mail:delete and mail:send", cs)
	}
	// The advertised default is the one a create without the field gets.
	if s, adv := asObj(asArr(got["list"])[0])["suspendOnAnomaly"], apiKeysCapability()["defaultSuspendOnAnomaly"]; s != adv {
		t.Errorf("suspendOnAnomaly of a key created without it = %v, capability advertises %v", s, adv)
	}

	submit := func(draft string) (string, map[string]any) {
		_, res := h.keyCall(t, asker, "EmailSubmission/set", map[string]any{
			"create": map[string]any{"s": map[string]any{"emailId": draft}}})
		e := setError(t, res, "notCreated", "s")
		return e["pendingActionId"].(string), e
	}
	countSubs := func() int {
		var n int
		_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.submissions WHERE user_id=$1`, a.UserID.String()).Scan(&n)
		return n
	}
	_, res := h.keyCall(t, asker, "Email/set", draftCreateArgs(addr, "Re: invoice"))
	draft := asObj(asObj(res["created"])["d"])["id"].(string)

	// Confirm mode HOLDS the send.
	pid, e := submit(draft)
	if e["type"] != setPendingApproval || countSubs() != 0 {
		t.Fatalf("held send: %v, submissions %d — want pendingApproval and nothing sent", e, countSubs())
	}
	h.alerts.waitFor(t, func(al fakeAlert) bool {
		return !al.urgent && strings.Contains(al.body, "Re: invoice") && al.url == "/#/settings/api-keys"
	})

	// The asking key can poll it; another key cannot see it.
	_, res = h.keyCall(t, asker, "PendingAction/get", map[string]any{"ids": []string{pid}})
	if lst := asArr(res["list"]); len(lst) != 1 || asObj(lst[0])["state"] != "pending" ||
		!strings.Contains(asObj(lst[0])["summary"].(string), "alice@example.net") {
		t.Fatalf("asker polling its request: %v", res)
	}
	// The held call's account is on the object (#49, the keys UI looks the
	// held messages up there): the user's own JMAP account.
	if got := asObj(asArr(res["list"])[0])["accountId"]; got != a.UserID.String() {
		t.Errorf("held send accountId = %v, want the user's account %s", got, a.UserID)
	}
	_, res = h.keyCall(t, other, "PendingAction/get", map[string]any{"ids": []string{pid}})
	if len(asArr(res["list"])) != 0 {
		t.Fatalf("another key read the request: %v", res)
	}
	// A key can never approve, not even its own request.
	name, res := h.keyCall(t, asker, "PendingAction/set", map[string]any{
		"update": map[string]any{pid: map[string]any{"state": "approved"}}})
	wantForbidden(t, "a key approving its own request", name, res)

	// Approve EXECUTES it.
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid: map[string]any{"state": "approved"}}})
	if upd := asObj(asObj(res["updated"])[pid]); upd["state"] != "approved" || countSubs() != 1 {
		t.Fatalf("approval: %v, submissions %d — want approved and one submission", res, countSubs())
	}
	if ev := h.events(t, askerID); !slices.Contains(ev, store.KeyOutcomePending) || !slices.Contains(ev, store.KeyOutcomeApproved) {
		t.Errorf("audit lacks pending/approved: %v", ev)
	}
	// ...once.
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid: map[string]any{"state": "approved"}}})
	if setError(t, res, "notUpdated", pid)["type"] != setNotPending || countSubs() != 1 {
		t.Fatalf("second approval: %v", res)
	}

	// Approve after the DRAFT CHANGED refuses.
	_, res = h.keyCall(t, asker, "Email/set", draftCreateArgs(addr, "second"))
	draft2 := asObj(asObj(res["created"])["d"])["id"].(string)
	pid2, _ := submit(draft2)
	newHash := sha256.Sum256([]byte("edited " + draft2))
	if _, err := h.super.Exec(ctx, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1,$2,1,1,$3)`, a.UserID.String(), newHash[:], store.NewULID().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET content_hash=$2 WHERE id=$1`, draft2, newHash[:]); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid2: map[string]any{"state": "approved"}}})
	if setError(t, res, "notUpdated", pid2)["type"] != setDraftChanged || countSubs() != 1 {
		t.Fatalf("approval after the draft changed: %v", res)
	}

	// Approve after REVOKE refuses.
	_, res = h.keyCall(t, asker, "Email/set", draftCreateArgs(addr, "third"))
	draft3 := asObj(asObj(res["created"])["d"])["id"].(string)
	pid3, _ := submit(draft3)
	// Expiry is checked on the same path: a fourth request, left to age.
	_, res = h.keyCall(t, asker, "Email/set", draftCreateArgs(addr, "fourth"))
	draft4 := asObj(asObj(res["created"])["d"])["id"].(string)
	pid4, _ := submit(draft4)
	// A held delete, approved below before the key is revoked.
	victim := agentMessage(t, h, a)
	_, res = h.keyCall(t, asker, "Email/set", map[string]any{"destroy": []string{victim.String()}})
	de := setError(t, res, "notDestroyed", victim.String())
	if de["type"] != setPendingApproval {
		t.Fatalf("held delete: %v", res)
	}
	if _, tr := h.trashed(t, victim); tr {
		t.Fatal("a held delete deleted")
	}
	res = call(t, h.api.pendingGet, a.UserID, map[string]any{"ids": []string{de["pendingActionId"].(string)}})
	if o := asObj(asArr(res["list"])[0]); o["accountId"] != a.UserID.String() || len(asArr(o["emailIds"])) != 1 {
		t.Errorf("held delete seen by the session: accountId %v emailIds %v, want %s and the one message", o["accountId"], o["emailIds"], a.UserID)
	}
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{
		de["pendingActionId"].(string): map[string]any{"state": "approved"}}})
	if _, tr := h.trashed(t, victim); !tr {
		t.Fatalf("an approved delete did not delete: %v", res)
	}

	call(t, h.api.apiKeySet, a.UserID, map[string]any{"destroy": []string{askerID}})
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid3: map[string]any{"state": "approved"}}})
	if setError(t, res, "notUpdated", pid3)["type"] != setKeyNotActive || countSubs() != 1 {
		t.Fatalf("approval after revoke: %v", res)
	}

	// Expiry: 25 hours later the request is expired on read and cannot be approved.
	h.setClock(h.api.clock().Add(25 * time.Hour))
	res = call(t, h.api.pendingGet, a.UserID, map[string]any{"ids": []string{pid4}})
	if st := asObj(asArr(res["list"])[0])["state"]; st != store.PendingStateExpired {
		t.Fatalf("a day-old request reads %v, want expired", st)
	}
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid4: map[string]any{"state": "approved"}}})
	if setError(t, res, "notUpdated", pid4)["type"] != setNotPending || countSubs() != 1 {
		t.Fatalf("approving an expired request: %v", res)
	}
	if ev := h.events(t, askerID); !slices.Contains(ev, store.KeyOutcomeExpired) {
		t.Errorf("expiry not audited: %v", ev)
	}
}

func TestAgentKeyPendingSweepExpires(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "sweep", "scopes": []string{"mail:read", "mail:delete"}})
	victim := agentMessage(t, h, a)
	_, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{victim.String()}})
	pid := setError(t, res, "notDestroyed", victim.String())["pendingActionId"].(string)
	h.setClock(h.api.clock().Add(PendingApprovalWindow + time.Minute))
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.api_key_pending SET expires_at = now() - interval '1 minute' WHERE id=$1`, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.api.SweepPendingApprovals(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = h.super.QueryRow(context.Background(), `SELECT state FROM app.api_key_pending WHERE id=$1`, pid).Scan(&state)
	if state != store.PendingStateExpired || !slices.Contains(h.events(t, keyID), store.KeyOutcomeExpired) {
		t.Fatalf("after the sweep: state %q, events %v", state, h.events(t, keyID))
	}
	if _, tr := h.trashed(t, victim); tr {
		t.Fatal("an expired delete deleted")
	}
}

// ---- key management ------------------------------------------------------------------

func TestApiKeySetValidation(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	for name, settings := range map[string]map[string]any{
		"unknown scope":              {"name": "x1", "scopes": []string{"mail:read", "mail:everything"}},
		"no scope":                   {"name": "x2", "scopes": []string{}},
		"send+delete unacknowledged": {"name": "x3", "scopes": []string{"mail:send", "mail:delete"}},
		"rate over cap":              {"name": "x4", "scopes": []string{"mail:read"}, "ratePerMinute": 601},
		"budget zero":                {"name": "x5", "scopes": []string{"mail:read"}, "dailyBudget": 0},
		"foreign account":            {"name": "x6", "scopes": []string{"mail:read"}, "accountIds": []string{store.NewULID().String()}},
		"empty account list":         {"name": "x7", "scopes": []string{"mail:read"}, "accountIds": []string{}},
		"confirm non-destructive":    {"name": "x8", "scopes": []string{"mail:read"}, "confirmScopes": []string{"mail:read"}},
		"unknown property":           {"name": "x9", "scopes": []string{"mail:read"}, "admin": true},
	} {
		res := call(t, h.api.apiKeySet, a.UserID, map[string]any{"create": map[string]any{"k": settings}})
		if !mapHasKey(res, "notCreated", "k") {
			t.Errorf("%s: created anyway: %v", name, res)
		}
	}
	// Escalation by UPDATE needs the same acknowledgement.
	_, id := h.newKey(t, a.UserID, map[string]any{"name": "grow", "scopes": []string{"mail:read", "mail:send"}})
	res := call(t, h.api.apiKeySet, a.UserID, map[string]any{"update": map[string]any{id: map[string]any{
		"scopes": []string{"mail:read", "mail:send", "mail:delete"}}}})
	if !mapHasKey(res, "notUpdated", id) {
		t.Fatalf("an update granting send+delete without the acknowledgement was accepted: %v", res)
	}
	res = call(t, h.api.apiKeySet, a.UserID, map[string]any{"update": map[string]any{id: map[string]any{
		"scopes": []string{"mail:read", "mail:send", "mail:delete"}, "acknowledgeSendAndDelete": true}}})
	if !mapHasKey(res, "updated", id) {
		t.Fatalf("an acknowledged update was refused: %v", res)
	}
	got := call(t, h.api.apiKeyGet, a.UserID, map[string]any{"ids": []string{id}})
	k := asObj(asArr(got["list"])[0])
	if cs := asArr(k["confirmScopes"]); !strInArr(cs, "mail:delete") {
		t.Errorf("a newly granted mail:delete did not start in confirm mode: %v", cs)
	}
	if _, ok := k["token"]; ok {
		t.Error("ApiKey/get returned a token")
	}
}

func asObj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// agentMessage inserts one more Inbox message for f (unique bytes and UID).
func agentMessage(t *testing.T, h *agentHarness, f apiFixture) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	hash := sha256.Sum256([]byte("agent-msg-" + id.String()))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,100,132,$3)`,
			[]any{f.UserID.String(), hash[:], store.NewULID().String()}},
		{`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
		  VALUES ($1,$2,$3,$4,'agent test', now(), 100)`, []any{id.String(), f.UserID.String(), hash[:], f.ThreadID.String()}},
		{`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		  VALUES ($1,$2,$3,$4,$5,$6,1)`,
			[]any{store.NewULID().String(), f.UserID.String(), id.String(), f.AccountID.String(), f.InboxID.String(), testUID()}},
	} {
		if _, err := h.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("agent message: %v", err)
		}
	}
	return id
}
