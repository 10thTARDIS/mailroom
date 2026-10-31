// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests for the blob endpoints' PURE decisions — the ones that decide
// whether an attachment can run as code on the operator's origin (safeContentType,
// contentDisposition) and whether a blobId can name anything the server did not
// mint (parseBlobID). The authorization half is necessarily an integration test
// (it is a Postgres/RLS question): see blob_integration_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseBlobID(t *testing.T) {
	const hash = "966f0000000000000000000000000000000000000000000000000000000000e6"

	ok := []struct{ in, part string }{
		{hash, ""},
		{hash + ".1", "1"},
		{hash + ".1.2", "1.2"},
		{hash + ".12.3.4", "12.3.4"},
	}
	for _, tc := range ok {
		got, err := parseBlobID(tc.in)
		if err != nil {
			t.Fatalf("parseBlobID(%q): %v", tc.in, err)
		}
		if got.part != tc.part {
			t.Errorf("parseBlobID(%q).part = %q, want %q", tc.in, got.part, tc.part)
		}
		if len(got.hash) != 32 {
			t.Errorf("parseBlobID(%q): hash is %d bytes", tc.in, len(got.hash))
		}
	}

	bad := []string{
		"",
		"not-hex",
		hash[:62],                    // short hash
		hash + "ab",                  // long hash
		hash + ".",                   // empty part
		hash + ".1.",                 // empty trailing segment
		hash + "..1",                 // empty segment
		hash + ".a",                  // non-numeric part
		hash + ".../etc/passwd",      // traversal
		hash + ".1/../../etc/passwd", // traversal
		hash + "." + strings.Repeat("1.", 40),
	}
	for _, in := range bad {
		if _, err := parseBlobID(in); err == nil {
			t.Errorf("parseBlobID(%q) should have failed", in)
		}
	}
}

// TestSafeContentTypeNeverServesAScriptingContext is the anti-stored-XSS test: a
// phishing email's text/html or SVG attachment must never come back out of our
// own origin as a type a browser will execute.
func TestSafeContentTypeNeverServesAScriptingContext(t *testing.T) {
	dangerous := []string{
		"text/html",
		"text/html; charset=utf-8",
		"TEXT/HTML",
		"image/svg+xml",
		"application/xhtml+xml",
		"application/xml",
		"text/xml",
		"application/javascript",
		"text/javascript",
		"application/x-shockwave-flash",
		"application/octet-stream",
		"", // no declared type
	}
	for _, in := range dangerous {
		if got := safeContentType(in); got != "application/octet-stream" {
			t.Errorf("safeContentType(%q) = %q, want application/octet-stream", in, got)
		}
	}

	allowed := map[string]string{
		"image/png":                      "image/png",
		"image/jpeg; name=cat.jpg":       "image/jpeg",
		"application/pdf":                "application/pdf",
		"text/plain":                     "text/plain; charset=utf-8",
		"text/plain; charset=iso-8859-1": "text/plain; charset=utf-8", // mime decodes to UTF-8
	}
	for in, want := range allowed {
		if got := safeContentType(in); got != want {
			t.Errorf("safeContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentDispositionIsAlwaysAttachment(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantSubstr []string
	}{
		{"plain", "invoice.pdf", []string{`attachment; filename="invoice.pdf"`, "filename*=UTF-8''invoice.pdf"}},
		{"utf8", "faktúra.pdf", []string{`filename="fakt_ra.pdf"`, "filename*=UTF-8''fakt%C3%BAra.pdf"}},
		{"empty", "", []string{`filename="attachment"`}},
	}
	for _, tc := range tests {
		got := contentDisposition(sanitizeFilename(tc.in))
		if !strings.HasPrefix(got, "attachment;") {
			t.Errorf("%s: %q is not an attachment disposition", tc.name, got)
		}
		for _, want := range tc.wantSubstr {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q does not contain %q", tc.name, got, want)
			}
		}
	}
}

// TestFilenameCannotForgeHeaders: the {name} path segment is client-supplied and
// ends up in a header. A CRLF (or a quote) in it must not be able to add one.
func TestFilenameCannotForgeHeaders(t *testing.T) {
	hostile := []string{
		"a\r\nSet-Cookie: pwned=1",
		"a\nX-Evil: 1",
		`a"; filename="b`,
		"../../etc/passwd",
		"..",
		"\x00nul",
	}
	for _, in := range hostile {
		got := contentDisposition(sanitizeFilename(in))
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Errorf("contentDisposition(%q) = %q: contains a line break", in, got)
		}
		// The quoted-string must contain exactly two quotes (open + close) plus
		// nothing that could terminate it early.
		quoted := strings.SplitN(got, `"`, 3)
		if len(quoted) != 3 {
			t.Fatalf("contentDisposition(%q) = %q: malformed quoted filename", in, got)
		}
		if strings.Contains(quoted[1], `"`) || strings.Contains(quoted[1], `\`) {
			t.Errorf("contentDisposition(%q) = %q: filename escapes its quotes", in, got)
		}
		if strings.Contains(quoted[1], "/") {
			t.Errorf("contentDisposition(%q) = %q: filename carries a path separator", in, got)
		}
	}
}

func TestUploadTypeSanitizes(t *testing.T) {
	tests := map[string]string{
		"application/pdf":               "application/pdf",
		"IMAGE/PNG":                     "image/png",
		"text/plain; charset=utf-8":     "text/plain",
		"":                              "application/octet-stream",
		"nonsense":                      "application/octet-stream",
		"text/plain\r\nX-Evil: 1":       "application/octet-stream",
		"application/":                  "application/octet-stream",
		"/pdf":                          "application/octet-stream",
		strings.Repeat("a", 200) + "/b": "application/octet-stream",
	}
	for in, want := range tests {
		if got := uploadType(in); got != want {
			t.Errorf("uploadType(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBlobHandlersRequireAuth: no identity in the context (i.e. the handler was
// reached without the auth middleware) is a 401, never a read.
func TestBlobHandlersRequireAuth(t *testing.T) {
	a := New(nil)
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{"download", http.MethodGet, "/jmap/download/acct/blob/name", a.DownloadHandler},
		{"preview", http.MethodGet, "/jmap/preview/acct/blob/name", a.PreviewHandler},
		{"upload", http.MethodPost, "/jmap/upload/acct/", a.UploadHandler},
		{"eventsource", http.MethodGet, "/jmap/eventsource/", a.EventSourceHandler},
	} {
		rec := httptest.NewRecorder()
		tc.handler(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, rec.Code)
		}
	}
}

// --- The preview door (#131) --------------------------------------------------
//
// docs/hostile-content.md §2.4 had exactly two doors until this feature: rendering
// is sanitized, downloading is inert. The preview route is a third, and these are
// the assertions that keep it narrower than the second rather than wider. Each one
// pins a property whose loss would not look like a bug — it would look like a
// preview that handles more files.

// The inline allowlist is the whole reason this route is safe to have. It must
// contain nothing that can execute, and it must stay MUCH shorter than the
// download allowlist: "safe to save" and "safe to render" are different questions,
// and answering the second with the first's list is the mistake to prevent.
func TestInlinePreviewTypesRenderNothingThatExecutes(t *testing.T) {
	for _, banned := range []string{
		"text/html", "application/xhtml+xml", "image/svg+xml",
		"text/xml", "application/xml", "application/rss+xml",
		"application/javascript", "text/javascript",
		// The bucket every unlisted type is downgraded into. If it were ever
		// previewable, safeContentType's downgrade would stop protecting anything.
		"application/octet-stream",
	} {
		if inlinePreviewTypes[banned] {
			t.Errorf("inlinePreviewTypes contains %q — it would be served INLINE from our own origin", banned)
		}
	}
	if !inlinePreviewTypes["application/pdf"] {
		t.Error("inlinePreviewTypes lost application/pdf — the preview route now renders nothing")
	}
	if len(inlinePreviewTypes) > len(downloadableTypes) {
		t.Errorf("inlinePreviewTypes (%d) is larger than downloadableTypes (%d): "+
			"the set of things safe to RENDER cannot exceed the set safe to SAVE",
			len(inlinePreviewTypes), len(downloadableTypes))
	}
	// Everything renderable must also be downloadable — the preview route runs its
	// candidate through safeContentType first, so a type absent from that allowlist
	// could never reach the inline check anyway, and an entry here that is not there
	// is dead code pretending to be a feature.
	for ct := range inlinePreviewTypes {
		if !downloadableTypes[ct] {
			t.Errorf("inlinePreviewTypes has %q but downloadableTypes does not: unreachable", ct)
		}
	}
}

// The inline disposition is the one thing this route does that the download route
// must never do. It still has to encode a hostile filename exactly as carefully.
func TestInlineDispositionIsInlineAndCannotForgeHeaders(t *testing.T) {
	if got := inlineDisposition(sanitizeFilename("invoice.pdf")); !strings.HasPrefix(got, "inline;") {
		t.Errorf("inlineDisposition = %q, want an inline disposition", got)
	}
	if got := inlineDisposition(""); !strings.Contains(got, `filename="preview"`) {
		t.Errorf("inlineDisposition(\"\") = %q, want a neutral default name", got)
	}
	for _, in := range []string{
		"a\r\nSet-Cookie: pwned=1",
		"a\nX-Evil: 1",
		`a"; filename="b`,
		"../../etc/passwd",
		"\x00nul",
	} {
		got := inlineDisposition(sanitizeFilename(in))
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Errorf("inlineDisposition(%q) = %q: contains a line break", in, got)
		}
		quoted := strings.SplitN(got, `"`, 3)
		if len(quoted) != 3 {
			t.Fatalf("inlineDisposition(%q) = %q: malformed quoted filename", in, got)
		}
		if strings.ContainsAny(quoted[1], `"\/`) {
			t.Errorf("inlineDisposition(%q) = %q: filename escapes its quotes", in, got)
		}
	}
}

// The preview response's headers, asserted as a set. Two of them differ from the
// download response's ON PURPOSE and the rest must not: this is where a copy-paste
// of writeBlobHeaders would silently drop the sandbox.
func TestPreviewResponseHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writePreviewHeaders(rec, "application/pdf", "invoice.pdf", 1234, `"deadbeef.2"`)
	h := rec.Header()

	for k, want := range map[string]string{
		"Content-Type":           "application/pdf",
		"X-Content-Type-Options": "nosniff",
		// THE LOAD-BEARING HEADER. `sandbox` gives the served document an opaque
		// origin however it is embedded — verified in a browser: without it the
		// embedder can reach into the document; with it, it cannot and no script
		// runs. It is here rather than on the client's iframe so that it cannot be
		// forgotten by an edit to a JSX element.
		"Content-Security-Policy": "default-src 'none'; sandbox; frame-ancestors 'self'",
		// Looser than the download response's 'none'/DENY, and it has to be: a
		// browser enforces frame-ancestors 'none' by refusing the load, which is the
		// entire feature. Relaxed to our own origin and no further.
		"X-Frame-Options":              "SAMEORIGIN",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if disp := h.Get("Content-Disposition"); !strings.HasPrefix(disp, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline", disp)
	}
	// The sandbox must not have been handed any escape tokens.
	csp := h.Get("Content-Security-Policy")
	for _, banned := range []string{"allow-same-origin", "allow-scripts", "allow-forms", "allow-popups"} {
		if strings.Contains(csp, banned) {
			t.Errorf("preview CSP grants %q: %q", banned, csp)
		}
	}
	// frame-ancestors must name US and nothing else — 'self' only, never a wildcard
	// or a remote origin, or another site could frame the operator's attachments.
	if strings.Contains(csp, "frame-ancestors *") || strings.Contains(csp, "http") {
		t.Errorf("preview CSP frame-ancestors is not restricted to 'self': %q", csp)
	}
}

// A bare blobId names raw RFC 5322 (or an unattached upload). Neither is a
// document we have any business rendering, so the whole-blob case is refused
// before authorization is even consulted.
func TestPreviewRefusesAWholeBlob(t *testing.T) {
	a := New(nil)
	const hash = "966f0000000000000000000000000000000000000000000000000000000000e6"
	req := httptest.NewRequest(http.MethodGet, "/jmap/preview/acct/"+hash+"/x.pdf", nil)
	req.SetPathValue("accountId", "acct")
	req.SetPathValue("blobId", hash)
	req.SetPathValue("name", "x.pdf")
	rec := httptest.NewRecorder()
	// No auth context: this must be a 401 rather than reaching the whole-blob path
	// at all. The refusal itself is asserted through the real handler in the
	// integration suite, where a session exists.
	a.PreviewHandler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 before any blob decision", rec.Code)
	}
}

// A filename is not only a header value — it is what the browser writes to
// DISK, and the two have different failure modes. A scoping bidi control forges
// no header and injects nothing, so TestFilenameCannotForgeHeaders above passes
// on one happily; it is dangerous for an unrelated reason. U+202E RIGHT-TO-LEFT
// OVERRIDE means "report\u202Egnp.exe" is saved under that exact name and drawn
// by every file manager as "reportexe.png" — an executable wearing an image's
// extension, chosen by whoever sent the mail.
func TestFilenameCannotDisguiseAnExtensionWithBidi(t *testing.T) {
	const rlo = "\u202e"
	for in, want := range map[string]string{
		"report" + rlo + "gnp.exe":      "reportgnp.exe",
		"\u202dinvoice.pdf":             "invoice.pdf",
		"a\u2066b\u2067c\u2068d\u2069e": "abcde",
		"\u202a\u202b\u202c":            "", // nothing but controls leaves nothing
		"ordinary\u200bname.pdf":        "ordinary\u200bname.pdf",
	} {
		got := sanitizeFilename(in)
		if got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
		// ⚠ THE HEADER MUST BE CHECKED IN BOTH REPRESENTATIONS. filename* is
		// RFC 5987 percent-encoded, so a surviving override appears there as
		// %E2%80%AE — a test that looked only for the raw rune would pass while
		// the file still landed on disk misnamed.
		hdr := contentDisposition(got)
		if strings.Contains(hdr, rlo) {
			t.Errorf("contentDisposition(sanitizeFilename(%q)) = %q: raw override survived", in, hdr)
		}
		if strings.Contains(strings.ToUpper(hdr), "%E2%80%AE") {
			t.Errorf("contentDisposition(sanitizeFilename(%q)) = %q: percent-encoded override survived", in, hdr)
		}
	}
}

// #47 6b: a decrypted-entity part has its own blobId form, "<hash>.e.<path>",
// so it can never be confused with a part of the stored ciphertext.
func TestParseBlobIDDecryptedForm(t *testing.T) {
	h := strings.Repeat("ab", 32)
	ref, err := parseBlobID(h + ".e.1.2")
	if err != nil || !ref.decrypted || ref.part != "1.2" {
		t.Fatalf("parseBlobID(.e.1.2) = %+v, %v", ref, err)
	}
	plain, err := parseBlobID(h + ".1.2")
	if err != nil || plain.decrypted {
		t.Fatalf("a plain part parsed as decrypted: %+v %v", plain, err)
	}
	if partETag(ref) == partETag(plain) {
		t.Error("a decrypted part and the stored part at the same path share an ETag")
	}
	for _, bad := range []string{h + ".e", h + ".e.", h + ".e.x", h + ".x.1", h + ".e.e.1"} {
		if _, err := parseBlobID(bad); err == nil {
			t.Errorf("parseBlobID(%q) accepted it", bad)
		}
	}
}
