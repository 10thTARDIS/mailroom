// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2eprivateimages

package api

import (
	"net/netip"
	"testing"
)

// The tagged exemption must stay narrow: private ranges in, loopback and the
// metadata service out.
func TestE2EPrivateExemptionIsNarrow(t *testing.T) {
	for s, want := range map[string]bool{
		"10.250.99.5": true, "172.16.0.1": true, "192.168.1.1": true,
		"127.0.0.1": false, "169.254.169.254": false, "::1": false, "192.88.99.1": false,
	} {
		if got := proxyEgressExempt(netip.MustParseAddr(s)); got != want {
			t.Errorf("exempt(%s) = %v, want %v", s, got, want)
		}
	}
}
