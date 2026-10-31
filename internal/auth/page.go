// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

// The sign-in page: the ONE surface an unauthenticated visitor is allowed to
// reach (besides the health probes and the OIDC flow endpoints themselves).
//
// # Why it is server-rendered here and not part of the Vite SPA
//
// The web client is served by a single http.FileServer mounted at "/" BEHIND the
// auth middleware (cmd/mailroom). Making the sign-in page a second entry point
// of that bundle would mean serving its JS/CSS to anonymous visitors — and Vite
// emits content-hashed, shared chunks, so the only way to do that is to open the
// whole /assets/ prefix. A prefix hole in the gate cannot be enumerated, cannot
// be asserted on in a route table, and is exactly the "quietly defaults to
// public" failure this gate exists to prevent.
//
// So the page is a single self-contained HTML document with NO external
// resources at all: the CSS and the (tiny) theme script are inlined, pinned by
// Content-Security-Policy sha256 hashes, and there is not one byte to fetch
// afterwards. Consequences that are features:
//
//   - the gate needs zero exceptions for static files;
//   - the front door still works when the SPA build is missing entirely (which
//     is a supported configuration — see cmd/mailroom's webHandler);
//   - it renders before a single line of JS is parsed, and it is fully usable
//     with JavaScript disabled.
//
// Its palette and type are lifted from web/src/app/styles.css so the door and
// the room behind it look like the same building (spec §12), and it honors the
// same rules: solid opaque surfaces (NO transparencies), light + dark with
// system-follow AND a manual override that persists into the app (it writes the
// same localStorage key app/prefs.ts reads), responsive, keyboard-navigable,
// screen-reader-labelled, high-contrast aware.
//
// # Refused logins
//
// A verified identity the resolver refuses is denied BY DESIGN (cmd/mailroom's
// provisioningResolver — app-binding is the access gate, but a token with no
// email to provision from, or a subject an admin has disabled, is still refused).
// CallbackHandler redirects such a login back here with ?error=denied and the
// page SAYS SO, plainly. It never bounces the browser back at the provider: the
// provider still has a valid SSO session, would hand back the very same subject,
// and the user would spin in a redirect loop with nothing on screen ever telling
// them why. The loop is the failure mode; a plain sentence is the fix.
//
// A SEPARATE refusal (?error=unprovisionable, ErrorUnprovisionable — Forgejo
// #574) covers the case where the identity was verified but this SERVER's own
// records stop the resolver from setting up an account for it (the
// token's email already belongs to a different existing user — typically
// MAILROOM_OPERATOR_SUBJECT naming the wrong subject for the real operator).
// It is kept distinct from ?error=denied because the user cannot fix it by
// retrying or signing in with a different account; only whoever runs the
// instance can, on the server.

// Error and notice codes carried in the sign-in page's query string. They are a
// CLOSED SET, deliberately: the page renders a fixed sentence per code and never
// echoes provider- or request-supplied text (which would be both an injection
// vector and a way to leak subjects/emails into a URL bar and browser history).
const (
	// ErrorDenied — the identity was verified, and refused: a disabled or
	// soft-deleted user, or a token that carried no email claim to provision
	// from. NOT a same-identity-different-email collision — that is
	// ErrorUnprovisionable below (Forgejo #574).
	ErrorDenied = "denied"
	// ErrorExpired — the login-state cookie was missing or too old; the user
	// took too long, or started the flow somewhere else.
	ErrorExpired = "expired"
	// ErrorFailed — anything else went wrong (state/nonce mismatch, a failed
	// code exchange, an unverifiable ID token, a provider-side error). Kept
	// deliberately coarse: an unauthenticated caller learns nothing from a
	// finer-grained failure taxonomy.
	ErrorFailed = "failed"
	// ErrorUnprovisionable — the identity was verified, but the resolver could
	// not create or match an account for it because of THIS SERVER's OWN
	// records (store.ErrLoginEmailTaken: the token's email already
	// belongs to a different existing user — Forgejo #574). Deliberately
	// distinct from ErrorDenied: the user did nothing wrong and cannot fix
	// this by retrying or using a different account; only whoever runs the
	// instance can.
	ErrorUnprovisionable = "unprovisionable"

	// NoticeSignedOut — the user just signed out.
	NoticeSignedOut = "signed_out"
)

// Query parameters the page understands.
const (
	pageErrorParam    = "error"
	pageNoticeParam   = "notice"
	pageReturnToParam = "return_to"
)

// PageConfig configures the sign-in page.
type PageConfig struct {
	// ProductName is the name shown on the page. Defaults to "Mailroom".
	ProductName string

	// StartPath is the route that begins the OIDC flow (cmd/mailroom's
	// /auth/start → LoginHandler). The "Sign in" action links here, carrying a
	// sanitized return_to. Required unless DevAuth is set.
	StartPath string

	// AppPath is where the app itself lives. Defaults to "/".
	AppPath string

	// DevAuth renders the page in its dev-auth variant: there is no sign-in,
	// because the instance authenticates NOBODY. The page says that in as many
	// words rather than offering a button that cannot do anything.
	DevAuth bool

	// Lang is the BCP-47 language tag for <html lang>. Defaults to "en".
	Lang string
}

// Page renders the sign-in landing page. Build it with NewPage and serve its
// Handler on a route that is NOT behind Middleware.
type Page struct {
	cfg PageConfig
	csp string
}

// NewPage validates cfg and returns the page handler.
func NewPage(cfg PageConfig) (*Page, error) {
	if cfg.ProductName == "" {
		cfg.ProductName = "Mailroom"
	}
	if cfg.AppPath == "" {
		cfg.AppPath = "/"
	}
	if cfg.Lang == "" {
		cfg.Lang = "en"
	}
	if !cfg.DevAuth && cfg.StartPath == "" {
		return nil, fmt.Errorf("auth: NewPage: StartPath is required (the route that starts the OIDC flow)")
	}
	return &Page{cfg: cfg, csp: pageCSP()}, nil
}

// pageData is what the template renders.
type pageData struct {
	Lang        string
	Title       string
	ProductName string
	Tagline     string
	Heading     string
	Subheading  string
	Button      string
	SignInURL   string
	AppURL      string

	DevAuth      bool
	DevAuthTitle string
	DevAuthBody  string
	DevAuthOpen  string

	Alert     string // "" when there is nothing to say
	AlertKind string // "error" | "notice" — drives styling and role
	AlertRole string

	ThemeLabel  string
	ThemeSystem string
	ThemeLight  string
	ThemeDark   string

	CSS template.CSS
	JS  template.JS
}

// Handler serves the sign-in page. It is ALWAYS 200: this is the sign-in page,
// and a refusal is content on it, not a status of it — a 4xx here would make an
// intermediary (or a browser's own error handling) hide the one sentence the
// user needs to read. The refusal itself already failed with the right status at
// the callback, and is logged there.
func (p *Page) Handler(w http.ResponseWriter, r *http.Request) {
	d := pageData{
		Lang:        p.cfg.Lang,
		Title:       t("signin.title"),
		ProductName: p.cfg.ProductName,
		Tagline:     t("signin.tagline"),
		Heading:     t("signin.heading"),
		Subheading:  t("signin.subheading"),
		Button:      t("signin.button"),
		AppURL:      p.cfg.AppPath,

		DevAuth:      p.cfg.DevAuth,
		DevAuthTitle: t("signin.devauth.title"),
		DevAuthBody:  t("signin.devauth.body"),
		DevAuthOpen:  t("signin.devauth.open"),

		ThemeLabel:  t("signin.theme"),
		ThemeSystem: t("signin.theme.system"),
		ThemeLight:  t("signin.theme.light"),
		ThemeDark:   t("signin.theme.dark"),

		CSS: template.CSS(pageCSS),
		JS:  template.JS(pageJS),
	}
	d.SignInURL = p.signInURL(r)
	d.Alert, d.AlertKind = alertFor(r)
	d.AlertRole = "status"
	if d.AlertKind == "error" {
		d.AlertRole = "alert"
	}

	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, d); err != nil {
		http.Error(w, "auth: sign-in page", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// A sign-in page must never be cached: its content depends on the query
	// (error/notice) and it is the page a shared browser lands on after logout.
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", p.csp)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// signInURL is where the "Sign in" action goes: the flow-start route, carrying
// the sanitized return_to so a user who was deep-linked into the app lands back
// where they were. sanitizeReturnTo (login.go) is what stops this from being an
// open redirect — the same function LoginHandler uses.
func (p *Page) signInURL(r *http.Request) string {
	if p.cfg.DevAuth {
		return ""
	}
	rt := sanitizeReturnTo(r.URL.Query().Get(pageReturnToParam))
	if rt == "" {
		return p.cfg.StartPath
	}
	return p.cfg.StartPath + "?" + url.Values{pageReturnToParam: []string{rt}}.Encode()
}

// alertFor maps the closed set of error/notice codes to the one sentence the
// page will show. An unrecognized code is treated as no code at all (never
// echoed), so a crafted link cannot put text of an attacker's choosing on the
// sign-in page.
func alertFor(r *http.Request) (msg, kind string) {
	q := r.URL.Query()
	switch q.Get(pageErrorParam) {
	case ErrorDenied:
		return t("signin.error.denied"), "error"
	case ErrorExpired:
		return t("signin.error.expired"), "error"
	case ErrorFailed:
		return t("signin.error.failed"), "error"
	case ErrorUnprovisionable:
		return t("signin.error.unprovisionable"), "error"
	}
	if q.Get(pageNoticeParam) == NoticeSignedOut {
		return t("signin.notice.signedOut"), "notice"
	}
	return "", ""
}

// --- Strings -----------------------------------------------------------------

// t looks up a user-facing string. The keys and the {param} convention mirror
// web/src/app/i18n.ts exactly, and page_i18n_test.go FAILS if the two catalogs'
// signin.* keys ever drift apart — so "i18n-ready" (spec §12) means one catalog
// a translator can actually find, not two that quietly disagree.
//
// The duplication is deliberate and bounded: a Go binary cannot call a
// TypeScript t(), and the alternative (a shared JSON catalog) would either drag
// the web build into the Go package tree or the Go package into web/, for eleven
// strings. The drift test buys the important half of the guarantee.
func t(key string) string {
	if s, ok := enSignIn[key]; ok {
		return s
	}
	return key
}

var enSignIn = map[string]string{
	"signin.title":   "Sign in — Mailroom",
	"signin.tagline": "Your mail, on hardware you control.",
	"signin.heading": "Sign in",
	"signin.button":  "Sign in",
	"signin.subheading": "Mailroom uses your identity provider. " +
		"You will be sent there to sign in, then brought straight back.",

	// #574: this used to claim the account was wrong ("not the account this
	// instance is configured for") and that Mailroom "never creates one
	// automatically" — false on both counts for what actually reaches
	// ErrorDenied: a disabled/soft-deleted user, or a token with no email
	// claim to provision from. Since #98, ANY identity that completes OIDC is
	// auto-provisioned; a same-email collision with a DIFFERENT existing user
	// is a distinct, more specific refusal — see signin.error.unprovisionable.
	"signin.error.denied": "That account cannot use this Mailroom: it has been disabled here, " +
		"or your identity provider did not send an email address for it. " +
		"Ask whoever runs this instance for help.",
	"signin.error.expired": "That sign-in took too long and has expired. Please start again.",
	"signin.error.failed": "Sign-in could not be completed, so no session was created. " +
		"Please try again.",
	// #574: reached when the server's OWN records — not anything about
	// the user's credentials — stops it from setting up the account (the
	// token's email already belongs to a different existing user, most often
	// because MAILROOM_OPERATOR_SUBJECT names the wrong subject). Says so
	// plainly rather than folding it into the generic denial, because
	// retrying or picking a different account cannot fix it.
	"signin.error.unprovisionable": "Your sign-in was verified, but Mailroom could not set up your account: " +
		"its email address already belongs to a different account on this server. " +
		"Signing in again will not fix it; it has to be fixed on the server, " +
		"and whoever runs this Mailroom will find the details in its log.",
	"signin.notice.signedOut": "You are signed out.",

	"signin.devauth.title": "This instance has no authentication",
	"signin.devauth.body": "Dev-auth is enabled: every request that reaches this server is treated as the operator, " +
		"with no sign-in and no session. It must not be reachable from an untrusted network. " +
		"Configure the OIDC block to turn it off.",
	"signin.devauth.open": "Open Mailroom",

	"signin.theme":        "Theme",
	"signin.theme.system": "System",
	"signin.theme.light":  "Light",
	"signin.theme.dark":   "Dark",
}

// --- Content-Security-Policy -------------------------------------------------

// pageCSP pins the page's one style element and one script element by hash, so
// the policy can be `default-src 'none'` with no 'unsafe-inline' anywhere: this
// document may load nothing, connect nowhere, be framed by no one, and submit to
// nothing. The hashes are computed from the very constants the template inlines
// (page_test.go re-extracts them from the rendered HTML and re-hashes, so a
// template change that alters the bytes fails the test rather than silently
// tripping the browser's policy).
func pageCSP() string {
	return strings.Join([]string{
		"default-src 'none'",
		"style-src " + sha256Source(pageCSS),
		"script-src " + sha256Source(pageJS),
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

func sha256Source(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// --- Markup ------------------------------------------------------------------

var pageTemplate = template.Must(template.New("signin").Parse(pageHTML))

// The <html> element deliberately carries NO data-theme attribute: with
// JavaScript disabled the page must follow the system theme, and a pinned
// data-theme="light" would (correctly, by specificity) defeat the
// prefers-color-scheme rules. pageJS sets it on load.
const pageHTML = `<!doctype html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex, nofollow">
<title>{{.Title}}</title>
<style>{{.CSS}}</style>
<script>{{.JS}}</script>
</head>
<body>
<main class="page">
  <div class="card">
    <div class="brand">
      <span class="mark" aria-hidden="true">M</span>
      <span class="wordmark">{{.ProductName}}</span>
    </div>
    <p class="tagline">{{.Tagline}}</p>
{{if .DevAuth}}
    <h1 class="heading">{{.DevAuthTitle}}</h1>
    <p class="alert alert-error" role="alert">{{.DevAuthBody}}</p>
    <a class="button" href="{{.AppURL}}">{{.DevAuthOpen}}</a>
{{else}}
    <h1 class="heading">{{.Heading}}</h1>
{{if .Alert}}    <p class="alert alert-{{.AlertKind}}" role="{{.AlertRole}}">{{.Alert}}</p>
{{end}}    <p class="sub">{{.Subheading}}</p>
    <a class="button" href="{{.SignInURL}}">{{.Button}}</a>
{{end}}
  </div>
  <div class="theme" id="theme" role="group" aria-label="{{.ThemeLabel}}" hidden>
    <button type="button" data-theme="system" aria-pressed="false">{{.ThemeSystem}}</button>
    <button type="button" data-theme="light" aria-pressed="false">{{.ThemeLight}}</button>
    <button type="button" data-theme="dark" aria-pressed="false">{{.ThemeDark}}</button>
  </div>
</main>
</body>
</html>
`

// pageCSS is the whole stylesheet. Solid surfaces only — there is not one rgba()
// fill, opacity, or backdrop-filter on a content surface (spec §12). The single
// alpha value is inside box-shadow, which is an elevation cue rather than a
// surface, exactly as web/src/app/styles.css allows.
//
// Theme resolution has two independent halves, on purpose:
//   - @media (prefers-color-scheme: dark) — works with JS disabled;
//   - html[data-theme="…"] — the manual override, set by pageJS from the SAME
//     localStorage key the app reads (app/prefs.ts), so a choice made at the
//     front door survives into the app. Its specificity (0,1,1) beats :root's
//     (0,1,0), so a manual "light" holds even on a system-dark machine.
//
// It must contain no "</" sequence (it is inlined into a <style> element).
const pageCSS = `
:root {
  --font: system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  --bg: #f1f3f4;
  --surface: #ffffff;
  --fg: #16191d;
  --fg-muted: #5a6570;
  --border: #d5d9de;
  --border-strong: #a9b1b8;
  --accent: #1a56c4;
  --accent-hover: #14459d;
  --accent-fg: #ffffff;
  --error-bg: #fadcdc;
  --error-fg: #6b1015;
  --error-border: #c98d8d;
  --notice-bg: #e4ecf9;
  --notice-fg: #1a3260;
  --notice-border: #9db4d8;
  --shadow: 0 1px 3px rgb(0 0 0 / 20%);
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #101317;
    --surface: #1e2228;
    --fg: #e8eaed;
    --fg-muted: #9aa4af;
    --border: #333941;
    --border-strong: #5b646e;
    --accent: #7cadf5;
    --accent-hover: #a3c6f8;
    --accent-fg: #10151c;
    --error-bg: #521418;
    --error-fg: #fadcdc;
    --error-border: #8c3036;
    --notice-bg: #24405f;
    --notice-fg: #dce8f7;
    --notice-border: #4769a0;
    --shadow: 0 1px 3px rgb(0 0 0 / 50%);
  }
}
html[data-theme="light"] {
  --bg: #f1f3f4;
  --surface: #ffffff;
  --fg: #16191d;
  --fg-muted: #5a6570;
  --border: #d5d9de;
  --border-strong: #a9b1b8;
  --accent: #1a56c4;
  --accent-hover: #14459d;
  --accent-fg: #ffffff;
  --error-bg: #fadcdc;
  --error-fg: #6b1015;
  --error-border: #c98d8d;
  --notice-bg: #e4ecf9;
  --notice-fg: #1a3260;
  --notice-border: #9db4d8;
  --shadow: 0 1px 3px rgb(0 0 0 / 20%);
}
html[data-theme="dark"] {
  --bg: #101317;
  --surface: #1e2228;
  --fg: #e8eaed;
  --fg-muted: #9aa4af;
  --border: #333941;
  --border-strong: #5b646e;
  --accent: #7cadf5;
  --accent-hover: #a3c6f8;
  --accent-fg: #10151c;
  --error-bg: #521418;
  --error-fg: #fadcdc;
  --error-border: #8c3036;
  --notice-bg: #24405f;
  --notice-fg: #dce8f7;
  --notice-border: #4769a0;
  --shadow: 0 1px 3px rgb(0 0 0 / 50%);
}
* { box-sizing: border-box; }
html, body { height: 100%; }
body {
  margin: 0;
  font-family: var(--font);
  font-size: 15px;
  line-height: 1.5;
  color: var(--fg);
  background: var(--bg);
}
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.page {
  min-height: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 20px;
  padding: 24px 16px 40px;
}
.card {
  width: 100%;
  max-width: 420px;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 10px;
  box-shadow: var(--shadow);
  padding: 40px 36px 36px;
  text-align: center;
}
.brand {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
}
.mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 34px;
  height: 34px;
  border-radius: 8px;
  background: var(--accent);
  color: var(--accent-fg);
  font-weight: 700;
  font-size: 19px;
  line-height: 1;
}
.wordmark { font-size: 22px; font-weight: 600; letter-spacing: 0.2px; }
.tagline { margin: 10px 0 0; color: var(--fg-muted); font-size: 14px; }
.heading { margin: 28px 0 0; font-size: 22px; font-weight: 500; }
.sub { margin: 12px 0 0; color: var(--fg-muted); font-size: 14px; text-align: left; }
.alert {
  margin: 20px 0 0;
  padding: 12px 14px;
  border-radius: 8px;
  border: 1px solid var(--border-strong);
  font-size: 14px;
  text-align: left;
}
.alert-error { background: var(--error-bg); color: var(--error-fg); border-color: var(--error-border); }
.alert-notice { background: var(--notice-bg); color: var(--notice-fg); border-color: var(--notice-border); }
.button {
  display: block;
  margin: 28px 0 0;
  padding: 11px 20px;
  border-radius: 8px;
  border: 1px solid var(--accent);
  background: var(--accent);
  color: var(--accent-fg);
  font: inherit;
  font-weight: 600;
  text-decoration: none;
  min-height: 44px;
  line-height: 22px;
}
.button:hover { background: var(--accent-hover); border-color: var(--accent-hover); }
.theme {
  display: flex;
  gap: 6px;
  align-items: center;
}
.theme button {
  padding: 6px 12px;
  min-height: 32px;
  border-radius: 6px;
  border: 1px solid var(--border);
  background: var(--surface);
  color: var(--fg-muted);
  font: inherit;
  font-size: 13px;
  cursor: pointer;
}
.theme button:hover { border-color: var(--border-strong); color: var(--fg); }
.theme button[aria-pressed="true"] {
  border-color: var(--accent);
  color: var(--fg);
  font-weight: 600;
}
@media (max-width: 480px) {
  .card { padding: 28px 20px 24px; border-radius: 8px; }
}
@media (prefers-contrast: more) {
  .card, .alert, .theme button { border-color: var(--border-strong); }
  .sub, .tagline, .theme button { color: var(--fg); }
}
@media (prefers-reduced-motion: reduce) {
  * { transition: none; animation: none; }
}
`

// pageJS is the page's only script: it applies the manual theme override (the
// same "mailroom.theme" localStorage key app/prefs.ts owns, with the same
// chosen-vs-resolved split) and reveals the theme control. Everything the page
// DOES — signing in — is a plain link, so with JavaScript off the page is still
// completely usable; it just follows the system theme and hides a control that
// could not work.
//
// It must contain neither "</" nor "<" (it is inlined into a <script> element),
// hence "i !== bs.length" rather than a "<" comparison.
const pageJS = `
(function () {
  var KEY = "mailroom.theme";
  var root = document.documentElement;
  function stored() {
    try {
      var v = localStorage.getItem(KEY);
      return v === "light" || v === "dark" || v === "system" ? v : "system";
    } catch (e) {
      return "system";
    }
  }
  function sync(theme) {
    var group = document.getElementById("theme");
    if (!group) return;
    group.hidden = false;
    var buttons = group.querySelectorAll("button[data-theme]");
    for (var i = 0; i !== buttons.length; i++) {
      buttons[i].setAttribute("aria-pressed", buttons[i].getAttribute("data-theme") === theme ? "true" : "false");
    }
  }
  function apply(theme) {
    var dark = theme === "dark" ||
      (theme === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);
    var resolved = dark ? "dark" : "light";
    root.setAttribute("data-theme", resolved);
    root.style.colorScheme = resolved;
    sync(theme);
  }
  document.addEventListener("DOMContentLoaded", function () { apply(stored()); });
  document.addEventListener("click", function (ev) {
    var el = ev.target;
    var button = el && el.closest ? el.closest("button[data-theme]") : null;
    if (!button) return;
    var theme = button.getAttribute("data-theme");
    try { localStorage.setItem(KEY, theme); } catch (e) {}
    apply(theme);
  });
  apply(stored());
})();
`
