// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ContextWithSessionForTest puts a session on a context WITHOUT going through
// Middleware, so it exists for tests only. It is exported because the API and
// cmd tests live in other packages. This guard makes "nothing outside tests
// calls it" a checked fact instead of a comment: no non-test Go file anywhere in
// the module may mention it, apart from the file that defines it.

const testHookName = "ContextWithSessionForTest"

// moduleRoot walks up from the test's directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// filesMentioning returns the non-test .go files under root (relative paths)
// that contain name, except those in allowed.
func filesMentioning(t *testing.T, root, name string, allowed map[string]bool) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if allowed[rel] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), name) {
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestTestOnlySessionHookIsNeverUsedByProductionCode(t *testing.T) {
	root := moduleRoot(t)
	const definingFile = "internal/auth/session.go"

	if hits := filesMentioning(t, root, testHookName, map[string]bool{definingFile: true}); len(hits) != 0 {
		t.Fatalf("%s is a test-only hook but non-test files mention it: %v", testHookName, hits)
	}

	// Negative controls: the scan can fail. Without the allowance it finds the
	// defining file (so the walk really reads the module's files), and a production
	// file added anywhere that mentions the hook is caught.
	if hits := filesMentioning(t, root, testHookName, nil); len(hits) != 1 || hits[0] != definingFile {
		t.Fatalf("scan with no allowance found %v, want exactly the defining file %s (the scan is not reading the module)", hits, definingFile)
	}
	fake := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fake, "internal", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(fake, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/x/prod.go", "package x\nvar _ = auth."+testHookName+"\n")
	write("internal/x/prod_test.go", "package x\nvar _ = auth."+testHookName+"\n")
	if hits := filesMentioning(t, fake, testHookName, nil); len(hits) != 1 || hits[0] != "internal/x/prod.go" {
		t.Fatalf("scan of a tree with a production use found %v, want exactly internal/x/prod.go (and not the _test.go file)", hits)
	}
}
