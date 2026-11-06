// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// THE SERVER HALF OF THE EDITOR ROUND TRIP (ADR-051 §3, beta 2).
//
// The compose editor's HTML is the input to SanitizeOutboundHTML before a draft
// is saved or a message sent. Every beta-2 format (strike-through, sub- and
// superscript, code, code blocks, quotes, colour, highlight, font family and size,
// paragraph and list indentation) has to come through the sanitiser WITH its tag
// and its style property and value, or the writer's formatting silently vanishes
// on send while the editor still shows it.
//
// The HTML is not written by hand here. web/src/app/ui/editor/roundTrip.test.tsx
// builds each format through the real toolbar and commits the editor's getHTML()
// output to testdata/sanitizer-roundtrip.json; that web test fails if the file is
// stale, so this test always runs against what the editor writes today. (Twin
// pattern of the consent-page goldens, with the web side as the writer.) If a
// format does NOT survive, the sanitiser is NOT widened to make this pass: the
// finding goes to the design (an editor change, or a deliberate allowlist
// decision with its own review).

const editorRoundTripFixture = "../../web/src/app/ui/editor/testdata/sanitizer-roundtrip.json"

type roundTripExpect struct {
	Tag    string            `json:"tag"`
	Style  map[string]string `json:"style"`
	Within string            `json:"within"`
}

type roundTripCase struct {
	Name   string            `json:"name"`
	HTML   string            `json:"html"`
	Expect []roundTripExpect `json:"expect"`
}

func loadRoundTripCases(t *testing.T) []roundTripCase {
	t.Helper()
	raw, err := os.ReadFile(editorRoundTripFixture)
	if err != nil {
		t.Fatalf("reading the editor fixture (run `npx vitest run src/app/ui/editor/roundTrip.test.tsx -u` in web/ to create it): %v", err)
	}
	var cases []roundTripCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parsing the editor fixture: %v", err)
	}
	return cases
}

// styleDecls reads an element's style attribute as property to value. The values
// the editor writes contain no ";" (quotes are not inside url() or strings that
// hold one), so a plain split is enough here.
func styleDecls(n *html.Node) map[string]string {
	out := map[string]string{}
	for _, a := range n.Attr {
		if a.Key != "style" {
			continue
		}
		for _, d := range strings.Split(a.Val, ";") {
			i := strings.Index(d, ":")
			if i < 0 {
				continue
			}
			out[strings.ToLower(strings.TrimSpace(d[:i]))] = strings.TrimSpace(d[i+1:])
		}
	}
	return out
}

// sameCSSValue compares two values as CSS does: case and whitespace do not
// matter and a family name reads the same in single or double quotes. It does not
// treat any other difference as equal, and a value that merely contains the
// expected one is not equal to it.
func sameCSSValue(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "\"", "'")
		return strings.Join(strings.Fields(strings.ReplaceAll(s, ",", " , ")), "")
	}
	return norm(a) == norm(b)
}

func parseFragmentNodes(t *testing.T, s string) []*html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<!doctype html><html><body>" + s + "</body></html>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var body *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil && body == nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if body == nil {
		t.Fatalf("no body in %q", s)
	}
	return []*html.Node{body}
}

// findMatching returns the elements with the tag (inside an ancestor with the
// `within` tag, when given) whose style carries every wanted declaration.
func findMatching(roots []*html.Node, e roundTripExpect, wantStyle map[string]string) []*html.Node {
	var hits []*html.Node
	var walk func(n *html.Node, inside bool)
	walk = func(n *html.Node, inside bool) {
		if n.Type == html.ElementNode {
			if n.Data == e.Tag && (e.Within == "" || inside) {
				got := styleDecls(n)
				ok := true
				for p, v := range wantStyle {
					if g, present := got[p]; !present || !sameCSSValue(g, v) {
						ok = false
					}
				}
				if ok {
					hits = append(hits, n)
				}
			}
			if e.Within != "" && n.Data == e.Within {
				inside = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inside)
		}
	}
	for _, r := range roots {
		walk(r, false)
	}
	return hits
}

func TestSanitizeOutboundKeepsEveryEditorFormat(t *testing.T) {
	cases := loadRoundTripCases(t)
	if len(cases) < 15 {
		t.Fatalf("the fixture holds only %d cases; the editor test builds at least 15 formats", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if len(c.Expect) == 0 {
				t.Fatal("a case with no expectation proves nothing")
			}
			out := SanitizeOutboundHTML(c.HTML)
			assertSafeDOM(t, c.HTML, out)

			in := parseFragmentNodes(t, c.HTML)
			got := parseFragmentNodes(t, out)
			for _, e := range c.Expect {
				// The expectation is true of the INPUT (so a bad fixture cannot make
				// this test pass vacuously) ...
				if len(findMatching(in, e, e.Style)) == 0 {
					t.Fatalf("the fixture is wrong: %+v is not in its own input\n  in: %s", e, c.HTML)
				}
				// ... and of the OUTPUT: the tag and every style property and value.
				if len(findMatching(got, e, e.Style)) == 0 {
					t.Errorf("the sanitiser lost %+v\n  in:  %s\n  out: %s", e, c.HTML, out)
				}
			}
		})
	}
}

// The test above could pass against a sanitiser that keeps everything. These show
// it can fail: a value that is NOT intact, a missing tag, and the sanitiser's own
// removals, all go red through the same helpers.
func TestEditorRoundTripCheckCanFail(t *testing.T) {
	out := parseFragmentNodes(t, `<p><span style="color: rgb(204, 0, 0)">x</span></p>`)
	if len(findMatching(out, roundTripExpect{Tag: "span"}, map[string]string{"color": "rgb(204, 0, 0)"})) != 1 {
		t.Fatal("control: the matching value should be found")
	}
	for name, want := range map[string]map[string]string{
		"a different value":    {"color": "rgb(204, 0, 1)"},
		"a missing property":   {"font-size": "12px"},
		"a longer value":       {"color": "rgb(204, 0, 0) !important"},
		"a value contained in": {"color": "rgb(204"},
	} {
		if len(findMatching(out, roundTripExpect{Tag: "span"}, want)) != 0 {
			t.Errorf("%s was accepted", name)
		}
	}
	if len(findMatching(out, roundTripExpect{Tag: "em"}, nil)) != 0 {
		t.Error("a tag that is not there was found")
	}
	if len(findMatching(out, roundTripExpect{Tag: "span", Within: "blockquote"}, nil)) != 0 {
		t.Error("a span outside a blockquote was found inside one")
	}
	// What the sanitiser does remove really is gone from its output.
	stripped := SanitizeOutboundHTML(`<span style="color:red;background:url(https://tracker.example/p.png)">x</span>`)
	if strings.Contains(stripped, "tracker.example") {
		t.Fatalf("control: the url() should be stripped: %s", stripped)
	}
}
