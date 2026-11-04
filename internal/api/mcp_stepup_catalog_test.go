// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"

	"mailroom/internal/mcp"
)

// A step-up 403 replaces the tool's answer after the call ran. That is safe only
// while a refused scope cannot hide a completed WRITE (the client would retry a
// write that already happened). So in every tool, a use that follows a write must
// need no scope of its own: the scoped uses are the writes themselves, or come
// before them. A future tool that writes and then makes a scoped call fails here.
func TestNoScopedUseFollowsAWriteInAnyTool(t *testing.T) {
	isRead := func(method string) bool {
		for _, suffix := range []string{"/get", "/query", "/changes", "/queryChanges"} {
			if strings.HasSuffix(method, suffix) {
				return true
			}
		}
		return false
	}
	scopeFree := func(method string) bool {
		pol, ok := keyPolicies[method]
		return ok && pol.ownPending && pol.needs == nil && len(pol.scopes) == 0
	}
	for _, tl := range mcp.Catalog() {
		wrote := ""
		for _, u := range tl.Uses {
			if wrote != "" && !scopeFree(u.Method) {
				t.Errorf("%s: %s needs a scope but follows the write %s; a scope refusal there would hide a completed write",
					tl.Name, u.Method, wrote)
			}
			if !isRead(u.Method) {
				wrote = u.Method
			}
		}
	}
	// The check can fail: a made-up tool with a write and then a scoped read.
	bad := []mcp.Use{{Method: "Email/set"}, {Method: "Email/get"}}
	wrote, failed := "", false
	for _, u := range bad {
		if wrote != "" && !scopeFree(u.Method) {
			failed = true
		}
		if !isRead(u.Method) {
			wrote = u.Method
		}
	}
	if !failed {
		t.Error("the check does not flag a write followed by a scoped call")
	}
}
