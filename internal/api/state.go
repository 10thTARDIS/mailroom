// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"fmt"
	"strconv"
	"strings"
)

// JMAP state strings are opaque to clients (RFC 8620 §5.2); internally they are
// the decimal per-(user, type) change sequence rendered with an "s:" prefix
// (data-model §3.1, e.g. "s:184223"). The prefix lets the encoding evolve
// without a client that treated the value as opaque ever noticing.
const statePrefix = "s:"

// encodeState renders a change sequence as its opaque JMAP state string.
func encodeState(seq int64) string {
	return statePrefix + strconv.FormatInt(seq, 10)
}

// decodeState parses a client-supplied state string back to a change sequence.
// A malformed state is an error the caller maps to cannotCalculateChanges (the
// client then refetches) rather than trusting a forged/garbled cursor.
func decodeState(s string) (int64, error) {
	if !strings.HasPrefix(s, statePrefix) {
		return 0, fmt.Errorf("api: malformed state string %q", s)
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(s, statePrefix), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("api: malformed state string %q", s)
	}
	return n, nil
}
