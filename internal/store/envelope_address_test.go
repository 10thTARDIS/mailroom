// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"strings"
	"testing"
)

// The strict envelope addr-spec (#852 review).
//
// RED PROBE: put back the first version (net/mail.ParseAddress, no display
// name, address equal to NormalizeAddress of the input, ASCII-whitespace
// check) and the no-break-space/quoted-local spoof is accepted.
func TestCanonicalEnvelopeAddress(t *testing.T) {
	nbsp := " "
	for name, in := range map[string]string{
		"NBSP and a quoted local part": `"alice@example.com` + nbsp + `(+0` + nbsp + `more)` + nbsp + `with` + nbsp + `0` + nbsp +
			`attachments"@evil.example`,
		"quoted local part, ASCII":   `"alice smith"@example.com`,
		"U+2003 em space":            "alice @example.com",
		"non-ASCII local part":       "jöran@example.com",
		"non-ASCII domain":           "alice@exämple.com",
		"double dot":                 "alice..smith@example.com",
		"leading dot":                ".alice@example.com",
		"trailing dot":               "alice.@example.com",
		"over-long local part":       strings.Repeat("a", 65) + "@example.com",
		"over-long address":          strings.Repeat("a", 64) + "@" + strings.Repeat(strings.Repeat("b", 60)+".", 4) + "com",
		"over-long label":            "alice@" + strings.Repeat("b", 64) + ".com",
		"label starts with a hyphen": "alice@-example.com",
		"label ends with a hyphen":   "alice@example-.com",
		"underscore in domain":       "alice@ex_ample.com",
		"single-label domain":        "alice@localhost",
		"IP literal":                 "alice@[192.0.2.1]",
		"display name":               "Alice <alice@example.com>",
		"angle brackets":             "<alice@example.com>",
		"comment":                    "alice@example.com (x)",
		"summary prose":              "alice@example.com (+0 more) with 0 attachments: draft 'notes'",
		"two addresses":              "alice@example.com,bob@example.com",
		"surrounding space":          " alice@example.com",
		"CRLF":                       "alice@example.com\r\nRCPT TO:<x@example.com>",
		"no local part":              "@example.com",
		"no domain":                  "alice@",
		"no at sign":                 "alice.example.com",
		"empty":                      "",
		"empty domain label":         "alice@example..com",
	} {
		if got, ok := CanonicalEnvelopeAddress(in); ok {
			t.Errorf("%s: %q accepted as %q", name, in, got)
		}
	}
	for in, want := range map[string]string{
		"bob@example.net":                        "bob@example.net",
		"bob+tag@example.net":                    "bob+tag@example.net",
		"Bob.Smith@Mail.Example.NET":             "bob.smith@mail.example.net",
		"carol@xn--bcher-kva.example":            "carol@xn--bcher-kva.example",
		"o'brien!#$%&*/=?^_`{|}~-@example.org":   "o'brien!#$%&*/=?^_`{|}~-@example.org",
		strings.Repeat("a", 64) + "@example.com": strings.Repeat("a", 64) + "@example.com",
	} {
		if got, ok := CanonicalEnvelopeAddress(in); !ok || got != want {
			t.Errorf("%q = %q, %v; want %q accepted", in, got, ok, want)
		}
	}
}
