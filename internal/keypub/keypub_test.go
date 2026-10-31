// SPDX-License-Identifier: AGPL-3.0-only

package keypub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/wkd"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeLookup is the only implementation of Lookup in this package's tests.
// It records exactly what the handler asked for, so tests can assert on the
// call itself rather than only its consequences — that is the only way to
// pin "the domain came from config, not the request" and "malformed input
// never reaches the database".
type fakeLookup struct {
	t *testing.T

	// failIfCalled turns any call into a fatal test failure. Used for the
	// validation tests, where the entire point is that the store is never
	// consulted for malformed input.
	failIfCalled bool

	called    bool
	gotDomain string
	gotHash   string

	key         []byte
	fingerprint string
	found       bool
	err         error
}

// LookupWKDKeyPage exists because keypub.New REQUIRES it: a lookup that cannot
// serve the key page is refused at construction rather than 404ing at request
// time, where a mis-wire is indistinguishable from an unpublished key. These
// WKD tests do not exercise the page, so it answers nothing — but it has to be
// here, and that is the constructor check earning its place.
func (f *fakeLookup) LookupWKDKeyPage(_ context.Context, _, _ string) (string, string, bool, error) {
	return "", "", false, nil
}

func (f *fakeLookup) LookupWKDKey(_ context.Context, domain, hash string) ([]byte, string, bool, error) {
	f.called = true
	f.gotDomain = domain
	f.gotHash = hash
	if f.failIfCalled {
		f.t.Fatalf("LookupWKDKey called unexpectedly with domain=%q hash=%q — malformed input reached the store", domain, hash)
	}
	return f.key, f.fingerprint, f.found, f.err
}

// validHash is a well-formed WKD directory name (32 z-base-32 characters),
// used wherever a test needs a request that gets PAST the path-shape check.
// Two are used to probe the anti-enumeration property — an unpublished
// address and a nonexistent one both hash to something this shape.
const (
	validHash1 = "iy9q119eutrkn8s1mk4r39qejnbu3n5q" // wkd.HashLocalPart("Joe.Doe")
	validHash2 = "s9eyq38w8ptio3w48wyoq4jxihjib4is" // wkd.HashLocalPart("nonexistent")
)

func TestValidHashesAreSelfConsistentWithTheEncoder(t *testing.T) {
	// If this ever drifts, the two constants above stop being "two different
	// well-formed hashes" and the enumeration test below stops testing
	// anything.
	if got := wkd.HashLocalPart("Joe.Doe"); got != validHash1 {
		t.Fatalf("wkd.HashLocalPart(Joe.Doe) = %q, want %q", got, validHash1)
	}
	if got := wkd.HashLocalPart("nonexistent"); got != validHash2 {
		t.Fatalf("wkd.HashLocalPart(nonexistent) = %q, want %q", got, validHash2)
	}
	if !validHash(validHash1) || !validHash(validHash2) {
		t.Fatal("test fixture hashes do not pass validHash — the tests below would be exercising the wrong code path")
	}
}

// TestHUHandlerServesBinaryNotArmor is the single most valuable test in this
// file. draft-koch-openpgp-webkey-service requires the raw OpenPGP packets
// with Content-Type application/octet-stream; shipping ASCII armor instead
// is the single most common way to build a WKD endpoint that returns 200 to
// curl and that no real mail client will ever accept. The failure is
// invisible from the server side — there is no error, no bad status, just a
// client silently refusing to find the key — so this must be caught here,
// as an explicit assertion, or it is never caught at all.
func TestHUHandlerServesBinaryNotArmor(t *testing.T) {
	rawKey := []byte{0x99, 0x01, 0x0d, 0x04, 0xde, 0xad, 0xbe, 0xef, 0x00, 0x01, 0x02, 0x03}
	fake := &fakeLookup{key: rawKey, found: true, fingerprint: "ABCDEF"}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash1, nil)
	svc.HUHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, rawKey) {
		t.Errorf("body = %x, want the exact raw key bytes %x", got, rawKey)
	}
	// The decisive negative control: even a correct status and content-type
	// header are not proof of binary output if the body itself is armor text.
	if bytes.Contains(rec.Body.Bytes(), []byte("-----BEGIN PGP")) {
		t.Error("response body contains ASCII armor — every real mail client will reject this WKD response despite the 200")
	}
}

// TestHUHandlerRefusesStoredArmor pins the explicit guard in HUHandler: even
// if something upstream (a bug, a bad migration, a manual DB edit) put
// armored text where the binary key belongs, the handler must refuse to
// serve it rather than pass it through. Silently serving armor here would
// make the endpoint look like it worked while being useless to every real
// client.
func TestHUHandlerRefusesStoredArmor(t *testing.T) {
	armored := []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQENBF...\n-----END PGP PUBLIC KEY BLOCK-----\n")
	fake := &fakeLookup{key: armored, found: true}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash1, nil)
	svc.HUHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "-----BEGIN PGP") {
		t.Errorf("response body leaked the armored key material: %q", rec.Body.String())
	}
}

// TestHUHandlerMissIsIndistinguishable is the anti-enumeration property: a
// hash naming an address that exists but was never published, and a hash
// naming an address that does not exist at all, must produce responses a
// caller cannot tell apart. If they differed — even just in body length or
// an extra header — a stranger could use this endpoint to discover which
// addresses exist on the domain, one guess at a time.
func TestHUHandlerMissIsIndistinguishable(t *testing.T) {
	svc, err := New(&fakeLookup{found: false}, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recUnpublished := httptest.NewRecorder()
	svc.HUHandler().ServeHTTP(recUnpublished, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash1, nil))

	recNonexistent := httptest.NewRecorder()
	svc.HUHandler().ServeHTTP(recNonexistent, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash2, nil))

	if recUnpublished.Code != http.StatusNotFound {
		t.Fatalf("unpublished: status = %d, want 404", recUnpublished.Code)
	}
	if recUnpublished.Code != recNonexistent.Code {
		t.Errorf("status differs: unpublished=%d nonexistent=%d — this alone would let a caller enumerate addresses",
			recUnpublished.Code, recNonexistent.Code)
	}
	if !bytes.Equal(recUnpublished.Body.Bytes(), recNonexistent.Body.Bytes()) {
		t.Errorf("body differs: unpublished=%q nonexistent=%q", recUnpublished.Body.String(), recNonexistent.Body.String())
	}
	// "No body" means no CUSTOM body — the handler calls the stdlib's generic
	// http.NotFound rather than writing anything of its own. Compare against
	// a reference response built the same way, so this fails if the handler
	// ever starts writing a body that carries information (e.g. distinguishing
	// "no such user" from "not published").
	wantRec := httptest.NewRecorder()
	http.NotFound(wantRec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash1, nil))
	if !bytes.Equal(recUnpublished.Body.Bytes(), wantRec.Body.Bytes()) {
		t.Errorf("miss body = %q, want the stdlib's generic http.NotFound body %q", recUnpublished.Body.String(), wantRec.Body.String())
	}
}

// TestHUHandlerRejectsMalformedHashesWithoutLookup drives the path-shape
// check with input that must never reach the store. Length-only validation
// would accept a right-length string built from characters outside the
// z-base-32 alphabet, so the out-of-alphabet case here is the one that
// actually distinguishes "checks length" from "checks shape" — the
// alphabet is "ybndrfg8ejkmcpqxot1uwisza345h769", so l, v, 0 and 2 are
// genuinely absent from it and would otherwise sail through length
// validation. If the fake's LookupWKDKey is ever invoked here, the test
// fails via fakeLookup's failIfCalled — a wrong or missing guard produces a
// visible test failure, not a silent pass.
func TestHUHandlerRejectsMalformedHashesWithoutLookup(t *testing.T) {
	rightLengthInvalidChar := "l" + strings.Repeat("y", hashLen-1) // 32 chars, 'l' is not in the alphabet
	if len(rightLengthInvalidChar) != hashLen {
		t.Fatalf("test fixture is %d chars, want %d", len(rightLengthInvalidChar), hashLen)
	}

	cases := map[string]string{
		"empty":                 "",
		"too_short":             validHash1[:hashLen-1],
		"too_long":              validHash1 + "y",
		"uppercase":             strings.ToUpper(validHash1),
		"out_of_alphabet_l":     rightLengthInvalidChar,
		"out_of_alphabet_v":     "v" + strings.Repeat("y", hashLen-1),
		"out_of_alphabet_0":     "0" + strings.Repeat("y", hashLen-1),
		"out_of_alphabet_2":     "2" + strings.Repeat("y", hashLen-1),
		"path_traversal_shaped": "../../../../etc/passwd",
	}

	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLookup{t: t, failIfCalled: true}
			svc, err := New(fake, "example.org", testLogger())
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+hash, nil)
			svc.HUHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if fake.called {
				// Reachable in principle if failIfCalled's t.Fatalf did not
				// actually stop execution; kept as a belt-and-braces check.
				t.Error("lookup was called for a malformed hash")
			}
		})
	}
}

// TestDisabledServiceNever404sViaLookup asserts that an instance with no
// configured domain answers every route with a bare 404 and never touches
// the store — WKD being off must mean off, not "off except it still queries
// the database and discards the answer".
func TestDisabledServiceNever404sViaLookup(t *testing.T) {
	fake := &fakeLookup{t: t, failIfCalled: true}
	svc, err := New(fake, "", testLogger()) // empty domain: disabled
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.Enabled() {
		t.Fatal("service reports enabled with an empty domain")
	}

	for _, tc := range []struct {
		name    string
		path    string
		handler http.Handler
	}{
		{"hu_valid_shape", "/.well-known/openpgpkey/hu/" + validHash1, svc.HUHandler()},
		{"hu_malformed", "/.well-known/openpgpkey/hu/not-a-hash", svc.HUHandler()},
		{"policy", "/.well-known/openpgpkey/policy", svc.PolicyHandler()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}

// TestHUHandlerDomainIsAlwaysServerConfigured pins that the domain answered
// for comes from the service's own configuration, never from anything the
// caller supplies. The direct method's path carries no domain at all — so
// if a future change threaded one in from the Host header, a query
// parameter, or anywhere else on the request, a caller could make this
// instance answer for a domain it does not serve. The request below is
// deliberately hostile on every axis that is NOT the path, to prove none of
// them reach the lookup.
func TestHUHandlerDomainIsAlwaysServerConfigured(t *testing.T) {
	const configuredDomain = "example.org"
	fake := &fakeLookup{found: true, key: []byte{0x01}}
	svc, err := New(fake, configuredDomain, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash1+"?l=someone", nil)
	req.Host = "attacker.example.net"
	req.Header.Set("X-Forwarded-Host", "another-attacker.example.net")

	rec := httptest.NewRecorder()
	svc.HUHandler().ServeHTTP(rec, req)

	if !fake.called {
		t.Fatal("lookup was never called")
	}
	if fake.gotDomain != configuredDomain {
		t.Errorf("lookup received domain %q, want the server's configured domain %q", fake.gotDomain, configuredDomain)
	}
}

// TestPolicyHandler pins the marker file's contract: the draft treats its
// mere existence, with a 200, as the signal that the domain supports WKD. A
// non-empty body or a wrong content type risks some clients not recognising
// the marker at all.
func TestPolicyHandler(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		svc, err := New(&fakeLookup{}, "example.org", testLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		rec := httptest.NewRecorder()
		svc.PolicyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/policy", nil))

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty — the draft treats an empty policy file as valid", rec.Body.String())
		}
	})

	t.Run("disabled", func(t *testing.T) {
		svc, err := New(&fakeLookup{}, "", testLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		rec := httptest.NewRecorder()
		svc.PolicyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/policy", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

// TestHUHandlerLookupErrorNotRelayed: this response goes to an anonymous
// caller, so a store error (a DSN fragment, a constraint name, a query
// snippet) must never ride along in the body or any header. The handler is
// expected to log it and return a generic message instead.
func TestHUHandlerLookupErrorNotRelayed(t *testing.T) {
	const secret = "SECRET-INTERNAL-DETAIL"
	fake := &fakeLookup{err: errors.New("pq: constraint violation near " + secret)}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+validHash1, nil)
	svc.HUHandler().ServeHTTP(rec, req)

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
