// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// SenderStats/get (#53) through the JMAP surface: the ranking's keys are the
// ones the clean-up view's action queries search for — a list row's key with
// `list:`, a domain row's with `from:@`, an address row's with `from:` — and
// the arguments it refuses.

import (
	"context"
	"testing"
)

func TestSenderStatsKeysAreWhatTheActionsSearchFor(t *testing.T) {
	x := newOpsFixture(t)
	ranking := func(group string) []map[string]any {
		res := call(t, x.h.api.senderStatsGet, x.a.UserID, map[string]any{"groupBy": group})
		out := []map[string]any{}
		for _, it := range asArr(res["list"]) {
			out = append(out, it.(map[string]any))
		}
		return out
	}

	lists := ranking("list")
	if len(lists) != 1 {
		t.Fatalf("list ranking = %v, want the one list in the fixture", lists)
	}
	key := lists[0]["key"].(string)
	if key != "billing.lists.shop.example" {
		t.Errorf("list key = %q, want billing.lists.shop.example (search's normalisation)", key)
	}
	if lists[0]["unsubscribeEmailId"] != x.ids["list"].String() {
		t.Errorf("unsubscribeEmailId = %v, want the list message %s", lists[0]["unsubscribeEmailId"], x.ids["list"])
	}
	x.expect(t, "list:"+key, "list")

	// Every one of a's messages is from friend@example.com: one address row,
	// one domain row, and each row's COUNT is what its action query finds —
	// trash and junk excluded from both by the same rules, including the
	// fixture's message that sits in a Junk FOLDER without the verdict (which
	// a first version of the ranking counted and the search did not).
	addrs := ranking("address")
	if len(addrs) != 1 || addrs[0]["key"] != "friend@example.com" {
		t.Fatalf("address ranking = %v", addrs)
	}
	doms := ranking("domain")
	if len(doms) != 1 || doms[0]["key"] != "example.com" {
		t.Fatalf("domain ranking = %v", doms)
	}
	for _, c := range []struct {
		row   map[string]any
		query string
	}{
		{addrs[0], "from:" + addrs[0]["key"].(string)},
		{doms[0], "from:@" + doms[0]["key"].(string)},
	} {
		found := x.query(t, x.a.UserID, c.query, nil).Total
		if got := int(c.row["total"].(float64)); got != found || found < len(everyday) {
			t.Errorf("%s: the ranking counts %d, the search finds %d (at least %d expected)", c.query, got, found, len(everyday))
		}
	}
}

// The wire carries the server's actionable verdict (#53 security review): a
// From with no local part is shown, and marked so no client acts on it.
func TestSenderStatsMarksMalformedKeysNonActionable(t *testing.T) {
	x := newOpsFixture(t)
	mustExecAPI(t, x.h, `UPDATE app.messages SET from_address = 'example.com' WHERE id = $1`, x.ids["plain"].String())
	res := call(t, x.h.api.senderStatsGet, x.a.UserID, map[string]any{"groupBy": "address"})
	seen := map[string]any{}
	for _, it := range asArr(res["list"]) {
		m := it.(map[string]any)
		seen[m["key"].(string)] = m["actionable"]
	}
	if seen["example.com"] != false {
		t.Errorf("example.com row actionable = %v, want false", seen["example.com"])
	}
	if seen["friend@example.com"] != true {
		t.Errorf("friend@example.com row actionable = %v, want true", seen["friend@example.com"])
	}
}

func TestSenderStatsRefusesBadArguments(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	for _, args := range []map[string]any{
		{"groupBy": "subject"},
		{"sort": "random"},
		{"sinceDays": 0},
		{"limit": 0},
		{"limit": 500},
		{"position": -1},
	} {
		if _, mErr := h.api.senderStatsGet(context.Background(), a.UserID, mustJSON(t, args)); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("SenderStats/get %v: got %+v, want invalidArguments", args, mErr)
		}
	}
}
