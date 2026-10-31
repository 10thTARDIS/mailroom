// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// timingAPI builds an API whose method table is three stubs registered under
// real, capability-gated names, so the dispatcher's own gate runs unmodified.
func timingAPI(opts ...Option) *API {
	a := New(nil, opts...)
	a.methods = map[string]methodFn{
		"Email/get": func(context.Context, ulid.ULID, json.RawMessage) (json.RawMessage, *MethodError) {
			time.Sleep(20 * time.Millisecond)
			return json.RawMessage(`{"list":[]}`), nil
		},
		"Email/query": func(context.Context, ulid.ULID, json.RawMessage) (json.RawMessage, *MethodError) {
			return nil, &MethodError{Type: errInvalidArguments}
		},
		"Mailbox/get": func(context.Context, ulid.ULID, json.RawMessage) (json.RawMessage, *MethodError) {
			return json.RawMessage(`{"list":[]}`), nil
		},
	}
	return a
}

func timingRequest() Request {
	call := func(name, id string) Invocation {
		return Invocation{Name: name, Args: json.RawMessage(`{}`), CallID: id}
	}
	return Request{
		Using: []string{capMail},
		MethodCalls: []Invocation{
			call("Email/get", "c0"),
			call("Email/query", "c1"),
			call("Bogus/<script>client-chosen", "c2"),
			call("Mailbox/get", "c3"),
			// A known name whose capability was not declared is unknown too.
			call("Rule/get", "c4"),
		},
	}
}

func TestDispatchReportsPerMethodTiming(t *testing.T) {
	var got []RequestTiming
	a := timingAPI(WithRequestTiming(func(rt RequestTiming) { got = append(got, rt) }))
	uid := ulid.Make()

	a.dispatch(context.Background(), uid, timingRequest())

	if len(got) != 1 {
		t.Fatalf("observer called %d times, want exactly 1 per request", len(got))
	}
	rt := got[0]
	if rt.UserID != uid {
		t.Errorf("UserID = %v, want %v", rt.UserID, uid)
	}
	want := []struct {
		name   string
		failed bool
	}{
		{"Email/get", false},
		{"Email/query", true},
		{"unknown", true},
		{"Mailbox/get", false},
		{"unknown", true},
	}
	if len(rt.Methods) != len(want) {
		t.Fatalf("got %d timings, want %d: %+v", len(rt.Methods), len(want), rt.Methods)
	}
	for i, w := range want {
		if rt.Methods[i].Name != w.name || rt.Methods[i].Failed != w.failed {
			t.Errorf("call %d = %+v, want name %q failed %v", i, rt.Methods[i], w.name, w.failed)
		}
	}
	if rt.Methods[0].Duration < 20*time.Millisecond {
		t.Errorf("a method that sleeps 20ms recorded %v", rt.Methods[0].Duration)
	}
	if rt.Total < rt.Methods[0].Duration {
		t.Errorf("Total %v is less than one of its calls (%v)", rt.Total, rt.Methods[0].Duration)
	}
	for _, m := range rt.Methods {
		if m.Name == "Bogus/<script>client-chosen" || m.Name == "Rule/get" {
			t.Errorf("an uncallable method was recorded under its client name %q", m.Name)
		}
	}
}

func TestDispatchTimesAReferenceFailureUnderItsName(t *testing.T) {
	var got RequestTiming
	a := timingAPI(WithRequestTiming(func(rt RequestTiming) { got = rt }))
	req := Request{Using: []string{capMail}, MethodCalls: []Invocation{
		{Name: "Email/get", CallID: "c0",
			Args: json.RawMessage(`{"#ids":{"resultOf":"nope","name":"Email/query","path":"/ids"}}`)},
	}}
	a.dispatch(context.Background(), ulid.ULID{}, req)
	if len(got.Methods) != 1 || got.Methods[0].Name != "Email/get" || !got.Methods[0].Failed {
		t.Errorf("reference failure timing = %+v, want Email/get failed", got.Methods)
	}
}

func TestDispatchResponseIsIdenticalWithAndWithoutTiming(t *testing.T) {
	plain := timingAPI()
	observed := timingAPI(WithRequestTiming(func(RequestTiming) {}))

	a, err := json.Marshal(plain.dispatch(context.Background(), ulid.ULID{}, timingRequest()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(observed.dispatch(context.Background(), ulid.ULID{}, timingRequest()))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("the observer changed the response:\n without: %s\n with:    %s", a, b)
	}
}
