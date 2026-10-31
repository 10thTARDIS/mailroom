// SPDX-License-Identifier: AGPL-3.0-only

package main

// The login gate.
//
// This file exists to answer ONE question, over the REAL mux, for BOTH auth
// paths: what does an UNAUTHENTICATED request get?
//
// The route inventory (main.go's routes()) is the source of truth for both the
// server and this test. Every route declares a gate — gateOpen / gateAPI /
// gateBrowser — and at least one concrete probe. TestGate drives every probe of
// every route through the whole composed surface (internal/server's probe mux in
// front of cmd/mailroom's app mux, exactly as main() wires it) and asserts the
// behavior its gate promises. Adding a route without choosing a side of the gate,
// or without a probe, fails here — a route cannot quietly default to "public".
//
// The two PROTECTED leaf handlers (JMAP, the web client) are replaced with
// sentinels that fail the test the instant they are invoked. That is a stronger
// claim than a status-code assertion — "an anonymous request reached NOTHING",
// not "an anonymous request got a 401 from something" — and it is what lets the
// dev-auth stack be tested at all: there, nothing rejects the request, so the
// leaf genuinely runs (and the test asserts THAT, loudly, because the whole point
// of the escape hatch is that it hands everything over).
//
// No database, no network, no provider: OIDC discovery is served by a local
// httptest server. The end-to-end proof against a real Postgres and a real OIDC
// provider lives in scripts/gate-e2e.sh.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	"mailroom/internal/keypub"
	"mailroom/internal/metrics"
	"mailroom/internal/server"
	"mailroom/internal/wkd"
)

// Markers the sentinel leaf handlers write. If either string ever appears in a
// response to an unauthenticated request under OIDC, the gate leaked.
const (
	jmapMarker = "SENTINEL-JMAP-PAYLOAD"
	webMarker  = "SENTINEL-APP-SHELL"
)

func quietLogger() *slog.Logger { return server.NewLoggerWriter("json", "error", io.Discard) }

// discoveryServer stands in for the identity provider's discovery document, so
// auth.New (which really does perform OIDC discovery) works with no network.
func discoveryServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/authorize",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func oidcTestConfig(t *testing.T) *config.Config {
	t.Helper()
	issuer := discoveryServer(t).URL
	return &config.Config{
		// WKD must be ENABLED here, or the public routes answer 404 for a
		// reason that has nothing to do with the gate and the gatePublic
		// assertions below prove nothing. This was the bug: mountApp built its
		// own disabled service and every probe 404'd.
		WKDDomain:        "example.com",
		OIDCIssuer:       issuer,
		OIDCClientID:     "mailroom",
		OIDCClientSecret: "test-secret",
		// Plain http on purpose: this is the local/dev shape, and it is the one
		// where CookieSecure must be false or the browser drops the cookie.
		OIDCRedirectURL: "http://localhost:8080/auth/callback",
		SessionKey:      []byte(strings.Repeat("k", config.MinSessionKeyLen)),
		OperatorSubject: "operator-sub",
		OperatorEmail:   "operator@example.com",
	}
}

func devAuthTestConfig() *config.Config {
	return &config.Config{
		DevAuthSubject: "dev-operator", DevAuthEmail: "dev@example.com",
		WKDDomain: "example.com",
	}
}

// gateHandler builds the whole composed surface the server actually serves: the
// unauthenticated probes (internal/server) in front of the app mux (mountApp —
// the real gate, the real middleware, the real route inventory), with sentinels
// where the protected handlers go.
func gateHandler(t *testing.T, cfg *config.Config) (http.Handler, []route) {
	t.Helper()
	operatorID := ulid.Make()
	logger := quietLogger()

	jmap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.UserIDFromContext(r.Context()); !ok {
			t.Errorf("JMAP handler reached with NO user id in context: %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, jmapMarker)
	})
	web := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.UserIDFromContext(r.Context()); !ok {
			t.Errorf("web handler reached with NO user id in context: %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, webMarker)
	})

	app, err := mountApp(context.Background(), nil, cfg, operatorID, logger, jmap, web, web, gateTestKeys{}, nil)
	if err != nil {
		t.Fatalf("mountApp: %v", err)
	}

	open, _, _, err := gateRoutesForTest(t, cfg, operatorID, logger)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}

	// The PUBLIC key surface, built the same way mountApp builds it — but with a
	// lookup that would SERVE A KEY if it were reached with a published hash.
	//
	// A stub returning "not found" would make every probe below pass for the
	// wrong reason: 404 because nothing is published is indistinguishable from
	// 404 because the route is broken. This one hands back a key for exactly one
	// hash, so the gate test can tell "the route works and published keys are
	// public" apart from "the route answers nothing to anybody".
	keyPub, err := keypub.New(gateTestKeys{}, "example.com", logger)
	if err != nil {
		t.Fatalf("keypub: %v", err)
	}

	ready := func(context.Context) error { return nil }
	srv := server.New(":0", logger, metrics.New().Handler(), ready, server.WithAppHandler(app))
	return srv.Handler(), routes(jmap, web, web, open, keyPub)
}

// gateTestFingerprint is a realistic 40-hex-character OpenPGP fingerprint. The
// length matters: the key page groups it for reading aloud, so a short stub
// would exercise a formatting path no real key takes.
const gateTestFingerprint = "DEADBEEF0123456789ABCDEF0123456789ABCDEF"

// gateTestKeys serves one published key, for the hash of "joe.doe".
type gateTestKeys struct{}

// LookupWKDKeyPage satisfies keypub.PageLookup.
//
// ⚠ Implementing only keypub.Lookup made the key page 404 — the handler
// type-asserts for PageLookup and FAILS OPEN to a generic 404 when the
// assertion misses. That is invisible: a mis-wired page is indistinguishable
// from an unpublished key, which is the same class of "quietly serves nobody"
// bug that made this whole probe set vacuous.
func (gateTestKeys) LookupWKDKeyPage(_ context.Context, domain, hash string) (string, string, bool, error) {
	if domain == "example.com" && hash == wkd.HashLocalPart("joe.doe") {
		return "joe.doe@example.com", gateTestFingerprint, true, nil
	}
	return "", "", false, nil
}

func (gateTestKeys) LookupWKDKey(_ context.Context, domain, hash string) ([]byte, string, bool, error) {
	if domain == "example.com" && hash == wkd.HashLocalPart("joe.doe") {
		// Binary, as WKD requires — armor here would be a bug the real handler
		// refuses to serve.
		return []byte{0x99, 0x01, 0x02, 0x03}, gateTestFingerprint, true, nil
	}
	return nil, "", false, nil
}

// gateRoutesForTest rebuilds the auth path's open routes so the test can see the
// same inventory mountApp registered.
func gateRoutesForTest(t *testing.T, cfg *config.Config, operatorID ulid.ULID, logger *slog.Logger) ([]route, func(http.Handler) http.Handler, func(http.Handler) http.Handler, error) {
	t.Helper()
	if cfg.OIDCConfigured() {
		return oidcGate(context.Background(), nil, cfg, logger, nil)
	}
	return devAuthGate(cfg, operatorID, logger)
}

// do drives one probe. It never sends a cookie, a header, or a credential of any
// kind: this is a stranger with a URL.
func do(t *testing.T, h http.Handler, p probe) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(p.method, p.url, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessionCookie returns the session cookie the response set, if any. NOTHING an
// unauthenticated request touches may ever mint one.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.DefaultSessionCookieName && c.Value != "" {
			return c
		}
	}
	return nil
}

// TestGateOIDC is the gate: every route, every probe, no session.
func TestGateOIDC(t *testing.T) {
	h, rs := gateHandler(t, oidcTestConfig(t))

	for _, rt := range rs {
		if len(rt.probes) == 0 {
			t.Errorf("route %q declares no probe — say what an UNAUTHENTICATED request gets from it", rt.pattern)
			continue
		}
		for _, p := range rt.probes {
			t.Run(p.method+" "+p.url, func(t *testing.T) {
				rec := do(t, h, p)
				body := rec.Body.String()

				// Universal, whatever the gate: an anonymous request never gets
				// a session and never gets the app shell or mail data. (The
				// sentinels also fail the test from inside if they are reached.)
				if c := sessionCookie(rec); c != nil {
					t.Errorf("response set a session cookie for an unauthenticated request")
				}
				if strings.Contains(body, webMarker) || strings.Contains(body, jmapMarker) {
					t.Errorf("a protected handler served an unauthenticated request; body = %q", body)
				}

				switch rt.gate {
				case gateAPI:
					// 401, NOT a redirect: a 302 is meaningless to a
					// programmatic client (spec §10).
					if rec.Code != http.StatusUnauthorized {
						t.Errorf("status = %d, want 401 (JMAP must not redirect)", rec.Code)
					}
					if loc := rec.Header().Get("Location"); loc != "" {
						t.Errorf("Location = %q, want none — JMAP answers 401, it does not redirect", loc)
					}
				case gateBrowser:
					// 302 to the sign-in page. Never the app shell.
					if rec.Code != http.StatusFound {
						t.Errorf("status = %d, want 302 to the sign-in page", rec.Code)
					}
					if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, loginPath) {
						t.Errorf("Location = %q, want a redirect to %s", loc, loginPath)
					}
				case gateOpen:
					// These handlers ARE the gate. They must answer without a
					// session (checked above: no session cookie is minted, and
					// no protected handler ran). A 401 here would mean the
					// sign-in page itself needs a session — a redirect loop.
					if rec.Code == http.StatusUnauthorized {
						t.Errorf("status = 401 on an open route — the sign-in path must not require a session")
					}
				case gatePublic:
					// Public BY DESIGN, and reached by machines: a mail client
					// doing a WKD lookup follows no redirects and understands no
					// login page, so either answer would silently unpublish
					// every key on this instance.
					if rec.Code == http.StatusUnauthorized {
						t.Errorf("status = 401 on the public key surface — no correspondent can authenticate")
					}
					if loc := rec.Header().Get("Location"); loc != "" {
						t.Errorf("Location = %q — a key lookup must not redirect", loc)
					}
					// gatePublic exists precisely to apply the hardening headers
					// that gateOpen does not. If this is absent, the route was
					// registered on the wrong gate and nothing else would say so.
					if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
						t.Errorf("X-Content-Type-Options = %q, want nosniff — is this route on gatePublic?", got)
					}
				}
			})
		}
	}
}

// The dev-auth escape hatch builds a DIFFERENT handler stack, and this is what
// it does: nothing is gated, every request is the operator, and the SPA is
// handed to whoever asks. That is not a bug — it is the documented, deliberately
// awkward escape hatch (auth.DevAuthWarning) — but it must be TRUE and VISIBLE,
// not assumed. If this test ever starts failing because dev-auth began rejecting
// something, someone has confused the escape hatch for a gate.
func TestGateDevAuthIsWideOpen(t *testing.T) {
	h, rs := gateHandler(t, devAuthTestConfig())

	for _, rt := range rs {
		for _, p := range rt.probes {
			t.Run(p.method+" "+p.url, func(t *testing.T) {
				rec := do(t, h, p)
				if rec.Code == http.StatusUnauthorized {
					t.Errorf("status = 401 under dev-auth, which authenticates nobody")
				}
				if loc := rec.Header().Get("Location"); strings.HasPrefix(loc, loginPath) {
					t.Errorf("redirected to the sign-in page under dev-auth, which has no sign-in")
				}
			})
		}
	}

	// The escape hatch, stated as an assertion: an anonymous stranger gets the
	// app shell and the mail API, with the operator's identity injected.
	if got := do(t, h, get("/")).Body.String(); !strings.Contains(got, webMarker) {
		t.Errorf("dev-auth did not serve the app shell to an anonymous request; body = %q", got)
	}
	if got := do(t, h, get("/jmap/session")).Body.String(); !strings.Contains(got, jmapMarker) {
		t.Errorf("dev-auth did not serve JMAP to an anonymous request; body = %q", got)
	}

	// ...and it says so, on the page, not just in a log line nobody reads.
	rec := do(t, h, get(loginPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200", loginPath, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no authentication") {
		t.Errorf("the dev-auth sign-in page does not say the instance authenticates nobody")
	}
}

// Every JMAP route the API serves must be in the inventory, on the 401 side.
// internal/api owns that list; if it grows a route, this fails until cmd/mailroom
// gates it — which is exactly the moment to decide whether it belongs behind the
// gate at all.
func TestEveryJMAPPathIsGated(t *testing.T) {
	_, rs := gateHandler(t, oidcTestConfig(t))

	gated := map[string]gate{}
	for _, rt := range rs {
		gated[rt.pattern] = rt.gate
	}
	for _, p := range api.JMAPPaths() {
		g, ok := gated[p]
		if !ok {
			t.Errorf("JMAP route %q is not in the app mux's route inventory", p)
			continue
		}
		if g != gateAPI {
			t.Errorf("JMAP route %q is gated %v, want gateAPI (401, never a redirect)", p, g)
		}
	}
}

// The probes must stay in front of the gate: a healthcheck and a Prometheus
// scraper carry no session. And /metrics must not be a side channel — it carries
// no user id, no subject, no email address.
func TestProbesAreOpenAndCarryNoIdentity(t *testing.T) {
	cfg := oidcTestConfig(t)
	h, _ := gateHandler(t, cfg)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rec := do(t, h, get(path))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 — probes must not need a session", path, rec.Code)
		}
	}

	body := do(t, h, get("/metrics")).Body.String()
	for _, secret := range []string{cfg.OperatorSubject, cfg.OperatorEmail, "@"} {
		if strings.Contains(body, secret) {
			t.Errorf("/metrics leaked %q:\n%s", secret, body)
		}
	}
}

// --- The sign-in page --------------------------------------------------------

func TestSignInPageIsServedToAnonymousVisitors(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))

	rec := do(t, h, get(loginPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Mailroom", "Sign in", startPath} {
		if !strings.Contains(body, want) {
			t.Errorf("sign-in page does not contain %q", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// The page is one self-contained document: it must not send the anonymous
	// visitor off to fetch anything, because there is nothing outside the gate
	// for them to fetch.
	if strings.Contains(body, `src="/assets`) || strings.Contains(body, `href="/assets`) {
		t.Errorf("the sign-in page references a bundle asset — those are all behind the gate")
	}
}

// An unauthenticated browser is bounced to the sign-in page carrying where it
// was going, and the sign-in page's own action carries it onward — so a deep
// link survives the round trip through the provider.
func TestBrowserRedirectPreservesDestination(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))

	rec := do(t, h, get("/inbox/thread-1"))
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.HasPrefix(loc, loginPath+"?") {
		t.Fatalf("GET /inbox/thread-1 = %d %q, want a 302 to the sign-in page", rec.Code, loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if got := u.Query().Get("return_to"); got != "/inbox/thread-1" {
		t.Errorf("return_to = %q, want /inbox/thread-1", got)
	}

	page := do(t, h, get(loc)).Body.String()
	if !strings.Contains(page, startPath+"?return_to=%2Finbox%2Fthread-1") {
		t.Errorf("the sign-in page's action does not carry return_to onward:\n%s", page)
	}
}

// The whole reason /auth/login is a PAGE and /auth/start is the flow: a refused
// user must land somewhere that tells them what happened. If /auth/login itself
// bounced to the provider, a subject the operator allowlist rejects would loop
// — provider → callback → refused → provider → ... — and never read a word of it.
func TestSignInPageIsNotARedirectToTheProvider(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))

	rec := do(t, h, get(loginPath))
	if rec.Code == http.StatusFound {
		t.Fatalf("%s redirected (to %q) instead of rendering the sign-in page — that is the redirect loop",
			loginPath, rec.Header().Get("Location"))
	}

	// The flow itself lives on its own route, and THAT is what goes to the
	// provider (with PKCE and a login-state cookie).
	start := do(t, h, get(startPath))
	if start.Code != http.StatusFound {
		t.Fatalf("%s: status = %d, want a 302 to the provider", startPath, start.Code)
	}
	loc := start.Header().Get("Location")
	if !strings.Contains(loc, "/authorize") || !strings.Contains(loc, "code_challenge") {
		t.Errorf("%s: Location = %q, want the provider's authorization endpoint with PKCE", startPath, loc)
	}
	if sessionCookie(start) != nil {
		t.Errorf("starting a login minted a session cookie")
	}
}

// A refused login lands on the sign-in page with a reason — and no session. The
// callback with no login state is the cheapest way to reach a failure path
// without a provider; the full "wrong subject is refused" proof needs a real
// provider and lives in scripts/gate-e2e.sh.
func TestCallbackFailureLandsOnTheSignInPageWithNoSession(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))

	rec := do(t, h, get("/auth/callback?code=x&state=y"))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want a 302 back to the sign-in page", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, loginPath+"?") {
		t.Fatalf("Location = %q, want the sign-in page", loc)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a failed callback minted a session cookie")
	}

	// ...and the page it lands on actually says something.
	page := do(t, h, get(loc)).Body.String()
	if !strings.Contains(page, "expired") {
		t.Errorf("the sign-in page said nothing about the failed login:\n%s", page)
	}
}

// The refusal the operator allowlist produces, rendered. This is the sentence a
// stranger with a valid token from the same issuer sees.
func TestDeniedLoginIsExplained(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))

	page := do(t, h, get(loginPath+"?error="+auth.ErrorDenied)).Body.String()
	if !strings.Contains(page, "cannot use this Mailroom") {
		t.Errorf("a denied login is not explained on the sign-in page:\n%s", page)
	}
	if !strings.Contains(page, `role="alert"`) {
		t.Errorf("the denial is not announced to a screen reader (no role=alert)")
	}
}

// Signing out lands on a page that says so, rather than on "/" — which is behind
// the gate and would only bounce here anyway.
func TestLogoutClearsTheSessionAndLandsOnTheSignInPage(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))

	rec := do(t, h, get(logoutPath))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, loginPath) {
		t.Errorf("Location = %q, want the sign-in page", loc)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.DefaultSessionCookieName && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout did not clear the session cookie")
	}
}

// buildAppHandler is what main() actually calls. The gate tests above drive
// mountApp (same gate, sentinel leaves); this proves the production constructor
// wires the same thing, so the sentinels are not testing a fiction.
func TestBuildAppHandlerGatesTheRealHandlers(t *testing.T) {
	cfg := oidcTestConfig(t)
	cfg.WebRoot = t.TempDir() // an empty web root: the route exists, the files do not

	h, _, err := buildAppHandler(context.Background(), nil, cfg, ulid.Make(), nil, nil, nil, nil, nil, nil, oauthWiring{}, nil, quietLogger())
	if err != nil {
		t.Fatalf("buildAppHandler: %v", err)
	}

	// A nil store is safe here precisely BECAUSE the gate holds: no request
	// below reaches a handler that would touch it. If one ever did, this test
	// would panic — which is a perfectly good way to find out.
	for _, p := range []probe{
		get("/jmap/session"), post("/jmap"), get("/.well-known/jmap"),
		get("/jmap/download/01J0000000000000000000000/blob-1/x.pdf"),
		post("/jmap/upload/01J0000000000000000000000/"),
		get("/jmap/eventsource/"),
	} {
		if rec := do(t, h, p); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", p.method, p.url, rec.Code)
		}
	}
	for _, p := range []probe{get("/"), get("/index.html"), get("/assets/index-abc.js")} {
		rec := do(t, h, p)
		if rec.Code != http.StatusFound {
			t.Errorf("%s: status = %d, want 302 to the sign-in page", p.url, rec.Code)
		}
	}
	if rec := do(t, h, get(loginPath)); rec.Code != http.StatusOK {
		t.Errorf("%s: status = %d, want the sign-in page", loginPath, rec.Code)
	}
}

// TestPublicKeySurfaceActuallyServes is the assertion whose absence made the
// whole gatePublic probe set worthless.
//
// TestGateOIDC checks what an anonymous caller does NOT get. Every one of those
// checks is satisfied by a 404, so a public surface that served nobody anything
// passed all of them — which is exactly what happened: mountApp built its own
// keypub service with an empty domain, the test's lookup was attached to the
// route inventory and never reached, and all nine probes 404'd while the suite
// stayed green.
//
// So this test asserts the POSITIVE half through the real mux: a published key
// comes back, with the content type WKD requires, to a caller with no session.
func TestPublicKeySurfaceActuallyServes(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))

	t.Run("a published key is served to an anonymous caller", func(t *testing.T) {
		rec := do(t, h, get("/.well-known/openpgpkey/hu/"+wkd.HashLocalPart("joe.doe")))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the public key surface is not reachable through the "+
				"real mux, and every gatePublic probe is therefore vacuous", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", ct)
		}
		if rec.Body.Len() == 0 {
			t.Error("200 with an empty body")
		}
		if strings.Contains(rec.Body.String(), "-----BEGIN PGP") {
			t.Error("WKD served ASCII armor; no mail client will accept it")
		}
	})

	t.Run("the policy file is served", func(t *testing.T) {
		if rec := do(t, h, get("/.well-known/openpgpkey/policy")); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 — without the policy file some clients do not "+
				"consider this a WKD domain at all", rec.Code)
		}
	})

	t.Run("the key page renders for a published key", func(t *testing.T) {
		rec := do(t, h, get("/key/"+wkd.HashLocalPart("joe.doe")))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		// The page groups the fingerprint for reading aloud, so assert the
		// GROUPED form — asserting the raw 40 characters would fail against a
		// correctly formatted page, and asserting a 4-character fragment would
		// pass against almost anything.
		if !strings.Contains(rec.Body.String(), "DEAD BEEF 0123 4567 89AB") {
			t.Error("the page did not render the grouped fingerprint it was given")
		}
		if !strings.Contains(rec.Body.String(), "joe.doe@example.com") {
			t.Error("the page did not render the address the key belongs to")
		}
	})

	// ⚠ And the control that keeps the three above honest: an UNPUBLISHED hash
	// must still 404. Without this, a handler that answered 200 to everything
	// would pass all of them.
	t.Run("an unpublished key still 404s", func(t *testing.T) {
		for _, p := range []string{
			"/.well-known/openpgpkey/hu/" + wkd.HashLocalPart("nobody-here"),
			"/key/" + wkd.HashLocalPart("nobody-here"),
		} {
			if rec := do(t, h, get(p)); rec.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404", p, rec.Code)
			}
		}
	})
}

// A bearer token is decided by the bearer gate on EVERY API route, under BOTH
// auth paths (#49, ADR-042 §2). The decisive case is dev-auth: it lets every
// anonymous request through, so if the bearer gate were not mounted in front of
// it an unknown key would reach the JMAP sentinel instead of a 401. The store is
// nil here, so every token is unknown — fail closed.
//
// A browser route never accepts a key: there the header is not the gate's
// business, and the request takes the browser path it would have taken anyway.
func TestBearerTokensAreDecidedOnEveryAPIRoute(t *testing.T) {
	unknown, _, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	withBearer := func(t *testing.T, h http.Handler, p probe) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(p.method, p.url, nil)
		req.Header.Set("Authorization", "Bearer "+unknown)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for name, cfg := range map[string]*config.Config{"oidc": oidcTestConfig(t), "dev-auth": devAuthTestConfig()} {
		t.Run(name, func(t *testing.T) {
			h, rs := gateHandler(t, cfg)
			for _, rt := range rs {
				if rt.gate != gateAPI {
					continue
				}
				for _, p := range rt.probes {
					rec := withBearer(t, h, p)
					if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), jmapMarker) {
						t.Errorf("%s %s with an unknown key: status %d body %q — want 401 and the handler untouched",
							p.method, p.url, rec.Code, rec.Body.String())
					}
					if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="mailroom"` {
						t.Errorf("%s %s: WWW-Authenticate = %q", p.method, p.url, got)
					}
				}
			}
			// The browser shell ignores the header: under OIDC it still bounces
			// to sign-in, under dev-auth it still serves the shell.
			rec := withBearer(t, h, get("/"))
			if name == "oidc" && rec.Code != http.StatusFound {
				t.Errorf("GET / with a bearer under OIDC: status %d, want the sign-in redirect", rec.Code)
			}
			if name == "dev-auth" && !strings.Contains(rec.Body.String(), webMarker) {
				t.Errorf("GET / with a bearer under dev-auth: body %q, want the app shell", rec.Body.String())
			}
		})
	}
}
