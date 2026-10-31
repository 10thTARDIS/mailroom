// SPDX-License-Identifier: AGPL-3.0-only

package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"mailroom/internal/egress/egresstest"
)

// The account dialer's tests (#625 §3). ⚠ Every refusal case asserts on two
// observers, not on the error: the dial seam (was a dial attempted at all?) and
// a real listener on 127.0.0.1 (did a connection arrive?). The seam passes
// every address it is given straight to a real net.Dialer, so for the loopback
// forms a guard that failed to refuse would demonstrably CONNECT, and the
// listener would count it.

// spyDial records every address handed beneath the guard and really dials it.
type spyDial struct {
	mu     gosync.Mutex
	addrs  []string
	target string // when set, connect here instead (a public literal cannot be dialled in a test)
}

func (s *spyDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	s.mu.Lock()
	s.addrs = append(s.addrs, addr)
	target := s.target
	s.mu.Unlock()
	if target == "" {
		target = addr
	}
	// Never leave the machine, even when a mutated guard hands down a public
	// literal: the attempt is already recorded above, which is the observation.
	if h, _, err := net.SplitHostPort(target); err != nil || !net.ParseIP(h).IsLoopback() {
		return nil, fmt.Errorf("test: not dialling non-loopback %s", target)
	}
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

func (s *spyDial) dialled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.addrs...)
}

func TestDialerRefusesNonPublicAddressesAndNeverConnects(t *testing.T) {
	for _, tc := range []struct{ name, addr string }{
		{"loopback", "127.0.0.1"},
		// Loopback in an IPv6 costume. Dialled unguarded, this reaches the
		// 127.0.0.1 listener — which is what makes the listener assertion bite.
		{"IPv4-mapped loopback", "::ffff:127.0.0.1"},
		// A zoned address matches no netip.Prefix unless the zone is stripped
		// first (address.go); this is the bypass that has bitten this guard.
		{"zoned link-local", "fe80::1%eth0"},
		{"metadata service", "169.254.169.254"},
		{"IPv6 loopback", "::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := egresstest.Listen(t, nil)
			spy := &spyDial{}
			d := &Dialer{Resolver: egresstest.NewResolver(t, []string{tc.addr}), dial: spy.dial}

			conn, err := d.DialContext(context.Background(), "tcp",
				net.JoinHostPort("mail.example.com", strconv.Itoa(ln.Port())))
			if conn != nil {
				_ = conn.Close()
			}
			if got := spy.dialled(); len(got) != 0 {
				t.Errorf("resolving to %s: a dial was attempted beneath the guard: %v", tc.addr, got)
			}
			if n := ln.Connections(t); n != 0 {
				t.Errorf("resolving to %s: %d connection(s) reached the listener", tc.addr, n)
			}
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("resolving to %s: err = %v, want ErrBlockedAddress", tc.addr, err)
			}
			if !strings.Contains(err.Error(), "mail.example.com") {
				t.Errorf("refusal does not name the host: %v", err)
			}
		})
	}
}

// TestDialerAllowPrivateConnects is the other half: with the switch on, the
// same answers connect — to the listener, through the resolved literal.
func TestDialerAllowPrivateConnects(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::ffff:127.0.0.1"} {
		t.Run(addr, func(t *testing.T) {
			ln := egresstest.Listen(t, nil)
			spy := &spyDial{}
			d := &Dialer{AllowPrivate: true, Resolver: egresstest.NewResolver(t, []string{addr}), dial: spy.dial}
			conn, err := d.DialContext(context.Background(), "tcp",
				net.JoinHostPort("mail.example.com", strconv.Itoa(ln.Port())))
			if err != nil {
				t.Fatalf("AllowPrivate did not permit %s: %v", addr, err)
			}
			_ = conn.Close()
			want := net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Port()))
			if got := spy.dialled(); len(got) != 1 || got[0] != want {
				t.Errorf("dialled %v, want exactly [%s] (the resolved literal, unmapped)", got, want)
			}
			if n := ln.Connections(t); n != 1 {
				t.Errorf("listener saw %d connection(s), want 1", n)
			}
		})
	}
}

// TestDialerRebinding is the whole point of #625 §3. The first answer is what
// account validation saw (public: CheckHost passes); every answer after it is
// loopback. The later dial must be refused, and nothing may connect.
func TestDialerRebinding(t *testing.T) {
	ln := egresstest.Listen(t, nil)
	r := egresstest.NewResolver(t, []string{"93.184.216.34"}, []string{"127.0.0.1"})

	if err := CheckHost(context.Background(), r, "mail.example.com"); err != nil {
		t.Fatalf("validation-time check refused the public answer: %v", err)
	}

	spy := &spyDial{}
	d := &Dialer{Resolver: r, dial: spy.dial}
	conn, err := d.DialContext(context.Background(), "tcp",
		net.JoinHostPort("mail.example.com", strconv.Itoa(ln.Port())))
	if conn != nil {
		_ = conn.Close()
	}
	if got := spy.dialled(); len(got) != 0 {
		t.Errorf("rebound dial reached the dialler: %v", got)
	}
	if n := ln.Connections(t); n != 0 {
		t.Errorf("rebound dial: %d connection(s) reached the loopback listener", n)
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("rebound dial: err = %v, want ErrBlockedAddress", err)
	}
}

// TestDialerDialsTheLiteralItChecked: one resolution, and the dial target is the
// checked IP literal — never the hostname, which would resolve a second time.
func TestDialerDialsTheLiteralItChecked(t *testing.T) {
	ln := egresstest.Listen(t, nil)
	r := egresstest.NewResolver(t, []string{"93.184.216.34"}, []string{"127.0.0.1"})
	spy := &spyDial{target: net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Port()))}
	d := &Dialer{Resolver: r, dial: spy.dial}

	conn, err := d.DialContext(context.Background(), "tcp", "mail.example.com:993")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if got := spy.dialled(); len(got) != 1 || got[0] != "93.184.216.34:993" {
		t.Fatalf("dialled %v, want exactly [93.184.216.34:993]", got)
	}
	if n := r.Calls(); n != 1 {
		t.Fatalf("resolver called %d times, want 1 — a second lookup is the rebinding window", n)
	}
}

// TestDialerTriesEachCheckedAddress: a host with two public addresses whose
// first is unreachable still connects through the second, and both attempts
// are checked literals.
func TestDialerTriesEachCheckedAddress(t *testing.T) {
	ln := egresstest.Listen(t, nil)
	r := egresstest.NewResolver(t, []string{"93.184.216.34", "93.184.216.35"})
	var mu gosync.Mutex
	var dialled []string
	d := &Dialer{Resolver: r, Timeout: 10 * time.Second, dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, addr)
		mu.Unlock()
		if strings.HasPrefix(addr, "93.184.216.34:") {
			return nil, fmt.Errorf("test: %s unreachable", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Port())))
	}}
	conn, err := d.DialContext(context.Background(), "tcp", "mail.example.com:993")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if want := []string{"93.184.216.34:993", "93.184.216.35:993"}; fmt.Sprint(dialled) != fmt.Sprint(want) {
		t.Fatalf("dialled %v, want %v", dialled, want)
	}
}

// TestDialerRefusesIfAnyAddressIsNonPublic: a public answer does not launder a
// loopback one published alongside it, in either order.
func TestDialerRefusesIfAnyAddressIsNonPublic(t *testing.T) {
	for _, answer := range [][]string{{"93.184.216.34", "127.0.0.1"}, {"127.0.0.1", "93.184.216.34"}} {
		ln := egresstest.Listen(t, nil)
		spy := &spyDial{}
		d := &Dialer{Resolver: egresstest.NewResolver(t, answer), dial: spy.dial}
		_, err := d.DialContext(context.Background(), "tcp",
			net.JoinHostPort("mail.example.com", strconv.Itoa(ln.Port())))
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("answer %v: err = %v, want ErrBlockedAddress", answer, err)
		}
		if got := spy.dialled(); len(got) != 0 {
			t.Errorf("answer %v: dialled %v", answer, got)
		}
		if n := ln.Connections(t); n != 0 {
			t.Errorf("answer %v: %d connection(s) reached the listener", answer, n)
		}
	}
}

// TestDialerResolutionFailureIsNotAPolicyRefusal: "the host does not resolve"
// must not read as "the host is forbidden" — the first is transient for the
// sync loop, the second is a configuration refusal.
func TestDialerResolutionFailureIsNotAPolicyRefusal(t *testing.T) {
	d := &Dialer{Resolver: egresstest.NewResolver(t)} // answers nothing
	_, err := d.DialContext(context.Background(), "tcp", "mail.example.com:993")
	if err == nil {
		t.Fatal("an unresolvable host dialled successfully")
	}
	if errors.Is(err, ErrBlockedAddress) {
		t.Errorf("a resolution failure was reported as a policy refusal: %v", err)
	}
}
