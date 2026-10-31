// SPDX-License-Identifier: AGPL-3.0-only

package auth

// CallbackHandler tests exercise the full flow (state check, code exchange
// with PKCE verifier, ID token verification, nonce check, claim
// extraction, session issuance) without a live provider: a local RSA key
// pair signs a hand-built ID token, verified via oidc.StaticKeySet, and an
// httptest.Server stands in for the provider's token endpoint. OIDC
// discovery (oidc.NewProvider, exercised only by New) is out of scope here
// — see the package doc's "Testing scope" section.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/oklog/ulid/v2"
	"golang.org/x/oauth2"
)

const (
	testIssuer   = "https://auth.example.com/application/o/mailroom/"
	testClientID = "test-client"
)

// callbackHarness bundles everything needed to drive CallbackHandler
// end-to-end against fakes.
type callbackHarness struct {
	t           *testing.T
	priv        *rsa.PrivateKey
	tokenServer *httptest.Server
	a           *Authenticator
	resolveArgs []Claims
	resolveErr  error
	resolveID   ulid.ULID
}

func newCallbackHarness(t *testing.T) *callbackHarness {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	h := &callbackHarness{t: t, priv: priv, resolveID: ulid.Make()}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", h.handleToken)
	h.tokenServer = httptest.NewServer(mux)
	t.Cleanup(h.tokenServer.Close)

	verifier := oidc.NewVerifier(testIssuer, &oidc.StaticKeySet{
		PublicKeys: []crypto.PublicKey{&priv.PublicKey},
	}, &oidc.Config{ClientID: testClientID})

	a := testAuthenticator(t, true)
	a.oauth2Config = oauth2.Config{
		ClientID:     testClientID,
		ClientSecret: "test-secret",
		RedirectURL:  "https://mail.example.com/auth/callback",
		Endpoint: oauth2.Endpoint{
			AuthURL:  h.tokenServer.URL + "/authorize",
			TokenURL: h.tokenServer.URL + "/token",
		},
		Scopes: DefaultScopes,
	}
	a.verifier = verifier
	a.resolveUser = func(ctx context.Context, claims Claims) (ulid.ULID, error) {
		h.resolveArgs = append(h.resolveArgs, claims)
		if h.resolveErr != nil {
			return ulid.ULID{}, h.resolveErr
		}
		return h.resolveID, nil
	}
	h.a = a
	return h
}

// tokenResponse is set by each test before issuing the callback request.
type tokenResponseSpec struct {
	wantCode     string
	wantVerifier string
	idToken      string
	refreshToken string // "" = the provider issues none
}

var currentTokenSpec tokenResponseSpec

func (h *callbackHarness) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.t.Fatalf("token endpoint: parse form: %v", err)
	}
	spec := currentTokenSpec
	if got := r.PostFormValue("grant_type"); got != "authorization_code" {
		h.t.Errorf("token endpoint: grant_type = %q, want authorization_code", got)
	}
	if got := r.PostFormValue("code"); spec.wantCode != "" && got != spec.wantCode {
		h.t.Errorf("token endpoint: code = %q, want %q", got, spec.wantCode)
	}
	if got := r.PostFormValue("code_verifier"); spec.wantVerifier != "" && got != spec.wantVerifier {
		h.t.Errorf("token endpoint: code_verifier = %q, want %q", got, spec.wantVerifier)
	}

	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     spec.idToken,
	}
	if spec.refreshToken != "" {
		body["refresh_token"] = spec.refreshToken
	}
	_ = json.NewEncoder(w).Encode(body)
}

// signIDToken builds and RS256-signs a minimal ID token JWT.
func (h *callbackHarness) signIDToken(claims map[string]any) string {
	h.t.Helper()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		h.t.Fatalf("marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		h.t.Fatalf("marshal claims: %v", err)
	}
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	signingInput := seg(headerJSON) + "." + seg(claimsJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, h.priv, crypto.SHA256, sum[:])
	if err != nil {
		h.t.Fatalf("sign id token: %v", err)
	}
	return signingInput + "." + seg(sig)
}

func (h *callbackHarness) baseClaims(subject, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":                testIssuer,
		"sub":                subject,
		"aud":                []string{testClientID},
		"exp":                now.Add(time.Hour).Unix(),
		"iat":                now.Unix(),
		"nonce":              nonce,
		"email":              "person@example.com",
		"email_verified":     true,
		"preferred_username": "person",
	}
}

func setStateCookie(t *testing.T, a *Authenticator, req *http.Request, fs flowState) {
	t.Helper()
	val, err := encodeSigned(a.cfg.SigningKey, statePurpose, fs)
	if err != nil {
		t.Fatalf("encodeSigned(flowState): %v", err)
	}
	req.AddCookie(&http.Cookie{Name: a.cfg.StateCookieName, Value: val})
}

func TestCallbackHandler_Success(t *testing.T) {
	h := newCallbackHarness(t)

	fs := flowState{
		State:    "state-123",
		Nonce:    "nonce-123",
		Verifier: "verifier-123",
		ReturnTo: "/inbox",
		Expiry:   time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
	req.RemoteAddr = "192.0.2.7:51234"
	req.Header.Set("User-Agent", "callback-test-agent")
	setStateCookie(t, h.a, req, fs)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/inbox" {
		t.Errorf("Location = %q, want /inbox", got)
	}

	if len(h.resolveArgs) != 1 {
		t.Fatalf("resolveUser called %d times, want 1", len(h.resolveArgs))
	}
	got := h.resolveArgs[0]
	if got.Subject != "user-sub-1" {
		t.Errorf("Claims.Subject = %q, want user-sub-1", got.Subject)
	}
	if got.Email != "person@example.com" || !got.EmailVerified || got.PreferredUsername != "person" {
		t.Errorf("Claims = %+v, unexpected", got)
	}

	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == h.a.cfg.SessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie set on success")
	}
	if !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie flags wrong: %+v", sessionCookie)
	}
	if !wellFormedSessionCookie(sessionCookie.Value) {
		t.Errorf("session cookie value %q is not in the s1. format", sessionCookie.Value)
	}

	// Exactly one session row, bound to the cookie by its hash, for the user
	// ResolveUser returned, carrying the ID token, the user agent and the
	// address; the provider issued no refresh token in this exchange.
	fake := fakeOf(h.a)
	if fake.count() != 1 {
		t.Fatalf("session rows = %d, want exactly 1", fake.count())
	}
	row := fake.rowFor(sessionCookie.Value)
	if row == nil {
		t.Fatal("no session row matches the issued cookie's hash")
	}
	if row.in.UserID != h.resolveID {
		t.Errorf("session UserID = %v, want %v", row.in.UserID, h.resolveID)
	}
	if row.in.IDToken != currentTokenSpec.idToken {
		t.Error("the session row does not hold the raw ID token (the logout hint)")
	}
	if row.in.RefreshToken != "" {
		t.Error("a refresh token was stored though the provider issued none")
	}
	if row.in.UserAgent != "callback-test-agent" {
		t.Errorf("UserAgent = %q", row.in.UserAgent)
	}
	if row.in.Addr != "192.0.2.7" {
		t.Errorf("Addr = %q, want the client address without its port", row.in.Addr)
	}
}

// A refresh token the provider issues is handed to the store, to be sealed.
func TestCallbackHandler_StoresTheRefreshToken(t *testing.T) {
	h := newCallbackHarness(t)
	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
		refreshToken: "refresh-token-value",
	}
	rr := h.run(fs, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	c := issuedSession(rr, h.a)
	if c == nil {
		t.Fatal("no session cookie")
	}
	if got := fakeOf(h.a).rowFor(c.Value).in.RefreshToken; got != "refresh-token-value" {
		t.Errorf("stored refresh token = %q", got)
	}
}

// run drives the callback with the standard query and the given flow state,
// optionally with an extra cookie already in the browser.
func (h *callbackHarness) run(fs flowState, extra *http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+fs.State+"&code=auth-code-123", nil)
	req.RemoteAddr = "192.0.2.7:51234"
	req.Header.Set("User-Agent", "callback-test-agent")
	setStateCookie(h.t, h.a, req, fs)
	if extra != nil {
		req.AddCookie(extra)
	}
	rr := httptest.NewRecorder()
	h.a.CallbackHandler(rr, req)
	return rr
}

func issuedSession(rr *httptest.ResponseRecorder, a *Authenticator) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == a.cfg.SessionCookieName {
			return c
		}
	}
	return nil
}

// A Create failure fails the login: 500, no cookie, no row.
func TestCallbackHandler_CreateErrorFailsTheLoginWithNoCookie(t *testing.T) {
	h := newCallbackHarness(t)
	fakeOf(h.a).createErr = errors.New("database is down")
	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
	}
	rr := h.run(fs, nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if c := issuedSession(rr, h.a); c != nil {
		t.Fatalf("a failed Create still set a session cookie: %+v", c)
	}
}

// The name and groups claims (used by the auto-provisioning resolver for
// display_name and the admin-group role) are extracted from the ID token and
// handed to ResolveUser. Providers vary on whether they emit these in the ID
// token or only on UserInfo; this covers the ID-token path (the UserInfo backfill
// needs a live provider and so is out of scope here, like New's discovery).
func TestCallbackHandler_ParsesNameAndGroups(t *testing.T) {
	h := newCallbackHarness(t)

	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	claims := h.baseClaims("user-sub-1", fs.Nonce)
	claims["name"] = "Ada Lovelace"
	claims["groups"] = []string{"staff", "mailroom-admin"}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(claims),
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
	setStateCookie(t, h.a, req, fs)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body: %s", rr.Code, rr.Body.String())
	}
	if len(h.resolveArgs) != 1 {
		t.Fatalf("resolveUser called %d times, want 1", len(h.resolveArgs))
	}
	got := h.resolveArgs[0]
	if got.Name != "Ada Lovelace" {
		t.Errorf("Claims.Name = %q, want %q", got.Name, "Ada Lovelace")
	}
	if len(got.Groups) != 2 || got.Groups[0] != "staff" || got.Groups[1] != "mailroom-admin" {
		t.Errorf("Claims.Groups = %v, want [staff mailroom-admin]", got.Groups)
	}
}

func TestCallbackHandler_StateMismatchRejected(t *testing.T) {
	h := newCallbackHarness(t)
	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=WRONG&code=auth-code-123", nil)
	setStateCookie(t, h.a, req, fs)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if len(h.resolveArgs) != 0 {
		t.Fatal("resolveUser must not be called on state mismatch")
	}
}

func TestCallbackHandler_MissingStateCookieRejected(t *testing.T) {
	h := newCallbackHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCallbackHandler_ExpiredFlowStateRejected(t *testing.T) {
	h := newCallbackHarness(t)
	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(-time.Minute), // already expired
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
	setStateCookie(t, h.a, req, fs)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCallbackHandler_NonceMismatchRejected(t *testing.T) {
	h := newCallbackHarness(t)
	fs := flowState{
		State: "state-123", Nonce: "nonce-expected", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		// Signed with a different nonce than fs.Nonce.
		idToken: h.signIDToken(h.baseClaims("user-sub-1", "nonce-attacker-supplied")),
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
	setStateCookie(t, h.a, req, fs)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rr.Code, rr.Body.String())
	}
	if len(h.resolveArgs) != 0 {
		t.Fatal("resolveUser must not be called on nonce mismatch")
	}
}

func TestCallbackHandler_ResolveUserErrorRejected(t *testing.T) {
	h := newCallbackHarness(t)
	h.resolveErr = errResolveDenied

	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
	setStateCookie(t, h.a, req, fs)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if len(rr.Result().Cookies()) != 1 {
		// Only the cleared state cookie, no session cookie.
		t.Fatalf("expected only the cleared state cookie, got %d cookies", len(rr.Result().Cookies()))
	}
	if fakeOf(h.a).count() != 0 {
		t.Fatal("a refused login created a session row")
	}
}

var errResolveDenied = &resolveDeniedError{}

type resolveDeniedError struct{}

func (*resolveDeniedError) Error() string { return "user not provisioned" }

// A resolver error wrapping ErrCannotProvision (Forgejo #574) must NOT
// collapse to the generic "denied" refusal: the identity was verified, and
// the server's own records are what stopped provisioning, so the caller
// gets a distinguishable status (409, not 403) — still with no session
// cookie, exactly like every other refusal.
func TestCallbackHandler_CannotProvisionRejectedWith409(t *testing.T) {
	h := newCallbackHarness(t)
	h.resolveErr = fmt.Errorf("store: resolve login: %w", ErrCannotProvision)

	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
	setStateCookie(t, h.a, req, fs)
	rr := httptest.NewRecorder()

	h.a.CallbackHandler(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if len(rr.Result().Cookies()) != 1 {
		// Only the cleared state cookie, no session cookie.
		t.Fatalf("expected only the cleared state cookie, got %d cookies", len(rr.Result().Cookies()))
	}
}

// With a sign-in page configured (WithFailureRedirect), the redirect the
// browser lands on must carry the SPECIFIC code — unprovisionable, not
// denied — so page.go renders the sentence that says the server, not the
// user, is at fault. A plain resolver error alongside it proves the split:
// only the ErrCannotProvision-wrapping error changes the code.
func TestCallbackHandler_RedirectCarriesTheSpecificErrorCode(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr string
	}{
		{"cannot provision", fmt.Errorf("store: resolve login: %w", ErrCannotProvision), ErrorUnprovisionable},
		{"plain refusal", errResolveDenied, ErrorDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCallbackHarness(t)
			h.resolveErr = tt.err
			h.a.failureRedirect = "/auth/login"

			fs := flowState{
				State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
				Expiry: time.Now().Add(time.Minute),
			}
			currentTokenSpec = tokenResponseSpec{
				wantCode:     "auth-code-123",
				wantVerifier: fs.Verifier,
				idToken:      h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
			}

			req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-123&code=auth-code-123", nil)
			setStateCookie(t, h.a, req, fs)
			rr := httptest.NewRecorder()

			h.a.CallbackHandler(rr, req)

			if rr.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302; body: %s", rr.Code, rr.Body.String())
			}
			loc := rr.Header().Get("Location")
			if !strings.Contains(loc, "error="+tt.wantErr) {
				t.Errorf("Location = %q, want it to carry error=%s", loc, tt.wantErr)
			}
		})
	}
}

// SESSION FIXATION: a session that exists BEFORE the login must not survive it
// (Forgejo #310).
//
// The attack. An attacker plants a session cookie in the victim's browser, a
// value the attacker knows, by any of the usual routes (a subdomain that can
// write cookies for the parent domain, a physically shared machine, an XSS on a
// sibling origin). The victim then logs in properly. If the server ADOPTS the
// cookie it was handed and merely marks it authenticated, the attacker's known
// value is now a live session for the victim, and neither party can tell.
//
// With server-side sessions a login always mints a NEW secret and a NEW row, and
// revokes the row the request already held as "replaced". "Structurally hard to
// get wrong" is a claim about today's code, though: one plausible future change
// ("resume an existing session instead of forcing a round trip") reintroduces
// the bug silently, and a fixated session looks exactly like a working one from
// every other test in this package.
//
// So the planted cookie here is not junk. It is a VALID, live session for a
// DIFFERENT user, i.e. the strongest thing an attacker could plant, which is
// what makes "the value changed and the old one died" a meaningful assertion
// rather than one satisfied by any malformed input being discarded.
func TestCallbackHandler_ReplacesAPreExistingSession(t *testing.T) {
	h := newCallbackHarness(t)

	attackerID := ulid.Make()
	planted := signIn(t, h.a, attackerID, "")
	plantedRow := fakeOf(h.a).rowFor(planted.Value)
	if _, outcome := h.a.lookupSession(&http.Request{
		Header: http.Header{"Cookie": []string{planted.Name + "=" + planted.Value}},
	}); outcome != sessionOK {
		t.Fatal("the planted cookie is not actually a valid session, so this test would " +
			"prove nothing: ANY handling of it would look like a rejection")
	}

	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(h.baseClaims("victim-sub", fs.Nonce)),
	}

	rr := h.run(fs, planted) // the victim's browser already holds the attacker's session
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body: %s", rr.Code, rr.Body.String())
	}

	issued := issuedSession(rr, h.a)
	if issued == nil {
		// This is the fixation outcome itself: no Set-Cookie means the browser
		// keeps the value it already had, and that value is the attacker's.
		t.Fatal("a successful login issued NO session cookie, so the browser keeps the one it " +
			"arrived with, which is the attacker's. A login MUST replace the session.")
	}
	if issued.Value == planted.Value {
		t.Fatal("the login ADOPTED the session cookie it was handed: the attacker's known " +
			"value now authenticates the victim (session fixation)")
	}

	// The issued session is the VICTIM's...
	newRow := fakeOf(h.a).rowFor(issued.Value)
	if newRow == nil || newRow.in.UserID != h.resolveID {
		t.Fatalf("the issued session is not a new row for the resolved user: %+v", newRow)
	}
	if newRow.ref.ID == plantedRow.ref.ID {
		t.Error("the issued session reuses the planted row's id")
	}
	// ...and the planted one is DEAD: revoked as replaced, so it no longer works.
	if plantedRow.revokedAs != revokeReplaced {
		t.Fatalf("the planted session was revoked as %q, want %q", plantedRow.revokedAs, revokeReplaced)
	}
	rr2, called := runMiddleware(h.a, planted)
	if rr2.Code != http.StatusUnauthorized || called {
		t.Fatalf("the planted cookie still authenticates after the login (status %d)", rr2.Code)
	}
}

// A login whose new session cannot be recorded must NOT revoke the session the
// browser already had: the order is create first, then revoke the replaced one.
// The other order would sign a user out of a working session on a database blip.
func TestCallbackHandler_CreateErrorKeepsTheExistingSession(t *testing.T) {
	h := newCallbackHarness(t)
	existing := signIn(t, h.a, h.resolveID, "")
	row := fakeOf(h.a).rowFor(existing.Value)

	fakeOf(h.a).createErr = errors.New("database is down")
	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	currentTokenSpec = tokenResponseSpec{
		wantCode:     "auth-code-123",
		wantVerifier: fs.Verifier,
		idToken:      h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
	}
	rr := h.run(fs, existing)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if row.revokedAs != "" {
		t.Fatalf("a failed login revoked the existing session (%q)", row.revokedAs)
	}
	fakeOf(h.a).createErr = nil
	if rr2, called := runMiddleware(h.a, existing); rr2.Code != http.StatusOK || !called {
		t.Fatalf("the existing session no longer authenticates (status %d)", rr2.Code)
	}
}
