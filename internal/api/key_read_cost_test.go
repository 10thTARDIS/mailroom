// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// #49 review, F3. RED PROBES: return the request unchanged from keyQueryLimit,
// or drop readAnswerCost's Thread/get branch, and these fail.
func TestKeyQueryLimit(t *testing.T) {
	ptr := func(n int) *int { return &n }
	for _, tc := range []struct {
		in   *int
		want int
	}{
		{nil, KeyQueryDefaultLimit},
		{ptr(0), 0},
		{ptr(7), 7},
		{ptr(KeyQueryMaxLimit), KeyQueryMaxLimit},
		{ptr(KeyQueryMaxLimit + 1), KeyQueryMaxLimit},
		{ptr(1 << 30), KeyQueryMaxLimit},
	} {
		if got := keyQueryLimit(tc.in); got != tc.want {
			t.Errorf("keyQueryLimit(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestReadAnswerCostChargesThreadGetByEmails(t *testing.T) {
	ids := make([]string, 2*ReadUnitObjects+1)
	for i := range ids {
		ids[i] = "m"
	}
	thread, _ := json.Marshal(map[string]any{"list": []any{map[string]any{"id": "t1", "emailIds": ids}}})
	if got := readAnswerCost("Thread/get", thread); got != 3 {
		t.Errorf("Thread/get of one thread with %d emails costs %d, want 3", len(ids), got)
	}
	// Other reads are still charged by objects listed.
	if got := readAnswerCost("Email/get", thread); got != 1 {
		t.Errorf("Email/get listing one object costs %d, want 1", got)
	}
}

// /changes answers are charged by the ids they name (#49 review, third pass).
// RED PROBE: drop the /changes branch in readAnswerCost and this costs 1.
func TestReadAnswerCostChargesChangesByIDs(t *testing.T) {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "x"
		}
		return out
	}
	ans, _ := json.Marshal(map[string]any{"created": ids(60), "updated": ids(30), "destroyed": ids(20)})
	if got := readAnswerCost("Email/changes", ans); got != 3 {
		t.Errorf("Email/changes naming 110 ids costs %d, want 3", got)
	}
	if got := readAnswerCost("Email/changes", []byte(`{"created":[],"updated":[],"destroyed":[]}`)); got != 1 {
		t.Errorf("an empty /changes costs %d, want 1", got)
	}
}

// #850, #852: every key-reachable /query and /changes in the policy table is
// charged up front for the bound it will run with — the default page when the
// call states none, the maximum when it asks for more — so a method opened to
// keys later cannot ship without the cap (the answer side is
// TestEveryKeyQueryRunsWithTheKeyLimit and TestAgentKeyChangesAreBounded).
//
// RED PROBES: drop readCost's /query case and Mailbox/query, Label/query,
// Contact/query and EmailSubmission/query are charged 1 for a limit-less
// call; drop its /changes case and every /changes is.
func TestEveryKeyQueryIsBounded(t *testing.T) {
	a := New(nil)
	cost := func(name string, pol keyPolicy, args string) int {
		t.Helper()
		if pol.needs != nil {
			n, mErr := pol.needs(context.Background(), a, ulid.ULID{}, nil, json.RawMessage(args))
			if mErr != nil {
				t.Fatalf("%s %s: %v", name, args, mErr)
			}
			return n.cost
		}
		return pol.staticNeeds(name, json.RawMessage(args)).cost
	}
	huge := 1 << 30
	queries, changes := 0, 0
	for name, pol := range keyPolicies {
		if !pol.keyReachable() {
			continue
		}
		switch {
		case strings.HasSuffix(name, "/query"):
			queries++
			if got, want := cost(name, pol, `{}`), unitsFor(keyQueryLimitFor(name, nil)); got != want {
				t.Errorf("%s with no limit costs %d, want %d (the default page, charged up front)", name, got, want)
			}
			want := unitsFor(keyQueryLimitFor(name, &huge))
			if want > unitsFor(KeyQueryMaxLimit) {
				t.Errorf("%s runs with more than KeyQueryMaxLimit", name)
			}
			if got := cost(name, pol, `{"limit":1073741824}`); got != want {
				t.Errorf("%s with a huge limit costs %d, want %d", name, got, want)
			}
			if got := cost(name, pol, `{"limit":1073741824,"ids":["x"]}`); got != want {
				t.Errorf("%s with a huge limit and an ids argument costs %d, want %d", name, got, want)
			}
		case strings.HasSuffix(name, "/changes"):
			changes++
			if got, want := cost(name, pol, `{}`), unitsFor(KeyQueryDefaultLimit); got != want {
				t.Errorf("%s with no maxChanges costs %d, want %d", name, got, want)
			}
			if got, want := cost(name, pol, `{"maxChanges":1073741824}`), unitsFor(KeyQueryMaxLimit); got != want {
				t.Errorf("%s with a huge maxChanges costs %d, want %d", name, got, want)
			}
		}
	}
	if queries < 5 || changes < 7 {
		t.Fatalf("found %d /query and %d /changes key-reachable methods: the walk is not looking at keyPolicies", queries, changes)
	}
}

func TestKeyChangesLimitAndWindow(t *testing.T) {
	ptr := func(n int) *int { return &n }
	for _, tc := range []struct {
		in   *int
		want int
	}{{nil, KeyQueryDefaultLimit}, {ptr(7), 7}, {ptr(KeyQueryMaxLimit + 1), KeyQueryMaxLimit}, {ptr(1 << 30), KeyQueryMaxLimit}} {
		if got := keyChangesLimit(tc.in); got != tc.want {
			t.Errorf("keyChangesLimit(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
	ids := make([]ulid.ULID, 5)
	for i := range ids {
		ids[i] = ulid.ULID{byte(i)}
	}
	for _, tc := range []struct {
		pos, limit, wantPos, wantLen int
	}{{0, 2, 0, 2}, {4, 2, 4, 1}, {5, 2, 5, 0}, {9, 2, 5, 0}, {-2, 5, 3, 2}, {-9, 1, 0, 1}} {
		page, pos := keyQueryWindow("Label/query", ids, tc.pos, ptr(tc.limit))
		if pos != tc.wantPos || len(page) != tc.wantLen {
			t.Errorf("window(pos %d, limit %d) = pos %d, %d ids; want %d, %d", tc.pos, tc.limit, pos, len(page), tc.wantPos, tc.wantLen)
		}
	}
	if page, _ := keyQueryWindow("Label/query", make([]ulid.ULID, 2*KeyQueryMaxLimit), 0, nil); len(page) != KeyQueryDefaultLimit {
		t.Errorf("a window with no limit has %d ids, want %d", len(page), KeyQueryDefaultLimit)
	}
	if got := keyServerLimit("Contact/query", ptr(KeyQueryMaxLimit)); got == nil || *got != store.MaxContactQueryLimit {
		t.Errorf("Contact/query's server limit for %d = %v, want its own maximum", KeyQueryMaxLimit, got)
	}
	if got := keyServerLimit("Mailbox/query", ptr(7)); got != nil {
		t.Errorf("a limit within the maximum is echoed as %v, want nil", *got)
	}
}
