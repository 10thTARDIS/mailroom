// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/auth"
)

// CSRF defence in depth (#860), driven through the real composed gate. Dev-auth
// makes every request a session request without minting a cookie; the guard is
// the same one the OIDC path gets (mountApp wraps the session gate, not the
// auth flavour).
func TestSessionRequestsAreCSRFGuarded(t *testing.T) {
	h, _ := gateHandler(t, devAuthTestConfig())
	send := func(method, url, ct string, hdr map[string]string, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, strings.NewReader("{}"))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	cross := map[string]string{"Sec-Fetch-Site": "cross-site"}
	same := map[string]string{"Sec-Fetch-Site": "same-origin"}
	up := "/jmap/upload/01J0000000000000000000000/"

	for _, c := range []struct {
		name string
		rec  *httptest.ResponseRecorder
		want int
	}{
		{"jmap cross-site", send("POST", "/jmap", "application/json", cross, ""), 403},
		{"jmap same-origin", send("POST", "/jmap", "application/json", same, ""), 200},
		{"jmap no header", send("POST", "/jmap", "application/json", nil, ""), 200},
		{"jmap text/plain", send("POST", "/jmap", "text/plain", same, ""), 415},
		{"jmap json charset", send("POST", "/jmap", "application/json; charset=utf-8", same, ""), 200},
		{"upload cross-site", send("POST", up, "image/png", cross, ""), 403},
		{"upload same-origin", send("POST", up, "image/png", same, ""), 200},
		{"mcp cross-site", send("POST", api.MCPPath, "application/json", cross, ""), 403},
		{"cross-site GET unaffected", send("GET", "/jmap/session", "", cross, ""), 200},
	} {
		if c.rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (body %q)", c.name, c.rec.Code, c.want, c.rec.Body.String())
		}
	}

	// A bearer request is decided by the bearer gate alone: a cross-site header
	// and a non-JSON body change nothing (an unknown key is still its 401, not
	// the guard's 403/415).
	unknown, _, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if rec := send("POST", "/jmap", "text/plain", cross, unknown); rec.Code != http.StatusUnauthorized {
		t.Errorf("bearer + cross-site + text/plain: status %d, want the bearer gate's 401", rec.Code)
	}
}
