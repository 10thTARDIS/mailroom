// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	stdmime "mime"
	"mime/multipart"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"mailroom/internal/compose"
	mailmime "mailroom/internal/mime"
)

// THE SERVER HALF OF THE EDITOR ROUND TRIP (ADR-051 §3 and §6).
//
// The compose editor's HTML is the input to SanitizeOutboundHTML before a draft
// is saved or a message sent, and what that produces becomes the text/html part
// of the message, which a recipient (or this server, reading its own Sent copy)
// parses back and runs through the READER's sanitiser before it is shown. Every
// format of betas 1 to 4 (bold, italic, underline, lists, links, inline pictures,
// strike-through, sub- and superscript, code, code blocks, quotes, colour,
// highlight, font family and size, paragraph and list indentation, tables) has to
// come through EVERY stage of that path WITH its tag, its attributes and its
// style properties and values, or the writer's formatting silently vanishes
// somewhere between the editor and the screen while the editor still shows it.
//
// The stages, in order, each asserted on its own so that a failure names the one
// that lost the format:
//
//	1. sanitise (outbound)   SanitizeOutboundHTML, the send path's sanitiser
//	2. MIME build            compose.Build, read back with the standard library
//	                         (net/mail + mime/multipart), independent of our parser
//	3. MIME parse            internal/mime, how a received message is parsed
//	4. sanitise (reader)     the read path's sanitiser, the HTML that is rendered
//
// The HTML is not written by hand here. web/src/app/ui/editor/roundTrip.test.tsx
// builds each format through the real toolbar and commits the editor's getHTML()
// output to testdata/sanitizer-roundtrip.json; that web test fails if the file is
// stale, so this test always runs against what the editor writes today. (Twin
// pattern of the consent-page goldens, with the web side as the writer.) If a
// format does NOT survive a stage, NEITHER SANITISER IS WIDENED to make this pass:
// the finding goes to the design (an editor change, or a deliberate allowlist
// decision with its own review).

const editorRoundTripFixture = "../../web/src/app/ui/editor/testdata/sanitizer-roundtrip.json"

type roundTripExpect struct {
	Tag    string            `json:"tag"`
	Style  map[string]string `json:"style"`
	Attrs  map[string]string `json:"attrs"`
	Within string            `json:"within"`
}

type roundTripCase struct {
	Name   string            `json:"name"`
	HTML   string            `json:"html"`
	Text   string            `json:"text"`
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

// hasAttr reports whether the element carries the attribute with exactly that value.
func hasAttr(n *html.Node, key, want string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) && a.Val == want {
			return true
		}
	}
	return false
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
				for k, v := range e.Attrs {
					if !hasAttr(n, k, v) {
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

// stage is one step of the path from the editor's HTML to the HTML on screen. It
// takes what the step before produced.
type stage struct {
	name string
	run  func(in string) (string, error)
}

// roundTripStages is the real path. The builder stage is read back with the
// standard library, not with our own parser, so a fault in one cannot hide a
// fault in the other; the parse stage then uses the parser a received message goes
// through.
func roundTripStages(textBody string) []stage {
	var raw []byte
	return []stage{
		{"1 sanitise (outbound)", func(in string) (string, error) { return SanitizeOutboundHTML(in), nil }},
		{"2 MIME build", func(in string) (string, error) {
			built, err := compose.Build(compose.Draft{
				From:     compose.Address{Email: "sender@example.com"},
				To:       []compose.Address{{Email: "rcpt@example.com"}},
				Subject:  "round trip",
				TextBody: textBody,
				HTMLBody: in,
			})
			if err != nil {
				return "", err
			}
			raw = built.Raw
			got, err := htmlPartOfRaw(raw)
			if err != nil {
				return "", err
			}
			// The builder must not alter the HTML it was given (line endings aside).
			if lf := strings.ReplaceAll(got, "\r\n", "\n"); lf != in {
				return got, fmt.Errorf("the builder changed the HTML:\n  in:  %q\n  out: %q", in, lf)
			}
			return got, nil
		}},
		{"3 MIME parse (received)", func(string) (string, error) {
			msg, err := mailmime.ParseBytes(raw)
			if err != nil {
				return "", err
			}
			return msg.HTML, nil
		}},
		// (The body path calls sanitizeHTMLWithProxy with the proxy off, which
		// TestProxyOffIsByteIdenticalToSanitizeHTML pins to SanitizeHTML.)
		{"4 sanitise (reader)", func(in string) (string, error) { return sanitizeHTMLWithProxy(in, nil), nil }},
	}
}

// htmlPartOfRaw finds the text/html part of a built message with the standard
// library alone (multipart.Reader undoes quoted-printable itself).
func htmlPartOfRaw(raw []byte) (string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	mt, params, err := stdmime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	if mt == "text/html" {
		b, err := io.ReadAll(msg.Body)
		return string(b), err
	}
	if !strings.HasPrefix(mt, "multipart/") {
		return "", fmt.Errorf("no HTML part: the message is %s", mt)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return "", fmt.Errorf("no text/html part in the message")
		}
		if err != nil {
			return "", err
		}
		pt, _, _ := stdmime.ParseMediaType(p.Header.Get("Content-Type"))
		if pt == "text/html" {
			b, err := io.ReadAll(p)
			return string(b), err
		}
	}
}

// roundTripLosses takes one case through the stages and returns, for each
// expectation, the FIRST stage whose output no longer carries it (an expectation
// lost early is lost later too; naming the first is what finds the culprit). An
// error inside a stage is reported against that stage.
func roundTripLosses(t *testing.T, c roundTripCase, stages []stage) []string {
	t.Helper()
	var losses []string
	lost := make([]bool, len(c.Expect))
	cur := c.HTML
	for _, st := range stages {
		out, err := st.run(cur)
		if err != nil {
			losses = append(losses, fmt.Sprintf("stage %s failed: %v", st.name, err))
			return losses
		}
		cur = out
		nodes := parseFragmentNodes(t, cur)
		for i, e := range c.Expect {
			if lost[i] {
				continue
			}
			if len(findMatching(nodes, e, e.Style)) == 0 {
				lost[i] = true
				losses = append(losses, fmt.Sprintf("stage %s lost %+v\n  it produced: %s", st.name, e, cur))
			}
		}
	}
	return losses
}

func TestSanitizeOutboundKeepsEveryEditorFormat(t *testing.T) {
	cases := loadRoundTripCases(t)
	if len(cases) < 30 {
		t.Fatalf("the fixture holds only %d cases; the editor test builds at least 30 formats (betas 1 to 4)", len(cases))
	}
	// Tables are the beta-4 formats; if the fixture has none, this has proved nothing about them.
	tables := 0
	for _, c := range cases {
		if strings.Contains(c.HTML, "<table") {
			tables++
		}
	}
	if tables < 5 {
		t.Fatalf("the fixture holds %d table cases; beta 4 builds at least 5", tables)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if len(c.Expect) == 0 {
				t.Fatal("a case with no expectation proves nothing")
			}
			// The expectation is true of the INPUT (so a bad fixture cannot make
			// this test pass vacuously) ...
			in := parseFragmentNodes(t, c.HTML)
			for _, e := range c.Expect {
				if len(findMatching(in, e, e.Style)) == 0 {
					t.Fatalf("the fixture is wrong: %+v is not in its own input\n  in: %s", e, c.HTML)
				}
			}
			// ... and of the output of EVERY stage: the tag, the attributes and every
			// style property and value.
			for _, loss := range roundTripLosses(t, c, roundTripStages(c.Text)) {
				t.Errorf("%s\n  in: %s", loss, c.HTML)
			}
			// Both sanitisers' outputs are still safe documents.
			assertSafeDOM(t, c.HTML, SanitizeOutboundHTML(c.HTML))
			assertSafeDOM(t, c.HTML, sanitizeHTMLWithProxy(SanitizeOutboundHTML(c.HTML), nil))
		})
	}
}

// Without these the test above could pass against a pipeline that keeps
// everything, or one that cannot tell which stage lost something. Each control
// breaks ONE stage, in the harness only (no production code is touched), and
// shows that the loss is found and attributed to that stage.
func TestEditorRoundTripNamesTheStageThatLostIt(t *testing.T) {
	cases := loadRoundTripCases(t)
	var tableCase, colourCase roundTripCase
	for _, c := range cases {
		switch c.Name {
		case "table, header row":
			tableCase = c
		case "text colour":
			colourCase = c
		}
	}
	if tableCase.HTML == "" || colourCase.HTML == "" {
		t.Fatal("the fixture lacks the cases these controls use")
	}
	borderRe := regexp.MustCompile(`border:[^;"]*;?\s*`)
	breakStage := func(name string, wrap func(stage) stage) []stage {
		stages := roundTripStages("text")
		for i, st := range stages {
			if strings.Contains(st.name, name) {
				stages[i] = wrap(st)
			}
		}
		return stages
	}
	stripBorder := func(st stage) stage {
		return stage{st.name, func(in string) (string, error) {
			out, err := st.run(in)
			return borderRe.ReplaceAllString(out, ""), err
		}}
	}

	if losses := roundTripLosses(t, tableCase, roundTripStages("text")); len(losses) != 0 {
		t.Fatalf("control: the unbroken pipeline already loses something: %v", losses)
	}
	// Stripping `border` in each stage in turn: the loss is named at THAT stage.
	for _, name := range []string{"1 sanitise", "2 MIME build", "3 MIME parse", "4 sanitise (reader)"} {
		losses := roundTripLosses(t, tableCase, breakStage(name, stripBorder))
		if len(losses) == 0 {
			t.Errorf("breaking %q was not noticed", name)
			continue
		}
		for _, l := range losses {
			if !strings.Contains(l, "stage "+name) {
				t.Errorf("breaking %q was attributed elsewhere: %s", name, l)
			}
		}
	}
	// A case with no border in it is unaffected (the control is not just failing everything).
	if losses := roundTripLosses(t, colourCase, breakStage("4 sanitise (reader)", stripBorder)); len(losses) != 0 {
		t.Errorf("a case with no border was reported as lost: %v", losses)
	}
	// A stage that drops the HTML part altogether.
	dropped := breakStage("3 MIME parse", func(st stage) stage {
		return stage{st.name, func(string) (string, error) { return "", nil }}
	})
	if losses := roundTripLosses(t, colourCase, dropped); len(losses) == 0 || !strings.Contains(losses[0], "stage 3 MIME parse") {
		t.Errorf("an empty parse result was not attributed to the parse stage: %v", losses)
	}
	// And a stage that errors names itself.
	failing := breakStage("2 MIME build", func(st stage) stage {
		return stage{st.name, func(string) (string, error) { return "", fmt.Errorf("boom") }}
	})
	if losses := roundTripLosses(t, colourCase, failing); len(losses) != 1 || !strings.Contains(losses[0], "stage 2 MIME build failed") {
		t.Errorf("a failing stage was not named: %v", losses)
	}
}

// The tests above could pass against a sanitiser that keeps everything. These show
// the matcher can fail: a value that is NOT intact, a missing tag, and the sanitiser's own
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
