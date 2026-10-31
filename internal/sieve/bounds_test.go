// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"mailroom/internal/sieve/sievetest"
)

// endless is a reader that never ends, repeating chunk, and counts what was
// taken from it — so a test can show the client stopped near its budget
// rather than merely that it stopped.
type endless struct {
	chunk []byte
	off   int
	taken int
}

func (e *endless) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], e.chunk[e.off:])
		n += c
		e.off = (e.off + c) % len(e.chunk)
	}
	e.taken += n
	return n, nil
}

var _ io.Reader = (*endless)(nil)

// counting counts the bytes read through it.
type counting struct {
	r io.Reader
	n int
}

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestEveryResponseHasAByteBudget: three endless shapes, each of which slipped
// past a count-only cap — spaces (no token at all), lines of near-maximum
// quoted strings (100000 lines x 64 KiB), and short lines — stop at the
// budget, having read no more than the budget plus one bufio buffer.
func TestEveryResponseHasAByteBudget(t *testing.T) {
	big := `"` + strings.Repeat("a", maxQuotedBytes-1) + "\"\r\n"
	shapes := map[string]string{
		"spaces":           " ",
		"max quoted lines": big,
		"short data lines": "\"x\"\r\n",
		"capability lines": "\"X-PAD\" \"" + strings.Repeat("b", 900) + "\"\r\n",
		"literal per line": "{5}\r\nhello\r\n",
	}
	for name, chunk := range shapes {
		for _, budget := range []int{smallResponseBudget, listResponseBudget, scriptResponseBudget} {
			src := &endless{chunk: []byte(chunk)}
			_, err := newReader(src).readResponse(budget)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("%s at %d: err = %v, want ErrProtocol", name, budget, err)
			}
			if src.taken > budget+2*4096+len(chunk) {
				t.Errorf("%s at %d: read %d bytes, far past the budget", name, budget, src.taken)
			}
		}
	}
}

// TestLiteralIsChargedAgainstTheBudget: a literal that fits MaxScriptBytes but
// not the response's budget is refused before it is allocated.
func TestLiteralIsChargedAgainstTheBudget(t *testing.T) {
	wire := fmt.Sprintf("{%d}\r\n%s\r\nOK\r\n", smallResponseBudget, strings.Repeat("a", smallResponseBudget))
	src := &counting{r: strings.NewReader(wire)}
	if _, err := newReader(src).readResponse(smallResponseBudget); !errors.Is(err, ErrProtocol) ||
		!strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	// Refused on the DECLARED length: the body was never read (one bufio fill
	// at most), let alone allocated.
	if src.n > 4096 {
		t.Fatalf("read %d bytes of a literal the budget refused on its declared length", src.n)
	}
	// Positive control: the same literal under the script budget is read.
	if _, err := newReader(strings.NewReader(wire)).readResponse(scriptResponseBudget); err != nil {
		t.Fatalf("under the script budget: %v", err)
	}
}

func quick(t *testing.T, what string, f func() error) error {
	t.Helper()
	start := time.Now()
	err := f()
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("%s took %v: it ran into a timeout, not the budget", what, d)
	}
	return err
}

func TestEndlessServerResponsesAreCutOff(t *testing.T) {
	g := newFake(t, sievetest.Options{Hostile: sievetest.EndlessGreeting})
	err := quick(t, "endless greeting", func() error { _, err := openFake(t, g, nil); return err })
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("endless greeting: err = %v, want ErrProtocol", err)
	}

	for _, mode := range []sievetest.Hostile{sievetest.EndlessListScripts, sievetest.EndlessSpaces} {
		f := newFake(t, sievetest.Options{Hostile: mode})
		s, err := openFake(t, f, nil)
		if err != nil {
			t.Fatalf("mode %d: open: %v", mode, err)
		}
		err = quick(t, fmt.Sprint("mode ", mode), func() error {
			if mode == sievetest.EndlessListScripts {
				_, err := s.ListScripts(context.Background())
				return err
			}
			_, err := s.GetScript(context.Background(), "roundcube")
			return err
		})
		_ = s.Close()
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("mode %d: err = %v, want ErrProtocol", mode, err)
		}
	}
}

// TestTooManyScriptsIsRefused: LISTSCRIPTS within the byte budget but over
// MaxScripts entries is refused; exactly MaxScripts is accepted.
func TestTooManyScriptsIsRefused(t *testing.T) {
	mk := func(n int) []sievetest.Script {
		out := make([]sievetest.Script, n)
		for i := range out {
			out[i] = sievetest.Script{Name: fmt.Sprintf("s%d", i), Body: "keep;"}
		}
		return out
	}
	for _, tc := range []struct {
		n  int
		ok bool
	}{{MaxScripts, true}, {MaxScripts + 1, false}} {
		f := newFake(t, sievetest.Options{Scripts: mk(tc.n)})
		s, err := openFake(t, f, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ListScripts(context.Background())
		_ = s.Close()
		if tc.ok && (err != nil || len(got) != tc.n) {
			t.Errorf("%d scripts: %d, %v", tc.n, len(got), err)
		}
		if !tc.ok && !errors.Is(err, ErrProtocol) {
			t.Errorf("%d scripts: err = %v, want ErrProtocol", tc.n, err)
		}
	}
}

// TestServerErrorIsSanitised: a NO whose code and text carry terminal escape
// sequences renders without a single control character.
func TestServerErrorIsSanitised(t *testing.T) {
	r, err := newReader(strings.NewReader("NO (\x1b[2JEVIL\x9b) \"bad\x1b]0;title\x07\"\r\n")).readResponse(smallResponseBudget)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(r.code, "\x1b\x07") || strings.ContainsRune(r.code, 0x9b) {
		t.Errorf("code kept a control character: %q", r.code)
	}
	e := &ServerError{Verb: "GET\x1bSCRIPT", Status: "NO\x1b", Code: "\x1b[31m", Text: "x\x1b[0m\u009b"}
	msg := e.Error()
	for _, r := range msg {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Fatalf("ServerError.Error() carries control character %U: %q", r, msg)
		}
	}
}

func TestEscapeControls(t *testing.T) {
	in := "if true {\r\n\tkeep; # \x1b[2J\x07\x7f\u009b\xff ok é\n}"
	got := EscapeControls(in)
	want := "if true {\r\n\tkeep; # \\x1b[2J\\x07\\x7f\\u009b\\xff ok é\n}"
	if got != want {
		t.Fatalf("EscapeControls:\n got %q\nwant %q", got, want)
	}
}

// TestEscapeControlsBareCR: CR is kept only as half of CRLF. A bare CR, a CR
// at the very end, and CR CR LF's first CR are escaped, so nothing can return
// the cursor and overwrite a line already on the terminal.
func TestEscapeControlsBareCR(t *testing.T) {
	cases := map[string]string{
		"keep;\r\n":          "keep;\r\n",
		"safe\rEVIL":         `safe\rEVIL`,
		"end\r":              `end\r`,
		"a\r\r\nb":           `a\r` + "\r\nb",
		"\r\n\r\n":           "\r\n\r\n",
		"lf only\nnext\tcol": "lf only\nnext\tcol",
	}
	for in, want := range cases {
		if got := EscapeControls(in); got != want {
			t.Errorf("EscapeControls(%q) = %q, want %q", in, got, want)
		}
	}
}
