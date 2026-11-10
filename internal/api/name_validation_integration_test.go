// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The saved-name refusals (#148) end to end, with the POSITIVE CONTROL that
// international names still save: without it every refusal could be a validator
// that refuses everything. Rules and API keys are the two user-typed names.

import "testing"

var internationalNames = []string{"قواعد البريد", "請求書フィルター", "Zoë's rules", "mail \U0001F4EC", "école"}

func TestRuleAndKeyNamesRefuseUnsafeAndAcceptInternational(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	ruleCreates := map[string]any{}
	keyCreates := map[string]any{}
	for i, n := range internationalNames {
		id := string(rune('a' + i))
		ruleCreates["ok"+id] = kindRuleCreate(n, nil)
		keyCreates["ok"+id] = map[string]any{"name": n, "scopes": []string{"mail:read"}}
	}
	for label, n := range unsafeNames {
		ruleCreates["bad "+label] = kindRuleCreate(n, nil)
		keyCreates["bad "+label] = map[string]any{"name": n, "scopes": []string{"mail:read"}}
	}
	for what, res := range map[string]map[string]any{
		"rule": call(t, h.api.ruleSet, a.UserID, map[string]any{"create": ruleCreates}),
		"key":  call(t, h.api.apiKeySet, a.UserID, map[string]any{"create": keyCreates}),
	} {
		created, _ := res["created"].(map[string]any)
		notCreated, _ := res["notCreated"].(map[string]any)
		for i := range internationalNames {
			id := "ok" + string(rune('a'+i))
			if created[id] == nil {
				t.Errorf("%s: international name %q refused: %v", what, internationalNames[i], notCreated[id])
			}
		}
		for label := range unsafeNames {
			se, _ := notCreated["bad "+label].(map[string]any)
			if se == nil || se["type"] != setInvalidProps {
				t.Errorf("%s: %s accepted or wrong error: %v", what, label, se)
				continue
			}
			if props, _ := se["properties"].([]any); len(props) != 1 || props[0] != "name" {
				t.Errorf("%s: %s names %v, want [name]", what, label, se["properties"])
			}
			if created["bad "+label] != nil {
				t.Errorf("%s: %s was created", what, label)
			}
		}
	}
}
