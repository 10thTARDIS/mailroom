// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The set_user_disabled definer is EXECUTE-granted to mailroom_app (the
// operator CLI connects as that role, as for hard_delete_user), so what keeps it
// off the API is that only cmd/mailroom-admin calls it. This scan pins that: a
// call from any other package (an HTTP handler, a JMAP method, MCP) would let a
// request disable or re-enable users.
func TestSetUserDisabledHasNoAPICaller(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{
		filepath.Join("cmd", "mailroom-admin"): true,
		filepath.Join("internal", "store"):     true,
	}
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		if allowed[rel] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "SetUserDisabled(") || strings.Contains(string(b), "set_user_disabled") {
			bad = append(bad, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("set_user_disabled is reachable outside mailroom-admin: %v", bad)
	}
}
