// SPDX-License-Identifier: AGPL-3.0-only

package api

// Tests for List-Unsubscribe parsing (#58).
//
// Two halves, and they matter for different reasons:
//
//   - WHAT IT REFUSES. The output goes straight into an href in the reading pane, and
//     every byte of the input was written by a stranger. The scheme gate is the control,
//     and each refusal has a negative control beside it — "no URL produced" also passes
//     when the parser is simply broken.
//   - WHAT REAL MAIL LOOKS LIKE. Every case in the second half is a shape actually
//     present in real mailboxes. A
//     strict reading of RFC 2369 drops some of the messages that carry the header, so
//     leniency here is a requirement, not a kindness.

import (
	"strings"
	"testing"
)

func TestParseListUnsubscribeRefusesUnsafeTargets(t *testing.T) {
	refused := []struct {
		name, header string
	}{
		{"javascript", "<javascript:alert(1)>"},
		{"data", "<data:text/html;base64,PHNjcmlwdD4=>"},
		{"file", "<file:///etc/passwd>"},
		{"ftp", "<ftp://example.com/unsub>"},
		// Plain http is refused on purpose: no real message used it, so the
		// measured cost is nothing, and an unsubscribe over cleartext both leaks the
		// subscription and can be rewritten in flight.
		{"plain http", "<http://example.com/unsub>"},
		// A relative or schemeless target has no host for a browser to resolve.
		{"schemeless", "<//example.com/unsub>"},
		{"path only", "</unsubscribe>"},
		// https-shaped but not resolvable.
		{"https with no host", "<https://>"},
		// Userinfo is the classic read-as-one-host-resolve-to-another trick.
		{"https with userinfo", "<https://real.example@evil.example/unsub>"},
		// A mailto with no address is a button that cannot act.
		{"mailto with no address", "<mailto:?subject=unsubscribe>"},
		{"mailto with no @", "<mailto:notanaddress>"},
		{"empty", ""},
		{"whitespace", "   \t "},
		{"brackets with nothing in them", "<>"},
	}

	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			if got := parseListUnsubscribe(c.header); got != nil {
				t.Errorf("parseListUnsubscribe(%q) = %+v, want nil", c.header, got)
			}
		})
	}

	// NEGATIVE CONTROLS for the group above: the same shapes with the one bad part
	// fixed must parse. Without these, a parser that returned nil for everything
	// would pass every assertion above.
	t.Run("negative control: the safe forms do parse", func(t *testing.T) {
		if got := parseListUnsubscribe("<https://example.com/unsub>"); got == nil || got.URL == "" {
			t.Errorf("a plain https target did not parse: %+v", got)
		}
		if got := parseListUnsubscribe("<mailto:unsub@example.com>"); got == nil || got.Email == nil {
			t.Errorf("a plain mailto target did not parse: %+v", got)
		}
	})

	// A message offering ONLY an http link keeps its mailto. Refusing the link must
	// not cost the user the other route.
	t.Run("an http link is dropped but a mailto beside it survives", func(t *testing.T) {
		got := parseListUnsubscribe("<http://example.com/unsub>, <mailto:unsub@example.com>")
		if got == nil || got.Email == nil {
			t.Fatalf("the mailto was lost along with the http link: %+v", got)
		}
		if got.URL != "" {
			t.Errorf("URL = %q, want the http link refused", got.URL)
		}
	})
}

func TestParseListUnsubscribeHandlesRealMail(t *testing.T) {
	// Common in real mail: both a mailto and an https entry. Both are offered — the user picks.
	t.Run("both entries, as many real messages carry", func(t *testing.T) {
		got := parseListUnsubscribe(
			"<mailto:unsub-p03@fbl.esp.example?subject=ListUnsub_000001_a1b2c3d4>, " +
				"<https://clk.example.com/e/u?s=181115&t=18>")
		if got == nil {
			t.Fatal("nil")
		}
		if got.URL != "https://clk.example.com/e/u?s=181115&t=18" {
			t.Errorf("URL = %q", got.URL)
		}
		if got.Email == nil {
			t.Fatal("no mailto")
		}
		if got.Email.To != "unsub-p03@fbl.esp.example" {
			t.Errorf("to = %q", got.Email.To)
		}
		// THE SUBJECT IS LOAD-BEARING: senders key the unsubscribe on it, so a
		// message sent without it arrives and does nothing.
		if got.Email.Subject != "ListUnsub_000001_a1b2c3d4" {
			t.Errorf("subject = %q, want the sender's token", got.Email.Subject)
		}
	})

	// Seen in real mail: a bare URL, no angle brackets at all. RFC 2369 requires them; a strict
	// parser drops these, and the user would see an unsubscribe link in the message
	// body that Mailroom claimed not to have.
	t.Run("a bare URL with no angle brackets, as some real messages carry", func(t *testing.T) {
		got := parseListUnsubscribe("https://www.shop.example/subscriptions/dG9rZW4=--a1b2c3")
		if got == nil || got.URL == "" {
			t.Fatalf("a bracket-less URL was dropped: %+v", got)
		}
	})

	// Seen in real mail: undecoded RFC 2047 encoded-words. Illegal in a structured field, sent
	// anyway, and entirely unusable as stored — `=3C` is the encoded `<`.
	t.Run("an RFC 2047 encoded header, as some real messages carry", func(t *testing.T) {
		got := parseListUnsubscribe("=?us-ascii?Q?=3Chttps=3A=2F=2Fsubs=2Eexample=3Ftoken=3Dabc=3E?=")
		if got == nil {
			t.Fatal("an encoded-word header produced nothing")
		}
		if got.URL != "https://subs.example?token=abc" {
			t.Errorf("URL = %q, want it MIME-decoded", got.URL)
		}
	})

	t.Run("a header that only looks encoded is left alone", func(t *testing.T) {
		// `=?` present but not a valid encoded-word: the decode fails and the raw
		// value must still be parsed rather than discarded.
		got := parseListUnsubscribe("<https://example.com/u?a==?b>")
		if got == nil || got.URL == "" {
			t.Errorf("a failed decode lost the header: %+v", got)
		}
	})

	t.Run("the first https entry wins", func(t *testing.T) {
		got := parseListUnsubscribe("<https://first.example/u>, <https://second.example/u>")
		if got == nil || got.URL != "https://first.example/u" {
			t.Errorf("URL = %+v, want the first entry", got)
		}
	})

	t.Run("a mailto's body is carried too", func(t *testing.T) {
		got := parseListUnsubscribe("<mailto:u@example.com?subject=stop&body=please%20stop>")
		if got == nil || got.Email == nil {
			t.Fatal("nil")
		}
		if got.Email.Subject != "stop" || got.Email.Body != "please stop" {
			t.Errorf("subject/body = %q / %q", got.Email.Subject, got.Email.Body)
		}
	})

	t.Run("a mailto listing several addresses uses only the first", func(t *testing.T) {
		got := parseListUnsubscribe("<mailto:one@example.com,two@example.net>")
		if got == nil || got.Email == nil {
			t.Fatal("nil")
		}
		if got.Email.To != "one@example.com" {
			t.Errorf("to = %q, want only the first address", got.Email.To)
		}
	})

	t.Run("an unterminated bracket keeps what is there", func(t *testing.T) {
		got := parseListUnsubscribe("<https://example.com/unsub")
		if got == nil || got.URL != "https://example.com/unsub" {
			t.Errorf("got %+v, want the truncated-but-usable URL", got)
		}
	})

	// A hostile header must not make Email/get expensive, and must never panic.
	t.Run("a pathological header is bounded and does not panic", func(t *testing.T) {
		huge := "<https://example.com/" + strings.Repeat("a", 1<<20) + ">"
		_ = parseListUnsubscribe(huge)
		_ = parseListUnsubscribe(strings.Repeat("<", 1<<16))
		_ = parseListUnsubscribe(strings.Repeat("=?", 1<<16))
		_ = parseListUnsubscribe(strings.Repeat("mailto:a@b,", 1<<14))
	})
}
