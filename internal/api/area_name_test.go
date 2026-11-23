// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"
)

// reachesStore runs fn and reports whether it got past request validation: the
// tests below pass a nil store, so a request that validates fails by touching
// it (a panic), while a refused one returns a SetError first.
func reachesStore(fn func() *SetError) (refused *SetError, reached bool) {
	defer func() {
		if recover() != nil {
			reached = true
		}
	}()
	return fn(), false
}

// Area/set refuses a name with a control or invisible formatting character on
// create and on update, as invalidProperties naming "name", and still accepts
// an international name with an emoji (the positive control: without it the
// refusal could be refusing everything).
func TestAreaSetRefusesUnsafeNames(t *testing.T) {
	for _, bad := range []string{"Work\u001b[31m", "Re\u202evil", "zero\u200bwidth", "tab\tname"} {
		raw, _ := json.Marshal(map[string]any{"name": bad})
		pos := 0
		_, _, serr := createArea(context.Background(), nil, raw, &pos)
		if serr == nil || serr.Type != setInvalidProps || len(serr.Properties) != 1 || serr.Properties[0] != "name" {
			t.Errorf("create %q: %+v, want invalidProperties on name", bad, serr)
		}
		serr = updateArea(context.Background(), nil, ulid.Make(), raw)
		if serr == nil || serr.Type != setInvalidProps || len(serr.Properties) != 1 || serr.Properties[0] != "name" {
			t.Errorf("update %q: %+v, want invalidProperties on name", bad, serr)
		}
	}
	good := "Café 日本 ✉️ Заметки"
	raw, _ := json.Marshal(map[string]any{"name": good})
	_, reached := reachesStore(func() *SetError {
		pos := 0
		_, _, serr := createArea(context.Background(), nil, raw, &pos)
		return serr
	})
	if !reached {
		t.Errorf("create %q was refused by validation", good)
	}
	_, reached = reachesStore(func() *SetError { return updateArea(context.Background(), nil, ulid.Make(), raw) })
	if !reached {
		t.Errorf("update %q was refused by validation", good)
	}
}

// A day-one plan naming an area or label with an ESC sequence or a bidi
// override is refused when the plan is parsed, which is what both the preview
// and the apply do. A plain international name passes (positive control).
//
// NEGATIVE CONTROL: with the areaName check removed from parseSetupPlan the
// area cases go red (run and recorded in the PR).
func TestSetupPlanRefusesUnsafeNames(t *testing.T) {
	mailbox := ulid.Make().String()
	plan := func(area, label string) setupPlanArgs {
		return setupPlanArgs{Folders: []setupFolderChoiceArgs{{MailboxID: mailbox, Action: "bringToDesk", AreaName: area, LabelName: label}}}
	}
	for _, bad := range []string{"Work\u001b[2J", "A\u202eB"} {
		if _, merr := parseSetupPlan(plan(bad, "")); merr == nil {
			t.Errorf("area name %q was accepted", bad)
		}
		if _, merr := parseSetupPlan(plan("", bad)); merr == nil {
			t.Errorf("label name %q was accepted", bad)
		}
	}
	if _, merr := parseSetupPlan(plan("Café 日本 ✉️", "Заметки")); merr != nil {
		t.Errorf("a plain international name was refused: %v", merr.Description)
	}
	if _, merr := parseSetupPlan(plan("", "")); merr != nil {
		t.Errorf("no names at all is a legitimate plan: %v", merr.Description)
	}
}
