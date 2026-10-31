// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/dkimverify"
	"mailroom/internal/dkimverify/dkimtest"
	"mailroom/internal/store"
)

const dkimProviderID = "mx.provider.example"

// dkimFixture builds messages signed by throwaway keys, served by a fake resolver.
type dkimFixture struct {
	t    *testing.T
	keyA *dkimtest.Key // example.com
	keyB *dkimtest.Key // example.net
	dns  map[string][]string
}

func newDKIMFixture(t *testing.T) *dkimFixture {
	f := &dkimFixture{t: t, keyA: dkimtest.NewKey(t), keyB: dkimtest.NewKey(t)}
	f.dns = map[string][]string{
		dkimtest.Name("example.com", "s1"): {f.keyA.Record()},
		dkimtest.Name("example.net", "s2"): {f.keyB.Record()},
	}
	return f
}

func (f *dkimFixture) lookup(_ context.Context, name string) ([]string, error) {
	return f.dns[name], nil
}

// msg is an unsigned message From from@fromDom, received via route.
func msg(fromDom, route, body string) string {
	return "Received: from " + route + " by mx.example.org with ESMTP; Mon, 5 Oct 2026 10:00:00 +0000\r\n" +
		"From: Sender <from@" + fromDom + ">\r\nTo: you@example.org\r\nSubject: private subject\r\n" +
		"Date: Mon, 5 Oct 2026 10:00:00 +0000\r\n\r\n" + body + "\r\n"
}

func arHeader(res string) []string {
	return []string{dkimProviderID + "; " + res}
}

func TestDKIMReportCountsAndCalibration(t *testing.T) {
	f := newDKIMFixture(t)
	ver := dkimverify.New(dkimverify.Options{LookupTXT: f.lookup})
	agg := newAuthservAggregate([]string{dkimProviderID})
	agg.noar = newNoARReport(2)
	agg.noar.dkim = newNoARDKIM(dkimConfig{verifier: ver, sample: 2})
	ctx := context.Background()

	feed := func(id, month, fromDom, raw string, headers []string) {
		ref := refAt(month, false)
		cl := agg.add(ref, headers, false, false, nil)
		if !cl.other && !cl.control {
			t.Fatalf("%s fell in neither population: %+v", id, cl)
		}
		agg.observeNoAR(ctx, noARMsg{id: id, month: month, account: "work", mailboxes: []string{"inbox"},
			fromDomain: fromDom, raw: []byte(raw)}, ref, headers, cl)
	}

	// NOAR (no Authentication-Results at all).
	feed("N1", "2026-09", "example.com", f.keyA.Sign(t, msg("example.com", "mta0.example.net", "body-one-text"), "example.com", "s1"), nil)
	feed("N2", "2026-09", "example.com", f.keyB.Sign(t, msg("example.com", "mta0.example.net", "body-two-text"), "example.net", "s2"), nil)
	feed("N3", "2026-10", "example.com", strings.Replace(f.keyA.Sign(t, msg("example.com", "relay.example.net", "body-three-text"), "example.com", "s1"), "body-three-text", "BODY", 1), nil)
	feed("N4", "2026-10", "example.com", msg("example.com", "relay.example.net", "four"), nil)
	feed("N5", "2026-10", "example.org", f.keyA.Sign(t, msg("example.org", "relay.example.net", "five"), "example.org", "s9"), nil)

	// CONTROL: sample=2 verifies the 2nd, 4th and 6th only.
	feed("C1", "2026-10", "example.com", msg("example.com", "x.example.net", "c1"), arHeader("dkim=pass header.d=example.com"))
	feed("C2", "2026-10", "example.com", f.keyA.Sign(t, msg("example.com", "x.example.net", "c2"), "example.com", "s1"), arHeader("dkim=pass header.d=example.com"))
	feed("C3", "2026-10", "example.com", msg("example.com", "x.example.net", "c3"), arHeader("dkim=pass header.d=example.com"))
	feed("C4", "2026-10", "example.com", strings.Replace(f.keyA.Sign(t, msg("example.com", "x.example.net", "body-c4-text"), "example.com", "s1"), "body-c4-text", "BODY", 1), arHeader("dkim=fail header.d=example.com"))
	feed("C5", "2026-10", "example.com", msg("example.com", "x.example.net", "c5"), arHeader("dkim=pass header.d=example.com"))
	feed("C6", "2026-10", "example.com", f.keyA.Sign(t, msg("example.com", "x.example.net", "c6"), "example.com", "s1"), arHeader("spf=pass smtp.mailfrom=example.com"))

	d := agg.noar.dkim
	if d.noarVerified != 5 || d.ctlSeen != 6 || d.ctlVerified != 3 {
		t.Fatalf("noar=%d ctlSeen=%d ctlVerified=%d", d.noarVerified, d.ctlSeen, d.ctlVerified)
	}
	// By status, per message.
	for k, want := range map[string]int{statusPassAligned: 1, statusPassUnaligned: 1, "fail_body": 1, statusNoSignature: 1, "key_not_found": 1} {
		if got := d.status.rowNoAR(k); got != want {
			t.Errorf("NOAR status %q = %d, want %d", k, got, want)
		}
	}
	if r := d.status[statusPassAligned]; r == nil || r.ctl != 2 { // C2 and C6
		t.Errorf("sampled CONTROL pass-aligned = %+v, want 2", r)
	}
	// From-domain and route tables.
	if g := d.fromDom.cell("example.com", colPassAligned); g != 1 {
		t.Errorf("from example.com pass aligned = %d", g)
	}
	if g := d.fromDom.cell("example.com", colPassUnaligned); g != 1 {
		t.Errorf("from example.com pass unaligned = %d", g)
	}
	if g := d.fromDom.cell("example.org", colKeyGone); g != 1 {
		t.Errorf("from example.org key gone = %d", g)
	}
	if g, w := d.route.cell("mta0.example.net", colPassAligned)+d.route.cell("mta0.example.net", colPassUnaligned), 2; g != w {
		t.Errorf("route mta0 passes = %d, want %d", g, w)
	}
	if g := d.route.cell("relay.example.net", colFail); g != 1 {
		t.Errorf("route relay fail = %d", g)
	}
	if d.month.cell("2026-09", "pass") != 2 || d.month.cell("2026-10", "other") != 3 {
		t.Errorf("month grid = %+v", d.month.rows)
	}
	// Agreement matrix: C2 pass/pass, C4 fail/fail, C6 none/pass.
	if d.matrix.cell("pass", "pass") != 1 || d.matrix.cell("fail", "fail") != 1 || d.matrix.cell("none", "pass") != 1 {
		t.Fatalf("matrix = %+v", d.matrix.rows)
	}
	if both, agree, _, _, _, _ := d.agreement(); both != 2 || agree != 2 {
		t.Fatalf("both=%d agree=%d", both, agree)
	}

	out := captureOut(agg.noar.print)
	for _, want := range []string{
		"NOAR messages verified     : 5 of 5",
		"every 2nd CONTROL message of 6 walked",
		"agreement where both have a verdict (pass/fail on each side): 2 of 2 (100.0%)",
		"provider pass -> local pass : 1 of 1",
		"DNS key lookups            : 3 used of a budget of 5000",
		"temperror from the budget  : 0 signature(s)",
		"NOAR messages that pass    : 2 of 5 verified (40.0%); 1 with d= aligned to From, 1 unaligned",
		"APPROXIMATED as the last two labels",
		"(all 2 rows shown; limit 15)",
		// D1c names the REASON a signature did not pass, not just its status.
		"fail_body: body hash mismatch",
		"key_not_found: no key record",
		"D1d. NON-PASS SIGNATURES BY SIGNING DOMAIN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, leak := range []string{"private subject", "from@", f.keyA.Record()[len("v=DKIM1; k=ed25519; p="):], "bh="} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q", leak)
		}
	}
	t.Log("\n" + out)
}

// A walk where nothing can be compared must say so rather than print a rate.
func TestDKIMReportSaysWhenNothingCompares(t *testing.T) {
	d := newNoARDKIM(dkimConfig{verifier: dkimverify.New(dkimverify.Options{LookupTXT: func(context.Context, string) ([]string, error) { return nil, nil }}), sample: 1})
	d.observe(context.Background(), noARMsg{id: "C", month: "2026-10", fromDomain: "example.com", raw: []byte(msg("example.com", "a.example.net", "x"))}, false,
		func() string { return "pass" })
	out := captureOut(d.print)
	if !strings.Contains(out, "nothing to compare") {
		t.Fatalf("an empty comparison was not flagged:\n%s", out)
	}
}

func TestDKIMBudgetShowsInReport(t *testing.T) {
	f := newDKIMFixture(t)
	ver := dkimverify.New(dkimverify.Options{LookupTXT: f.lookup, MaxLookups: 1})
	d := newNoARDKIM(dkimConfig{verifier: ver, sample: 1})
	for i := 0; i < 3; i++ {
		sel, dom, k := "s1", "example.com", f.keyA
		if i > 0 {
			sel, dom, k = "s2", "example.net", f.keyB
		}
		d.observe(context.Background(), noARMsg{id: fmt.Sprint(i), month: "2026-10", fromDomain: "example.com",
			raw: []byte(k.Sign(t, msg("example.com", "a.example.net", "b"), dom, sel))}, true, nil)
	}
	out := captureOut(d.print)
	if !strings.Contains(out, "temperror from the budget  : 2 signature(s)") || !strings.Contains(out, "the budget was exhausted") {
		t.Fatalf("budget bound not reported:\n%s", out)
	}
	if d.status.rowNoAR("temperror") != 2 {
		t.Fatalf("status = %+v", d.status)
	}
}

func TestDKIMVerdictFromTrustedHeaderOnly(t *testing.T) {
	ref := store.MessageAuthRef{}
	if got := providerDKIMVerdict([]string{"mx.other.example; dkim=pass"}, []string{dkimProviderID}, ref); got != "none" {
		t.Errorf("an untrusted id's dkim=pass was believed: %q", got)
	}
	for in, want := range map[string]string{
		dkimProviderID + "; dkim=pass": "pass", dkimProviderID + "; dkim=fail": "fail",
		dkimProviderID + "; dkim=pass; dkim=fail": "pass", dkimProviderID + "; dkim=neutral": "other",
		dkimProviderID + "; dkim=none": "none", dkimProviderID + "; spf=pass": "none",
	} {
		if got := providerDKIMVerdict([]string{in}, []string{dkimProviderID}, ref); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestOrgDomain(t *testing.T) {
	for in, want := range map[string]string{"mail.example.com": "example.com", "Example.COM.": "example.com", "example.com": "example.com", "localhost": "localhost", "a.b.example.co.uk": "co.uk"} {
		if got := orgDomain(in); got != want {
			t.Errorf("orgDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
