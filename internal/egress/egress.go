// SPDX-License-Identifier: AGPL-3.0-only

package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Refusal sentinels. Each one is a distinct outcome a caller must handle: a
// guard that reports a refusal as an empty success is the failure shape ADR-026
// §3a forbids by name.
var (
	// ErrSchemeNotPermitted: the URL's scheme is not in the caller's Policy.
	ErrSchemeNotPermitted = errors.New("egress: URL scheme is not permitted")
	// ErrTooManyRedirects: the redirect chain exceeded Policy.MaxRedirects.
	ErrTooManyRedirects = errors.New("egress: too many redirects")
	// ErrTooLarge: the response body exceeded Policy.MaxBytes. It surfaces from
	// a Read on the body, not from Do, because that is where the byte arrives.
	ErrTooLarge = errors.New("egress: response body is too large")
)

// Defaults applied to a zero-valued field. https-only is the default scheme set
// so a caller that says nothing gets the safe policy rather than the permissive
// one.
const (
	defaultTimeout      = 30 * time.Second
	defaultMaxBytes     = 1 << 20 // 1 MiB — ample for an API response
	defaultMaxRedirects = 5
	defaultUserAgent    = "Mailroom"
)

// Policy is what one caller is permitted to reach and how much of it. The zero
// value is a usable https-only policy.
type Policy struct {
	// Schemes permitted, lower-case. Empty means https only.
	Schemes []string
	// Timeout bounds one whole request including redirects. Zero means 30s.
	Timeout time.Duration
	// MaxBytes bounds the response body. Zero means 1 MiB. Negative means
	// unbounded, which no current caller wants and which must be justified.
	MaxBytes int64
	// MaxRedirects is the hop limit. Zero means 5. Negative means no redirects
	// are followed at all.
	MaxRedirects int
	// UserAgent is sent verbatim. Zero means "Mailroom". It is deliberately
	// fixed and neutral: hiding the reader's browser is the point of the proxy,
	// and forwarding a real one would be the whole feature failing quietly.
	UserAgent string
	// Exempt, when non-nil, is consulted for an address the table refuses; true
	// admits it. It is nil for every production caller. Only a build-tagged file
	// can supply one (see internal/api/proxy_e2e_private.go), so the shipped
	// binary has no way to widen the guard.
	Exempt func(netip.Addr) bool
}

// check is CheckAddress with the Policy's optional exemption applied.
func (p Policy) check(a netip.Addr) error {
	err := CheckAddress(a)
	if err != nil && p.Exempt != nil && p.Exempt(a.Unmap().WithZone("")) {
		return nil
	}
	return err
}

// Resolver is the DNS seam. *net.Resolver satisfies it; tests inject a fake so
// that rebinding and multi-address behaviour can be exercised without DNS.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Client is a guarded outbound HTTP client. Its Do makes it a drop-in for the
// minimal client interfaces callers already define (webpush.HTTPClient), so
// adopting the guard is a construction change rather than a rewrite.
//
// It is safe for concurrent use.
type Client struct {
	policy   Policy
	resolver Resolver
	// dial is the seam beneath the guard: it receives an IP literal that has
	// already been validated. Tests replace it to observe exactly what was
	// dialled, which is how "connects to the address it decided about" is
	// asserted rather than assumed.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	http *http.Client
}

// withDefaults fills the zero-valued fields. It is applied by both New and
// Policy.CheckURL so a Policy validates exactly as it would fetch — in
// particular, an empty Schemes means https in both, and never "anything".
func (p Policy) withDefaults() Policy {
	if len(p.Schemes) == 0 {
		p.Schemes = []string{"https"}
	}
	if p.Timeout == 0 {
		p.Timeout = defaultTimeout
	}
	if p.MaxBytes == 0 {
		p.MaxBytes = defaultMaxBytes
	}
	if p.MaxRedirects == 0 {
		p.MaxRedirects = defaultMaxRedirects
	}
	if p.UserAgent == "" {
		p.UserAgent = defaultUserAgent
	}
	return p
}

// New builds a Client for one caller's Policy.
func New(p Policy) *Client {
	p = p.withDefaults()
	c := &Client{
		policy:   p,
		resolver: net.DefaultResolver,
		dial:     (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	c.http = &http.Client{
		Timeout: p.Timeout,
		// ⚠ Proxy is nil, NOT ProxyFromEnvironment. Behind a proxy the transport
		// would connect to the proxy and send the target as a request line, so the
		// URL's host would never be resolved here and guardedDial would validate
		// the proxy's address instead of the target's — the guard would be
		// perfectly green and completely bypassed.
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           c.guardedDial,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: c.checkRedirect,
		// No cookie jar, ever: Jar stays nil so nothing of the reader's is
		// carried outward and nothing a remote host sets is remembered.
		Jar: nil,
	}
	return c
}

// requestCheckKey carries a per-request URL check in a request's context.
type requestCheckKey struct{}

// WithRequestCheck returns a context under which a request made by Do is also
// refused when check returns an error — for the first URL AND EVERY REDIRECT
// HOP. It can only NARROW the Policy, never widen it: the Policy's own checks
// still run first, and check's error is returned as the refusal.
//
// It exists so one caller can apply a stricter rule to some of its requests
// without a second client (#922: the image prefetch accepts only default ports,
// while an open through the same client does not). A check applied only to the
// first URL would be bypassed by a single redirect, which is why it lives here,
// beside checkRedirect, and not at the call site.
func WithRequestCheck(ctx context.Context, check func(*url.URL) error) context.Context {
	return context.WithValue(ctx, requestCheckKey{}, check)
}

// RequestCheck runs the per-request check carried by ctx, if any. Client uses
// it on every hop; it is exported so a test double of the client can honour
// the same contract.
func RequestCheck(ctx context.Context, u *url.URL) error { return requestCheck(ctx, u) }

// requestCheck runs the per-request check carried by ctx, if any.
func requestCheck(ctx context.Context, u *url.URL) error {
	if check, ok := ctx.Value(requestCheckKey{}).(func(*url.URL) error); ok && check != nil {
		return check(u)
	}
	return nil
}

// Do performs a guarded request. The returned body is capped at Policy.MaxBytes;
// reading past that yields ErrTooLarge rather than a silently truncated success.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		return nil, fmt.Errorf("%w: request has no URL", ErrSchemeNotPermitted)
	}
	if err := c.CheckURL(req.URL); err != nil {
		return nil, err
	}
	if err := requestCheck(req.Context(), req.URL); err != nil {
		return nil, err
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.policy.UserAgent)
	}
	req.Header.Del("Referer")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if c.policy.MaxBytes >= 0 {
		resp.Body = &cappedBody{rc: resp.Body, remaining: c.policy.MaxBytes}
	}
	return resp, nil
}

// CheckURL validates a URL against the policy WITHOUT contacting it: the scheme
// must be permitted, there must be a host, and a host given as an IP literal
// must itself be permitted.
//
// It is on Policy rather than on Client so a registration path can refuse an
// endpoint at the moment it is offered — with a clear message naming what is
// wrong — without standing up a client it will never fetch with.
//
// ⚠ It is NOT a substitute for the dial-time guard, and must never be treated as
// one. A hostname's addresses can change between this call and any later
// request; that is precisely what DNS rebinding is. The address check that
// actually holds is the one in guardedDial, on the address being dialled.
func (p Policy) CheckURL(u *url.URL) error {
	p = p.withDefaults()
	if u == nil {
		return fmt.Errorf("%w: no URL", ErrSchemeNotPermitted)
	}
	if err := p.checkScheme(u); err != nil {
		return err
	}
	if u.Host == "" {
		return fmt.Errorf("%w: URL has no host", ErrBlockedAddress)
	}
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
		return p.check(addr)
	}
	return nil
}

// CheckURLString parses raw and validates it. A URL that does not parse, or that
// is not absolute, is refused here rather than at send time.
func (p Policy) CheckURLString(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: not a valid URL: %v", ErrSchemeNotPermitted, err)
	}
	return p.CheckURL(u)
}

func (p Policy) checkScheme(u *url.URL) error {
	for _, s := range p.Schemes {
		if u.Scheme == s {
			return nil
		}
	}
	return fmt.Errorf("%w: %q (permitted: %s)",
		ErrSchemeNotPermitted, u.Scheme, strings.Join(p.Schemes, ", "))
}

// CheckURL validates a URL against this client's policy. See Policy.CheckURL.
func (c *Client) CheckURL(u *url.URL) error { return c.policy.CheckURL(u) }

// checkRedirect re-validates every hop and caps the chain.
//
// The ADDRESS needs no explicit re-check here, and the reason is worth stating
// so nobody later "fixes" its absence: a redirect to a different host opens a
// new connection, which goes through guardedDial and is therefore resolved and
// validated exactly like the first hop; a redirect to the same host reuses a
// connection whose address was already validated.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if c.policy.MaxRedirects < 0 {
		return http.ErrUseLastResponse
	}
	if len(via) > c.policy.MaxRedirects {
		return fmt.Errorf("%w: %d (limit %d)", ErrTooManyRedirects, len(via), c.policy.MaxRedirects)
	}
	if err := c.CheckURL(req.URL); err != nil {
		return err
	}
	// The caller's per-request narrowing (WithRequestCheck), on every hop.
	if err := requestCheck(req.Context(), req.URL); err != nil {
		return err
	}
	// net/http adds a Referer on redirects. Nothing about where the reader came
	// from should leave this machine.
	req.Header.Del("Referer")
	return nil
}

// guardedDial is where the guarantee lives: resolve, decide, then connect to the
// address that was decided about.
func (c *Client) guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("egress: dial target %q: %w", addr, err)
	}
	addrs, err := c.resolveAndCheck(ctx, network, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, a := range addrs {
		// ⚠ An IP LITERAL, never the hostname. Handing the hostname on would let
		// the dialer resolve a second time, and the address that gets connected
		// to would be one nothing ever checked.
		conn, derr := c.dial(ctx, network, net.JoinHostPort(a.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

// resolveAndCheck returns the validated addresses for a host, in dial order.
func (c *Client) resolveAndCheck(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return checkedAddrs(ctx, c.resolver, network, host, c.policy.check)
}

// CheckHost reports whether EVERY address `host` resolves to may be connected
// to, returning an error wrapping ErrBlockedAddress if not.
//
// It exists so a path that must dial something OTHER than HTTP can adopt this
// package's rules instead of restating them. The account-validation dial
// (internal/onboarding) is the first such caller: it speaks IMAP, so it cannot
// use Client, but the question it needs answered — "may I connect to this?" —
// is exactly the one Client answers on every request.
//
// ⚠ WHAT THIS DOES AND DOES NOT CLOSE, because the difference matters and a
// caller should not have to read the implementation to learn it:
//
//   - It DOES close the SSRF / port-scan oracle. The address is refused before
//     any connection is attempted, so no observable difference between an open
//     and a closed port reaches the caller.
//   - It does NOT close DNS rebinding on its own. Client and Dialer dial the
//     literal address they validated; a caller that takes this answer and then
//     dials the HOSTNAME re-resolves, and the second resolution can differ. A
//     caller that goes on to CONNECT must do so through Dialer (or Client),
//     which is what every account connection now does (#625 §3) — this answer
//     is only ever a pre-check.
func CheckHost(ctx context.Context, r Resolver, host string) error {
	_, err := checkedAddrs(ctx, r, "tcp", host, CheckAddress)
	return err
}

// checkedAddrs is the single implementation of "resolve, then check every
// address", shared by Client and CheckHost. Two copies of this would be two
// policies that drift.
func checkedAddrs(ctx context.Context, r Resolver, network, host string, check func(netip.Addr) error) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if cerr := check(a); cerr != nil {
			return nil, cerr
		}
		return []netip.Addr{a.Unmap().WithZone("")}, nil
	}
	found, err := r.LookupNetIP(ctx, ipNetwork(network), host)
	if err != nil {
		return nil, fmt.Errorf("egress: resolve %q: %w", host, err)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("egress: resolve %q: no addresses", host)
	}
	out := make([]netip.Addr, 0, len(found))
	for _, a := range found {
		// ⚠ EVERY address must pass, not just the one that would be dialled
		// first. A host publishing a public address alongside 127.0.0.1 is
		// refused outright — otherwise whether the guard holds would depend on
		// resolver ordering, which the other side chooses.
		if cerr := check(a); cerr != nil {
			return nil, fmt.Errorf("egress: %q resolves to a refused address: %w", host, cerr)
		}
		// Store the canonical form that was actually checked, so what gets
		// dialled and what got validated cannot drift apart.
		out = append(out, a.Unmap().WithZone(""))
	}
	return out, nil
}

// ipNetwork maps a dial network to the network LookupNetIP expects.
func ipNetwork(network string) string {
	switch network {
	case "tcp4", "udp4", "ip4":
		return "ip4"
	case "tcp6", "udp6", "ip6":
		return "ip6"
	default:
		return "ip"
	}
}

// cappedBody bounds a response body. Reading past the cap returns ErrTooLarge —
// a distinct failure — rather than an EOF that a caller would read as a short
// but complete body.
type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		// The cap is reached. Probe one byte to tell "exactly at the cap, and
		// done" from "over the cap": without this, a body of exactly MaxBytes
		// and a body of MaxBytes+1 would both end in a clean EOF.
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, ErrTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }
