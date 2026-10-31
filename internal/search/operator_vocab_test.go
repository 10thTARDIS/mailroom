// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"os"
	"testing"
)

// TestSearchQueryTokenizerKnowsEveryOperator pins web/src/core/searchQuery.ts's
// OPERATOR_NAMES to the parser's own list, IN BOTH DIRECTIONS (#724). That list
// is how the web client decides whether `name:value` is an operator — for the
// search-options form, which lifts operators into fields and leaves everything
// else in "Has the words", and for completion. A name missing there would read
// a real operator as words; an extra one would lift words into a field.
func TestSearchQueryTokenizerKnowsEveryOperator(t *testing.T) {
	const path = "../../web/src/core/searchQuery.ts"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	client := extractStringArray(t, string(src), "OPERATOR_NAMES", path)
	have := map[string]bool{}
	for _, op := range client {
		have[op] = true
	}
	known := map[string]bool{}
	for _, op := range Operators() {
		known[op] = true
		if !have[op] {
			t.Errorf("searchQuery.ts OPERATOR_NAMES is missing %q: the web client would read %s:value as plain words", op, op)
		}
	}
	for _, op := range client {
		if !known[op] {
			t.Errorf("searchQuery.ts OPERATOR_NAMES has %q, which the parser does not know", op)
		}
	}
}
