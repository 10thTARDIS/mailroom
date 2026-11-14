// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryIntentRowLiteralSetsASource enumerates the enqueue sites and fails if
// any of them does not say what asked for the intent (ADR-028 amendment A2).
//
// WHY A SOURCE SCAN AND NOT ONLY A BEHAVIOURAL TEST. A source-less enqueue is
// legal SQL: the column is nullable on purpose (NULL means "enqueued before the
// column existed"), so the CHECK constraint cannot object and nothing errors. The
// row simply reads "unknown" in the report for ever, which is the failure that
// runs without erroring while doing nothing. Every IntentRow literal in the
// production tree is found by parsing it, so a new site that forgets the field is
// caught whether or not any test happens to drive it.
//
// It scans internal/ and cmd/, because the writeback path is reachable from
// neither alone.
//
// NEGATIVE CONTROL: delete `Source: IntentByPurge,` from internal/store/purge.go
// (or any site) and this names the file and line.
func TestEveryIntentRowLiteralSetsASource(t *testing.T) {
	var sites, missing []string
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isIntentRowType(lit.Type) {
					return true
				}
				where := fset.Position(lit.Pos()).String()
				sites = append(sites, where)
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Source" {
							return true
						}
					}
				}
				missing = append(missing, where)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// ARMING CONTROL: the scan found the sites. Six at the time of writing
	// (jmap_email.go x2, drafts.go x3, purge.go); a count of zero means the walk
	// or the type match broke, and this test would pass for ever.
	if len(sites) < 6 {
		t.Fatalf("found %d IntentRow literals (%v), want at least 6: the scan is not seeing the enqueue sites", len(sites), sites)
	}
	for _, m := range missing {
		t.Errorf("%s: IntentRow literal without a Source - the intent would read 'unknown' in the ledger report for ever", m)
	}
}

func isIntentRowType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "IntentRow"
	case *ast.SelectorExpr:
		return t.Sel.Name == "IntentRow"
	}
	return false
}
