// SPDX-License-Identifier: AGPL-3.0-only

package egress

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// TestCheckAddressRefusesNonPublic walks every range the table names, plus the
// three encodings that are the usual way this check is bypassed. Each case
// carries the reason it exists so a future edit that deletes one has to argue
// with the comment.
func TestCheckAddressRefusesNonPublic(t *testing.T) {
	cases := []struct {
		addr string
		why  string
	}{
		// IPv4.
		{"0.0.0.0", "unspecified"},
		{"0.1.2.3", "0.0.0.0/8 is \"this network\", not a destination"},
		{"10.1.2.3", "RFC 1918"},
		{"100.64.0.1", "CGNAT — NOT covered by net/netip's IsPrivate, so a table miss here is silent"},
		{"127.0.0.1", "loopback"},
		{"127.1.2.3", "the whole of 127/8 is loopback, not just .0.1"},
		{"169.254.169.254", "⚠ the cloud instance-metadata service: the highest-value SSRF target there is"},
		{"169.254.0.1", "the rest of link-local"},
		{"172.16.0.1", "RFC 1918 lower bound"},
		{"172.31.255.255", "RFC 1918 upper bound — /12, not /16"},
		{"192.0.0.1", "IETF protocol assignments"},
		{"192.0.2.1", "TEST-NET-1"},
		{"192.168.1.1", "RFC 1918"},
		{"198.18.0.1", "benchmarking"},
		{"198.51.100.1", "TEST-NET-2"},
		{"203.0.113.1", "TEST-NET-3"},
		{"224.0.0.1", "multicast"},
		{"239.255.255.255", "multicast upper bound"},
		{"240.0.0.1", "reserved"},
		{"255.255.255.255", "broadcast"},

		// IPv6.
		{"::", "unspecified"},
		{"::1", "loopback"},
		{"64:ff9b::1.2.3.4", "NAT64 embeds IPv4"},
		{"64:ff9b:1::1", "local-use NAT64"},
		{"100::1", "discard-only"},
		{"2001:db8::1", "documentation"},
		{"2002::1", "6to4 embeds IPv4"},
		{"fc00::1", "unique local, lower"},
		{"fdff::1", "unique local, upper — /7, not /8"},
		{"fe80::1", "link-local"},
		{"ff02::1", "multicast"},

		// ⚠ The encodings. These are the bypasses, and each one passed every
		// other entry in the table until the line that handles it was written.
		{"::ffff:127.0.0.1", "IPv4-MAPPED loopback: matches no IPv6 entry, so without Unmap it sails through"},
		{"::ffff:169.254.169.254", "IPv4-mapped metadata service"},
		{"::ffff:10.0.0.1", "IPv4-mapped RFC 1918"},
		{"::7f00:1", "IPv4-COMPATIBLE loopback (deprecated ::a.b.c.d): Unmap does NOT touch this one"},
		{"::a9fe:a9fe", "IPv4-compatible metadata service"},
		{"fe80::1%eth0", "⚠ ZONED: netip.Prefix.Contains is false for ANY zoned address, so this matched nothing at all"},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.addr, err)
			}
			err = CheckAddress(addr)
			if err == nil {
				t.Fatalf("CheckAddress(%s) = nil, want refusal (%s)", tc.addr, tc.why)
			}
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("CheckAddress(%s) error %v does not wrap ErrBlockedAddress", tc.addr, err)
			}
			// The reason has to reach the message: "blocked" alone is not a
			// diagnosis an operator can act on.
			if !strings.Contains(err.Error(), "egress:") || len(err.Error()) < 30 {
				t.Fatalf("CheckAddress(%s) error is not descriptive: %q", tc.addr, err)
			}
		})
	}
}

// TestCheckAddressPermitsPublic is the other half: a guard that refuses
// everything is not a guard, it is an outage, and a refusal test alone cannot
// tell the two apart.
func TestCheckAddressPermitsPublic(t *testing.T) {
	for _, s := range []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",
		"172.32.0.1",  // just above the RFC 1918 /12
		"172.15.0.1",  // just below it
		"100.63.0.1",  // just below CGNAT
		"100.128.0.1", // just above CGNAT
		"192.0.1.1",   // between the IETF /24 and TEST-NET-1
		"223.255.255.255",
		"2606:4700:4700::1111",
		"2001:db9::1", // just outside the documentation /32
		"fbff::1",     // just below unique-local fc00::/7
		"2003::1",     // just above 6to4
	} {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if err := CheckAddress(addr); err != nil {
			t.Errorf("CheckAddress(%s) = %v, want nil (this is a public address)", s, err)
		}
	}
}

// TestTableAgreesWithStdlibPredicates is the safety net for blockedPrefixes
// being the ONLY run-time mechanism (see its comment). Rather than running a
// second overlapping check in production — which would make every other test in
// this file ambiguous about which mechanism refused — the stdlib's knowledge is
// applied here, where a disagreement goes red instead of being papered over.
//
// It sweeps a wide address space and asserts one direction: anything net/netip
// considers non-routable MUST be in the table. The converse is deliberately not
// asserted, because the table refuses strictly more than the stdlib does
// (CGNAT, NAT64, 6to4, the documentation ranges).
func TestTableAgreesWithStdlibPredicates(t *testing.T) {
	nonRoutable := func(a netip.Addr) bool {
		return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
			a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
			a.IsMulticast() || a.IsUnspecified()
	}
	checked := 0
	// Every /8 boundary, and a spread inside each, for IPv4.
	for b0 := 0; b0 < 256; b0++ {
		for _, rest := range [][3]int{{0, 0, 1}, {1, 2, 3}, {127, 127, 127}, {255, 255, 254}} {
			a := netip.AddrFrom4([4]byte{byte(b0), byte(rest[0]), byte(rest[1]), byte(rest[2])})
			checked++
			if nonRoutable(a) && CheckAddress(a) == nil {
				t.Errorf("%s: net/netip calls this non-routable but the table permits it", a)
			}
		}
	}
	// A spread of IPv6 prefixes the stdlib has opinions about.
	for _, s := range []string{
		"::", "::1", "fe80::1", "fec0::1", "fc00::1", "fdff::ffff",
		"ff00::1", "ff01::1", "ff02::1", "ff05::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.1.1", "::ffff:224.0.0.1",
	} {
		a := netip.MustParseAddr(s)
		checked++
		if nonRoutable(a) && CheckAddress(a) == nil {
			t.Errorf("%s: net/netip calls this non-routable but the table permits it", a)
		}
	}
	if checked < 1000 {
		t.Fatalf("swept only %d addresses; this test is meant to be broad", checked)
	}
	t.Logf("cross-checked %d addresses against the net/netip predicates", checked)
}

// TestSixToFourRelayAnycastRefused: 192.88.99.0/24 (RFC 7526) is refused, and
// the addresses just outside it are not (#676).
func TestSixToFourRelayAnycastRefused(t *testing.T) {
	for _, s := range []string{"192.88.99.0", "192.88.99.1", "192.88.99.255"} {
		if err := CheckAddress(netip.MustParseAddr(s)); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("CheckAddress(%s) = %v, want ErrBlockedAddress", s, err)
		}
	}
	for _, s := range []string{"192.88.98.255", "192.88.100.0"} {
		if err := CheckAddress(netip.MustParseAddr(s)); err != nil {
			t.Errorf("CheckAddress(%s) = %v, want nil", s, err)
		}
	}
}

// TestPolicyExempt: an Exempt hook admits what the table refuses for that
// Policy only, and the package-level CheckAddress is unaffected.
func TestPolicyExempt(t *testing.T) {
	a := netip.MustParseAddr("10.1.2.3")
	if err := (Policy{}).check(a); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("no hook: %v", err)
	}
	p := Policy{Exempt: func(x netip.Addr) bool { return x == a }}
	if err := p.check(a); err != nil {
		t.Errorf("hooked: %v", err)
	}
	if err := p.check(netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Error("hook admitted an address it did not name")
	}
	if err := CheckAddress(a); err == nil {
		t.Error("CheckAddress was widened")
	}
}
