// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the Rule/* JMAP surface (urn:mailroom:rules, mailroom#32):
// a Rule/set create round-trips through Rule/get, and a rule with an uncompilable
// regex is refused with a clean invalidProperties SetError (never a 500, never
// persisted). Cross-user isolation for Rule/* is proven in rls_integration_test.go.

import (
	"testing"
)

func TestRuleSetCreateGetRoundTrip(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.ruleSet, a.UserID, map[string]any{
		"create": map[string]any{
			"r1": map[string]any{
				"name":     "from-bob",
				"priority": 42,
				"conditions": map[string]any{
					"field": "from", "op": "equals", "value": "bob@example.com",
				},
				"actions": []any{
					map[string]any{"type": "addLabel", "label": a.LabelID.String()},
				},
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["r1"].(map[string]any)
	if !ok {
		t.Fatalf("r1 not created: %v", res["notCreated"])
	}
	id, _ := obj["id"].(string)
	if id == "" {
		t.Fatalf("created rule has no id: %v", obj)
	}

	got := call(t, h.api.ruleGet, a.UserID, map[string]any{"ids": []string{id}})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Rule/get returned %d objects, want 1", len(list))
	}
	rule, _ := list[0].(map[string]any)
	if rule["name"] != "from-bob" {
		t.Errorf("name = %v, want from-bob", rule["name"])
	}
	if rule["priority"] != float64(42) {
		t.Errorf("priority = %v, want 42", rule["priority"])
	}
	if rule["enabled"] != true {
		t.Errorf("enabled = %v, want true (default)", rule["enabled"])
	}
	conds, _ := rule["conditions"].(map[string]any)
	if conds["field"] != "from" || conds["value"] != "bob@example.com" {
		t.Errorf("conditions round-trip = %v", conds)
	}
}

func TestRuleSetBadRegexIsSetError(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.ruleSet, a.UserID, map[string]any{
		"create": map[string]any{
			"bad": map[string]any{
				"name": "bad-regex",
				"conditions": map[string]any{
					"field": "subject", "op": "matches", "value": "([unclosed",
				},
				"actions": []any{map[string]any{"type": "markPriority"}},
			},
		},
	})
	if created, _ := res["created"].(map[string]any); len(created) != 0 {
		t.Errorf("bad-regex rule was created: %v", created)
	}
	notCreated, _ := res["notCreated"].(map[string]any)
	se, ok := notCreated["bad"].(map[string]any)
	if !ok {
		t.Fatalf("bad rule not in notCreated: %v", res)
	}
	if se["type"] != setInvalidProps {
		t.Errorf("SetError type = %v, want %s", se["type"], setInvalidProps)
	}
}

func TestRuleSetUnsupportedActionIsSetError(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.ruleSet, a.UserID, map[string]any{
		"create": map[string]any{
			"j": map[string]any{
				"name":       "junker",
				"conditions": map[string]any{"field": "from", "op": "exists"},
				"actions":    []any{map[string]any{"type": "junk"}},
			},
		},
	})
	notCreated, _ := res["notCreated"].(map[string]any)
	se, ok := notCreated["j"].(map[string]any)
	if !ok {
		t.Fatalf("junk rule not refused: %v", res)
	}
	if se["type"] != setInvalidProps {
		t.Errorf("SetError type = %v, want %s", se["type"], setInvalidProps)
	}
}
