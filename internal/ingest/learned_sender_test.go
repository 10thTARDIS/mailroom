// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"strings"
	"testing"
	"time"

	"mailroom/internal/mime"
)

// #754: learned placement must not act on a message with an EMPTY From field
// beside the real one. Values("From") drops the empty field, so before this
// the message read as exactly one From — and with a trusted DMARC pass for the
// real one's domain, placement acted. Through the real parser, from real bytes.
func TestLearnedSenderRefusesAnEmptyFromBesideARealOne(t *testing.T) {
	trusted := []string{"mx.example.test"}
	parse := func(lines ...string) *mime.Message {
		t.Helper()
		m, _ := mime.ParseBytes([]byte(strings.Join(lines, "\r\n") + "\r\n\r\nbody\r\n"))
		if m == nil {
			t.Fatal("did not parse")
		}
		return m
	}
	pass := "Authentication-Results: mx.example.test; dmarc=pass header.from=bank.example"

	// CONTROL: one From field, authenticated — placement may act.
	ok := LearnedSender("alerts@bank.example", parse(pass, "From: Bank <alerts@bank.example>"), trusted, time.Time{})
	if ok.Ambiguous() || !ok.Auth().Authenticated() {
		t.Fatalf("control: a single authenticated From is ambiguous=%v authenticated=%v",
			ok.Ambiguous(), ok.Auth().Authenticated())
	}

	for _, order := range [][]string{
		{pass, "From: Bank <alerts@bank.example>", "From:"},
		{pass, "From: Bank <alerts@bank.example>", "From:   "},
	} {
		s := LearnedSender("alerts@bank.example", parse(order...), trusted, time.Time{})
		if !s.Ambiguous() {
			t.Errorf("%q: an empty From field beside the real one is not ambiguous", order)
		}
	}
}

// #766: a date-bounded authserv-id entry is judged at the message's
// received_at. Past the bound, or with no received time, the old id's DMARC
// pass is not believed (TrustConfigured stays true: configured but not
// matched, which fails closed rather than falling back to the unconfigured
// floor).
func TestLearnedSenderHonoursTheReceivedTimeBound(t *testing.T) {
	entries := []string{"mx.example.test", "old.example.test<2026-07-01"}
	m, _ := mime.ParseBytes([]byte("Authentication-Results: old.example.test; dmarc=pass header.from=bank.example\r\n" +
		"From: Bank <alerts@bank.example>\r\n\r\nbody\r\n"))
	if m == nil {
		t.Fatal("did not parse")
	}
	before := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if s := LearnedSender("alerts@bank.example", m, entries, before); !s.Auth().Authenticated() {
		t.Fatal("CONTROL FAILED: a pass from the old id, received before the bound, is not believed")
	}
	for name, at := range map[string]time.Time{"at the bound": after, "zero time": {}} {
		if s := LearnedSender("alerts@bank.example", m, entries, at); s.Auth().Authenticated() {
			t.Errorf("%s: the old id's pass was believed past its bound", name)
		}
	}
}

// #846: learned placement reads DMARC with RELAXED alignment. A subdomain
// sender whose trusted header carries a pass for its organisational domain is
// authenticated, and the match is recorded as "org-domain" for
// placement-explain; a sibling's pass is not.
//
// NEGATIVE CONTROL: LearnedSender switched to ml.DMARCStrict turns the first
// assertion red.
func TestLearnedSenderUsesRelaxedDMARCAlignment(t *testing.T) {
	trusted := []string{"mx.example.test"}
	parse := func(lines ...string) *mime.Message {
		t.Helper()
		m, _ := mime.ParseBytes([]byte(strings.Join(lines, "\r\n") + "\r\n\r\nbody\r\n"))
		if m == nil {
			t.Fatal("did not parse")
		}
		return m
	}
	const from = "hello@news.bank.example"
	ar := func(hf string) string {
		return "Authentication-Results: mx.example.test; dkim=pass header.d=bank.example; dmarc=pass (policy=reject) header.from=" + hf
	}
	s := LearnedSender(from, parse(ar("bank.example"), "From: <"+from+">"), trusted, time.Time{})
	if a := s.Auth(); !a.Authenticated() || !a.DMARCPass || a.DMARCMatch != "org-domain" {
		t.Errorf("org-domain pass: auth = %+v, want authenticated with an org-domain match", a)
	}
	s = LearnedSender(from, parse(ar(from[6:]), "From: <"+from+">"), trusted, time.Time{})
	if a := s.Auth(); !a.Authenticated() || a.DMARCMatch != "exact" {
		t.Errorf("exact pass: auth = %+v, want authenticated with an exact match", a)
	}
	s = LearnedSender(from, parse(ar("shop.bank.example"), "From: <"+from+">"), trusted, time.Time{})
	if a := s.Auth(); a.Authenticated() || a.DMARCPass || a.DMARCMatch != "" {
		t.Errorf("sibling pass: auth = %+v, want unauthenticated", a)
	}
}
