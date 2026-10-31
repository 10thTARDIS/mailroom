// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"fmt"
	"sort"
	"strings"
)

// Summary is the STRUCTURE of a Sieve script (RFC 5228): which extensions it
// requires, which commands and tests it uses, and whether it already sends a
// vacation reply. It deliberately carries no string or number VALUES from the
// script — those are where personal addresses, folder names and message text
// live — so a Summary is safe to print where the body is not.
type Summary struct {
	Bytes int
	Lines int
	// Require is every extension named in a require command, lower-cased,
	// sorted, de-duplicated.
	Require []string
	// Commands counts identifiers in command position (if, elsif, else,
	// fileinto, redirect, vacation, stop, ...).
	Commands map[string]int
	// Tests counts identifiers in test position (header, address, anyof, ...).
	Tests map[string]int
	// Vacation is true when the script contains a vacation command.
	Vacation bool
	// VacationTags is the set of tagged arguments the vacation commands use
	// (":days", ":seconds", ":subject", ...): their NAMES only, never values.
	VacationTags []string
	// Includes counts include commands. Their targets are NOT recorded: a
	// script name is the server's to report via LISTSCRIPTS, not the parser's.
	Includes int
	// Complete is false when the lexer stopped early; StoppedAt is then the
	// byte offset, and every count above is a LOWER BOUND over the first
	// StoppedAt bytes.
	Complete  bool
	StoppedAt int
	Problem   string
}

// SortedCounts renders a count map as "name×n" in descending count, then name.
func SortedCounts(m map[string]int) []string {
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range m {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v != kvs[j].v {
			return kvs[i].v > kvs[j].v
		}
		return kvs[i].k < kvs[j].k
	})
	out := make([]string, len(kvs))
	for i, e := range kvs {
		out[i] = fmt.Sprintf("%s×%d", e.k, e.v)
	}
	return out
}

type sieveTokKind int

const (
	sIdent sieveTokKind = iota
	sTag
	sString
	sNumber
	sPunct
)

type sieveTok struct {
	kind sieveTokKind
	text string // identifier/tag name (lower-cased) or punctuation; string VALUES only for require
}

// Summarize lexes a Sieve script and reports its structure. It never fails:
// a script it cannot fully lex yields Complete=false with the counts so far.
func Summarize(script string) Summary {
	s := Summary{
		Bytes:    len(script),
		Lines:    countLines(script),
		Commands: map[string]int{},
		Tests:    map[string]int{},
	}
	toks, stopped, problem := lexSieve(script)
	s.Complete = problem == ""
	if !s.Complete {
		s.StoppedAt, s.Problem = stopped, problem
	}

	req := map[string]bool{}
	vtags := map[string]bool{}
	atStatementStart := true
	current := "" // the command whose arguments we are inside, until ';' or '{'
	for _, t := range toks {
		switch t.kind {
		case sIdent:
			if atStatementStart {
				s.Commands[t.text]++
				current = t.text
				switch t.text {
				case "vacation":
					s.Vacation = true
				case "include":
					s.Includes++
				}
			} else {
				s.Tests[t.text]++
			}
			atStatementStart = false
		case sTag:
			if current == "vacation" {
				vtags[":"+t.text] = true
			}
			atStatementStart = false
		case sString:
			if current == "require" {
				req[strings.ToLower(t.text)] = true
			}
			atStatementStart = false
		case sNumber:
			atStatementStart = false
		case sPunct:
			switch t.text {
			case ";", "{", "}":
				atStatementStart = true
				current = ""
			default:
				atStatementStart = false
			}
		}
	}
	for k := range req {
		s.Require = append(s.Require, Printable(k))
	}
	sort.Strings(s.Require)
	for k := range vtags {
		s.VacationTags = append(s.VacationTags, k)
	}
	sort.Strings(s.VacationTags)
	return s
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// lexSieve tokenises per RFC 5228 §8.1. String values are kept only so that
// Summarize can read require's extension names; nothing else reads them.
// It returns the tokens so far, plus the offset and a description on failure.
func lexSieve(src string) ([]sieveTok, int, string) {
	var toks []sieveTok
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return toks, i, "unterminated /* comment"
			}
			i += 2 + end + 2
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < n && src[j] != '"' {
				if src[j] == '\\' && j+1 < n {
					j++
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= n {
				return toks, i, "unterminated quoted string"
			}
			toks = append(toks, sieveTok{kind: sString, text: sb.String()})
			i = j + 1
		case c == ':':
			j := i + 1
			for j < n && isIdentChar(src[j], j == i+1) {
				j++
			}
			if j == i+1 {
				return toks, i, "':' not followed by a tag name"
			}
			toks = append(toks, sieveTok{kind: sTag, text: strings.ToLower(src[i+1 : j])})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < n && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < n && strings.ContainsRune("KMGkmg", rune(src[j])) {
				j++
			}
			toks = append(toks, sieveTok{kind: sNumber})
			i = j
		case isIdentChar(c, true):
			j := i
			for j < n && isIdentChar(src[j], j == i) {
				j++
			}
			word := strings.ToLower(src[i:j])
			if word == "text" && j < n && src[j] == ':' {
				// multi-line string: "text:" *(SP / HTAB) (hash-comment / CRLF),
				// then lines until one that is exactly ".".
				end, ok := skipMultiline(src, j+1)
				if !ok {
					return toks, i, "unterminated text: block"
				}
				toks = append(toks, sieveTok{kind: sString})
				i = end
				continue
			}
			toks = append(toks, sieveTok{kind: sIdent, text: word})
			i = j
		case strings.IndexByte("[](){},;", c) >= 0:
			toks = append(toks, sieveTok{kind: sPunct, text: string(c)})
			i++
		default:
			return toks, i, fmt.Sprintf("unexpected byte 0x%02x", c)
		}
	}
	return toks, n, ""
}

// skipMultiline returns the offset just past the terminating "." line of a
// text: block that starts at i (just after the colon). Line ends may be CRLF
// or a bare LF: the RFC says CRLF, but real servers store what was uploaded
// (Pigeonhole kept LF-only scripts byte-for-byte when this was written).
func skipMultiline(src string, i int) (int, bool) {
	// Rest of the "text:" line: whitespace and an optional comment.
	nl := strings.IndexByte(src[i:], '\n')
	if nl < 0 {
		return 0, false
	}
	i += nl + 1
	for i < len(src) {
		nl := strings.IndexByte(src[i:], '\n')
		var ln string
		next := len(src)
		if nl < 0 {
			ln = src[i:]
		} else {
			ln = src[i : i+nl]
			next = i + nl + 1
		}
		if strings.TrimSuffix(ln, "\r") == "." {
			return next, true
		}
		if nl < 0 {
			return 0, false
		}
		i = next
	}
	return 0, false
}

func isIdentChar(c byte, first bool) bool {
	if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
		return true
	}
	return !first && c >= '0' && c <= '9'
}
