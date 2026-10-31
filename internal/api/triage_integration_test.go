// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration coverage for the Triage control methods (#34) against a real
// Postgres with RLS enforced. The cross-user isolation of these methods is proven
// in rls_integration_test.go; this file proves the FUNCTIONAL behavior on the
// caller's OWN model and messages: read state, reset, rollback, and one-click
// correction.

import (
	"context"
	"testing"

	"mailroom/internal/store"
)

// fakeMasterKeys is a crypto.KeyProvider with a fixed 32-byte master key. The key
// hierarchy is tested in internal/crypto; the registry only needs A key here, to
// seal the baseline artifact a reset snapshots. It is shared with newRLSHarness
// (rls_integration_test.go), which wires the registry the Triage controls use.
type fakeMasterKeys struct{}

func (fakeMasterKeys) MasterKey(context.Context) ([]byte, error) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key, nil
}

// seedTriageUser gives us one user with the standard fixtures (a message, an
// active priority-model v1) via the shared RLS seeder.
func seedTriageUser(t *testing.T, h *rlsHarness) rlsUser {
	t.Helper()
	return seedRLSUser(t, h.super, "triage-"+store.NewULID().String())
}

func TestTriageGetReturnsSeededVersions(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)

	res, mErr := invokeAs(t, h, "Triage/get", u.UserID, map[string]any{})
	if mErr != nil {
		t.Fatalf("Triage/get: %+v", mErr)
	}
	if av, _ := res["activeVersion"].(float64); av != 1 {
		t.Errorf("activeVersion = %v, want 1", res["activeVersion"])
	}
	versions := asArr(res["versions"])
	if len(versions) != 1 {
		t.Fatalf("versions len = %d, want 1", len(versions))
	}
	v0, _ := versions[0].(map[string]any)
	if ver, _ := v0["version"].(float64); ver != 1 {
		t.Errorf("versions[0].version = %v, want 1", v0["version"])
	}
	if active, _ := v0["active"].(bool); !active {
		t.Errorf("versions[0].active = %v, want true", v0["active"])
	}
	if v0["parentVersion"] != nil {
		t.Errorf("versions[0].parentVersion = %v, want null (a root)", v0["parentVersion"])
	}
}

func TestTriageSetResetActivatesFreshVersion(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)

	res, mErr := invokeAs(t, h, "Triage/set", u.UserID, map[string]any{"reset": true})
	if mErr != nil {
		t.Fatalf("Triage/set reset: %+v", mErr)
	}
	// The reset response is the new state: a fresh active version, lineage grown.
	if av, _ := res["activeVersion"].(float64); av != 2 {
		t.Errorf("activeVersion after reset = %v, want 2", res["activeVersion"])
	}
	if vs := asArr(res["versions"]); len(vs) != 2 {
		t.Errorf("versions after reset = %d, want 2", len(vs))
	}
	// The fresh baseline broke lineage: its parentVersion is null even though v1
	// exists. Confirm via Triage/get too.
	got, mErr := invokeAs(t, h, "Triage/get", u.UserID, map[string]any{})
	if mErr != nil {
		t.Fatalf("Triage/get: %+v", mErr)
	}
	if av, _ := got["activeVersion"].(float64); av != 2 {
		t.Errorf("Triage/get activeVersion = %v, want 2", got["activeVersion"])
	}
	for _, item := range asArr(got["versions"]) {
		v, _ := item.(map[string]any)
		if ver, _ := v["version"].(float64); ver == 2 {
			if v["parentVersion"] != nil {
				t.Errorf("reset v2 parentVersion = %v, want null (lineage broken)", v["parentVersion"])
			}
			if active, _ := v["active"].(bool); !active {
				t.Errorf("reset v2 should be active")
			}
		}
	}
}

func TestTriageSetRollbackReactivatesOlder(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)

	// Reset first so there are two versions (v1 seeded, v2 fresh + active).
	if _, mErr := invokeAs(t, h, "Triage/set", u.UserID, map[string]any{"reset": true}); mErr != nil {
		t.Fatalf("Triage/set reset: %+v", mErr)
	}
	// Roll back to v1.
	res, mErr := invokeAs(t, h, "Triage/set", u.UserID, map[string]any{"rollbackTo": 1})
	if mErr != nil {
		t.Fatalf("Triage/set rollbackTo 1: %+v", mErr)
	}
	if av, _ := res["activeVersion"].(float64); av != 1 {
		t.Errorf("activeVersion after rollback = %v, want 1", res["activeVersion"])
	}
	// Rollback deletes nothing: both versions still present.
	if vs := asArr(res["versions"]); len(vs) != 2 {
		t.Errorf("versions after rollback = %d, want 2 (rollback never deletes)", len(vs))
	}
	// The DB agrees: exactly one active row, at v1.
	var n int
	superScan(t, h, &n, `SELECT count(*) FROM app.ml_models WHERE user_id=$1 AND active AND version=1`, u.UserID.String())
	if n != 1 {
		t.Errorf("active v1 rows = %d, want 1", n)
	}
}

func TestTriageSetRollbackBogusIsMethodError(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)

	_, mErr := invokeAs(t, h, "Triage/set", u.UserID, map[string]any{"rollbackTo": 999})
	if mErr == nil {
		t.Fatal("Triage/set rollbackTo 999: want a MethodError, got nil")
	}
	if mErr.Type != errInvalidArguments {
		t.Errorf("rollbackTo bogus error type = %q, want %q", mErr.Type, errInvalidArguments)
	}
	// The active model is unchanged: still the seeded v1.
	var n int
	superScan(t, h, &n, `SELECT count(*) FROM app.ml_models WHERE user_id=$1 AND active AND version=1`, u.UserID.String())
	if n != 1 {
		t.Errorf("active v1 rows after bogus rollback = %d, want 1 (unchanged)", n)
	}
}

func TestTriageSetRejectsNeitherOrBoth(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)

	// Neither (empty, and reset:false), and both, are all invalidArguments.
	for _, args := range []map[string]any{
		{},
		{"reset": false},
		{"reset": true, "rollbackTo": 1},
	} {
		_, mErr := invokeAs(t, h, "Triage/set", u.UserID, args)
		if mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("Triage/set %v: got %+v, want invalidArguments", args, mErr)
		}
	}
}

func TestTriageCorrectOwnMessageEmits(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)

	res, mErr := invokeAs(t, h, "Triage/correct", u.UserID, map[string]any{
		"corrections": map[string]any{u.MessageID.String(): "important"},
	})
	if mErr != nil {
		t.Fatalf("Triage/correct: %+v", mErr)
	}
	if !strInArr(res["corrected"], u.MessageID.String()) {
		t.Errorf("own message not in corrected: %v", res["corrected"])
	}
	if mapHasKey(res, "notCorrected", u.MessageID.String()) {
		t.Errorf("own message wrongly in notCorrected: %v", res["notCorrected"])
	}
	// Exactly one feedback_events row, with the mark_important token.
	var action string
	superScan(t, h, &action, `SELECT action FROM app.feedback_events WHERE message_id=$1`, u.MessageID.String())
	if action != feedbackMarkImportant {
		t.Errorf("feedback action = %q, want %q", action, feedbackMarkImportant)
	}

	// notImportant maps to the negative token.
	u2 := seedTriageUser(t, h)
	if _, mErr := invokeAs(t, h, "Triage/correct", u2.UserID, map[string]any{
		"corrections": map[string]any{u2.MessageID.String(): "notImportant"},
	}); mErr != nil {
		t.Fatalf("Triage/correct notImportant: %+v", mErr)
	}
	superScan(t, h, &action, `SELECT action FROM app.feedback_events WHERE message_id=$1`, u2.MessageID.String())
	if action != feedbackMarkNotImportant {
		t.Errorf("feedback action = %q, want %q", action, feedbackMarkNotImportant)
	}
}

func TestTriageCorrectUnknownMessageAndValue(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)

	// (1) A non-existent message id: notCorrected/notFound, and NO feedback row.
	ghost := store.NewULID()
	res, mErr := invokeAs(t, h, "Triage/correct", u.UserID, map[string]any{
		"corrections": map[string]any{ghost.String(): "important"},
	})
	if mErr != nil {
		t.Fatalf("Triage/correct ghost: %+v", mErr)
	}
	if got := res["notCorrected"].(map[string]any)[ghost.String()]; got != triageReasonNotFound {
		t.Errorf("ghost reason = %v, want %q", got, triageReasonNotFound)
	}
	if strInArr(res["corrected"], ghost.String()) {
		t.Errorf("ghost id wrongly reported corrected")
	}
	var n int
	superScan(t, h, &n, `SELECT count(*) FROM app.feedback_events WHERE message_id=$1`, ghost.String())
	if n != 0 {
		t.Errorf("feedback rows for a ghost message = %d, want 0", n)
	}

	// (2) An unknown correction value on the caller's OWN message: invalidArguments,
	// no emit.
	res, mErr = invokeAs(t, h, "Triage/correct", u.UserID, map[string]any{
		"corrections": map[string]any{u.MessageID.String(): "meh"},
	})
	if mErr != nil {
		t.Fatalf("Triage/correct bad value: %+v", mErr)
	}
	if got := res["notCorrected"].(map[string]any)[u.MessageID.String()]; got != triageReasonInvalidArguments {
		t.Errorf("bad-value reason = %v, want %q", got, triageReasonInvalidArguments)
	}
	superScan(t, h, &n, `SELECT count(*) FROM app.feedback_events WHERE message_id=$1`, u.MessageID.String())
	if n != 0 {
		t.Errorf("feedback rows after an invalid correction = %d, want 0", n)
	}
}
