// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// Unit coverage for the Triage controls that needs no database: the nil-registry
// fail-closed guard and the correction-token → label mapping. The DB-backed
// behavior (reset/rollback/correct against real rows and RLS) lives in
// triage_integration_test.go.

// TestTriageControlsRequireRegistry: with no model registry wired, Triage/get and
// Triage/set return a clean MethodError (not a panic) BEFORE touching any store,
// so this runs with a nil store. Triage/correct is deliberately NOT registry-gated
// — it only writes feedback_events — so it is excluded here.
func TestTriageControlsRequireRegistry(t *testing.T) {
	a := New(nil) // no store, no registry
	if a.registry != nil {
		t.Fatal("test precondition: registry should be nil")
	}
	for _, method := range []string{"Triage/get", "Triage/set"} {
		fn, ok := a.methods[method]
		if !ok {
			t.Fatalf("%s is not registered", method)
		}
		_, mErr := fn(context.Background(), ulid.Make(), json.RawMessage(`{}`))
		if mErr == nil {
			t.Errorf("%s with nil registry: want a MethodError, got nil", method)
		}
	}
}

// TestCorrectionTokenMapsToLabelledActions locks the client correction values to
// feedback tokens that ml.LabelForAction actually maps, with the right sign — an
// unmapped token would be silently dropped by the training harness. An unknown
// value is rejected (ok == false → invalidArguments for that entry).
func TestCorrectionTokenMapsToLabelledActions(t *testing.T) {
	cases := []struct {
		value        string
		wantToken    string
		wantPositive bool
	}{
		{correctionImportant, feedbackMarkImportant, true},
		{correctionNotImportant, feedbackMarkNotImportant, false},
	}
	for _, c := range cases {
		tok, ok := correctionToken(c.value)
		if !ok || tok != c.wantToken {
			t.Fatalf("correctionToken(%q) = (%q, %v), want (%q, true)", c.value, tok, ok, c.wantToken)
		}
		label, mapped := ml.LabelForAction(tok)
		if !mapped {
			t.Errorf("token %q is not in ml.LabelForAction — the training harness would drop it", tok)
			continue
		}
		if (label >= 0.5) != c.wantPositive {
			t.Errorf("token %q label = %v, want positive=%v", tok, label, c.wantPositive)
		}
	}
	if tok, ok := correctionToken("nonsense"); ok {
		t.Errorf("correctionToken accepted an unknown value, returned %q", tok)
	}
}

// TestCorrectionSideAgreesWithTheFeedbackSign locks Triage/correct's TWO
// consequences to one vocabulary (#464): every value that maps to a feedback
// token must also map to a side of the desk's line, with the matching sign —
// a positive signal ("important") places the message on the surface, a
// negative one in the drawer.
//
// It exists because the two switches are separate functions over the same
// values, and the failure mode of a drift is silent in the worst way: a
// correction that trains the model without moving the mail, or moves it the
// wrong way, and returns "corrected" either way. An unknown value must be
// rejected by BOTH, or triageCorrect would place a message it refused to
// record feedback for.
func TestCorrectionSideAgreesWithTheFeedbackSign(t *testing.T) {
	for _, value := range []string{correctionImportant, correctionNotImportant} {
		token, ok := correctionToken(value)
		if !ok {
			t.Fatalf("correctionToken(%q) rejected a known value", value)
		}
		side, ok := correctionSide(value)
		if !ok {
			t.Fatalf("correctionSide(%q) rejected a value correctionToken accepts — "+
				"a correction would train the model without moving the mail", value)
		}
		if !side.Valid() {
			t.Fatalf("correctionSide(%q) = %q, which app.messages' CHECK would reject", value, side)
		}
		label, mapped := ml.LabelForAction(token)
		if !mapped {
			t.Fatalf("token %q is not in ml.LabelForAction", token)
		}
		wantSide := store.LineSideDrawer
		if label >= 0.5 {
			wantSide = store.LineSideSurface
		}
		if side != wantSide {
			t.Errorf("correction %q: feedback label %v (positive=%v) but side %q, want %q",
				value, label, label >= 0.5, side, wantSide)
		}
	}
	if side, ok := correctionSide("nonsense"); ok {
		t.Errorf("correctionSide accepted an unknown value, returned %q", side)
	}
}
