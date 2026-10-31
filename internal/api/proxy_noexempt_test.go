// SPDX-License-Identifier: AGPL-3.0-only

//go:build !e2eprivateimages

package api

import "testing"

// TestProxyEgressHasNoExemptionInShippedBuild: the shipped (untagged) build has
// no way to widen the image proxy's egress guard (#676).
func TestProxyEgressHasNoExemptionInShippedBuild(t *testing.T) {
	if proxyEgressExempt != nil {
		t.Fatal("proxyEgressExempt is set in a build without the e2eprivateimages tag")
	}
	if c := newProxyEgressClient(ProxyConfig{Enabled: true}); c == nil {
		t.Fatal("no client")
	}
}
