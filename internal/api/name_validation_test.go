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

// A user-supplied name is refused when it carries a control, bidi, format or
// separator character (#148). The refusal happens before any database access,
// so these run without a database; the positive control for international
// names is in the integration suite, where a rule and a key are really saved.

var unsafeNames = map[string]string{
	"C0 newline": "a\nb", "C0 ESC": "\x1b[2Jx", "C1 NEL": "a\u0085b", "bidi override": "a‮b",
	"bidi isolate": "a⁦b", "zero width": "a​b", "line separator": "a b", "trailing paragraph separator": "ab ",
}

func TestRuleSetRefusesUnsafeNames(t *testing.T) {
	ctx := context.Background()
	for label, name := range unsafeNames {
		raw, _ := json.Marshal(map[string]any{"name": name, "conditions": map[string]any{}, "actions": []any{}})
		_, _, se := createRule(ctx, nil, raw)
		if se == nil || se.Type != setInvalidProps || len(se.Properties) != 1 || se.Properties[0] != "name" ||
			!strings.Contains(se.Description, "name") {
			t.Errorf("create, %s: %+v", label, se)
		}
		patch, _ := json.Marshal(map[string]any{"name": name})
		se = updateRule(ctx, nil, ulid.Make(), patch)
		if se == nil || se.Type != setInvalidProps || len(se.Properties) != 1 || se.Properties[0] != "name" {
			t.Errorf("update, %s: %+v", label, se)
		}
	}
}

func TestApiKeySetRefusesUnsafeNames(t *testing.T) {
	ctx := context.Background()
	for label, name := range unsafeNames {
		n := name
		se := applyKeySettings(ctx, nil, &store.APIKey{}, keySettings{Name: &n}, nil)
		if se == nil || se.Type != setInvalidProps || len(se.Properties) != 1 || se.Properties[0] != "name" {
			t.Errorf("%s: %+v", label, se)
		}
	}
}
