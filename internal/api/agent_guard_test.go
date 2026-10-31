// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// A request the bearer gate authenticated reaches no JMAP route unless its
// policy is enforced. Driven through the REAL bearer gate (so the context is
// the one production builds) in front of the REAL API handler.
func TestAPIKeyRequestWithoutEnforcedPolicyIsRefused(t *testing.T) {
	tok, hash, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	gate := &auth.BearerGate{Resolve: func(_ context.Context, h [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
		return ulid.Make(), ulid.Make(), h == hash, nil
	}}
	a := New(nil)
	h := gate.Wrap(a.Handler(), http.NotFoundHandler())
	for _, target := range []struct{ method, path, body string }{
		{http.MethodPost, "/jmap", `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"x":1},"0"]]}`},
		{http.MethodGet, "/jmap/session", ""},
		{http.MethodGet, "/.well-known/jmap", ""},
		{http.MethodGet, "/jmap/eventsource/?types=*", ""},
	} {
		r := httptest.NewRequest(target.method, target.path, strings.NewReader(target.body))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s by a key with no enforced policy: status %d body %q, want refused",
				target.method, target.path, w.Code, w.Body.String())
		}
	}
}

// Triage/open refuses a key in the method itself, not only in the scope table
// (#49 review, F6, as pendingSet): a key's reads must never train the
// priority model. RED PROBE: remove the principalFrom check in triageOpen and
// this call succeeds.
func TestTriageOpenRefusesAKeyInTheMethod(t *testing.T) {
	a := New(nil)
	ctx := withPrincipal(context.Background(), newPrincipal(store.APIKey{ID: ulid.Make()}, ""))
	_, mErr := a.triageOpen(ctx, ulid.Make(), []byte(`{"emailIds":[]}`))
	if mErr == nil || mErr.Type != errForbidden {
		t.Fatalf("Triage/open by a key: %+v, want forbidden", mErr)
	}
	// Control: the same call from a session is answered.
	if _, mErr := a.triageOpen(context.Background(), ulid.Make(), []byte(`{"emailIds":[]}`)); mErr != nil {
		t.Fatalf("Triage/open by a session: %+v, want an answer", mErr)
	}
}
