// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"testing"
)

// The canonical form a key's draft is fingerprinted on must be injective:
// two different drafts never encode alike, whatever their fields contain
// (#874 review). The NUL-joined form made each of these pairs one string.
func TestDraftCanonicalIsInjective(t *testing.T) {
	pairs := []struct {
		name string
		a, b emailCreate
	}{
		{"address name/email boundary",
			emailCreate{To: []jmapEmailAddr{{Email: "a@b", Name: "n\x00m"}}},
			emailCreate{To: []jmapEmailAddr{{Email: "a@b\x00n", Name: "m"}}}},
		{"attachment field boundary",
			emailCreate{Attachments: []jmapAttachment{{BlobID: "x", Type: "t\x00n"}}},
			emailCreate{Attachments: []jmapAttachment{{BlobID: "x\x00t", Type: "n"}}}},
	}
	for _, p := range pairs {
		if bytes.Equal(draftCanonical(p.a), draftCanonical(p.b)) {
			t.Errorf("%s: two different drafts have the canonical form %s", p.name, draftCanonical(p.a))
		}
	}
	// Positive control: what the form deliberately treats as the same draft.
	same := emailCreate{To: []jmapEmailAddr{{Email: "C@d", Name: " Carol "}, {Email: "a@b"}}}
	reordered := emailCreate{To: []jmapEmailAddr{{Email: "a@b"}, {Email: "c@d", Name: "Carol"}}}
	if !bytes.Equal(draftCanonical(same), draftCanonical(reordered)) {
		t.Error("the same recipients in another order and case must be the same draft")
	}
}
