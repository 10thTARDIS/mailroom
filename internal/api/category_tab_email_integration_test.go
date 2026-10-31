// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Email.categoryTab and the inTab query filter over the wire (#228, ADR-022).
//
// The claims worth pinning are the ones a client would otherwise get wrong:
//
//  1. The property is OPT-IN. It costs a join and a five-row read, and a list
//     view that never names it must not pay for either.
//  2. NULL means "nothing has decided", NOT "primary". Such a message is also
//     invisible to an inTab query, so a client that rendered null as Primary
//     would put it in a tab whose own list does not contain it.
//  3. A DISABLED tab is refused rather than answered with an empty list, because
//     an empty list is a false statement about the mailbox.
//  4. Asking for Primary includes the disabled tabs' mail, which is where
//     tabs.Fold sends it.

import (
	"context"
	"testing"

	"mailroom/internal/tabs"
)

func TestEmailCategoryTabIsOptInAndDistinguishesUnclassified(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// The fixture message has a label; map that label to a tab so the message
	// resolves through the MAPPING half.
	labels := call(t, h.api.labelGet, a.UserID, map[string]any{})
	list, _ := labels["list"].([]any)
	if len(list) == 0 {
		t.Fatal("fixture has no label to map")
	}
	first, _ := list[0].(map[string]any)
	labelID, _ := first["id"].(string)
	call(t, h.api.labelSet, a.UserID, map[string]any{
		"update": map[string]any{labelID: map[string]any{"tab": "promotions"}},
	})

	// Not requested → absent. A list view must not pay for it.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "subject"},
	})
	objs, _ := got["list"].([]any)
	if len(objs) != 1 {
		t.Fatalf("Email/get returned %d objects", len(objs))
	}
	obj, _ := objs[0].(map[string]any)
	if _, present := obj["categoryTab"]; present {
		t.Error("categoryTab was returned without being asked for; it must be opt-in")
	}

	// Requested → the mapping's answer, flagged as coming from the mapping.
	got = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "categoryTab"},
	})
	objs, _ = got["list"].([]any)
	obj, _ = objs[0].(map[string]any)
	ct, ok := obj["categoryTab"].(map[string]any)
	if !ok {
		t.Fatalf("categoryTab = %#v, want an object", obj["categoryTab"])
	}
	if ct["tab"] != "promotions" {
		t.Errorf("tab = %v, want promotions", ct["tab"])
	}
	if ct["fromMapping"] != true {
		t.Errorf("fromMapping = %v, want true — a mapped label decided this", ct["fromMapping"])
	}

	// Unmap the label. With no mapped label and no stored verdict, NOTHING has
	// decided, and the property must be null rather than "primary".
	call(t, h.api.labelSet, a.UserID, map[string]any{
		"update": map[string]any{labelID: map[string]any{"tab": nil}},
	})
	got = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "categoryTab"},
	})
	objs, _ = got["list"].([]any)
	obj, _ = objs[0].(map[string]any)
	if obj["categoryTab"] != nil {
		t.Errorf("categoryTab = %#v, want null — nothing has classified this message, and "+
			"rendering it as primary would put it in a tab whose own list excludes it",
			obj["categoryTab"])
	}
}

func TestEmailQueryInTabRefusesADisabledTabAndFoldsIntoPrimary(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// Map the fixture's label to Promotions, then switch Promotions OFF.
	labels := call(t, h.api.labelGet, a.UserID, map[string]any{})
	list, _ := labels["list"].([]any)
	first, _ := list[0].(map[string]any)
	labelID, _ := first["id"].(string)
	call(t, h.api.labelSet, a.UserID, map[string]any{
		"update": map[string]any{labelID: map[string]any{"tab": "promotions"}},
	})

	// While enabled, the message is in Promotions and NOT in Primary.
	inPromos := queryTabIDs(t, h, a, "promotions")
	if !containsID(inPromos, a.MessageID.String()) {
		t.Fatalf("message not in promotions while that tab is enabled: %v", inPromos)
	}
	if inPrimary := queryTabIDs(t, h, a, "primary"); containsID(inPrimary, a.MessageID.String()) {
		t.Errorf("message appeared in primary while promotions is enabled: %v", inPrimary)
	}

	strip := tabsByName(t, call(t, h.api.categoryTabGet, a.UserID, map[string]any{}))
	promoID, _ := strip[string(tabs.Promotions)]["id"].(string)
	call(t, h.api.categoryTabSet, a.UserID, map[string]any{
		"update": map[string]any{promoID: map[string]any{"enabled": false}},
	})

	// Asking for the disabled tab is REFUSED, not answered with an empty list.
	_, mErr := h.api.emailQuery(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"filter": map[string]any{"inTab": "promotions"},
	}))
	if mErr == nil {
		t.Error("querying a disabled tab succeeded; an empty result would read as " +
			"'this tab has no mail', which is false")
	} else if mErr.Type != errInvalidArguments {
		t.Errorf("error type = %q, want %q", mErr.Type, errInvalidArguments)
	}

	// And its mail now folds into Primary.
	if inPrimary := queryTabIDs(t, h, a, "primary"); !containsID(inPrimary, a.MessageID.String()) {
		t.Errorf("message did not fold into primary after its tab was disabled: %v", inPrimary)
	}
}

// TestEmailQueryInTabRejectsAnUnknownTab — a filter naming something outside the
// vocabulary must fail, not return everything or nothing. Silently ignoring an
// unknown condition returns a SUPERSET of what was asked for and the client
// cannot tell (the #20 lesson this filter allowlist exists for).
func TestEmailQueryInTabRejectsAnUnknownTab(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	_, mErr := h.api.emailQuery(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"filter": map[string]any{"inTab": "inbox"},
	}))
	if mErr == nil {
		t.Fatal("an unknown tab was accepted")
	}
	if mErr.Type != errInvalidArguments {
		t.Errorf("error type = %q, want %q", mErr.Type, errInvalidArguments)
	}
}

// TestEmailQueryChangesRefusesATabFilter. A tab's membership can change with no
// Email change_log row behind it — a re-pointed label, a classifier version bump
// — so a delta computed from that log would silently omit mail that moved tabs.
// cannotCalculateChanges is the honest answer, and it comes from `inTab` being
// absent from deltaStableConditions.
func TestEmailQueryChangesRefusesATabFilter(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	_, mErr := h.api.emailQueryChanges(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"filter":          map[string]any{"inTab": "updates"},
		"sinceQueryState": "s:1",
	}))
	if mErr == nil || mErr.Type != errCannotCalculateChanges {
		t.Errorf("queryChanges with inTab = %v, want %q — a tab filter is not delta-stable",
			mErr, errCannotCalculateChanges)
	}
}

func queryTabIDs(t *testing.T, h *apiHarness, a apiFixture, tab string) []string {
	t.Helper()
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inTab": tab},
	})
	var out []string
	ids, _ := res["ids"].([]any)
	for _, id := range ids {
		if s, ok := id.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
