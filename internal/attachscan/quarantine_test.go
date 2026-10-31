// SPDX-License-Identifier: AGPL-3.0-only

package attachscan

import (
	"crypto/sha256"
	"testing"

	"mailroom/internal/mime"
)

// Blocked is the gate rule, and it is shared by the download route, the JMAP
// property and `attachment-report` so those three cannot disagree about what is
// quarantined. A table test is worth more than its size here: every row is a
// decision ADR-027 argues for somewhere, and the two easy mistakes — letting
// `unknown` through, or blocking `unscanned` — are one character each.
func TestBlockedIsTheGateRule(t *testing.T) {
	for _, tc := range []struct {
		status   string
		released bool
		want     bool
		why      string
	}{
		{"flagged", false, true, "a real signature matched"},
		{"unknown", false, true, "§5b: the same enforcement as flagged, or the attacker only has to AVOID the scanner"},
		{"clean", false, false, "nothing was found"},
		{"unscanned", false, false, "no scanner is configured, or none has reached this row; blocking it breaks a mailbox that never asked for one"},
		{"flagged", true, false, "the user overrode it"},
		{"unknown", true, false, "the user overrode it"},
		{"", false, false, "an unrecognised state must not block: a typo in a writer would otherwise quarantine a mailbox"},
	} {
		if got := Blocked(tc.status, tc.released); got != tc.want {
			t.Errorf("Blocked(%q, released=%v) = %v, want %v — %s",
				tc.status, tc.released, got, tc.want, tc.why)
		}
	}
}

// PartHash is the CONTENT IDENTITY a quarantine and a release act on, so the
// two properties that matter are that identical bytes agree and that empty
// bytes produce NO identity at all.
func TestPartHashIdentifiesContentAndRefusesEmptiness(t *testing.T) {
	mk := func(data []byte) mime.AttachmentPart {
		return mime.AttachmentPart{Path: "2", Node: &mime.Structure{Data: data}}
	}
	want := sha256.Sum256([]byte("the same bytes"))
	a := PartHash(mk([]byte("the same bytes")))
	b := PartHash(mk([]byte("the same bytes")))
	if string(a) != string(want[:]) || string(b) != string(a) {
		t.Errorf("identical bytes produced different identities (%x vs %x) — a release keyed on "+
			"one would miss the other, which is the hit-1-of-N-copies bug", a, b)
	}
	if c := PartHash(mk([]byte("different bytes"))); string(c) == string(a) {
		t.Errorf("different bytes collided on %x", c)
	}
	// ⚠ EMPTY BYTES GET NO IDENTITY. sha256 of nothing is a perfectly good
	// digest, and that is the problem: every empty part in the mailbox would
	// share it, so one release against it would open the gate on all of them.
	if got := PartHash(mk(nil)); got != nil {
		t.Errorf("PartHash of an empty part = %x, want nil — a single release against the digest "+
			"of nothing would cover every empty part in the mailbox", got)
	}
}
