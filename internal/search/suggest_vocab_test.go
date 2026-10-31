// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"os"
	"regexp"
	"testing"
)

// TestSearchSuggestVocabularyMatchesParser pins the web client's `is:`/`has:`
// autocomplete vocabulary (web/src/core/searchSuggest.ts IS_VALUES/HAS_VALUES)
// against this package's own flagAtom (parse.go) — the parser that actually
// decides what those two operators accept.
//
// #623: IS_VALUES once offered `unflagged`, `snoozed` and `pinned`, none of
// which flagAtom recognises. Choosing any of the three from the suggestion
// list — or simply typing one, since the suggestion IS the documentation
// (searchSuggest.ts's own header says so) — produced a query flagAtom
// refuses, and parse.go's `case "is", "has":` falls back to None{}: a query
// that silently matches nothing, with no error anywhere to say so.
//
// The two vocabularies live in different languages with no shared type to
// enforce agreement between them, so this reads the TypeScript source as text
// (the same reason web/src/app/ui/outbox.test.ts reads Sidebar.tsx's source:
// a cross-file invariant is enforced or it is forgotten) and asserts every
// value the client offers is one the parser actually accepts.
//
// It deliberately does NOT require the reverse — every value flagAtom accepts
// being offered as a suggestion. `answered`/`replied`, `junk`/`spam`, and
// `attachments`/`file` are all valid queries a user can type without this
// client needing to suggest every synonym; a curated, narrower suggestion
// list is a design choice, not a bug this test should flag.
//
// #722/#723 widened it in two ways. The `is:`/`has:` values are now checked
// against everything the parser accepts for them (StateValueKnown: flag atoms,
// attachment families, and the store-side states is:snoozed/muted/pinned/
// important), and the new `in:` values likewise. And the OPERATORS the client
// offers (SEARCH_OPERATORS) are checked against the parser's own operator list:
// an operator the parser does not know is not an operator at all — `nope:x`
// silently searches the text "nope:x" — which is the same failure one level up.
func TestSearchSuggestVocabularyMatchesParser(t *testing.T) {
	const path = "../../web/src/core/searchSuggest.ts"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	for _, set := range []struct {
		name, op string
	}{
		{"IS_VALUES", "is"},
		{"HAS_VALUES", "has"},
		{"IN_VALUES", "in"},
	} {
		for _, v := range extractStringArray(t, string(src), set.name, path) {
			if !StateValueKnown(set.op, v) {
				t.Errorf("searchSuggest.ts %s offers %q, which the parser does not accept for %s: — "+
					"choosing it (or typing it) produces a query that matches nothing", set.name, v, set.op)
			}
		}
	}

	ops := extractOperators(t, string(src), path)
	known := map[string]bool{}
	for _, op := range Operators() {
		known[op] = true
	}
	for _, op := range ops {
		if !known[op] {
			t.Errorf("searchSuggest.ts SEARCH_OPERATORS offers %q:, which the parser does not know — "+
				"it would search the literal text instead", op)
		}
	}
}

// extractOperators returns the operator names (without the colon) of
// SEARCH_OPERATORS' `{ op: "from:", … }` entries, failing loudly when none are
// found, for extractStringArray's reason.
func extractOperators(t *testing.T, src, path string) []string {
	t.Helper()
	decl := regexp.MustCompile(`SEARCH_OPERATORS[^=]*=\s*\[((?s).*?)\];`).FindStringSubmatch(src)
	if decl == nil {
		t.Fatalf("%s: could not find SEARCH_OPERATORS' array literal — has it moved or changed shape?", path)
	}
	items := regexp.MustCompile(`op:\s*"([^":]+):"`).FindAllStringSubmatch(decl[1], -1)
	if len(items) == 0 {
		t.Fatalf("%s: SEARCH_OPERATORS matched but contained no `op: \"name:\"` entries", path)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it[1])
	}
	return out
}

// extractStringArray finds `<name> = [ "a", "b", … ]` in src (TypeScript
// source, read as text — there is no Go parser for it) and returns the quoted
// string literals inside the brackets, in declaration order.
//
// It fails the test loudly — via t.Fatalf, not by returning an empty slice —
// when the declaration cannot be found, or is found but contains no quoted
// strings, so a renamed or reshaped constant breaks this test instead of
// silently making every assertion above vacuously pass on zero values.
func extractStringArray(t *testing.T, src, name, path string) []string {
	t.Helper()
	declPattern := regexp.MustCompile(regexp.QuoteMeta(name) + `\s*=\s*\[([^\]]*)\]`)
	m := declPattern.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s: could not find %s's array literal — has it moved or changed shape?", path, name)
	}
	itemPattern := regexp.MustCompile(`"([^"]*)"`)
	items := itemPattern.FindAllStringSubmatch(m[1], -1)
	if len(items) == 0 {
		t.Fatalf("%s: %s's array literal matched but contained no quoted strings: %q", path, name, m[1])
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it[1])
	}
	return out
}
