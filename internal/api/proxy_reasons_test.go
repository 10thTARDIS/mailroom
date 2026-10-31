// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"os"
	"regexp"
	"testing"
)

// TestProxyRefusalReasonsListsEveryConstant reads proxy.go itself, so a reason
// added to the const block without being added to ProxyRefusalReasons fails
// here instead of being a series that silently starts absent (#674).
func TestProxyRefusalReasonsListsEveryConstant(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	decl := regexp.MustCompile(`(?m)^\s*reason[A-Z]\w*\s*=\s*"([a-z_]+)"`)
	listed := map[string]bool{}
	for _, r := range ProxyRefusalReasons() {
		if listed[r] {
			t.Errorf("reason %q listed twice", r)
		}
		listed[r] = true
	}
	matches := decl.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 20 {
		t.Fatalf("found only %d reason constants in proxy.go; the pattern no longer matches the const block", len(matches))
	}
	for _, m := range matches {
		if !listed[m[1]] {
			t.Errorf("reason %q is declared in proxy.go but missing from ProxyRefusalReasons", m[1])
		}
	}
	if len(listed) != len(matches) {
		t.Errorf("ProxyRefusalReasons has %d entries, proxy.go declares %d", len(listed), len(matches))
	}
}
