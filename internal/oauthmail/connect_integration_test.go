// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package oauthmail

// The connect flow end to end over the REAL store (RLS, the sealed
// credentials, the change_log), against a fake provider (authorize, token,
// revoke) served by httptest: happy paths for both providers, every refusal
// ADR-046 §3 names, re-authentication, disconnect and reconnect. Every test
// runs with a leak check over the connector's log, every redirect Location and
// every error: no token, code, state or verifier may appear in any of them.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/xoauth2"
)

// --- the fake provider ----------------------------------------------------------

type fakeWho struct {
	sub, email string
	scope      string // granted scope ("" = everything requested)
	tid        string // Microsoft tenant
}

type fakeCode struct {
	provider, challenge, nonce, redirect string
	who                                  fakeWho
	used                                 bool
	refresh                              string
}

type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	mu  stdsync.Mutex
	n   int
	// codes by code value
	codes      map[string]*fakeCode
	revoked    []string
	refreshes  int
	verifiers  []string
	omitIDTok  bool
	omitRefTok bool
	// sentinels issued (codes, tokens, verifiers, states) for the leak check
	issued []string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{t: t, codes: map[string]*fakeCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PostForm.Get("token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) remember(s string) {
	f.mu.Lock()
	f.issued = append(f.issued, s)
	f.mu.Unlock()
}

// authorize plays the user consenting at the provider: it reads the
// authorization URL Start returned and mints a code bound to its challenge,
// nonce and redirect URI. It returns the code and the state to call back with.
func (f *fakeIdP) authorize(t *testing.T, provider, authURL string, who fakeWho) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("authorization URL lacks PKCE S256: %v", q)
	}
	f.mu.Lock()
	f.n++
	code = fmt.Sprintf("4/0CODE-sentinel-%d-%s", f.n, provider)
	f.codes[code] = &fakeCode{provider: provider, challenge: q.Get("code_challenge"), nonce: q.Get("nonce"),
		redirect: q.Get("redirect_uri"), who: who, refresh: fmt.Sprintf("1//REFRESH-sentinel-%d", f.n)}
	f.issued = append(f.issued, code, q.Get("state"), f.codes[code].refresh)
	f.mu.Unlock()
	return code, q.Get("state")
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	f.mu.Lock()
	defer f.mu.Unlock()
	if form.Get("client_id") != "client-id" || form.Get("client_secret") != sentClientSec {
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	switch form.Get("grant_type") {
	case "refresh_token":
		f.refreshes++
		tok := fmt.Sprintf("ya29.ACCESS-refresh-sentinel-%d", f.refreshes)
		f.issued = append(f.issued, tok)
		writeJSON(w, 200, map[string]any{"access_token": tok, "expires_in": 3599})
		return
	case "authorization_code":
	default:
		writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	c, ok := f.codes[form.Get("code")]
	if !ok || c.used {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "code " + form.Get("code") + " is spent"})
		return
	}
	c.used = true
	v := form.Get("code_verifier")
	f.verifiers = append(f.verifiers, v)
	f.issued = append(f.issued, v)
	sum := sha256.Sum256([]byte(v))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge || form.Get("redirect_uri") != c.redirect {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "PKCE or redirect mismatch"})
		return
	}
	now := time.Now()
	claims := map[string]any{"aud": "client-id", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"nonce": c.nonce, "sub": c.who.sub}
	scope := c.who.scope
	if c.provider == ProviderGoogle {
		claims["iss"] = "https://accounts.google.com"
		claims["email"], claims["email_verified"] = c.who.email, true
		if scope == "" {
			scope = "openid email https://mail.google.com/"
		}
	} else {
		tid := c.who.tid
		if tid == "" {
			tid = msTenant
		}
		claims["iss"] = "https://login.microsoftonline.com/" + tid + "/v2.0"
		claims["tid"], claims["preferred_username"] = tid, c.who.email
		if scope == "" {
			scope = "https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send openid email offline_access"
		}
	}
	access := fmt.Sprintf("ya29.ACCESS-code-sentinel-%d", f.n)
	f.issued = append(f.issued, access)
	body := map[string]any{"access_token": access, "expires_in": 3599, "token_type": "Bearer", "scope": scope}
	if !f.omitRefTok {
		body["refresh_token"] = c.refresh
	}
	if !f.omitIDTok {
		b, _ := json.Marshal(claims)
		body["id_token"] = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." +
			base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
	}
	writeJSON(w, 200, body)
}

// --- the harness ------------------------------------------------------------------

type connectHarness struct {
	t      *testing.T
	ctx    context.Context
	super  *pgxpool.Pool
	st     *store.Store
	keys   staticKeys
	idp    *fakeIdP
	conn   *Connector
	mgr    *Manager
	lc     *leakCheck
	locs   []string
	clock  func() time.Time
	mu     stdsync.Mutex
	offset time.Duration
	// checks records each mailbox check's address; checkErr is its answer.
	checks   []string
	checkErr error
	// smtpChecks counts the receive-only SMTP probes (ADR-046 §12);
	// smtpErr is their answer.
	smtpChecks int
	smtpErr    error
}

func newConnectHarness(t *testing.T) *connectHarness {
	t.Helper()
	superURL, appURL := os.Getenv("MAILROOM_TEST_SUPER_URL"), os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(7*i + 3)
	}
	h := &connectHarness{t: t, ctx: ctx, super: super, st: store.NewFromPool(app), keys: staticKeys{master}}
	h.lc = newLeakCheck(t)
	h.idp = newFakeIdP(t)
	h.clock = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return time.Now().Add(h.offset) }
	g := Google(Client{ID: "client-id", Secret: sentClientSec})
	g.AuthURL, g.TokenURL, g.RevokeURL = h.idp.srv.URL+"/auth", h.idp.srv.URL+"/token", h.idp.srv.URL+"/revoke"
	ms, _ := Microsoft(Client{ID: "client-id", Secret: sentClientSec}, "common")
	ms.AuthURL, ms.TokenURL = h.idp.srv.URL+"/authorize", h.idp.srv.URL+"/token"
	reg, err := NewRegistry(g, ms)
	if err != nil {
		t.Fatal(err)
	}
	h.mgr = NewManager(reg, StoreGrants{Store: h.st, Keys: h.keys}, WithHTTPClient(h.idp.srv.Client()), WithLogger(h.lc.logger()))
	h.conn, err = NewConnector(ConnectorConfig{
		Providers: reg, Store: h.st, Keys: h.keys, Manager: h.mgr, PublicURL: "https://mail.example.com",
		HTTPClient: h.idp.srv.Client(), Logger: h.lc.logger(), Clock: h.clock,
		CheckMailbox: func(_ context.Context, p Provider, address string, tok xoauth2.Secret) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if tok.Empty() {
				return errors.New("no token")
			}
			h.checks = append(h.checks, p.Name+":"+address)
			return h.checkErr
		},
		CheckSMTP: func(_ context.Context, p Provider, address string, tok xoauth2.Secret) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if tok.Empty() {
				return errors.New("no token")
			}
			h.smtpChecks++
			return h.smtpErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Everything the provider issued, plus every state, is a sentinel.
		h.idp.mu.Lock()
		issued := append([]string(nil), h.idp.issued...)
		h.idp.mu.Unlock()
		hay := h.lc.buf.String() + "\n" + strings.Join(h.lc.errs, "\n") + "\n" + strings.Join(h.locs, "\n")
		for _, s := range issued {
			if s != "" && strings.Contains(hay, s) {
				t.Errorf("SECRET LEAK: %q appeared in a log line, an error or a redirect", s)
			}
		}
	})
	return h
}

func (h *connectHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.offset += d
	h.mu.Unlock()
}

func (h *connectHarness) user() ulid.ULID {
	h.t.Helper()
	id := store.NewULID()
	if _, err := h.super.Exec(h.ctx, `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		id.String(), "sub-"+id.String(), id.String()+"@example.com"); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *connectHarness) start(user ulid.ULID, provider string, reauth *ulid.ULID) string {
	h.t.Helper()
	res, err := h.conn.Start(h.ctx, user, provider, reauth)
	if h.lc.err(err) != nil {
		h.t.Fatalf("Start(%s): %v", provider, err)
	}
	return res.AuthorizeURL
}

// callback presents q at /oauth/callback/{provider} with user's session and
// returns the result query of the redirect.
func (h *connectHarness) callback(user ulid.ULID, provider string, q url.Values) url.Values {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, CallbackPathPrefix+provider+"?"+q.Encode(), nil)
	req.SetPathValue("provider", provider)
	rec := httptest.NewRecorder()
	h.conn.Callback(rec, req, user, true)
	if rec.Code != http.StatusSeeOther {
		h.t.Fatalf("callback status = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	h.locs = append(h.locs, loc)
	if !strings.HasPrefix(loc, "/?") || !strings.HasSuffix(loc, "#/settings") {
		h.t.Fatalf("callback redirected to %q, not the fixed in-app location", loc)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		h.t.Errorf("callback headers = %v", rec.Header())
	}
	u, _ := url.Parse(loc)
	return u.Query()
}

func (h *connectHarness) connect(user ulid.ULID, provider string, who fakeWho) url.Values {
	h.t.Helper()
	code, state := h.idp.authorize(h.t, provider, h.start(user, provider, nil), who)
	return h.callback(user, provider, url.Values{"code": {code}, "state": {state}})
}

func (h *connectHarness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.super.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (h *connectHarness) accounts(user ulid.ULID) int {
	return h.count(`SELECT count(*) FROM app.accounts WHERE user_id = $1`, user.String())
}

func (h *connectHarness) creds(user, acct ulid.ULID) onboarding.Credentials {
	h.t.Helper()
	var ct []byte
	if err := h.super.QueryRow(h.ctx, `SELECT credentials_ct FROM app.accounts WHERE id = $1`, acct.String()).Scan(&ct); err != nil {
		h.t.Fatal(err)
	}
	c, err := onboarding.OpenCredentials(h.keys.master, user, ct)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func mustAccount(t *testing.T, res url.Values) ulid.ULID {
	t.Helper()
	id, err := ulid.Parse(res.Get("account"))
	if err != nil {
		t.Fatalf("result %v names no account", res)
	}
	return id
}

var alice = fakeWho{sub: "google-sub-alice", email: "alice@example.com"}

// --- tests ------------------------------------------------------------------------

func TestConnectGoogleHappyPath(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	authURL := h.start(a, ProviderGoogle, nil)
	u, _ := url.Parse(authURL)
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": "client-id", "access_type": "offline", "prompt": "consent",
		"scope": "openid email https://mail.google.com/", "code_challenge_method": "S256",
		"redirect_uri": "https://mail.example.com/oauth/callback/google",
	} {
		if q.Get(k) != want {
			t.Errorf("authorization %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("response_mode") == "form_post" || len(q.Get("state")) < 40 || q.Get("nonce") == "" {
		t.Errorf("authorization URL: response_mode=%q state len %d nonce %q", q.Get("response_mode"), len(q.Get("state")), q.Get("nonce"))
	}
	// Only the state's hash is stored, and the verifier only sealed.
	if n := h.count(`SELECT count(*) FROM app.oauth_flows WHERE user_id = $1 AND state_hash = $2`,
		a.String(), sha256Sum(q.Get("state"))); n != 1 {
		t.Fatalf("flow rows by state hash = %d, want 1", n)
	}

	code, state := h.idp.authorize(t, ProviderGoogle, authURL, alice)
	res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}, "iss": {"https://accounts.google.com"}})
	if res.Get("oauth") != ResultConnected || res.Get("provider") != ProviderGoogle {
		t.Fatalf("result = %v", res)
	}
	acct := mustAccount(t, res)
	var (
		name, profile, kind, imapHost, smtpHost string
		imapPort, smtpPort                      int
		syncOn, writeback                       bool
	)
	if err := h.super.QueryRow(h.ctx, `SELECT name, provider_profile, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
	       sync_enabled, writeback_enabled FROM app.accounts WHERE id = $1`, acct.String()).Scan(
		&name, &profile, &kind, &imapHost, &imapPort, &smtpHost, &smtpPort, &syncOn, &writeback); err != nil {
		t.Fatal(err)
	}
	if name != "alice@example.com" || profile != "gmail" || kind != "oauth2" || imapHost != "imap.gmail.com" ||
		imapPort != 993 || smtpHost != "smtp.gmail.com" || smtpPort != 465 || writeback {
		t.Errorf("account = %s %s %s %s:%d %s:%d sync=%v writeback=%v", name, profile, kind, imapHost, imapPort,
			smtpHost, smtpPort, syncOn, writeback)
	}
	// The Gmail sync hold: connected with sync OFF, and the hold names why.
	if syncOn {
		t.Error("a connected Gmail account has sync on while the Gmail sync hold stands")
	}
	if code, _, held := SyncHold(kind, profile); !held || code != SyncHoldGmailPending {
		t.Errorf("SyncHold(%s, %s) = %q held=%v", kind, profile, code, held)
	}
	var subject string
	var scopes []string
	var gen int64
	if err := h.super.QueryRow(h.ctx, `SELECT subject, scopes, generation FROM app.oauth_grants WHERE account_id = $1`,
		acct.String()).Scan(&subject, &scopes, &gen); err != nil {
		t.Fatal(err)
	}
	if subject != alice.sub || len(scopes) != 3 || gen < 1<<32 {
		t.Errorf("grant = subject %q scopes %v generation %d", subject, scopes, gen)
	}
	c := h.creds(a, acct)
	if c.OAuth == nil || c.OAuth.Provider != ProviderGoogle || c.OAuth.Subject != alice.sub ||
		!strings.HasPrefix(c.OAuth.RefreshToken, "1//REFRESH-sentinel") || c.IMAPUsername != "alice@example.com" ||
		c.IMAPPassword != "" {
		t.Errorf("sealed credentials wrong (oauth set=%v)", c.OAuth != nil)
	}
	if n := h.count(`SELECT count(*) FROM app.identities WHERE account_id = $1 AND address = 'alice@example.com'`, acct.String()); n != 1 {
		t.Errorf("identities = %d, want 1", n)
	}
	// change_log: the account creation (and the grant's 'updated'), in the
	// same transaction; audit: oauth.connect.
	if n := h.count(`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Account' AND object_id = $2`,
		a.String(), acct.String()); n < 1 {
		t.Error("no Account change_log row for the new account")
	}
	if n := h.count(`SELECT count(*) FROM app.audit_log WHERE user_id = $1 AND action = 'oauth.connect' AND object_id = $2`,
		a.String(), acct.String()); n != 1 {
		t.Errorf("oauth.connect audit rows = %d", n)
	}
	if n := h.count(`SELECT count(*) FROM app.oauth_flows WHERE user_id = $1`, a.String()); n != 0 {
		t.Errorf("flow rows after the callback = %d, want 0 (single use)", n)
	}
	if len(h.checks) != 1 || h.checks[0] != "google:alice@example.com" {
		t.Errorf("mailbox checks = %v", h.checks)
	}
	// The grant works: the Manager refreshes from the sealed token.
	if tok, err := h.mgr.AccessToken(h.ctx, a, acct); h.lc.err(err) != nil || tok.Empty() {
		t.Errorf("AccessToken after connect: %v", err)
	}
}

func TestConnectMicrosoftHappyPath(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	authURL := h.start(a, ProviderMicrosoft, nil)
	u, _ := url.Parse(authURL)
	if q := u.Query(); q.Get("response_mode") != "query" ||
		q.Get("scope") != "openid email offline_access https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send" ||
		q.Get("redirect_uri") != "https://mail.example.com/oauth/callback/microsoft" || q.Has("access_type") {
		t.Errorf("authorization query = %v", q)
	}
	code, state := h.idp.authorize(t, ProviderMicrosoft, authURL, fakeWho{sub: "ms-sub-bob", email: "Bob@Example.com"})
	res := h.callback(a, ProviderMicrosoft, url.Values{"code": {code}, "state": {state}})
	if res.Get("oauth") != ResultConnected {
		t.Fatalf("result = %v", res)
	}
	acct := mustAccount(t, res)
	var profile, imapHost, smtpHost string
	var smtpPort int
	var writeback, syncOn bool
	if err := h.super.QueryRow(h.ctx, `SELECT provider_profile, imap_host, smtp_host, smtp_port, writeback_enabled, sync_enabled
	       FROM app.accounts WHERE id = $1`, acct.String()).Scan(&profile, &imapHost, &smtpHost, &smtpPort, &writeback, &syncOn); err != nil {
		t.Fatal(err)
	}
	if profile != "microsoft" || imapHost != "outlook.office365.com" || smtpHost != "smtp.office365.com" || smtpPort != 587 || !writeback {
		t.Errorf("account = %s %s %s:%d writeback=%v", profile, imapHost, smtpHost, smtpPort, writeback)
	}
	// Not held: a Microsoft account syncs from the moment it connects.
	if !syncOn {
		t.Error("a connected Microsoft account has sync off")
	}
	if _, _, held := SyncHold("oauth2", profile); held {
		t.Error("a Microsoft account is under the Gmail sync hold")
	}
	if c := h.creds(a, acct); c.IMAPUsername != "bob@example.com" {
		t.Errorf("address = %q, want the lower-cased preferred_username", c.IMAPUsername)
	}
}

// Replay (the same state twice), an unknown state, and an expired flow.
func TestConnectStateRefusals(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	code, state := h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, nil), alice)
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultConnected {
		t.Fatalf("first callback = %v", res)
	}
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultInvalidState {
		t.Errorf("REPLAY = %v, want invalid_state", res)
	}
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {"x"}, "state": {"not-a-state-anyone-issued"}}); res.Get("oauth") != ResultInvalidState {
		t.Errorf("unknown state = %v, want invalid_state", res)
	}
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {"x"}}); res.Get("oauth") != ResultInvalidRequest {
		t.Errorf("no state = %v, want invalid_request", res)
	}
	if h.accounts(a) != 1 {
		t.Errorf("accounts = %d, want 1", h.accounts(a))
	}

	// Expired: consumed and refused, and stays refused.
	code, state = h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, nil), fakeWho{sub: "s2", email: "x2@example.com"})
	h.advance(flowTTL + time.Minute)
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultExpired {
		t.Errorf("expired flow = %v, want expired", res)
	}
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultInvalidState {
		t.Errorf("expired flow replayed = %v, want invalid_state (it was consumed)", res)
	}
}

// CROSS-USER: user B's session presenting user A's state finds nothing (RLS),
// creates nothing, and does NOT consume A's flow — A then completes normally.
// This is the "attacker connects their own mailbox into the victim's
// Mailroom" defence: the state is bound to the user who started it.
func TestConnectCrossUserStateRefused(t *testing.T) {
	h := newConnectHarness(t)
	a, b := h.user(), h.user()
	code, state := h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, nil), alice)
	if res := h.callback(b, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultInvalidState {
		t.Fatalf("B presenting A's state = %v, want invalid_state", res)
	}
	if h.accounts(b) != 0 || h.accounts(a) != 0 {
		t.Fatalf("accounts after the cross-user attempt: A=%d B=%d", h.accounts(a), h.accounts(b))
	}
	if n := h.count(`SELECT count(*) FROM app.oauth_flows WHERE user_id = $1`, a.String()); n != 1 {
		t.Fatalf("A's flow rows = %d after B's attempt, want 1 (B cannot consume it)", n)
	}
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultConnected {
		t.Errorf("A completing afterwards = %v, want connected", res)
	}
}

// MIX-UP: a flow started for Google completing at the Microsoft callback is
// refused and consumed; so is an authorization response whose RFC 9207 iss
// names another provider.
func TestConnectProviderMixUp(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	code, state := h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, nil), alice)
	if res := h.callback(a, ProviderMicrosoft, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultProviderMismatch {
		t.Fatalf("google flow at the microsoft callback = %v, want provider_mismatch", res)
	}
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultInvalidState {
		t.Errorf("retrying at the right callback = %v, want invalid_state (consumed)", res)
	}
	code, state = h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, nil), alice)
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state},
		"iss": {"https://login.microsoftonline.com/" + msTenant + "/v2.0"}}); res.Get("oauth") != ResultProviderMismatch {
		t.Errorf("foreign iss = %v, want provider_mismatch", res)
	}
	if h.accounts(a) != 0 {
		t.Errorf("accounts = %d, want 0", h.accounts(a))
	}
}

func TestConnectRefusalsCreateNothing(t *testing.T) {
	h := newConnectHarness(t)
	var a ulid.ULID // a fresh user per case, so one case's failure cannot mask another's
	for _, tc := range []struct {
		name  string
		setup func()
		who   fakeWho
		q     func(code, state string) url.Values
		want  string
	}{
		{"user declined", nil, alice, func(_, state string) url.Values {
			return url.Values{"error": {"access_denied"}, "error_description": {"The user denied"}, "state": {state}}
		}, ResultDenied},
		{"provider error", nil, alice, func(_, state string) url.Values {
			return url.Values{"error": {"server_error"}, "state": {state}}
		}, ResultProviderError},
		{"partial grant: mail scope unticked", nil, fakeWho{sub: "s", email: "a@example.com", scope: "openid email"},
			nil, ResultMissingScope},
		{"no refresh token", func() { h.idp.omitRefTok = true }, alice, nil, ResultNoRefreshToken},
		{"no id token", func() { h.idp.omitIDTok = true }, alice, nil, ResultIdentityInvalid},
		{"IMAP refuses the token", func() { h.checkErr = &sync.Error{Kind: sync.ErrorKindAuth, Op: "login", Err: errors.New("NO")} },
			alice, nil, ResultMailboxRefused},
		{"IMAP unreachable", func() {
			h.checkErr = &sync.Error{Kind: sync.ErrorKindTransient, Op: "dial", Err: errors.New("timeout")}
		},
			alice, nil, ResultMailboxUnreachable},
		{"spent code at the token endpoint", nil, alice, func(code, state string) url.Values {
			h.idp.mu.Lock()
			h.idp.codes[code].used = true
			h.idp.mu.Unlock()
			return url.Values{"code": {code}, "state": {state}}
		}, ResultExchangeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a = h.user()
			h.idp.omitRefTok, h.idp.omitIDTok, h.checkErr = false, false, nil
			if tc.setup != nil {
				tc.setup()
			}
			code, state := h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, nil), tc.who)
			q := url.Values{"code": {code}, "state": {state}}
			if tc.q != nil {
				q = tc.q(code, state)
			}
			if res := h.callback(a, ProviderGoogle, q); res.Get("oauth") != tc.want {
				t.Fatalf("result = %v, want %s", res, tc.want)
			}
			if h.accounts(a) != 0 {
				t.Fatalf("a refused connect created an account")
			}
			if n := h.count(`SELECT count(*) FROM app.oauth_flows WHERE user_id = $1`, a.String()); n != 0 {
				t.Errorf("flow rows = %d after a refusal, want 0 (consumed)", n)
			}
		})
	}
}

// DUPLICATE: connecting the same provider account twice answers
// already_connected with the existing account, and creates nothing.
func TestConnectDuplicate(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	first := h.connect(a, ProviderGoogle, alice)
	if first.Get("oauth") != ResultConnected {
		t.Fatalf("first = %v", first)
	}
	again := h.connect(a, ProviderGoogle, alice)
	if again.Get("oauth") != ResultAlreadyConnected || again.Get("account") != first.Get("account") {
		t.Fatalf("duplicate = %v, want already_connected naming %s", again, first.Get("account"))
	}
	if h.accounts(a) != 1 {
		t.Errorf("accounts = %d, want 1", h.accounts(a))
	}
	// A DIFFERENT Mailroom user may connect the same provider account: the
	// uniqueness is per Mailroom user.
	b := h.user()
	if res := h.connect(b, ProviderGoogle, alice); res.Get("oauth") != ResultConnected {
		t.Errorf("another user connecting the same mailbox = %v", res)
	}
	// Same address, same provider, different subject (a different Google
	// account): a second account, named apart.
	other := h.connect(a, ProviderGoogle, fakeWho{sub: "google-sub-other", email: "alice@example.com"})
	if other.Get("oauth") != ResultConnected || h.accounts(a) != 2 {
		t.Errorf("second subject = %v, accounts %d", other, h.accounts(a))
	}
}

// RE-AUTHENTICATION (obligation (d)): same subject → the grant is updated in
// place (state back to active, generation bumped with the new sealed token,
// obligation (c)); a different subject → refused, nothing changes.
func TestReauthenticate(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	acct := mustAccount(t, h.connect(a, ProviderGoogle, alice))
	var gen0 int64
	_ = h.super.QueryRow(h.ctx, `SELECT generation FROM app.oauth_grants WHERE account_id = $1`, acct.String()).Scan(&gen0)
	if _, err := h.super.Exec(h.ctx, `UPDATE app.oauth_grants SET state = 'needs_reauth', state_reason = 'invalid_grant' WHERE account_id = $1`, acct.String()); err != nil {
		t.Fatal(err)
	}
	old := h.creds(a, acct).OAuth.RefreshToken
	changes0 := h.count(`SELECT count(*) FROM app.change_log WHERE object_type = 'Account' AND object_id = $1`, acct.String())

	// Different subject: refused.
	authURL := h.start(a, ProviderGoogle, &acct)
	if u, _ := url.Parse(authURL); u.Query().Get("login_hint") != "alice@example.com" {
		t.Errorf("re-auth login_hint = %q", u.Query().Get("login_hint"))
	}
	code, state := h.idp.authorize(t, ProviderGoogle, authURL, fakeWho{sub: "google-sub-mallory", email: "alice@example.com"})
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultWrongAccount {
		t.Fatalf("re-auth as another subject = %v, want wrong_account", res)
	}
	if c := h.creds(a, acct); c.OAuth.RefreshToken != old {
		t.Fatal("a refused re-auth replaced the refresh token")
	}

	// Same subject: updated.
	code, state = h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, &acct), alice)
	if res := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); res.Get("oauth") != ResultReconnected {
		t.Fatalf("re-auth = %v, want reconnected", res)
	}
	var st string
	var gen1 int64
	_ = h.super.QueryRow(h.ctx, `SELECT state, generation FROM app.oauth_grants WHERE account_id = $1`, acct.String()).Scan(&st, &gen1)
	if st != store.OAuthStateActive || gen1 != gen0+1 {
		t.Errorf("after re-auth: state %q generation %d (was %d)", st, gen1, gen0)
	}
	if c := h.creds(a, acct); c.OAuth.RefreshToken == old || !strings.HasPrefix(c.OAuth.RefreshToken, "1//REFRESH-sentinel") {
		t.Error("re-auth did not seal the new refresh token")
	}
	if n := h.count(`SELECT count(*) FROM app.change_log WHERE object_type = 'Account' AND object_id = $1`, acct.String()) - changes0; n != 1 {
		t.Errorf("re-auth appended %d Account change rows, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM app.audit_log WHERE action = 'oauth.reauth' AND object_id = $1`, acct.String()); n != 1 {
		t.Errorf("oauth.reauth audit rows = %d", n)
	}
	// A stale rotation from before the re-auth loses its compare-and-swap.
	won, err := StoreGrants{Store: h.st, Keys: h.keys}.RotateRefreshToken(h.ctx, a, acct, gen0, "1//REFRESH-sentinel-stale", time.Now())
	if h.lc.err(err) != nil || won {
		t.Errorf("a rotation holding the pre-reauth generation won=%v err=%v", won, err)
	}
	h.idp.remember("1//REFRESH-sentinel-stale")
}

// Start refuses what it must: an unconfigured provider, someone else's
// account, a password account, the wrong provider.
func TestStartRefusals(t *testing.T) {
	h := newConnectHarness(t)
	a, b := h.user(), h.user()
	acct := mustAccount(t, h.connect(a, ProviderGoogle, alice))
	if _, err := h.conn.Start(h.ctx, a, "yahoo", nil); !errors.Is(err, ErrProviderNotEnabled) {
		t.Errorf("unknown provider = %v", err)
	}
	if _, err := h.conn.Start(h.ctx, b, ProviderGoogle, &acct); !errors.Is(err, store.ErrAccountNotFound) {
		t.Errorf("B re-authenticating A's account = %v, want ErrAccountNotFound", err)
	}
	if _, err := h.conn.Start(h.ctx, a, ProviderMicrosoft, &acct); !errors.Is(err, ErrWrongProvider) {
		t.Errorf("wrong provider = %v", err)
	}
	pw := store.NewULID()
	if _, err := h.super.Exec(h.ctx, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'pw', 'imap.example.com', 'smtp.example.com', '\x00', $3)`, pw.String(), a.String(),
		onboarding.CredentialsSchemeV1.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conn.Start(h.ctx, a, ProviderGoogle, &pw); !errors.Is(err, ErrNotOAuthAccount) {
		t.Errorf("password account = %v", err)
	}
	if n := h.count(`SELECT count(*) FROM app.oauth_flows WHERE user_id IN ($1, $2)`, a.String(), b.String()); n != 0 {
		t.Errorf("refused starts left %d flow rows", n)
	}
}

// DISCONNECT: Google revokes; the grant and the refresh token go; sync stops;
// the cached access token is forgotten; the mail stays. Microsoft has no
// revocation. Reconnect: same subject re-attaches, another is refused.
func TestDisconnectAndReconnect(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	acct := mustAccount(t, h.connect(a, ProviderGoogle, alice))
	refresh := h.creds(a, acct).OAuth.RefreshToken
	// A message living in this account (mailbox + provenance), so "keeps the
	// mail" is a claim about mail, not about an empty account.
	msg, mbox := store.NewULID(), store.NewULID()
	hash := sha256Sum(msg.String())
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name) VALUES ($1, $2, $3, 'INBOX', 'Inbox')`,
			[]any{mbox.String(), a.String(), acct.String()}},
		{`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1, $2, 10, 42, $3)`,
			[]any{a.String(), hash, store.NewULID().String()}},
		{`INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes) VALUES ($1, $2, $3, 'kept', now(), 10)`,
			[]any{msg.String(), a.String(), hash}},
		{`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		  VALUES ($1, $2, $3, $4, $5, 1, 1)`, []any{store.NewULID().String(), a.String(), msg.String(), acct.String(), mbox.String()}},
	} {
		if _, err := h.super.Exec(h.ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed mail: %v", err)
		}
	}
	// Prime the Manager's cache.
	if _, err := h.mgr.AccessToken(h.ctx, a, acct); h.lc.err(err) != nil {
		t.Fatal(err)
	}
	refreshes := h.idp.refreshes
	changes0 := h.count(`SELECT count(*) FROM app.change_log WHERE object_type = 'Account' AND object_id = $1`, acct.String())

	// Sync on before the disconnect (as it will be once the Gmail sync hold
	// lifts), so "disconnect switches sync off" is observed, not inherited.
	if _, err := h.super.Exec(h.ctx, `UPDATE app.accounts SET sync_enabled = true WHERE id = $1`, acct.String()); err != nil {
		t.Fatal(err)
	}
	res, err := h.conn.Disconnect(h.ctx, a, acct)
	if h.lc.err(err) != nil || res.Revocation != "revoked" || res.Provider != ProviderGoogle {
		t.Fatalf("Disconnect = %+v, %v", res, err)
	}
	if len(h.idp.revoked) != 1 || h.idp.revoked[0] != refresh {
		t.Errorf("revocation calls = %d (sent the refresh token: %v)", len(h.idp.revoked), len(h.idp.revoked) == 1 && h.idp.revoked[0] == refresh)
	}
	if n := h.count(`SELECT count(*) FROM app.oauth_grants WHERE account_id = $1`, acct.String()); n != 0 {
		t.Error("the grant survived the disconnect")
	}
	c := h.creds(a, acct)
	if c.OAuth == nil || c.OAuth.RefreshToken != "" || c.OAuth.Subject != alice.sub || c.IMAPUsername != "alice@example.com" {
		t.Errorf("sealed credentials after disconnect: oauth=%v refresh-empty=%v subject-kept=%v",
			c.OAuth != nil, c.OAuth != nil && c.OAuth.RefreshToken == "", c.OAuth != nil && c.OAuth.Subject == alice.sub)
	}
	var syncOn bool
	_ = h.super.QueryRow(h.ctx, `SELECT sync_enabled FROM app.accounts WHERE id = $1`, acct.String()).Scan(&syncOn)
	if syncOn || h.accounts(a) != 1 {
		t.Errorf("after disconnect: sync %v, accounts %d (want off, 1)", syncOn, h.accounts(a))
	}
	if h.count(`SELECT count(*) FROM app.messages WHERE id = $1`, msg.String()) != 1 ||
		h.count(`SELECT count(*) FROM app.message_provenance WHERE account_id = $1`, acct.String()) != 1 {
		t.Error("disconnect deleted mail")
	}
	if n := h.count(`SELECT count(*) FROM app.change_log WHERE object_type = 'Account' AND object_id = $1`, acct.String()) - changes0; n != 1 {
		t.Errorf("disconnect appended %d Account change rows, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM app.audit_log WHERE action = 'oauth.disconnect' AND object_id = $1 AND detail->>'revocation' = 'revoked'`, acct.String()); n != 1 {
		t.Errorf("oauth.disconnect audit rows = %d", n)
	}
	// Forget: the cached access token is gone, and there is no grant to
	// refresh from — no token at all, and no call to the token endpoint.
	if _, err := h.mgr.AccessToken(h.ctx, a, acct); !errors.Is(h.lc.err(err), xoauth2.ErrReauthRequired) {
		t.Errorf("AccessToken after disconnect = %v, want ErrReauthRequired (no grant)", err)
	}
	if h.idp.refreshes != refreshes {
		t.Error("a disconnected account's token was refreshed")
	}
	if _, err := h.conn.Disconnect(h.ctx, a, acct); !errors.Is(err, ErrNoGrant) {
		t.Errorf("second disconnect = %v, want ErrNoGrant", err)
	}

	// Reconnect as another subject: refused.
	code, state := h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, &acct), fakeWho{sub: "google-sub-mallory", email: "alice@example.com"})
	if r := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); r.Get("oauth") != ResultWrongAccount {
		t.Fatalf("reconnect as another subject = %v", r)
	}
	// Reconnect as the same subject: re-attached; a Gmail account stays held
	// with sync off (the Gmail sync hold), whatever it was before.
	code, state = h.idp.authorize(t, ProviderGoogle, h.start(a, ProviderGoogle, &acct), alice)
	if r := h.callback(a, ProviderGoogle, url.Values{"code": {code}, "state": {state}}); r.Get("oauth") != ResultReconnected {
		t.Fatalf("reconnect = %v", r)
	}
	_ = h.super.QueryRow(h.ctx, `SELECT sync_enabled FROM app.accounts WHERE id = $1`, acct.String()).Scan(&syncOn)
	var gen int64
	_ = h.super.QueryRow(h.ctx, `SELECT generation FROM app.oauth_grants WHERE account_id = $1`, acct.String()).Scan(&gen)
	if syncOn || gen < 1<<32 {
		t.Errorf("after reconnect: sync %v (want off: Gmail sync hold) generation %d", syncOn, gen)
	}

	// Microsoft: no programmatic revocation; a reconnect switches sync back on.
	ms := mustAccount(t, h.connect(a, ProviderMicrosoft, fakeWho{sub: "ms-sub", email: "m@example.com"}))
	before := len(h.idp.revoked)
	r2, err := h.conn.Disconnect(h.ctx, a, ms)
	if h.lc.err(err) != nil || r2.Revocation != "unsupported" || len(h.idp.revoked) != before {
		t.Errorf("microsoft disconnect = %+v, %v, revocations %d", r2, err, len(h.idp.revoked)-before)
	}
	_ = h.super.QueryRow(h.ctx, `SELECT sync_enabled FROM app.accounts WHERE id = $1`, ms.String()).Scan(&syncOn)
	if syncOn {
		t.Error("microsoft disconnect left sync on")
	}
	code, state = h.idp.authorize(t, ProviderMicrosoft, h.start(a, ProviderMicrosoft, &ms), fakeWho{sub: "ms-sub", email: "m@example.com"})
	if r := h.callback(a, ProviderMicrosoft, url.Values{"code": {code}, "state": {state}}); r.Get("oauth") != ResultReconnected {
		t.Fatalf("microsoft reconnect = %v", r)
	}
	_ = h.super.QueryRow(h.ctx, `SELECT sync_enabled FROM app.accounts WHERE id = $1`, ms.String()).Scan(&syncOn)
	if !syncOn {
		t.Error("microsoft reconnect did not switch sync back on")
	}
}

// RLS through the connector: another user can neither disconnect nor
// re-authenticate A's account.
func TestConnectorCrossUserRefusals(t *testing.T) {
	h := newConnectHarness(t)
	a, b := h.user(), h.user()
	acct := mustAccount(t, h.connect(a, ProviderGoogle, alice))
	if _, err := h.conn.Disconnect(h.ctx, b, acct); !errors.Is(err, store.ErrAccountNotFound) {
		t.Errorf("B disconnecting A's account = %v, want ErrAccountNotFound", err)
	}
	if n := h.count(`SELECT count(*) FROM app.oauth_grants WHERE account_id = $1`, acct.String()); n != 1 || len(h.idp.revoked) != 0 {
		t.Errorf("B's disconnect reached A's grant (grants %d, revocations %d)", n, len(h.idp.revoked))
	}
}

func TestSweepExpiredFlows(t *testing.T) {
	h := newConnectHarness(t)
	a, b := h.user(), h.user()
	h.start(a, ProviderGoogle, nil)
	h.start(b, ProviderMicrosoft, nil)
	if n, err := h.conn.SweepExpiredFlows(h.ctx, []ulid.ULID{a, b}); err != nil || n != 0 {
		t.Fatalf("sweep before expiry = %d, %v", n, err)
	}
	h.advance(flowTTL + time.Second)
	if n, err := h.conn.SweepExpiredFlows(h.ctx, []ulid.ULID{a, b}); err != nil || n != 2 {
		t.Fatalf("sweep after expiry = %d, %v; want 2", n, err)
	}
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
