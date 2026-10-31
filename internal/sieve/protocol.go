// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The ManageSieve response grammar (RFC 5804 §4), reduced to what a client
// needs: a response is zero or more DATA lines followed by exactly one
// OK / NO / BYE line. Every line is a sequence of tokens — atoms, strings
// (quoted or {n}/{n+} literal), and parentheses for response codes — ended by
// CRLF. A literal's bytes may themselves contain CRLF, which is why the reader
// tokenises rather than splitting on line breaks.

// Bounds on what one server response may make us hold in memory. A script is
// the only large thing the protocol carries; everything else is a short line.
const (
	// MaxScriptBytes bounds a single literal (in practice: a script body from
	// GETSCRIPT). Pigeonhole's own default sieve_max_script_size is 1M.
	MaxScriptBytes = 1 << 20
	// maxQuotedBytes bounds a quoted string (names, capability values, text).
	maxQuotedBytes = 64 << 10
	// maxTokensPerLine and maxLinesPerResponse bound a hostile or broken server
	// that never sends its final OK.
	maxTokensPerLine    = 4096
	maxLinesPerResponse = 100000

	// Per-response BYTE budgets. The caps above bound counts, not bytes: on
	// their own, 100000 lines of 64 KiB quoted strings is 6 GiB, and a stream
	// of spaces adds no token at all. Every byte read from the server is
	// charged against the budget of the response it belongs to.
	//
	// smallResponseBudget: the greeting, CAPABILITY, STARTTLS, AUTHENTICATE
	// and LOGOUT responses — a capability listing is well under 4 KiB.
	smallResponseBudget = 64 << 10
	// listResponseBudget: LISTSCRIPTS (MaxScripts names of a few hundred bytes).
	listResponseBudget = 256 << 10
	// scriptResponseBudget: GETSCRIPT — one literal of up to MaxScriptBytes,
	// plus its framing and the status line.
	scriptResponseBudget = MaxScriptBytes + 4<<10

	// MaxScripts bounds the LISTSCRIPTS entries the client accepts.
	MaxScripts = 1000
)

type tokenKind int

const (
	tokAtom tokenKind = iota
	tokString
	tokLParen
	tokRParen
)

type token struct {
	kind tokenKind
	text string
}

// line is one response line as tokens.
type line []token

// response is a full server response: the data lines and the final status.
type response struct {
	data   []line
	status string // "OK", "NO" or "BYE"
	code   string // response code, e.g. "NONEXISTENT" (first atom inside the parens), or ""
	text   string // the human-readable string after the status, or ""
}

// ServerError is a NO or BYE from the server. Text is the server's own words,
// already stripped of control characters; it never contains anything we sent.
type ServerError struct {
	Verb   string
	Status string
	Code   string
	Text   string
}

// Error renders every field through Printable: they are filled from server
// bytes, and this string ends up on an operator's terminal.
func (e *ServerError) Error() string {
	s := fmt.Sprintf("sieve: %s: server said %s", Printable(e.Verb), Printable(e.Status))
	if e.Code != "" {
		s += " (" + Printable(e.Code) + ")"
	}
	if e.Text != "" {
		s += ": " + Printable(e.Text)
	}
	return s
}

// ErrProtocol marks a response that does not parse as RFC 5804.
var ErrProtocol = errors.New("sieve: protocol error")

// reader tokenises server responses. Every byte goes through readByte (or the
// literal path), which charges it against the current response's budget.
type reader struct {
	r      *bufio.Reader
	budget int
	used   int
}

func newReader(r io.Reader) *reader { return &reader{r: bufio.NewReader(r)} }

func (rd *reader) overBudget() error {
	return fmt.Errorf("%w: response exceeds its %d-byte budget", ErrProtocol, rd.budget)
}

func (rd *reader) readByte() (byte, error) {
	if rd.used >= rd.budget {
		return 0, rd.overBudget()
	}
	b, err := rd.r.ReadByte()
	if err == nil {
		rd.used++
	}
	return b, err
}

func (rd *reader) unreadByte() {
	if rd.r.UnreadByte() == nil {
		rd.used--
	}
}

// readResponse reads data lines up to and including the final status line,
// reading at most budget bytes from the server.
func (rd *reader) readResponse(budget int) (response, error) {
	rd.budget, rd.used = budget, 0
	var resp response
	for i := 0; i < maxLinesPerResponse; i++ {
		ln, err := rd.readLine()
		if err != nil {
			return response{}, err
		}
		if len(ln) > 0 && ln[0].kind == tokAtom {
			switch st := strings.ToUpper(ln[0].text); st {
			case "OK", "NO", "BYE":
				resp.status = st
				resp.code, resp.text = statusDetail(ln[1:])
				return resp, nil
			}
		}
		resp.data = append(resp.data, ln)
	}
	return response{}, fmt.Errorf("%w: more than %d lines without a final status", ErrProtocol, maxLinesPerResponse)
}

// statusDetail pulls the response code and the text out of the tokens after
// OK/NO/BYE: [SP "(" code ... ")"] [SP string].
func statusDetail(rest line) (code, text string) {
	i := 0
	if i < len(rest) && rest[i].kind == tokLParen {
		depth := 0
		for ; i < len(rest); i++ {
			switch rest[i].kind {
			case tokLParen:
				depth++
			case tokRParen:
				depth--
			case tokAtom:
				if code == "" && depth == 1 {
					code = Printable(strings.ToUpper(rest[i].text))
				}
			}
			if depth == 0 {
				i++
				break
			}
		}
	}
	for ; i < len(rest); i++ {
		if rest[i].kind == tokString {
			return code, Printable(rest[i].text)
		}
	}
	return code, ""
}

// readLine reads tokens until the CRLF that ends a line. A bare LF is accepted
// as a line end too (robustness); a CR not followed by LF is a protocol error.
func (rd *reader) readLine() (line, error) {
	var ln line
	for len(ln) <= maxTokensPerLine {
		b, err := rd.readByte()
		if err != nil {
			return nil, unexpectedEOF(err)
		}
		switch b {
		case ' ', '\t':
			continue
		case '\r':
			nb, err := rd.readByte()
			if err != nil {
				return nil, unexpectedEOF(err)
			}
			if nb != '\n' {
				return nil, fmt.Errorf("%w: CR without LF", ErrProtocol)
			}
			return ln, nil
		case '\n':
			return ln, nil
		case '(':
			ln = append(ln, token{kind: tokLParen})
		case ')':
			ln = append(ln, token{kind: tokRParen})
		case '"':
			s, err := rd.readQuoted()
			if err != nil {
				return nil, err
			}
			ln = append(ln, token{kind: tokString, text: s})
		case '{':
			s, err := rd.readLiteral()
			if err != nil {
				return nil, err
			}
			ln = append(ln, token{kind: tokString, text: s})
		default:
			rd.unreadByte()
			a, err := rd.readAtom()
			if err != nil {
				return nil, err
			}
			ln = append(ln, token{kind: tokAtom, text: a})
		}
	}
	return nil, fmt.Errorf("%w: more than %d tokens on one line", ErrProtocol, maxTokensPerLine)
}

func (rd *reader) readQuoted() (string, error) {
	var sb strings.Builder
	for sb.Len() <= maxQuotedBytes {
		b, err := rd.readByte()
		if err != nil {
			return "", unexpectedEOF(err)
		}
		switch b {
		case '"':
			return sb.String(), nil
		case '\\':
			nb, err := rd.readByte()
			if err != nil {
				return "", unexpectedEOF(err)
			}
			sb.WriteByte(nb)
		case '\r', '\n':
			return "", fmt.Errorf("%w: line break inside a quoted string", ErrProtocol)
		default:
			sb.WriteByte(b)
		}
	}
	return "", fmt.Errorf("%w: quoted string longer than %d bytes", ErrProtocol, maxQuotedBytes)
}

// readLiteral reads "{n}" or "{n+}" CRLF and then exactly n bytes. The opening
// brace has already been consumed.
func (rd *reader) readLiteral() (string, error) {
	var digits strings.Builder
	for {
		b, err := rd.readByte()
		if err != nil {
			return "", unexpectedEOF(err)
		}
		if b == '}' {
			break
		}
		if b == '+' {
			continue
		}
		if b < '0' || b > '9' || digits.Len() > 10 {
			return "", fmt.Errorf("%w: malformed literal length", ErrProtocol)
		}
		digits.WriteByte(b)
	}
	n, err := strconv.Atoi(digits.String())
	if err != nil {
		return "", fmt.Errorf("%w: malformed literal length", ErrProtocol)
	}
	if n > MaxScriptBytes {
		return "", fmt.Errorf("%w: literal of %d bytes exceeds the %d-byte bound", ErrProtocol, n, MaxScriptBytes)
	}
	// CRLF after the length.
	b, err := rd.readByte()
	if err != nil {
		return "", unexpectedEOF(err)
	}
	if b == '\r' {
		if b, err = rd.readByte(); err != nil {
			return "", unexpectedEOF(err)
		}
	}
	if b != '\n' {
		return "", fmt.Errorf("%w: literal length not followed by CRLF", ErrProtocol)
	}
	if n > rd.budget-rd.used {
		return "", rd.overBudget()
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rd.r, buf); err != nil {
		return "", unexpectedEOF(err)
	}
	rd.used += n
	return string(buf), nil
}

func (rd *reader) readAtom() (string, error) {
	var sb strings.Builder
	for sb.Len() <= maxQuotedBytes {
		b, err := rd.readByte()
		if err != nil {
			return "", unexpectedEOF(err)
		}
		switch b {
		case ' ', '\t', '\r', '\n', '(', ')', '"', '{':
			rd.unreadByte()
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
	return "", fmt.Errorf("%w: atom longer than %d bytes", ErrProtocol, maxQuotedBytes)
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: connection closed mid-response", ErrProtocol)
	}
	return err
}

// quote renders s as a ManageSieve string argument: a quoted string when it
// can be one, otherwise a non-synchronising literal (RFC 5804 §4: the client
// form is always "{n+}").
func quote(s string) string {
	if strings.ContainsAny(s, "\r\n\x00") || len(s) > 1024 {
		return "{" + strconv.Itoa(len(s)) + "+}\r\n" + s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// EscapeControls renders a script body for a terminal: tab, LF and CRLF are
// kept so the body still reads as a script, a bare CR is shown as \r, and every other control character
// (C0, DEL, C1) and every byte that is not valid UTF-8 is shown as a visible
// \xNN / \u00NN escape instead of reaching the terminal raw.
func EscapeControls(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r == '\r' && i+1 < len(s) && s[i+1] == '\n':
			// CR is kept only as half of CRLF. A bare CR returns the cursor to
			// the start of the line, so whatever follows would overwrite what
			// the terminal already shows.
			b.WriteRune(r)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Printable strips control characters from server-supplied text before it is
// shown on an operator's terminal, so a hostile server cannot inject escape
// sequences. It is exported for the report command, which prints names.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}
