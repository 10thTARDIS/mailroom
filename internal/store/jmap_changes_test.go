// SPDX-License-Identifier: AGPL-3.0-only

package store

// Unit tests for the pure §3.2 collapse (no database). These run in the default
// (non-integration) build so the bucketing is exercised without a Postgres.

import (
	"testing"

	"github.com/oklog/ulid/v2"
)

// oid returns a deterministic ULID for a small integer, so tests can name
// objects "1".."9" and still compare by value.
func oid(n byte) ulid.ULID {
	var u ulid.ULID
	u[15] = n
	return u
}

func seqOf(ids []ulid.ULID) []byte {
	out := make([]byte, len(ids))
	for i, id := range ids {
		out[i] = id[15]
	}
	return out
}

func TestCollapseChanges(t *testing.T) {
	// Each entry is {seq, object(n), op}. Objects are addressed by their low
	// byte for readable expectations.
	e := func(seq int64, n byte, op ChangeOp) changeEntry {
		return changeEntry{seq: seq, objectID: oid(n), op: op}
	}

	tests := []struct {
		name       string
		entries    []changeEntry
		since      int64
		current    int64
		maxChanges int
		wantNew    int64
		wantMore   bool
		created    []byte
		updated    []byte
		destroyed  []byte
	}{
		{
			name:    "empty range reports current state",
			entries: nil,
			since:   5, current: 5,
			wantNew: 5,
		},
		{
			// created … last op ≠ destroyed → created
			name:    "created then updated is created",
			entries: []changeEntry{e(1, 1, OpCreated), e(2, 1, OpUpdated)},
			since:   0, current: 2,
			wantNew: 2,
			created: []byte{1},
		},
		{
			// created … destroyed → omitted entirely (client never knew it)
			name:    "created then destroyed is omitted",
			entries: []changeEntry{e(1, 1, OpCreated), e(2, 1, OpDestroyed)},
			since:   0, current: 2,
			wantNew: 2,
		},
		{
			// first op ≠ created … last destroyed → destroyed
			name:    "updated then destroyed is destroyed",
			entries: []changeEntry{e(1, 1, OpUpdated), e(2, 1, OpDestroyed)},
			since:   0, current: 2,
			wantNew:   2,
			destroyed: []byte{1},
		},
		{
			// otherwise → updated
			name:    "updated only is updated",
			entries: []changeEntry{e(1, 1, OpUpdated)},
			since:   0, current: 1,
			wantNew: 1,
			updated: []byte{1},
		},
		{
			name: "multiple objects bucket independently, first-touch order",
			entries: []changeEntry{
				e(1, 3, OpCreated),
				e(2, 1, OpUpdated),
				e(3, 2, OpCreated), e(4, 2, OpDestroyed), // omitted
				e(5, 4, OpUpdated), e(6, 4, OpDestroyed), // destroyed
				e(7, 3, OpUpdated), // 3 stays created
			},
			since: 0, current: 7,
			wantNew:   7,
			created:   []byte{3},
			updated:   []byte{1},
			destroyed: []byte{4},
		},
		{
			// A destroyed-then-recreated object within the window resolves to
			// created (first=destroyed? no — first op is destroyed, last is
			// created → "otherwise" → updated). Documents the edge.
			name:    "destroyed then recreated collapses to updated",
			entries: []changeEntry{e(1, 1, OpDestroyed), e(2, 1, OpCreated)},
			since:   0, current: 2,
			wantNew: 2,
			updated: []byte{1},
		},
		{
			// maxChanges truncates before the 3rd distinct object; newState is
			// the seq of the last included row (an object boundary), hasMore set.
			name: "maxChanges truncates at object boundary",
			entries: []changeEntry{
				e(1, 1, OpCreated),
				e(2, 2, OpUpdated),
				e(3, 3, OpCreated), // would be the 3rd distinct object
			},
			since: 0, current: 3, maxChanges: 2,
			wantNew:  2,
			wantMore: true,
			created:  []byte{1},
			updated:  []byte{2},
		},
		{
			// A later row for an already-included object beyond the cap is not
			// scanned; the object still resolves from the rows within the window.
			name: "maxChanges keeps already-included object rows within window",
			entries: []changeEntry{
				e(1, 1, OpCreated),
				e(2, 2, OpUpdated),
				e(3, 3, OpUpdated), // 3rd object triggers the cap here
				e(4, 1, OpUpdated),
			},
			since: 0, current: 4, maxChanges: 2,
			wantNew:  2,
			wantMore: true,
			created:  []byte{1},
			updated:  []byte{2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collapseChanges(tc.entries, tc.since, tc.current, tc.maxChanges)
			if got.OldState != tc.since {
				t.Errorf("OldState = %d, want %d", got.OldState, tc.since)
			}
			if got.NewState != tc.wantNew {
				t.Errorf("NewState = %d, want %d", got.NewState, tc.wantNew)
			}
			if got.HasMore != tc.wantMore {
				t.Errorf("HasMore = %v, want %v", got.HasMore, tc.wantMore)
			}
			assertBucket(t, "created", seqOf(got.Created), tc.created)
			assertBucket(t, "updated", seqOf(got.Updated), tc.updated)
			assertBucket(t, "destroyed", seqOf(got.Destroyed), tc.destroyed)
		})
	}
}

func assertBucket(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
	}
}

func TestApplyKeywordDelta(t *testing.T) {
	tests := []struct {
		name          string
		cur, add, rem []string
		want          []string
	}{
		{"add new", []string{"a"}, []string{"b"}, nil, []string{"a", "b"}},
		{"remove existing", []string{"a", "b"}, nil, []string{"a"}, []string{"b"}},
		{"add dedupes", []string{"a"}, []string{"a"}, nil, []string{"a"}},
		{"remove wins over add", []string{"a"}, []string{"b"}, []string{"b"}, []string{"a"}},
		{"empty stays empty", nil, nil, nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyKeywordDelta(tc.cur, tc.add, tc.rem)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
