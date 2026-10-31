// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

func parseHeaders(t *testing.T, lines ...string) *mime.Message {
	t.Helper()
	m, _ := mime.ParseBytes([]byte(strings.Join(lines, "\r\n") + "\r\n\r\nbody\r\n"))
	if m == nil {
		t.Fatal("did not parse")
	}
	return m
}

// Every shape, from real header bytes through the real mime parser — and the
// plain case as the control, so a classifier that called everything unusual
// would fail here too.
func TestClassifyFromEveryShape(t *testing.T) {
	cases := []struct {
		want  string
		lines []string
	}{
		{"one", []string{"From: Ann <ann@example.com>", "To: x@example.net"}},
		{"none", []string{"To: x@example.net"}},
		{"multiEmptyPlus", []string{"From:", "From: ann@example.com"}},
		{"multiEmptyPlus", []string{"From: ann@example.com", "From:   "}},
		{"multiAllEmpty", []string{"From:", "From: "}},
		{"multi", []string{"From: ann@example.com", "From: mallory@example.org"}},
		{"oneEmpty", []string{"From:", "To: x@example.net"}},
		{"group", []string{"From: Team: ann@example.com, bob@example.com;"}},
		{"multiMailbox", []string{"From: ann@example.com, bob@example.com"}},
		{"unparsable", []string{"From: <<<not an address"}},
	}
	for _, c := range cases {
		if got := classifyFrom(parseHeaders(t, c.lines...).Header); got != c.want {
			t.Errorf("%q: shape = %s, want %s", c.lines, got, c.want)
		}
	}
}

// The notice column restates web/src/core/senderAuth.ts senderAuthNotice over
// what the server sends; these are that function's own cases.
func TestAuthNoticeShownMirrorsTheClientRule(t *testing.T) {
	trusted := []string{"mx.example.test"}
	parse := func(v ...string) ml.AuthResults { return ml.TrustedAuthResults(v, trusted) }
	untrusted := "evil.example; dmarc=pass header.from=example.com"
	cases := []struct {
		name    string
		res     ml.AuthResults
		local   bool
		self    bool
		trusted bool
		want    string
	}{
		{"nothing configured: the server sends nothing", parse(), false, false, false, ""},
		{"our own mail with no header: null", parse(), true, false, true, ""},
		{"Sent/Drafts, no header: null", parse(), false, true, true, ""},
		{"arrived with no header at all", parse(), false, false, true, "unauthenticated"},
		{"untrusted id only: still unauthenticated (#766)", parse(untrusted), false, false, true, "unauthenticated"},
		{"untrusted id only, own mail: none", parse(untrusted), true, true, true, ""},
		{"dmarc fail", parse("mx.example.test; dmarc=fail header.from=example.com"), false, false, true, "failed"},
		{"dkim fail and pass: not failed", parse("mx.example.test; dkim=fail; dkim=pass"), false, false, true, ""},
		{"spf temperror", parse("mx.example.test; spf=temperror"), false, false, true, "unverifiable"},
		{"all pass", parse("mx.example.test; spf=pass; dkim=pass; dmarc=pass header.from=example.com"), false, false, true, ""},
	}
	for _, c := range cases {
		if got := authNoticeShown(c.res, c.local, c.self, c.trusted); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func captureFromReport(a *fromAggregate) string {
	var sb strings.Builder
	old := reportOut
	reportOut = &sb
	defer func() { reportOut = old }()
	a.print()
	return sb.String()
}

func TestFromReportCountsPerShapeAndPrintsNoAddress(t *testing.T) {
	a := newFromAggregate([]string{"mx.example.test"}, 2)
	pass := "Authentication-Results: mx.example.test; dmarc=pass header.from=example.com"
	id1, id2 := ulid.Make(), ulid.Make()
	msgs := []struct {
		ref   store.MessageFromRef
		lines []string
	}{
		{store.MessageFromRef{ID: ulid.Make()}, []string{pass, "From: secret-one@example.com"}},
		{store.MessageFromRef{ID: id1, LearnedPlaced: true}, []string{pass, "From:", "From: secret-two@example.com"}},
		{store.MessageFromRef{ID: id2}, []string{"From: secret-three@example.com", "From: secret-four@example.org"}},
	}
	for _, m := range msgs {
		a.scanned++
		a.add(m.ref, parseHeaders(t, m.lines...), false)
	}
	// dmarcPass is 0 here, and that is a measured fact about the parser, not a
	// slip: mime.Header.From() reads the FIRST From field, which is the empty
	// one, so no From domain exists to bind the pass to.
	if r := a.rows["multiEmptyPlus"]; r.messages != 1 || r.learnedPlaced != 1 || r.dmarcPass != 0 {
		t.Fatalf("multiEmptyPlus row = %+v", r)
	}
	// The same shape with the real field FIRST does bind the pass — the
	// control that shows the 0 above is the field order, not a broken column.
	b := newFromAggregate([]string{"mx.example.test"}, 0)
	b.add(store.MessageFromRef{ID: ulid.Make()}, parseHeaders(t, pass, "From: secret-five@example.com", "From:"), false)
	if r := b.rows["multiEmptyPlus"]; r.messages != 1 || r.dmarcPass != 1 {
		t.Fatalf("real-From-first multiEmptyPlus row = %+v", r)
	}
	if r := a.rows["multi"]; r.messages != 1 || r.notice["unauthenticated"] != 1 || r.dmarcPass != 0 {
		t.Fatalf("multi row = %+v", r)
	}
	if r := a.rows["one"]; r.messages != 1 || r.dmarcPass != 1 || len(r.examples) != 0 {
		t.Fatalf("one row = %+v", r)
	}
	out := captureFromReport(a)
	for _, s := range fromShapes {
		if !strings.Contains(out, s+" ") {
			t.Errorf("shape %s not printed", s)
		}
	}
	if !strings.Contains(out, "not exactly one plain From : 2 of 3 readable") {
		t.Errorf("bounded total missing:\n%s", out)
	}
	if !strings.Contains(out, id1.String()) || !strings.Contains(out, id2.String()) {
		t.Errorf("example ids not printed:\n%s", out)
	}
	// PRIVACY: ids only. An "@" anywhere in the output is an address leak.
	if strings.Contains(out, "@") || strings.Contains(out, "secret") {
		t.Fatalf("the report printed an address:\n%s", out)
	}
}

func TestFromReportSaysWhenNothingIsTrusted(t *testing.T) {
	a := newFromAggregate(nil, 0)
	a.scanned = 1
	a.add(store.MessageFromRef{ID: ulid.Make()}, parseHeaders(t,
		"Authentication-Results: mx.example.test; dmarc=pass header.from=example.com", "From: a@example.com"), false)
	if r := a.rows["one"]; r.dmarcPass != 0 || len(r.notice) != 0 {
		t.Fatalf("with nothing trusted nothing may count as a pass or a notice: %+v", r)
	}
	if out := captureFromReport(a); !strings.Contains(out, "NONE configured") {
		t.Fatalf("the zero is not explained:\n%s", out)
	}
}

// The DMARC column measures the verdict learned placement acts on, so it
// counts a subdomain sender's organisational-domain pass (#846, relaxed) and
// still not a sibling's.
//
// NEGATIVE CONTROL: the report switched to ml.DMARCStrict turns the first
// assertion red.
func TestFromReportDMARCColumnIsRelaxedLikePlacement(t *testing.T) {
	a := newFromAggregate([]string{"mx.example.test"}, 0)
	a.add(store.MessageFromRef{ID: ulid.Make()}, parseHeaders(t,
		"Authentication-Results: mx.example.test; dmarc=pass header.from=example.com", "From: a@news.example.com"), false)
	if r := a.rows["one"]; r.messages != 1 || r.dmarcPass != 1 {
		t.Fatalf("org-domain pass row = %+v, want it counted", r)
	}
	b := newFromAggregate([]string{"mx.example.test"}, 0)
	b.add(store.MessageFromRef{ID: ulid.Make()}, parseHeaders(t,
		"Authentication-Results: mx.example.test; dmarc=pass header.from=shop.example.com", "From: a@news.example.com"), false)
	if r := b.rows["one"]; r.messages != 1 || r.dmarcPass != 0 {
		t.Fatalf("sibling pass row = %+v, want it not counted", r)
	}
}
