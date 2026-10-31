// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"testing"

	"mailroom/internal/search"
)

// A query clause whose syntax can match nothing is invalidArguments through the
// ordinary JMAP error path, not an empty list (#712). Negative control: the valid
// forms next to it must NOT be refused.
func TestEmailQueryRejectsASearchClauseThatCanMatchNothing(t *testing.T) {
	parse := func(query string) *MethodError {
		raw, err := json.Marshal(map[string]string{"query": query})
		if err != nil {
			t.Fatal(err)
		}
		_, mErr := parseEmailQuery(emailQueryArgs{Filter: raw})
		return mErr
	}
	for _, bad := range []string{"from:*foo", "to:@", "invoice from:*.", "cc:foo*", "from:alice@"} {
		mErr := parse(bad)
		if mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("query %q: got %v, want invalidArguments", bad, mErr)
			continue
		}
		if mErr.Description == "" {
			t.Errorf("query %q: the error carries no description", bad)
		}
	}
	for _, ok := range []string{
		"from:shop.example", "from:*.shop.example", "from:@shop.example",
		"from:alice@shop.example", "from:alice", "from:", "shop.example", "is:unread",
	} {
		if mErr := parse(ok); mErr != nil {
			t.Errorf("query %q was refused: %v", ok, mErr)
		}
	}
}

// The RFC 8621 from/to/cc conditions also reach a bare domain's mail (#712), and
// an address stays exact.
func TestAddressClauseDomain(t *testing.T) {
	has := func(n search.Node, f search.Field) bool {
		or, ok := n.(search.Or)
		if !ok {
			return false
		}
		for _, c := range or.Nodes {
			if tm, ok := c.(search.Term); ok && tm.Field == f {
				return true
			}
		}
		return false
	}
	if n := addressClause(search.FieldFrom, "shop.example"); !has(n, search.FieldFromDomain) {
		t.Errorf("from: shop.example lacks the domain branch: %#v", n)
	}
	if n := addressClause(search.FieldTo, "*.shop.example"); !has(n, search.FieldToDomain) {
		t.Errorf("to: *.shop.example lacks the domain branch: %#v", n)
	}
	if n := addressClause(search.FieldFrom, "alice@shop.example"); has(n, search.FieldFromDomain) {
		t.Errorf("an address must stay exact: %#v", n)
	}
	if n := addressClause(search.FieldFrom, "Acme Billing"); has(n, search.FieldFromDomain) {
		t.Errorf("a display name must not gain a domain branch: %#v", n)
	}
}
