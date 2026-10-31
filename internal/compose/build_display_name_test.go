// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"strings"
	"testing"

	"mailroom/internal/mime"
)

// A RECIPIENT PICKED FROM THE SUGGESTIONS CARRIES ITS CONTACT'S DISPLAY NAME
// (#689), and contact names are harvested from real headers, where they arrive
// already decoded: `"Doe, Jane" <r@example.com>` becomes the name
// `Doe, Jane`. Writing that back out UNQUOTED produced a To field no parser
// can read — `Doe, Jane <r@example.com>` is two list members, the first of
// them not an address — so the submit path's ParseEnvelope fell back to a
// synthetic address with an empty mailbox, dropped it, and refused the send
// with "the message has no recipients". A typed bare address has no name and
// never reached the broken branch, which is why typed sends kept working.
//
// The assertion is the round trip the send path depends on: whatever name a
// chip carries, the bytes Build writes must parse back to that same mailbox
// (the RCPT TO set) and that same name.
func TestDisplayNamesRoundTripThroughTheBytes(t *testing.T) {
	names := []string{
		"Jane Doe",              // plain atoms: the case that always worked
		"Doe, Jane",             // directory "Last, First" — the #689 shape
		"jane@work.example.com", // a name that is itself an address
		`Jane "JD" Doe`,         // embedded quotes
		`Back\slash`,            // a quoted-pair is needed
		"Ops: on call",          // a colon would start a group
		"Jane; Doe",             // a semicolon would end one
		"Jane (work)",           // a comment, not part of the name
		"Jane <work>",           // angle brackets
		"Jane [work]",           // domain-literal brackets
		"J. Doe",                // obs-phrase period
		"O'Brien",               // apostrophe is atext
		"Müller, Jörg",          // non-ASCII AND a special
		"Zoë (Büro)",            // non-ASCII with a comment-looking part
		"Jörg Müller",           // non-ASCII, no specials
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			const to = "recipient@example.com"
			b := mustBuild(t, Draft{
				From:     Address{Name: name, Email: "you@example.com"},
				To:       []Address{{Name: name, Email: to}},
				TextBody: "hi",
			})

			env := ParseEnvelope(b.Raw)
			if len(env.Recipients) != 1 || env.Recipients[0] != to {
				t.Fatalf("recipients = %q, want [%q]\nheaders:\n%s",
					env.Recipients, to, headerBlock(b.Raw))
			}
			if env.From != "you@example.com" {
				t.Fatalf("from = %q, want you@example.com\nheaders:\n%s",
					env.From, headerBlock(b.Raw))
			}

			parsed, err := mime.ParseBytes(b.Raw)
			if err != nil || parsed == nil {
				t.Fatalf("parse: %v", err)
			}
			got := parsed.Header.To()
			if len(got) != 1 || got[0].Name != name || got[0].Address != to {
				t.Fatalf("To = %+v, want one {%q %q}\nheaders:\n%s",
					got, name, to, headerBlock(b.Raw))
			}
		})
	}
}

// A plain name keeps its plain, unquoted rendering: the fix quotes only what
// needs quoting, so every message that was already correct is byte-identical.
func TestPlainDisplayNameIsNotQuoted(t *testing.T) {
	cases := map[string]string{
		"Jane Doe": "Jane Doe <r@example.com>",
		"O'Brien":  "O'Brien <r@example.com>",
		"":         "r@example.com",
	}
	for name, want := range cases {
		if got := (Address{Name: name, Email: "r@example.com"}).String(); got != want {
			t.Errorf("Address{%q}.String() = %q, want %q", name, got, want)
		}
	}
}

func headerBlock(raw []byte) string {
	s := string(raw)
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
