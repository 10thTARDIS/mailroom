// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/netip"
	"strings"
)

// maxForwardedHops bounds how many X-Forwarded-For entries are examined, so a
// hostile header cannot make the walk long. A chain this deep is not a real
// proxy chain; the walk gives up and the connection's own address is used.
const maxForwardedHops = 16

// displayAddr is the address a session's devices-list entry shows ("where it last
// connected from"), behind a reverse proxy if one is trusted.
//
// ⚠ IT IS DISPLAY DATA ONLY. NEVER USE IT FOR A SECURITY DECISION: a rate limit, an
// allow-list, a lockout. Even with the right trusted set the answer is the
// proxy's claim about the client, and the bearer rate limiter keeps using
// RemoteAddr (clientAddr) for exactly that reason.
func (a *Authenticator) displayAddr(r *http.Request) string {
	return forwardedClientAddr(r, a.cfg.TrustedProxies)
}

// forwardedClientAddr returns the connection's address, except when that address
// is in trusted: then X-Forwarded-For is walked right to left and the first hop
// NOT in trusted is returned (the rightmost entries are the ones our own proxies
// appended; everything left of them is client-supplied, and only believed up to
// the first untrusted hop).
//
// It falls back to the connection's address, never to a guess, when:
//   - the connection is not from a trusted proxy (the header is attacker-written
//     and ignored, which is the spoofing case);
//   - an entry is malformed (empty, not an IP, carries a port or a zone): the walk
//     stops, and the trusted hop it reached is NOT returned, since that would
//     report a proxy as the client;
//   - every entry is trusted, or there are more than maxForwardedHops.
func forwardedClientAddr(r *http.Request, trusted []netip.Prefix) string {
	remote := clientAddr(r)
	if len(trusted) == 0 {
		return remote
	}
	remoteIP, err := netip.ParseAddr(remote)
	if err != nil || !inPrefixes(trusted, remoteIP) {
		return remote
	}
	header := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if header == "" {
		return remote
	}
	hops := strings.Split(header, ",")
	if len(hops) > maxForwardedHops {
		return remote
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil || ip.Zone() != "" {
			return remote
		}
		if !inPrefixes(trusted, ip) {
			return ip.Unmap().String()
		}
	}
	return remote
}

func inPrefixes(prefixes []netip.Prefix, ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
