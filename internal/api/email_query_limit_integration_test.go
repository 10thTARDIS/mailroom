// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Email/query's `limit: 0` (#469, RFC 8620 §5.5). "limit" absent/null means
// the server chooses a page size; limit: 0 is a client asking for ZERO ids —
// the way to ask only for `total` and `queryState` without paying for a page.
// The bug: store.EmailQuery.Limit is an int, so an explicit 0 was
// indistinguishable from "no limit sent" once it crossed the API/store
// boundary, and the store only emitted a SQL LIMIT clause when Limit > 0 —
// so limit: 0 silently returned every matching id.

import "testing"

func TestEmailQueryLimitZero(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// seedTwo's fixture already leaves one live message (a.MessageID) in
	// a.InboxID; insertInboxMessage adds a second, so the mailbox holds
	// exactly two — enough to distinguish "zero ids" from "one id" from
	// "every id".
	insertInboxMessage(t, h.super, a, "limit0", 2)

	filter := map[string]any{"inMailbox": a.InboxID.String()}

	// limit: 0, explicit: zero ids, but total and queryState are computed
	// exactly as for any other query — they must not read as "no query ran".
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter":         filter,
		"limit":          0,
		"calculateTotal": true,
	})
	ids, ok := res["ids"].([]any)
	if !ok || len(ids) != 0 {
		t.Errorf("limit:0 ids = %#v, want []", res["ids"])
	}
	if total, ok := res["total"].(float64); !ok || total != 2 {
		t.Errorf("limit:0 total = %v, want 2", res["total"])
	}
	if qs, _ := res["queryState"].(string); qs == "" {
		t.Errorf("limit:0 queryState = %q, want non-empty", qs)
	}

	// No limit at all — the argument genuinely absent — returns every
	// matching id. This is the case limit:0 must NOT be confused with.
	resAll := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": filter,
	})
	idsAll, _ := resAll["ids"].([]any)
	if len(idsAll) != 2 {
		t.Errorf("no-limit ids = %#v, want 2 entries", resAll["ids"])
	}

	// limit: 1 returns exactly one id, with the same total as the unbounded
	// and zero-limit queries — the window narrows, the count of matches does
	// not.
	res1 := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter":         filter,
		"limit":          1,
		"calculateTotal": true,
	})
	ids1, _ := res1["ids"].([]any)
	if len(ids1) != 1 {
		t.Errorf("limit:1 ids = %#v, want exactly 1", res1["ids"])
	}
	if total, ok := res1["total"].(float64); !ok || total != 2 {
		t.Errorf("limit:1 total = %v, want 2", res1["total"])
	}
}
