// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"

	"mailroom/internal/nudge"
	"mailroom/internal/store"
)

// The store's candidate window and the rule's expiry are one number (#58).
func TestFollowUpMaxAgeIsNudgeMaxAge(t *testing.T) {
	if store.FollowUpMaxAge != nudge.MaxAge {
		t.Fatalf("store.FollowUpMaxAge %v != nudge.MaxAge %v", store.FollowUpMaxAge, nudge.MaxAge)
	}
}

// The parse cache is bounded and evicts the least recently used entry.
func TestFollowUpCacheEvictsLRU(t *testing.T) {
	c := newFollowUpCache(2)
	c.put(parsedEntry{key: "a", ok: true})
	c.put(parsedEntry{key: "b", ok: true})
	if _, ok := c.get("a"); !ok { // a is now the most recent
		t.Fatal("a missing")
	}
	c.put(parsedEntry{key: "c", ok: true})
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted (least recently used)")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s evicted", k)
		}
	}
	if c.order.Len() != 2 || len(c.items) != 2 {
		t.Fatalf("size %d/%d, want 2", c.order.Len(), len(c.items))
	}
}
