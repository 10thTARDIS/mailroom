// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"bytes"
	stdmime "mime"
	stdmail "net/mail"
	"strings"
	"time"
	"unicode/utf8"

	message "github.com/emersion/go-message"
	emmail "github.com/emersion/go-message/mail"
	"golang.org/x/text/unicode/norm"
)

// Header FIELD access for JMAP's header:* and headers properties (RFC 8621
// §4.1.2, ADR-052).
//
// This is deliberately not built on Header/Message: those expose the handful of
// fields Mailroom itself reads, already decoded. JMAP needs every field, in
// order, with its RAW value (folding and all), and then the parsed forms of it
// on demand. So this file owns a small field splitter, and the form parsers sit
// next to it, behind Mailroom-owned types (no go-message types leave the
// package).

const (
	// MaxFieldLines bounds the lines (the first plus continuations) kept for one
	// field. Past it the rest of that field is dropped and the block reports
	// truncated, the same rule as the byte cap.
	MaxFieldLines = 1000
	// MaxHeaderFields bounds the fields returned for one block. Past it, parsing
	// stops and the block reports truncated.
	MaxHeaderFields = 5000
)

// Field is one header field exactly as the message carried it.
type Field struct {
	// Name is the field name as written (case preserved).
	Name string
	// Raw is everything after the colon up to, not including, the line ending
	// that ends the field: the leading space and any folding stay (RFC 8621
	// §4.1.2.1, "Raw").
	Raw string
}

// SplitHeaderFields splits the header block at the start of raw into fields, in
// message order. The block ends at the first blank line. At most maxBytes of it
// are read, at most MaxHeaderFields fields are returned and at most MaxFieldLines
// lines of one field are kept (all three report truncated): a longer block is cut at the end of the last WHOLE field that fits
// and truncated is true, so a caller can say the answer is bounded rather than
// present a prefix as the whole.
//
// It never fails: a line that is not a field (no colon, or a space before it,
// like an mbox "From " separator) is skipped, and a continuation line with no
// field above it is dropped.
func SplitHeaderFields(raw []byte, maxBytes int) (fields []Field, truncated bool) {
	// End of the header block: the first empty line (LF LF or CRLF CRLF, or a
	// mix), or the end of the input.
	end := len(raw)
	for i := 0; i < len(raw); {
		nl := bytes.IndexByte(raw[i:], '\n')
		if nl < 0 {
			break
		}
		line := raw[i : i+nl]
		if len(bytes.TrimRight(line, "\r")) == 0 && i != 0 {
			end = i
			break
		}
		if i == 0 && len(bytes.TrimRight(line, "\r")) == 0 {
			// A message that starts with an empty line has no header.
			end = 0
			break
		}
		i += nl + 1
	}
	block := raw[:end]
	cutByBytes := false
	if maxBytes > 0 && len(block) > maxBytes {
		truncated, cutByBytes = true, true
		block = block[:maxBytes]
		// Cut at a line boundary, and drop a trailing field that the cut may
		// have split: everything after the last line break is partial.
		if nl := bytes.LastIndexByte(block, '\n'); nl >= 0 {
			block = block[:nl+1]
		} else {
			block = nil
		}
	}

	// Offsets, not concatenation: a field's Raw is sliced out of the block ONCE.
	// Appending each continuation line to a string is quadratic in the number of
	// lines, and a header folded into one-byte lines makes that the cost of the
	// whole block even under the byte cap.
	var (
		name         string
		fStart, fEnd = -1, 0 // Raw span of the open field in block; fStart < 0 = none
		lines        int     // lines in the open field
		pos          int
	)
	flush := func() {
		if fStart >= 0 {
			fields = append(fields, Field{Name: name,
				Raw: strings.TrimRight(string(block[fStart:fEnd]), "\r\n")})
			fStart = -1
		}
	}
	for pos < len(block) {
		next := len(block)
		if nl := bytes.IndexByte(block[pos:], '\n'); nl >= 0 {
			next = pos + nl + 1
		}
		line := block[pos:next]
		lineStart := pos
		pos = next
		if line[0] == ' ' || line[0] == '\t' {
			if fStart >= 0 {
				if lines >= MaxFieldLines {
					truncated = true // the rest of this field is dropped
					continue
				}
				lines++
				fEnd = pos
			}
			continue
		}
		flush()
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 || !validFieldName(line[:colon]) {
			continue
		}
		if len(fields) >= MaxHeaderFields {
			truncated = true
			break
		}
		name, fStart, fEnd, lines = string(line[:colon]), lineStart+colon+1, pos, 1
	}
	flush()
	if cutByBytes && len(fields) > 0 {
		// The last field before the cut may itself continue past it; we cannot
		// know, so it is dropped rather than returned partial.
		fields = fields[:len(fields)-1]
	}
	return fields, truncated
}

// validFieldName is RFC 5322's field-name: printable ASCII except the colon.
func validFieldName(b []byte) bool {
	for _, c := range b {
		if c < 0x21 || c > 0x7e || c == ':' {
			return false
		}
	}
	return true
}

// UnfoldField turns a raw field value into its unfolded, trimmed form (RFC 5322
// §2.2.3: remove each line break that is followed by white space).
func UnfoldField(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' && i+1 < len(raw) && (raw[i+1] == ' ' || raw[i+1] == '\t') {
			continue
		}
		if raw[i] == '\n' {
			continue
		}
		b.WriteByte(raw[i])
	}
	return strings.Trim(b.String(), " \t")
}

var wordDecoder = &stdmime.WordDecoder{CharsetReader: message.CharsetReader}

// FieldText is RFC 8621 §4.1.2.2 "Text": unfolded, RFC 2047 encoded words
// decoded, white space at both ends removed, normalised to Unicode NFC. A value
// that is not valid UTF-8 after decoding has its bad bytes replaced rather than
// refused.
func FieldText(raw string) string {
	s := UnfoldField(raw)
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		s = d
	}
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	return strings.Trim(norm.NFC.String(s), " \t")
}

// FieldAddress is one mailbox of an address header.
type FieldAddress struct {
	Name  string // "" when the address has no display name
	Email string
}

// FieldAddressGroup is a group of addresses. Name is "" for the addresses that
// were not written inside a group (RFC 8621 §4.1.2.4: those form one group whose
// name is null).
type FieldAddressGroup struct {
	Name      string
	Named     bool
	Addresses []FieldAddress
}

// FieldAddresses is RFC 8621 §4.1.2.3 "Addresses": the flat list, group members
// included in order. ok is false when the value is not an address list at all.
func FieldAddresses(raw string) (out []FieldAddress, ok bool) {
	groups, ok := FieldGroupedAddresses(raw)
	if !ok {
		return nil, false
	}
	out = []FieldAddress{}
	for _, g := range groups {
		out = append(out, g.Addresses...)
	}
	return out, true
}

// FieldGroupedAddresses is RFC 8621 §4.1.2.4 "GroupedAddresses". The addresses
// outside any group come first-seen-ordered into groups with Named false.
func FieldGroupedAddresses(raw string) (out []FieldAddressGroup, ok bool) {
	s := UnfoldField(raw)
	if s == "" {
		return nil, false
	}
	segs, ok := splitAddressList(s)
	if !ok {
		return nil, false
	}
	out = []FieldAddressGroup{}
	loose := -1 // index of the group collecting ungrouped addresses
	for _, sg := range segs {
		addrs := make([]FieldAddress, 0, len(sg.members))
		for _, m := range sg.members {
			a, err := emmail.ParseAddress(m)
			if err != nil || a == nil {
				return nil, false
			}
			addrs = append(addrs, FieldAddress{Name: strings.TrimSpace(a.Name), Email: a.Address})
		}
		if sg.named {
			name := FieldText(sg.name)
			out = append(out, FieldAddressGroup{Name: unquotePhrase(name), Named: true, Addresses: addrs})
			continue
		}
		if loose < 0 {
			out = append(out, FieldAddressGroup{})
			loose = len(out) - 1
		}
		out[loose].Addresses = append(out[loose].Addresses, addrs...)
	}
	return out, true
}

func unquotePhrase(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
	}
	return s
}

type addrSegment struct {
	named   bool
	name    string
	members []string
}

// splitAddressList cuts an address list at its top-level commas and groups,
// honouring quoted strings, comments and angle brackets. It does not validate
// the pieces; emmail.ParseAddress does that.
func splitAddressList(s string) (segs []addrSegment, ok bool) {
	var (
		cur     strings.Builder
		quoted  bool
		comment int
		angle   bool
		group   *addrSegment
		// sawAddr: the current segment holds '@' or '<'. Kept as state rather
		// than rescanning cur at every ':' (strings.ContainsAny(cur.String(),..)),
		// which made a long segment followed by many colons quadratic: a hostile
		// 128 KiB header cost ~3 s per message (security review of v1.102 beta 3).
		sawAddr bool
	)
	put := func(b byte) {
		if b == '@' || b == '<' {
			sawAddr = true
		}
		cur.WriteByte(b)
	}
	reset := func() {
		cur.Reset()
		sawAddr = false
	}
	emit := func() bool {
		t := strings.TrimSpace(cur.String())
		reset()
		if t == "" {
			return true
		}
		if group != nil {
			group.members = append(group.members, t)
		} else {
			segs = append(segs, addrSegment{members: []string{t}})
		}
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted:
			put(c)
			if c == '\\' && i+1 < len(s) {
				i++
				put(s[i])
			} else if c == '"' {
				quoted = false
			}
			continue
		case comment > 0:
			put(c)
			if c == '\\' && i+1 < len(s) {
				i++
				put(s[i])
			} else if c == '(' {
				comment++
			} else if c == ')' {
				comment--
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
			put(c)
		case '(':
			comment++
			put(c)
		case '<':
			angle = true
			put(c)
		case '>':
			angle = false
			put(c)
		case ':':
			if angle || group != nil || sawAddr {
				put(c)
				continue
			}
			g := addrSegment{named: true, name: strings.TrimSpace(cur.String())}
			reset()
			segs = append(segs, g)
			group = &segs[len(segs)-1]
		case ';':
			if group == nil {
				put(c)
				continue
			}
			emit()
			group = nil
			// Skip to the next top-level comma, if any.
		case ',':
			if angle {
				put(c)
				continue
			}
			emit()
		default:
			put(c)
		}
	}
	if quoted || comment > 0 || angle {
		return nil, false
	}
	if group != nil {
		return nil, false // a group that never closed
	}
	emit()
	return segs, true
}

// FieldMessageIDs is RFC 8621 §4.1.2.5 "MessageIds": the ids in a msg-id list,
// without the angle brackets. ok is false when there is none.
func FieldMessageIDs(raw string) (ids []string, ok bool) {
	return angleItems(raw, func(item string) (string, bool) {
		if item == "" || strings.ContainsAny(item, " \t<>") {
			return "", false
		}
		return item, true
	})
}

// FieldURLs is RFC 8621 §4.1.2.7 "URLs": the <...> items of a List-* field,
// trimmed, with comments removed. ok is false when there is none.
func FieldURLs(raw string) (urls []string, ok bool) {
	return angleItems(raw, func(item string) (string, bool) {
		item = strings.Join(strings.Fields(item), "")
		return item, item != ""
	})
}

// angleItems collects the contents of every top-level <...> in a field value,
// skipping comments. keep may veto or rewrite an item.
func angleItems(raw string, keep func(string) (string, bool)) (items []string, ok bool) {
	s := UnfoldField(raw)
	depth := 0
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case start >= 0:
			if c == '>' {
				if v, good := keep(s[start:i]); good {
					items = append(items, v)
				}
				start = -1
			}
		case c == '\\' && depth > 0 && i+1 < len(s):
			i++
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case c == '<' && depth == 0:
			start = i + 1
		}
	}
	return items, len(items) > 0
}

// FieldDate is RFC 8621 §4.1.2.6 "Date": the value as an RFC 3339 date-time
// keeping its own offset. ok is false when it does not parse as an RFC 5322 date.
func FieldDate(raw string) (string, bool) {
	s := UnfoldField(raw)
	// Drop a trailing comment ("... +0000 (UTC)").
	if i := strings.Index(s, "("); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	t, err := stdmail.ParseDate(s)
	if err != nil {
		return "", false
	}
	return t.Format(time.RFC3339), true
}
