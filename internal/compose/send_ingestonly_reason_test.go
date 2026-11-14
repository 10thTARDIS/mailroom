// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"strings"
	"testing"
)

// The stored reason for a send refused because the account is shadowed has to
// answer the question the user asks next, which is "is it still coming?". It is
// not: ADR-028 A1 decided a scheduled send dispatched while shadowed fails for
// good and is not sent when the mode lifts.
//
// The integration test for the gate compares the stored text with this very
// variable, so it cannot notice the sentence being edited away. This one pins the
// content: cause, consequence and the way out.
//
// NEGATIVE CONTROL: shorten errIngestOnlyCannotSend back to its first sentence
// and each phrase below fails.
func TestIngestOnlyRefusalSaysItWillNotBeSentLater(t *testing.T) {
	got := errIngestOnlyCannotSend.Error()
	for _, want := range []string{
		"ingest-only mode",          // the cause
		"was not sent",              // the fact
		"not be sent automatically", // the consequence of lifting the mode
		"turned off",                // ...named as that event
		"send it again",             // the way out
		"Drafts",                    // ...and where it is
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the stored reason %q does not contain %q", got, want)
		}
	}
}
