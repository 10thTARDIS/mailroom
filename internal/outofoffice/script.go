// SPDX-License-Identifier: AGPL-3.0-only

package outofoffice

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ScriptName is the one script Mailroom installs, activates and deletes. Every
// other script on the server is somebody else's and is never written,
// activated, deleted or even read by this package.
const ScriptName = "mailroom-vacation"

// MaxContacts bounds the contacts-only sender list. The UI states it, and the
// script states how many were included (a bounded result reports its bound).
const MaxContacts = 2000

// MinReplyDays and MaxReplyDays bound :days. RFC 5230 makes 1 the minimum;
// 30 is Mailroom's ceiling, well inside every server's own.
const (
	MinReplyDays = 1
	MaxReplyDays = 30
)

// ScriptSettings is everything that goes into the script.
type ScriptSettings struct {
	// From and To bound the window as UTC instants; nil is open-ended.
	From, To *time.Time
	Subject  string
	Body     string
	Days     int
	// FromAddress is the reply's From (:from).
	FromAddress string
	// Addresses are the account's own addresses (:addresses): mail that names
	// none of them in To or Cc gets no reply.
	Addresses []string
	// ContactsOnly limits replies to senders in Contacts, most recent first.
	ContactsOnly bool
	Contacts     []string
}

// Built is a generated script and what went into it.
type Built struct {
	Script string
	SHA256 [32]byte
	// ContactsIncluded / ContactsTotal: how many of the candidate contacts the
	// script names (only meaningful with ContactsOnly). Included < Total when
	// MaxContacts or the server's size limit cut the list.
	ContactsIncluded int
	ContactsTotal    int
}

// ErrTooLarge is returned when even the smallest form of the script exceeds
// the server's limit.
var ErrTooLarge = errors.New("the reply is too large for the mail server's script limit")

// ErrNoContacts is returned for contacts-only with no contact to reply to.
var ErrNoContacts = errors.New("replying only to contacts is on, but there is nobody you have written to yet")

// ErrInvalid wraps a setting that cannot be put in a script.
var ErrInvalid = errors.New("invalid out-of-office setting")

// Build generates the script. maxSize is the server's limit in bytes (0: none
// stated). With ContactsOnly the list is cut, most recent kept, until the
// script fits; the cut is reported in Built, never hidden.
func Build(s ScriptSettings, maxSize int) (Built, error) {
	if s.Days < MinReplyDays || s.Days > MaxReplyDays {
		return Built{}, fmt.Errorf("%w: reply interval must be %d to %d days", ErrInvalid, MinReplyDays, MaxReplyDays)
	}
	if s.From != nil && s.To != nil && !s.From.Before(*s.To) {
		return Built{}, fmt.Errorf("%w: the end must be after the start", ErrInvalid)
	}
	if strings.TrimSpace(s.Body) == "" {
		return Built{}, fmt.Errorf("%w: the message is empty", ErrInvalid)
	}
	if err := checkAddress(s.FromAddress); err != nil {
		return Built{}, err
	}
	if len(s.Addresses) == 0 {
		return Built{}, fmt.Errorf("%w: the account has no address to reply from", ErrInvalid)
	}
	for _, a := range s.Addresses {
		if err := checkAddress(a); err != nil {
			return Built{}, err
		}
	}
	var contacts []string
	if s.ContactsOnly {
		for _, c := range s.Contacts {
			if checkAddress(c) == nil {
				contacts = append(contacts, c)
			}
		}
		if len(contacts) == 0 {
			return Built{}, ErrNoContacts
		}
	}
	total := len(contacts)
	if len(contacts) > MaxContacts {
		contacts = contacts[:MaxContacts]
	}
	limit := maxSize
	fits := func(n int) (string, bool) {
		sc := render(s, contacts[:n])
		return sc, limit <= 0 || len(sc) <= limit
	}
	n := len(contacts)
	script, ok := fits(n)
	if !ok {
		if !s.ContactsOnly {
			return Built{}, fmt.Errorf("%w: the script would be %d bytes and the server allows %d; shorten the message",
				ErrTooLarge, len(script), limit)
		}
		// The largest prefix (most recent contacts) that fits.
		lo, hi := 0, n
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if _, ok := fits(mid); ok {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo == 0 {
			sc, _ := fits(1)
			return Built{}, fmt.Errorf("%w: even one contact makes the script %d bytes and the server allows %d; shorten the message",
				ErrTooLarge, len(sc), limit)
		}
		n = lo
		script, _ = fits(n)
	}
	return Built{
		Script:           script,
		SHA256:           sha256.Sum256([]byte(script)),
		ContactsIncluded: n,
		ContactsTotal:    total,
	}, nil
}

// render writes the script. Every user-supplied value is either quoted with
// sieveQuote (single-line values) or carried in a dot-stuffed text: block.
func render(s ScriptSettings, contacts []string) string {
	var b strings.Builder
	nl := "\r\n"
	b.WriteString("# Managed by Mailroom: out-of-office reply." + nl)
	b.WriteString("# Mailroom replaces or deletes this script. Change the reply in Mailroom," + nl)
	b.WriteString("# under Settings, Out of office; edits made here are overwritten." + nl)
	req := []string{`"vacation"`}
	if s.From != nil || s.To != nil {
		req = append(req, `"date"`, `"relational"`)
	}
	b.WriteString("require [" + strings.Join(req, ", ") + "];" + nl + nl)

	var tests []string
	if s.From != nil {
		tests = append(tests, `currentdate :zone "+0000" :value "ge" "iso8601" `+sieveQuote(isoUTC(*s.From)))
	}
	if s.To != nil {
		tests = append(tests, `currentdate :zone "+0000" :value "lt" "iso8601" `+sieveQuote(isoUTC(*s.To)))
	}
	// Mail the provider's spam filter has marked. Which header a provider uses
	// varies; these are the common ones (rspamd's X-Spam, SpamAssassin's
	// X-Spam-Flag and X-Spam-Status). Mail a provider files to Junk WITHOUT a
	// header cannot be told apart here — see docs/adr/ADR-034.
	tests = append(tests,
		`not header :is ["X-Spam", "X-Spam-Flag"] "yes"`,
		`not header :matches "X-Spam-Status" "yes*"`)
	if len(contacts) > 0 {
		tests = append(tests, `address :is "from" `+sieveList(contacts))
	}
	b.WriteString("if allof (" + nl)
	for i, t := range tests {
		b.WriteString("    " + t)
		if i < len(tests)-1 {
			b.WriteString(",")
		}
		b.WriteString(nl)
	}
	b.WriteString(") {" + nl)
	_, _ = fmt.Fprintf(&b, "    vacation :days %d", s.Days)
	if subj := oneLine(s.Subject); subj != "" {
		b.WriteString(" :subject " + sieveQuote(subj))
	}
	b.WriteString(" :from " + sieveQuote(s.FromAddress))
	b.WriteString(" :addresses " + sieveList(s.Addresses))
	b.WriteString(" text:" + nl)
	for _, ln := range bodyLines(s.Body) {
		if strings.HasPrefix(ln, ".") {
			ln = "." + ln // dot-stuffing (RFC 5228 §2.4.2)
		}
		b.WriteString(ln + nl)
	}
	b.WriteString("." + nl + ";" + nl + "}" + nl)
	return b.String()
}

func isoUTC(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// sieveQuote renders a quoted string: backslash and double quote escaped.
// Callers pass single-line values only.
func sieveQuote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

func sieveList(vs []string) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = sieveQuote(v)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// oneLine drops line breaks and other control characters from a header value.
func oneLine(v string) string {
	v = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
	return strings.TrimSpace(v)
}

// bodyLines splits the message into lines, dropping control characters other
// than tab and trailing whitespace at the end of the message.
func bodyLines(v string) []string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	v = strings.ReplaceAll(v, "\r", "\n")
	v = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
	return strings.Split(strings.TrimRight(v, " \t\n"), "\n")
}

// checkAddress accepts a plain address that can sit in a quoted string.
func checkAddress(a string) error {
	at := strings.LastIndexByte(a, '@')
	if a == "" || at <= 0 || at == len(a)-1 || strings.ContainsAny(a, "\"\\<>,; \t\r\n") {
		return fmt.Errorf("%w: %q is not a plain email address", ErrInvalid, a)
	}
	for _, r := range a {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: an address holds a control character", ErrInvalid)
		}
	}
	return nil
}
