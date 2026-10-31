// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"testing"

	"mailroom/internal/search"
)

// The server half of the search box (#724): the as-you-type modifier on the
// `query` condition, and the relevance sort. Pure — no database, no engine.

// finalTerm digs the free-text term for the query's last word out of a parsed
// tree: a one-word query parses to the Term itself.
func finalTerm(t *testing.T, n search.Node) search.Term {
	t.Helper()
	switch v := n.(type) {
	case search.Term:
		return v
	case search.And:
		return finalTerm(t, v.Nodes[len(v.Nodes)-1])
	}
	t.Fatalf("no term in %#v", n)
	return search.Term{}
}

// queryPrefix reaches the parser: the final bare word becomes a prefix match,
// and without it the same word is the ordinary whole-word match.
func TestQueryPrefixReachesTheParser(t *testing.T) {
	with := searchFilter{Query: "from:alice invo", QueryPrefix: true}
	if got := finalTerm(t, with.ast(true).Root); got.Match != search.MatchPrefix || got.Value != "invo" {
		t.Fatalf("queryPrefix: final word = %+v, want a prefix match on %q", got, "invo")
	}
	without := searchFilter{Query: "from:alice invo"}
	if got := finalTerm(t, without.ast(true).Root); got.Match == search.MatchPrefix {
		t.Fatalf("no queryPrefix: final word must not be a prefix match, got %+v", got)
	}
	// SearchSnippet/get highlights with the same reading of the query.
	if got := finalTerm(t, with.snippetQuery(true).Root); got.Match != search.MatchPrefix {
		t.Fatalf("snippet query lost the prefix: %+v", got)
	}
}

// queryPrefix arrives through the JMAP filter object, is allowlisted (an
// unknown key is unsupportedFilter), and is a modifier — on its own it is not a
// text condition and must not send an empty search to the index.
func TestQueryPrefixIsAnAllowlistedModifier(t *testing.T) {
	if !emailFilterConditions["queryPrefix"] {
		t.Fatal("queryPrefix must be an allowlisted filter condition")
	}
	if deltaStableConditions["queryPrefix"] {
		t.Fatal("queryPrefix belongs to a text search, which is never delta-stable")
	}
	if !(searchFilter{QueryPrefix: true}).empty() {
		t.Error("queryPrefix alone is not a condition")
	}
	plan, mErr := parseEmailQuery(emailQueryArgs{Filter: json.RawMessage(`{"query":"invo","queryPrefix":true}`)})
	if mErr != nil {
		t.Fatalf("parse: %+v", mErr)
	}
	if !plan.text.QueryPrefix {
		t.Fatal("queryPrefix was not decoded from the filter")
	}
}

// "relevance" is a sort property Email/query accepts; it asks the store for the
// index's order and is never delta-stable.
func TestRelevanceSortParses(t *testing.T) {
	asc := true
	plan, mErr := parseEmailQuery(emailQueryArgs{
		Filter: json.RawMessage(`{"query":"invoice"}`),
		Sort:   []sortComparator{{Property: "relevance"}},
	})
	if mErr != nil {
		t.Fatalf("parse: %+v", mErr)
	}
	if !plan.query.SortByRank {
		t.Fatal("relevance sort did not set SortByRank")
	}
	if plan.stable {
		t.Fatal("a relevance sort cannot be delta-stable")
	}
	if plan.query.SortAscending {
		t.Fatal("relevance defaults to best first, not ascending")
	}
	plan, mErr = parseEmailQuery(emailQueryArgs{
		Filter: json.RawMessage(`{"query":"invoice"}`),
		Sort:   []sortComparator{{Property: "relevance", IsAscending: &asc}},
	})
	if mErr != nil || !plan.query.SortByRank || !plan.query.SortAscending {
		t.Fatalf("isAscending must carry through: %+v %+v", plan.query, mErr)
	}
	// The plain date sort does not rank.
	plan, _ = parseEmailQuery(emailQueryArgs{Sort: []sortComparator{{Property: "receivedAt"}}})
	if plan.query.SortByRank {
		t.Fatal("receivedAt must not set SortByRank")
	}
}

func TestSearchCapabilityAdvertisesRelevance(t *testing.T) {
	if v, _ := searchCapability()["relevanceSort"].(bool); !v {
		t.Fatal("the session must advertise relevanceSort, or a client cannot know to offer it")
	}
}
