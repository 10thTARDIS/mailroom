// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests (no database): state-string round trips, result-reference
// resolution (JSON pointer), the request/response envelope, and the
// authentication guard on the HTTP handlers.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestStateRoundTrip(t *testing.T) {
	for _, seq := range []int64{0, 1, 184223} {
		s := encodeState(seq)
		got, err := decodeState(s)
		if err != nil {
			t.Fatalf("decodeState(%q): %v", s, err)
		}
		if got != seq {
			t.Errorf("round trip %d -> %q -> %d", seq, s, got)
		}
	}
	for _, bad := range []string{"", "184223", "s:", "s:-1", "s:abc", "x:1"} {
		if _, err := decodeState(bad); err == nil {
			t.Errorf("decodeState(%q) should have failed", bad)
		}
	}
}

func TestEvalPointer(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		path string
		want string
	}{
		{"direct ids array", `{"ids":["A","B"]}`, "/ids", `["A","B"]`},
		{"map over list ids", `{"list":[{"id":"A"},{"id":"B"}]}`, "/list/*/id", `["A","B"]`},
		{"nested member", `{"a":{"b":"c"}}`, "/a/b", `"c"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalPointer(json.RawMessage(tc.doc), tc.path)
			if err != nil {
				t.Fatalf("evalPointer: %v", err)
			}
			if !jsonEqual(t, got, tc.want) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}

	if _, err := evalPointer(json.RawMessage(`{"ids":"x"}`), "/ids/*/id"); err == nil {
		t.Error("expected error mapping '*' over a non-array")
	}
	if _, err := evalPointer(json.RawMessage(`{}`), "/missing"); err == nil {
		t.Error("expected error for a missing key")
	}
}

func TestResolveReferences(t *testing.T) {
	prior := []Invocation{{
		Name:   "Email/query",
		Args:   json.RawMessage(`{"ids":["A","B"]}`),
		CallID: "c0",
	}}
	args := json.RawMessage(`{"accountId":"u","#ids":{"resultOf":"c0","name":"Email/query","path":"/ids"}}`)
	out, mErr := resolveReferences(args, prior)
	if mErr != nil {
		t.Fatalf("resolveReferences: %+v", mErr)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["#ids"]; ok {
		t.Error("#ids reference was not consumed")
	}
	if !jsonEqual(t, m["ids"], `["A","B"]`) {
		t.Errorf("ids = %s, want [\"A\",\"B\"]", m["ids"])
	}

	// A reference to a non-existent call is invalidResultReference.
	bad := json.RawMessage(`{"#ids":{"resultOf":"nope","name":"Email/query","path":"/ids"}}`)
	if _, mErr := resolveReferences(bad, prior); mErr == nil || mErr.Type != errInvalidResultReference {
		t.Errorf("expected invalidResultReference, got %+v", mErr)
	}
}

func TestInvocationRoundTrip(t *testing.T) {
	in := Invocation{Name: "Email/get", Args: json.RawMessage(`{"ids":["A"]}`), CallID: "c1"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, b, `["Email/get",{"ids":["A"]},"c1"]`) {
		t.Fatalf("marshal = %s", b)
	}
	var out Invocation
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.CallID != in.CallID || !jsonEqual(t, out.Args, string(in.Args)) {
		t.Errorf("round trip mismatch: %+v", out)
	}

	// Wrong arity is rejected.
	if err := json.Unmarshal([]byte(`["only","two"]`), &out); err == nil {
		t.Error("expected error for a 2-element invocation")
	}
}

func TestDispatchUnknownMethod(t *testing.T) {
	a := New(nil) // unknownMethod never touches the store
	req := Request{MethodCalls: []Invocation{
		{Name: "Bogus/method", Args: json.RawMessage(`{}`), CallID: "c0"},
	}}
	resp := a.dispatch(context.Background(), ulid.ULID{}, req)
	if len(resp.MethodResponses) != 1 {
		t.Fatalf("got %d responses", len(resp.MethodResponses))
	}
	got := resp.MethodResponses[0]
	if got.Name != "error" || got.CallID != "c0" {
		t.Fatalf("got %+v", got)
	}
	var me MethodError
	if err := json.Unmarshal(got.Args, &me); err != nil {
		t.Fatal(err)
	}
	if me.Type != errUnknownMethod {
		t.Errorf("error type = %q, want %q", me.Type, errUnknownMethod)
	}
}

func TestRequestHandlerUnauthorized(t *testing.T) {
	a := New(nil)
	r := httptest.NewRequest("POST", "/jmap", nil)
	w := httptest.NewRecorder()
	a.RequestHandler(w, r) // no user id in context
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestSessionHandlerUnauthorized(t *testing.T) {
	a := New(nil)
	r := httptest.NewRequest("GET", "/jmap/session", nil)
	w := httptest.NewRecorder()
	a.SessionHandler(w, r)
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("unmarshal got %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("unmarshal want %s: %v", want, err)
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
