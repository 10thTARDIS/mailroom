// SPDX-License-Identifier: AGPL-3.0-only

package keypub

import (
	"bytes"
	"context"
	"errors"
	htmltemplate "html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/wkd"
)

// pageFakeLookup is PageLookup's only implementation in this package's
// tests. It also satisfies Lookup — trivially, returning "not found" — only
// because Service.lookup is field-typed as Lookup; PageHandler never calls
// LookupWKDKey, it reaches PageLookup purely via the runtime type assertion,
// so exercising that path is the point of this type existing at all.
type pageFakeLookup struct {
	t *testing.T

	// failIfCalled turns any call into a fatal test failure. Used for the
	// disabled-service test, where the whole point is that the store is
	// never consulted.
	failIfCalled bool

	address     string
	fingerprint string
	found       bool
	err         error

	called    bool
	gotDomain string
	gotHash   string
}

func (f *pageFakeLookup) LookupWKDKey(_ context.Context, _, _ string) ([]byte, string, bool, error) {
	return nil, "", false, nil // unused by PageHandler; present only to satisfy Lookup.
}

func (f *pageFakeLookup) LookupWKDKeyPage(_ context.Context, domain, hash string) (string, string, bool, error) {
	f.called = true
	f.gotDomain = domain
	f.gotHash = hash
	if f.failIfCalled {
		f.t.Fatalf("LookupWKDKeyPage called unexpectedly with domain=%q hash=%q", domain, hash)
	}
	return f.address, f.fingerprint, f.found, f.err
}

// knownFingerprint is easy to verify by eye: each 4-character group is a
// distinct repeated character, so a transcription error in the expected
// grouped string would be obvious rather than needing to be counted out.
const knownFingerprint = "AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555"

func TestGroupFingerprint(t *testing.T) {
	const want = "AAAA 1111 BBBB 2222 CCCC  3333 DDDD 4444 EEEE 5555"
	if got := groupFingerprint(knownFingerprint); got != want {
		t.Errorf("groupFingerprint(%q) = %q, want %q", knownFingerprint, got, want)
	}
}

// TestPageHandlerRendersPublishedKey pins the page's actual content
// contract: the address, the grouped fingerprint, and BOTH WKD URLs (advanced
// before direct, per the draft's lookup order) must be present.
func TestPageHandlerRendersPublishedKey(t *testing.T) {
	fake := &pageFakeLookup{address: "joe.doe@example.org", fingerprint: knownFingerprint, found: true}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil)
	svc.PageHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !fake.called {
		t.Fatal("PageLookup was never called")
	}
	if fake.gotDomain != "example.org" || fake.gotHash != validHash1 {
		t.Errorf("lookup got domain=%q hash=%q, want example.org / %s", fake.gotDomain, fake.gotHash, validHash1)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"joe.doe@example.org",
		"AAAA 1111 BBBB 2222 CCCC  3333 DDDD 4444 EEEE 5555",
		wkd.AdvancedPath("joe.doe", "example.org"),
		wkd.DirectPath("joe.doe"),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}

	// The download link must be the WKD path itself — there is exactly one
	// way to get the bytes, and it must be the one every real client uses.
	wantHref := `href="` + wkd.DirectPath("joe.doe") + `"`
	if !strings.Contains(body, wantHref) {
		t.Errorf("body missing download link %q\nbody:\n%s", wantHref, body)
	}
}

// TestPageHandlerEscapesUntrustedInput is the single most valuable test in
// this file. The address is untrusted output straight out of the database,
// and it lands inside HTML three separate times in this template (title,
// heading, hint sentence). If any of those used text/template or string
// concatenation instead of html/template's structural escaping, this is
// where it would show up as a stored XSS reachable by anyone who knows a
// hash. The test asserts BOTH that the raw dangerous substring is absent AND
// that its escaped form is present, so it cannot pass by silently dropping
// the address instead of escaping it.
func TestPageHandlerEscapesUntrustedInput(t *testing.T) {
	payloads := map[string]string{
		"script_tag":         `<script>alert(1)</script>`,
		"attribute_breakout": `"><img src=x onerror=alert(1)>`,
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			addr := payload + "@example.org"
			fake := &pageFakeLookup{address: addr, fingerprint: knownFingerprint, found: true}
			svc, err := New(fake, "example.org", testLogger())
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil)
			svc.PageHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}

			body := rec.Body.String()
			if strings.Contains(body, payload) {
				t.Errorf("response body contains the RAW unescaped payload %q — html/template failed to escape it:\n%s", payload, body)
			}
			wantEscaped := htmltemplate.HTMLEscapeString(payload)
			if !strings.Contains(body, wantEscaped) {
				t.Errorf("response body does not contain the escaped form %q — the address may have been dropped rather than escaped:\n%s", wantEscaped, body)
			}
		})
	}
}

// TestPageHandlerMissMatchesWKDHandlerMiss enforces the anti-enumeration
// requirement carried over from WKD: an unpublished address and a
// nonexistent one must be indistinguishable, and this route must not
// introduce a SECOND way to tell them apart with a friendlier body than the
// WKD handler gives. The page's 404 is compared byte-for-byte against both
// the WKD handler's own 404 and the stdlib's generic http.NotFound.
func TestPageHandlerMissMatchesWKDHandlerMiss(t *testing.T) {
	fake := &pageFakeLookup{found: false}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pageRec := httptest.NewRecorder()
	svc.PageHandler().ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil))

	huRec := httptest.NewRecorder()
	svc.HUHandler().ServeHTTP(huRec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash1, nil))

	if pageRec.Code != http.StatusNotFound {
		t.Fatalf("page status = %d, want 404", pageRec.Code)
	}
	if pageRec.Code != huRec.Code {
		t.Errorf("status differs: page=%d hu=%d", pageRec.Code, huRec.Code)
	}
	if !bytes.Equal(pageRec.Body.Bytes(), huRec.Body.Bytes()) {
		t.Errorf("body differs: page=%q hu=%q — a difference here would let a caller tell the key page's miss apart from WKD's, reopening the enumeration WKD is careful to avoid",
			pageRec.Body.String(), huRec.Body.String())
	}

	wantRec := httptest.NewRecorder()
	http.NotFound(wantRec, httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil))
	if !bytes.Equal(pageRec.Body.Bytes(), wantRec.Body.Bytes()) {
		t.Errorf("miss body = %q, want the stdlib's generic http.NotFound body %q", pageRec.Body.String(), wantRec.Body.String())
	}
}

// TestPageHandlerDisabledService404s mirrors the WKD handlers' behavior: an
// instance with no configured domain answers 404 for every request and never
// touches the store, so "disabled" cannot mean "disabled except it still
// queries the database and discards the answer".
func TestPageHandlerDisabledService404s(t *testing.T) {
	fake := &pageFakeLookup{t: t, failIfCalled: true}
	svc, err := New(fake, "", testLogger()) // empty domain: disabled
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.Enabled() {
		t.Fatal("service reports enabled with an empty domain")
	}

	rec := httptest.NewRecorder()
	svc.PageHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if fake.called {
		t.Error("lookup was called while the service is disabled")
	}
}

// TestPageHandlerRejectsMalformedHashWithoutLookup pins that a malformed
// hash never reaches the store, matching HUHandler's cheap-rejection
// property (keypub_test.go's equivalent for the binary endpoint).
func TestPageHandlerRejectsMalformedHashWithoutLookup(t *testing.T) {
	fake := &pageFakeLookup{t: t, failIfCalled: true}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	svc.PageHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/key/not-a-valid-hash", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if fake.called {
		t.Error("lookup was called for a malformed hash")
	}
}

// TestPageHandlerLookupWithoutPageSupport404s pins the design choice this
// file makes: PageLookup is a SEPARATE interface from Lookup, reached by a
// runtime type assertion on Service.lookup rather than by widening Lookup's
// declared type. A Lookup that does not additionally implement PageLookup
// must not panic and must not leak "well, WKD works but the page doesn't" —
// it gets the same generic 404 as any other miss.
func TestPageHandlerLookupWithoutPageSupport404s(t *testing.T) {
	fake := &fakeLookup{found: true, key: []byte{0x01}, fingerprint: "ABCDEF"} // implements Lookup only
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	svc.PageHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (lookup does not implement PageLookup)", rec.Code)
	}
}

// TestPageHandlerLookupErrorNotRelayed: this response goes to an anonymous
// caller, so a store error must never ride along in the body or a header.
func TestPageHandlerLookupErrorNotRelayed(t *testing.T) {
	const secret = "SECRET-INTERNAL-DETAIL"
	fake := &pageFakeLookup{err: errors.New("pq: constraint violation near " + secret)}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	svc.PageHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("response body leaked the internal error: %q", rec.Body.String())
	}
	for name, vals := range rec.Header() {
		for _, v := range vals {
			if strings.Contains(v, secret) {
				t.Errorf("header %s leaked the internal error: %q", name, v)
			}
		}
	}
}
