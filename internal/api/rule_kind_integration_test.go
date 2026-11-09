// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// A rule can be bound to a message kind through Rule/set: the value round-trips
// through Rule/get (null when unbound), an unknown kind is refused with an
// invalidProperties SetError that names the property and persists nothing, null
// clears the binding, and a bound rule cannot be applied to existing mail.

import (
	"testing"
	"time"
)

func kindRuleCreate(name string, kind any) map[string]any {
	c := map[string]any{
		"name":       name,
		"conditions": map[string]any{"field": "from", "op": "exists"},
		"actions":    []any{map[string]any{"type": "markPriority"}},
	}
	if kind != nil {
		c["kind"] = kind
	}
	return c
}

func TestRuleSetKindRoundTripAndInvalidKindIsRefused(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.ruleSet, a.UserID, map[string]any{"create": map[string]any{
		"bound":   kindRuleCreate("kind-bound", "promotional"),
		"unbound": kindRuleCreate("kind-unbound", nil),
		"bad":     kindRuleCreate("kind-bad", "spam"),
	}})
	created, _ := res["created"].(map[string]any)
	notCreated, _ := res["notCreated"].(map[string]any)

	// The invalid kind is refused, names the property, and nothing was stored.
	se, ok := notCreated["bad"].(map[string]any)
	if !ok {
		t.Fatalf("a rule with kind \"spam\" was not refused: %v", res)
	}
	if se["type"] != setInvalidProps {
		t.Errorf("SetError type = %v, want %s", se["type"], setInvalidProps)
	}
	if props, _ := se["properties"].([]any); len(props) != 1 || props[0] != "kind" {
		t.Errorf("SetError properties = %v, want [kind]", se["properties"])
	}
	if _, stored := created["bad"]; stored {
		t.Error("the invalid-kind rule was created")
	}

	bound, _ := created["bound"].(map[string]any)
	unbound, _ := created["unbound"].(map[string]any)
	if bound == nil || unbound == nil {
		t.Fatalf("valid rules not created: %v", res)
	}
	if bound["kind"] != "promotional" {
		t.Errorf("created bound rule kind = %v, want promotional", bound["kind"])
	}
	if k, present := unbound["kind"]; !present || k != nil {
		t.Errorf("unbound rule kind = %v (present=%v), want null", k, present)
	}

	boundID, _ := bound["id"].(string)
	got := call(t, h.api.ruleGet, a.UserID, map[string]any{"ids": []string{boundID}})
	list, _ := got["list"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["kind"] != "promotional" {
		t.Fatalf("Rule/get did not return the kind: %v", got)
	}

	// An update to an unknown kind is refused and leaves the binding alone.
	up := call(t, h.api.ruleSet, a.UserID, map[string]any{"update": map[string]any{
		boundID: map[string]any{"kind": "Promotional"},
	}})
	if ne, _ := up["notUpdated"].(map[string]any); ne[boundID] == nil {
		t.Fatalf("an update to an unknown kind was accepted: %v", up)
	}
	got = call(t, h.api.ruleGet, a.UserID, map[string]any{"ids": []string{boundID}})
	if got["list"].([]any)[0].(map[string]any)["kind"] != "promotional" {
		t.Error("a refused update changed the binding")
	}

	// A valid change takes, and null clears it.
	call(t, h.api.ruleSet, a.UserID, map[string]any{"update": map[string]any{boundID: map[string]any{"kind": "list"}}})
	got = call(t, h.api.ruleGet, a.UserID, map[string]any{"ids": []string{boundID}})
	if got["list"].([]any)[0].(map[string]any)["kind"] != "list" {
		t.Error("changing the kind did not take")
	}
	call(t, h.api.ruleSet, a.UserID, map[string]any{"update": map[string]any{boundID: map[string]any{"kind": nil}}})
	got = call(t, h.api.ruleGet, a.UserID, map[string]any{"ids": []string{boundID}})
	if got["list"].([]any)[0].(map[string]any)["kind"] != nil {
		t.Error("null did not clear the binding")
	}
}

// Applying a kind-bound rule to existing mail is refused: the scan has no kind to
// test, so it would label everything the conditions match.
func TestRuleApplyRefusesAKindBoundRule(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	res := call(t, h.api.ruleSet, a.UserID, map[string]any{"create": map[string]any{"r": map[string]any{
		"name":       "kind-bound-apply",
		"kind":       "promotional",
		"conditions": map[string]any{"field": "fromDomain", "op": "equals", "value": "wayfair.example"},
		"actions":    []any{map[string]any{"type": "addLabel", "label": promotions.String()}},
	}}})
	obj, _ := res["created"].(map[string]any)["r"].(map[string]any)
	if obj == nil {
		t.Fatalf("rule not created: %v", res)
	}
	msg := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)

	mErr := callErr(t, h.api.ruleApply, a.UserID, map[string]any{"ruleId": obj["id"]})
	if mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("applying a kind-bound rule: %+v, want invalidArguments", mErr)
	}
	if by := h.labelledBy(t, msg, promotions); by != "" {
		t.Errorf("the refused apply labelled a message (applied_by %q)", by)
	}
}
