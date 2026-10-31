// SPDX-License-Identifier: AGPL-3.0-only

package xoauth2

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const sentinel = "SENTINEL-TOKEN-7f3a9c"

// Every rendering path the standard library has must print the placeholder,
// including a Secret nested in a struct, a pointer, a slice and a map. The
// positive control at the end proves the check can see the sentinel.
func TestSecretNeverRenders(t *testing.T) {
	s := Secret(sentinel)
	type holder struct {
		Tok  Secret
		Ptr  *Secret
		List []Secret
		Map  map[string]Secret
	}
	h := holder{Tok: s, Ptr: &s, List: []Secret{s}, Map: map[string]Secret{"k": s}}

	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s", "%T"} {
		outs = append(outs, fmt.Sprintf(verb, s), fmt.Sprintf(verb, h))
	}
	outs = append(outs, fmt.Sprint(s, h), fmt.Sprintln(s, h), s.String(), s.GoString())

	js, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(js))
	type xmlHolder struct{ Tok Secret }
	x, err := xml.Marshal(xmlHolder{s})
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(x))

	var buf bytes.Buffer
	for _, hnd := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		l := slog.New(hnd)
		l.Info("refresh", "token", s, "holder", h, slog.Any("any", s), slog.Group("g", "tok", s))
	}
	outs = append(outs, buf.String())

	for _, o := range outs {
		if strings.Contains(o, sentinel) {
			t.Errorf("secret rendered in %q", o)
		}
	}
	// Positive control: the sentinel is detectable when it IS present, so the
	// loop above is not passing because it cannot see anything.
	if !strings.Contains(fmt.Sprint(s.Reveal()), sentinel) {
		t.Fatal("control: Reveal does not return the plaintext")
	}
}

func TestInitialResponse(t *testing.T) {
	got, err := InitialResponse("you@example.com", Secret("ya29.abc"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "user=you@example.com\x01auth=Bearer ya29.abc\x01\x01"; string(got) != want {
		t.Errorf("InitialResponse = %q, want %q", got, want)
	}
	for _, tc := range []struct{ user, tok string }{
		{"", "t"}, {"u", ""}, {"u\x01auth=Bearer x", "t"}, {"u", "t\x01\x01"},
	} {
		if _, err := InitialResponse(tc.user, Secret(tc.tok)); err == nil {
			t.Errorf("InitialResponse(%q, %q) accepted", tc.user, tc.tok)
		}
	}
	// The refusal must not echo the token either.
	_, err = InitialResponse("u", Secret(sentinel+"\x01"))
	if err == nil || strings.Contains(err.Error(), sentinel) {
		t.Errorf("refusal = %v", err)
	}
}

func TestChallengeStatus(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`, "401", true},
		{`{"status":"400"}`, "400", true},
		{`{"status":"4\r\n01 injected"}`, "401 injected", true},
		{`not json`, "", false},
		{`{"schemes":"bearer"}`, "", false},
	} {
		got, ok := ChallengeStatus([]byte(tc.in))
		if got != tc.want || ok != tc.ok {
			t.Errorf("ChallengeStatus(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	long, ok := ChallengeStatus([]byte(`{"status":"` + strings.Repeat("9", 100) + `"}`))
	if !ok || len(long) != 32 {
		t.Errorf("long status not capped: %d bytes", len(long))
	}
}

func TestScrub(t *testing.T) {
	got := Scrub("token "+sentinel+" and secret s3cr3t, twice "+sentinel, Secret(sentinel), Secret("s3cr3t"), Secret(""))
	if strings.Contains(got, sentinel) || strings.Contains(got, "s3cr3t") {
		t.Errorf("Scrub left a secret: %q", got)
	}
	if strings.Count(got, redacted) != 3 {
		t.Errorf("Scrub = %q, want three redactions", got)
	}
}
