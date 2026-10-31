// SPDX-License-Identifier: AGPL-3.0-only

package census

import (
	"reflect"
	"sort"
	"testing"
)

// TestReaderMethodSetIsExactlyReadOnly pins Reader's method set. It exists so
// that adding a method to Reader — which could reintroduce a write path — is
// a change this test forces a reviewer to notice and consciously approve,
// rather than something that slips in as an incidental part of an unrelated
// feature. See the Reader doc comment: the read-only guarantee is that this
// interface has no method that can mutate server state.
func TestReaderMethodSetIsExactlyReadOnly(t *testing.T) {
	typ := reflect.TypeOf((*Reader)(nil)).Elem()

	want := []string{"Capabilities", "ListMailboxes", "Examine", "Close"}
	sort.Strings(want)

	got := make([]string, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		got[i] = typ.Method(i).Name
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("census.Reader method set = %v, want exactly %v\n\n"+
			"This is not an ordinary test failure: Reader's whole point is that it has no\n"+
			"method that can change server state (no STORE/EXPUNGE/APPEND/COPY/MOVE/CREATE/\n"+
			"DELETE/RENAME/SUBSCRIBE). Adding a method here is a DESIGN CHANGE to that\n"+
			"guarantee and needs to be reviewed as one — update this test's `want` slice\n"+
			"only once that review has actually happened.", got, want)
	}
}
