// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// testClosedSet is ADR-042's closed scope set in the order the session
// advertises it. internal/api's TestConsentScopeWordsMatchTheClosedSet is the
// check that THIS list is the real one; here it only drives rendering.
var testClosedSet = []string{"mail:read", "mail:send", "mail:draft", "mail:move", "mail:delete",
	"labels:read", "labels:write", "search:read", "contacts:read", "contacts:write"}

func testServer() *Server {
	return &Server{policy: Policy{Scopes: testClosedSet, Destructive: []string{"mail:send", "mail:delete"},
		DefaultRate: 60, MaxRate: 600, DefaultBudget: 500, MaxBudget: 20000, DefaultSuspend: true}}
}

// consentSamples are the pages the golden files and the web accessibility
// test (web/src/app/ui/settings/apikeys/consentPage.test.tsx) render: the full
// screen, a read-only screen, and the error page. Synthetic data only.
func consentSamples() map[string]func(w *httptest.ResponseRecorder) {
	s := testServer()
	acct := []accountRow{{ID: "01J00000000000000000000001", Name: "Personal"}, {ID: "01J00000000000000000000002", Name: "Work", Checked: true}}
	return map[string]func(*httptest.ResponseRecorder){
		"consent-full": func(w *httptest.ResponseRecorder) {
			groups, also, hint := s.groupsFor([]string{"mail:read", "mail:send", "mail:delete"}, store.APIKey{Scopes: []string{"mail:read", "mail:send"},
				ConfirmScopes: []string{}}, true)
			renderConsent(w, 422, consentView{RequestID: "req-0123", ClientName: "Example Assistant", Verified: true,
				ClientHost: "app.example.com", RedirectHost: "127.0.0.1", LoopbackWarning: true, Reauth: true,
				Groups: groups, AlsoGroups: also, ShowAck: true, AskHint: hint, Unavailable: []string{"admin:all"}, Accounts: acct, Restricted: true,
				Rate: 60, Budget: 500, Suspend: true, MaxRate: 600, MaxBudget: 20000,
				Error: "To allow both sending and deleting, type: send and delete"})
		},
		"consent-readonly": func(w *httptest.ResponseRecorder) {
			groups, also, hint := s.groupsFor([]string{"mail:read", "search:read"}, store.APIKey{}, false)
			renderConsent(w, 200, consentView{RequestID: "req-4567", ClientName: "Unverified Tool",
				RedirectHost: "tool.example.com", Groups: groups, AlsoGroups: also, ShowAck: true, AskHint: hint, Accounts: acct[:1],
				Rate: 60, Budget: 500, Suspend: true, MaxRate: 600, MaxBudget: 20000})
		},
		"error": func(w *httptest.ResponseRecorder) {
			errorPage(w, 400, "This approval has expired or was already used. Start again from the app.")
		},
	}
}

func renderSample(name string) string {
	w := httptest.NewRecorder()
	consentSamples()[name](w)
	return w.Body.String()
}

// The golden files are the rendered pages. The web unit lane loads the same
// files into a DOM and runs axe over them, so what is checked there is exactly
// what the server sends. UPDATE_GOLDEN=1 rewrites them.
func TestConsentPagesMatchGolden(t *testing.T) {
	for name := range consentSamples() {
		path := filepath.Join("testdata", name+".html")
		got := renderSample(name)
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (UPDATE_GOLDEN=1 go test ./internal/oauthserver writes it)", name, err)
		}
		if string(want) != got {
			t.Errorf("%s differs from testdata/%s.html; if the page changed on purpose, run UPDATE_GOLDEN=1 go test ./internal/oauthserver", name, name)
		}
	}
}

func TestConsentPageContent(t *testing.T) {
	full := renderSample("consent-full")

	// Every scope in plain words with the raw name secondary, in its group.
	for _, sc := range testClosedSet {
		words := ScopeWords[sc]
		if words == "" || !strings.Contains(full, "> "+words+" <code>"+sc+"</code>") {
			t.Errorf("scope %s is not shown as %q with the raw name beside it", sc, words)
		}
	}
	// Groups, in order, and only destructive scopes have ask-first choices.
	i1, i2, i3 := strings.Index(full, "What it may read"), strings.Index(full, "What it may change"), strings.Index(full, "Sending and deleting")
	// In the ticked part only read and sending/deleting appear (the sample ticks
	// no "change" scope); the change group comes later, under Also allow.
	if i1 < 0 || i3 <= i1 || i2 <= i3 || i2 < strings.Index(full, "Also allow") {
		t.Errorf("groups out of order or missing: %d %d %d", i1, i2, i3)
	}
	for _, sc := range testClosedSet {
		has := strings.Contains(full, `name="confirm" value="`+sc+`"`)
		destructive := sc == "mail:send" || sc == "mail:delete"
		if has != destructive {
			t.Errorf("ask-first choice for %s = %v, want %v", sc, has, destructive)
		}
	}
	ticked := func(page, sc string) bool { return strings.Contains(page, `name="scope" value="`+sc+`" checked`) }
	// The full sample ticks read, send and delete; the other seven are offered unticked.
	also := strings.Index(full, "Also allow")
	if also < 0 || !strings.Contains(full, "The app did not ask for these. Tick any you want it to have.") {
		t.Fatal("no Also allow part")
	}
	for _, sc := range testClosedSet {
		want := sc == "mail:read" || sc == "mail:send" || sc == "mail:delete"
		if got := ticked(full, sc); got != want {
			t.Errorf("full page: %s ticked = %v, want %v", sc, got, want)
		}
		pos := strings.Index(full, `name="scope" value="`+sc+`"`)
		if pos < 0 || (pos > also) == want {
			t.Errorf("full page: %s is in the wrong part (also=%d pos=%d)", sc, also, pos)
		}
	}
	// Re-authorization: the grant's own ask-first choice and the new marker.
	// The sample grant holds send WITHOUT ask-first: that choice is kept. Delete
	// is newly requested, so it starts as ask-first.
	if strings.Contains(full, `name="confirm" value="mail:send" checked`) || !strings.Contains(full, `name="confirm" value="mail:delete" checked`) {
		t.Error("on re-authorization the grant's own ask-first choice must be kept and a newly requested scope must start as ask-first")
	}
	if strings.Count(full, "(new)") != 1 {
		t.Error("exactly the newly requested scope is marked new")
	}
	// The typed confirmation is explained, labelled and tied to its help text.
	for _, want := range []string{"Only needed if you allow both sending and deleting", `<label for="ack">`, `id="ack"`, `aria-describedby="ack-help"`,
		`role="alert"`, "from <strong>app.example.com</strong>", "This app receives the approval on this computer"} {
		if !strings.Contains(full, want) {
			t.Errorf("full page lacks %q", want)
		}
	}
	// The read-only request: two scopes ticked, everything else offered
	// unticked, and an unticked destructive scope still defaults to ask-first.
	ro := renderSample("consent-readonly")
	for _, sc := range testClosedSet {
		want := sc == "mail:read" || sc == "search:read"
		if got := ticked(ro, sc); got != want {
			t.Errorf("read-only page: %s ticked = %v, want %v", sc, got, want)
		}
		if !strings.Contains(ro, `name="scope" value="`+sc+`"`) {
			t.Errorf("read-only page does not offer %s", sc)
		}
	}
	for _, sc := range []string{"mail:send", "mail:delete"} {
		if !strings.Contains(ro, `name="confirm" value="`+sc+`" checked`) {
			t.Errorf("unticked %s does not default to ask-first", sc)
		}
	}
	if !strings.Contains(ro, "Name given by the app, not verified") {
		t.Error("an unverified app is not labelled as such")
	}
	// No script anywhere, and the policy forbids it.
	for name := range consentSamples() {
		page := renderSample(name)
		if strings.Contains(strings.ToLower(page), "<script") || regexp.MustCompile(`(?i)\son[a-z]+=`).MatchString(page) {
			t.Errorf("%s contains script", name)
		}
	}
	if strings.Contains(pageCSP, "script-src") || strings.Contains(pageCSP, "unsafe-inline") || !strings.Contains(pageCSP, "default-src 'none'") ||
		!strings.Contains(pageCSP, "frame-ancestors 'none'") {
		t.Errorf("CSP changed shape: %q", pageCSP)
	}
	// Both colour schemes and a visible focus ring are declared.
	for _, want := range []string{"prefers-color-scheme:dark", ":focus-visible", "max-width:30rem", "width=device-width"} {
		if !strings.Contains(pageCSS+full, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// luminance is the WCAG 2 relative luminance of #rrggbb.
func luminance(hex string) float64 {
	c := func(i int) float64 {
		v, _ := strconv.ParseUint(hex[i:i+2], 16, 8)
		f := float64(v) / 255
		if f <= 0.03928 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*c(1) + 0.7152*c(3) + 0.0722*c(5)
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// jsdom has no layout, so axe cannot measure colour contrast in the unit lane;
// this measures the palette itself, in both schemes, against WCAG AA.
func TestConsentPaletteContrast(t *testing.T) {
	light := pageCSS[strings.Index(pageCSS, ":root{")+6:]
	light = light[:strings.Index(light, "}")]
	dark := pageCSS[strings.Index(pageCSS, "@media (prefers-color-scheme:dark){:root{")+len("@media (prefers-color-scheme:dark){:root{"):]
	dark = dark[:strings.Index(dark, "}")]
	vars := func(block string) map[string]string {
		m := map[string]string{}
		for _, mm := range regexp.MustCompile(`--([a-z-]+):(#[0-9a-f]{6})`).FindAllStringSubmatch(block, -1) {
			m[mm[1]] = mm[2]
		}
		return m
	}
	// text pairs need 4.5:1; borders and the focus ring need 3:1 (WCAG 1.4.11).
	pairs := []struct {
		fg, bg string
		min    float64
	}{
		{"fg", "bg", 4.5}, {"muted", "bg", 4.5}, {"btn-fg", "btn", 4.5}, {"fg", "warn-bg", 4.5}, {"fg", "err-bg", 4.5},
		{"muted", "warn-bg", 4.5}, {"line", "bg", 3}, {"focus", "bg", 3}, {"warn-line", "bg", 3}, {"err-line", "bg", 3},
		{"btn", "bg", 3},
	}
	for scheme, block := range map[string]string{"light": light, "dark": dark} {
		m := vars(block)
		if len(m) < 11 {
			t.Fatalf("%s: found only %d palette variables in %q", scheme, len(m), block)
		}
		for _, p := range pairs {
			if r := contrast(m[p.fg], m[p.bg]); r < p.min {
				t.Errorf("%s: %s on %s is %.2f:1, want at least %.1f:1", scheme, p.fg, p.bg, r, p.min)
			}
		}
	}
}
