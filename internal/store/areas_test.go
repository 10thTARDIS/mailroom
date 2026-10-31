// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// TestValidateAreaFeed pins the Go-side half of area_feeds_exactly_one
// (migration 00046) — a database round trip should never be the first place a
// malformed feed is noticed.
func TestValidateAreaFeed(t *testing.T) {
	id := ulid.ULID{}
	cases := []struct {
		name    string
		feed    AreaFeed
		wantErr bool
	}{
		{"tab only", AreaFeed{Tab: tabs.Social}, false},
		{"label only", AreaFeed{LabelID: &id}, false},
		{"neither", AreaFeed{}, true},
		{"both", AreaFeed{Tab: tabs.Social, LabelID: &id}, true},
		{"unknown tab", AreaFeed{Tab: tabs.Tab("bogus")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateAreaFeed(c.feed)
			if c.wantErr && !errors.Is(err, ErrAreaFeedInvalid) {
				t.Errorf("validateAreaFeed(%+v) = %v, want ErrAreaFeedInvalid", c.feed, err)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validateAreaFeed(%+v) = %v, want nil", c.feed, err)
			}
		})
	}
}

// TestFeedKeyDistinguishesTabsFromLabels: a tab and a label must never collide
// in the map replaceAreaFeeds diffs against, even by coincidence of string
// value.
func TestFeedKeyDistinguishesTabsFromLabels(t *testing.T) {
	id := ulid.ULID{}
	tabKey := feedKey(AreaFeed{Tab: tabs.Social})
	labelKey := feedKey(AreaFeed{LabelID: &id})
	if tabKey == labelKey {
		t.Errorf("feedKey collided: tab key %q == label key %q", tabKey, labelKey)
	}
}
