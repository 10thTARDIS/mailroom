// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/oauthmail"
)

// The OAuth callback (#52) sits behind the BROWSER gate: a signed-out browser
// is bounced to sign-in, and the authorization response's code and state are
// NOT carried into return_to (they would otherwise travel through the whole
// sign-in round trip). The path survives, so the user lands somewhere
// sensible.
func TestOAuthCallbackGateDropsCodeFromReturnTo(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))
	req := httptest.NewRequest(http.MethodGet,
		oauthmail.CallbackPathPrefix+"google?code=4%2F0sentinel-code&state=sentinel-state&scope=x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to the sign-in page", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, loginPath) {
		t.Fatalf("Location = %q, want the sign-in page", loc)
	}
	if strings.Contains(loc, "sentinel-code") || strings.Contains(loc, "sentinel-state") {
		t.Fatalf("the authorization code or state was carried into the sign-in redirect: %q", loc)
	}
	if !strings.Contains(loc, "return_to=%2Foauth%2Fcallback%2Fgoogle") {
		t.Errorf("Location = %q, want return_to naming the callback path", loc)
	}

	// Control: an ordinary browser route still keeps its query in return_to,
	// so the drop above is specific to an authorization response.
	req = httptest.NewRequest(http.MethodGet, "/inbox?view=all", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "view%3Dall") {
		t.Errorf("ordinary route lost its query: Location = %q", loc)
	}
}

// Account/oauthStart is a JMAP POST, so a cross-site page cannot start a
// connect in a signed-in user's name: the CSRF guard refuses it before the
// API sees it. The provider's callback, by contrast, IS a cross-site top-level
// GET and must get through (it reaches the handler under dev-auth's session).
func TestOAuthStartIsCSRFGuardedAndCallbackIsNot(t *testing.T) {
	h, _ := gateHandler(t, devAuthTestConfig())
	body := `{"using":["urn:ietf:params:jmap:core","urn:mailroom:accounts"],` +
		`"methodCalls":[["Account/oauthStart",{"provider":"google"},"0"]]}`
	for _, c := range []struct {
		site string
		want int
	}{{"cross-site", http.StatusForbidden}, {"same-site", http.StatusForbidden}, {"same-origin", http.StatusOK}} {
		req := httptest.NewRequest(http.MethodPost, "/jmap", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", c.site)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s oauthStart POST: status %d, want %d", c.site, rec.Code, c.want)
		}
		if reached := strings.Contains(rec.Body.String(), jmapMarker); reached != (c.want == http.StatusOK) {
			t.Errorf("%s oauthStart POST: API reached = %v", c.site, reached)
		}
	}
	req := httptest.NewRequest(http.MethodGet, oauthmail.CallbackPathPrefix+"google?code=c&state=s", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden || !strings.Contains(rec.Body.String(), webMarker) {
		t.Errorf("cross-site callback GET: status %d, handler reached %v", rec.Code, strings.Contains(rec.Body.String(), webMarker))
	}
}

// Without a provider configured the callback still answers, with the fixed
// "not configured" result, and never a destination taken from the request.
func TestOAuthCallbackWithoutConnector(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, oauthmail.CallbackPathPrefix+"google?code=c&state=s", nil)
	req.SetPathValue("provider", "google")
	oauthCallbackHandler(nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?oauth=not_configured#/settings" {
		t.Errorf("Location = %q", loc)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers = %v", rec.Header())
	}
}
