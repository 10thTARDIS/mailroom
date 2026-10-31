// SPDX-License-Identifier: AGPL-3.0-only

package provider

import "testing"

func TestManageSieveEndpoint(t *testing.T) {
	for _, profile := range []string{"migadu", "generic", "", "unknown-profile"} {
		ep, ok, why := ManageSieveEndpoint(profile, "imap.example.com")
		if !ok || why != "" || ep.Host != "imap.example.com" || ep.Port != 4190 || ep.ImplicitTLS || ep.Source == "" {
			t.Errorf("%q: %+v ok=%v why=%q", profile, ep, ok, why)
		}
	}
	if _, ok, why := ManageSieveEndpoint("gmail", "imap.gmail.example"); ok || why == "" {
		t.Errorf("gmail: ok=%v why=%q, want no ManageSieve with a reason", ok, why)
	}
}
