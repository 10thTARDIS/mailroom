// SPDX-License-Identifier: AGPL-3.0-only

package main

// The hostile-content and transport-security controls, asserted over the REAL
// mux and the REAL source tree (Forgejo #39, spec §11; docs/hostile-content.md).
//
// Same house pattern as gate_test.go: mountApp builds the real route inventory
// with the real middleware, and the protected leaf handlers are sentinels — so
// what is asserted here is what the server actually serves, not what a
// hand-rolled mux in a test file would have served.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/onboarding"
	imapsync "mailroom/internal/sync"
)

// --- The app shell's CSP -----------------------------------------------------

// The web client is the ONE document on this origin that runs JavaScript. Its
// policy is asserted directive by directive, on the real mux, on BOTH auth paths
// — including on the 302 that bounces an anonymous browser to the sign-in page,
// because that response comes from this origin too.
func TestAppShellSecurityHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{"oidc", oidcTestConfig(t)},
		{"dev-auth", devAuthTestConfig()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := gateHandler(t, tc.cfg)
			// The DEPLOYMENT policy, not the base constant (#460): a cleartext
			// redirect URL adds that one origin to img-src, an https one adds
			// nothing. TestAppCSPForCleartextOriginAdmitsExactlyThatOrigin pins
			// what appCSPFor may change; this pins that the header carries it.
			wantCSP := appCSPFor(tc.cfg.OIDCRedirectURL)

			for _, p := range []probe{
				get("/"),
				get("/index.html"),
				get("/assets/index-abc123.js"),
				get("/inbox/01J0000000000000000000000"), // a client-side route
				get("/nope"),                            // a 404 from the file server
			} {
				rec := do(t, h, p)
				hdr := rec.Header()

				if got := hdr.Get("Content-Security-Policy"); got != wantCSP {
					t.Errorf("%s: CSP = %q\n              want %q", p.url, got, wantCSP)
				}
				for k, want := range map[string]string{
					"X-Content-Type-Options":       "nosniff",
					"X-Frame-Options":              "DENY",
					"Referrer-Policy":              "no-referrer",
					"Cross-Origin-Opener-Policy":   "same-origin",
					"Cross-Origin-Resource-Policy": "same-origin",
				} {
					if got := hdr.Get(k); got != want {
						t.Errorf("%s: %s = %q, want %q", p.url, k, got, want)
					}
				}
				if hdr.Get("Permissions-Policy") == "" {
					t.Errorf("%s: no Permissions-Policy", p.url)
				}
			}
		})
	}
}

// The directives that make the policy a policy. Written out one by one so that
// deleting any of them fails HERE, loudly, with the reason attached — rather
// than silently widening the origin in a diff nobody reads closely.
func TestAppCSPDirectives(t *testing.T) {
	must := map[string]string{
		"default-src 'none'":     "everything is denied unless it is named below",
		"script-src 'self'":      "SPEC §11: no inline scripts. No 'unsafe-inline', no 'unsafe-eval', no nonce",
		"connect-src 'self'":     "an injected script must not be able to exfiltrate off-origin",
		"object-src 'none'":      "no plugins",
		"form-action 'none'":     "form-action does NOT fall back to default-src — it must be stated",
		"base-uri 'none'":        "base-uri does NOT fall back to default-src — a <base> hijack rewrites every relative URL",
		"frame-ancestors 'none'": "nobody frames our mail (clickjacking)",
		"img-src":                "see the appCSP comment: https: is headroom for the opt-in remote-content affordance",
		"frame-src 'self'":       "the message iframe, and the #131 attachment-preview frame (same-origin)",
		"worker-src 'self'":      "the Web Push service worker registers from an external module (#42) — never inline",
	}
	for directive, why := range must {
		if !strings.Contains(appCSP, directive) {
			t.Errorf("appCSP lost %q — %s\n  got: %s", directive, why, appCSP)
		}
	}

	// The ones that must NEVER appear. script-src is the whole ballgame: a single
	// 'unsafe-inline' here would turn any HTML-injection bug in the shell into
	// full session compromise, which is precisely the outcome this issue exists
	// to prevent.
	for _, banned := range []string{"'unsafe-inline'; script", "'unsafe-eval'", "script-src *", "default-src *"} {
		if strings.Contains(appCSP, banned) {
			t.Errorf("appCSP contains %q: %s", banned, appCSP)
		}
	}
	scriptSrc := directiveOf(appCSP, "script-src")
	for _, banned := range []string{"'unsafe-inline'", "'unsafe-eval'", "*", "http:", "https:", "data:"} {
		if strings.Contains(scriptSrc, banned) {
			t.Errorf("script-src contains %q — it must be exactly 'self': %q", banned, scriptSrc)
		}
	}
	// img-src carries https: ON PURPOSE (the srcdoc message iframe inherits this
	// policy, and the user's explicit "Show remote content" needs the headroom —
	// see the appCSP comment). It must not carry http:: a remote image is opt-in,
	// but it is not opt-in over cleartext.
	imgSrc := directiveOf(appCSP, "img-src")
	if strings.Contains(strings.ReplaceAll(imgSrc, "https:", ""), "http:") {
		t.Errorf("img-src permits cleartext http: %q", imgSrc)
	}
	if strings.Contains(imgSrc, "*") {
		t.Errorf("img-src is a wildcard: %q", imgSrc)
	}
	// frame-src is EXACTLY 'self' and must stay that way. It carries both frames
	// this app has: the srcdoc message body, and the #131 attachment preview, which
	// is a SAME-ORIGIN url precisely so that this directive does not have to widen.
	//
	// blob: was tried for the preview and is not here on purpose. A sandboxed iframe
	// cannot load a blob: URL at all (Chromium: net::ERR_BLOCKED_BY_CLIENT), so the
	// blob route could only work by dropping the frame's sandbox — trading an origin
	// boundary for a CSP source. Serving the preview from our own path keeps both.
	frameSrc := directiveOf(appCSP, "frame-src")
	for _, banned := range []string{"*", "http:", "https:", "data:", "blob:", "'unsafe-inline'"} {
		if strings.Contains(frameSrc, banned) {
			t.Errorf("frame-src contains %q — it must be exactly 'self': %q", banned, frameSrc)
		}
	}
}

// directiveOf pulls one directive out of a CSP string.
// TestAppCSPForCleartextOriginAdmitsExactlyThatOrigin pins #460: a deployment
// reached over http:// gets its OWN origin added to img-src and nothing else —
// not the bare http: scheme, not a wildcard, and no other directive touched.
// An https deployment (or a missing/garbled URL) gets appCSP unchanged.
//
// Without the origin, every proxied image (/jmap/proxy/...) inside the srcdoc
// message frame was refused by the browser on an http deployment, with no
// server-side trace: a failure found by eye in a browser.
func TestAppCSPForCleartextOriginAdmitsExactlyThatOrigin(t *testing.T) {
	got := appCSPFor("http://mail.example.test:8090/auth/callback")
	imgSrc := directiveOf(got, "img-src")
	if !strings.Contains(imgSrc, " http://mail.example.test:8090") {
		t.Errorf("img-src does not admit the cleartext deployment origin: %q", imgSrc)
	}
	for _, tok := range strings.Fields(imgSrc) {
		if tok == "http:" || tok == "*" || tok == "http://*" {
			t.Errorf("img-src admits %q — only the one explicit origin may be added: %q", tok, imgSrc)
		}
	}
	if strings.Replace(got, " http://mail.example.test:8090", "", 1) != appCSP {
		t.Errorf("appCSPFor changed something other than img-src: got %q base %q", got, appCSP)
	}
	for _, u := range []string{"https://mail.example.test/auth/callback", "", "not a url", "ftp://x/y", "http://"} {
		if appCSPFor(u) != appCSP {
			t.Errorf("appCSPFor(%q) altered the policy; only an http:// origin may: %q", u, appCSPFor(u))
		}
	}
	// And the shipped route table uses it: the header on a response must be the
	// deployment policy, not the base constant, or this function is decoration.
	rec := httptest.NewRecorder()
	secureHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
		false, got).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Content-Security-Policy") != got {
		t.Errorf("secureHeaders did not emit the deployment policy it was given")
	}
}

func directiveOf(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, name+" ") || d == name {
			return d
		}
	}
	return ""
}

// HSTS is emitted only when the instance is actually served over TLS. Sending it
// from a plain-http dev instance would pin http://localhost to https and brick
// it; NOT sending it from a real HTTPS deployment leaves the first request
// downgradeable.
func TestHSTSFollowsTheDeploymentScheme(t *testing.T) {
	httpsCfg := oidcTestConfig(t)
	httpsCfg.OIDCRedirectURL = "https://mail.example.com/auth/callback"
	h, _ := gateHandler(t, httpsCfg)
	if got := do(t, h, get("/")).Header().Get("Strict-Transport-Security"); got == "" {
		t.Errorf("no HSTS on an https:// deployment")
	}

	// ...and the same signal that decides Secure on the session cookie, so the two
	// can never disagree.
	plain, _ := gateHandler(t, oidcTestConfig(t)) // http://localhost:8080/...
	if got := do(t, plain, get("/")).Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS = %q on a plain-http instance — that pins http://localhost to https forever", got)
	}
}

// The sign-in page keeps its OWN, stricter policy: it pins its inline CSS and JS
// by sha256 and permits no external fetch at all (auth/page.go). Wrapping it in
// the app shell's policy would REPLACE that with a weaker one, so gateOpen routes
// are deliberately not wrapped — assert it, because "deliberately not wrapped" is
// one refactor away from "accidentally not hardened".
func TestSignInPageKeepsItsOwnStricterCSP(t *testing.T) {
	h, _ := gateHandler(t, oidcTestConfig(t))
	rec := do(t, h, get(loginPath))

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == appCSP {
		t.Fatalf("the sign-in page is serving the app shell's CSP instead of its own hash-pinned one")
	}
	if !strings.Contains(csp, "'sha256-") {
		t.Errorf("the sign-in page's CSP does not pin its inline script/style by hash: %q", csp)
	}
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("the sign-in page's CSP is not default-src 'none': %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("the sign-in page is not nosniff")
	}
}

// --- The web client's source ------------------------------------------------

// webRoot locates web/ from cmd/mailroom/.
func webRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "web"))
	if err != nil {
		t.Fatalf("web root: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("web/ is not present: %v", err)
	}
	return root
}

// `script-src 'self'` is only enforceable if the shipped page has no inline
// script. Vite emits an external module script, but a build-tool upgrade (or a
// plugin) can start inlining a module-preload shim — and the failure mode is the
// app silently not booting under CSP, which is exactly the kind of thing that
// gets "fixed" by adding 'unsafe-inline'. So the SOURCE index.html is asserted
// here, and the BUILT one is asserted against the real image in
// scripts/hostile-content-e2e.sh.
func TestWebIndexHasNoInlineScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(webRoot(t), "index.html"))
	if err != nil {
		t.Fatalf("read web/index.html: %v", err)
	}
	assertNoInlineScript(t, "web/index.html", string(b))
}

// assertNoInlineScript fails if any <script> element in html has a body.
func assertNoInlineScript(t *testing.T, name, html string) {
	t.Helper()
	low := strings.ToLower(html)
	for i := 0; ; {
		open := strings.Index(low[i:], "<script")
		if open < 0 {
			return
		}
		open += i
		gt := strings.Index(low[open:], ">")
		if gt < 0 {
			return
		}
		close := strings.Index(low[open:], "</script")
		if close < 0 {
			return
		}
		body := strings.TrimSpace(html[open+gt+1 : open+close])
		if body != "" {
			t.Errorf("%s has an INLINE script — appCSP is script-src 'self' with no "+
				"'unsafe-inline' and no nonce, so this will not run:\n\t%s", name, body)
		}
		i = open + close + 1
	}
}

// The render path must never put message-derived HTML into OUR DOM. There is
// exactly one safe way to render a hostile body — the sandboxed iframe — and
// this is the assertion that keeps a future "just render it, we sanitize it now"
// from quietly landing.
func TestWebNeverInjectsHTMLIntoOurOwnDOM(t *testing.T) {
	src := filepath.Join(webRoot(t), "src")
	// The rich-text COMPOSE editor is the ONE place allowed to touch innerHTML —
	// and what it touches is NOT attacker-controlled message HTML. Reply quotes
	// are built from the parent's PLAIN text, HTML-escaped; the html→text step
	// parses into an INERT <template> (no rendering, no resource loads, no script
	// or onerror execution); and reads return the user's OWN authored content,
	// re-sanitized server-side on send. Every such line must live in Compose.tsx
	// AND carry the "trusted-html:" marker, and each is justified in
	// docs/adr/ADR-015. The message-RENDERING path is unaffected and still
	// forbidden everywhere. The exact count is pinned so a new use cannot slip in
	// without review (bump it here AND extend ADR-015).
	composeEditor := filepath.FromSlash("app/ui/Compose.tsx")
	const trustedHTMLMarker = "trusted-html:"
	const wantTrustedHTMLUses = 3
	trustedUses := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".ts" && ext != ".tsx" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		for _, line := range strings.Split(string(b), "\n") {
			// A comment SAYING the word is how the rule is documented (ThreadPane).
			// A line that actually USES it is the bug.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			for _, bad := range []string{"dangerouslySetInnerHTML", ".innerHTML", ".outerHTML",
				"insertAdjacentHTML", "document.write"} {
				if !strings.Contains(line, bad) {
					continue
				}
				if rel == composeEditor && strings.Contains(line, trustedHTMLMarker) {
					trustedUses++
					continue // an individually-justified compose-editor use (ADR-015)
				}
				t.Errorf("web/src/%s uses %s:\n\t%s\n"+
					"A message body is attacker-controlled. It is rendered in a sandboxed "+
					"iframe (ui/messageDocument.ts) and nowhere else — see docs/hostile-content.md.",
					rel, bad, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking web/src: %v", err)
	}
	if trustedUses != wantTrustedHTMLUses {
		t.Errorf("found %d trusted-html: compose-editor uses, want exactly %d.\n"+
			"A new innerHTML use in Compose.tsx must be individually justified in ADR-015 and "+
			"this count bumped; a removed one must drop it — silent drift is not allowed.",
			trustedUses, wantTrustedHTMLUses)
	}
}

// THE SANDBOX AT ITS POINT OF USE (Forgejo #298).
//
// TestMessageSandboxIsNotRelaxed, below, asserts the VALUE of the MESSAGE_SANDBOX
// constant. That is necessary and it is not sufficient, and #298 measured exactly
// how insufficient: deleting `sandbox={MESSAGE_SANDBOX}` from the <iframe> in
// ThreadPane.tsx — or writing `sandbox={MESSAGE_SANDBOX + " allow-same-origin"}`
// — left the Go suite, `tsc --noEmit` and all 242 vitest tests GREEN. The
// constant was still correct; nothing was reading it, or something was widening
// it on the way in.
//
// What that regression costs is the whole defence. `allow-scripts` is on this
// frame ON PURPOSE (the #100 height reporter); `allow-scripts` WITH
// `allow-same-origin` lets the framed document reach into the parent origin AND
// remove its own sandbox attribute — full stored XSS from any HTML email against
// the operator's live session. And it would be invisible: mail would still
// render, auto-height would still work, and both e2e suites would stay green,
// because every assertion they make is about what the message CANNOT do, and a
// same-origin frame passes all of them while being able to do everything.
//
// So this walks the real source, finds every <iframe> element the client
// actually writes, and requires the message frame to carry the token list
// VERBATIM — `sandbox={MESSAGE_SANDBOX}`, no concatenation, no template literal,
// no spread that could override it. It is the same shape as
// TestWebNeverInjectsHTMLIntoOurOwnDOM: walk web/src, pin the count, and make a
// new use cost a deliberate edit here.
func TestEveryIframeCarriesTheMessageSandbox(t *testing.T) {
	src := filepath.Join(webRoot(t), "src")

	// THE ONE EXEMPTION, and it is an exemption from `sandbox=`, not from being
	// sandboxed. The #131 attachment-preview frame carries NO sandbox attribute
	// deliberately: a sandboxed frame is an opaque origin to the SERVER too, and
	// the preview response's `Cross-Origin-Resource-Policy: same-origin` then
	// refuses the load outright. Its opaque origin comes from that response's own
	// CSP `sandbox` directive instead — which is the stronger place for it, since
	// no edit to this JSX can drop it (internal/api/blob.go, and the reasoning is
	// written out at ui/attachmentPreview.tsx and docs/hostile-content.md §2.4a).
	//
	// The exemption is per FILE, so it is pinned by count as well: otherwise
	// "attachmentPreview.tsx is exempt" would quietly mean "any number of
	// unsandboxed frames may be added to attachmentPreview.tsx".
	exemptFile := filepath.FromSlash("app/ui/attachmentPreview.tsx")
	const wantExemptFrames = 1    // the #131 PDF preview
	const wantSandboxedFrames = 1 // the message body (ThreadPane.tsx)

	const wantSandboxAttr = `sandbox={MESSAGE_SANDBOX}`
	sandboxed, exempt := 0, 0

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".ts" && ext != ".tsx" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(b)
		rel, _ := filepath.Rel(src, path)

		for _, at := range jsxElementStarts(body, "iframe") {
			elem := jsxOpeningTag(body, at)
			if rel == exemptFile {
				exempt++
				// It really must be the one that is documented, i.e. it must carry
				// no sandbox attribute at all. A `sandbox="..."` appearing HERE would
				// be a hand-written token list escaping review entirely.
				if strings.Contains(elem, "sandbox") {
					t.Errorf("web/src/%s: the attachment-preview frame grew a sandbox attribute:\n\t%s\n"+
						"Its opaque origin comes from the RESPONSE's CSP sandbox directive "+
						"(internal/api/blob.go). A sandbox attribute here makes the frame opaque to "+
						"the server too, and Cross-Origin-Resource-Policy: same-origin then refuses "+
						"the load — see docs/hostile-content.md §2.4a.", rel, elem)
				}
				continue
			}
			sandboxed++
			if !strings.Contains(elem, wantSandboxAttr) {
				t.Errorf("web/src/%s: an <iframe> that does not carry %s verbatim:\n\t%s\n"+
					"A message body is attacker-controlled HTML. The ONLY thing standing between it "+
					"and the operator's session is this frame's opaque origin, and the opaque origin "+
					"is this attribute. It must be exactly %s — not a concatenation, not a template "+
					"literal, not a variable that merely mentions it: `MESSAGE_SANDBOX + \" allow-same-origin\"` "+
					"renders identically, keeps auto-height working, and hands any HTML email full "+
					"same-origin access (Forgejo #298, docs/hostile-content.md §2.1).",
					rel, wantSandboxAttr, elem, wantSandboxAttr)
			}
			// A JSX spread AFTER the attribute silently wins — `<iframe
			// sandbox={MESSAGE_SANDBOX} {...rest} />` is a sandbox the caller
			// chooses. There is no reason to spread props onto this element, so
			// the presence of one is the finding.
			if strings.Contains(elem, "{...") {
				t.Errorf("web/src/%s: the message iframe carries a JSX spread:\n\t%s\n"+
					"A later spread OVERRIDES `sandbox`, so the attribute above stops being a "+
					"guarantee and becomes a default the caller may replace.", rel, elem)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking web/src: %v", err)
	}

	// THE COUNTS ARE THE NEGATIVE CONTROL. Without them this test passes
	// vacuously the day the scanner stops finding the element it is asserting
	// about — which is the failure mode #298 is a case of, one level up.
	if sandboxed != wantSandboxedFrames {
		t.Errorf("found %d sandboxed <iframe> element(s) in web/src, want exactly %d.\n"+
			"If this is 0, this test just stopped asserting anything. If it is more, a new frame "+
			"was added: justify it in docs/hostile-content.md and bump the count here.",
			sandboxed, wantSandboxedFrames)
	}
	if exempt != wantExemptFrames {
		t.Errorf("found %d <iframe> element(s) in web/src/%s, want exactly %d — "+
			"the exemption is for ONE documented frame, not for the file.",
			exempt, exemptFile, wantExemptFrames)
	}
}

// jsxElementStarts returns the offsets of every `<name` that opens a real JSX
// element in src — skipping the ones that appear on a comment line, because
// naming an element in prose (this repository does it constantly) must not fail
// a test about the element.
func jsxElementStarts(src, name string) []int {
	var out []int
	for _, line := range lineSpans(src) {
		text := src[line.start:line.end]
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") ||
			strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "{/*") {
			continue
		}
		for i := 0; ; {
			j := strings.Index(text[i:], "<"+name)
			if j < 0 {
				break
			}
			out = append(out, line.start+i+j)
			i += j + 1
		}
	}
	return out
}

type span struct{ start, end int }

func lineSpans(src string) []span {
	var out []span
	start := 0
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			out = append(out, span{start, i})
			start = i + 1
		}
	}
	return append(out, span{start, len(src)})
}

// jsxOpeningTag returns the text of the JSX opening tag beginning at src[at],
// through the '>' that closes it. A '>' inside a braced expression (an arrow
// function, a comparison) or inside a quoted string does not end the tag, so
// brace depth and string state are both tracked — otherwise a multi-line element
// would be truncated to its first line and the attribute we are looking for
// would go missing for the wrong reason.
func jsxOpeningTag(src string, at int) string {
	depth := 0
	var quote byte
	for i := at; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '{':
			depth++
		case '}':
			depth--
		case '>':
			if depth == 0 {
				return src[at : i+1]
			}
		}
	}
	return src[at:]
}

// The message render sandbox, pinned. These are the exact tokens
// ui/messageDocument.ts must carry; the behavioural proof is the real-browser
// run in scripts/hostile-content-e2e.sh, and this is the tripwire that fails in
// CI (with no browser) the moment somebody relaxes one of them.
//
// It asserts the CONSTANT. TestEveryIframeCarriesTheMessageSandbox, above,
// asserts that the constant is what the <iframe> actually receives — the two
// together are the assertion; either alone passes while the other's failure is
// live (Forgejo #298).
func TestMessageSandboxIsNotRelaxed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(webRoot(t), "src", "app", "ui", "messageDocument.ts"))
	if err != nil {
		t.Fatalf("read messageDocument.ts: %v", err)
	}
	src := string(b)

	// The sandbox token list, exactly. allow-scripts is present (ADDED for #100
	// content-fit auto-height) and is safe ONLY because the frame stays opaque AND
	// the CSP hash-pins the single reporter (docs/hostile-content.md §2.1).
	// allow-same-origin is the token that must NEVER appear: WITH allow-scripts it
	// would let the frame delete its own sandbox attribute.
	const wantSandbox = `export const MESSAGE_SANDBOX = "allow-popups allow-popups-to-escape-sandbox allow-scripts";`
	if !strings.Contains(src, wantSandbox) {
		t.Errorf("the message iframe's sandbox changed. Expected exactly:\n\t%s\n"+
			"If you are adding a token, say why in docs/hostile-content.md first. "+
			"allow-same-origin must NEVER be added.", wantSandbox)
	}

	// The message document's CSP.
	for _, want := range []string{
		`"default-src 'none'"`,
		`script-src '${REPORTER_SHA256}'`, // the ONLY script: the hash-pinned reporter
		`"style-src 'unsafe-inline'"`,
		`"font-src data:"`,
		`"form-action 'none'"`,                           // does not fall back to default-src
		`"base-uri 'none'"`,                              // does not fall back to default-src
		"allowRemote ? `data: https:${own}` : \"data:\"", // remote images: OFF by default; the app origin only when ON (#460)
	} {
		if !strings.Contains(src, want) {
			t.Errorf("messageDocument.ts lost the CSP directive %s", want)
		}
	}

	// The reporter is pinned by sha256 in BOTH the message CSP (here) and appCSP.
	// If they drift, the srcdoc-inheritance intersection bans the reporter and
	// content-fit auto-height dies silently.
	const reporterHash = "sha256-1XkLCQZRSIgsRrQKKNvaJTcjTinrA6u/CFBnA4fTy+4="
	if !strings.Contains(src, reporterHash) {
		t.Errorf("messageDocument.ts no longer pins the reporter hash %s", reporterHash)
	}
	if !strings.Contains(appCSP, reporterHash) {
		t.Errorf("appCSP must ALSO name the reporter hash %s — the message frame "+
			"inherits appCSP and the policies intersect; without it the reporter is "+
			"blocked and auto-height breaks (docs/hostile-content.md §2.1)", reporterHash)
	}

	// The banned tokens are checked against the CODE only, not the comments — the
	// file explains at length what it does NOT do, and naming a thing in order to
	// forbid it must not fail the test that forbids it. allow-scripts and
	// script-src are NO LONGER banned (they are the #100 reporter); allow-same-origin
	// and the rest still are.
	code := stripComments(src)
	for _, banned := range []string{"allow-same-origin", "allow-forms",
		"allow-top-navigation", "allow-modals", "unsafe-eval"} {
		if strings.Contains(code, banned) {
			t.Errorf("the message sandbox/CSP now contains %q — see docs/hostile-content.md", banned)
		}
	}

	// The names the SERVER uses must match the ones the client restores from
	// (internal/api/sanitize.go: remoteSrcAttr / remoteSrcsetAttr / blockedAttr).
	// The Go side asserts its half in TestRemoteContentContract; if they drift,
	// the "Show remote content" button silently stops working.
	for _, want := range []string{
		`data-mailroom-blocked`,
		`'data-mailroom-remote-src="'`,
		`'data-mailroom-remote-srcset="'`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("messageDocument.ts no longer agrees with internal/api/sanitize.go about %s", want)
		}
	}
}

// stripComments removes // line comments and /* */ blocks from TypeScript source,
// so an assertion about the POLICY is not confused by prose about the policy.
func stripComments(src string) string {
	var b strings.Builder
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case inBlock:
			if strings.Contains(t, "*/") {
				inBlock = false
			}
			continue
		case strings.HasPrefix(t, "/*"):
			if !strings.Contains(t, "*/") {
				inBlock = true
			}
			continue
		case strings.HasPrefix(t, "//"), strings.HasPrefix(t, "*"):
			continue
		}
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// --- TLS everywhere ----------------------------------------------------------

// InsecureSkipVerify exists on onboarding.AccountSpec and sync.Config "for the
// test harness only". This proves it: there is NO PATH from an operator's
// configuration to it.
//
// Three independent locks, because one of them is a comment and comments do not
// enforce anything:
//
//  1. config.Account — the accounts file's schema — has no field that could
//     carry it, and no field that could select a TLS mode at all.
//  2. LoadAccountsFile uses DisallowUnknownFields, so an operator who WRITES
//     "insecure_skip_verify": true into the file gets a hard boot failure rather
//     than a silently-ignored key that looks like it worked.
//  3. accountSpec — the one function that turns a declared account into an
//     onboarding spec — pins TLS to implicit and leaves InsecureSkipVerify false,
//     whatever is in the Account.
func TestAccountsFileCannotDisableTLSVerification(t *testing.T) {
	// 1. The schema has no door.
	for _, field := range structFields(config.Account{}) {
		lower := strings.ToLower(field)
		for _, forbidden := range []string{"insecure", "skipverify", "tls", "starttls", "verify"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("config.Account has a field %q — the accounts file must not be able to "+
					"weaken transport security", field)
			}
		}
	}

	// 2. Writing one anyway is a HARD ERROR, not a silent no-op.
	dir := t.TempDir()
	for _, key := range []string{"insecure_skip_verify", "tls", "tls_mode", "starttls", "insecure"} {
		path := filepath.Join(dir, key+".json")
		body := `{"accounts":[{"name":"a","imap_host":"mail.example.com","smtp_host":"mail.example.com",` +
			`"username":"a@example.com","password":"p","` + key + `":true}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadAccountsFile(path); err == nil {
			t.Errorf("an accounts file with %q was ACCEPTED — an operator would think it worked", key)
		}
	}

	// 3. accountSpec pins it, for every account.
	spec := accountSpec(config.Account{
		Name: "a", IMAPHost: "mail.example.com", IMAPPort: 143,
		SMTPHost: "mail.example.com", SMTPPort: 25,
		Username: "a@example.com", Password: "p",
	})
	if spec.TLS != imapsync.TLSImplicit {
		t.Errorf("accountSpec TLS = %v, want TLSImplicit — a real account always uses implicit TLS", spec.TLS)
	}
	if spec.InsecureSkipVerify {
		t.Error("accountSpec set InsecureSkipVerify")
	}
	// The zero value must be the SAFE value: an AccountSpec built anywhere, by
	// anyone, that forgets to say anything about TLS, gets verification.
	var zero onboarding.AccountSpec
	if zero.InsecureSkipVerify {
		t.Error("the zero onboarding.AccountSpec skips certificate verification")
	}
	if zero.TLS != imapsync.TLSImplicit {
		t.Errorf("the zero AccountSpec's TLS mode is %v, want TLSImplicit (the zero value must be the safe value)",
			zero.TLS)
	}
}

// structFields returns the field names of a struct value.
func structFields(v any) []string {
	rt := reflect.TypeOf(v)
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		out = append(out, rt.Field(i).Name)
	}
	return out
}

// NO PRODUCTION FILE, ANYWHERE IN THIS REPOSITORY, MAY TURN OFF TLS.
//
// This walks the actual source tree with go/ast and fails if any non-test file
// sets InsecureSkipVerify or AllowInsecureNoTLS to `true`, or names the
// TLSInsecure / TLSStartTLS mode. It covers packages this change does not own
// (internal/sync, internal/syncrunner, internal/onboarding) without editing
// them, and — unlike a comment that says "test harness only" — it keeps being
// true tomorrow.
//
// Assignments that FORWARD the field (`InsecureSkipVerify: spec.InsecureSkipVerify`)
// are fine and are the plumbing that lets a test harness set it: what is
// forbidden is originating a `true`.
func TestNoProductionCodeDisablesTLS(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	const (
		skipVerify = "InsecureSkipVerify"
		noTLS      = "AllowInsecureNoTLS"
	)
	// TLSInsecure/TLSStartTLS are DECLARED in internal/sync/config.go and switched
	// on in internal/sync/session.go — that is the definition of the enum, not a
	// use of it. Every other production file must name only TLSImplicit.
	//
	// internal/census/imap.go declares its own TLSMode enum for the same
	// reason internal/sync/config.go does (restated, not imported, because
	// internal/sync's SyncSession has write methods — see reader.go).
	// TLSInsecure exists there for the Dovecot harness only:
	// cmd/mailroom-census has no -tls flag and always dials TLSImplicit, so
	// that file is NOT in this allowlist.
	declaresTLSModes := map[string]bool{
		filepath.Join("internal", "sync", "config.go"):  true,
		filepath.Join("internal", "sync", "session.go"): true,
		filepath.Join("internal", "census", "imap.go"):  true,
	}

	fset := token.NewFileSet()
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil // a package mid-edit by another agent; not our business
			}

			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.KeyValueExpr:
					key, ok := node.Key.(*ast.Ident)
					if !ok || (key.Name != skipVerify && key.Name != noTLS) {
						return true
					}
					if lit, ok := node.Value.(*ast.Ident); ok && lit.Name == "true" {
						t.Errorf("%s:%d: %s: true — TLS verification is not optional in production. "+
							"If this is a test harness, the file must end in _test.go.",
							rel, fset.Position(node.Pos()).Line, key.Name)
					}
				case *ast.AssignStmt:
					for i, lhs := range node.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || (sel.Sel.Name != skipVerify && sel.Sel.Name != noTLS) {
							continue
						}
						if i < len(node.Rhs) {
							if lit, ok := node.Rhs[i].(*ast.Ident); ok && lit.Name == "true" {
								t.Errorf("%s:%d: assigns %s = true in production code",
									rel, fset.Position(node.Pos()).Line, sel.Sel.Name)
							}
						}
					}
				case *ast.Ident:
					if declaresTLSModes[rel] {
						return true
					}
					if node.Name == "TLSInsecure" || node.Name == "TLSStartTLS" {
						t.Errorf("%s:%d: names sync.%s — a real account dials implicit TLS (993/465); "+
							"the other modes exist only for the Dovecot test harness",
							rel, fset.Position(node.Pos()).Line, node.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
}

// The web handler must not autoindex the build output.
func TestWebHandlerServesNoDirectoryListing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.html", filepath.Join("assets", "index-abc123.js")} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	h := webHandler(root, quietLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /assets/ = %d (%q), want 404 — http.FileServer's autoindex is a free map of the app",
			rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "index-abc123.js") {
		t.Errorf("the directory listing leaked the build output:\n%s", rec.Body.String())
	}

	// ...and the files themselves still serve.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/index-abc123.js", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /assets/index-abc123.js = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET / = %d, want 200 (the index is still served)", rec.Code)
	}
}

// buildAppHandler is what main() actually calls; the tests above drive mountApp.
// This proves the production constructor puts the same headers on the same
// surface — so the assertions are not testing a fiction.
func TestBuildAppHandlerHardensTheRealWebHandler(t *testing.T) {
	cfg := oidcTestConfig(t)
	cfg.WebRoot = t.TempDir()

	h, _, err := buildAppHandler(context.Background(), nil, cfg, ulid.Make(), nil, nil, nil, nil, nil, nil, oauthWiring{}, nil, quietLogger())
	if err != nil {
		t.Fatalf("buildAppHandler: %v", err)
	}
	rec := do(t, h, get("/"))
	// oidcTestConfig is a cleartext http://localhost:8080 deployment, so the real
	// handler must serve the #460 policy — appCSP plus that one origin in
	// img-src — or the fix is a function nothing calls.
	wantCSP := appCSPFor(cfg.OIDCRedirectURL)
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("the REAL app handler serves CSP %q, want %q", got, wantCSP)
	}
	if !strings.Contains(directiveOf(rec.Header().Get("Content-Security-Policy"), "img-src"), " http://localhost:8080") {
		t.Errorf("the REAL app handler on a cleartext deployment does not admit its own origin for images (#460)")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("the REAL app handler does not send nosniff")
	}

	// And every JMAP response carries nosniff too (internal/api's own middleware).
	for _, p := range []probe{get("/jmap/session"), post("/jmap"), get("/jmap/eventsource/")} {
		if got := do(t, h, p).Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", p.url, got)
		}
	}
}
