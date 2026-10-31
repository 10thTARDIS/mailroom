// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func testPage(t *testing.T) *Page {
	t.Helper()
	p, err := NewPage(PageConfig{StartPath: "/auth/start"})
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	return p
}

func render(t *testing.T, p *Page, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestNewPageRequiresAStartPath(t *testing.T) {
	if _, err := NewPage(PageConfig{}); err == nil {
		t.Fatal("NewPage with no StartPath = nil error; a sign-in page with nowhere to sign in is not a page")
	}
	if _, err := NewPage(PageConfig{DevAuth: true}); err != nil {
		t.Fatalf("NewPage(DevAuth) = %v; the dev-auth page has no sign-in by design", err)
	}
}

func TestPageRenders(t *testing.T) {
	rec := render(t, testPage(t), "/auth/login")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<html lang="en"`,
		"Mailroom",
		t2("signin.heading"),
		t2("signin.tagline"),
		`href="/auth/start"`,
		`<h1`,
		`role="group"`, // the theme control, labelled
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	// A sign-in page must never be cached — it is what a shared browser lands on
	// after a logout.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// The page must never pin a theme in the markup: with JavaScript off, the CSS's
// prefers-color-scheme rules are the ONLY thing that can follow the system, and
// a data-theme attribute would outrank them.
func TestPageDoesNotPinAThemeInMarkup(t *testing.T) {
	body := render(t, testPage(t), "/auth/login").Body.String()
	head, _, _ := strings.Cut(body, "<head>")
	if strings.Contains(head, "data-theme") {
		t.Errorf("<html> carries a data-theme attribute; that defeats system dark mode with JS disabled:\n%s", head)
	}
	if !strings.Contains(body, "prefers-color-scheme: dark") {
		t.Errorf("the stylesheet has no system dark mode")
	}
}

// --- Error surfacing ---------------------------------------------------------

func TestPageSurfacesEachErrorCode(t *testing.T) {
	tests := []struct {
		query string
		want  string
		role  string
	}{
		{"?error=" + ErrorDenied, t2("signin.error.denied"), "alert"},
		{"?error=" + ErrorExpired, t2("signin.error.expired"), "alert"},
		{"?error=" + ErrorFailed, t2("signin.error.failed"), "alert"},
		{"?error=" + ErrorUnprovisionable, t2("signin.error.unprovisionable"), "alert"},
		{"?notice=" + NoticeSignedOut, t2("signin.notice.signedOut"), "status"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			body := render(t, testPage(t), "/auth/login"+tt.query).Body.String()
			// The rendered text is HTML-escaped, so compare on a distinctive
			// escaping-free fragment of the message.
			frag := firstWords(tt.want, 5)
			if !strings.Contains(body, frag) {
				t.Errorf("page does not show %q:\n%s", frag, body)
			}
			if !strings.Contains(body, `role="`+tt.role+`"`) {
				t.Errorf("the message is not announced with role=%q", tt.role)
			}
		})
	}
}

// The denial is the one a user can act on, and it must say true things about
// what actually reaches ErrorDenied: a disabled/deleted user, or a token with
// no email claim — NEVER "the wrong account" (Forgejo #574 found that claim
// false: since #98, any identity that completes OIDC is auto-provisioned).
//
// This test used to assert the OLD, false wording ("not the account this
// instance is configured for", "Mailroom never creates one automatically").
// #574 INVERTS it rather than deleting it: the old claims must now be ABSENT,
// and the new, true ones present.
func TestDeniedMessageIsHonest(t *testing.T) {
	msg := t2("signin.error.denied")
	for _, want := range []string{"cannot use this Mailroom", "disabled", "Ask whoever runs this instance"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the denial message does not say %q: %q", want, msg)
		}
	}
	for _, mustNotSay := range []string{
		"not the account this instance is configured for",
		"never creates one automatically",
	} {
		if strings.Contains(msg, mustNotSay) {
			t.Errorf("the denial message repeats the pre-#574 false claim %q: %q", mustNotSay, msg)
		}
	}
}

// The unprovisionable message (Forgejo #574) must say the identity WAS
// verified (so the user does not suspect their own credentials) and that the
// fix is on the SERVER, not something the user can do by signing in again.
// It must not blame the configuration: a provider that re-issued a subject
// reaches this too, and nothing is misconfigured then.
func TestUnprovisionableMessageIsHonest(t *testing.T) {
	msg := t2("signin.error.unprovisionable")
	for _, want := range []string{"was verified", "Signing in again will not fix it", "fixed on the server"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the unprovisionable message does not say %q: %q", want, msg)
		}
	}
}

// An unrecognized code is not an error message — and, crucially, is never echoed.
func TestUnknownCodesAreIgnoredAndNeverReflected(t *testing.T) {
	const payload = "SomethingTheAttackerWrote"
	body := render(t, testPage(t), "/auth/login?error="+payload+"&notice="+payload).Body.String()

	if strings.Contains(body, payload) {
		t.Errorf("the page reflected a request-supplied error code — a fixed set of codes, or nothing:\n%s", body)
	}
	if strings.Contains(body, `class="alert`) {
		t.Errorf("an unknown code produced an alert; it must produce nothing")
	}
}

// return_to is carried onto the sign-in action, but only when it is a same-origin
// path: an absolute URL is an open redirect through the login flow, and is
// dropped (sanitizeReturnTo — the same function LoginHandler uses).
func TestReturnToIsCarriedAndSanitized(t *testing.T) {
	body := render(t, testPage(t), "/auth/login?return_to=%2Finbox%2F1").Body.String()
	if !strings.Contains(body, "/auth/start?return_to=%2Finbox%2F1") {
		t.Errorf("return_to was not carried onto the sign-in action:\n%s", body)
	}

	for _, evil := range []string{"https%3A%2F%2Fevil.example.com", "%2F%2Fevil.example.com", "javascript%3Aalert(1)"} {
		body := render(t, testPage(t), "/auth/login?return_to="+evil).Body.String()
		if !strings.Contains(body, `href="/auth/start"`) {
			t.Errorf("return_to=%s was not dropped; the sign-in action is not the bare start path:\n%s", evil, body)
		}
		if strings.Contains(body, "evil.example.com") || strings.Contains(body, "javascript:") {
			t.Errorf("return_to=%s leaked into the page", evil)
		}
	}
}

// --- Content-Security-Policy -------------------------------------------------

// The CSP pins the page's inline style and script by hash. Re-extract them from
// the RENDERED document and re-hash: if html/template ever transforms those bytes
// (or someone edits the template), the policy would silently stop matching and
// the browser would drop the stylesheet — a broken page nobody's test caught.
func TestCSPHashesMatchTheRenderedDocument(t *testing.T) {
	rec := render(t, testPage(t), "/auth/login")
	csp := rec.Header().Get("Content-Security-Policy")
	body := rec.Body.String()

	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want default-src 'none'", csp)
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP = %q, want no 'unsafe-inline' — the inline style and script are hash-pinned", csp)
	}

	style := between(t, body, "<style>", "</style>")
	script := between(t, body, "<script>", "</script>")

	if style != pageCSS {
		t.Fatalf("the rendered <style> is not the pageCSS constant byte-for-byte; the CSP hash cannot match")
	}
	if script != pageJS {
		t.Fatalf("the rendered <script> is not the pageJS constant byte-for-byte; the CSP hash cannot match")
	}
	for _, want := range []string{sha256Source(style), sha256Source(script)} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q does not carry the hash %s of what the page actually inlines", csp, want)
		}
	}
}

// A "</" inside an inlined <style>/<script> ends the element early — the rest of
// the CSS/JS would be parsed as HTML. The constants must not contain one.
func TestInlinedAssetsCannotEscapeTheirElement(t *testing.T) {
	if strings.Contains(pageCSS, "</") {
		t.Error("pageCSS contains \"</\", which would close its <style> element early")
	}
	if strings.Contains(pageJS, "</") || strings.Contains(pageJS, "<") {
		t.Error("pageJS contains \"<\", which html/template may escape (breaking the CSP hash) and which can close its <script> element early")
	}
}

// --- The dev-auth variant ----------------------------------------------------

func TestDevAuthPageSaysTheInstanceAuthenticatesNobody(t *testing.T) {
	p, err := NewPage(PageConfig{DevAuth: true})
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	body := render(t, p, "/auth/login").Body.String()

	if !strings.Contains(body, "no authentication") {
		t.Errorf("the dev-auth page does not say the instance has no authentication:\n%s", body)
	}
	if strings.Contains(body, "/auth/start") {
		t.Errorf("the dev-auth page offers a sign-in action, but there is no login flow to start")
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Errorf("the dev-auth warning is not announced to a screen reader")
	}
}

// --- i18n --------------------------------------------------------------------

// "i18n-ready" (spec §12) means ONE catalog a translator can find, not two that
// quietly disagree. The page's strings live in Go (a Go binary cannot call a
// TypeScript t()), but the SAME keys must exist in web/src/app/i18n.ts — so the
// web catalog stays the canonical, complete list of every user-facing string in
// the product, sign-in page included. This test is what keeps that true.
func TestSignInCatalogMatchesTheWebCatalog(t *testing.T) {
	const webCatalog = "../../web/src/app/i18n.ts"

	src, err := os.ReadFile(webCatalog)
	if err != nil {
		t.Fatalf("read %s: %v", webCatalog, err)
	}
	re := regexp.MustCompile(`"(signin\.[A-Za-z.]+)"\s*:`)

	web := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		web[m[1]] = true
	}

	var missing, extra []string
	for k := range enSignIn {
		if !web[k] {
			missing = append(missing, k)
		}
	}
	for k := range web {
		if _, ok := enSignIn[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("these sign-in page strings are missing from %s: %v", webCatalog, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s has signin.* keys the page does not use: %v", webCatalog, extra)
	}
}

// --- helpers -----------------------------------------------------------------

// t2 is the package's t(), renamed so it does not shadow *testing.T inside tests.
func t2(key string) string { return t(key) }

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

func between(t *testing.T, s, openTag, closeTag string) string {
	t.Helper()
	_, rest, ok := strings.Cut(s, openTag)
	if !ok {
		t.Fatalf("no %s in the document", openTag)
	}
	inner, _, ok := strings.Cut(rest, closeTag)
	if !ok {
		t.Fatalf("no %s in the document", closeTag)
	}
	return inner
}
