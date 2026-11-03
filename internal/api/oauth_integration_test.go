// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/mcp/mcptest"
	"mailroom/internal/oauthserver"
)

// THE OAUTH AUTHORIZATION SERVER END TO END (ADR-050, increment 1). Every test
// drives the composed stack over real HTTP: the discovery documents, the
// browser endpoints behind a stand-in session gate (cookie "u:<user id>" is that
// user's browser session), the token endpoint, and /mcp and /jmap behind the
// REAL bearer gate and store resolvers, with the MCP SDK's own client.

type oaHarness struct {
	*agentHarness
	srv     *httptest.Server
	os      *oauthserver.Server
	issuer  string
	cimd    *httptest.Server
	cimdMu  sync.Mutex
	cimdFn  http.HandlerFunc
	browser *http.Client
	a, b    apiFixture
}

const oaRedirect = "https://app.example.com/cb"

func newOAHarness(t *testing.T) *oaHarness {
	t.Helper()
	h := &oaHarness{agentHarness: newAgentHarness(t)}
	h.a, h.b = seedTwo(t, h.super)
	h.browser = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// The client's metadata host: TLS, trusted by the Doer below.
	h.cimd = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.cimdMu.Lock()
		fn := h.cimdFn
		h.cimdMu.Unlock()
		fn(w, r)
	}))
	t.Cleanup(h.cimd.Close)
	h.setCIMD(h.cimdDoc(h.cimdURL(), map[string]any{}))
	doer := h.cimd.Client()
	doer.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	srv := httptest.NewUnstartedServer(nil)
	h.issuer = "http://" + srv.Listener.Addr().String()
	var err error
	h.os, err = oauthserver.New(oauthserver.Config{Issuer: h.issuer, Store: h.store, Policy: OAuthPolicy(),
		Approve: ApproveOAuthGrant, Alert: h.api.AlertOAuthRefreshReuse, CIMD: doer,
		CIMDTimeout: 300 * time.Millisecond, Limiter: auth.NewFailureLimiter(1000, time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	apiH := h.api.Handler()
	session := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie(auth.DefaultSessionCookieName); err == nil && strings.HasPrefix(c.Value, "u:") {
				if id, perr := ulid.Parse(strings.TrimPrefix(c.Value, "u:")); perr == nil {
					auth.DevAuth(next, id).ServeHTTP(w, r)
					return
				}
			}
			http.Redirect(w, r, "/auth/login", http.StatusFound)
		})
	}
	gate := &auth.BearerGate{Resolve: h.store.ResolveAPIKeyToken, ResolveOAuth: h.store.ResolveOAuthAccessToken,
		OAuthPath: MCPPath, ChallengePath: MCPPath, Challenge: h.os.Challenge(),
		Limiter: auth.NewFailureLimiter(1000, time.Millisecond)}
	apiSession := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(auth.DefaultSessionCookieName); err == nil && strings.HasPrefix(c.Value, "u:") {
			id, _ := ulid.Parse(strings.TrimPrefix(c.Value, "u:"))
			auth.DevAuth(apiH, id).ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	mux := http.NewServeMux()
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", h.os.ProtectedResource())
	mux.Handle("GET /.well-known/oauth-protected-resource", h.os.ProtectedResource())
	mux.Handle("GET /.well-known/oauth-authorization-server", h.os.AuthorizationServer())
	mux.Handle("GET /oauth/authorize", session(h.os.Authorize()))
	mux.Handle("POST /oauth/consent", session(h.os.Consent()))
	mux.Handle("POST /oauth/token", h.os.Token())
	mux.Handle("POST /oauth/revoke", h.os.Revoke())
	mux.Handle("/mcp", MCPChallengeWith(gate.Wrap(apiH, apiSession), auth.DefaultSessionCookieName, h.os.Challenge()))
	mux.Handle("/jmap", gate.Wrap(apiH, apiSession))
	mux.Handle("/jmap/session", gate.Wrap(apiH, apiSession))
	srv.Config.Handler = mux
	srv.Start()
	t.Cleanup(srv.Close)
	h.srv = srv
	return h
}

func (h *oaHarness) cimdURL() string { return h.cimd.URL + "/client.json" }

func (h *oaHarness) setCIMD(fn http.HandlerFunc) {
	h.cimdMu.Lock()
	defer h.cimdMu.Unlock()
	h.cimdFn = fn
}

// cimdDoc serves a valid document for clientID, with fields overridden by over
// (a nil value deletes the field).
func (h *oaHarness) cimdDoc(clientID string, over map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{"client_id": clientID, "client_name": "Example Assistant",
			"redirect_uris": []string{oaRedirect, "http://127.0.0.1/cb"}, "token_endpoint_auth_method": "none"}
		for k, v := range over {
			if v == nil {
				delete(doc, k)
			} else {
				doc[k] = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// ---- driving the endpoints --------------------------------------------------

func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v1Z-._~", 7)[:43] + time.Now().Format("150405.000000")[:6]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (h *oaHarness) do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := h.browser.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (h *oaHarness) authorizeURL(clientID, challenge string, over url.Values) string {
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {oaRedirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st-1"},
		"scope": {"mail:read search:read"}, "resource": {h.issuer + "/mcp"}}
	for k, v := range over {
		if v == nil {
			q.Del(k)
		} else {
			q[k] = v
		}
	}
	return h.srv.URL + "/oauth/authorize?" + q.Encode()
}

func (h *oaHarness) authorize(t *testing.T, user ulid.ULID, rawURL string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	if user != (ulid.ULID{}) {
		req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: "u:" + user.String()})
	}
	return h.do(t, req)
}

var requestIDRe = regexp.MustCompile(`name="request" value="([^"]+)"`)

func requestID(t *testing.T, body string) string {
	t.Helper()
	m := requestIDRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no consent request id in the page: %.300s", body)
	}
	return m[1]
}

func defaultDecision(reqID, action string) url.Values {
	return url.Values{"request": {reqID}, "action": {action}, "scope": {"mail:read", "search:read"},
		"accounts_mode": {"all"}, "rate": {"60"}, "budget": {"500"}, "suspend": {"1"}}
}

func (h *oaHarness) decide(t *testing.T, user ulid.ULID, form url.Values, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/oauth/consent", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: "u:" + user.String()})
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return h.do(t, req)
}

func (h *oaHarness) token(t *testing.T, form url.Values) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, body := h.do(t, req)
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	return resp.StatusCode, out, resp.Header
}

type oaTokens struct {
	access, refresh, code, verifier, clientID string
}

// grantCode runs authorize and approve and returns the authorization code.
func (h *oaHarness) grantCode(t *testing.T, user ulid.ULID, clientID string) (code, verifier string) {
	t.Helper()
	verifier, challenge := pkce()
	resp, body := h.authorize(t, user, h.authorizeURL(clientID, challenge, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: status %d: %.300s", resp.StatusCode, body)
	}
	resp, body = h.decide(t, user, defaultDecision(requestID(t, body), "approve"), nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: status %d: %.300s", resp.StatusCode, body)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("approve redirected to %q", resp.Header.Get("Location"))
	}
	if loc.Query().Get("iss") != h.issuer || loc.Query().Get("state") != "st-1" {
		t.Errorf("redirect lacks iss/state: %s", loc)
	}
	return loc.Query().Get("code"), verifier
}

func (h *oaHarness) exchange(t *testing.T, code, verifier, clientID string) (int, map[string]any) {
	st, out, _ := h.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"client_id": {clientID}, "redirect_uri": {oaRedirect}})
	return st, out
}

// connect runs the whole dance for user and returns the tokens.
func (h *oaHarness) connect(t *testing.T, user ulid.ULID) oaTokens {
	t.Helper()
	cid := h.cimdURL()
	code, verifier := h.grantCode(t, user, cid)
	st, out := h.exchange(t, code, verifier, cid)
	if st != 200 {
		t.Fatalf("token: %d %v", st, out)
	}
	return oaTokens{access: out["access_token"].(string), refresh: out["refresh_token"].(string), code: code,
		verifier: verifier, clientID: cid}
}

func (h *oaHarness) refresh(t *testing.T, refresh, clientID string) (int, map[string]any) {
	st, out, _ := h.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	return st, out
}

// status of a bearer request to path.
func (h *oaHarness) bearer(t *testing.T, method, path, token string) (int, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(`{"using":[],"methodCalls":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ := h.do(t, req)
	return resp.StatusCode, resp.Header
}

func (h *oaHarness) mcp(t *testing.T, token string) *mcptest.Session {
	t.Helper()
	s, err := mcptest.Connect(context.Background(), h.srv.URL+MCPPath, token, h.srv.Client())
	if err != nil {
		t.Fatalf("MCP connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (h *oaHarness) grantFor(t *testing.T, user ulid.ULID, clientID string) (keyID string, scopes []string, revoked, suspended bool) {
	t.Helper()
	err := h.super.QueryRow(context.Background(), `
		SELECT id, scopes, revoked_at IS NOT NULL, suspended_at IS NOT NULL FROM app.api_keys
		 WHERE user_id=$1 AND kind='oauth' AND oauth_client_id=$2 ORDER BY created_at DESC LIMIT 1`,
		user.String(), clientID).Scan(&keyID, &scopes, &revoked, &suspended)
	if err != nil {
		t.Fatalf("grant row: %v", err)
	}
	return
}

// ---- tests -------------------------------------------------------------------

func TestOAuthDiscoveryAndChallenge(t *testing.T) {
	h := newOAHarness(t)
	get := func(path string) map[string]any {
		resp, body := h.do(t, mustReq(http.MethodGet, h.srv.URL+path))
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		return m
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		m := get(p)
		if m["resource"] != h.issuer+"/mcp" || m["resource_name"] != "Mailroom" ||
			jsonOf(m["authorization_servers"]) != `["`+h.issuer+`"]` ||
			jsonOf(m["bearer_methods_supported"]) != `["header"]` ||
			jsonOf(m["scopes_supported"]) != `["mail:read","search:read"]` {
			t.Errorf("%s: %v", p, m)
		}
	}
	m := get("/.well-known/oauth-authorization-server")
	want := map[string]string{"issuer": h.issuer, "authorization_endpoint": h.issuer + "/oauth/authorize",
		"token_endpoint": h.issuer + "/oauth/token", "revocation_endpoint": h.issuer + "/oauth/revoke"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %s", k, m[k], v)
		}
	}
	for k, v := range map[string]string{"response_types_supported": `["code"]`,
		"grant_types_supported":                 `["authorization_code","refresh_token"]`,
		"code_challenge_methods_supported":      `["S256"]`,
		"token_endpoint_auth_methods_supported": `["none"]`} {
		if jsonOf(m[k]) != v {
			t.Errorf("%s = %s, want %s", k, jsonOf(m[k]), v)
		}
	}
	if m["client_id_metadata_document_supported"] != true || m["authorization_response_iss_parameter_supported"] != true {
		t.Errorf("flags: %v", m)
	}
	if _, has := m["registration_endpoint"]; has {
		t.Error("registration_endpoint advertised before DCR exists")
	}
	if !strings.Contains(jsonOf(m["scopes_supported"]), "mail:delete") {
		t.Errorf("AS scopes_supported is not the closed set: %v", m["scopes_supported"])
	}

	// The 401 challenge on /mcp: no credential, a bad one, a malformed one.
	wantCh := `Bearer realm="mailroom", resource_metadata="` + h.issuer +
		`/.well-known/oauth-protected-resource/mcp", scope="mail:read search:read"`
	for name, tok := range map[string]string{"none": "", "unknown access": "mra_" + strings.Repeat("a", 52),
		"unknown key": "mr_" + strings.Repeat("a", 52), "malformed": "junk"} {
		var st int
		var hd http.Header
		if tok == "" {
			resp, _ := h.do(t, mustReq(http.MethodPost, h.srv.URL+"/mcp"))
			st, hd = resp.StatusCode, resp.Header
		} else {
			st, hd = h.bearer(t, http.MethodPost, "/mcp", tok)
		}
		if st != 401 || hd.Get("WWW-Authenticate") != wantCh {
			t.Errorf("/mcp %s: %d %q, want 401 %q", name, st, hd.Get("WWW-Authenticate"), wantCh)
		}
	}
	// Other routes keep the plain challenge.
	if st, hd := h.bearer(t, http.MethodPost, "/jmap", "mra_"+strings.Repeat("a", 52)); st != 401 ||
		hd.Get("WWW-Authenticate") != `Bearer realm="mailroom"` {
		t.Errorf("/jmap: %d %q", st, hd.Get("WWW-Authenticate"))
	}
}

func mustReq(method, u string) *http.Request {
	r, _ := http.NewRequest(method, u, nil)
	return r
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// The whole journey: authorize, approve, token, an MCP tool call through the
// real handler chain, refresh, revoke, 401.
func TestOAuthEndToEnd(t *testing.T) {
	h := newOAHarness(t)
	tok := h.connect(t, h.a.UserID)
	if !strings.HasPrefix(tok.access, "mra_") || !strings.HasPrefix(tok.refresh, "mrr_") {
		t.Fatalf("token prefixes: %q %q", tok.access[:4], tok.refresh[:4])
	}
	sess := h.mcp(t, tok.access)
	names, err := sess.ToolNames(context.Background())
	if err != nil || len(names) == 0 {
		t.Fatalf("tools/list: %v %v", names, err)
	}
	if res := mcpCall(t, sess, "list_mailboxes", map[string]any{}); res.IsError {
		t.Fatalf("list_mailboxes: %s", res.Text)
	}
	// The consent became an OAuth grant, listed beside keys with its client.
	keyID, scopes, _, _ := h.grantFor(t, h.a.UserID, tok.clientID)
	if strings.Join(scopes, ",") != "mail:read,search:read" {
		t.Errorf("grant scopes = %v", scopes)
	}
	res := call(t, h.api.apiKeyGet, h.a.UserID, map[string]any{"ids": []string{keyID}})
	obj := asArr(res["list"])[0].(map[string]any)
	oc, _ := obj["oauthClient"].(map[string]any)
	if obj["kind"] != "oauth" || obj["tokenHint"] != nil || oc["id"] != tok.clientID || oc["name"] != "Example Assistant" ||
		oc["redirectHost"] != "app.example.com" || len(asArr(obj["families"])) != 1 {
		t.Errorf("ApiKey/get shows the grant as %v", obj)
	}
	// Refresh rotates.
	st, out := h.refresh(t, tok.refresh, tok.clientID)
	if st != 200 || out["refresh_token"] == tok.refresh || out["access_token"] == tok.access {
		t.Fatalf("refresh: %d %v", st, out)
	}
	newAccess, newRefresh := out["access_token"].(string), out["refresh_token"].(string)
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", newAccess); s == 401 {
		t.Error("the refreshed access token is refused")
	}
	// Revoke the refresh token (RFC 7009): the family's access tokens go too.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/oauth/revoke", strings.NewReader(url.Values{"token": {newRefresh}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := h.do(t, req); resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	for name, a := range map[string]string{"new": newAccess, "first": tok.access} {
		if s, _ := h.bearer(t, http.MethodPost, "/mcp", a); s != 401 {
			t.Errorf("%s access token after revocation: %d, want 401", name, s)
		}
	}
	if st, _ := h.refresh(t, newRefresh, tok.clientID); st != 400 {
		t.Errorf("revoked refresh token: %d, want 400", st)
	}
	// Token responses are never cached.
	_, _, hd := h.token(t, url.Values{"grant_type": {"bogus"}})
	if hd.Get("Cache-Control") != "no-store" {
		t.Errorf("token endpoint Cache-Control = %q", hd.Get("Cache-Control"))
	}
}

// ADR-050 section 9, Audience.
func TestOAuthAudience(t *testing.T) {
	h := newOAHarness(t)
	tok := h.connect(t, h.a.UserID)
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", tok.access); s == 401 {
		t.Fatalf("positive control: the access token is refused on /mcp")
	}
	for _, p := range []string{"/jmap", "/jmap/session"} {
		for _, m := range []string{http.MethodPost, http.MethodGet} {
			if s, _ := h.bearer(t, m, p, tok.access); s != 401 {
				t.Errorf("OAuth access token on %s %s: %d, want 401", m, p, s)
			}
		}
	}
	// An API key keeps /mcp and /jmap.
	key, _ := h.newKey(t, h.a.UserID, map[string]any{"name": "k", "scopes": []string{"mail:read"}})
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", key); s == 401 {
		t.Error("an API key is refused on /mcp")
	}
	if s, _ := h.bearer(t, http.MethodPost, "/jmap", key); s == 401 {
		t.Error("an API key is refused on /jmap")
	}
	// A refresh token is never a bearer credential.
	for _, p := range []string{"/mcp", "/jmap"} {
		if s, _ := h.bearer(t, http.MethodPost, p, tok.refresh); s != 401 {
			t.Errorf("refresh token as bearer on %s: %d, want 401", p, s)
		}
	}
}

// ADR-050 section 9, Revocation is immediate.
func TestOAuthRevocationIsImmediate(t *testing.T) {
	h := newOAHarness(t)
	check := func(label, access string, want bool) {
		t.Helper()
		s, _ := h.bearer(t, http.MethodPost, "/mcp", access)
		if (s != 401) != want {
			t.Errorf("%s: /mcp status %d, live=%v expected", label, s, want)
		}
	}
	tok := h.connect(t, h.a.UserID)
	keyID, _, _, _ := h.grantFor(t, h.a.UserID, tok.clientID)
	check("before", tok.access, true)

	call(t, h.api.apiKeySet, h.a.UserID, map[string]any{"update": map[string]any{keyID: map[string]any{"suspended": true}}})
	check("suspended", tok.access, false)
	call(t, h.api.apiKeySet, h.a.UserID, map[string]any{"update": map[string]any{keyID: map[string]any{"suspended": false}}})
	check("reactivated", tok.access, true)

	// One family: a second device connects; revoking the first leaves it alone.
	second := h.connect(t, h.a.UserID)
	check("second device", second.access, true)
	res := call(t, h.api.apiKeyGet, h.a.UserID, map[string]any{"ids": []string{keyID}})
	fams := asArr(asArr(res["list"])[0].(map[string]any)["families"])
	if len(fams) != 2 {
		t.Fatalf("families = %v", fams)
	}
	var firstFam string
	if err := h.super.QueryRow(context.Background(), `SELECT family_id FROM app.oauth_tokens WHERE token_hash=$1`,
		oaHash(tok.access)).Scan(&firstFam); err != nil {
		t.Fatal(err)
	}
	call(t, h.api.oauthFamilySet, h.a.UserID, map[string]any{"destroy": []string{firstFam}})
	check("revoked family", tok.access, false)
	check("other family", second.access, true)
	if st, _ := h.refresh(t, tok.refresh, tok.clientID); st != 400 {
		t.Errorf("refresh of a revoked family: %d", st)
	}

	call(t, h.api.apiKeySet, h.a.UserID, map[string]any{"destroy": []string{keyID}})
	check("revoked grant", second.access, false)
	if st, _ := h.refresh(t, second.refresh, second.clientID); st != 400 {
		t.Errorf("refresh after the grant was revoked: %d", st)
	}
}

// ADR-050 section 9, Cross-user.
func TestOAuthCrossUser(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	verifier, challenge := pkce()
	_, body := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil))
	reqID := requestID(t, body)

	// B cannot decide A's request.
	resp, _ := h.decide(t, h.b.UserID, defaultDecision(reqID, "approve"), nil)
	if resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Fatalf("B approving A's request: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var grants int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.api_keys WHERE kind='oauth' AND user_id IN ($1,$2)`, h.a.UserID.String(), h.b.UserID.String()).Scan(&grants)
	if grants != 0 {
		t.Errorf("B's attempt created %d grants", grants)
	}
	// A can still approve it (B's attempt consumed nothing).
	resp, _ = h.decide(t, h.a.UserID, defaultDecision(reqID, "approve"), nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("A approving: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	// A's code with a verifier that is not A's PKCE verifier (another party's).
	otherVerifier, _ := pkce()
	otherVerifier = "B" + otherVerifier[1:]
	if st, _ := h.exchange(t, code, otherVerifier, cid); st != 400 {
		t.Errorf("code redeemed with the wrong party's verifier: %d, want 400", st)
	}
	_ = verifier
	// B's view of A's grant and tokens: nothing (RLS), by the API.
	res := call(t, h.api.apiKeyGet, h.b.UserID, map[string]any{})
	if b, _ := json.Marshal(res); strings.Contains(string(b), cid) {
		t.Errorf("B's key list shows A's grant: %s", b)
	}
	tokA := h.connect(t, h.a.UserID)
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", tokA.access); s == 401 {
		t.Fatal("control")
	}
	// B's own grant for the same client is a separate row, not A's.
	tokB := h.connect(t, h.b.UserID)
	kA, _, _, _ := h.grantFor(t, h.a.UserID, cid)
	kB, _, _, _ := h.grantFor(t, h.b.UserID, cid)
	if kA == kB {
		t.Error("two users share one grant")
	}
	call(t, h.api.apiKeySet, h.b.UserID, map[string]any{"destroy": []string{kA}})
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", tokA.access); s == 401 {
		t.Error("B revoked A's grant")
	}
	_ = tokB
}

// ADR-050 section 9, PKCE.
func TestOAuthPKCE(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	_, challenge := pkce()
	for name, over := range map[string]url.Values{
		"missing challenge": {"code_challenge": nil},
		"plain method":      {"code_challenge_method": {"plain"}},
		"no method":         {"code_challenge_method": nil},
		"short challenge":   {"code_challenge": {"abc"}},
	} {
		resp, _ := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, over))
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusSeeOther || loc.Query().Get("error") != "invalid_request" ||
			!strings.HasPrefix(loc.String(), oaRedirect) {
			t.Errorf("authorize with %s: %d %q", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	code, verifier := h.grantCode(t, h.a.UserID, cid)
	if st, out := h.exchange(t, code, "", cid); st != 400 || out["error"] != "invalid_request" {
		t.Errorf("missing verifier: %d %v", st, out)
	}
	code, verifier = h.grantCode(t, h.a.UserID, cid)
	wrong := verifier[:len(verifier)-1] + "X"
	if verifier[len(verifier)-1] == 'X' {
		wrong = verifier[:len(verifier)-1] + "Y"
	}
	if st, out := h.exchange(t, code, wrong, cid); st != 400 || out["error"] != "invalid_grant" {
		t.Errorf("wrong verifier: %d %v", st, out)
	}
	code, verifier = h.grantCode(t, h.a.UserID, cid)
	if st, _ := h.exchange(t, code, verifier, cid); st != 200 {
		t.Errorf("positive control: %d", st)
	}
}

// ADR-050 section 9, Codes.
func TestOAuthCodes(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	post := func(code, verifier string, over url.Values) (int, map[string]any) {
		f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
			"client_id": {cid}, "redirect_uri": {oaRedirect}}
		for k, v := range over {
			f[k] = v
		}
		st, out, _ := h.token(t, f)
		return st, out
	}
	// Expired.
	code, verifier := h.grantCode(t, h.a.UserID, cid)
	if _, err := h.super.Exec(context.Background(), `UPDATE app.oauth_codes SET expires_at = now() - interval '1 second' WHERE code_hash=$1`, oaHash(code)); err != nil {
		t.Fatal(err)
	}
	if st, out := post(code, verifier, nil); st != 400 || out["error"] != "invalid_grant" {
		t.Errorf("expired code: %d %v", st, out)
	}
	// Another client_id, redirect URI, resource: each refused.
	for name, over := range map[string]url.Values{
		"other client":   {"client_id": {cid + "x"}},
		"other redirect": {"redirect_uri": {"https://app.example.com/other"}},
		"other resource": {"resource": {"https://example.com/other"}},
	} {
		code, verifier := h.grantCode(t, h.a.UserID, cid)
		if st, out := post(code, verifier, over); st != 400 {
			t.Errorf("%s: %d %v", name, st, out)
		}
	}
	// A second redemption is refused and revokes the family it produced.
	code, verifier = h.grantCode(t, h.a.UserID, cid)
	st, out := post(code, verifier, nil)
	if st != 200 {
		t.Fatalf("first redemption: %d %v", st, out)
	}
	access := out["access_token"].(string)
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", access); s == 401 {
		t.Fatal("control: the first access token should work")
	}
	if st, out := post(code, verifier, nil); st != 400 || out["error"] != "invalid_grant" {
		t.Errorf("replay: %d %v", st, out)
	}
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", access); s != 401 {
		t.Errorf("the family survived a code replay: %d", s)
	}
	keyID, _, _, _ := h.grantFor(t, h.a.UserID, cid)
	if n := h.countReason(t, keyID, "code_replay"); n != 0 {
		_ = n
	}
	var n int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.api_key_events WHERE key_id=$1 AND anomaly='code_replay'`, keyID).Scan(&n)
	if n != 1 {
		t.Errorf("code_replay events = %d, want 1", n)
	}
	if rows := h.auditRows(t, keyID, "oauth.code_replay"); len(rows) != 1 {
		t.Errorf("code_replay audit rows = %d, want 1", len(rows))
	}
}

// ADR-050 section 9, Redirects.
func TestOAuthRedirects(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	_, challenge := pkce()
	for name, ru := range map[string]string{
		"unregistered":     "https://evil.example.net/cb",
		"private-use":      "com.example.app:/cb",
		"http non-loopbck": "http://app.example.com/cb",
		"host case":        "https://App.example.com/cb",
		"extra path":       "https://app.example.com/cb/extra",
		"extra query":      oaRedirect + "?x=1",
		"trailing slash":   oaRedirect + "/",
		"missing":          "",
	} {
		resp, body := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"redirect_uri": {ru}}))
		if resp.StatusCode != 400 || resp.Header.Get("Location") != "" || strings.Contains(body, `name="request"`) {
			t.Errorf("%s: %d location %q", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Loopback matches on any port; the page warns only when every URI is loopback.
	resp, _ := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"redirect_uri": {"http://127.0.0.1:53124/cb"}}))
	if resp.StatusCode != 200 {
		t.Errorf("loopback with a port: %d", resp.StatusCode)
	}
	resp, _ = h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"redirect_uri": {"http://127.0.0.1:53124/other"}}))
	if resp.StatusCode != 400 {
		t.Errorf("loopback with another path: %d", resp.StatusCode)
	}
	// Unknown client and unknown response_type.
	resp, _ = h.authorize(t, h.a.UserID, h.authorizeURL("nonsense", challenge, nil))
	if resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Errorf("unknown client: %d", resp.StatusCode)
	}
	resp, _ = h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"resource": {"https://example.com/other"}}))
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != 303 || loc.Query().Get("error") != "invalid_target" {
		t.Errorf("other resource: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// ADR-050 section 9, Consent forgery.
func TestOAuthConsentForgery(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	_, challenge := pkce()
	resp, body := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil))
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" ||
		strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("consent page policy: CSP %q XFO %q", csp, resp.Header.Get("X-Frame-Options"))
	}
	reqID := requestID(t, body)
	// The page shows the client, how it is identified, and the redirect host.
	for _, want := range []string{"Example Assistant", "127.0.0.1", "app.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	// A cross-site POST is refused before it can decide anything.
	resp, _ = h.decide(t, h.a.UserID, defaultDecision(reqID, "approve"), map[string]string{"Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != 403 {
		t.Errorf("cross-site approve: %d, want 403", resp.StatusCode)
	}
	// Unknown id, other user's id: the same refusal.
	for name, f := range map[string]struct {
		user ulid.ULID
		id   string
	}{"unknown": {h.a.UserID, "mrq_nope"}, "other user": {h.b.UserID, reqID}} {
		if resp, _ := h.decide(t, f.user, defaultDecision(f.id, "approve"), nil); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	// Approved once, never again.
	if resp, _ := h.decide(t, h.a.UserID, defaultDecision(reqID, "approve"), nil); resp.StatusCode != 303 {
		t.Fatalf("approve: %d", resp.StatusCode)
	}
	if resp, _ := h.decide(t, h.a.UserID, defaultDecision(reqID, "approve"), nil); resp.StatusCode != 400 {
		t.Errorf("second approval of the same request: %d, want 400", resp.StatusCode)
	}
	if resp, _ := h.decide(t, h.a.UserID, defaultDecision(reqID, "deny"), nil); resp.StatusCode != 400 {
		t.Errorf("deny after approve: %d, want 400", resp.StatusCode)
	}
	var codes int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.oauth_codes WHERE user_id=$1`, h.a.UserID.String()).Scan(&codes)
	if codes != 1 {
		t.Errorf("codes issued = %d, want 1", codes)
	}
	// Expired.
	_, body = h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil))
	id2 := requestID(t, body)
	_, _ = h.super.Exec(context.Background(), `UPDATE app.oauth_requests SET expires_at = now() - interval '1 second' WHERE request_hash=$1`, oaHash(id2))
	if resp, _ := h.decide(t, h.a.UserID, defaultDecision(id2, "approve"), nil); resp.StatusCode != 400 {
		t.Errorf("expired request approved: %d", resp.StatusCode)
	}
	// Deny redirects with access_denied, state and iss.
	_, body = h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil))
	resp, _ = h.decide(t, h.a.UserID, defaultDecision(requestID(t, body), "deny"), nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 303 || loc.Query().Get("error") != "access_denied" || loc.Query().Get("state") != "st-1" || loc.Query().Get("iss") != h.issuer {
		t.Errorf("deny: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A signed-out visitor never sees the page: the session gate redirects.
	resp, body = h.authorize(t, ulid.ULID{}, h.authorizeURL(cid, challenge, nil))
	if resp.StatusCode != 302 || strings.Contains(body, `name="request"`) {
		t.Errorf("signed out: %d", resp.StatusCode)
	}
}

// ADR-050 section 9, Refresh reuse.
func TestOAuthRefreshReuse(t *testing.T) {
	h := newOAHarness(t)
	tok := h.connect(t, h.a.UserID)
	keyID, _, _, _ := h.grantFor(t, h.a.UserID, tok.clientID)
	st, out := h.refresh(t, tok.refresh, tok.clientID)
	if st != 200 {
		t.Fatalf("rotation: %d %v", st, out)
	}
	live, liveRefresh := out["access_token"].(string), out["refresh_token"].(string)
	// The old refresh token again: reuse.
	if st, out := h.refresh(t, tok.refresh, tok.clientID); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("reuse: %d %v", st, out)
	}
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", live); s != 401 {
		t.Errorf("the family's newest access token survived reuse: %d", s)
	}
	if st, _ := h.refresh(t, liveRefresh, tok.clientID); st != 400 {
		t.Errorf("the family's newest refresh token survived reuse: %d", st)
	}
	var n int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.api_key_events WHERE key_id=$1 AND outcome='anomaly' AND anomaly='refresh_reuse'`, keyID).Scan(&n)
	if n != 1 {
		t.Errorf("refresh_reuse anomaly events = %d, want 1", n)
	}
	if got := h.countAlerts(func(a fakeAlert) bool { return strings.Contains(a.title, "Example Assistant") }); got != 1 {
		t.Errorf("alerts pushed = %d, want 1", got)
	}
	// The grant stays and is not suspended; the client can authorize again.
	if _, _, revoked, suspended := h.grantFor(t, h.a.UserID, tok.clientID); revoked || suspended {
		t.Errorf("reuse revoked=%v suspended=%v the grant", revoked, suspended)
	}
	again := h.connect(t, h.a.UserID)
	if s, _ := h.bearer(t, http.MethodPost, "/mcp", again.access); s == 401 {
		t.Error("re-authorization after reuse does not work")
	}
	var grants int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.api_keys WHERE kind='oauth' AND user_id=$1`, h.a.UserID.String()).Scan(&grants)
	if grants != 1 {
		t.Errorf("re-authorization made %d grants, want 1 (one per user and client)", grants)
	}
}

// ADR-050 section 9, Scope ceiling.
func TestOAuthScopeCeiling(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	_, challenge := pkce()
	// The client asks for mail:read only; a forged form adds mail:delete.
	_, body := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"scope": {"mail:read"}}))
	if strings.Contains(body, `value="mail:delete"`) {
		t.Error("the page offers a scope nobody asked for")
	}
	reqID := requestID(t, body)
	form := defaultDecision(reqID, "approve")
	form["scope"] = []string{"mail:read", "mail:delete"}
	if resp, _ := h.decide(t, h.a.UserID, form, nil); resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a scope beyond the request was approved")
	}
	// The request is still undecided: the honest form works, holding mail:read only.
	form["scope"] = []string{"mail:read"}
	resp, _ := h.decide(t, h.a.UserID, form, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("honest approval: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	verifier, _ := pkce()
	_ = verifier
	// (the verifier of this request is lost; use a fresh flow for the token)
	_, scopes, _, _ := h.grantFor(t, h.a.UserID, cid)
	if strings.Join(scopes, ",") != "mail:read" {
		t.Errorf("grant scopes = %v", scopes)
	}
	_ = loc
	tok := h.connect(t, h.a.UserID) // default decision: mail:read + search:read of the request
	_, scopes, _, _ = h.grantFor(t, h.a.UserID, cid)
	if strings.Join(scopes, ",") != "mail:read,search:read" {
		t.Errorf("re-authorized grant scopes = %v", scopes)
	}
	// The gate refuses beyond the grant: draft creation needs mail:draft.
	sess := h.mcp(t, tok.access)
	names, _ := sess.ToolNames(context.Background())
	for _, n := range names {
		if n == "draft_new" || n == "delete_message" || n == "send_draft" {
			t.Errorf("tool %s is listed to a read-only grant", n)
		}
	}
	// A refresh cannot widen it.
	st, out, _ := h.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.refresh},
		"client_id": {cid}, "scope": {"mail:read mail:delete"}})
	if st != 400 || out["error"] != "invalid_scope" {
		t.Errorf("widening refresh: %d %v", st, out)
	}
	// ...and it can narrow (a refused refresh consumed that token, so use a new connection).
	h.connectAgain(t, h.connect(t, h.a.UserID))
}

func (h *oaHarness) connectAgain(t *testing.T, tok oaTokens) (int, map[string]any) {
	st, out, _ := h.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.refresh},
		"client_id": {tok.clientID}, "scope": {"mail:read"}})
	if st != 200 || out["scope"] != "mail:read" {
		t.Errorf("narrowing refresh: %d %v", st, out)
	}
	return st, out
}

// A grant is subject to the SAME gate as a key: the tools an OAuth client sees
// are exactly the tools a key with the same settings sees (ADR-050 no new
// enforcement path), and a confirm-mode scope holds for approval.
func TestOAuthGrantIsKeyGated(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	tok := h.connect(t, h.a.UserID)
	keyTok, _ := h.newKey(t, h.a.UserID, map[string]any{"name": "twin", "scopes": []string{"mail:read", "search:read"}})
	oauthTools, _ := h.mcp(t, tok.access).ToolNames(context.Background())
	keyTools, _ := mcpConnect(t, h.mcpKeyServer(t), keyTok).ToolNames(context.Background())
	if strings.Join(oauthTools, ",") != strings.Join(keyTools, ",") {
		t.Errorf("tools differ:\n oauth %v\n key   %v", oauthTools, keyTools)
	}
	// The grant is an api_keys row of kind oauth, audited like a key.
	keyID, _, _, _ := h.grantFor(t, h.a.UserID, cid)
	if rows := h.auditRows(t, keyID, "api_key.create"); len(rows) != 1 {
		t.Errorf("api_key.create audit rows = %d", len(rows))
	} else if rows[0]["kind"] != "oauth" || rows[0]["_actor"] != "user" {
		t.Errorf("audit detail = %v", rows[0])
	}
	// Send and delete together need the typed confirmation, and start in confirm mode.
	_, challenge := pkce()
	_, body := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"scope": {"mail:read mail:send mail:delete"}}))
	id := requestID(t, body)
	form := defaultDecision(id, "approve")
	form["scope"] = []string{"mail:read", "mail:send", "mail:delete"}
	form["confirm"] = []string{"mail:send", "mail:delete"}
	if resp, _ := h.decide(t, h.a.UserID, form, nil); resp.StatusCode != 422 {
		t.Errorf("send+delete without the typed confirmation: %d, want 422", resp.StatusCode)
	}
	form["ack"] = []string{"send and delete"}
	if resp, _ := h.decide(t, h.a.UserID, form, nil); resp.StatusCode != 303 {
		t.Fatalf("send+delete with confirmation: %d", resp.StatusCode)
	}
	var confirm []string
	_ = h.super.QueryRow(context.Background(), `SELECT confirm_scopes FROM app.api_keys WHERE id=$1`, keyID).Scan(&confirm)
	if strings.Join(confirm, ",") != "mail:delete,mail:send" {
		t.Errorf("confirm scopes = %v", confirm)
	}
}

// mcpKeyServer serves /mcp for API keys with the plain gate, as the key suites do.
func (h *oaHarness) mcpKeyServer(t *testing.T) *httptest.Server { return h.mcpServer(t, h.a.UserID) }

// ADR-050 section 4: manual clients, and ids that cannot be forged.
func TestOAuthManualClients(t *testing.T) {
	h := newOAHarness(t)
	res := call(t, h.api.oauthClientSet, h.a.UserID, map[string]any{"create": map[string]any{"c": map[string]any{
		"name": "My tool", "redirectUris": []string{oaRedirect}}}})
	c := res["created"].(map[string]any)["c"].(map[string]any)
	id := c["id"].(string)
	if !regexp.MustCompile(`^mc_[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("minted id = %q", id)
	}
	bad := call(t, h.api.oauthClientSet, h.a.UserID, map[string]any{"create": map[string]any{
		"c1": map[string]any{"name": "x", "redirectUris": []string{"com.example.app:/cb"}},
		"c2": map[string]any{"name": "x", "redirectUris": []string{"http://example.com/cb"}},
		"c3": map[string]any{"name": "x", "redirectUris": []string{}}}})
	if len(bad["notCreated"].(map[string]any)) != 3 {
		t.Errorf("bad redirect URIs accepted: %v", bad)
	}
	// The owner can use it; another user cannot; it lists for its owner only.
	_, challenge := pkce()
	if resp, body := h.authorize(t, h.a.UserID, h.authorizeURL(id, challenge, nil)); resp.StatusCode != 200 || !strings.Contains(body, "registered yourself") {
		t.Errorf("owner: %d", resp.StatusCode)
	}
	if resp, _ := h.authorize(t, h.b.UserID, h.authorizeURL(id, challenge, nil)); resp.StatusCode != 400 {
		t.Errorf("another user's manual client: %d, want 400", resp.StatusCode)
	}
	if b, _ := json.Marshal(call(t, h.api.oauthClientGet, h.b.UserID, map[string]any{})); strings.Contains(string(b), id) {
		t.Errorf("B lists A's client: %s", b)
	}
	// An id shaped as a metadata-document URL is never a registered client: the
	// table refuses to store one, whoever writes it, and nothing resolves one.
	_, err := h.super.Exec(context.Background(), `INSERT INTO app.oauth_clients (id, kind, user_id, name, redirect_uris)
		VALUES ($1,'manual',$2,'squat',ARRAY['https://x.example.com/cb'])`, h.cimdURL(), h.a.UserID.String())
	if err == nil {
		t.Error("a URL-shaped client id was stored")
	}
	if _, found, _ := h.store.ResolveOAuthClient(context.Background(), h.cimdURL()); found {
		t.Error("a URL resolved as a registered client")
	}
	// Destroy.
	d := call(t, h.api.oauthClientSet, h.a.UserID, map[string]any{"destroy": []string{id}})
	if len(asArr(d["destroyed"])) != 1 {
		t.Errorf("destroy: %v", d)
	}
}

// ADR-050 section 4, CIMD fetch.
func TestOAuthCIMDFetch(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	_, challenge := pkce()
	status := func() int {
		// A fresh cache each time: the cache is exactly what hides a refusal.
		_, _ = h.super.Exec(context.Background(), `DELETE FROM app.oauth_cimd_cache`)
		resp, _ := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil))
		return resp.StatusCode
	}
	if s := status(); s != 200 {
		t.Fatalf("a valid document: %d", s)
	}
	var cached int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.oauth_cimd_cache WHERE client_id=$1`, cid).Scan(&cached)
	if cached != 1 {
		t.Errorf("document not cached (%d)", cached)
	}
	big := strings.Repeat("a", 17<<10)
	for name, fn := range map[string]http.HandlerFunc{
		"client_id mismatch":  h.cimdDoc(cid, map[string]any{"client_id": cid + "2"}),
		"no redirect_uris":    h.cimdDoc(cid, map[string]any{"redirect_uris": nil}),
		"no client_name":      h.cimdDoc(cid, map[string]any{"client_name": nil}),
		"bad redirect scheme": h.cimdDoc(cid, map[string]any{"redirect_uris": []string{"com.example.app:/cb"}}),
		"confidential method": h.cimdDoc(cid, map[string]any{"token_endpoint_auth_method": "client_secret_basic"}),
		"oversize":            h.cimdDoc(cid, map[string]any{"client_name": big}),
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere.json", http.StatusFound)
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>") },
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		},
		"server error": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) },
	} {
		h.setCIMD(fn)
		if s := status(); s != 400 {
			t.Errorf("%s: %d, want 400 on Mailroom's page (never a redirect)", name, s)
		}
	}
	// Not an https URL with a path: refused without any fetch.
	h.setCIMD(func(http.ResponseWriter, *http.Request) { t.Error("fetched an unacceptable client_id") })
	for _, bad := range []string{"http://127.0.0.1/c.json", h.cimd.URL, h.cimd.URL + "/", h.cimd.URL + "/a/../b", h.cimd.URL + "/c#frag"} {
		resp, _ := h.authorize(t, h.a.UserID, h.authorizeURL(bad, challenge, nil))
		if resp.StatusCode != 400 {
			t.Errorf("client_id %q: %d", bad, resp.StatusCode)
		}
	}
	// Cache lifetime is clamped: 1 s max-age becomes 5 minutes.
	h.setCIMD(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=1")
		h.cimdDoc(cid, nil)(w, r)
	})
	if s := status(); s != 200 {
		t.Fatal(s)
	}
	var ttl float64
	_ = h.super.QueryRow(context.Background(), `SELECT extract(epoch FROM expires_at - fetched_at) FROM app.oauth_cimd_cache WHERE client_id=$1`, cid).Scan(&ttl)
	if ttl < 290 || ttl > 310 {
		t.Errorf("cache ttl for max-age=1 = %.0fs, want about 300", ttl)
	}
}

// The production egress client refuses a private address at dial time and
// opens NO connection; the opt-in lets the connection through.
func TestOAuthCIMDEgressGuard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	accepted := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			_ = c.Close()
		}
	}()
	count := func() int { mu.Lock(); defer mu.Unlock(); return accepted }
	fetch := func(allow bool) error {
		c := oauthserver.NewCIMDClient(allow)
		req, _ := http.NewRequest(http.MethodGet, "https://"+ln.Addr().String()+"/c.json", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err := c.Do(req.WithContext(ctx))
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := fetch(false); err == nil {
		t.Error("loopback fetch succeeded under the default policy")
	}
	time.Sleep(100 * time.Millisecond)
	if n := count(); n != 0 {
		t.Errorf("the guard let %d connection(s) through to a loopback address", n)
	}
	_ = fetch(true) // the TLS handshake fails; the point is that a connection was made
	time.Sleep(100 * time.Millisecond)
	if n := count(); n == 0 {
		t.Error("MAILROOM_OAUTH_ALLOW_PRIVATE_CLIENT_HOSTS did not lift the guard")
	}
}

// Failed token requests are charged to the shared limiter; successes never are.
func TestOAuthTokenFailureLimiter(t *testing.T) {
	h := newOAHarness(t)
	lim := auth.NewFailureLimiter(3, time.Hour)
	h.os.UseLimiter(lim)
	cid := h.cimdURL()
	for i := 0; i < 5; i++ {
		code, verifier := h.grantCode(t, h.a.UserID, cid)
		if st, out := h.exchange(t, code, verifier, cid); st != 200 {
			t.Fatalf("success #%d refused: %d %v", i, st, out)
		}
	}
	var last int
	for i := 0; i < 6; i++ {
		last, _ = h.exchange(t, "mrcode_bogus", "x", cid)
	}
	if last != 429 {
		t.Errorf("after repeated failures: %d, want 429", last)
	}
}

// The authorization endpoint's limiter is charged on failure only: any number of
// SUCCESSFUL authorizations from one caller (and one address) are never refused
// (ADR-042 section 2, amended).
func TestOAuthAuthorizeNotChargedOnSuccess(t *testing.T) {
	h := newOAHarness(t)
	cid := h.cimdURL()
	_, challenge := pkce()
	for i := 0; i < 80; i++ { // far past the limiter's burst of 30
		if resp, _ := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil)); resp.StatusCode != 200 {
			t.Fatalf("successful authorization #%d refused: %d", i, resp.StatusCode)
		}
	}
	// Failures are charged, per user: user A's failures do not touch user B.
	for i := 0; i < 40; i++ {
		h.authorize(t, h.a.UserID, h.authorizeURL("nonsense", challenge, nil))
	}
	if resp, _ := h.authorize(t, h.a.UserID, h.authorizeURL("nonsense", challenge, nil)); resp.StatusCode != 429 {
		t.Errorf("repeated failures were not slowed: %d", resp.StatusCode)
	}
	if resp, _ := h.authorize(t, h.b.UserID, h.authorizeURL(cid, challenge, nil)); resp.StatusCode != 200 {
		t.Errorf("another user was refused because of A's failures: %d", resp.StatusCode)
	}
}
