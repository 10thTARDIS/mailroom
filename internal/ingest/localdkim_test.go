// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bytes"

	"github.com/emersion/go-msgauth/dkim"

	"mailroom/internal/dkimverify"
	"mailroom/internal/dkimverify/dkimtest"
	"mailroom/internal/mime"
)

func TestDKIMAligned(t *testing.T) {
	cases := []struct {
		name     string
		from, d  string
		want     bool
		negative string // what this case excludes, for the reader
	}{
		{"exact", "example.com", "example.com", true, ""},
		{"exact, case and trailing dot", "Example.COM.", "example.com", true, ""},
		{"From is a subdomain of d=", "mail.example.com", "example.com", true, ""},
		{"From is a deep subdomain of d=", "a.b.example.com", "example.com", true, ""},
		{"d= is a subdomain of From (not aligned)", "example.com", "mail.example.com", false, "a subdomain signing for its parent"},
		{"sibling subdomains", "a.example.com", "b.example.com", false, ""},
		{"suffix match without a label boundary", "badexample.com", "example.com", false, "HasSuffix without the dot"},
		{"unrelated domain", "example.com", "example.net", false, ""},
		{"d= is a TLD", "example.com", "com", false, "d=com speaking for every .com"},
		{"d= is a TLD, From equal", "com", "com", false, ""},
		{"d= is a co.uk-style public suffix", "shop.example.co.uk", "co.uk", false, "d=co.uk speaking for every .co.uk"},
		{"co.uk-style: the registrable domain is fine", "shop.example.co.uk", "example.co.uk", true, ""},
		{"co.uk-style: exact registrable", "example.co.uk", "example.co.uk", true, ""},
		{"private-section suffix (a shared host)", "tenant.blogspot.com", "blogspot.com", false, "one tenant signing for another"},
		{"private-section: the tenant's own name", "tenant.blogspot.com", "tenant.blogspot.com", true, ""},
		{"IDN From and A-label d=", "mail.bücher.de", "xn--bcher-kva.de", true, ""},
		{"IDN A-label From and U-label d=", "mail.xn--bcher-kva.de", "bücher.de", true, ""},
		{"empty From", "", "example.com", false, ""},
		{"empty d=", "example.com", "", false, ""},
	}
	for _, c := range cases {
		if got := DKIMAligned(c.from, c.d); got != c.want {
			t.Errorf("%s: DKIMAligned(%q, %q) = %v, want %v", c.name, c.from, c.d, got, c.want)
		}
	}
}

func TestDecideLocalDKIM(t *testing.T) {
	pass := func(d string) dkimverify.Result {
		return dkimverify.Result{Domain: d, Status: dkimverify.StatusPass, SignedHeaders: []string{"from", "subject"}}
	}
	st := func(d string, s dkimverify.Status) dkimverify.Result { return dkimverify.Result{Domain: d, Status: s} }
	cases := []struct {
		name    string
		results []dkimverify.Result
		store   bool
		result  string
		aligned bool
		domain  string
		label   string
	}{
		{"aligned pass", []dkimverify.Result{pass("example.com")}, true, "pass", true, "example.com", LocalDKIMPass},
		{"aligned pass beside a fail", []dkimverify.Result{st("example.com", dkimverify.StatusFailBody), pass("example.com")}, true, "pass", true, "example.com", LocalDKIMPass},
		{"unaligned pass is stored, aligned=false", []dkimverify.Result{pass("esp.example.net")}, true, "pass", false, "esp.example.net", LocalDKIMPassUnaligned},
		{"unaligned pass beside an aligned one: aligned wins", []dkimverify.Result{pass("esp.example.net"), pass("example.com")}, true, "pass", true, "example.com", LocalDKIMPass},
		{"pass for a public suffix is unaligned", []dkimverify.Result{pass("com")}, true, "pass", false, "com", LocalDKIMPassUnaligned},
		{"fail_body is stored", []dkimverify.Result{st("example.com", dkimverify.StatusFailBody)}, true, "fail_body", false, "example.com", "fail_body"},
		{"most informative failure wins", []dkimverify.Result{st("a.example", dkimverify.StatusUnsupported), st("example.com", dkimverify.StatusFailSignature), st("b.example", dkimverify.StatusKeyNotFound)}, true, "fail_signature", false, "example.com", "fail_signature"},
		{"key not found", []dkimverify.Result{st("example.com", dkimverify.StatusKeyNotFound)}, true, "key_not_found", false, "example.com", "key_not_found"},
		{"permerror (expired)", []dkimverify.Result{st("example.com", dkimverify.StatusPermError)}, true, "permerror", false, "example.com", "permerror"},
		{"unsupported", []dkimverify.Result{st("", dkimverify.StatusUnsupported)}, true, "unsupported", false, "", "unsupported"},
		// Nothing is stored for a transient failure, even beside a failure or an
		// unaligned pass: the undecided signature might have been the aligned one.
		{"temperror alone", []dkimverify.Result{st("example.com", dkimverify.StatusTempError)}, false, "", false, "", LocalDKIMNotChecked},
		{"temperror beside a fail", []dkimverify.Result{st("example.com", dkimverify.StatusTempError), st("example.com", dkimverify.StatusFailBody)}, false, "", false, "", LocalDKIMNotChecked},
		{"temperror beside an unaligned pass", []dkimverify.Result{pass("esp.example.net"), st("example.com", dkimverify.StatusTempError)}, false, "", false, "", LocalDKIMNotChecked},
		{"temperror beside an ALIGNED pass: the pass stands", []dkimverify.Result{st("x.example", dkimverify.StatusTempError), pass("example.com")}, true, "pass", true, "example.com", LocalDKIMPass},
		{"no signature", nil, false, "", false, "", LocalDKIMNoSignature},
	}
	for _, c := range cases {
		got := decideLocalDKIM(c.results, "mail.example.com", func(h string) int {
			if h == "From" || h == "Subject" {
				return 1
			}
			return 0
		})
		if got.label != c.label {
			t.Errorf("%s: label %q, want %q", c.name, got.label, c.label)
		}
		if (got.row != nil) != c.store {
			t.Errorf("%s: stored = %v, want %v", c.name, got.row != nil, c.store)
			continue
		}
		if got.row != nil && (got.row.Result != c.result || got.row.Aligned != c.aligned || got.row.Domain != c.domain) {
			t.Errorf("%s: row %+v, want result=%s aligned=%v domain=%q", c.name, *got.row, c.result, c.aligned, c.domain)
		}
	}
}

const ldTrustedHeader = "Authentication-Results: mx.provider.example; dkim=fail\r\n"

func ldMessage(from string, extra string) string {
	return extra + "From: Alice <alice@" + from + ">\r\nTo: you@example.org\r\nSubject: hi\r\nDate: Mon, 5 Oct 2026 10:00:00 +0000\r\n\r\nHello.\r\n"
}

func ldIngestor(lookup dkimverify.LookupTXTFunc, trusted ...string) *Ingestor {
	in := &Ingestor{trustedAuthservIDs: trusted}
	WithLocalDKIM(nil)(in)
	withDKIMLookup(lookup)(in)
	return in
}

func ldCheck(t *testing.T, in *Ingestor, raw string, from string) localDKIMOutcome {
	t.Helper()
	parsed, _ := mime.ParseBytes([]byte(raw))
	return in.checkLocalDKIM(context.Background(), []byte(raw), parsed, "alice@"+from, time.Now())
}

func TestCheckLocalDKIM(t *testing.T) {
	key := dkimtest.NewKey(t)
	var lookups atomic.Int32
	dns := func(_ context.Context, name string) ([]string, error) {
		lookups.Add(1)
		if name == dkimtest.Name("example.com", "s1") {
			return []string{key.Record()}, nil
		}
		return nil, nil
	}
	signed := key.Sign(t, ldMessage("example.com", ""), "example.com", "s1")

	t.Run("no trusted header: an aligned pass is stored", func(t *testing.T) {
		got := ldCheck(t, ldIngestor(dns, "mx.provider.example"), signed, "example.com")
		if got.row == nil || got.row.Result != "pass" || !got.row.Aligned || got.row.Domain != "example.com" {
			t.Fatalf("got %+v (label %s)", got, got.label)
		}
	})

	t.Run("a trusted header: skipped, and NO lookup is made", func(t *testing.T) {
		lookups.Store(0)
		got := ldCheck(t, ldIngestor(dns, "mx.provider.example"), ldTrustedHeader+signed, "example.com")
		if got.row != nil || got.label != LocalDKIMSkippedTrusted || lookups.Load() != 0 {
			t.Fatalf("got %+v, lookups %d", got, lookups.Load())
		}
		// CONTROL: the same bytes under a different trusted id are checked, so
		// the assertion above excludes something this setup can produce.
		if got := ldCheck(t, ldIngestor(dns, "other.provider.example"), ldTrustedHeader+signed, "example.com"); got.row == nil {
			t.Fatalf("CONTROL FAILED: an untrusted header suppressed the check: %+v", got)
		}
	})

	t.Run("a date-bounded entry past its bound does not skip", func(t *testing.T) {
		parsed, _ := mime.ParseBytes([]byte(ldTrustedHeader + signed))
		in := ldIngestor(dns, "mx.provider.example<2026-01-01")
		got := in.checkLocalDKIM(context.Background(), []byte(ldTrustedHeader+signed), parsed, "alice@example.com", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
		if got.label == LocalDKIMSkippedTrusted {
			t.Fatal("a header past the bound was trusted")
		}
		got = in.checkLocalDKIM(context.Background(), []byte(ldTrustedHeader+signed), parsed, "alice@example.com", time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
		if got.label != LocalDKIMSkippedTrusted {
			t.Fatalf("a header inside the bound was not trusted: %+v", got)
		}
	})

	t.Run("a pass for another domain is stored unaligned", func(t *testing.T) {
		got := ldCheck(t, ldIngestor(dns), signed, "other.example")
		if got.row == nil || got.row.Aligned || got.row.Result != "pass" || got.label != LocalDKIMPassUnaligned {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("a tampered body is a stored fail", func(t *testing.T) {
		got := ldCheck(t, ldIngestor(dns), strings.Replace(signed, "Hello.", "Hello!", 1), "example.com")
		if got.row == nil || got.row.Result != "fail_body" || got.row.Aligned {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("a DNS failure stores nothing", func(t *testing.T) {
		failing := func(context.Context, string) ([]string, error) { return nil, errors.New("servfail") }
		got := ldCheck(t, ldIngestor(failing), signed, "example.com")
		if got.row != nil || got.label != LocalDKIMNotChecked {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("no signature stores nothing", func(t *testing.T) {
		got := ldCheck(t, ldIngestor(dns), ldMessage("example.com", ""), "example.com")
		if got.row != nil || got.label != LocalDKIMNoSignature {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("disabled: nothing, no label", func(t *testing.T) {
		parsed, _ := mime.ParseBytes([]byte(signed))
		got := (&Ingestor{}).checkLocalDKIM(context.Background(), []byte(signed), parsed, "alice@example.com", time.Now())
		if got.row != nil || got.label != "" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("at most four lookups per message; the rest is not_checked", func(t *testing.T) {
		// Six signatures from six domains, none publishing a key the budget lets
		// us reach: the cap stops at 4 and the remainder is a temperror.
		msg := ldMessage("example.com", "")
		for i := 0; i < 6; i++ {
			msg = key.Sign(t, msg, "d"+string(rune('a'+i))+".example", "s")
		}
		lookups.Store(0)
		got := ldCheck(t, ldIngestor(dns), msg, "example.com")
		if n := lookups.Load(); n != localDKIMMaxLookups {
			t.Fatalf("%d lookups, want exactly %d", n, localDKIMMaxLookups)
		}
		if got.row != nil || got.label != LocalDKIMNotChecked {
			t.Fatalf("a spent budget must store nothing: %+v", got)
		}
	})

	t.Run("a cancelled context stores nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		in := ldIngestor(func(ctx context.Context, _ string) ([]string, error) { return nil, ctx.Err() })
		parsed, _ := mime.ParseBytes([]byte(signed))
		got := in.checkLocalDKIM(ctx, []byte(signed), parsed, "alice@example.com", time.Now())
		if got.row != nil || got.label != LocalDKIMNotChecked {
			t.Fatalf("got %+v", got)
		}
	})
}

// The header-prepend attack (RFC 6376 §8.15, ADR-036): genuinely signed mail
// with an extra header ABOVE the signed ones still verifies, so a pass must not
// be displayed when a covered header is duplicated.
func TestLocalDKIMHeaderPrepend(t *testing.T) {
	key := dkimtest.NewKey(t)
	dns := func(_ context.Context, name string) ([]string, error) {
		if name == dkimtest.Name("example.com", "s1") {
			return []string{key.Record()}, nil
		}
		return nil, nil
	}
	signed := key.Sign(t, ldMessage("example.com", ""), "example.com", "s1")
	in := ldIngestor(dns)

	// CONTROL: the untouched message is a displayable pass, so every "blocked"
	// below is caused by the prepend and not by the setup.
	if got := ldCheck(t, in, signed, "example.com"); got.row == nil || !got.row.Aligned || got.row.DisplayBlocked != "" {
		t.Fatalf("CONTROL FAILED: %+v", got)
	}

	cases := []struct {
		name, prepend, why string
	}{
		{"second From", "From: Boss <boss@example.com>\r\n", "duplicate_header:from"},
		{"duplicated Subject", "Subject: Urgent\r\n", "duplicate_header:subject"},
		{"added Reply-To the signature never covered", "Reply-To: evil@example.net\r\n", "unsigned_header:reply-to"},
		{"added Cc the signature never covered", "Cc: evil@example.net\r\n", "unsigned_header:cc"},
	}
	for _, c := range cases {
		got := ldCheck(t, in, c.prepend+signed, "example.com")
		if got.row == nil || got.row.Result != "pass" {
			t.Errorf("%s: want the pass stored, got %+v", c.name, got)
			continue
		}
		if c.why == "" {
			continue
		}
		wantLabel := LocalDKIMPassBlocked
		if strings.HasPrefix(c.why, "unsigned_header:") {
			wantLabel = LocalDKIMPassBlockedUnsigned
		}
		if got.row.Aligned || got.row.DisplayBlocked != c.why || got.label != wantLabel {
			t.Errorf("%s: %+v (label %s), want aligned=false blocked=%q", c.name, *got.row, got.label, c.why)
		}
	}

	t.Run("From not in h= is not displayed", func(t *testing.T) {
		var out bytes.Buffer
		err := dkim.Sign(&out, strings.NewReader(ldMessage("example.com", "")), &dkim.SignOptions{
			Domain: "example.com", Selector: "s1", Signer: key.Signer(), HeaderKeys: []string{"Subject", "To"}})
		if err != nil {
			t.Skip("library refuses to sign without From: " + err.Error())
		}
		got := ldCheck(t, in, out.String(), "example.com")
		if got.row != nil && got.row.Aligned {
			t.Fatalf("displayed a pass whose signature does not cover From: %+v", got.row)
		}
	})

	t.Run("a header the signature does not cover but the message has is blocked", func(t *testing.T) {
		msg := "From: Alice <alice@example.com>\r\nTo: you@example.org\r\nCc: carol@example.org\r\nSubject: hi\r\n\r\nHello.\r\n"
		var out bytes.Buffer
		if err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{
			Domain: "example.com", Selector: "s1", Signer: key.Signer(), HeaderKeys: []string{"From", "To", "Subject"}}); err != nil {
			t.Fatal(err)
		}
		got := ldCheck(t, in, out.String(), "example.com")
		if got.row == nil || got.row.Aligned || got.row.DisplayBlocked != "unsigned_header:cc" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("a header OVERSIGNED while absent passes, and an added one then breaks the signature", func(t *testing.T) {
		msg := "From: Alice <alice@example.com>\r\nTo: you@example.org\r\nSubject: hi\r\n\r\nHello.\r\n"
		var out bytes.Buffer
		if err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{
			Domain: "example.com", Selector: "s1", Signer: key.Signer(), HeaderKeys: []string{"From", "To", "Subject", "Reply-To"}}); err != nil {
			t.Fatal(err)
		}
		if got := ldCheck(t, in, out.String(), "example.com"); got.row == nil || !got.row.Aligned {
			t.Fatalf("oversigned absent Reply-To should pass: %+v", got)
		}
		// CONTROL: adding the Reply-To is not a displayed pass (the signature fails).
		if got := ldCheck(t, in, "Reply-To: evil@example.net\r\n"+out.String(), "example.com"); got.row != nil && got.row.Aligned {
			t.Fatalf("an added Reply-To under an oversigned slot was displayed: %+v", got.row)
		}
	})

	t.Run("a duplicate of an OVERSIGNED header is allowed", func(t *testing.T) {
		msg := "From: Alice <alice@example.com>\r\nSubject: one\r\nSubject: two\r\nTo: you@example.org\r\nDate: Mon, 5 Oct 2026 10:00:00 +0000\r\n\r\nHello.\r\n"
		var out bytes.Buffer
		err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{
			Domain: "example.com", Selector: "s1", Signer: key.Signer(),
			HeaderKeys: []string{"From", "Subject", "Subject", "Subject", "To", "Date"}})
		if err != nil {
			t.Fatal(err)
		}
		got := ldCheck(t, in, out.String(), "example.com")
		if got.row == nil || !got.row.Aligned {
			t.Fatalf("oversigned Subject should not block: %+v", got)
		}
		// CONTROL: the same duplicate WITHOUT oversigning is blocked.
		out.Reset()
		if err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{
			Domain: "example.com", Selector: "s1", Signer: key.Signer(),
			HeaderKeys: []string{"From", "Subject", "Subject", "To", "Date"}}); err != nil {
			t.Fatal(err)
		}
		if got := ldCheck(t, in, out.String(), "example.com"); got.row == nil || got.row.Aligned {
			t.Fatalf("CONTROL FAILED: an unprotected duplicate Subject was displayed: %+v", got)
		}
	})
}
