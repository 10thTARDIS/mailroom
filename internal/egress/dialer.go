// SPDX-License-Identifier: AGPL-3.0-only

package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Dialer is the guarded dialer for outbound connections that are not HTTP: the
// mail account's IMAP (sync, writeback, IDLE, validation) and SMTP (submission)
// connections (#625 §3).
//
// It makes the same guarantee Client.guardedDial makes, from the same code:
// RESOLVE, DECIDE, THEN CONNECT TO THE ADDRESS THAT WAS DECIDED ABOUT. The host
// is resolved once through checkedAddrs — the one implementation of "every
// resolved address must pass CheckAddress" — and what is dialled is the checked
// IP LITERAL, never the hostname, so nothing can resolve a second time between
// the decision and the connection. That second resolution is DNS rebinding: an
// account host that resolved public when the account was validated can resolve
// to 127.0.0.1 or 169.254.169.254 on the next sync pass, and a dialer that
// handed the hostname on would connect there with the account's credentials.
//
// ⚠ TLS IS NOT THIS TYPE'S JOB, and the split is load-bearing. The caller wraps
// the returned conn in TLS with ServerName set to the account's HOSTNAME, so the
// certificate is still verified against the name the operator typed — dialling
// an IP literal changes where the bytes go, not whose certificate is accepted.
//
// The zero value is the strict policy with the system resolver, so a caller
// that forgets to configure it fails closed.
type Dialer struct {
	// AllowPrivate turns the address policy OFF: every resolved address may be
	// dialled. It is MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS (#527), for a mail
	// server that genuinely lives on the same network as the instance. It does
	// not change HOW the dial happens — the host is still resolved exactly once
	// and the resolved literal is what gets dialled — so the permissive and the
	// strict paths differ in one decision only.
	AllowPrivate bool

	// Resolver is the DNS seam; nil means net.DefaultResolver. Tests inject a
	// scripted resolver to make rebinding observable without DNS.
	Resolver Resolver

	// Timeout bounds the whole connect phase — resolution and every address
	// tried. Zero means only ctx bounds it.
	Timeout time.Duration

	// dial is the seam beneath the guard: it receives an IP literal that has
	// already been decided about. nil means a plain net.Dialer. Tests in this
	// package replace it to observe exactly what was dialled.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// minAttemptTimeout is the floor on one address's share of the connect budget,
// matching net.Dialer's own: a host with many addresses must not divide the
// budget into slices too thin for any of them to complete a handshake.
const minAttemptTimeout = 2 * time.Second

// DialContext connects to address ("host:port"). A host that resolves to any
// refused address is refused BEFORE any connection is attempted, with an error
// wrapping ErrBlockedAddress that names the host and the refused address.
//
// A resolution failure or a connection failure is returned as the underlying
// error (a *net.DNSError or *net.OpError in the chain), so a caller that
// classifies network errors as transient keeps doing so; only the policy
// refusal is ErrBlockedAddress.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("egress: dial target %q: %w", address, err)
	}
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	addrs, err := d.resolve(ctx, network, host)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return nil, fmt.Errorf("egress: refusing to connect to %s: %w", address, err)
		}
		return nil, err
	}

	dial := d.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var lastErr error
	for i, a := range addrs {
		actx, cancel := attemptContext(ctx, len(addrs)-i)
		// ⚠ AN IP LITERAL, NEVER THE HOSTNAME — see the type's doc.
		conn, derr := dial(actx, network, net.JoinHostPort(a.String(), port))
		cancel() // harmless on success: an established conn ignores its dial ctx
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// resolve returns the addresses to dial, in order. With the policy on it IS
// checkedAddrs; with it off it performs the same single resolution and skips
// only the decision.
func (d *Dialer) resolve(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r := d.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	if !d.AllowPrivate {
		return checkedAddrs(ctx, r, network, host, CheckAddress)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		// Unmapped so ::ffff:a.b.c.d dials as the IPv4 address it is. The zone is
		// KEPT here, unlike on the strict path: a permitted link-local address is
		// undiallable without it, and the strict path refuses zoned (link-local)
		// addresses before this question arises.
		return []netip.Addr{a.Unmap()}, nil
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
		out = append(out, a.Unmap())
	}
	return out, nil
}

// attemptContext gives one address its share of what remains of ctx's
// deadline, as net.Dialer does across a host's addresses: without the split, an
// unreachable first address (a blackholed IPv6 route) would spend the whole
// budget and the reachable second one would never be tried.
func attemptContext(ctx context.Context, remainingAddrs int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || remainingAddrs <= 1 {
		return context.WithCancel(ctx)
	}
	share := time.Until(deadline) / time.Duration(remainingAddrs)
	if share < minAttemptTimeout {
		share = minAttemptTimeout
	}
	return context.WithTimeout(ctx, share) // never extends past ctx's own deadline
}
