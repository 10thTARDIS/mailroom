// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// THE SERVER HALF OF A QUOTE'S ROUND TRIP (ADR-051 §7, layer 3).
//
// web/src/app/ui/compose/quote.roundtrip.test.tsx quotes each received document
// (quote-received.json, written by sanitize_quote_received_test.go from the
// reader's own output) in the real editor and writes what a SAVED DRAFT would
// carry to quote-roundtrip.json. Here that goes through the same four stages as
// the beta-4 editor round trip (outbound sanitiser, MIME build, MIME parse, reader
// sanitiser), with every format required inside the quote at every stage, and every
// case, hostile ones included, is checked for the payloads that must not be in a
// sent message.
//
// If a format does not survive a stage NEITHER SANITISER IS WIDENED to make this
// pass: the finding goes to the design.

const quoteRoundTripFile = "../../web/src/app/ui/compose/testdata/quote-roundtrip.json"

type quoteSaved struct {
	Name   string            `json:"name"`
	Group  string            `json:"group"`
	HTML   string            `json:"html"`
	Text   string            `json:"text"`
	Expect []roundTripExpect `json:"expect"`
}

func loadQuoteSaved(t *testing.T) []quoteSaved {
	t.Helper()
	raw, err := os.ReadFile(quoteRoundTripFile)
	if err != nil {
		t.Fatalf("%v (run `npx vitest run src/app/ui/compose/quote.roundtrip.test.tsx -u` in web/ to create it)", err)
	}
	var cases []quoteSaved
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

var quotePayload = regexp.MustCompile(`(?i)evil\.example|tracker\.example|javascript:|pwned|/jmap/proxy/|data-mailroom|srcdoc|http-equiv|\bon[a-z]+\s*=|url\(|expression\(|<(script|style|iframe|object|embed|form|input|svg|meta|base)\b`)

func TestQuoteSavedDraftsSurviveEveryStageAndLeakNothing(t *testing.T) {
	cases := loadQuoteSaved(t)
	formats, hostile := 0, 0
	for _, c := range cases {
		switch c.Group {
		case "format":
			formats++
		case "hostile":
			hostile++
		}
	}
	if formats < 30 || hostile < 8 {
		t.Fatalf("the fixture holds %d format and %d hostile cases; the quote test builds at least 30 and 8", formats, hostile)
	}
	for _, c := range cases {
		t.Run(c.Group+"/"+c.Name, func(t *testing.T) {
			if c.Group == "format" {
				if len(c.Expect) == 0 {
					t.Fatal("a format case with no expectation proves nothing")
				}
				in := parseFragmentNodes(t, c.HTML)
				for _, e := range c.Expect {
					if len(findMatching(in, e, e.Style)) == 0 {
						t.Fatalf("the fixture is wrong: %+v is not in its own input (inside the quote)\n  in: %s", e, c.HTML)
					}
				}
				rc := roundTripCase{Name: c.Name, HTML: c.HTML, Text: c.Text, Expect: c.Expect}
				for _, loss := range roundTripLosses(t, rc, roundTripStages(c.Text)) {
					t.Errorf("%s\n  in: %s", loss, c.HTML)
				}
			}
			out := SanitizeOutboundHTML(c.HTML)
			assertSafeDOM(t, c.HTML, out)
			assertSafeDOM(t, c.HTML, sanitizeHTMLWithProxy(out, nil))
			for _, stage := range []string{c.HTML, out, sanitizeHTMLWithProxy(out, nil)} {
				if m := quotePayload.FindString(stage); m != "" {
					t.Errorf("%q reached %q\n  html: %s", m, c.Name, stage)
				}
			}
		})
	}
}

// The payload check and the stage check can fail: a planted payload is found, and
// a quote with a format taken out is reported as a loss.
func TestQuoteRoundTripChecksCanFail(t *testing.T) {
	for _, planted := range []string{
		`<blockquote><img src="https://evil.example/t.png"></blockquote>`,
		`<blockquote><img data-mailroom-remote-src="/jmap/proxy/a/b/c"></blockquote>`,
		`<blockquote><a href="javascript:x()">x</a></blockquote>`,
		`<blockquote><p onclick="x()">x</p></blockquote>`,
	} {
		if quotePayload.FindString(planted) == "" {
			t.Errorf("the payload check missed %s", planted)
		}
	}
	if quotePayload.FindString(`<blockquote><p><strong>fine</strong> <a href="https://example.com/x">link</a></p></blockquote>`) != "" {
		t.Error("the payload check rejects a clean quote")
	}
	c := roundTripCase{
		Name:   "bold in a quote",
		HTML:   `<blockquote><p>plain</p></blockquote>`,
		Text:   "plain",
		Expect: []roundTripExpect{{Tag: "strong", Within: "blockquote"}},
	}
	losses := roundTripLosses(t, c, roundTripStages(c.Text))
	if len(losses) == 0 || !strings.Contains(losses[0], "stage 1 sanitise (outbound) lost") {
		t.Errorf("a missing format was not reported at the first stage: %v", losses)
	}
}
