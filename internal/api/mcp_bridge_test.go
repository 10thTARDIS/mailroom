// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"mailroom/internal/mcp"
)

// A tool's declared JMAP uses decide whether tools/list shows it. A use whose
// method has no reachable line in the key policy table — a typo, or a method
// the table refuses — would hide the tool from every key without an error.
// This holds each declared use to the table.
func TestEveryToolUseIsInTheKeyPolicyTable(t *testing.T) {
	tools := mcp.Catalog()
	if len(tools) == 0 {
		t.Fatal("no tools: the check below would pass vacuously")
	}
	for _, tl := range tools {
		for _, u := range tl.Uses {
			pol, known := keyPolicies[u.Method]
			switch {
			case !known:
				t.Errorf("%s uses %s, which has no line in keyPolicies", tl.Name, u.Method)
				continue
			case !pol.keyReachable():
				t.Errorf("%s uses %s, which keyPolicies refuses to keys (%s)", tl.Name, u.Method, pol.why)
				continue
			}
			for _, f := range u.EmailUpdate {
				if u.Method != "Email/set" {
					t.Errorf("%s declares EmailUpdate fields on %s", tl.Name, u.Method)
				}
				if _, ok := emailUpdateFieldScopes[f]; !ok {
					t.Errorf("%s declares EmailUpdate field %q, which emailUpdateFieldScopes does not classify", tl.Name, f)
				}
			}
			if u.Method == "Email/set" && len(u.EmailUpdate) == 0 && (len(u.Args) == 0 || hasUpdate(u.Args)) {
				t.Errorf("%s: an Email/set use must say what it does (create/destroy args, or EmailUpdate fields)", tl.Name)
			}
		}
	}
}

// KeyMayCall fails closed: with no key in the context (a session, or no
// identity at all) no tool is listed.
func TestKeyMayCallFailsClosedWithoutAKey(t *testing.T) {
	b := mcpBridge{a: &API{}}
	r := httptest.NewRequest(http.MethodPost, MCPPath, nil)
	for _, tl := range mcp.Catalog() {
		if b.KeyMayCall(r.Context(), tl.Uses...) {
			t.Errorf("%s listed for a context with no key", tl.Name)
		}
	}
	if _, err := b.Call(r.Context(), []mcp.Invocation{{Name: "Core/echo", Args: []byte(`{}`), CallID: "0"}}); err == nil {
		t.Error("Call ran without a key in the context")
	}
}

func TestMCPChallenge(t *testing.T) {
	reached := false
	h := MCPChallenge(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }), "mailroom_session")
	for _, c := range []struct {
		name          string
		auth, cookie  bool
		wantReach     bool
		wantChallenge bool
	}{
		{"nothing", false, false, false, true},
		{"bearer", true, false, true, false},
		{"cookie", false, true, true, false},
	} {
		reached = false
		r := httptest.NewRequest(http.MethodPost, MCPPath, nil)
		if c.auth {
			r.Header.Set("Authorization", "Bearer x")
		}
		if c.cookie {
			r.AddCookie(&http.Cookie{Name: "mailroom_session", Value: "x"})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if reached != c.wantReach {
			t.Errorf("%s: reached=%v", c.name, reached)
		}
		if c.wantChallenge && (w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Bearer realm="mailroom"`) {
			t.Errorf("%s: %d %q, want 401 with the Bearer challenge", c.name, w.Code, w.Header().Get("WWW-Authenticate"))
		}
	}
}
