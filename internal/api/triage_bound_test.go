// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestTriageCorrectRefusesMoreThanTheBound: one call may carry at most
// maxTriageCorrections entries (#921). The refusal happens before any
// transaction, so a zero API is enough; at the bound the call gets past the
// check (and only then would need a store, which is why the control stops at
// the length check by asserting the error is NOT the bound's).
func TestTriageCorrectRefusesMoreThanTheBound(t *testing.T) {
	build := func(n int) json.RawMessage {
		c := make(map[string]string, n)
		for i := 0; i < n; i++ {
			c[fmt.Sprintf("id-%d", i)] = "important"
		}
		raw, err := json.Marshal(map[string]any{"accountId": "x", "corrections": c})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	a := &API{}
	_, mErr := a.triageCorrect(context.Background(), ulid.Make(), build(maxTriageCorrections+1))
	if mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("over the bound: got %+v, want invalidArguments", mErr)
	}
	want := fmt.Sprintf("at most %d corrections per call", maxTriageCorrections)
	if mErr.Description != want {
		t.Fatalf("description %q, want %q", mErr.Description, want)
	}

	// Control: exactly at the bound is not refused by the bound. With no store
	// the call cannot complete, so recover from whatever the missing store does
	// and only assert that the bound's refusal was not what came back.
	func() {
		defer func() { _ = recover() }()
		_, mErr := a.triageCorrect(context.Background(), ulid.Make(), build(maxTriageCorrections))
		if mErr != nil && mErr.Description == want {
			t.Fatalf("at the bound: refused by the bound")
		}
	}()
}
