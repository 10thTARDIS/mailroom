// SPDX-License-Identifier: AGPL-3.0-only

package dkimverify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

const testBody = "Hello,\r\nthis is the body.\r\n"

func testMsg(from string) string {
	return "From: Alice <alice@" + from + ">\r\nTo: you@example.org\r\nSubject: hi\r\nDate: Mon, 5 Oct 2026 10:00:00 +0000\r\n\r\n" + testBody
}

type testKey struct {
	pub ed25519.PublicKey
	prv ed25519.PrivateKey
}

func newKey(t *testing.T) testKey {
	t.Helper()
	pub, prv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{pub, prv}
}

func (k testKey) record() string {
	return "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(k.pub)
}

func signWith(t *testing.T, msg, domain, selector string, opts dkim.SignOptions) string {
	t.Helper()
	opts.Domain, opts.Selector = domain, selector
	var out bytes.Buffer
	if err := dkim.Sign(&out, strings.NewReader(msg), &opts); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func signEd(t *testing.T, k testKey, msg, domain, selector string) string {
	return signWith(t, msg, domain, selector, dkim.SignOptions{Signer: k.prv})
}

// fakeDNS serves TXT records by name and counts lookups.
type fakeDNS struct {
	records map[string][]string
	err     error
	n       atomic.Int32
}

func (f *fakeDNS) lookup(_ context.Context, name string) ([]string, error) {
	f.n.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.records[name], nil
}

func one(t *testing.T, rs []Result) Result {
	t.Helper()
	if len(rs) != 1 {
		t.Fatalf("want 1 result, got %+v", rs)
	}
	return rs[0]
}

func TestVerifyStatuses(t *testing.T) {
	k := newKey(t)
	other := newKey(t)
	signed := signEd(t, k, testMsg("example.com"), "example.com", "sel1")
	name := "sel1._domainkey.example.com"
	good := map[string][]string{name: {k.record()}}

	cases := []struct {
		name   string
		raw    string
		dns    *fakeDNS
		want   Status
		detail string
	}{
		{"pass", signed, &fakeDNS{records: good}, StatusPass, ""},
		{"body changed", strings.Replace(signed, "the body", "the BODY", 1), &fakeDNS{records: good}, StatusFailBody, "body hash mismatch"},
		// Subject is signed; the body hash still matches, so only the signature fails.
		{"header changed", strings.Replace(signed, "Subject: hi", "Subject: ho", 1), &fakeDNS{records: good}, StatusFailSignature, "signature mismatch"},
		{"wrong key", signed, &fakeDNS{records: map[string][]string{name: {other.record()}}}, StatusFailSignature, "signature mismatch"},
		{"no key", signed, &fakeDNS{}, StatusKeyNotFound, "no key record"},
		{"revoked", signed, &fakeDNS{records: map[string][]string{name: {"v=DKIM1; k=ed25519; p="}}}, StatusKeyRevoked, "key revoked"},
		{"lookup error", signed, &fakeDNS{err: errors.New("servfail")}, StatusTempError, "dns lookup failed"},
		{"lookup timeout", signed, &fakeDNS{err: context.DeadlineExceeded}, StatusTempError, "dns lookup failed"},
		{"two key records", signed, &fakeDNS{records: map[string][]string{name: {k.record(), k.record()}}}, StatusPermError, "multiple key records"},
		{"garbage key record", signed, &fakeDNS{records: map[string][]string{name: {"v=DKIM1; k=ed25519; p=!!!"}}}, StatusPermError, "malformed signature or key"},
		{"unknown key type", signed, &fakeDNS{records: map[string][]string{name: {"v=DKIM1; k=ecdsa; p=AAAA"}}}, StatusUnsupported, "unsupported algorithm or key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := New(Options{LookupTXT: c.dns.lookup})
			r := one(t, v.Verify(context.Background(), []byte(c.raw)))
			if r.Status != c.want || r.Detail != c.detail {
				t.Fatalf("got %+v, want status %s detail %q", r, c.want, c.detail)
			}
			if r.Domain != "example.com" || r.Selector != "sel1" {
				t.Fatalf("domain/selector = %q/%q", r.Domain, r.Selector)
			}
		})
	}
}

func TestVerifyRSAAndRelaxed(t *testing.T) {
	prv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&prv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	rec := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	signed := signWith(t, testMsg("example.net"), "example.net", "rsa1", dkim.SignOptions{
		Signer: prv, HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed})
	dns := &fakeDNS{records: map[string][]string{"rsa1._domainkey.example.net": {rec}}}
	r := one(t, New(Options{LookupTXT: dns.lookup}).Verify(context.Background(), []byte(signed)))
	if r.Status != StatusPass || r.Domain != "example.net" || r.Selector != "rsa1" {
		t.Fatalf("got %+v", r)
	}
}

func TestVerifyNoSignature(t *testing.T) {
	dns := &fakeDNS{}
	rs := New(Options{LookupTXT: dns.lookup}).Verify(context.Background(), []byte(testMsg("example.com")))
	if len(rs) != 0 || dns.n.Load() != 0 {
		t.Fatalf("results %+v lookups %d; an unsigned message needs neither", rs, dns.n.Load())
	}
}

func TestVerifyTwoSignaturesKeepsEachSelector(t *testing.T) {
	k1, k2 := newKey(t), newKey(t)
	msg := signEd(t, k1, testMsg("example.com"), "example.com", "first")
	msg = signEd(t, k2, msg, "example.org", "second")
	dns := &fakeDNS{records: map[string][]string{
		"first._domainkey.example.com":  {k1.record()},
		"second._domainkey.example.org": {"v=DKIM1; k=ed25519; p="},
	}}
	rs := New(Options{LookupTXT: dns.lookup}).Verify(context.Background(), []byte(msg))
	if len(rs) != 2 {
		t.Fatalf("results = %+v", rs)
	}
	got := map[string]Result{rs[0].Selector: rs[0], rs[1].Selector: rs[1]}
	if r := got["first"]; r.Domain != "example.com" || r.Status != StatusPass {
		t.Fatalf("first = %+v", r)
	}
	if r := got["second"]; r.Domain != "example.org" || r.Status != StatusKeyRevoked {
		t.Fatalf("second = %+v", r)
	}
}

func TestLookupBudgetAndCache(t *testing.T) {
	k := newKey(t)
	dns := &fakeDNS{records: map[string][]string{}}
	for _, s := range []string{"a", "b", "c"} {
		dns.records[s+"._domainkey.example.com"] = []string{k.record()}
	}
	v := New(Options{LookupTXT: dns.lookup, MaxLookups: 2})
	ctx := context.Background()
	msgA := []byte(signEd(t, k, testMsg("example.com"), "example.com", "a"))
	msgB := []byte(signEd(t, k, testMsg("example.com"), "example.com", "b"))
	msgC := []byte(signEd(t, k, testMsg("example.com"), "example.com", "c"))

	if r := one(t, v.Verify(ctx, msgA)); r.Status != StatusPass {
		t.Fatalf("a: %+v", r)
	}
	if r := one(t, v.Verify(ctx, msgA)); r.Status != StatusPass { // cached: free
		t.Fatalf("a again: %+v", r)
	}
	if r := one(t, v.Verify(ctx, msgB)); r.Status != StatusPass {
		t.Fatalf("b: %+v", r)
	}
	r := one(t, v.Verify(ctx, msgC)) // third distinct name: budget (2) is spent
	if r.Status != StatusTempError || r.Detail != "lookup budget spent" {
		t.Fatalf("c: %+v", r)
	}
	one(t, v.Verify(ctx, msgC))
	st := v.Stats()
	if st.Lookups != 2 || st.CacheHits != 1 || st.BudgetRefused != 2 || st.Budget != 2 || dns.n.Load() != 2 {
		t.Fatalf("stats = %+v, dns lookups %d", st, dns.n.Load())
	}
}

func TestNegativeAndFailureAnswersAreCached(t *testing.T) {
	k := newKey(t)
	msg := []byte(signEd(t, k, testMsg("example.com"), "example.com", "a"))
	dns := &fakeDNS{}
	v := New(Options{LookupTXT: dns.lookup})
	for i := 0; i < 3; i++ {
		if r := one(t, v.Verify(context.Background(), msg)); r.Status != StatusKeyNotFound {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
	if dns.n.Load() != 1 {
		t.Fatalf("a not-found answer must be cached: %d lookups", dns.n.Load())
	}
}

func TestLookupTimeoutIsPassedToResolver(t *testing.T) {
	k := newKey(t)
	msg := []byte(signEd(t, k, testMsg("example.com"), "example.com", "a"))
	var deadline time.Time
	v := New(Options{LookupTimeout: 3 * time.Second, LookupTXT: func(ctx context.Context, _ string) ([]string, error) {
		deadline, _ = ctx.Deadline()
		return nil, nil
	}})
	start := time.Now()
	v.Verify(context.Background(), msg)
	if d := deadline.Sub(start); d <= 0 || d > 4*time.Second {
		t.Fatalf("lookup deadline %v from start, want about 3s", d)
	}
}

func TestTooLarge(t *testing.T) {
	v := New(Options{MaxMessageBytes: 100, LookupTXT: (&fakeDNS{}).lookup})
	r := one(t, v.Verify(context.Background(), []byte(testMsg("example.com")+strings.Repeat("x", 200))))
	if r.Status != StatusUnsupported || r.Detail != "too large" {
		t.Fatalf("got %+v", r)
	}
}

func TestUnreadableMessageIsNotAPass(t *testing.T) {
	r := one(t, New(Options{LookupTXT: (&fakeDNS{}).lookup}).Verify(context.Background(), []byte("not a message at all")))
	if r.Status != StatusPermError {
		t.Fatalf("got %+v", r)
	}
}

func TestDefaultsAndSignatureCap(t *testing.T) {
	v := New(Options{})
	if st := v.Stats(); st.Budget != DefaultMaxLookups || DefaultMaxLookups != 5000 || DefaultLookupTimeout != 5*time.Second || DefaultMaxMessageBytes != 25<<20 {
		t.Fatalf("defaults = %+v", st)
	}
	k := newKey(t)
	msg := testMsg("example.com")
	for i := 0; i < maxSignatures+2; i++ {
		msg = signEd(t, k, msg, "example.com", "s")
	}
	rs := New(Options{LookupTXT: (&fakeDNS{}).lookup}).Verify(context.Background(), []byte(msg))
	if len(rs) != maxSignatures+1 || rs[maxSignatures].Status != StatusUnsupported {
		t.Fatalf("cap not reported: %d results, last %+v", len(rs), rs[len(rs)-1])
	}
}

// classify must keep working against the real library's wording: each library
// error it maps is produced by a real verification above. This pins the pure
// cases the table above cannot reach without a hand-built signature.
func TestClassifyNil(t *testing.T) {
	if st, _ := classify(nil); st != StatusPass {
		t.Fatalf("nil error must be a pass, got %s", st)
	}
}
