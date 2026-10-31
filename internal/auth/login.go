// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// flowState is the payload of the signed login-state cookie LoginHandler
// sets and CallbackHandler consumes. It carries everything the callback
// needs to complete the flow without server-side storage: the OAuth2
// "state" value (CSRF defense — checked against the state query
// parameter), the OIDC nonce (replay defense for the ID token), the PKCE
// code verifier, and where to send the user after login. See the package
// doc's "Login-state storage" section for why this is a cookie rather than
// a server-side map.
type flowState struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	ReturnTo string    `json:"return_to,omitempty"`
	Expiry   time.Time `json:"exp"`
}

// LoginHandler starts the Authorization Code + PKCE flow: it generates a
// fresh state, nonce, and PKCE verifier/challenge (RFC 7636, S256 only —
// via golang.org/x/oauth2's GenerateVerifier/S256ChallengeOption), stores
// them in a signed login-state cookie, and redirects the user to the
// provider's authorization endpoint.
//
// An optional "return_to" query parameter (a same-origin absolute path) is
// honored and carried through to the post-login redirect; anything else
// (open-redirect risk) is ignored in favor of "/".
func (a *Authenticator) LoginHandler(w http.ResponseWriter, r *http.Request) {
	state, err := randomURLSafeString(randomTokenBytes)
	if err != nil {
		http.Error(w, "auth: internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randomURLSafeString(randomTokenBytes)
	if err != nil {
		http.Error(w, "auth: internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	fs := flowState{
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
		ReturnTo: sanitizeReturnTo(r.URL.Query().Get("return_to")),
		Expiry:   time.Now().Add(a.cfg.StateTTL),
	}

	cookieVal, err := encodeSigned(a.cfg.SigningKey, statePurpose, fs)
	if err != nil {
		http.Error(w, "auth: internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, a.stateCookie(cookieVal, fs.Expiry))

	authURL := a.oauth2Config.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// stateCookie builds the login-state cookie carrying value, expiring at exp.
func (a *Authenticator) stateCookie(value string, exp time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     a.cfg.StateCookieName,
		Value:    value,
		Path:     "/",
		Domain:   a.cfg.CookieDomain,
		Expires:  exp,
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// sanitizeReturnTo restricts return_to to a same-origin absolute path, so
// LoginHandler/LogoutHandler can't be abused as an open redirect. Anything
// else (empty, scheme-relative "//host/...", an absolute URL, or a value
// not starting with "/") falls back to "" (caller then uses its own
// default, typically "/").
//
// ⚠ A BACKSLASH IS A SLASH TO A BROWSER. The WHATWG URL parser treats `\` as
// `/` in http(s) URLs, so `Location: /\evil.example` resolves to
// https://evil.example/ — an open redirect that the "//" check above cannot
// see. Any backslash or control character is therefore refused outright, and
// the value returned is RE-SERIALISED from the parse (escaped path plus raw
// query) rather than echoed, so what reaches Location is what was checked.
func sanitizeReturnTo(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return ""
	}
	for _, c := range v {
		if c == '\\' || c < 0x20 || c == 0x7f {
			return ""
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.IsAbs() || u.Host != "" || u.Opaque != "" {
		return ""
	}
	out := u.EscapedPath()
	if !strings.HasPrefix(out, "/") || strings.HasPrefix(out, "//") {
		return ""
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}
