// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// #867: the feedback emitters return before touching the transaction when
// the request is an API key's. The context is built by the REAL bearer gate. The
// transaction is nil, so an emitter that did NOT return early would dereference
// it and panic: the test goes red if the guard is removed (probe it), and the
// session control proves the nil tx really is what a non-returning emitter hits.
func TestFeedbackEmittersSkipAPIKeyRequests(t *testing.T) {
	tok, hash, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	gate := &auth.BearerGate{Resolve: func(_ context.Context, h [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
		return ulid.Make(), ulid.Make(), h == hash, nil
	}}
	var keyCtx context.Context
	h := gate.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { keyCtx = r.Context() }), http.NotFoundHandler())
	r := httptest.NewRequest(http.MethodPost, "/jmap", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if _, isKey := auth.APIKeyIDFromContext(keyCtx); !isKey {
		t.Fatal("the gate did not mark the request as a key's; the test would prove nothing")
	}

	a := New(nil)
	id := ulid.Make()
	// Would panic on the nil tx without the guard.
	a.emitEmailSetFeedback(keyCtx, nil, id, store.EmailUpdate{Trash: true})
	a.emitSendFeedback(keyCtx, nil, id)

	// Control: a session request does reach the transaction.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("control: a session request did not reach the transaction, so the nil tx proves nothing")
			}
		}()
		a.emitEmailSetFeedback(context.Background(), nil, id, store.EmailUpdate{Trash: true})
	}()
}
