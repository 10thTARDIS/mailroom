// SPDX-License-Identifier: AGPL-3.0-only

package compose

// RFC 3676 format=flowed encoding (#93).
//
// The oracle is unflowRFC3676 below, written from RFC 3676 §4 for this test and
// deliberately sharing nothing with the encoder: a round trip through a decoder
// that mirrors the encoder's own assumptions would pass whatever the encoder
// did. The TypeScript reader (web/src/core/flowed.ts) is the production
// decoder; web/src/core/flowedWire.test.ts feeds it this encoder's output.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"mailroom/internal/pgpmime"
)

// unflowRFC3676 decodes flowed text (delsp=no) into LF-separated lines.
func unflowRFC3676(wire string) string {
	wire = strings.TrimSuffix(wire, "\r\n")
	var out []string
	var acc strings.Builder
	accDepth := -1 // -1: no paragraph open
	for _, raw := range strings.Split(wire, "\r\n") {
		depth := 0
		for depth < len(raw) && raw[depth] == '>' {
			depth++
		}
		rest := raw[depth:]
		rest = strings.TrimPrefix(rest, " ") // stuffing, or the space after the marks
		if accDepth != -1 && accDepth != depth {
			// A soft break never crosses a depth change: close the paragraph.
			out = append(out, strings.Repeat(">", accDepth)+prefixSpace(accDepth)+acc.String())
			acc.Reset()
			accDepth = -1
		}
		acc.WriteString(rest)
		accDepth = depth
		if strings.HasSuffix(rest, " ") && rest != "-- " {
			continue // soft break: keep the space (delsp=no), join the next line
		}
		out = append(out, strings.Repeat(">", depth)+prefixSpace(depth)+acc.String())
		acc.Reset()
		accDepth = -1
	}
	if accDepth != -1 { // a dangling soft break at end of text
		out = append(out, strings.Repeat(">", accDepth)+prefixSpace(accDepth)+acc.String())
	}
	return strings.Join(out, "\n")
}

func prefixSpace(depth int) string {
	if depth > 0 {
		return " "
	}
	return ""
}

func physicalLines(wire string) []string {
	return strings.Split(strings.TrimSuffix(wire, "\r\n"), "\r\n")
}

func TestFlowRoundTrip(t *testing.T) {
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 12)
	url := "https://example.com/" + strings.Repeat("segment/", 19) + "end"
	if len(url) < 150 {
		t.Fatalf("fixture url is %d chars, want >= 150", len(url))
	}
	cases := map[string]string{
		"long paragraph":       strings.TrimSpace(long),
		"multiple paragraphs":  strings.TrimSpace(long) + "\n\nSecond paragraph " + strings.TrimSpace(long) + "\n\nShort.",
		"deliberate breaks":    "line one\nline two\nline three",
		"quote depth 1":        "> " + strings.TrimSpace(long),
		"quote depth 1-3":      "> " + strings.TrimSpace(long) + "\n>> " + strings.TrimSpace(long) + "\n>>> " + strings.TrimSpace(long) + "\nreply " + strings.TrimSpace(long),
		"quote then reply":     "> short quoted\nreply",
		"adjacent depths":      "> aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa aaa\n>> bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb bbb",
		"leading space":        " indented line\n  two spaces\nplain",
		"from line":            "From the desk of nobody\nFrom: not a header",
		"wrap lands on From":   strings.Repeat("word ", 14) + "From here on it goes",
		"wrap lands on gt":     strings.Repeat("word ", 14) + ">not a quote but wrapped",
		"signature":            "body text\n\n-- \nName\nTitle",
		"signature after long": strings.TrimSpace(long) + "\n-- \nName",
		"dashes in text":       strings.Repeat("word ", 14) + "-- is an aside here",
		"url alone":            url,
		"url in sentence":      "see " + url + " for details and then some more words after it to wrap",
		"non-ascii":            strings.TrimSpace(strings.Repeat("Renée wrote — “naïve” 日本語 café ", 8)),
		"combining and emoji":  strings.TrimSpace(strings.Repeat("éé 👨‍👩‍👧 🎉 ", 20)),
		"empty lines":          "one\n\n\ntwo\n\n",
		"inner double spaces":  strings.TrimSpace(strings.Repeat("a  b   c ", 20)),
		"empty body":           "",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			wire := flowText(in)
			if !strings.HasSuffix(wire, "\r\n") {
				t.Fatalf("no trailing CRLF: %q", wire)
			}
			if strings.Contains(strings.ReplaceAll(wire, "\r\n", ""), "\n") || strings.Contains(strings.ReplaceAll(wire, "\r\n", ""), "\r") {
				t.Fatalf("bare CR or LF in output")
			}
			if !utf8.ValidString(wire) {
				t.Fatalf("output is not valid UTF-8")
			}
			for i, l := range physicalLines(wire) {
				n := utf8.RuneCountInString(l)
				if n > flowMax {
					// Only a single unbreakable token may exceed the maximum.
					if strings.Contains(strings.TrimRight(strings.TrimLeft(l, "> "), " "), " ") {
						t.Errorf("line %d is %d chars and breakable: %q", i, n, l)
					}
				}
			}
			want := strings.TrimSuffix(strings.ReplaceAll(in, "\r\n", "\n"), "\n")
			if got := unflowRFC3676(wire); got != want {
				t.Errorf("round trip differs\n got: %q\nwant: %q\nwire: %q", got, want, wire)
			}
		})
	}
}

func TestFlowWrapsNearWidth(t *testing.T) {
	wire := flowText(strings.TrimSpace(strings.Repeat("alpha beta gamma delta ", 30)))
	lines := physicalLines(wire)
	if len(lines) < 5 {
		t.Fatalf("a 700-char paragraph produced %d lines", len(lines))
	}
	for i, l := range lines[:len(lines)-1] {
		if !strings.HasSuffix(l, " ") {
			t.Errorf("line %d has no soft-break space: %q", i, l)
		}
		if n := utf8.RuneCountInString(l); n > flowWidth {
			t.Errorf("line %d is %d chars, want <= %d", i, n, flowWidth)
		}
	}
	if strings.HasSuffix(lines[len(lines)-1], " ") {
		t.Error("the paragraph's last line must be a hard break")
	}
}

func TestFlowSpaceStuffing(t *testing.T) {
	cases := map[string]string{
		" leading":      "  leading",
		"From a friend": " From a friend",
		"From:header":   "From:header", // not "From " so not stuffed
		"-- ":           "-- ",         // the separator, exactly
		"plain":         "plain",
	}
	for in, want := range cases {
		if got := strings.TrimSuffix(flowText(in), "\r\n"); got != want {
			t.Errorf("flowText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlowQuoteDepthKeptOnEveryLine(t *testing.T) {
	in := ">> " + strings.TrimSpace(strings.Repeat("quoted words go on and on ", 10))
	lines := physicalLines(flowText(in))
	if len(lines) < 2 {
		t.Fatal("fixture did not wrap")
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, ">> ") {
			t.Errorf("line %d lost its quote depth: %q", i, l)
		}
	}
}

func TestFlowUserTrailingSpacesAreNotSoftBreaks(t *testing.T) {
	wire := flowText("first   \nsecond \t\nthird  \n")
	// "second \t" ends in a tab, which is not a soft break and is kept.
	want := "first\r\nsecond \t\r\nthird\r\n"
	if wire != want {
		t.Errorf("got %q, want %q", wire, want)
	}
	if got := unflowRFC3676(wire); got != "first\nsecond \t\nthird" {
		t.Errorf("decoded %q: trailing spaces became soft breaks", got)
	}
}

func TestFlowSignatureNeverSoftBroken(t *testing.T) {
	// A line made only of "--" mid-paragraph: breaking after it would leave
	// the line "-- ", which a reader treats as the separator.
	in := strings.Repeat("x", 71) + " -- " + strings.Repeat("y", 10) + " tail"
	wire := flowText(in)
	for _, l := range physicalLines(wire) {
		if l == "-- " {
			t.Fatalf("a soft-break line is exactly the signature separator: %q", wire)
		}
	}
	if got := unflowRFC3676(wire); got != in {
		t.Errorf("round trip differs: %q", got)
	}
	// And the real separator survives next to a long paragraph.
	wire = flowText(strings.TrimSpace(strings.Repeat("word ", 40)) + "\n-- \nme")
	ls := physicalLines(wire)
	if ls[len(ls)-2] != "-- " {
		t.Errorf("separator mangled: %q", ls)
	}
	if prev := ls[len(ls)-3]; strings.HasSuffix(prev, " ") {
		t.Errorf("the line before the separator is a soft break: %q", prev)
	}
}

// --- MIME level ---

// plainPart returns the text/plain entity of a built message (the message
// itself, or the first part of its multipart/alternative), as header map and
// the still-encoded body.
func plainPart(t *testing.T, raw []byte) (hdr map[string][]string, body string) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content-type: %v", err)
	}
	if mt == "multipart/alternative" {
		// Split by hand: the first part is the plain one.
		b, _ := io.ReadAll(m.Body)
		sep := "--" + params["boundary"] + "\r\n"
		first := strings.SplitN(strings.SplitN(string(b), sep, 3)[1], "--"+params["boundary"], 2)[0]
		hs, bs, _ := strings.Cut(first, "\r\n\r\n")
		hdr = map[string][]string{}
		for _, l := range strings.Split(hs, "\r\n") {
			k, v, _ := strings.Cut(l, ": ")
			hdr[strings.ToLower(k)] = append(hdr[strings.ToLower(k)], v)
		}
		return hdr, bs
	}
	b, _ := io.ReadAll(m.Body)
	hdr = map[string][]string{}
	for k, v := range m.Header {
		hdr[strings.ToLower(k)] = v
	}
	return hdr, string(b)
}

func decodedPlain(t *testing.T, raw []byte) string {
	t.Helper()
	hdr, body := plainPart(t, raw)
	if cte := strings.ToLower(strings.Join(hdr["content-transfer-encoding"], "")); cte == "quoted-printable" {
		b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
		if err != nil {
			t.Fatalf("qp decode: %v", err)
		}
		return string(b)
	}
	return body
}

const flowedFixture = "Dear Renée — here is a long paragraph that must wrap near the usual width, and it carries on a while.\n" +
	"> quoted line from someone else that is also long enough that it has to wrap at least once or twice\n" +
	"\n-- \nMe"

func TestBuildDeclaresFlowedDelSpNo(t *testing.T) {
	for name, d := range map[string]Draft{
		"plain only":  {TextBody: flowedFixture},
		"alternative": {TextBody: flowedFixture, HTMLBody: "<p>hi</p>"},
	} {
		t.Run(name, func(t *testing.T) {
			hdr, _ := plainPart(t, buildOne(t, d).Raw)
			ct := strings.Join(hdr["content-type"], "")
			_, params, err := mime.ParseMediaType(ct)
			if err != nil || !strings.HasPrefix(ct, "text/plain") {
				t.Fatalf("content-type %q (%v)", ct, err)
			}
			if params["format"] != "flowed" || params["delsp"] != "no" || params["charset"] != "utf-8" {
				t.Errorf("params = %v, want format=flowed delsp=no charset=utf-8", params)
			}
		})
	}
	// The HTML alternative is not flowed.
	raw := string(buildOne(t, Draft{TextBody: "a", HTMLBody: "<p>a</p>"}).Raw)
	if strings.Contains(raw, "text/html; charset=utf-8; format=flowed") {
		t.Error("the HTML part was marked flowed")
	}
}

func TestBuildRoundTripsThroughTransport(t *testing.T) {
	ascii := strings.TrimSpace(strings.Repeat("plain ascii words wrap here ", 12)) + "\n> quote " + strings.TrimSpace(strings.Repeat("q ", 50)) + "\n-- \nMe"
	for name, in := range map[string]string{"ascii": ascii, "non-ascii": flowedFixture} {
		for _, rich := range []bool{false, true} {
			d := Draft{TextBody: in}
			if rich {
				d.HTMLBody = "<p>x</p>"
			}
			got := unflowRFC3676(decodedPlain(t, buildOne(t, d).Raw))
			if want := strings.TrimSuffix(in, "\n"); got != want {
				t.Errorf("%s rich=%v: round trip\n got: %q\nwant: %q", name, rich, got, want)
			}
		}
	}
}

// TestFlowedSoftBreaksSurviveQuotedPrintable is the transport half: a soft
// break is a trailing space, and quoted-printable must write it as =20 or a
// gateway that strips trailing white space turns the message into one long line.
func TestFlowedSoftBreaksSurviveQuotedPrintable(t *testing.T) {
	// All-ASCII with soft breaks: must NOT go out 7bit.
	hdr, body := plainPart(t, buildOne(t, Draft{TextBody: strings.TrimSpace(strings.Repeat("ascii only words ", 20))}).Raw)
	if cte := strings.Join(hdr["content-transfer-encoding"], ""); cte != "quoted-printable" {
		t.Fatalf("soft-wrapped ASCII was sent as %q; trailing spaces would be exposed", cte)
	}
	if strings.Contains(body, " \r\n") || strings.Contains(body, "\t\r\n") {
		t.Errorf("raw trailing white space on the wire:\n%q", body)
	}
	if !strings.Contains(body, "=20\r\n") {
		t.Errorf("no =20 soft break on the wire:\n%q", body)
	}
	// Non-ASCII goes through the same encoding.
	_, body = plainPart(t, buildOne(t, Draft{TextBody: flowedFixture}).Raw)
	if !strings.Contains(body, "=20\r\n") || strings.Contains(body, " \r\n") {
		t.Errorf("non-ASCII body lost =20:\n%q", body)
	}
	// A short body with no soft break keeps the 7bit it always had.
	hdr, _ = plainPart(t, buildOne(t, Draft{TextBody: "hello\nworld\n"}).Raw)
	if cte := strings.Join(hdr["content-transfer-encoding"], ""); cte != "7bit" {
		t.Errorf("short ASCII body is %q, want 7bit", cte)
	}
	// Simulate a gateway stripping trailing white space from the wire form:
	// the decoded text must be unchanged.
	_, body = plainPart(t, buildOne(t, Draft{TextBody: flowedFixture}).Raw)
	var stripped []string
	for _, l := range strings.Split(body, "\r\n") {
		stripped = append(stripped, strings.TrimRight(l, " \t"))
	}
	got, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(strings.Join(stripped, "\r\n"))))
	want, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if !bytes.Equal(got, want) {
		t.Error("stripping trailing white space from the wire changed the decoded body")
	}
}

// TestSignedFlowedPartSignsTheBytesSent: pgpmime hashes a CANONICAL form of the
// part (trailing white space removed). The flowed bytes must be unchanged by
// that, or the signature covers text different from what the recipient parses
// (and a soft break would be lost to the verifier's view).
func TestSignedFlowedPartSignsTheBytesSent(t *testing.T) {
	for name, d := range map[string]Draft{
		"ascii":       {TextBody: strings.TrimSpace(strings.Repeat("ascii only words ", 20)) + "\n-- \nMe"},
		"non-ascii":   {TextBody: flowedFixture},
		"alternative": {TextBody: flowedFixture, HTMLBody: "<p>x</p>"},
	} {
		t.Run(name, func(t *testing.T) {
			built := buildOne(t, d)
			var hashed [32]byte
			var canonical []byte
			signed, err := pgpmime.Sign(built.Raw, func(c []byte) (string, string, error) {
				canonical = append([]byte(nil), c...)
				hashed = sha256.Sum256(c)
				return fakeSignature, "pgp-sha256", nil
			})
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			wire := SendPayload(signed)
			// The transmitted first part: between the opening delimiter and
			// the CRLF-delimiter that follows it.
			_, afterHeaders, _ := bytes.Cut(wire, []byte("\r\n\r\n"))
			open := bytes.Index(afterHeaders, []byte("\r\n--")) + 2
			rest := afterHeaders[open:]
			_, partAndMore, _ := bytes.Cut(rest, []byte("\r\n"))
			boundaryLine, _, _ := bytes.Cut(rest, []byte("\r\n"))
			sent, _, ok := bytes.Cut(partAndMore, append([]byte("\r\n"), boundaryLine...))
			if !ok {
				t.Fatalf("could not isolate the transmitted part")
			}
			if sha256.Sum256(sent) != hashed {
				t.Errorf("signature hash is not over the transmitted bytes\n sent:      %q\n canonical: %q", sent, canonical)
			}
			if !bytes.Contains(sent, []byte("format=flowed")) {
				t.Error("the signed entity does not carry format=flowed")
			}
			if !bytes.Contains(sent, []byte("=20\r\n")) {
				t.Error("the signed entity has no =20 soft breaks")
			}
		})
	}
}

// flowedWireFixturePath holds {original, wire} pairs shared with the web suite
// (web/src/core/flowedWire.test.ts), which feeds each wire form to the
// production reader. This test fails when the encoder's output drifts from the
// committed file, so the two suites cannot disagree silently; regenerate with
// UPDATE_FLOWED_FIXTURE=1.
const flowedWireFixturePath = "../../web/src/core/flowedWire.fixture.json"

func TestFlowedWireFixtureMatchesEncoder(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 8))
	originals := []string{
		long,
		long + "\n\nSecond paragraph.\nA hard break above.",
		"> " + long + "\n>> " + long + "\n>>> " + long + "\nmy reply",
		" leading space\nFrom the desk\n" + strings.Repeat("word ", 14) + ">wrapped gt",
		"body\n\n-- \nName",
		"see https://example.com/" + strings.Repeat("segment/", 19) + "end now",
		strings.TrimSpace(strings.Repeat("Renée wrote — “naïve” 日本語 🎉 ", 6)),
	}
	type pair struct {
		Original string `json:"original"`
		Wire     string `json:"wire"`
	}
	var pairs []pair
	for _, o := range originals {
		pairs = append(pairs, pair{Original: o, Wire: flowText(o)})
	}
	want, err := json.MarshalIndent(pairs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("UPDATE_FLOWED_FIXTURE") != "" {
		if err := os.WriteFile(flowedWireFixturePath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(flowedWireFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the encoder's output no longer matches web/src/core/flowedWire.fixture.json; " +
			"regenerate it with UPDATE_FLOWED_FIXTURE=1 and re-run the web tests")
	}
}
