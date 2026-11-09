// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitHeaderFieldsKeepsOrderFoldingAndRaw(t *testing.T) {
	raw := []byte("Received: from a\r\n by b\r\n\twith ESMTP\r\nSubject: Hello\r\n World\r\nReceived: from c\r\n\r\nBody: not a header\r\n")
	got, trunc := SplitHeaderFields(raw, 0)
	if trunc {
		t.Fatal("truncated without a bound")
	}
	want := []Field{
		{"Received", " from a\r\n by b\r\n\twith ESMTP"},
		{"Subject", " Hello\r\n World"},
		{"Received", " from c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
}

func TestSplitHeaderFieldsSkipsNonFieldsAndHandlesBareLF(t *testing.T) {
	raw := []byte("From someone@example.com Mon Jan  1 00:00:00 2024\nA: 1\n\tmore\nnot a field\nB:2\n\nB: body\n")
	got, _ := SplitHeaderFields(raw, 0)
	want := []Field{{"A", " 1\n\tmore"}, {"B", "2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
	if f, _ := SplitHeaderFields([]byte("\r\nSubject: x\r\n"), 0); len(f) != 0 {
		t.Fatalf("a message starting with a blank line has no header, got %#v", f)
	}
}

func TestSplitHeaderFieldsByteCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString("X-Pad: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n")
	}
	b.WriteString("\r\nbody")
	got, trunc := SplitHeaderFields([]byte(b.String()), 1000)
	if !trunc {
		t.Fatal("a block over the cap must report truncation")
	}
	if len(got) == 0 || len(got) >= 1000 {
		t.Fatalf("got %d fields, want a bounded non-empty prefix", len(got))
	}
	total := 0
	for _, f := range got {
		total += len(f.Name) + 1 + len(f.Raw) + 2
	}
	if total > 1000 {
		t.Fatalf("parsed %d bytes of fields, cap is 1000", total)
	}
	// Below the cap nothing is reported.
	if _, trunc := SplitHeaderFields([]byte("A: 1\r\n\r\n"), 1000); trunc {
		t.Fatal("small block reported truncated")
	}
}

func TestFieldText(t *testing.T) {
	cases := map[string]string{
		" Hello\r\n World ":                  "Hello World",
		" =?UTF-8?Q?Caf=C3=A9?= au lait":     "Café au lait",
		" =?ISO-8859-1?Q?Andr=E9?=":          "André",
		" =?UTF-8?B?w6k=?= =?UTF-8?B?w6k=?=": "éé",
		" é":                                "é", // NFC
		"":                                   "",
		" =?bogus-charset?Q?x?= tail":        "=?bogus-charset?Q?x?= tail",
	}
	for in, want := range cases {
		if got := FieldText(in); got != want {
			t.Errorf("FieldText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFieldAddresses(t *testing.T) {
	got, ok := FieldAddresses(` "Joe Bloggs" <joe@example.com>,` + "\r\n" + ` =?UTF-8?Q?Andr=C3=A9?= <andre@example.com>, plain@example.com`)
	if !ok {
		t.Fatal("not parsed")
	}
	want := []FieldAddress{{"Joe Bloggs", "joe@example.com"}, {"André", "andre@example.com"}, {"", "plain@example.com"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if _, ok := FieldAddresses(" not an address <"); ok {
		t.Fatal("garbage parsed")
	}
	if _, ok := FieldAddresses(" "); ok {
		t.Fatal("empty parsed")
	}
}

func TestFieldGroupedAddresses(t *testing.T) {
	got, ok := FieldGroupedAddresses(` solo@example.com, Friends: a@example.com, "B B" <b@example.com>;, Empty:;, last@example.com`)
	if !ok {
		t.Fatal("not parsed")
	}
	want := []FieldAddressGroup{
		{Addresses: []FieldAddress{{"", "solo@example.com"}, {"", "last@example.com"}}},
		{Name: "Friends", Named: true, Addresses: []FieldAddress{{"", "a@example.com"}, {"B B", "b@example.com"}}},
		{Name: "Empty", Named: true, Addresses: []FieldAddress{}},
	}
	// The loose group is first-seen ordered, so it precedes Friends.
	if len(got) != 3 {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		bothEmpty := len(got[i].Addresses) == 0 && len(want[i].Addresses) == 0
		if got[i].Name != want[i].Name || got[i].Named != want[i].Named ||
			(!bothEmpty && !reflect.DeepEqual(got[i].Addresses, want[i].Addresses)) {
			t.Errorf("group %d = %#v, want %#v", i, got[i], want[i])
		}
	}
	flat, _ := FieldAddresses(` Friends: a@example.com, b@example.com;`)
	if len(flat) != 2 {
		t.Errorf("flat addresses of a group = %#v", flat)
	}
	if _, ok := FieldGroupedAddresses(" Open: a@example.com"); ok {
		t.Error("an unclosed group parsed")
	}
}

func TestFieldMessageIDs(t *testing.T) {
	got, ok := FieldMessageIDs(" <a@example.com> (comment <x@y>)\r\n <b@example.com>")
	if !ok || !reflect.DeepEqual(got, []string{"a@example.com", "b@example.com"}) {
		t.Fatalf("got %#v ok=%v", got, ok)
	}
	if _, ok := FieldMessageIDs(" no ids here"); ok {
		t.Fatal("parsed ids from nothing")
	}
}

func TestFieldURLs(t *testing.T) {
	got, ok := FieldURLs(" <mailto:list-request@example.com?subject=unsubscribe>,\r\n <https://example.com/u/1>")
	want := []string{"mailto:list-request@example.com?subject=unsubscribe", "https://example.com/u/1"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v ok=%v", got, ok)
	}
	if _, ok := FieldURLs(" NO"); ok {
		t.Fatal("parsed urls from nothing")
	}
}

func TestFieldDate(t *testing.T) {
	cases := map[string]string{
		" Fri, 21 Nov 1997 09:55:06 -0600":      "1997-11-21T09:55:06-06:00",
		" Mon, 2 Jan 2006 15:04:05 +0000 (UTC)": "2006-01-02T15:04:05Z",
		"\r\n 21 Nov 97 09:55:06 GMT":           "1997-11-21T09:55:06Z",
	}
	for in, want := range cases {
		got, ok := FieldDate(in)
		if !ok || got != want {
			t.Errorf("FieldDate(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := FieldDate(" yesterday-ish"); ok {
		t.Error("garbage date parsed")
	}
}

// A header folded into 1-byte continuation lines must parse in linear time:
// the cost may not depend on the line count beyond the byte cap.
func TestSplitHeaderFieldsManyContinuationLinesIsLinear(t *testing.T) {
	const max = 128 << 10
	var b strings.Builder
	b.WriteString("X-Folded:")
	for b.Len() < max-8 {
		b.WriteString("\r\n x") // 4 bytes per line
	}
	b.WriteString("\r\n\r\n")
	raw := []byte(b.String())

	start := time.Now()
	got, _ := SplitHeaderFields(raw, max)
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("parsing a %d byte block of 1-byte folds took %v (quadratic?)", len(raw), elapsed)
	}
	if len(got) != 1 || got[0].Name != "X-Folded" {
		t.Fatalf("fields = %d", len(got))
	}
	// The per-field line cap keeps MaxFieldLines lines, first included, and says so.
	want := "" + strings.Repeat("\r\n x", MaxFieldLines-1)
	if got[0].Raw != want {
		t.Fatalf("Raw is %d bytes, want %d (first %d lines kept)", len(got[0].Raw), len(want), MaxFieldLines)
	}
	if _, trunc := SplitHeaderFields(raw, max); !trunc {
		t.Fatal("dropping lines past the cap must report truncated")
	}
}

func TestSplitHeaderFieldsCapsFieldCount(t *testing.T) {
	raw := []byte(strings.Repeat("A: 1\r\n", MaxHeaderFields+10) + "\r\nbody")
	got, trunc := SplitHeaderFields(raw, 0)
	if len(got) != MaxHeaderFields || !trunc {
		t.Fatalf("got %d fields, truncated=%v; want %d and true", len(got), trunc, MaxHeaderFields)
	}
}
