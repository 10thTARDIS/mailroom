// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// SCOPE STEP-UP (ADR-050 section 8): an OAuth-authenticated tools/call refused
// ONLY for a scope the grant lacks is answered 403 with the RFC 6750
// insufficient_scope challenge. Every other refusal, and every API key, keeps
// the tool error it has always had.

// toolCall posts a raw tools/call with a bearer token and returns the answer.
func (h *oaHarness) toolCall(t *testing.T, token, tool string, args map[string]any) (status int, hdr http.Header, result map[string]any, raw string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+MCPPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var rpc struct {
		Result map[string]any `json:"result"`
	}
	_ = json.Unmarshal(b, &rpc)
	return resp.StatusCode, resp.Header, rpc.Result, string(b)
}

// connectWith runs authorize and approve for clientID, requesting `requested`
// and approving `scopes` (with `confirm` in approval mode), then redeems the
// code. It is a first connection or a re-consent, whichever the grant makes it.
func (h *oaHarness) connectWith(t *testing.T, user ulid.ULID, clientID, requested string, scopes, confirm []string) oaTokens {
	t.Helper()
	verifier, challenge := pkce()
	resp, body := h.authorize(t, user, h.authorizeURL(clientID, challenge, url.Values{"scope": {requested}}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize (%s): %d %.300s", requested, resp.StatusCode, body)
	}
	form := defaultDecision(requestID(t, body), "approve")
	form["scope"] = scopes
	if len(confirm) > 0 {
		form["confirm"] = confirm
	}
	resp, body = h.decide(t, user, form, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve (%v): %d %.300s", scopes, resp.StatusCode, body)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	st, out := h.exchange(t, loc.Query().Get("code"), verifier, clientID)
	if st != 200 {
		t.Fatalf("token: %d %v", st, out)
	}
	return oaTokens{access: out["access_token"].(string), refresh: out["refresh_token"].(string), clientID: clientID}
}

var challengeScopeRe = regexp.MustCompile(`scope="([^"]*)"`)

func (h *oaHarness) stepUpHeader(scopes string) string {
	return `Bearer error="insufficient_scope", scope="` + scopes + `", resource_metadata="` + h.issuer +
		`/.well-known/oauth-protected-resource/mcp"`
}

func TestOAuthStepUpChallenge(t *testing.T) {
	h := newOAHarness(t)
	tok := h.connect(t, h.a.UserID) // mail:read, search:read

	// send_draft needs mail:send, which the grant lacks: 403 and the exact challenge.
	st, hd, _, body := h.toolCall(t, tok.access, "send_draft", map[string]any{"emailId": ulid.Make().String()})
	if st != http.StatusForbidden || hd.Get("WWW-Authenticate") != h.stepUpHeader("mail:send") {
		t.Fatalf("send_draft on a read-only grant: %d %q (%.200s)", st, hd.Get("WWW-Authenticate"), body)
	}
	if strings.Contains(body, `"result"`) {
		t.Errorf("the 403 still carries the tool result: %s", body)
	}

	// The challenge names EVERY scope the tool's declared uses need, from the
	// gate's own table, including one the grant already holds (draft_reply reads
	// the message it replies to).
	st, hd, _, body = h.toolCall(t, tok.access, "draft_reply", map[string]any{"emailId": h.a.MessageID.String(), "body": "x"})
	if st != http.StatusForbidden {
		t.Fatalf("draft_reply on a read-only grant: %d %.200s", st, body)
	}
	want := h.stepUpScopesOf(t, "draft_reply")
	if got := hd.Get("WWW-Authenticate"); got != h.stepUpHeader(strings.Join(want, " ")) || !strings.Contains(strings.Join(want, " "), "mail:read") || len(want) < 2 {
		t.Errorf("draft_reply challenge = %q, want scopes %v (all the tool needs, mail:read included)", got, want)
	}

	// Controls. A tool inside the grant: no 403, no challenge.
	st, hd, res, body := h.toolCall(t, tok.access, "list_mailboxes", map[string]any{})
	if st != 200 || hd.Get("WWW-Authenticate") != "" || res["isError"] == true {
		t.Errorf("list_mailboxes: %d %q %s", st, hd.Get("WWW-Authenticate"), body)
	}
	// An API key with the SAME scopes calling the same tool: today's tool error,
	// status 200, no challenge. A key cannot be re-consented.
	keyTok, _ := h.newKey(t, h.a.UserID, map[string]any{"name": "twin", "scopes": []string{"mail:read", "search:read"}})
	st, hd, res, body = h.toolCall(t, keyTok, "send_draft", map[string]any{"emailId": ulid.Make().String()})
	if st != 200 || hd.Get("WWW-Authenticate") != "" || res["isError"] != true || !strings.Contains(body, "mail:send") {
		t.Errorf("an API key calling send_draft without mail:send: %d %q isError=%v %.300s", st, hd.Get("WWW-Authenticate"), res["isError"], body)
	}
	// The same OAuth call is a tool error for a client that does not read the
	// challenge only in that the status differs; the audit row is the same.
	keyID, _, _, _ := h.grantFor(t, h.a.UserID, tok.clientID)
	if rows := h.events(t, keyID); len(rows) == 0 {
		t.Error("the refused OAuth call left no audit row")
	}
}

// stepUpScopesOf is the scopes a grant with no scopes would be told it needs for
// a tool: the same computation the challenge uses, asked directly of a key with
// nothing, so the test compares the header with the declared uses rather than
// with a list typed in twice.
func (h *oaHarness) stepUpScopesOf(t *testing.T, tool string) []string {
	t.Helper()
	tokKey, _ := h.newKey(t, h.a.UserID, map[string]any{"name": "probe-" + tool, "scopes": []string{"labels:read"}})
	_, _, res, body := h.toolCall(t, tokKey, tool, map[string]any{"emailId": h.a.MessageID.String(), "body": "x"})
	// A key's refusal lists what it lacks in detail.missing; a key that holds
	// labels:read lacks every scope the tool needs.
	structured, _ := res["structuredContent"].(map[string]any)
	e, _ := structured["error"].(map[string]any)
	d, _ := e["detail"].(map[string]any)
	var missing []string
	for _, m := range asArr(d["missing"]) {
		missing = append(missing, m.(string))
	}
	if len(missing) == 0 {
		t.Fatalf("no missing scopes in %.400s", body)
	}
	return missing
}

// Nothing but a scope refusal earns the 403.
func TestOAuthStepUpOnlyForScope(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()

	// Control first: the same harness gives the 403 for a scope refusal, so the
	// absences below are not "the 403 never works here".
	tok := h.connectWith(t, h.a.UserID, cid, "contacts:read", []string{"contacts:read"}, nil)
	if st, _, _, body := h.toolCall(t, tok.access, "send_draft", map[string]any{"emailId": ulid.Make().String()}); st != http.StatusForbidden {
		t.Fatalf("control: a scope refusal gave %d (%.200s), want 403", st, body)
	}

	// An account restriction: the grant holds every scope find_contacts needs,
	// but is limited to some accounts and the tool cannot keep to that. Refused,
	// but not for a scope: a tool error, no challenge.
	verifier, challenge := pkce()
	_, page := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"scope": {"contacts:read"}}))
	form := defaultDecision(requestID(t, page), "approve")
	form["scope"] = []string{"contacts:read"}
	form["accounts_mode"] = []string{"some"}
	form["account"] = []string{h.a.AccountID.String()}
	resp, b := h.decide(t, h.a.UserID, form, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("restricted approval: %d %.300s", resp.StatusCode, b)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	_, out := h.exchange(t, loc.Query().Get("code"), verifier, cid)
	restrictedTok := out["access_token"].(string)
	st, hd, res, body := h.toolCall(t, restrictedTok, "find_contacts", map[string]any{"query": "a"})
	if st != 200 || hd.Get("WWW-Authenticate") != "" || res["isError"] != true || !strings.Contains(body, "restrict") {
		t.Errorf("an account restriction: %d %q isError=%v %.300s", st, hd.Get("WWW-Authenticate"), res["isError"], body)
	}

	// The gate checks the restriction BEFORE the scopes, so a restricted grant that
	// ALSO lacks a scope is refused for the restriction, and the answer must not
	// become a request for the scope: asking for contacts:read would not make the
	// call possible. (This is the case only the gate's own mark can tell apart.)
	h3 := newOAHarness(t)
	verifier, challenge = pkce()
	_, page = h3.authorize(t, h3.a.UserID, h3.authorizeURL(h3.cimdURL(), challenge, url.Values{"scope": {"mail:read"}}))
	form = defaultDecision(requestID(t, page), "approve")
	form["scope"] = []string{"mail:read"}
	form["accounts_mode"] = []string{"some"}
	form["account"] = []string{h3.a.AccountID.String()}
	resp, b = h3.decide(t, h3.a.UserID, form, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("restricted approval: %d %.300s", resp.StatusCode, b)
	}
	loc, _ = url.Parse(resp.Header.Get("Location"))
	_, out = h3.exchange(t, loc.Query().Get("code"), verifier, h3.cimdURL())
	st, hd, res, body = h3.toolCall(t, out["access_token"].(string), "find_contacts", map[string]any{"query": "a"})
	if st != 200 || hd.Get("WWW-Authenticate") != "" || res["isError"] != true {
		t.Errorf("a restricted grant that also lacks the scope: %d %q isError=%v %.300s", st, hd.Get("WWW-Authenticate"), res["isError"], body)
	}

	// The rate: a grant allowed one call a minute. The second call is refused
	// for the rate; its scopes are fine.
	h2 := newOAHarness(t)
	verifier, challenge = pkce()
	_, page = h2.authorize(t, h2.a.UserID, h2.authorizeURL(h2.cimdURL(), challenge, nil))
	form = defaultDecision(requestID(t, page), "approve")
	form["rate"] = []string{"1"}
	resp, b = h2.decide(t, h2.a.UserID, form, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rate-limited approval: %d %.300s", resp.StatusCode, b)
	}
	loc, _ = url.Parse(resp.Header.Get("Location"))
	_, out = h2.exchange(t, loc.Query().Get("code"), verifier, h2.cimdURL())
	slow := out["access_token"].(string)
	if st, _, res, body := h2.toolCall(t, slow, "list_mailboxes", map[string]any{}); st != 200 || res["isError"] == true {
		t.Fatalf("first call: %d %.300s", st, body)
	}
	st, hd, res, body = h2.toolCall(t, slow, "list_mailboxes", map[string]any{})
	if st != 200 || hd.Get("WWW-Authenticate") != "" || res["isError"] != true || !strings.Contains(strings.ToLower(body), "rate") {
		t.Errorf("a rate refusal: %d %q isError=%v %.300s", st, hd.Get("WWW-Authenticate"), res["isError"], body)
	}

	// A suspended grant is refused at the bearer gate: 401, not a challenge for more scope.
	keyID, _, _, _ := h.grantFor(t, h.a.UserID, cid)
	if _, err := h.super.Exec(context.Background(), `UPDATE app.api_keys SET suspended_at=now() WHERE id=$1`, keyID); err != nil {
		t.Fatal(err)
	}
	st, hd, _, _ = h.toolCall(t, tok.access, "send_draft", map[string]any{"emailId": ulid.Make().String()})
	if st != http.StatusUnauthorized || strings.Contains(hd.Get("WWW-Authenticate"), "insufficient_scope") {
		t.Errorf("a suspended grant: %d %q, want 401 and no insufficient_scope", st, hd.Get("WWW-Authenticate"))
	}
	// Other methods and shapes pass through untouched (no buffering surprises).
	if st, _ := h2.bearer(t, http.MethodGet, "/mcp", slow); st != http.StatusMethodNotAllowed && st != http.StatusOK && st != http.StatusBadRequest {
		t.Errorf("GET /mcp with an OAuth token: %d", st)
	}
}

// The whole step-up journey: read-only grant, a draft is refused with the scope
// it needs, the user approves it on the consent page, the call succeeds; then
// the same for sending, which the user chooses to approve each time.
func TestOAuthStepUpEndToEnd(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	addr := "me-" + h.a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, h.a, addr, "")
	ctx := context.Background()

	tok := h.connectWith(t, h.a.UserID, cid, "mail:read", []string{"mail:read"}, nil)
	draftArgs := map[string]any{"from": addr, "to": []string{"bob@example.net"}, "subject": "Lunch", "body": "Thursday?"}

	// 1. draft_new: refused for mail:draft.
	st, hd, _, body := h.toolCall(t, tok.access, "draft_new", draftArgs)
	if st != http.StatusForbidden || hd.Get("WWW-Authenticate") != h.stepUpHeader("mail:draft") {
		t.Fatalf("draft_new on mail:read: %d %q %.200s", st, hd.Get("WWW-Authenticate"), body)
	}
	needed := challengeScopeRe.FindStringSubmatch(hd.Get("WWW-Authenticate"))[1]

	// Re-consent, asking for exactly what the challenge named. The page shows
	// the grant's existing scope and the new one; the user approves both.
	_, challenge := pkce()
	_, page := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"scope": {needed}}))
	for _, want := range []string{`value="mail:read" checked`, `value="mail:draft" checked`} {
		if !strings.Contains(page, want) {
			t.Errorf("the re-consent page lacks %q", want)
		}
	}
	tok2 := h.connectWith(t, h.a.UserID, cid, needed, []string{"mail:read", "mail:draft"}, nil)
	var grants int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.api_keys WHERE user_id=$1 AND kind='oauth' AND oauth_client_id=$2 AND revoked_at IS NULL`,
		h.a.UserID.String(), cid).Scan(&grants)
	if _, scopes, _, _ := h.grantFor(t, h.a.UserID, cid); grants != 1 || strings.Join(scopes, ",") != "mail:draft,mail:read" {
		t.Fatalf("after re-consent: %d live grants with scopes %v, want one with mail:draft,mail:read", grants, scopes)
	}
	st, _, res, body := h.toolCall(t, tok2.access, "draft_new", draftArgs)
	if st != 200 || res["isError"] == true {
		t.Fatalf("draft_new after step-up: %d %.400s", st, body)
	}
	structured, _ := res["structuredContent"].(map[string]any)
	draft, _ := structured["emailId"].(string)
	if draft == "" {
		t.Fatalf("no draft id in %.400s", body)
	}

	// 2. send_draft: refused for mail:send; the user approves it, with each send
	// waiting for their approval.
	st, hd, _, body = h.toolCall(t, tok2.access, "send_draft", map[string]any{"emailId": draft})
	if st != http.StatusForbidden || hd.Get("WWW-Authenticate") != h.stepUpHeader("mail:send") {
		t.Fatalf("send_draft on mail:read mail:draft: %d %q %.200s", st, hd.Get("WWW-Authenticate"), body)
	}
	tok3 := h.connectWith(t, h.a.UserID, cid, "mail:send", []string{"mail:read", "mail:draft", "mail:send"}, []string{"mail:send"})
	st, _, res, body = h.toolCall(t, tok3.access, "send_draft", map[string]any{"emailId": draft})
	structured, _ = res["structuredContent"].(map[string]any)
	if st != 200 || res["isError"] == true || structured["status"] != "pending_approval" {
		t.Fatalf("send_draft after step-up (confirm mode): %d %.500s", st, body)
	}
	var subs int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.submissions WHERE user_id=$1`, h.a.UserID.String()).Scan(&subs)
	if subs != 0 {
		t.Errorf("a held send created %d submissions", subs)
	}
	if _, scopes, _, _ := h.grantFor(t, h.a.UserID, cid); strings.Join(scopes, ",") != "mail:draft,mail:read,mail:send" {
		t.Errorf("final scopes %v", scopes)
	}
	// The grant's ceiling still holds: a scope no one asked for is not in it.
	if names, _ := h.mcp(t, tok3.access).ToolNames(ctx); strings.Contains(strings.Join(names, ","), "delete_message") {
		t.Errorf("delete_message listed to a grant without mail:delete: %v", names)
	}
}
