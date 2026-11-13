// SPDX-License-Identifier: AGPL-3.0-only

package sync_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mailroom/internal/mailsync"
	"mailroom/internal/onboarding"
	"mailroom/internal/sync"
)

var writeMethods = []string{"Append", "Copy", "Expunge", "Move", "StoreFlags"}

func methodNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	sort.Strings(out)
	return out
}

// TestReadSessionCannotWrite pins ADR-028 amendment A7: the five calls that
// change a mailbox are in WriteSession and ONLY there, so code handed a
// ReadSession has nothing to write with.
//
// A compile-time property can be quietly undone by someone adding a convenient
// method to the wrong interface, which is a one-line change no compiler objects
// to. This is what objects.
//
// NEGATIVE CONTROL: add Append to ReadSession and the first loop fails; remove it
// from WriteSession and the second does.
func TestReadSessionCannotWrite(t *testing.T) {
	read := reflect.TypeOf((*sync.ReadSession)(nil)).Elem()
	write := reflect.TypeOf((*sync.WriteSession)(nil)).Elem()
	full := reflect.TypeOf((*sync.SyncSession)(nil)).Elem()

	readNames := methodNames(read)
	for _, w := range writeMethods {
		for _, r := range readNames {
			if r == w {
				t.Errorf("ReadSession has the write method %s", w)
			}
		}
	}
	if got := methodNames(write); !reflect.DeepEqual(got, writeMethods) {
		t.Errorf("WriteSession methods = %v, want exactly %v", got, writeMethods)
	}
	// SyncSession is exactly the two halves, nothing added to it directly.
	want := append(append([]string{}, readNames...), writeMethods...)
	sort.Strings(want)
	if got := methodNames(full); !reflect.DeepEqual(got, want) {
		t.Errorf("SyncSession methods = %v, want the union %v", got, want)
	}
	// Positive control: the reads the sync engine needs are on the read side, so
	// the split is not satisfied by emptying it.
	for _, need := range []string{"FetchByUID", "LiveUIDs", "ListMailboxes", "Select", "Idle"} {
		found := false
		for _, r := range readNames {
			found = found || r == need
		}
		if !found {
			t.Errorf("ReadSession lost %s", need)
		}
	}
}

// TestReadOnlyPathsDialReadOnly: the dial seams of the sync engine and of
// onboarding hand out a ReadSession, so the production code behind them cannot
// reach a write call.
func TestReadOnlyPathsDialReadOnly(t *testing.T) {
	readT := reflect.TypeOf((*sync.ReadSession)(nil)).Elem()
	for name, fn := range map[string]reflect.Type{
		"mailsync.SessionFactory": reflect.TypeOf(mailsync.SessionFactory(nil)),
		"onboarding.Dialer":       reflect.TypeOf(onboarding.Dialer(nil)),
		"sync.DialRead":           reflect.TypeOf(sync.DialRead),
	} {
		if fn.NumOut() != 2 || fn.Out(0) != readT {
			t.Errorf("%s returns %v, want sync.ReadSession", name, fn.Out(0))
		}
	}
}

// TestReadOnlyPackagesNameNoWriteSession fails when a package that must not write
// to a mailbox mentions the full SyncSession in its non-test code.
//
// The types above only bind where the seam is typed. A new helper in the sync
// engine that took a SyncSession would compile, and could call Append. This is the
// grep ADR-028 §3 relied on, made a test: it walks the sources of every package
// whose job is to READ a mailbox and finds any selector `<pkg>.SyncSession`.
//
// The writeback package is not on the list, on purpose: it is the one place that
// writes (and the only construction site of the writer is behind the shadow
// check, ADR-028 §3).
//
// NEGATIVE CONTROL: change a `sync.ReadSession` parameter in
// internal/mailsync/flags.go back to `sync.SyncSession` and this fails naming it.
func TestReadOnlyPackagesNameNoWriteSession(t *testing.T) {
	for _, dir := range []string{"mailsync", "syncrunner", "onboarding", "gmailsync", "census"} {
		root := filepath.Join("..", dir)
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v (the list of read-only packages is out of date)", root, err)
		}
		checked := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			checked++
			path := filepath.Join(root, e.Name())
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SyncSession" {
					t.Errorf("%s names SyncSession (%s): a read-only package takes sync.ReadSession (ADR-028 A7)",
						path, "sync."+sel.Sel.Name)
				}
				return true
			})
		}
		if checked == 0 {
			t.Errorf("%s: no source files checked, so this proves nothing about it", root)
		}
	}
}
