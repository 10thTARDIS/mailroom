// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"
)

// The parser's limits. Each is enforced while parsing, so hostile input costs at
// most the work these allow (see the package doc).
//
// The numbers are generous for real invitations and small for everything else:
// the largest real invitation in the sample set (an Outlook recurring meeting
// with a VTIMEZONE and 40 attendees) is about 12 KiB, 9 components and 80
// properties.
const (
	// MaxBytes is the largest calendar this package will read at all.
	MaxBytes = 256 << 10
	// MaxComponents bounds BEGIN blocks across the whole calendar.
	MaxComponents = 256
	// MaxProperties bounds content lines across the whole calendar.
	MaxProperties = 4096
	// MaxDepth bounds component nesting. VCALENDAR > VEVENT > VALARM is 3;
	// VCALENDAR > VTIMEZONE > STANDARD is 3. One level of slack for an X-
	// component somebody nests.
	MaxDepth = 4
	// MaxParams bounds the parameters on one property.
	MaxParams = 32
	// MaxParamValues bounds the comma-separated values of one parameter.
	MaxParamValues = 32
)

// Errors Parse returns. They are distinct so a caller can report WHICH bound a
// calendar hit — "too large" and "not a calendar" are different statements to a
// user, and one generic failure would make them the same.
var (
	ErrTooLarge       = errors.New("ical: calendar exceeds the size limit")
	ErrTooComplex     = errors.New("ical: calendar exceeds the component or property limit")
	ErrTooDeep        = errors.New("ical: calendar components are nested too deeply")
	ErrNotACalendar   = errors.New("ical: no VCALENDAR object")
	ErrMalformed      = errors.New("ical: malformed calendar structure")
	errBadContentLine = errors.New("ical: malformed content line")
)

// Component is one BEGIN/END block.
type Component struct {
	Name       string
	Properties []Property
	Children   []*Component
}

// Property is one content line: NAME;PARAM=V1,V2:VALUE. Name and parameter
// names are upper-cased; the value is exactly as written (still TEXT-escaped,
// see unescapeText).
type Property struct {
	Name   string
	Params map[string][]string
	Value  string
}

// Param returns the first value of a parameter, or "".
func (p Property) Param(name string) string {
	if v := p.Params[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Prop returns the first property with this name, or nil.
func (c *Component) Prop(name string) *Property {
	for i := range c.Properties {
		if c.Properties[i].Name == name {
			return &c.Properties[i]
		}
	}
	return nil
}

// Props returns every property with this name.
func (c *Component) Props(name string) []Property {
	var out []Property
	for _, p := range c.Properties {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// ChildrenNamed returns the direct children with this component name.
func (c *Component) ChildrenNamed(name string) []*Component {
	var out []*Component
	for _, ch := range c.Children {
		if ch.Name == name {
			out = append(out, ch)
		}
	}
	return out
}

// Parse reads the first VCALENDAR object in data.
//
// It is lenient about what real mailers get wrong (bare LF line endings, a BOM,
// a content line it cannot read — skipped, not fatal, a missing final END) and
// strict about the limits. Lines before the first BEGIN:VCALENDAR and after its
// END are ignored.
func Parse(data []byte) (*Component, error) {
	if len(data) > MaxBytes {
		return nil, ErrTooLarge
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	var (
		root       *Component
		stack      []*Component
		components int
		properties int
	)
	for _, line := range unfold(data) {
		if line == "" {
			continue
		}
		prop, err := parseContentLine(line)
		if err != nil {
			// One unreadable line costs that line. A calendar whose structure
			// lines are unreadable fails below as not-a-calendar or malformed.
			continue
		}
		switch prop.Name {
		case "BEGIN":
			name := strings.ToUpper(strings.TrimSpace(prop.Value))
			if name == "" {
				return nil, ErrMalformed
			}
			if len(stack) == 0 {
				if root != nil {
					// The first VCALENDAR is the calendar; anything after it is
					// ignored rather than merged.
					return root, nil
				}
				if name != "VCALENDAR" {
					continue
				}
			}
			components++
			if components > MaxComponents {
				return nil, ErrTooComplex
			}
			if len(stack) >= MaxDepth {
				return nil, ErrTooDeep
			}
			c := &Component{Name: name}
			if len(stack) == 0 {
				root = c
			} else {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, c)
			}
			stack = append(stack, c)
		case "END":
			if len(stack) == 0 {
				continue
			}
			name := strings.ToUpper(strings.TrimSpace(prop.Value))
			if stack[len(stack)-1].Name != name {
				return nil, ErrMalformed
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return root, nil
			}
		default:
			if len(stack) == 0 {
				continue
			}
			properties++
			if properties > MaxProperties {
				return nil, ErrTooComplex
			}
			top := stack[len(stack)-1]
			top.Properties = append(top.Properties, prop)
		}
	}
	if root == nil {
		return nil, ErrNotACalendar
	}
	// Truncated: the final END never came. What was read is still a calendar —
	// a mail client that cut the part short should not cost the reader the card.
	return root, nil
}

// unfold splits data into logical content lines (RFC 5545 §3.1): a line that
// begins with a space or a tab continues the previous one. CRLF and bare LF are
// both accepted; a stray CR anywhere else is dropped, because a CR that survives
// into a value is exactly what a header or content-line injection needs.
func unfold(data []byte) []string {
	raw := strings.Split(string(data), "\n")
	out := make([]string, 0, len(raw))
	var cur strings.Builder
	have := false
	for _, l := range raw {
		l = strings.ReplaceAll(l, "\r", "")
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') {
			if have {
				cur.WriteString(l[1:])
			}
			continue
		}
		if have {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteString(l)
		have = true
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// parseContentLine reads NAME *(";" param) ":" value (RFC 5545 §3.1).
func parseContentLine(line string) (Property, error) {
	if !utf8.ValidString(line) {
		line = strings.ToValidUTF8(line, "�")
	}
	i := 0
	for i < len(line) && isNameChar(line[i]) {
		i++
	}
	if i == 0 || i >= len(line) {
		return Property{}, errBadContentLine
	}
	p := Property{Name: strings.ToUpper(line[:i])}
	nparams := 0
	for i < len(line) && line[i] == ';' {
		i++
		// Counted per occurrence, not per distinct name: a repeated name merges
		// into one map entry and would otherwise never reach the bound.
		nparams++
		if nparams > MaxParams {
			return Property{}, errBadContentLine
		}
		start := i
		for i < len(line) && isNameChar(line[i]) {
			i++
		}
		if i == start || i >= len(line) || line[i] != '=' {
			return Property{}, errBadContentLine
		}
		pname := strings.ToUpper(line[start:i])
		i++ // '='
		var values []string
		for {
			var v string
			if i < len(line) && line[i] == '"' {
				end := strings.IndexByte(line[i+1:], '"')
				if end < 0 {
					return Property{}, errBadContentLine
				}
				v = line[i+1 : i+1+end]
				i += end + 2
			} else {
				start := i
				for i < len(line) && line[i] != ',' && line[i] != ';' && line[i] != ':' && line[i] != '"' {
					i++
				}
				v = line[start:i]
			}
			if len(values) >= MaxParamValues {
				return Property{}, errBadContentLine
			}
			values = append(values, v)
			if i < len(line) && line[i] == ',' {
				i++
				continue
			}
			break
		}
		if p.Params == nil {
			p.Params = map[string][]string{}
		}
		if len(p.Params[pname])+len(values) > MaxParamValues {
			return Property{}, errBadContentLine
		}
		p.Params[pname] = append(p.Params[pname], values...)
	}
	if i >= len(line) || line[i] != ':' {
		return Property{}, errBadContentLine
	}
	p.Value = line[i+1:]
	return p, nil
}

func isNameChar(b byte) bool {
	return b == '-' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}
