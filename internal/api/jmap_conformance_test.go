// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests (no database) for the JMAP conformance fixes the third-party
// client job found: the request-level error types, the session's URLs and
// read-only flag, the MessageIds form, and the /get id accounting.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// RFC 8620 §3.6.1: bytes that are not JSON are notJSON; JSON that is not a
// Request object stays notRequest. Both are application/problem+json.
func TestRequestBodyErrorTypes(t *testing.T) {
	h := auth.DevAuth(http.HandlerFunc(New(nil).RequestHandler), store.NewULID())
	cases := []struct {
		name, body, wantType string
	}{
		{"plain text", "this is not json", "notJSON"},
		{"empty body", "", "notJSON"},
		{"truncated object", `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[`, "notJSON"},
		{"trailing comma", `{"using":[],}`, "notJSON"},
		{"array is not a Request", `[]`, "notRequest"},
		{"string is not a Request", `"hello"`, "notRequest"},
		{"using is not a list", `{"using":"urn:ietf:params:jmap:core","methodCalls":[]}`, "notRequest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jmap", strings.NewReader(c.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if want := `"type":"` + jmapErrURN + c.wantType + `"`; !strings.Contains(rec.Body.String(), want) {
				t.Errorf("body %s, want %s", rec.Body.String(), want)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type %q, want application/problem+json", ct)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("the hardening headers are missing from a request-level error")
			}
		})
	}
}

// A body over the size cap is neither of the two: it must not be reported as
// "not JSON" just because the decoder stopped reading.
func TestRequestBodyTooLargeIsNotNotJSON(t *testing.T) {
	h := auth.DevAuth(http.HandlerFunc(New(nil).RequestHandler), store.NewULID())
	big := `{"using":[],"methodCalls":[],"pad":"` + strings.Repeat("a", maxRequestBytes) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jmap", strings.NewReader(big)))
	if strings.Contains(rec.Body.String(), "notJSON") {
		t.Errorf("an oversized body was called notJSON: %s", rec.Body.String())
	}
}

func TestSessionOrigin(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "http://mail.example.com:8090/jmap/session", nil)
	if got := sessionOrigin(plain, ""); got != "http://mail.example.com:8090" {
		t.Errorf("request origin = %q", got)
	}
	tlsReq := httptest.NewRequest(http.MethodGet, "https://mail.example.com/jmap/session", nil)
	if got := sessionOrigin(tlsReq, ""); got != "https://mail.example.com" {
		t.Errorf("TLS request origin = %q", got)
	}
	// The configured origin wins over whatever the request says, and a
	// trailing slash does not double up in front of the path.
	if got := sessionOrigin(plain, "https://public.example.com/"); got != "https://public.example.com" {
		t.Errorf("configured origin = %q", got)
	}
	// Forwarded headers are not trusted: nothing in this server says which
	// proxies may set them.
	fwd := httptest.NewRequest(http.MethodGet, "http://mail.example.com/jmap/session", nil)
	fwd.Header.Set("X-Forwarded-Proto", "https")
	fwd.Header.Set("X-Forwarded-Host", "evil.example.org")
	if got := sessionOrigin(fwd, ""); got != "http://mail.example.com" {
		t.Errorf("a forwarded header changed the origin: %q", got)
	}
	noHost := httptest.NewRequest(http.MethodGet, "/jmap/session", nil)
	noHost.Host = ""
	if got := sessionOrigin(noHost, ""); got != "" {
		t.Errorf("no host: origin %q, want empty (relative URLs)", got)
	}
}

func TestKeyIsReadOnly(t *testing.T) {
	cases := []struct {
		name   string
		scopes []string
		want   bool
	}{
		{"read only", []string{scopeMailRead, scopeSearchRead, scopeLabelsRead, scopeContactsRead}, true},
		{"no scopes at all", nil, true},
		{"draft", []string{scopeMailRead, scopeMailDraft}, false},
		{"move", []string{scopeMailRead, scopeMailMove}, false},
		{"delete", []string{scopeMailRead, scopeMailDelete}, false},
		{"send", []string{scopeMailRead, scopeMailSend}, false},
		{"label write", []string{scopeMailRead, scopeLabelsWrite}, false},
		{"contacts write", []string{scopeContactsRead, scopeContactsWrite}, false},
	}
	for _, c := range cases {
		if got := keyIsReadOnly(newPrincipal(store.APIKey{Scopes: c.scopes}, "")); got != c.want {
			t.Errorf("%s: isReadOnly = %v, want %v", c.name, got, c.want)
		}
	}
	// A signed-in session (no key principal) can write.
	if keyIsReadOnly(nil) {
		t.Error("a session is reported read-only")
	}
	// Every scope is classified: a new scope has to be put in one list or the
	// other on purpose.
	write := map[string]bool{}
	for _, s := range writeScopes {
		write[s] = true
	}
	for _, s := range allScopes {
		if !write[s] && !strings.HasSuffix(s, ":read") {
			t.Errorf("scope %q is neither a :read scope nor in writeScopes", s)
		}
	}
}

// RFC 8621 §4.1.2.3: the MessageIds form has no angle brackets, and a header
// that is absent is null, never [].
func TestMessageIDsProperty(t *testing.T) {
	if got := messageIDsProperty([]string{"<a@example.net>"}); !equalStrings(got, "a@example.net") {
		t.Errorf("single = %#v", got)
	}
	if got := messageIDsProperty([]string{"<a@example.net>", " <b@example.net> ", "c@example.net"}); !equalStrings(got, "a@example.net", "b@example.net", "c@example.net") {
		t.Errorf("chain = %#v", got)
	}
	for _, absent := range [][]string{nil, {}, {""}, {"<>"}, {"  "}} {
		if got := messageIDsProperty(absent); got != nil {
			t.Errorf("absent %q = %#v, want nil (JSON null)", absent, got)
		}
	}
}

func equalStrings(v any, want ...string) bool {
	got, ok := v.([]string)
	if !ok || len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Every requested id is accounted for (RFC 8620 §5.1): one that cannot be an id
// is notFound, once, in the order asked.
func TestParseIDsReportsMalformedIDs(t *testing.T) {
	good := store.NewULID().String()
	valid, notFound, requested := parseIDs([]string{"not-an-id", good, "", "ZZZ"})
	if len(valid) != 1 || valid[0].String() != good {
		t.Errorf("valid = %v", valid)
	}
	if len(notFound) != 3 || notFound[0] != "not-an-id" || notFound[1] != "" || notFound[2] != "ZZZ" {
		t.Errorf("notFound = %q", notFound)
	}
	if len(requested) != 4 {
		t.Errorf("requested keeps %d entries, want 4", len(requested))
	}
}
