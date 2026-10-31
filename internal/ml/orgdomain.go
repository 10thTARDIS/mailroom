// SPDX-License-Identifier: AGPL-3.0-only

package ml

import "golang.org/x/net/publicsuffix"

// OrgDomain returns d's organisational domain under the Public Suffix List:
// the public suffix plus one label (publicsuffix.EffectiveTLDPlusOne), so
// `news.shop.example.co.uk` is `example.co.uk`, never `co.uk`. Both the ICANN
// and the private sections count as suffixes, so one tenant of a hosting
// suffix is never another tenant's organisation.
//
// THE ONE PSL HELPER (#846): DMARC relaxed alignment (DMARCHeaderFromMatch)
// and local DKIM alignment (ingest.DKIMAligned) both read the list through it.
//
// ok is false, and the caller must refuse, when:
//
//   - d is empty, or not already in NormalizeAuthDomain form (lowercase,
//     no trailing dot, A-label). Normalising here instead would make a
//     double trailing dot vanish in two steps; a caller compares the very
//     string it passed, so it must pass the normal form;
//   - d is itself a public suffix (`co.uk`, `example`), or the list cannot
//     derive one (an empty label, for instance).
func OrgDomain(d string) (string, bool) {
	if d == "" || NormalizeAuthDomain(d) != d {
		return "", false
	}
	org, err := publicsuffix.EffectiveTLDPlusOne(d)
	if err != nil || org == "" {
		return "", false
	}
	return org, true
}
