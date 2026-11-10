// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/mcp"
	"mailroom/internal/oauthserver"
	"mailroom/internal/store"
)

// DYNAMIC CLIENT REGISTRATION (ADR-050 section 4, increment 3), end to end over
// the composed stack: the unauthenticated endpoint, what it refuses, what it
// stores, its limits, its cleanup, and a registered client completing the flow.

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newDCRHarness(t *testing.T, tweak func(*oauthserver.Config)) (*oaHarness, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Now()}
	h := newOAHarnessWith(t, func(c *oauthserver.Config) {
		c.DCR = true
		c.Now = clock.Now
		if tweak != nil {
			tweak(c)
		}
	})
	h.cleanRegistered(t)
	return h, clock
}

// cleanRegistered removes every registered client: they belong to no user, so
// other tests' leftovers would otherwise count against this one's caps and totals.
func (h *oaHarness) cleanRegistered(t *testing.T) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), `DELETE FROM app.oauth_clients WHERE kind='registered'`); err != nil {
		t.Fatal(err)
	}
}

var rcIDRe = regexp.MustCompile(`^rc_[0-9a-f]{32}$`)

// register posts raw to /oauth/register as JSON and returns the answer.
func (h *oaHarness) registerRaw(t *testing.T, contentType, body string) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/oauth/register", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, text := h.do(t, req)
	var out map[string]any
	_ = json.Unmarshal([]byte(text), &out)
	return resp.StatusCode, out, resp.Header
}

func (h *oaHarness) register(t *testing.T, doc map[string]any) (int, map[string]any, http.Header) {
	t.Helper()
	b, _ := json.Marshal(doc)
	return h.registerRaw(t, "application/json", string(b))
}

func goodRegistration(over map[string]any) map[string]any {
	doc := map[string]any{"client_name": "Example Assistant", "redirect_uris": []string{oaRedirect}}
	for k, v := range over {
		if v == nil {
			delete(doc, k)
		} else {
			doc[k] = v
		}
	}
	return doc
}

func (h *oaHarness) registeredCount(t *testing.T) (n int) {
	t.Helper()
	if err := h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.oauth_clients WHERE kind='registered'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDCRRegistersAStrangersClientAndStoresOnlyNameAndRedirects(t *testing.T) {
	h, _ := newDCRHarness(t, nil)

	// The metadata advertises the endpoint.
	_, body := h.do(t, mustReq(http.MethodGet, h.srv.URL+"/.well-known/oauth-authorization-server"))
	var meta map[string]any
	_ = json.Unmarshal([]byte(body), &meta)
	if meta["registration_endpoint"] != h.issuer+"/oauth/register" {
		t.Errorf("registration_endpoint = %v", meta["registration_endpoint"])
	}

	// Everything beyond name and redirect URIs is ignored: a logo or link a
	// stranger supplies must never reach the consent page.
	const marker = "https://evil.example.net/phish-marker"
	st, out, hd := h.register(t, goodRegistration(map[string]any{
		"client_uri": marker + "/home", "logo_uri": marker + "/logo.png", "tos_uri": marker + "/tos",
		"policy_uri": marker + "/policy", "contacts": []string{"x@example.net"}, "scope": "mail:delete mail:send",
		"software_id": "sw-1", "jwks_uri": marker + "/jwks", "unknown_field": map[string]any{"a": 1},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none"}))
	if st != http.StatusCreated {
		t.Fatalf("register: %d %v", st, out)
	}
	cid, _ := out["client_id"].(string)
	if !rcIDRe.MatchString(cid) {
		t.Fatalf("client_id = %q, want rc_ + 32 hex", cid)
	}
	issued, _ := out["client_id_issued_at"].(float64)
	if issued < float64(time.Now().Add(-time.Minute).Unix()) || issued > float64(time.Now().Add(time.Minute).Unix()) {
		t.Errorf("client_id_issued_at = %v", out["client_id_issued_at"])
	}
	for k, want := range map[string]string{"client_name": "Example Assistant", "token_endpoint_auth_method": "none",
		"redirect_uris": `["` + oaRedirect + `"]`, "grant_types": `["authorization_code","refresh_token"]`,
		"response_types": `["code"]`} {
		got := out[k]
		if s, ok := got.(string); ok {
			if s != want {
				t.Errorf("%s = %q, want %q", k, s, want)
			}
		} else if jsonOf(got) != want {
			t.Errorf("%s = %s, want %s", k, jsonOf(got), want)
		}
	}
	for _, k := range []string{"client_secret", "client_secret_expires_at", "registration_access_token",
		"registration_client_uri", "client_uri", "logo_uri", "tos_uri", "policy_uri", "scope", "contacts"} {
		if _, has := out[k]; has {
			t.Errorf("the response carries %s", k)
		}
	}
	if strings.Contains(jsonOf(out), "phish-marker") {
		t.Errorf("the response repeats a supplied link: %v", out)
	}
	if hd.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", hd.Get("Cache-Control"))
	}
	// Stored: a name and redirect URIs, owned by nobody.
	var kind, name string
	var uris []string
	var owner *string
	if err := h.super.QueryRow(context.Background(),
		`SELECT kind, name, redirect_uris, user_id FROM app.oauth_clients WHERE id=$1`, cid).Scan(&kind, &name, &uris, &owner); err != nil {
		t.Fatal(err)
	}
	if kind != "registered" || name != "Example Assistant" || strings.Join(uris, ",") != oaRedirect || owner != nil {
		t.Errorf("stored row = %s %q %v owner=%v", kind, name, uris, owner)
	}
	var wholeRow string
	_ = h.super.QueryRow(context.Background(), `SELECT c::text FROM app.oauth_clients c WHERE id=$1`, cid).Scan(&wholeRow)
	if strings.Contains(wholeRow, "phish-marker") || strings.Contains(wholeRow, "mail:delete") || strings.Contains(wholeRow, "sw-1") {
		t.Errorf("the stored row holds ignored metadata: %s", wholeRow)
	}

	// The consent page labels it as given by the app, shows the name, and shows
	// nothing the stranger supplied beyond that.
	_, challenge := pkce()
	resp, page := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: %d %.200s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "Example Assistant") || !strings.Contains(page, "Name given by the app, not verified.") {
		t.Errorf("the consent page does not label a registered client's name as unverified: %.600s", page)
	}
	if strings.Contains(page, "phish-marker") || strings.Contains(page, "evil.example.net") {
		t.Errorf("the consent page renders something the registration supplied beyond its name")
	}
	if strings.Contains(page, "registered yourself") {
		t.Error("a registered client is described as the user's own")
	}
}

// A registered client completes the whole journey.
func TestDCRRegisteredClientCompletesTheFlow(t *testing.T) {
	h, _ := newDCRHarness(t, nil)
	st, out, _ := h.register(t, goodRegistration(nil))
	if st != http.StatusCreated {
		t.Fatalf("register: %d %v", st, out)
	}
	cid := out["client_id"].(string)
	code, verifier := h.grantCode(t, h.a.UserID, cid)
	if st, out := h.exchange(t, code, verifier, cid); st != 200 {
		t.Fatalf("token: %d %v", st, out)
	} else {
		sess := h.mcp(t, out["access_token"].(string))
		if names, err := sess.ToolNames(context.Background()); err != nil || len(names) == 0 {
			t.Fatalf("tools/list with a registered client's token: %v %v", names, err)
		}
		if res := mcpCall(t, sess, "list_mailboxes", map[string]any{}); res.IsError {
			t.Fatalf("list_mailboxes: %s", res.Text)
		}
	}
	var ckind, cname string
	if err := h.super.QueryRow(context.Background(), `SELECT oauth_client_kind, oauth_client_name FROM app.api_keys
		WHERE user_id=$1 AND kind='oauth' AND oauth_client_id=$2`, h.a.UserID.String(), cid).Scan(&ckind, &cname); err != nil {
		t.Fatal(err)
	}
	if ckind != "registered" || cname != "Example Assistant" {
		t.Errorf("grant client = %s %q", ckind, cname)
	}
	// Its redirect URIs are enforced like any client's.
	_, challenge := pkce()
	if resp, _ := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, url.Values{"redirect_uri": {"https://other.example.com/cb"}})); resp.StatusCode != 400 {
		t.Errorf("an unregistered redirect URI for a registered client: %d, want 400", resp.StatusCode)
	}
}

func TestDCRRefusals(t *testing.T) {
	h, clock := newDCRHarness(t, nil)
	longURI := "https://app.example.com/" + strings.Repeat("a", 2048-len("https://app.example.com/"))
	cases := []struct {
		name   string
		doc    map[string]any
		status int
		code   string
	}{
		{"client secret auth", goodRegistration(map[string]any{"token_endpoint_auth_method": "client_secret_basic"}), 400, "invalid_client_metadata"},
		{"private_key_jwt", goodRegistration(map[string]any{"token_endpoint_auth_method": "private_key_jwt"}), 400, "invalid_client_metadata"},
		{"auth method of the wrong type", goodRegistration(map[string]any{"token_endpoint_auth_method": 7}), 400, "invalid_client_metadata"},
		{"password grant", goodRegistration(map[string]any{"grant_types": []string{"password"}}), 400, "invalid_client_metadata"},
		{"implicit grant", goodRegistration(map[string]any{"grant_types": []string{"implicit"}}), 400, "invalid_client_metadata"},
		{"client credentials among others", goodRegistration(map[string]any{"grant_types": []string{"authorization_code", "client_credentials"}}), 400, "invalid_client_metadata"},
		{"token response type", goodRegistration(map[string]any{"response_types": []string{"token"}}), 400, "invalid_client_metadata"},
		{"id_token among code", goodRegistration(map[string]any{"response_types": []string{"code", "id_token"}}), 400, "invalid_client_metadata"},
		{"no redirect uris", goodRegistration(map[string]any{"redirect_uris": nil}), 400, "invalid_redirect_uri"},
		{"empty redirect uris", goodRegistration(map[string]any{"redirect_uris": []string{}}), 400, "invalid_redirect_uri"},
		{"eleven redirect uris", goodRegistration(map[string]any{"redirect_uris": func() []string {
			var u []string
			for i := 0; i < 11; i++ {
				u = append(u, "https://app.example.com/cb"+string(rune('a'+i)))
			}
			return u
		}()}), 400, "invalid_redirect_uri"},
		{"http to a public host", goodRegistration(map[string]any{"redirect_uris": []string{"http://app.example.com/cb"}}), 400, "invalid_redirect_uri"},
		{"private-use scheme", goodRegistration(map[string]any{"redirect_uris": []string{"com.example.app:/cb"}}), 400, "invalid_redirect_uri"},
		{"fragment", goodRegistration(map[string]any{"redirect_uris": []string{"https://app.example.com/cb#x"}}), 400, "invalid_redirect_uri"},
		{"one good, one bad", goodRegistration(map[string]any{"redirect_uris": []string{oaRedirect, "javascript:alert(1)"}}), 400, "invalid_redirect_uri"},
		{"uri over 2048 bytes", goodRegistration(map[string]any{"redirect_uris": []string{longURI + "a"}}), 400, "invalid_redirect_uri"},
		{"redirect uris of the wrong type", goodRegistration(map[string]any{"redirect_uris": "https://app.example.com/cb"}), 400, "invalid_client_metadata"},
		{"no name", goodRegistration(map[string]any{"client_name": nil}), 400, "invalid_client_metadata"},
		{"empty name", goodRegistration(map[string]any{"client_name": ""}), 400, "invalid_client_metadata"},
		{"blank name", goodRegistration(map[string]any{"client_name": "   \t "}), 400, "invalid_client_metadata"},
		{"name of invisible characters only", goodRegistration(map[string]any{"client_name": "\u200b\u202e\u0007\u3164"}), 400, "invalid_client_metadata"},
		{"name over 100 characters", goodRegistration(map[string]any{"client_name": strings.Repeat("a", 101)}), 400, "invalid_client_metadata"},
		{"name over 100 multibyte characters", goodRegistration(map[string]any{"client_name": strings.Repeat("é", 101)}), 400, "invalid_client_metadata"},
		{"name of the wrong type", goodRegistration(map[string]any{"client_name": 5}), 400, "invalid_client_metadata"},
	}
	before := h.registeredCount(t)
	for _, c := range cases {
		clock.Advance(time.Minute) // keep the limiter out of it
		st, out, _ := h.register(t, c.doc)
		if st != c.status || out["error"] != c.code {
			t.Errorf("%s: %d %v, want %d %s", c.name, st, out, c.status, c.code)
		}
	}
	raw := []struct {
		name, ct, body string
		status         int
	}{
		{"body over 16 KiB", "application/json", `{"client_name":"x","redirect_uris":["` + oaRedirect + `"],"pad":"` + strings.Repeat("a", 17<<10) + `"}`, 413},
		{"not JSON", "application/json", `client_name=x`, 400},
		{"empty body", "application/json", ``, 400},
		{"JSON array", "application/json", `[{"client_name":"x"}]`, 400},
		{"form content type", "application/x-www-form-urlencoded", `client_name=x&redirect_uris=` + oaRedirect, 400},
		{"no content type", "", `{"client_name":"x","redirect_uris":["` + oaRedirect + `"]}`, 400},
	}
	for _, c := range raw {
		clock.Advance(time.Minute)
		st, out, _ := h.registerRaw(t, c.ct, c.body)
		if st != c.status || out["error"] != "invalid_client_metadata" {
			t.Errorf("%s: %d %v, want %d invalid_client_metadata", c.name, st, out, c.status)
		}
	}
	// A refusal stores nothing. The control: the same endpoint with a good
	// document does store, so a count of zero is not "the endpoint is broken".
	if got := h.registeredCount(t); got != before {
		t.Errorf("refused registrations left %d rows", got-before)
	}
	clock.Advance(time.Minute)
	if st, out, _ := h.register(t, goodRegistration(nil)); st != http.StatusCreated || h.registeredCount(t) != before+1 {
		t.Fatalf("control registration: %d %v", st, out)
	}

	// The boundaries are inclusive: 100 characters, 10 URIs, a 2048-byte URI.
	uris := []string{longURI}
	for i := 0; i < 9; i++ {
		uris = append(uris, "https://app.example.com/cb"+string(rune('a'+i)))
	}
	clock.Advance(time.Minute)
	if st, out, _ := h.register(t, goodRegistration(map[string]any{"client_name": strings.Repeat("é", 100), "redirect_uris": uris})); st != http.StatusCreated {
		t.Errorf("boundary registration: %d %v", st, out)
	}
	// Only a method absent or "none" is accepted; grant_types omitted defaults.
	clock.Advance(time.Minute)
	st, out, _ := h.register(t, goodRegistration(map[string]any{"token_endpoint_auth_method": "none"}))
	if st != http.StatusCreated || jsonOf(out["grant_types"]) != `["authorization_code"]` || jsonOf(out["response_types"]) != `["code"]` {
		t.Errorf("defaults: %d %v", st, out)
	}
	// A GET is not a registration.
	if resp, _ := h.do(t, mustReq(http.MethodGet, h.srv.URL+"/oauth/register")); resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /oauth/register: %d", resp.StatusCode)
	}
}

func TestDCRNameIsCleanedLikeALearnedContactName(t *testing.T) {
	h, clock := newDCRHarness(t, nil)
	// Control, bidi, format and separator characters are REFUSED now (#148), not
	// cleaned, and nothing is stored for a refused name. (The unit test in
	// internal/oauthserver covers every class; the registration limit allows only
	// a few attempts a minute here, so this keeps to a bidi override and a newline.)
	var before int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.oauth_clients`).Scan(&before)
	for _, bad := range []string{"Evil\u202eApp", "Na\nme"} {
		st, out, _ := h.register(t, goodRegistration(map[string]any{"client_name": bad}))
		if st != http.StatusBadRequest || out["error"] != "invalid_client_metadata" {
			t.Errorf("client_name %q: %d %v, want 400 invalid_client_metadata", bad, st, out)
		}
	}
	var refused int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.oauth_clients`).Scan(&refused)
	if refused != before {
		t.Errorf("%d clients stored after refused names", refused-before)
	}
	clock.Advance(time.Minute)
	// What is left to clean is the invisible characters that are not Cf (a Hangul
	// filler) and surrounding space.
	dirty := "  Evil\u3164App Name  "
	st, out, _ := h.register(t, goodRegistration(map[string]any{"client_name": dirty}))
	if st != http.StatusCreated {
		t.Fatalf("register: %d %v", st, out)
	}
	const want = "EvilApp Name"
	if out["client_name"] != want {
		t.Errorf("response name = %q, want %q", out["client_name"], want)
	}
	cid := out["client_id"].(string)
	var stored string
	_ = h.super.QueryRow(context.Background(), `SELECT name FROM app.oauth_clients WHERE id=$1`, cid).Scan(&stored)
	if stored != want {
		t.Errorf("stored name = %q, want %q", stored, want)
	}
	_, challenge := pkce()
	_, page := h.authorize(t, h.a.UserID, h.authorizeURL(cid, challenge, nil))
	for _, r := range []string{"\u3164", "\u202e", "\u200b", "\u0007", "\u2066"} {
		if strings.Contains(page, r) {
			t.Errorf("the consent page carries U+%04X", []rune(r)[0])
		}
	}
	if !strings.Contains(page, want) {
		t.Errorf("the consent page does not show the cleaned name")
	}
	// Markup in a name is text, never markup.
	clockless, _ := newDCRHarness(t, nil)
	st, out, _ = clockless.register(t, goodRegistration(map[string]any{"client_name": `<img src=x onerror=alert(1)>`}))
	if st != http.StatusCreated {
		t.Fatalf("markup name: %d %v", st, out)
	}
	_, challenge = pkce()
	_, page = clockless.authorize(t, clockless.a.UserID, clockless.authorizeURL(out["client_id"].(string), challenge, nil))
	if strings.Contains(page, "<img src=x") || !strings.Contains(page, "&lt;img") {
		t.Errorf("a client name is not escaped on the consent page")
	}
}

func TestDCROffAndOn(t *testing.T) {
	// Off (the harness default here): no endpoint, no metadata entry.
	h := newOAHarness(t)
	h.cleanRegistered(t)
	st, _, _ := h.register(t, goodRegistration(nil))
	if st != http.StatusNotFound {
		t.Errorf("POST /oauth/register with DCR off: %d, want 404", st)
	}
	if n := h.registeredCount(t); n != 0 {
		t.Errorf("a 404 stored %d rows", n)
	}
	_, body := h.do(t, mustReq(http.MethodGet, h.srv.URL+"/.well-known/oauth-authorization-server"))
	if strings.Contains(body, "registration_endpoint") {
		t.Errorf("metadata advertises registration with DCR off: %s", body)
	}
	// The control: with it on, the same request registers.
	on, _ := newDCRHarness(t, nil)
	if st, out, _ := on.register(t, goodRegistration(nil)); st != http.StatusCreated {
		t.Errorf("control: %d %v", st, out)
	}
}

func TestDCRPerAddressLimits(t *testing.T) {
	h, clock := newDCRHarness(t, nil)
	// Burst: five at one instant, then refused, with a wait stated.
	for i := 0; i < 5; i++ {
		if st, out, _ := h.register(t, goodRegistration(nil)); st != http.StatusCreated {
			t.Fatalf("burst %d: %d %v", i, st, out)
		}
	}
	st, out, hd := h.register(t, goodRegistration(nil))
	if st != http.StatusTooManyRequests || out["error"] != "slow_down" || hd.Get("Retry-After") == "" {
		t.Errorf("sixth registration in the same instant: %d %v retry-after %q", st, out, hd.Get("Retry-After"))
	}
	if n := h.registeredCount(t); n != 5 {
		t.Errorf("rows = %d after a refused sixth, want 5", n)
	}
	// An INVALID request costs the burst too: a flood of junk is not free.
	clock.Advance(time.Hour) // everything refilled; the daily window still holds 5
	for i := 0; i < 5; i++ {
		h.registerRaw(t, "application/json", `{`)
	}
	if st, _, _ := h.register(t, goodRegistration(nil)); st != http.StatusTooManyRequests {
		t.Errorf("a valid registration right after five invalid ones: %d, want 429", st)
	}
	clock.Advance(registrationRefillForTest)
	if st, out, _ := h.register(t, goodRegistration(nil)); st != http.StatusCreated {
		t.Errorf("after the burst refilled: %d %v", st, out)
	}

	// Daily: 50 in 24 hours from one address, however politely spaced.
	total := h.registeredCount(t)
	for total < oauthserver.RegistrationsPerAddressPerDay {
		clock.Advance(registrationRefillForTest)
		if st, out, _ := h.register(t, goodRegistration(nil)); st != http.StatusCreated {
			t.Fatalf("registration %d: %d %v", total+1, st, out)
		}
		total++
	}
	clock.Advance(registrationRefillForTest)
	st, out, hd = h.register(t, goodRegistration(nil))
	if st != http.StatusTooManyRequests || out["error"] != "slow_down" {
		t.Fatalf("the %dth registration in a day: %d %v", total+1, st, out)
	}
	if hd.Get("Retry-After") == "" {
		t.Error("no Retry-After on the daily limit")
	}
	if n := h.registeredCount(t); n != oauthserver.RegistrationsPerAddressPerDay {
		t.Errorf("rows = %d, want exactly %d", n, oauthserver.RegistrationsPerAddressPerDay)
	}
	// The window rolls: a day after the first, the address may register again.
	clock.Advance(24 * time.Hour)
	if st, out, _ := h.register(t, goodRegistration(nil)); st != http.StatusCreated {
		t.Errorf("a day later: %d %v", st, out)
	}
}

// registrationRefillForTest is a little over one burst-bucket token.
const registrationRefillForTest = 11 * time.Second

func TestDCRGlobalCapRefusesAndDoesNotEvict(t *testing.T) {
	h, clock := newDCRHarness(t, func(c *oauthserver.Config) { c.MaxUnusedRegistrations = 3 })
	var ids []string
	for i := 0; i < 3; i++ {
		clock.Advance(registrationRefillForTest)
		st, out, _ := h.register(t, goodRegistration(nil))
		if st != http.StatusCreated {
			t.Fatalf("registration %d: %d %v", i, st, out)
		}
		ids = append(ids, out["client_id"].(string))
	}
	clock.Advance(registrationRefillForTest)
	st, out, hd := h.register(t, goodRegistration(nil))
	if secs, _ := strconv.Atoi(hd.Get("Retry-After")); st != http.StatusServiceUnavailable || out["error"] != "server_error" || secs < 23*3600 {
		t.Fatalf("registration past the cap: %d %v", st, out)
	}
	if n := h.registeredCount(t); n != 3 {
		t.Errorf("rows = %d, want 3 (refused, not evicting)", n)
	}
	for _, id := range ids {
		if _, found, _ := h.store.ResolveOAuthClient(context.Background(), id); !found {
			t.Errorf("%s was evicted to make room", id)
		}
	}
	// A client a live grant uses is not "unused": consenting through one frees
	// a place.
	code, verifier := h.grantCode(t, h.a.UserID, ids[0])
	if st, out := h.exchange(t, code, verifier, ids[0]); st != 200 {
		t.Fatalf("token: %d %v", st, out)
	}
	clock.Advance(registrationRefillForTest)
	if st, out, _ := h.register(t, goodRegistration(nil)); st != http.StatusCreated {
		t.Errorf("after one was used: %d %v", st, out)
	}
	if n := h.registeredCount(t); n != 4 {
		t.Errorf("rows = %d, want 4", n)
	}
}

func TestDCRUnusedRegistrationsExpireAndUsedOnesAreKept(t *testing.T) {
	h, clock := newDCRHarness(t, nil)
	register := func() string {
		clock.Advance(registrationRefillForTest)
		st, out, _ := h.register(t, goodRegistration(nil))
		if st != http.StatusCreated {
			t.Fatalf("register: %d %v", st, out)
		}
		return out["client_id"].(string)
	}
	unused, used := register(), register()
	code, verifier := h.grantCode(t, h.a.UserID, used)
	if st, out := h.exchange(t, code, verifier, used); st != 200 {
		t.Fatalf("token: %d %v", st, out)
	}
	exists := func(id string) bool {
		var n int
		_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.oauth_clients WHERE id=$1`, id).Scan(&n)
		return n == 1
	}
	sweep := func(after time.Duration) map[string]int64 {
		t.Helper()
		out, err := h.store.SweepOAuth(context.Background(), time.Now().Add(after), store.OAuthSweepGrace)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Inside the day nothing goes.
	if c := sweep(23 * time.Hour); c["clients"] != 0 || !exists(unused) || !exists(used) {
		t.Fatalf("a sweep 23h after: clients=%d unused=%v used=%v", c["clients"], exists(unused), exists(used))
	}
	// Past it, the one no consent completed is deleted and the used one stays.
	if c := sweep(25 * time.Hour); c["clients"] != 1 || exists(unused) || !exists(used) {
		t.Fatalf("a sweep 25h after: clients=%d unused exists=%v used exists=%v", c["clients"], exists(unused), exists(used))
	}
	if _, found, _ := h.store.ResolveOAuthClient(context.Background(), unused); found {
		t.Error("an expired registration still resolves")
	}
	// A kept client outlives any number of sweeps while its grant lives...
	if c := sweep(400 * 24 * time.Hour); c["clients"] != 0 || !exists(used) {
		t.Fatalf("a sweep a year on: clients=%d used exists=%v", c["clients"], exists(used))
	}
	// ...and goes with the next sweep once the grant is revoked.
	if _, err := h.super.Exec(context.Background(), `UPDATE app.api_keys SET revoked_at=now() WHERE kind='oauth' AND oauth_client_id=$1`, used); err != nil {
		t.Fatal(err)
	}
	if c := sweep(25 * time.Hour); c["clients"] != 1 || exists(used) {
		t.Fatalf("a sweep after revoking its grant: clients=%d used exists=%v", c["clients"], exists(used))
	}
	// A suspended (not revoked) grant still holds its client.
	again := register()
	code, verifier = h.grantCode(t, h.b.UserID, again)
	if st, out := h.exchange(t, code, verifier, again); st != 200 {
		t.Fatalf("token: %d %v", st, out)
	}
	if _, err := h.super.Exec(context.Background(), `UPDATE app.api_keys SET suspended_at=now() WHERE kind='oauth' AND oauth_client_id=$1`, again); err != nil {
		t.Fatal(err)
	}
	if c := sweep(25 * time.Hour); c["clients"] != 0 || !exists(again) {
		t.Fatalf("a suspended grant lost its client: clients=%d exists=%v", c["clients"], exists(again))
	}
	// Manual and metadata-document clients are never touched by this sweep.
	manual := call(t, h.api.oauthClientSet, h.a.UserID, map[string]any{"create": map[string]any{"c": map[string]any{
		"name": "My tool", "redirectUris": []string{oaRedirect}}}})["created"].(map[string]any)["c"].(map[string]any)["id"].(string)
	if sweep(400 * 24 * time.Hour); !exists(manual) {
		t.Error("the sweep deleted a manual client")
	}
}

func TestDCRRegisteredClientsAreNotReachableThroughOAuthClientMethods(t *testing.T) {
	h, _ := newDCRHarness(t, nil)
	st, out, _ := h.register(t, goodRegistration(nil))
	if st != http.StatusCreated {
		t.Fatalf("register: %d %v", st, out)
	}
	cid := out["client_id"].(string)
	for _, user := range []struct {
		name string
		id   ulid.ULID
	}{{"A", h.a.UserID}, {"B", h.b.UserID}} {
		got, _ := json.Marshal(call(t, h.api.oauthClientGet, user.id, map[string]any{}))
		if strings.Contains(string(got), cid) {
			t.Errorf("user %s lists a registered client: %s", user.name, got)
		}
		d := call(t, h.api.oauthClientSet, user.id, map[string]any{"destroy": []string{cid}})
		if len(asArr(d["destroyed"])) != 0 {
			t.Errorf("user %s destroyed a registered client: %v", user.name, d)
		}
		if b, _ := json.Marshal(d["notDestroyed"]); !strings.Contains(string(b), cid) {
			t.Errorf("user %s: destroying a registered client was not reported as not destroyed: %s", user.name, b)
		}
	}
	var n int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.oauth_clients WHERE id=$1`, cid).Scan(&n)
	if n != 1 {
		t.Error("the registered client was deleted through OAuthClient/set")
	}
	// The control: a manual client IS listed to its owner, so the empty answer
	// above is not "the method lists nothing".
	manual := call(t, h.api.oauthClientSet, h.a.UserID, map[string]any{"create": map[string]any{"c": map[string]any{
		"name": "My tool", "redirectUris": []string{oaRedirect}}}})["created"].(map[string]any)["c"].(map[string]any)["id"].(string)
	if got, _ := json.Marshal(call(t, h.api.oauthClientGet, h.a.UserID, map[string]any{})); !strings.Contains(string(got), manual) {
		t.Errorf("control: the owner does not list a manual client: %s", got)
	}
}

// registerFrom posts a registration from a given loopback source address, so a
// test can be "many addresses" against one server.
func (h *oaHarness) registerFrom(t *testing.T, ip string, doc map[string]any) (int, map[string]any, http.Header) {
	t.Helper()
	cl := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}).DialContext}}
	defer cl.CloseIdleConnections()
	b, _ := json.Marshal(doc)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/oauth/register", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("register from %s: %v", ip, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

// However many addresses ask, the process registers at most 30 a minute.
func TestDCRProcessWideRateHoldsAcrossAddresses(t *testing.T) {
	h, clock := newDCRHarness(t, nil)
	ok, refused := 0, 0
	for i := 0; i < 31; i++ {
		st, out, _ := h.registerFrom(t, "127.0.0."+strconv.Itoa(10+i), goodRegistration(nil))
		switch {
		case st == http.StatusCreated:
			ok++
		case st == http.StatusTooManyRequests && out["error"] == "slow_down":
			refused++
		default:
			t.Fatalf("address %d: %d %v", i, st, out)
		}
	}
	if ok != 30 || refused != 1 || h.registeredCount(t) != 30 {
		t.Errorf("31 addresses in one instant: %d registered, %d refused, %d rows; want 30, 1, 30", ok, refused, h.registeredCount(t))
	}
	clock.Advance(3 * time.Second)
	if st, out, _ := h.registerFrom(t, "127.0.0.60", goodRegistration(nil)); st != http.StatusCreated {
		t.Errorf("after the rate refilled: %d %v", st, out)
	}
}

// A flood of concurrent registrations queues at the server, not in the
// database: at most two are inside the store at once (each would hold one of the
// shared pool's connections while it waits on the cap's lock).
func TestDCRConcurrentRegistrationsDoNotHoldThePool(t *testing.T) {
	h, _ := newDCRHarness(t, nil)
	ctx := context.Background()
	lock, err := h.super.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	unlock := func() {
		if locked {
			_, _ = lock.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('oauth_client_register', 0))`)
			lock.Release()
			locked = false
		}
	}
	defer unlock()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('oauth_client_register', 0))`); err != nil {
		t.Fatal(err)
	}

	type result struct{ status int }
	results := make(chan result, 5)
	for i := 0; i < 5; i++ {
		go func() {
			b, _ := json.Marshal(goodRegistration(nil))
			req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/oauth/register", strings.NewReader(string(b)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{-1}
				return
			}
			_ = resp.Body.Close()
			results <- result{resp.StatusCode}
		}()
	}
	// While the lock is held: exactly the three over the limit come back (429), and
	// no more than two sessions wait on the lock.
	refused := 0
	for i := 0; i < 3; i++ {
		select {
		case r := <-results:
			if r.status != http.StatusTooManyRequests {
				t.Errorf("a registration beyond the concurrency limit answered %d, want 429", r.status)
			}
			refused++
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of the 3 surplus registrations were refused while the lock was held", refused)
		}
	}
	var waiting int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&waiting)
	if waiting != 2 {
		t.Errorf("%d sessions wait on the registration lock, want exactly 2 (the control: the lock is held, and only two got in)", waiting)
	}
	unlock()
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.status != http.StatusCreated {
				t.Errorf("a queued registration answered %d after the lock was released, want 201", r.status)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the two queued registrations never finished")
		}
	}
}

// The gate's own finding is part of the challenge: a scope computed from the
// call's contents (an update into Trash needs mail:delete), which the tool's
// declared uses do not name.
func TestOAuthStepUpIncludesWhatTheGateFoundMissing(t *testing.T) {
	h := newOAHarness(t)
	ctx := context.Background()
	tok, keyID := h.newKey(t, h.a.UserID, map[string]any{"name": "grant-like", "scopes": []string{"mail:read"}})
	var key store.APIKey
	if err := h.store.UserTx(ctx, h.a.UserID, func(tx *store.UserTx) error {
		var e error
		key, _, e = tx.GetAPIKey(ctx, ulid.MustParse(keyID))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	p := newPrincipal(key, "")
	// A context as the bearer gate builds it: the user and the key.
	var uctx context.Context
	gate := &auth.BearerGate{Resolve: h.store.ResolveAPIKeyToken, Limiter: auth.NewFailureLimiter(100, time.Second)}
	breq := httptest.NewRequest(http.MethodGet, "/", nil)
	breq.Header.Set("Authorization", "Bearer "+tok)
	gate.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { uctx = r.Context() }), http.NotFoundHandler()).
		ServeHTTP(httptest.NewRecorder(), breq)
	if uctx == nil {
		t.Fatal("the bearer gate did not admit the key")
	}
	rctx := withPrincipal(uctx, p)
	args, _ := json.Marshal(map[string]any{"update": map[string]any{
		h.a.MessageID.String(): map[string]any{"mailboxIds/" + h.a.TrashID.String(): true}}})
	ran := false
	_, mErr := h.api.runKeyCall(rctx, h.a.UserID, p, "Email/set", args, func(context.Context, ulid.ULID, json.RawMessage) (json.RawMessage, *MethodError) {
		ran = true
		return nil, nil
	})
	if mErr == nil || ran || !p.scopeRefused.Load() {
		t.Fatalf("the gate did not refuse the trash patch for scope: %v ran=%v", mErr, ran)
	}
	if got := strings.Join(p.missingScopes(), " "); !strings.Contains(got, "mail:delete") {
		t.Fatalf("the gate recorded %q, want mail:delete among it", got)
	}
	// move_to_mailbox declares mail:move and mail:read; the challenge adds mail:delete.
	declared, _ := mcpBridge{h.api}.usesNeed(rctx, mcpUsesOf(t, "move_to_mailbox")...)
	if strings.Contains(strings.Join(declared, " "), "mail:delete") {
		t.Fatalf("the control is void: move_to_mailbox already declares mail:delete (%v)", declared)
	}
	got := h.api.stepUpScopes(rctx, p, "move_to_mailbox")
	if want := "mail:delete mail:move mail:read"; strings.Join(got, " ") != want {
		t.Errorf("step-up scopes = %v, want %q (declared %v plus the gate's finding)", got, want, declared)
	}
}

func mcpUsesOf(t *testing.T, tool string) []mcp.Use {
	t.Helper()
	for _, c := range mcp.Catalog() {
		if c.Name == tool {
			return c.Uses
		}
	}
	t.Fatalf("no tool %s", tool)
	return nil
}
