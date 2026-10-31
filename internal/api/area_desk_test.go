// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// TestDeskSectionQueryIsTheClientsFilter pins Area/getDesk's sections to the
// Email/query the web client sent before #665 (web/src/core/desk.ts
// sectionFilter, sorted by DEFAULT_SORT, collapseThreads, one page), parsed
// by the SAME parser Email/query uses. store.DeskSnapshot builds each
// section from store.DeskSectionQuery; if that ever stopped being what the
// client's filter means, the desk would show different rows depending on
// which path the client took — and nothing else would notice, because the
// store's agreement test compares DeskSnapshot against QueryEmails over
// whatever EmailQuery it is handed.
func TestDeskSectionQueryIsTheClientsFilter(t *testing.T) {
	area := ulid.Make()
	account := ulid.Make()
	threshold := float32(0.37)

	cases := []struct {
		name   string
		filter map[string]any
		sec    store.DeskSectionPage
		scope  store.DeskScope
	}{
		{"area, every account",
			map[string]any{"inArea": area.String(), "onDesk": true, "onSurface": true, "isSnoozed": false},
			store.DeskSectionPage{AreaID: &area, Limit: 50},
			store.DeskScope{Threshold: threshold}},
		{"area, one account, grown window",
			map[string]any{"inArea": area.String(), "onDesk": true, "onSurface": true, "isSnoozed": false,
				"inAccount": account.String()},
			store.DeskSectionPage{AreaID: &area, Limit: 100},
			store.DeskScope{AccountID: &account, Threshold: threshold}},
		{"needs sorting, every account",
			map[string]any{"unsorted": true, "onDesk": true, "isSnoozed": false},
			store.DeskSectionPage{Limit: 50},
			store.DeskScope{Threshold: threshold}},
		{"needs sorting, one account, second page",
			map[string]any{"unsorted": true, "onDesk": true, "isSnoozed": false, "inAccount": account.String()},
			store.DeskSectionPage{Limit: 50, Position: 50},
			store.DeskScope{AccountID: &account, Threshold: threshold}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			limit := tc.sec.Limit
			plan, mErr := parseEmailQuery(emailQueryArgs{
				Filter:          raw,
				Sort:            []sortComparator{{Property: "returnedAt", IsAscending: new(bool)}},
				Position:        tc.sec.Position,
				Limit:           &limit,
				CalculateTotal:  true,
				CollapseThreads: true,
			})
			if mErr != nil {
				t.Fatalf("parse: %+v", mErr)
			}
			want := plan.query
			// What emailQueryWith adds from the transaction: the stored line,
			// for onSurface. getDesk binds the same value into its sections.
			if want.OnSurface != nil {
				want.LineThreshold = tc.scope.Threshold
			}
			got := store.DeskSectionQuery(tc.sec, tc.scope)
			// ImportantThreshold is a presentation constant the parser always
			// sets; no desk section reads it (no isImportant, no priority sort).
			got.ImportantThreshold = want.ImportantThreshold
			if !reflect.DeepEqual(got, want) {
				t.Errorf("DeskSectionQuery = %+v\nclient's Email/query parses to %+v", got, want)
			}
		})
	}
}
