// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2eprivateimages

package api

import "net/netip"

// This file exists only in the image the proxied-images CI lane builds
// (-tags e2eprivateimages). The lane's image host lives on a private Docker
// network, which the egress guard rightly refuses; the release image is built
// without the tag, so it contains neither this hook nor an environment variable
// for it (#676).
//
// Deliberately narrow: RFC 1918 IPv4 only. Loopback, link-local (the cloud
// metadata service) and everything else stay refused.
var e2ePrivateNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func init() {
	proxyEgressExempt = func(a netip.Addr) bool {
		for _, p := range e2ePrivateNets {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
}
