// SPDX-License-Identifier: AGPL-3.0-only

package egress

import (
	"errors"
	"fmt"
	"net/netip"
)

// ErrBlockedAddress is returned when an address is not publicly routable. It is
// a distinct sentinel so a caller can tell "the guard refused this" from "the
// network failed" — a refusal that looks like an ordinary error is a refusal
// nobody can act on (ADR-026 §3a).
var ErrBlockedAddress = errors.New("egress: address is not publicly routable")

// blockedPrefix is one refused range together with the reason it is refused. The
// reason is carried into the error because "blocked" alone is not a diagnosis:
// an operator reading a log needs to know whether they hit the metadata service
// or their own LAN.
type blockedPrefix struct {
	prefix netip.Prefix
	reason string
}

// blockedPrefixes is the COMPLETE list of what this package refuses. There is
// deliberately no second, overlapping check against the net/netip predicates
// (IsPrivate, IsLoopback, …) at run time: two mechanisms that both refuse the
// same address make every test ambiguous about which one fired, which is how a
// test that cannot fail gets written. The stdlib's knowledge is used instead as
// a TEST — TestTableAgreesWithStdlibPredicates cross-checks this table against
// those predicates — so the safety net exists where it can actually go red.
//
// Order matters only for the error message: the first match wins, so the more
// specific entry is listed before the range that contains it.
var blockedPrefixes = []blockedPrefix{
	// ---- IPv4 ----
	{netip.MustParsePrefix("0.0.0.0/8"), "unspecified / \"this network\""},
	{netip.MustParsePrefix("10.0.0.0/8"), "private (RFC 1918)"},
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT (RFC 6598)"},
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	// ⚠ 169.254.169.254 lives here. It is the cloud instance-metadata service and
	// the single most valuable SSRF target there is; it is not a special case in
	// this table because the whole link-local range is refused.
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local (includes the 169.254.169.254 metadata service)"},
	{netip.MustParsePrefix("172.16.0.0/12"), "private (RFC 1918)"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation (TEST-NET-1)"},
	// 6to4 relay anycast (RFC 7526, deprecated): publicly routable in form but
	// assigned to no one, so nothing legitimate lives here, and a test lane once
	// relied on it being admitted (#676).
	{netip.MustParsePrefix("192.88.99.0/24"), "deprecated 6to4 relay anycast (RFC 7526)"},
	{netip.MustParsePrefix("192.168.0.0/16"), "private (RFC 1918)"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking (RFC 2544)"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation (TEST-NET-2)"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation (TEST-NET-3)"},
	{netip.MustParsePrefix("224.0.0.0/4"), "multicast"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved (includes the 255.255.255.255 broadcast address)"},

	// ---- IPv6 ----
	{netip.MustParsePrefix("::/128"), "unspecified"},
	{netip.MustParsePrefix("::1/128"), "loopback"},
	// ⚠ ::/96 is the deprecated IPv4-COMPATIBLE encoding (::a.b.c.d), which
	// embeds an IPv4 address without being an IPv4-MAPPED address. Unmap() does
	// not touch it, so without this entry ::7f00:1 would reach 127.0.0.1 while
	// matching no IPv4 rule and no IPv6 rule. Listed after ::/128 and ::1/128 so
	// those keep their more precise reasons.
	{netip.MustParsePrefix("::/96"), "IPv4-compatible IPv6 (deprecated; embeds an IPv4 address)"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "local-use NAT64 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("100::/64"), "discard-only (RFC 6666)"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("2002::/16"), "6to4 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("fc00::/7"), "unique local"},
	{netip.MustParsePrefix("fe80::/10"), "link-local"},
	{netip.MustParsePrefix("ff00::/8"), "multicast"},
}

// CheckAddress reports whether an address may be connected to, returning an
// error wrapping ErrBlockedAddress if not.
//
// ⚠ THIS COMMENT USED TO CLAIM "registration paths use it ... (internal/api)".
// THAT WAS FALSE, and it was false in the most expensive way available: two
// lines below, the same comment says "the difference between a guard and the
// appearance of one". No file in internal/api called this, and the account
// registration path — the one an authenticated caller can point at an arbitrary
// host:port — dialled with no guard at all (#527). A document asserting a guard
// exists is how the next reader stops looking for it.
//
// What is true: the external callers are `mailroom-admin proxy-report`, which
// checks an already-stored address without dialling; CheckHost, the pre-dial
// check account validation runs on both hosts; and, through checkedAddrs,
// Dialer — the guarded dialer every account connection (IMAP sync and
// validation, SMTP submission) goes through (#625 §3).
//
// ⚠ It unmaps before comparing ANYTHING. ::ffff:127.0.0.1 is loopback wearing an
// IPv6 costume: compared against the IPv6 entries alone it matches none of them
// and sails through. This one line is the difference between a guard and the
// appearance of one, and it is the bypass ADR-026 §3a calls out by name.
func CheckAddress(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: not a valid IP address", ErrBlockedAddress)
	}
	// ⚠ The zone must come off too, and for a nastier reason than the unmap.
	// netip.Prefix.Contains returns FALSE for any address carrying an IPv6 zone,
	// because prefixes have no zones — so fe80::1%eth0 would match no entry in
	// the table and be permitted, while plain fe80::1 is refused. Stripping the
	// zone is what makes the table apply to it. A zoned address is link-local in
	// practice, so this turns a silent pass into the fe80::/10 refusal it should
	// always have been.
	a := addr.Unmap().WithZone("")
	for _, b := range blockedPrefixes {
		// Contains is false for a family mismatch, so the IPv4 and IPv6 halves of
		// the table never need separating.
		if b.prefix.Contains(a) {
			return fmt.Errorf("%w: %s is %s (%s)", ErrBlockedAddress, a, b.reason, b.prefix)
		}
	}
	return nil
}
