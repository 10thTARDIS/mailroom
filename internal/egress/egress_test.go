// SPDX-License-Identifier: AGPL-3.0-only

package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeResolver answers from a script of successive replies. The script is what
// makes DNS rebinding testable: the second answer is what a hostile server would
// hand back on a re-resolution, so a guard that re-resolves gets caught by it.
type fakeResolver struct {
	mu      sync.Mutex
	answers [][]string // successive answers; the last one repeats
	calls   int
	hosts   []string
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts = append(r.hosts, host)
	i := r.calls
	r.calls++
	if len(r.answers) == 0 {
		return nil, fmt.Errorf("no such host: %s", host)
	}
	if i >= len(r.answers) {
		i = len(r.answers) - 1
	}
	out := make([]netip.Addr, 0, len(r.answers[i]))
	for _, s := range r.answers[i] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func (r *fakeResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// testClient builds a Client whose DNS is scripted and whose dial is recorded.
// srv, when non-nil, is the real server every dial is redirected to — so a test
// server on 127.0.0.1 can be reached AFTER the guard has already made its
// decision about the (public, fake) address it was given. srv == nil means no
// dial may succeed, which is what the refusal tests want.
func testClient(p Policy, answers [][]string, srv *httptest.Server) (*Client, *fakeResolver, func() []string) {
	c := New(p)
	res := &fakeResolver{answers: answers}
	c.resolver = res

	connectTo := ""
	if srv != nil {
		connectTo = mustHost(srv.URL)
		if srv.TLS != nil {
			// Trust the test server's own CA, and pin the handshake's ServerName
			// to a name its certificate actually carries: the URLs under test are
			// deliberately arbitrary hostnames, and the point of these tests is
			// the guard, not certificate verification.
			tr := c.http.Transport.(*http.Transport)
			tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			tr.TLSClientConfig.ServerName = "example.com"
		}
	}

	var mu sync.Mutex
	var dialled []string
	inner := &net.Dialer{}
	c.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, addr)
		mu.Unlock()
		if connectTo == "" {
			return nil, fmt.Errorf("test: refusing to actually dial %s", addr)
		}
		return inner.DialContext(ctx, network, connectTo)
	}
	return c, res, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), dialled...)
	}
}

func get(t *testing.T, c *Client, rawurl string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return c.Do(req)
}

// --- scheme -----------------------------------------------------------------

func TestDoRefusesSchemeNotPermitted(t *testing.T) {
	c, _, dialled := testClient(Policy{}, [][]string{{"93.184.216.34"}}, nil)
	for _, raw := range []string{
		"http://example.test/i.png", // the default policy is https only
		"file:///etc/passwd",
		"gopher://example.test/",
		"ftp://example.test/",
	} {
		_, err := get(t, c, raw)
		if !errors.Is(err, ErrSchemeNotPermitted) {
			t.Errorf("Do(%s) error = %v, want ErrSchemeNotPermitted", raw, err)
		}
	}
	// A refused scheme must not have touched the network at all.
	if d := dialled(); len(d) != 0 {
		t.Errorf("a refused scheme still dialled: %v", d)
	}
}

// TestDoPermitsHTTPWhenThePolicySaysSo is decision A: the proxy permits http,
// and that widening must be reachable — otherwise "schemes come from the caller"
// is a comment rather than a behaviour.
func TestDoPermitsHTTPWhenThePolicySaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c, _, _ := testClient(Policy{Schemes: []string{"https", "http"}},
		[][]string{{"93.184.216.34"}}, srv)
	resp, err := get(t, c, "http://example.test/i.png")
	if err != nil {
		t.Fatalf("http under a permissive policy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if b, _ := io.ReadAll(resp.Body); string(b) != "ok" {
		t.Fatalf("body = %q, want %q", b, "ok")
	}
}

// TestWebpushPolicyStaysHTTPSOnly pins the divergence ADR-026 §3a requires: the
// proxy's widening must not reach webpush. If schemes ever became a package
// constant, this goes red.
func TestWebpushPolicyStaysHTTPSOnly(t *testing.T) {
	if err := (Policy{}).CheckURLString("http://push.example/endpoint"); !errors.Is(err, ErrSchemeNotPermitted) {
		t.Fatalf("default policy accepted http: %v", err)
	}
	if err := (Policy{}).CheckURLString("https://push.example/endpoint"); err != nil {
		t.Fatalf("default policy rejected https: %v", err)
	}
}

// --- address ----------------------------------------------------------------

func TestDialRefusesResolvedPrivateAddress(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.5", "::1", "::ffff:127.0.0.1"} {
		c, _, dialled := testClient(Policy{}, [][]string{{ip}}, nil)
		_, err := get(t, c, "https://evil.test/")
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("host resolving to %s: error = %v, want ErrBlockedAddress", ip, err)
		}
		if d := dialled(); len(d) != 0 {
			t.Errorf("host resolving to %s was dialled anyway: %v", ip, d)
		}
	}
}

// TestDialRefusesIfANYResolvedAddressIsPrivate is the ordering-independence
// property. A host that publishes a good address and a bad one must be refused
// outright: if only the dialled address were checked, whether the guard held
// would depend on which record the other side chose to return first.
func TestDialRefusesIfANYResolvedAddressIsPrivate(t *testing.T) {
	for _, answer := range [][]string{
		{"93.184.216.34", "127.0.0.1"}, // bad one second
		{"127.0.0.1", "93.184.216.34"}, // bad one first
	} {
		c, _, dialled := testClient(Policy{}, [][]string{answer}, nil)
		_, err := get(t, c, "https://split.test/")
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("answer %v: error = %v, want ErrBlockedAddress", answer, err)
		}
		if d := dialled(); len(d) != 0 {
			t.Errorf("answer %v: dialled anyway: %v", answer, d)
		}
	}
}

// TestDialConnectsToTheAddressItValidated is the DNS-rebinding proof, and it is
// the reason validation lives in the dialer.
//
// The resolver hands back a public address the first time and 127.0.0.1 every
// time after. An implementation that validated the hostname and then passed the
// HOSTNAME to the transport would re-resolve and connect to loopback. The
// assertion is therefore on what was dialled: an IP literal, and specifically
// the one that was checked.
func TestDialConnectsToTheAddressItValidated(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c, res, dialled := testClient(Policy{},
		[][]string{{"93.184.216.34"}, {"127.0.0.1"}}, srv)
	resp, err := get(t, c, "https://rebind.test/i.png")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	d := dialled()
	if len(d) != 1 {
		t.Fatalf("dialled %v, want exactly one dial", d)
	}
	if d[0] != "93.184.216.34:443" {
		t.Fatalf("dialled %q, want the validated IP literal 93.184.216.34:443 — "+
			"a hostname here means the transport re-resolved after the check", d[0])
	}
	if n := res.callCount(); n != 1 {
		t.Fatalf("resolver called %d times, want 1: a second lookup between the "+
			"check and the connection is the rebinding window", n)
	}
}

// --- redirects --------------------------------------------------------------

func TestRedirectToForbiddenSchemeIsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://downgrade.test/i.png", http.StatusFound)
	}))
	defer srv.Close()

	c, _, _ := testClient(Policy{}, [][]string{{"93.184.216.34"}}, srv)
	_, err := get(t, c, "https://start.test/")
	if !errors.Is(err, ErrSchemeNotPermitted) {
		t.Fatalf("redirect to http under an https-only policy: error = %v, want ErrSchemeNotPermitted", err)
	}
}

func TestRedirectCapIsEnforced(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always redirect, forever.
		http.Redirect(w, r, "https://loop.test/next", http.StatusFound)
	}))
	defer srv.Close()

	c, _, _ := testClient(Policy{MaxRedirects: 3}, [][]string{{"93.184.216.34"}}, srv)
	_, err := get(t, c, "https://loop.test/")
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("endless redirect: error = %v, want ErrTooManyRedirects", err)
	}
}

// --- bounds -----------------------------------------------------------------

func TestBodyCapIsAFailureNotATruncation(t *testing.T) {
	const cap = 64
	body := strings.Repeat("A", cap+1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, _, _ := testClient(Policy{MaxBytes: cap}, [][]string{{"93.184.216.34"}}, srv)
	resp, err := get(t, c, "https://big.test/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("reading an over-cap body: error = %v, want ErrTooLarge — a "+
			"silent truncation here is a refusal reported as a success", err)
	}
}

func TestBodyExactlyAtTheCapSucceeds(t *testing.T) {
	const cap = 64
	body := strings.Repeat("A", cap)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, _, _ := testClient(Policy{MaxBytes: cap}, [][]string{{"93.184.216.34"}}, srv)
	resp, err := get(t, c, "https://exact.test/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("a body of exactly MaxBytes must be readable, got %v", err)
	}
	if len(got) != cap {
		t.Fatalf("read %d bytes, want %d", len(got), cap)
	}
}

// --- what leaves the machine -------------------------------------------------

func TestSendsNeutralUserAgentAndNoReferer(t *testing.T) {
	var gotUA, gotRef, gotCookie string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotRef = r.Header.Get("Referer")
		gotCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c, _, _ := testClient(Policy{}, [][]string{{"93.184.216.34"}}, srv)
	req, _ := http.NewRequest(http.MethodGet, "https://ua.test/", nil)
	// Whatever the caller was carrying, the reader's Referer does not leave.
	req.Header.Set("Referer", "https://mail.example/thread/42")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if gotUA != defaultUserAgent {
		t.Errorf("User-Agent = %q, want the fixed neutral %q", gotUA, defaultUserAgent)
	}
	if gotRef != "" {
		t.Errorf("Referer reached the far end: %q", gotRef)
	}
	if gotCookie != "" {
		t.Errorf("Cookie reached the far end: %q", gotCookie)
	}
}

// TestTransportUsesNoProxy pins the field, because a proxy would route the
// connection to the proxy's address and the target's address would never be
// resolved or checked here — the guard would pass every test and stop guarding.
func TestTransportUsesNoProxy(t *testing.T) {
	c := New(Policy{})
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.http.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("Transport.Proxy is set: an HTTP proxy bypasses the address guard entirely")
	}
	if c.http.Jar != nil {
		t.Fatal("http.Client.Jar is set: nothing of the reader's may be carried outward")
	}
}

// --- CheckURL (the registration-time half) -----------------------------------

func TestPolicyCheckURLString(t *testing.T) {
	p := Policy{}
	refused := []string{
		"http://push.example/x",           // scheme
		"https://127.0.0.1/x",             // literal loopback
		"https://[::1]/x",                 // literal loopback, v6
		"https://169.254.169.254/latest/", // literal metadata service
		"https://[::ffff:127.0.0.1]/x",    // literal, mapped
		"https://10.0.0.1/x",              // literal, RFC 1918
		"https:///nohost",                 // no host
		"not a url at all",                // unparseable as absolute
		"://missing-scheme",               // unparseable
	}
	for _, raw := range refused {
		if err := p.CheckURLString(raw); err == nil {
			t.Errorf("CheckURLString(%q) = nil, want a refusal", raw)
		}
	}
	allowed := []string{
		"https://push.example/endpoint",
		"https://fcm.googleapis.com/fcm/send/abc123",
		"https://93.184.216.34/x", // a public literal is fine
	}
	for _, raw := range allowed {
		if err := p.CheckURLString(raw); err != nil {
			t.Errorf("CheckURLString(%q) = %v, want nil", raw, err)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func mustHost(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		panic(fmt.Sprintf("test: parse %q: %v", rawurl, err))
	}
	return u.Host
}

// TestRequestCheckIsAppliedToEveryRedirectHop: a per-request check that only
// saw the first URL would be bypassed by one redirect (#922: the prefetch's
// default-port rule). The control is the same redirect without the check,
// which is followed.
func TestRequestCheckIsAppliedToEveryRedirectHop(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "https://other.test:8443/img.png", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("png"))
	}))
	defer srv.Close()
	errOddPort := errors.New("odd port")
	check := func(u *url.URL) error {
		if u.Port() != "" && u.Port() != "443" {
			return errOddPort
		}
		return nil
	}

	c, _, _ := testClient(Policy{}, [][]string{{"93.184.216.34"}}, srv)
	req, err := http.NewRequestWithContext(WithRequestCheck(context.Background(), check),
		http.MethodGet, "https://start.test/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(req); !errors.Is(err, errOddPort) {
		t.Fatalf("redirect to :8443 under the check: error = %v, want the check's refusal", err)
	}
	// The first URL is checked too.
	req, _ = http.NewRequestWithContext(WithRequestCheck(context.Background(), check),
		http.MethodGet, "https://start.test:8443/img.png", nil)
	if _, err := c.Do(req); !errors.Is(err, errOddPort) {
		t.Fatalf("first URL on :8443 under the check: error = %v, want the check's refusal", err)
	}

	// CONTROL: without the check the same redirect is followed.
	resp, err := get(t, c, "https://start.test/start")
	if err != nil {
		t.Fatalf("control redirect: %v", err)
	}
	_ = resp.Body.Close()
}
