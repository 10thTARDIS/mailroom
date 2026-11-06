// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/compose"
	mailmime "mailroom/internal/mime"
)

// THE SERVER HALF OF THE FORMATTED QUOTE'S TESTS (ADR-051 §7, #92).
//
// A reply or forward quotes the original from the HTML body value the API
// returns, which has been through THIS sanitiser. The web tests must therefore
// start from what this sanitiser really produces for a received message, not from
// HTML a test author believes it produces (CLAUDE.md, fixture realism).
//
// This writes web/src/app/ui/compose/testdata/quote-received.json:
//
//	sources:  quote-sources.json, hand-written (the hostile-paste payloads, the
//	          quote-specific ones, and synthetic newsletter-shaped mail)
//	formats:  every editor format of betas 1 to 4, from the beta-4 fixture
//
// each one BUILT into a MIME message (compose.Build), PARSED as a received message
// is (internal/mime), and sanitised for the reader with the image proxy ON, which
// is the default and the form in which parked pictures reach the client.
//
// Twin pattern of the editor round-trip: this test fails when the committed file
// is not what the sanitiser produces today; QUOTE_FIXTURE_UPDATE=1 rewrites it.
// The web suite fails when the file's sources are not the ones in
// quote-sources.json. Neither sanitiser is widened to make a case pass.

const (
	quoteSourcesFile  = "../../web/src/app/ui/compose/testdata/quote-sources.json"
	quoteReceivedFile = "../../web/src/app/ui/compose/testdata/quote-received.json"
)

type quoteSource struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	Live  bool   `json:"live"`
	HTML  string `json:"html"`
}

type quoteReceived struct {
	Name   string `json:"name"`
	Group  string `json:"group"`
	Live   bool   `json:"live"`
	Source string `json:"source"`
	HTML   string `json:"html"`
}

func receivedAsReader(t *testing.T, raw string) string {
	t.Helper()
	built, err := compose.Build(compose.Draft{
		From:     compose.Address{Email: "sender@example.com"},
		To:       []compose.Address{{Email: "rcpt@example.com"}},
		Subject:  "received",
		TextBody: "text",
		HTMLBody: raw,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	msg, err := mailmime.ParseBytes(built.Raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	acct, id := testIDs()
	return sanitizeHTMLWithProxy(msg.HTML, proxyRewriterFor(acct, id))
}

func quoteReceivedCases(t *testing.T) []quoteReceived {
	t.Helper()
	var sources []quoteSource
	raw, err := os.ReadFile(quoteSourcesFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &sources); err != nil {
		t.Fatal(err)
	}
	var out []quoteReceived
	for _, s := range sources {
		out = append(out, quoteReceived{s.Name, s.Group, s.Live, s.HTML, receivedAsReader(t, s.HTML)})
	}
	for _, c := range loadRoundTripCases(t) {
		out = append(out, quoteReceived{c.Name, "format", false, c.HTML, receivedAsReader(t, c.HTML)})
	}
	return out
}

func TestQuoteReceivedFixtureIsWhatTheReaderProducesToday(t *testing.T) {
	cases := quoteReceivedCases(t)
	want, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("QUOTE_FIXTURE_UPDATE") == "1" {
		if err := os.WriteFile(quoteReceivedFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(quoteReceivedFile)
	if err != nil {
		t.Fatalf("%v (QUOTE_FIXTURE_UPDATE=1 go test ./internal/api -run TestQuoteReceivedFixture writes it)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale: the sources or the reader sanitiser changed. "+
			"Run QUOTE_FIXTURE_UPDATE=1 go test ./internal/api -run TestQuoteReceivedFixture and review the diff.", quoteReceivedFile)
	}
}

// What the web suite relies on, stated here where the sanitiser is. If the reader
// stopped parking pictures or kept an active element, the web quote tests would
// still pass against a fixture that no longer shows the case.
func TestQuoteReceivedFixtureShowsTheCasesItClaims(t *testing.T) {
	byName := map[string]quoteReceived{}
	for _, c := range quoteReceivedCases(t) {
		byName[c.Name] = c
	}
	need := func(name string, ok func(h string) bool, why string) {
		t.Helper()
		c, found := byName[name]
		if !found {
			t.Fatalf("no case %q", name)
		}
		if !ok(c.HTML) {
			t.Errorf("%s: %s\n  html: %s", name, why, c.HTML)
		}
	}
	has := func(sub string) func(string) bool { return func(h string) bool { return strings.Contains(h, sub) } }
	proxied := regexp.MustCompile(`data-mailroom-remote-src="/jmap/proxy/`)
	need("remote image", func(h string) bool { return proxied.MatchString(h) && has("data-mailroom-blocked")(h) },
		"a remote picture should arrive parked under a proxy path")
	need("remote image", func(h string) bool { return !has(`src="https://`)(h) }, "the sender's address must not arrive")
	need("parked image with srcset", func(h string) bool { return proxied.MatchString(h) }, "parked")
	need("cid with no matching part", has(`src="cid:ghost@example.com"`), "a cid: picture reaches the client as written")
	for _, name := range []string{"script", "iframe", "iframe srcdoc", "form with input", "style element", "meta refresh", "base href", "object", "embed"} {
		need(name, func(h string) bool {
			return !regexp.MustCompile(`(?i)<(script|iframe|form|input|style|meta|base|object|embed)\b`).MatchString(h)
		}, "an active element survived the reader")
	}
	need("javascript: link", func(h string) bool { return !strings.Contains(strings.ToLower(h), "javascript:") }, "javascript: survived")
	need("newsletter", has(`role="presentation"`), "the layout table keeps its role")
	need("newsletter", func(h string) bool { return strings.Count(h, "<table") >= 4 }, "the nested layout survives")
}
