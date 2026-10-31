// SPDX-License-Identifier: AGPL-3.0-only

package mime

import "unicode/utf8"

// EAI/SMTPUTF8 (RFC 6531 internationalized email addresses) status: spec
// §18.1 flags Migadu's SMTPUTF8 support as an open question tracked in
// Forgejo issue #17. AddressRequiresEAI is deliberately a detection-only
// stub — it does not implement EAI itself (no SMTPUTF8 SIZE/negotiation,
// no A-label/U-label conversion, no envelope handling). It exists so
// callers can *notice* an internationalized address and decide not to
// silently mishandle it while the provider question is open.
//
// Compose-side behavior once the answer is known:
//   - If Migadu confirms SMTPUTF8 support: implement it properly (this
//     stub goes away).
//   - If not: compose must produce a clear, user-visible error for
//     addresses where AddressRequiresEAI is true, rather than attempting
//     to send and failing opaquely at the provider.
//
// Until issue #17 is resolved, treat AddressRequiresEAI == true as
// "unsupported — do not attempt to send"; this package does not enforce
// that itself, since enforcement is a compose-time/outbound concern, not a
// parsing concern.

// AddressRequiresEAI reports whether addr (an RFC 5321/5322 addr-spec,
// e.g. "user@例え.jp") contains non-ASCII bytes in the local-part or
// domain, and would therefore require SMTPUTF8 (RFC 6531) to send.
func AddressRequiresEAI(addr string) bool {
	for i := 0; i < len(addr); i++ {
		if addr[i] >= utf8.RuneSelf {
			return true
		}
	}
	return false
}
