// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests for the RFC 8620 conformance gaps closed by #85: `using`
// validation, nested result references, creation-id references, and the
// EventSource parameters. None of them touches the store.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// TestEveryMethodDeclaresACapability: a method with no entry in
// methodCapabilities is UNREACHABLE (mayCall fails closed). This test is the
// tripwire that keeps "I added a method" from silently meaning "I added a method
// nobody can call".
func TestEveryMethodDeclaresACapability(t *testing.T) {
	a := New(nil)
	a.SetSearch(nil) // no-op, but proves the search methods are not lost either
	for name := range a.methods {
		urn, ok := methodCapabilities[name]
		if !ok {
			t.Errorf("method %q has no capability in methodCapabilities: no client can call it", name)
			continue
		}
		if _, advertised := capabilityObjects()[urn]; !advertised {
			t.Errorf("method %q requires %q, which the session does not advertise", name, urn)
		}
	}
}

// TestCapabilitiesAreAdvertisedTopLevel is the regression test for the bug this
// project shipped once: a capability that appears only in accountCapabilities
// cannot legally be named in `using`, so its methods are unreachable.
func TestCapabilitiesAreAdvertisedTopLevel(t *testing.T) {
	top := capabilityObjects()
	for urn := range accountCapabilityObjects() {
		if _, ok := top[urn]; !ok {
			t.Errorf("%q is advertised per-account but NOT at the top level: no client may `using` it", urn)
		}
	}
	if _, ok := accountCapabilityObjects()[capCore]; ok {
		t.Error("urn:ietf:params:jmap:core is a server capability, not an account one")
	}
}

// The DUAL of TestEveryMethodDeclaresACapability (Forgejo #310).
//
// That test asks "does every method name a capability?". This one asks the
// question nobody was asking: "does every capability gate a method?" — and the
// answer is no. urn:mailroom:search and urn:mailroom:unsubscribe appear in no
// methodCapabilities entry, so declaring or omitting either changes nothing a
// client can observe. That is correct (both extend methods that
// urn:ietf:params:jmap:mail already gates — a FilterCondition and a property),
// and it is now WRITTEN DOWN in capabilitiesGatingNoMethod rather than being a
// fact one has to rediscover by grepping two tables against each other.
//
// The assertion runs in BOTH directions on purpose. One direction alone would
// let the list rot in the way that matters: a capability that quietly stops
// gating anything is exactly what a half-finished method rename produces, and it
// would look identical to a capability that never gated anything by design.
func TestCapabilitiesGatingNoMethodArePinned(t *testing.T) {
	gates := make(map[string]bool, len(methodCapabilities))
	for _, urn := range methodCapabilities {
		gates[urn] = true
	}

	// 1. Everything on the list really gates nothing.
	for urn := range capabilitiesGatingNoMethod {
		if gates[urn] {
			t.Errorf("%q is listed in capabilitiesGatingNoMethod but DOES gate a method — "+
				"the comment on it is now wrong; drop it from the list", urn)
		}
		if _, advertised := capabilityObjects()[urn]; !advertised {
			t.Errorf("%q is listed in capabilitiesGatingNoMethod but is not advertised at all", urn)
		}
	}

	// 2. Nothing else gates nothing. A capability that has quietly become inert is
	//    either a method table someone forgot to update or a deliberate
	//    conformance signal — and the difference has to be stated, not inferred.
	for urn := range capabilityObjects() {
		if gates[urn] || capabilitiesGatingNoMethod[urn] {
			continue
		}
		t.Errorf("%q is advertised but gates NO method, and is not listed in "+
			"capabilitiesGatingNoMethod.\nEither a method is missing from "+
			"methodCapabilities (in which case that method is unreachable), or this URN is a "+
			"conformance signal — say which, in capability.go, with the reason.", urn)
	}
}

func TestValidateUsing(t *testing.T) {
	if err := validateUsing([]string{capCore, capMail, capSubmission, capLabels, capCompose, capSearch}); err != nil {
		t.Fatalf("the advertised set must validate: %+v", err)
	}
	if err := validateUsing(nil); err != nil {
		t.Fatalf("an empty using list is not an unknown capability: %+v", err)
	}
	err := validateUsing([]string{capCore, "urn:ietf:params:jmap:vacationresponse"})
	if err == nil {
		t.Fatal("an unadvertised capability must be rejected")
	}
	if err.Type != jmapErrURN+errUnknownCapability {
		t.Errorf("type = %q, want %q", err.Type, jmapErrURN+errUnknownCapability)
	}
	if err.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", err.Status)
	}
}

// TestUndeclaredCapabilityIsUnknownMethod: calling a method whose capability the
// client did not declare must NOT be answered (RFC 8620 §3.3). Before #85 the
// `using` list was ignored entirely.
func TestUndeclaredCapabilityIsUnknownMethod(t *testing.T) {
	a := New(nil) // no store: a gated call must never reach one
	req := Request{
		Using: []string{capCore}, // core only — no mail
		MethodCalls: []Invocation{
			{Name: "Email/get", Args: json.RawMessage(`{"ids":[]}`), CallID: "c0"},
			{Name: "Core/echo", Args: json.RawMessage(`{"hello":"world"}`), CallID: "c1"},
		},
	}
	resp := a.dispatch(context.Background(), ulid.ULID{}, req)
	if len(resp.MethodResponses) != 2 {
		t.Fatalf("got %d responses", len(resp.MethodResponses))
	}
	if resp.MethodResponses[0].Name != "error" {
		t.Fatalf("Email/get without urn:…:mail in `using` was answered: %+v", resp.MethodResponses[0])
	}
	var me MethodError
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &me); err != nil {
		t.Fatal(err)
	}
	if me.Type != errUnknownMethod {
		t.Errorf("error type = %q, want %q", me.Type, errUnknownMethod)
	}
	// Core/echo IS declared, and echoes.
	if got := resp.MethodResponses[1]; got.Name != "Core/echo" || !jsonEqual(t, got.Args, `{"hello":"world"}`) {
		t.Errorf("Core/echo = %+v", got)
	}
}

// TestNestedResultReference: a result reference inside `create` (not a top-level
// argument) resolves. Before #85 only top-level "#args" were rewritten.
func TestNestedResultReference(t *testing.T) {
	prior := []Invocation{{
		Name:   "Email/query",
		Args:   json.RawMessage(`{"ids":["01ABC"]}`),
		CallID: "q0",
	}}
	args := json.RawMessage(`{"create":{"s1":{"#emailId":{"resultOf":"q0","name":"Email/query","path":"/ids/*"}}}}`)
	out, mErr := resolveReferences(args, prior)
	if mErr != nil {
		t.Fatalf("resolveReferences: %+v", mErr)
	}
	if !jsonEqual(t, out, `{"create":{"s1":{"emailId":["01ABC"]}}}`) {
		t.Errorf("got %s", out)
	}
}

// TestCreationIDKeyIsNotAResultReference: `update: {"#draft": {...}}` is a
// CREATION-ID key whose value is an object — it must not be mistaken for a
// (malformed) result reference, or every compose-then-edit batch breaks.
func TestCreationIDKeyIsNotAResultReference(t *testing.T) {
	args := json.RawMessage(`{"update":{"#draft":{"keywords/$seen":true}}}`)
	out, mErr := resolveReferences(args, nil)
	if mErr != nil {
		t.Fatalf("a creation-id key must not be read as a result reference: %+v", mErr)
	}
	if !jsonEqual(t, out, string(args)) {
		t.Errorf("args were rewritten: %s", out)
	}
}

// TestCreationIDReferenceNestedInCreate is RFC 8621 §7.5's canonical request:
// create the draft and submit it in ONE request, with `emailId: "#draft"` nested
// inside the submission's create. Two round trips became one.
func TestCreationIDReferenceNestedInCreate(t *testing.T) {
	ids := map[string]string{"draft": "01JEMAIL0000000000000000AA"}
	args := json.RawMessage(`{"create":{"s1":{"emailId":"#draft","identityId":"01JID"}}}`)
	out := resolveCreationIDs("EmailSubmission/set", args, ids)
	if !jsonEqual(t, out, `{"create":{"s1":{"emailId":"01JEMAIL0000000000000000AA","identityId":"01JID"}}}`) {
		t.Errorf("got %s", out)
	}
}

// TestCreationIDsDoNotRewriteUserText is why creation ids are resolved BY
// POSITION and not by value: a subject (or a search query) that happens to read
// "#draft" is text, not an id, and must survive a request that also creates
// something called "draft".
func TestCreationIDsDoNotRewriteUserText(t *testing.T) {
	ids := map[string]string{"draft": "01JEMAIL0000000000000000AA"}
	args := json.RawMessage(`{"create":{"d":{"subject":"#draft","bodyValues":{"b":{"value":"see #draft"}}}}}`)
	out := resolveCreationIDs("Email/set", args, ids)
	if !jsonEqual(t, out, string(args)) {
		t.Errorf("user text was rewritten as an id: %s", out)
	}
}

func TestCreationIDsInIDPositions(t *testing.T) {
	ids := map[string]string{"draft": "01JEMAIL0000000000000000AA", "mb": "01JMBOX0000000000000000BB"}

	// /get ids
	if out := resolveCreationIDs("Email/get", json.RawMessage(`{"ids":["#draft","01OTHER"]}`), ids); !jsonEqual(
		t, out, `{"ids":["01JEMAIL0000000000000000AA","01OTHER"]}`) {
		t.Errorf("ids: %s", out)
	}
	// /set destroy (array of ids) and update (keys)
	if out := resolveCreationIDs("Email/set", json.RawMessage(`{"destroy":["#draft"]}`), ids); !jsonEqual(
		t, out, `{"destroy":["01JEMAIL0000000000000000AA"]}`) {
		t.Errorf("destroy: %s", out)
	}
	if out := resolveCreationIDs("Email/set", json.RawMessage(`{"update":{"#draft":{"keywords/$seen":true}}}`), ids); !jsonEqual(
		t, out, `{"update":{"01JEMAIL0000000000000000AA":{"keywords/$seen":true}}}`) {
		t.Errorf("update: %s", out)
	}
	// mailboxIds keys inside a create
	if out := resolveCreationIDs("Email/set", json.RawMessage(`{"create":{"d":{"mailboxIds":{"#mb":true}}}}`), ids); !jsonEqual(
		t, out, `{"create":{"d":{"mailboxIds":{"01JMBOX0000000000000000BB":true}}}}`) {
		t.Errorf("mailboxIds: %s", out)
	}
	// An unknown creation id is left alone for the method to reject.
	if out := resolveCreationIDs("Email/get", json.RawMessage(`{"ids":["#nope"]}`), ids); !jsonEqual(
		t, out, `{"ids":["#nope"]}`) {
		t.Errorf("unknown creation id: %s", out)
	}
}

// TestRecordCreatedIDs harvests creation ids from any /set response.
func TestRecordCreatedIDs(t *testing.T) {
	ids := map[string]string{}
	recordCreatedIDs(json.RawMessage(`{"created":{"draft":{"id":"01JX","subject":"hi"}},"updated":{}}`), ids)
	if ids["draft"] != "01JX" {
		t.Errorf("createdIds = %v", ids)
	}
	recordCreatedIDs(json.RawMessage(`{"list":[]}`), ids) // a /get response: nothing to harvest
	if len(ids) != 1 {
		t.Errorf("createdIds = %v", ids)
	}
}

func TestParseEventTypes(t *testing.T) {
	all, err := parseEventTypes("")
	if err != nil || len(all) != len(eventSourceTypes) {
		t.Fatalf("empty types = %v, %v", all, err)
	}
	if star, err := parseEventTypes("*"); err != nil || len(star) != len(eventSourceTypes) {
		t.Fatalf(`"*" = %v, %v`, star, err)
	}
	got, err := parseEventTypes("Email,Mailbox,Email")
	if err != nil {
		t.Fatalf("parseEventTypes: %v", err)
	}
	if len(got) != 2 || got[0] != store.ObjectEmail || got[1] != store.ObjectMailbox {
		t.Errorf("types = %v", got)
	}
	if _, err := parseEventTypes("Email,VacationResponse"); err == nil {
		t.Error("an unknown type must be an error, not a silent omission")
	}
}

func TestParsePing(t *testing.T) {
	tests := map[string]time.Duration{
		"":    0,
		"0":   0,
		"1":   minPingInterval, // clamped
		"30":  30 * time.Second,
		"300": 300 * time.Second,
	}
	for in, want := range tests {
		got, err := parsePing(in)
		if err != nil {
			t.Fatalf("parsePing(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("parsePing(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"-1", "abc"} {
		if _, err := parsePing(bad); err == nil {
			t.Errorf("parsePing(%q) should have failed", bad)
		}
	}
}
