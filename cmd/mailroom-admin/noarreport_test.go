// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// Realistic Received shapes, hosts rewritten to reserved names.
func TestReceivedHosts(t *testing.T) {
	cases := []struct{ name, in, by, from string }{
		{"postfix", "from mail.example.net (mail.example.net [192.0.2.10])\r\n\tby mx.example.com (Postfix) with ESMTPS id 4abcDEF123\r\n\tfor <you@example.com>; Mon, 5 Oct 2026 10:00:00 +0000 (UTC)",
			"mx.example.com", "mail.example.net"},
		{"exim with bracketed literal", "from [192.0.2.44] (helo=Relay.Example.NET) by mx.example.com with esmtps (TLS1.3) tls TLS_AES_256_GCM_SHA384 (Exim 4.97) id 1abcDE-000123-Zz; Mon, 05 Oct 2026 10:00:00 +0000",
			"mx.example.com", ipHost},
		{"gmail ipv6-ish by", "by 2002:a05:6a00:1234:b0:7f:1234:5678 with SMTP id abc123csp1234;\r\n        Mon, 5 Oct 2026 03:00:00 -0700 (PDT)",
			ipHost, unparsedHost},
		{"gmail smtp-source", "from mail-sor-f41.example.net (mail-sor-f41.example.net. [2001:db8::41])\r\n        by mx.example.com with SMTPS id d9443c01a7336-abc.1.2026.10.05.03.00.00\r\n        for <you@example.com>\r\n        (Google Transport Security);\r\n        Mon, 05 Oct 2026 03:00:00 -0700 (PDT)",
			"mx.example.com", "mail-sor-f41.example.net"},
		{"microsoft", "from AM0PR01MB1234.eurprd01.prod.example.net (2603:10a6:208:1::10)\r\n by AM9PR01MB5678.eurprd01.prod.example.net (2603:10a6:20b:2::12) with\r\n Microsoft SMTP Server (version=TLS1_2, cipher=TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384) id\r\n 15.20.1234.5; Mon, 5 Oct 2026 10:00:00 +0000",
			"am9pr01mb5678.eurprd01.prod.example.net", "am0pr01mb1234.eurprd01.prod.example.net"},
		{"trailing dot and ipv6 prefix", "from MX1.Example.NET. (unknown [IPv6:2001:db8::1]) by ipv6:2001:db8::2 with ESMTP; Mon, 5 Oct 2026 10:00:00 +0000",
			ipHost, "mx1.example.net"},
		{"garbage", "(qmail 1234 invoked by uid 0); 5 Oct 2026", unparsedHost, unparsedHost},
		{"empty", "", unparsedHost, unparsedHost},
	}
	for _, c := range cases {
		by, from := receivedHosts(c.in)
		if by != c.by || from != c.from {
			t.Errorf("%s: got by=%q from=%q, want by=%q from=%q", c.name, by, from, c.by, c.from)
		}
	}
}

func TestParseHeaderFieldsFoldsAndStopsAtBody(t *testing.T) {
	raw := "Received: from a.example.net\n by b.example.com;\n date\nX-Forwarded-For: x\nnot a header\n\nResent-From: in-body@example.com\n"
	fs := parseHeaderFields([]byte(raw))
	if len(fs) != 2 || fs[0].name != "Received" || !strings.Contains(fs[0].value, "by b.example.com; date") || fs[1].name != "X-Forwarded-For" {
		t.Fatalf("fields = %+v", fs)
	}
}

func captureOut(f func()) string {
	var sb strings.Builder
	old := reportOut
	reportOut = &sb
	defer func() { reportOut = old }()
	f()
	return sb.String()
}

func hdrMsg(id, month, acct string, boxes []string, from, headers string) noARMsg {
	return noARMsg{id: id, month: month, account: acct, mailboxes: boxes, fromDomain: from, raw: []byte(headers + "\r\n\r\nbody")}
}

const noarHeaders = "Received: from mail.example.net ([192.0.2.1]) by mx.example.com with ESMTP; Mon, 5 Oct 2026 10:00:00 +0000\r\n" +
	"Received: from relay.example.net by mail.example.net; Mon, 5 Oct 2026 09:59:00 +0000\r\n" +
	"Return-Path: <>\r\nDelivered-To: you@example.org\r\nX-Forwarded-For: a@example.net"

const ctlHeaders = "Received: from mail.example.net ([192.0.2.1]) by mx.example.com with ESMTP; Mon, 5 Oct 2026 10:00:00 +0000\r\n" +
	"DKIM-Signature: v=1; a=rsa-sha256; d=example.net\r\nList-Id: <l.example.net>"

func TestNoARReportCountsAndBounds(t *testing.T) {
	r := newNoARReport(2)
	for i := 0; i < 3; i++ {
		r.observe(hdrMsg(fmt.Sprintf("N%d", i), "2026-10", "work", []string{"inbox"}, "a.example.net", noarHeaders), true)
	}
	r.observe(hdrMsg("N3", "2026-10", "home", []string{"Old/Stuff"}, "b.example.net", "Received: from [192.0.2.9] by x.example.com; d"), true)
	for i := 0; i < 5; i++ {
		r.observe(hdrMsg(fmt.Sprintf("C%d", i), "2026-10", "work", []string{"inbox"}, "a.example.net", ctlHeaders), false)
	}
	// A CONTROL message in a month with no NOAR must be left out and reported.
	r.observe(hdrMsg("C9", "2025-01", "work", []string{"inbox"}, "z.example.net", ctlHeaders), false)

	tb, outside := r.combined()
	if tb.noar != 4 || tb.ctl != 5 || outside != 1 {
		t.Fatalf("totals noar=%d ctl=%d outside=%d", tb.noar, tb.ctl, outside)
	}
	if g := tb.monthAccount["2026-10  work"]; g == nil || g.noar != 3 || g.ctl != 5 || len(g.examples) != 2 {
		t.Fatalf("month x account = %+v (examples must be capped at 2)", g)
	}
	if g := tb.monthAccount["2025-01  work"]; g != nil && g.ctl != 0 {
		t.Fatalf("control outside NOAR months leaked in: %+v", g)
	}
	if g := tb.received["2"]; g == nil || g.noar != 3 || g.ctl != 0 {
		t.Fatalf("received=2 row = %+v", g)
	}
	if g := tb.received["1"]; g == nil || g.noar != 1 || g.ctl != 5 {
		t.Fatalf("received=1 row = %+v", g)
	}
	if g := tb.recvFrom[ipHost]; g == nil || g.noar != 1 {
		t.Fatalf("from [ip] row = %+v", g)
	}
	if g := tb.recvFrom["mail.example.net"]; g == nil || g.noar != 3 || g.ctl != 5 {
		t.Fatalf("from host row = %+v", g)
	}
	if g := tb.presence["Return-Path empty or <>"]; g == nil || g.noar != 3 || g.ctl != 0 {
		t.Fatalf("return-path row = %+v", g)
	}
	if g := tb.presence["DKIM-Signature"]; g == nil || g.noar != 0 || g.ctl != 5 {
		t.Fatalf("dkim row = %+v", g)
	}
	if g := tb.presence["X-Forwarded-*"]; g == nil || g.noar != 3 {
		t.Fatalf("x-forwarded row = %+v", g)
	}
	if g := tb.deliveredTo["example.org"]; g == nil || g.noar != 3 {
		t.Fatalf("delivered-to domain row = %+v", g)
	}
	if g := tb.recvBy["x.example.com"]; g == nil || g.noar != 1 {
		t.Fatalf("by host = %+v", g)
	}

	out := captureOut(r.print)
	if !strings.Contains(out, "CONTROL messages : 5 ") {
		t.Fatalf("print must be idempotent and count CONTROL once:\n%s", out)
	}
	if !strings.Contains(out, "ARC-Seal") {
		t.Fatalf("a header absent from both populations must still get a zero row:\n%s", out)
	}
	if !strings.Contains(out, "NOAR messages    : 4") || !strings.Contains(out, "1 more CONTROL messages fall in months with no NOAR") {
		t.Fatalf("totals missing from output:\n%s", out)
	}
	if strings.Contains(out, "C9") {
		t.Fatalf("a control example id leaked (examples are NOAR only):\n%s", out)
	}
	if !strings.Contains(out, "e.g. N0 N1") {
		t.Fatalf("NOAR examples missing:\n%s", out)
	}
	t.Log("\n" + out)
}

func TestPrintNoARTableReportsItsCut(t *testing.T) {
	tb := noARTable{}
	for i, k := range []string{"a", "b", "c", "d"} {
		for j := 0; j <= i; j++ {
			tb.bump(k, true, "", 0)
		}
		tb.bump(k, false, "", 0)
	}
	out := captureOut(func() { printNoARTable("T", tb, 2, byCount, false) })
	// d=4,c=3 shown; a=1,b=2 cut -> 3 NOAR, 2 CONTROL.
	if !strings.Contains(out, "+2 more rows, 3 NOAR / 2 CONTROL messages (table shows top 2 of 4 rows)") {
		t.Fatalf("bound not reported:\n%s", out)
	}
	if strings.Contains(out, "\n  a ") || !strings.Contains(out, "\n  d ") {
		t.Fatalf("wrong rows shown:\n%s", out)
	}
	full := captureOut(func() { printNoARTable("T", tb, 10, byCount, false) })
	if !strings.Contains(full, "all 4 rows shown; limit 10") {
		t.Fatalf("uncut table must say so:\n%s", full)
	}
}

// The population split is add()'s job: NOAR is exactly the "other inbound" row,
// CONTROL is inbound with a trusted header.
func TestAddClassifiesPopulations(t *testing.T) {
	a := newAuthservAggregate([]string{"mx.provider.example"})
	trusted := []string{"mx.provider.example; dmarc=pass"}
	other := []string{"mx.old.example; spf=pass"}
	cases := []struct {
		name         string
		ref          store.MessageAuthRef
		headers      []string
		self, fromMe bool
		want         authClass
	}{
		{"other inbound", refAt("2026-10", false), nil, false, false, authClass{other: true}},
		{"local no header", refAt("2026-10", true), nil, false, false, authClass{}},
		{"sent no header", refAt("2026-10", false), nil, true, false, authClass{}},
		{"from own identity", refAt("2026-10", false), nil, false, true, authClass{}},
		{"inbound trusted", refAt("2026-10", false), trusted, false, false, authClass{control: true}},
		{"sent with trusted header", refAt("2026-10", false), trusted, true, false, authClass{}},
		{"local with trusted header", refAt("2026-10", true), trusted, false, false, authClass{}},
		{"inbound untrusted only", refAt("2026-10", false), other, false, false, authClass{}},
	}
	for _, c := range cases {
		if got := a.add(c.ref, c.headers, c.self, c.fromMe, nil); got != c.want {
			t.Errorf("%s: class = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Sender-written header values reach the operator's terminal through the
// table keys; control characters (an ANSI escape here) must not.
func TestNoARTableStripsControlCharacters(t *testing.T) {
	tb := noARTable{}
	tb.bump("evil\x1b[2Jhost.example", true, "", 0)
	out := captureOut(func() { printNoARTable("T", tb, 10, byCount, false) })
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("an escape character from a header reached the report output: %q", out)
	}
	if !strings.Contains(out, "evil[2Jhost.example") {
		t.Fatalf("the row itself was lost rather than cleaned: %q", out)
	}
}

// Many continuation lines on one field must unfold in linear time and keep
// every part.
func TestParseHeaderFieldsUnfoldsManyContinuations(t *testing.T) {
	var b strings.Builder
	b.WriteString("Received: from a.example.net\r\n")
	for i := 0; i < 20000; i++ {
		b.WriteString("\tx\r\n")
	}
	b.WriteString("Subject: s\r\n\r\nbody")
	f := parseHeaderFields([]byte(b.String()))
	if len(f) != 2 || f[0].name != "Received" || strings.Count(f[0].value, " x") != 20000 {
		t.Fatalf("fields = %d, first = %q (len %d)", len(f), f[0].name, len(f[0].value))
	}
}
