// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// logoutAuthenticator builds an Authenticator wired for RP-initiated logout
// (#314): a discovered end_session_endpoint and a client id to name in the
// request. endSession may be "" to model a provider that publishes none.
func logoutAuthenticator(t *testing.T, endSession, postLogoutURI string) *Authenticator {
	t.Helper()
	cfg := Config{
		SigningKey:            testSigningKey(),
		ClientID:              "mailroom-test-client",
		PostLogoutRedirectURI: postLogoutURI,
		SessionStore:          newFakeSessions(),
	}
	return &Authenticator{
		cfg:                cfg.withDefaults(),
		endSessionEndpoint: endSession,
		postLogoutRedirect: "/signin?notice=signed_out",
	}
}

// doLogout runs LogoutHandler for a request carrying cookie (which may be nil)
// and returns the Location it redirected to plus the cookies it set.
func doLogout(t *testing.T, a *Authenticator, cookie *http.Cookie) (string, []*http.Cookie) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.LogoutHandler(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want %d", w.Code, http.StatusFound)
	}
	return w.Header().Get("Location"), w.Result().Cookies()
}

// TestLogoutRedirectsToProviderCarryingIDTokenHint is the assertion #314 asks
// for by name, now sourced from the session row instead of a cookie.
//
// ⚠ IT DELIBERATELY DOES NOT MERELY CHECK "we redirected to the provider".
// RP-Initiated Logout without id_token_hint is ignored or downgraded to an
// "are you sure?" interstitial by most providers, so a logout that reaches
// end_session_endpoint empty-handed LOOKS like single sign-out while ending
// nothing. The hint's presence and VALUE are what is asserted, along with the
// server-side revocation that precedes the redirect.
func TestLogoutRedirectsToProviderCarryingIDTokenHint(t *testing.T) {
	const endSession = "https://idp.example.com/application/o/mailroom/end-session/"
	const postLogout = "https://mail.example.com/signin"
	const rawIDToken = "header.payload.signature-stand-in"

	a := logoutAuthenticator(t, endSession, postLogout)
	c := signIn(t, a, ulid.Make(), rawIDToken)
	loc, _ := doLogout(t, a, c)

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location %q does not parse: %v", loc, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != endSession {
		t.Fatalf("redirected to %q, want the provider's end_session_endpoint %q", got, endSession)
	}

	q := u.Query()
	if got := q.Get("id_token_hint"); got != rawIDToken {
		t.Fatalf("id_token_hint = %q, want the ID token stored on the session (%q)", got, rawIDToken)
	}
	if got := q.Get("client_id"); got != "mailroom-test-client" {
		t.Fatalf("client_id = %q, want the configured client id", got)
	}
	if got := q.Get("post_logout_redirect_uri"); got != postLogout {
		t.Fatalf("post_logout_redirect_uri = %q, want %q", got, postLogout)
	}

	// Revoked server-side, so the cookie is dead even if a copy survives.
	if got := fakeOf(a).rowFor(c.Value).revokedAs; got != revokeSignedOut {
		t.Fatalf("session revoked as %q, want %q", got, revokeSignedOut)
	}
	rr, called := runMiddleware(a, c)
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("the signed-out cookie still authenticates (status %d)", rr.Code)
	}
}

// TestLogoutWithoutIDTokenStaysLocal is the negative control that makes the
// test above mean something (#131's rule).
//
// ⚠ Without this, an implementation that ALWAYS redirects to the provider,
// hint or no hint, passes the happy-path test while shipping exactly the
// silent no-op #314 is about. Here the hint is unavailable (an anonymous
// request, and a session that holds no ID token), so the handler must NOT send
// the browser to the provider at all.
func TestLogoutWithoutIDTokenStaysLocal(t *testing.T) {
	const endSession = "https://idp.example.com/application/o/mailroom/end-session/"

	a := logoutAuthenticator(t, endSession, "")
	loc, _ := doLogout(t, a, nil)
	if strings.Contains(loc, "idp.example.com") {
		t.Fatalf("logout went to the provider with no id_token_hint: %q", loc)
	}
	if loc != "/signin?notice=signed_out" {
		t.Fatalf("Location = %q, want the local post-logout destination", loc)
	}

	c := signIn(t, a, ulid.Make(), "") // a session with no ID token
	loc, _ = doLogout(t, a, c)
	if strings.Contains(loc, "idp.example.com") {
		t.Fatalf("logout went to the provider with a session that has no ID token: %q", loc)
	}
	if got := fakeOf(a).rowFor(c.Value).revokedAs; got != revokeSignedOut {
		t.Fatalf("the hint-less session was revoked as %q, want %q", got, revokeSignedOut)
	}
}

// TestLogoutStaysLocalWhenProviderPublishesNoEndSessionEndpoint covers the
// other half of the same guard: RP-initiated logout is an OPTIONAL part of
// OIDC, and a provider without it is an ordinary deployment rather than a
// misconfiguration. Having the ID token must not be enough on its own.
func TestLogoutStaysLocalWhenProviderPublishesNoEndSessionEndpoint(t *testing.T) {
	a := logoutAuthenticator(t, "", "")
	c := signIn(t, a, ulid.Make(), "header.payload.sig")
	loc, _ := doLogout(t, a, c)

	if loc != "/signin?notice=signed_out" {
		t.Fatalf("Location = %q, want the local post-logout destination", loc)
	}
	if got := fakeOf(a).rowFor(c.Value).revokedAs; got != revokeSignedOut {
		t.Fatalf("session revoked as %q, want %q", got, revokeSignedOut)
	}
}

// TestLogoutAlwaysClearsTheCookies pins the ordering decision in LogoutHandler:
// Mailroom's own cookie is cleared UNCONDITIONALLY, before the provider branch,
// so no path can return early leaving it in place. Checked on the provider path
// (the one with an early return), and on a path where the store fails. The
// retired logout cookie is cleared too, at its original Path.
func TestLogoutAlwaysClearsTheCookies(t *testing.T) {
	for name, setup := range map[string]func(*Authenticator){
		"provider path": func(*Authenticator) {},
		"store down": func(a *Authenticator) {
			f := fakeOf(a)
			f.lookupErr = errors.New("database is down")
		},
		"revoke fails": func(a *Authenticator) { fakeOf(a).revokeErr = errors.New("database is down") },
		"id token read fails": func(a *Authenticator) {
			fakeOf(a).idTokenErr = errors.New("cannot open")
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := logoutAuthenticator(t, "https://idp.example.com/end-session/", "")
			c := signIn(t, a, ulid.Make(), "header.payload.sig")
			setup(a)
			loc, cookies := doLogout(t, a, c)

			cleared := map[string]*http.Cookie{}
			for _, ck := range cookies {
				if ck.Value == "" && ck.MaxAge < 0 {
					cleared[ck.Name] = ck
				}
			}
			if cleared[DefaultSessionCookieName] == nil {
				t.Fatalf("logout did not clear the session cookie (cleared: %v)", cleared)
			}
			legacy := cleared[legacyLogoutCookieName]
			if legacy == nil {
				t.Fatalf("logout did not clear the retired logout cookie (cleared: %v)", cleared)
			}
			if legacy.Path != DefaultLogoutPath {
				t.Fatalf("retired logout cookie cleared at Path %q, want %q: a mismatch leaves the original in place",
					legacy.Path, DefaultLogoutPath)
			}
			if name == "store down" && strings.Contains(loc, "idp.example.com") {
				t.Fatalf("with the store down there is no hint, so logout must stay local; got %q", loc)
			}
		})
	}
}

// A hint read failure still revokes: the sign-out completes locally.
func TestLogoutRevokesEvenWhenTheHintCannotBeRead(t *testing.T) {
	a := logoutAuthenticator(t, "https://idp.example.com/end-session/", "")
	c := signIn(t, a, ulid.Make(), "header.payload.sig")
	fakeOf(a).idTokenErr = errors.New("cannot open")
	loc, _ := doLogout(t, a, c)
	if strings.Contains(loc, "idp.example.com") {
		t.Fatalf("went to the provider without a hint: %q", loc)
	}
	if got := fakeOf(a).rowFor(c.Value).revokedAs; got != revokeSignedOut {
		t.Fatalf("session revoked as %q, want %q", got, revokeSignedOut)
	}
}
