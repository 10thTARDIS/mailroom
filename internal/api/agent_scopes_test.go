// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// THE GUARD AGAINST A NEW METHOD SILENTLY BECOMING KEY-REACHABLE (#49,
// ADR-042 §3). It walks every method the dispatcher can serve — the method
// table plus every method a capability gates (SearchSnippet/get is registered
// only when search is wired) — and requires a line in keyPolicies for each.
func TestEveryMethodHasAKeyPolicy(t *testing.T) {
	a := New(nil)
	all := map[string]bool{}
	for name := range a.methods {
		all[name] = true
	}
	for name := range methodCapabilities {
		all[name] = true
	}
	if len(all) < 90 {
		t.Fatalf("only %d methods found — the walk is not looking at the dispatcher", len(all))
	}
	for name := range all {
		pol, ok := keyPolicies[name]
		if !ok {
			t.Errorf("%s has no line in keyPolicies (agent_scopes.go): decide what an API key may do with it — "+
				"a scope mapping, notForKeys or sessionOnly", name)
			continue
		}
		kinds := 0
		if pol.keyReachable() {
			kinds++
		}
		if pol.sessionOnly {
			kinds++
		}
		if pol.notForKeys {
			kinds++
		}
		if kinds != 1 {
			t.Errorf("%s: policy is %d kinds at once, want exactly one", name, kinds)
		}
		if !pol.keyReachable() && pol.why == "" {
			t.Errorf("%s is refused to keys without a reason", name)
		}
		for _, s := range pol.scopes {
			if !isKnownScope(s) {
				t.Errorf("%s maps to unknown scope %q", name, s)
			}
		}
		if pol.keyReachable() && pol.needs == nil && !pol.ownPending && name != "Core/echo" && len(pol.scopes) == 0 {
			t.Errorf("%s is key-reachable with no scope at all", name)
		}
	}
	for name := range keyPolicies {
		if !all[name] {
			t.Errorf("keyPolicies names %s, which the dispatcher does not serve (stale line)", name)
		}
	}
	// What must never be reachable by a key, named outright so a well-meant
	// edit to the table cannot quietly open one.
	for _, name := range []string{"ApiKey/get", "ApiKey/set", "ApiKey/getEvents", "PendingAction/set",
		"UserKey/get", "UserKey/generate", "UserKey/import", "UserKey/export", "UserKey/revoke",
		"UserKey/extend", "UserKey/set", "UserKey/replace", "Identity/set", "Account/set", "Account/get",
		"Mailbox/set", "OutOfOffice/set", "Email/replyToInvite", "Rule/set", "Rule/apply", "Setup/apply",
		"WebPushSubscription/set", "Email/releaseAttachment"} {
		if pol := keyPolicies[name]; !pol.sessionOnly {
			t.Errorf("%s must be sessionOnly", name)
		}
	}
}

// Every field of store.EmailUpdate has a scope, so a new kind of change cannot
// ride in under a scope that never anticipated it.
func TestEveryEmailUpdateFieldIsClassified(t *testing.T) {
	typ := reflect.TypeOf(store.EmailUpdate{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		scope, ok := emailUpdateFieldScopes[f.Name]
		if _, refused := keyRefusedEmailUpdateFields[f.Name]; refused {
			if ok {
				t.Errorf("%s is both refused to keys and mapped to %s", f.Name, scope)
			}
			var up store.EmailUpdate
			if !setNonZero(reflect.ValueOf(&up).Elem().Field(i)) {
				t.Fatalf("%s: unhandled kind", f.Name)
			}
			if got, allowed := emailUpdateScopes(up); allowed {
				t.Errorf("%s is refused to keys by decision, but emailUpdateScopes allows it (%v)", f.Name, got)
			}
			continue
		}
		if !ok {
			t.Errorf("store.EmailUpdate.%s has no scope in emailUpdateFieldScopes: API keys are refused it "+
				"until it does, and this test fails until someone decides", f.Name)
			continue
		}
		if !isKnownScope(scope) {
			t.Errorf("%s maps to unknown scope %q", f.Name, scope)
		}
		// Setting the field alone yields exactly its scope.
		var up store.EmailUpdate
		v := reflect.ValueOf(&up).Elem().Field(i)
		if !setNonZero(v) {
			t.Fatalf("%s: unhandled kind %s — extend setNonZero", f.Name, v.Kind())
		}
		got, ok := emailUpdateScopes(up)
		if !ok || len(got) != 1 || got[0] != scope {
			t.Errorf("%s alone needs %v (ok=%v), want [%s]", f.Name, got, ok, scope)
		}
	}
	for _, field := range []string{"Trash"} {
		if emailUpdateFieldScopes[field] != scopeMailDelete {
			t.Errorf("%s must need mail:delete: a move into Trash is a delete", field)
		}
	}
}

// setNonZero makes v non-zero, whatever its kind.
func setNonZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		v.SetMapIndex(reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem())
	case reflect.Array:
		v.Index(0).Set(reflect.ValueOf(byte(1)).Convert(v.Type().Elem()))
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() && setNonZero(v.Field(i)) {
				return true
			}
		}
		return false
	default:
		return false
	}
	return true
}

// A request whose context names a key but carries no enforced policy runs NO
// method — checked at the dispatcher, beneath the HTTP gate, so a route that
// someday skips keyPolicyGate is still not a way in. The context comes from the
// REAL bearer gate.
func TestDispatchRefusesAKeyWithoutAPolicy(t *testing.T) {
	tok, hash, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	user := ulid.Make()
	gate := &auth.BearerGate{Resolve: func(_ context.Context, h [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
		return user, ulid.Make(), h == hash, nil
	}}
	a := New(nil)
	var resp Response
	h := gate.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp = a.dispatch(r.Context(), user, Request{Using: []string{capCore},
			MethodCalls: []Invocation{{Name: "Core/echo", Args: json.RawMessage(`{"x":1}`), CallID: "0"}}})
	}), http.NotFoundHandler())
	r := httptest.NewRequest(http.MethodPost, "/jmap", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if len(resp.MethodResponses) != 1 {
		t.Fatalf("dispatch did not run: %+v", resp)
	}
	got := resp.MethodResponses[0]
	if got.Name != "error" || !strings.Contains(string(got.Args), errForbidden) {
		t.Fatalf("a key with no policy got %s %s, want a forbidden error", got.Name, got.Args)
	}

	// Positive control: the same dispatch for a session answers.
	sess := a.dispatch(context.Background(), user, Request{Using: []string{capCore},
		MethodCalls: []Invocation{{Name: "Core/echo", Args: json.RawMessage(`{"x":1}`), CallID: "0"}}})
	if sess.MethodResponses[0].Name != "Core/echo" {
		t.Fatalf("session dispatch did not answer: %+v", sess)
	}
}

// The HTTP gate refuses a key outright when no policy can be loaded (no store
// here), and refuses every route a key may not reach.
func TestKeyPolicyGateRefusesWithoutAPolicyAndOffItsRoutes(t *testing.T) {
	tok, hash, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	gate := &auth.BearerGate{Resolve: func(_ context.Context, h [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
		return ulid.Make(), ulid.Make(), h == hash, nil
	}}
	a := New(nil)
	h := gate.Wrap(a.Handler(), http.NotFoundHandler())
	for _, target := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/jmap", http.StatusUnauthorized},
		{http.MethodGet, "/jmap/session", http.StatusUnauthorized},
		{http.MethodGet, "/jmap/eventsource/?types=*", http.StatusForbidden},
		{http.MethodGet, "/jmap/download/x/y/z", http.StatusForbidden},
		{http.MethodPost, "/jmap/upload/x/", http.StatusForbidden},
	} {
		r := httptest.NewRequest(target.method, target.path,
			strings.NewReader(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"x":1},"0"]]}`))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != target.want {
			t.Errorf("%s %s: status %d body %q, want %d", target.method, target.path, w.Code, w.Body.String(), target.want)
		}
	}
}

func TestOffHoursRule(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 21, h, m, 0, 0, time.UTC) } // a Wednesday
	none := store.QuietHours{}
	if !isOffHours(none, at(3, 0)) || isOffHours(none, at(6, 0)) || isOffHours(none, at(14, 0)) {
		t.Error("without quiet hours the window is 00:00–06:00 UTC")
	}
	zoned := store.QuietHours{Timezone: "America/New_York"} // 03:00 UTC is 23:00 there
	if isOffHours(zoned, at(3, 0)) || !isOffHours(zoned, at(7, 0)) {
		t.Error("without quiet hours the default window follows the user's time zone")
	}
	quiet := store.QuietHours{Enabled: true, Start: 13 * 60, End: 14 * 60, Days: 0x7f, Timezone: "UTC"}
	if !isOffHours(quiet, at(13, 30)) || isOffHours(quiet, at(3, 0)) {
		t.Error("with quiet hours on, they ARE the off hours")
	}
}

func TestSafeTextStripsBidiAndControls(t *testing.T) {
	if got := safeText("a\u202eb\x00c\nd", 80); got != "abcd" {
		t.Errorf("safeText = %q", got)
	}
	if got := safeText(strings.Repeat("x", 10), 4); got != "xxxx…" {
		t.Errorf("safeText truncation = %q", got)
	}
}
