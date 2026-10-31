// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// Silent session renewal against PostgreSQL and a stand-in identity provider
// (#937, ADR-048 sections 4 and 4a).
//
// What is proven here and cannot be proven with the auth package's fake store:
//
//  1. TWO PROCESSES, ONE USE. Two separate Authenticators (two "processes")
//     share one database and renew the same stale session concurrently against a
//     token endpoint that, like Authentik, refuses any refresh token presented a
//     second time. The session stays alive and the endpoint sees exactly one use.
//     The per-session advisory lock is what makes this pass: remove it and the
//     second process presents the old token.
//  2. THE SEALED ROUND TRIP. The rotated refresh token is sealed by the real
//     adapter, stored, and opens to the new value.
//  3. THE REAL RESOLVER. Renewal runs provisioningResolver, so removing the
//     admin group at the provider flips app.users.role to user at the next
//     renewal, and putting it back flips it again (the positive control: a
//     role that never changes proves nothing).

import (
	"context"
	stdcrypto "crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/crypto"
	"mailroom/internal/store"
)

const (
	idpClientID = "renew-client"
	idpSecret   = "renew-client-secret"
)

// fakeIDP is a minimal OIDC provider: discovery, JWKS and a token endpoint that
// rotates refresh tokens and refuses a second presentation of any of them.
type fakeIDP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu         sync.Mutex
	valid      map[string]bool // refresh tokens that may still be presented
	seen       map[string]bool // refresh tokens ever presented
	uses       int             // successful refreshes
	violations int             // a token presented twice
	counter    int
	delay      time.Duration
	subject    string
	email      string
	groups     []string
	// idTokenNoGroups leaves the groups claim out of the ID token (UserInfo
	// still reports p.groups): a provider whose scope mapping puts groups only
	// on UserInfo, which is the case a half-checked renewal would demote.
	idTokenNoGroups bool
	// userinfoFail makes the UserInfo endpoint answer 503.
	userinfoFail bool
	// withRevocation publishes an RFC 7009 revocation_endpoint in discovery (set
	// BEFORE an Authenticator is built); revoked records the tokens it was asked
	// to revoke, revokeStatus is its answer (200 when 0).
	withRevocation bool
	revoked        []string
	revokeStatus   int
}

func newFakeIDP(t *testing.T, subject, email, firstRefresh string) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIDP{t: t, key: key, valid: map[string]bool{firstRefresh: true}, seen: map[string]bool{},
		subject: subject, email: email}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"userinfo_endpoint":                     p.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		p.mu.Lock()
		if p.withRevocation {
			doc["revocation_endpoint"] = p.srv.URL + "/revoke"
		}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		defer p.mu.Unlock()
		if user, pass, ok := r.BasicAuth(); !ok || user != idpClientID || pass != idpSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.PostFormValue("token_type_hint") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.revoked = append(p.revoked, r.PostFormValue("token"))
		if p.revokeStatus != 0 {
			w.WriteHeader(p.revokeStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()),
			"e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.userinfoFail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		body := map[string]any{"sub": p.subject, "email": p.email, "name": "Renew Person"}
		if p.groups != nil {
			body["groups"] = p.groups
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (p *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	presented := r.PostFormValue("refresh_token")
	w.Header().Set("Content-Type", "application/json")

	p.mu.Lock()
	delay := p.delay
	p.mu.Unlock()
	time.Sleep(delay) // widen the window in which a second process could race

	p.mu.Lock()
	defer p.mu.Unlock()
	if user, pass, ok := r.BasicAuth(); !ok || user != idpClientID || pass != idpSecret {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
		return
	}
	if p.seen[presented] {
		p.violations++
	}
	p.seen[presented] = true
	if !p.valid[presented] {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "token already used"})
		return
	}
	delete(p.valid, presented)
	p.counter++
	next := "rt-" + strconv.Itoa(p.counter)
	p.valid[next] = true
	p.uses++

	claims := map[string]any{
		"iss": p.srv.URL, "sub": p.subject, "aud": []string{idpClientID},
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": p.email, "email_verified": true, "name": "Renew Person",
	}
	if p.groups != nil && !p.idTokenNoGroups {
		claims["groups"] = p.groups
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access-" + next, "token_type": "Bearer", "expires_in": 3600,
		"refresh_token": next, "id_token": p.sign(claims),
	})
}

func (p *fakeIDP) sign(claims map[string]any) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(claims)
	in := b64(hdr) + "." + b64(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, stdcrypto.SHA256, sum[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func (p *fakeIDP) setGroups(g []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.groups = g
}

func (p *fakeIDP) set(f func(p *fakeIDP)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *fakeIDP) stats() (uses, violations int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.uses, p.violations
}

// renewRig is a user with a session, a stand-in provider, and a way to make as
// many separate Authenticators ("processes") as a test needs.
type renewRig struct {
	t      *testing.T
	h      *signerHarness
	idp    *fakeIDP
	keys   crypto.KeyProvider
	userID ulid.ULID
	subj   string
	secret string
	// st is the store the processes use (a test may swap in a small pool).
	st *store.Store
}

func newRenewRig(t *testing.T, admin bool, groups []string) *renewRig {
	t.Helper()
	h := newSignerHarness(t)
	ctx := context.Background()
	uniq := store.NewULID().String()
	subject, email := "sub-"+uniq, uniq+"@example.com"
	userID, _, ok, err := h.store.ResolveLogin(ctx, subject, email, "Renew Person", admin)
	if err != nil || !ok {
		t.Fatalf("seed user: ok=%v err=%v", ok, err)
	}

	keyFile := filepath.Join(t.TempDir(), "master.key")
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, master, 0o600); err != nil {
		t.Fatal(err)
	}
	rig := &renewRig{t: t, h: h, idp: newFakeIDP(t, subject, email, "rt-start"),
		keys: crypto.NewFileKeyProvider(keyFile), userID: userID, subj: subject}
	rig.idp.groups = groups

	rig.st = h.store
	rig.secret = rig.addSession("rt-start")
	return rig
}

// addSession creates a session for the rig's user holding refresh, through the
// real adapter (so the tokens are sealed), and returns its cookie secret.
func (r *renewRig) addSession(refresh string) string {
	r.t.Helper()
	// The cookie shape Middleware accepts: "s1." + 256 random bits in lowercase
	// unpadded base32.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		r.t.Fatal(err)
	}
	secret := "s1." + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	if _, err := newSessionStore(r.h.store, r.keys).Create(context.Background(), auth.NewSessionInput{
		UserID: r.userID, TokenHash: sha256.Sum256([]byte(secret)), RefreshToken: refresh,
		IDToken: "initial-id-token", UserAgent: "renew-test", Addr: "192.0.2.1",
	}); err != nil {
		r.t.Fatalf("create session: %v", err)
	}
	r.idp.set(func(p *fakeIDP) { p.valid[refresh] = true })
	return secret
}

// process builds one Authenticator, as one running server would.
func (r *renewRig) process() *auth.Authenticator { return r.processWith(nil) }

// processWith is process with a chance to adjust the Config first.
func (r *renewRig) processWith(adjust func(*auth.Config)) *auth.Authenticator {
	r.t.Helper()
	cfg := auth.Config{
		IssuerURL: r.idp.srv.URL, ClientID: idpClientID, ClientSecret: idpSecret,
		RedirectURL:  "https://mail.example.com/auth/callback",
		SigningKey:   []byte("0123456789abcdef0123456789abcdef"),
		SessionStore: newSessionStore(r.st, r.keys),
		CookieSecure: true,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	a, err := auth.New(context.Background(), cfg,
		provisioningResolver(r.st, "", quietLogger()), auth.WithLogger(quietLogger()))
	if err != nil {
		r.t.Fatalf("auth.New: %v", err)
	}
	return a
}

// request drives one request through a's Middleware with the session cookie.
func (r *renewRig) request(a *auth.Authenticator) int { return r.requestWith(a, r.secret) }

// requestWith is request for the session with the given cookie secret.
func (r *renewRig) requestWith(a *auth.Authenticator, secret string) int {
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: secret})
	rr := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rr, req)
	return rr.Code
}

// age makes the session's last confirmation d old.
func (r *renewRig) age(d time.Duration) {
	r.t.Helper()
	secretHash := sha256.Sum256([]byte(r.secret))
	if _, err := r.h.super.Exec(context.Background(),
		`UPDATE app.sessions SET last_checked_at = now() - make_interval(secs => $2) WHERE token_hash = $1`,
		secretHash[:], d.Seconds()); err != nil {
		r.t.Fatalf("age session: %v", err)
	}
}

type sessionRow struct {
	revoked    *string
	generation int64
	checkedAgo time.Duration
	refreshCT  []byte
	id         ulid.ULID
}

func (r *renewRig) row() sessionRow {
	r.t.Helper()
	secretHash := sha256.Sum256([]byte(r.secret))
	var s sessionRow
	var id string
	var ago float64
	if err := r.h.super.QueryRow(context.Background(), `
		SELECT id, revoked_reason, generation, extract(epoch FROM now() - last_checked_at), refresh_ct
		  FROM app.sessions WHERE token_hash = $1`, secretHash[:],
	).Scan(&id, &s.revoked, &s.generation, &ago, &s.refreshCT); err != nil {
		r.t.Fatalf("read session: %v", err)
	}
	s.checkedAgo = time.Duration(ago * float64(time.Second))
	s.id = ulid.MustParse(id)
	return s
}

func (r *renewRig) role() string {
	r.t.Helper()
	var role string
	if err := r.h.super.QueryRow(context.Background(),
		`SELECT role::text FROM app.users WHERE id = $1`, r.userID.String()).Scan(&role); err != nil {
		r.t.Fatalf("read role: %v", err)
	}
	return role
}

func TestSessionRenewal_TwoProcessesPresentTheRefreshTokenOnce(t *testing.T) {
	rig := newRenewRig(t, false, nil)
	rig.idp.delay = 400 * time.Millisecond
	rig.age(time.Hour)
	procA, procB := rig.process(), rig.process()

	const perProcess = 6
	codes := make(chan int, 2*perProcess)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, p := range []*auth.Authenticator{procA, procB} {
		for i := 0; i < perProcess; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes <- rig.request(p)
			}()
		}
	}
	close(start)
	wg.Wait()
	close(codes)

	for c := range codes {
		if c != http.StatusOK {
			t.Errorf("a request answered %d, want 200: the session must survive a concurrent renewal", c)
		}
	}
	uses, violations := rig.idp.stats()
	// A second process queued behind the lock may find the session not yet
	// confirmed (the first is still checking identity) and renew once more, but
	// with the ROTATED token: never a token presented twice.
	if violations != 0 || uses < 1 || uses > 2 {
		t.Fatalf("the token endpoint saw %d successful uses and %d re-presentations of a used token; want 1 or 2 and 0", uses, violations)
	}
	row := rig.row()
	if row.revoked != nil {
		t.Fatalf("the session was revoked as %q", *row.revoked)
	}
	if row.generation != int64(uses) {
		t.Errorf("generation = %d, want %d (one rotation per use)", row.generation, uses)
	}
	if row.checkedAgo > time.Minute {
		t.Errorf("last_checked_at is %v old after a renewal", row.checkedAgo)
	}

	// The sealed round trip: the stored refresh token opens to the rotated one.
	master, err := rig.keys.MasterKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := openSessionToken(master, rig.userID, row.id, sessionPurposeRefresh, row.refreshCT)
	if want := "rt-" + strconv.Itoa(uses); err != nil || got != want {
		t.Fatalf("stored refresh token = %q, %v; want the latest rotation %s", got, err, want)
	}
}

func TestSessionRenewal_ARefusedTokenRevokesTheSession(t *testing.T) {
	rig := newRenewRig(t, false, nil)
	// The provider no longer knows the token the session holds.
	rig.idp.mu.Lock()
	rig.idp.valid = map[string]bool{}
	rig.idp.mu.Unlock()
	rig.age(time.Hour)

	if code := rig.request(rig.process()); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if row := rig.row(); row.revoked == nil || *row.revoked != "refused" {
		t.Fatalf("revoked_reason = %v, want refused", row.revoked)
	}
}

func TestSessionRenewal_GroupChangeAtTheProviderFlipsTheRole(t *testing.T) {
	rig := newRenewRig(t, false, []string{adminGroup})
	proc := rig.process()

	// Positive control: a user with the admin group is promoted by a renewal.
	if rig.role() != "user" {
		t.Fatalf("seeded role = %q, want user", rig.role())
	}
	rig.age(time.Hour)
	if code := rig.request(proc); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if got := rig.role(); got != "admin" {
		t.Fatalf("role after a renewal with the admin group = %q, want admin", got)
	}

	// The group is removed at the provider: the next renewal demotes.
	rig.idp.setGroups([]string{})
	rig.age(time.Hour)
	if code := rig.request(proc); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if got := rig.role(); got != "user" {
		t.Fatalf("role after the admin group was removed = %q, want user", got)
	}
	if uses, violations := rig.idp.stats(); uses != 2 || violations != 0 {
		t.Fatalf("provider saw %d uses, %d violations; want 2 and 0", uses, violations)
	}
}
