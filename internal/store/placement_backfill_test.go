// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
)

// An exclude names an address (exactly) or a domain (the From address's own
// domain, exactly: not a subdomain, not a suffix), case-insensitively.
func TestExcludedBy(t *testing.T) {
	ex := normalizeExcludes([]string{" Alice@Corp.Example ", "@other.example", "other.example", ""})
	if len(ex) != 2 {
		t.Fatalf("normalized = %v, want 2 distinct entries", ex)
	}
	for _, c := range []struct {
		from string
		want string
	}{
		{"alice@corp.example", "alice@corp.example"},
		{"bob@corp.example", ""},
		{"erin@other.example", "other.example"},
		{"erin@sub.other.example", ""},
		{"erin@another.example", ""},
	} {
		a, d, ok := placement.KeysFor(c.from)
		if !ok {
			t.Fatalf("keys for %s", c.from)
		}
		got, hit := excludedBy(ex, a, d)
		if got != c.want || hit != (c.want != "") {
			t.Errorf("%s: excluded by %q (%v), want %q", c.from, got, hit, c.want)
		}
	}
}

// The apply-time re-check compares label sets with their attribution.
func TestSameRemoved(t *testing.T) {
	l1, l2 := ulid.Make(), ulid.Make()
	a := []RemovedAreaLabel{{l1, LabelByDesk}, {l2, LabelBySetup}}
	if !sameRemoved(a, []RemovedAreaLabel{{l2, LabelBySetup}, {l1, LabelByDesk}}) {
		t.Error("order must not matter")
	}
	if sameRemoved(a, []RemovedAreaLabel{{l1, LabelByDesk}, {l2, LabelByDesk}}) {
		t.Error("a changed attribution must not compare equal")
	}
	if sameRemoved(a, a[:1]) || !sameRemoved(nil, []RemovedAreaLabel{}) {
		t.Error("length")
	}
}
