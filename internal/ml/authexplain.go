// SPDX-License-Identifier: AGPL-3.0-only

package ml

// AuthHeaderExplain is one Authentication-Results header as learned placement
// reads it, for `mailroom-admin placement-explain` (#840). DIAGNOSTIC ONLY: it
// restates what TopmostTrustedAuthResults and DMARCPassFor decide, from the
// same tokenizer and the same trust test, and decides nothing itself.
type AuthHeaderExplain struct {
	// AuthservID is the header's authserv-id after the strict parse, "" when
	// the header has none Mailroom accepts (authservIDOf).
	AuthservID string
	// Trusted: AuthservID is one of the trusted ids.
	Trusted bool
	// Topmost: the topmost trusted header, the only one learned placement
	// reads (TopmostTrustedAuthResults).
	Topmost bool
	// Clauses are the header's result clauses, in order (parseResinfo).
	Clauses []ResultClause
}

// ExplainAuthHeaders describes every Authentication-Results header value
// (message order, topmost first) against the trusted ids (already resolved
// with TrustedAt for the message's received time).
func ExplainAuthHeaders(values []string, trusted []string) []AuthHeaderExplain {
	ok := trustedSet(trusted)
	out := make([]AuthHeaderExplain, 0, len(values))
	seenTop := false
	for _, v := range values {
		id := authservIDOf(v)
		h := AuthHeaderExplain{AuthservID: id, Trusted: ok[id], Clauses: parseResinfo(v)}
		if h.Trusted && !seenTop {
			h.Topmost = true
			seenTop = true
		}
		out = append(out, h)
	}
	return out
}
