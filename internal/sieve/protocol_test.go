// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func parse(t *testing.T, wire string) (response, error) {
	t.Helper()
	return newReader(strings.NewReader(wire)).readResponse(scriptResponseBudget)
}

// pigeonholeGreeting is the exact greeting Dovecot Pigeonhole 2.3.21 sent on
// connect with its stock configuration (captured byte-for-byte).
const pigeonholeGreeting = "\"IMPLEMENTATION\" \"Dovecot Pigeonhole\"\r\n" +
	"\"SIEVE\" \"fileinto reject envelope encoded-character vacation subaddress comparator-i;ascii-numeric relational regex imap4flags copy include variables body enotify environment mailbox date index ihave duplicate mime foreverypart extracttext\"\r\n" +
	"\"NOTIFY\" \"mailto\"\r\n" +
	"\"SASL\" \"\"\r\n" +
	"\"STARTTLS\"\r\n" +
	"\"VERSION\" \"1.0\"\r\n" +
	"OK \"Dovecot ready.\"\r\n"

func TestParsesTheRealPigeonholeGreeting(t *testing.T) {
	r, err := parse(t, pigeonholeGreeting)
	if err != nil {
		t.Fatal(err)
	}
	c := parseCapabilities(r.data)
	if !c.StartTLS || len(c.SASL) != 0 || len(c.Sieve) != 24 || c.Other["NOTIFY"] != "mailto" {
		t.Fatalf("caps = %+v", c)
	}
	if r.status != "OK" || r.text != "Dovecot ready." {
		t.Fatalf("status = %q %q", r.status, r.text)
	}
}

// TestLiteralMayContainLineBreaksAndStatusWords: a script body containing
// "\r\nOK ..." must not end the response early.
func TestLiteralMayContainLineBreaksAndStatusWords(t *testing.T) {
	body := "keep;\r\nOK \"not the end\"\r\nNO\r\n"
	r, err := parse(t, fmt.Sprintf("{%d}\r\n%s\r\nOK \"Getscript completed.\"\r\n", len(body), body))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.data) != 1 || r.data[0][0].text != body || r.status != "OK" {
		t.Fatalf("response = %+v", r)
	}
}

func TestNonSynchronisingLiteralFromServer(t *testing.T) {
	r, err := parse(t, "{4+}\r\nab\"c\r\nOK\r\n")
	if err != nil || r.data[0][0].text != "ab\"c" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestResponseCodesAndQuotedEscapes(t *testing.T) {
	r, err := parse(t, `NO (QUOTA/MAXSIZE "x") "too \"big\" \\ here"`+"\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.status != "NO" || r.code != "QUOTA/MAXSIZE" || r.text != `too "big" \ here` {
		t.Fatalf("%+v", r)
	}
	r, err = parse(t, "bye (REFERRAL \"sieve://elsewhere.example\") \"go away\"\r\n")
	if err != nil || r.status != "BYE" || r.code != "REFERRAL" || r.text != "go away" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = parse(t, "OK\r\n")
	if err != nil || r.status != "OK" || r.code != "" || r.text != "" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBoundsAndTruncation(t *testing.T) {
	cases := map[string]string{
		"truncated literal":   "{10}\r\nabc",
		"no final status":     "\"a\"\r\n\"b\"\r\n",
		"unterminated quote":  "\"abc",
		"line break in quote": "\"ab\r\nc\"\r\nOK\r\n",
		"CR without LF":       "OK\rX",
		"bad literal length":  "{1x}\r\nOK\r\n",
	}
	for name, wire := range cases {
		if _, err := parse(t, wire); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: err = %v, want ErrProtocol", name, err)
		}
	}
}

// TestLiteralOverTheBoundIsRefused: a complete, well-formed literal one byte
// over MaxScriptBytes is refused on its declared length, while one exactly at
// the bound is read (the positive control that the refusal is the bound).
func TestLiteralOverTheBoundIsRefused(t *testing.T) {
	over := fmt.Sprintf("{%d}\r\n%s\r\nOK\r\n", MaxScriptBytes+1, strings.Repeat("a", MaxScriptBytes+1))
	if _, err := parse(t, over); !errors.Is(err, ErrProtocol) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("over the bound: err = %v, want ErrProtocol naming the bound", err)
	}
	at := fmt.Sprintf("{%d}\r\n%s\r\nOK\r\n", MaxScriptBytes, strings.Repeat("a", MaxScriptBytes))
	if r, err := parse(t, at); err != nil || len(r.data[0][0].text) != MaxScriptBytes {
		t.Fatalf("at the bound: %v", err)
	}
}

func TestQuote(t *testing.T) {
	if got := quote(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("quote = %s", got)
	}
	if got := quote("a\r\nb"); got != "{4+}\r\na\r\nb" {
		t.Errorf("quote with CRLF = %q", got)
	}
}

func TestPrintableStripsTerminalEscapes(t *testing.T) {
	if got := Printable("ok\x1b[31mred\x07\u009b"); got != "ok[31mred" {
		t.Errorf("Printable = %q", got)
	}
}
