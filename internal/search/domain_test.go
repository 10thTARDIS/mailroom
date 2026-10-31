// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// from:<domain> (#712). The parser cases are pure; the matching cases run against
// the in-memory backend, and the SAME cases run against Tantivy in
// internal/search/tantivy (TestTantivyDomainSearch).

func TestAddressDomains(t *testing.T) {
	tests := []struct {
		addr string
		want []string
	}{
		{"alice@shop.example", []string{"shop.example"}},
		{"Alice@Mail.Shop.Example", []string{"mail.shop.example", "shop.example"}},
		{"a@x.y.shop.example", []string{"x.y.shop.example", "y.shop.example", "shop.example"}},
		{"a@localhost", []string{"localhost"}},
		{"a@shop.example.", []string{"shop.example"}},
		{"no-at-sign", nil},
		{"trailing@", nil},
		{"", nil},
	}
	for _, tc := range tests {
		got := addressDomains(tc.addr)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("addressDomains(%q) = %v, want %v", tc.addr, got, tc.want)
		}
		for _, v := range got {
			if v == "example" || v == "com" {
				t.Errorf("addressDomains(%q) emitted a bare TLD %q", tc.addr, v)
			}
		}
	}
}

func TestParseAddressDomain(t *testing.T) {
	dom := func(f Field, v string) Node { return Term{Field: f, Value: v, Match: MatchExact, Boost: 1} }
	tests := []struct {
		name string
		in   string
		want Node
	}{
		{"bare domain", "from:shop.example", dom(FieldFromDomain, "shop.example")},
		{"wildcard form", "from:*.shop.example", dom(FieldFromDomain, "shop.example")},
		{"at form", "from:@shop.example", dom(FieldFromDomain, "shop.example")},
		{"case folded", "from:Shop.EXAMPLE", dom(FieldFromDomain, "shop.example")},
		{"subdomain", "from:mail.shop.example", dom(FieldFromDomain, "mail.shop.example")},
		{"to", "to:shop.example", dom(FieldToDomain, "shop.example")},
		{"cc folds into to", "cc:*.shop.example", dom(FieldToDomain, "shop.example")},
		{"recipient", "recipient:@shop.example", dom(FieldToDomain, "shop.example")},
		{"address stays exact", "from:alice@shop.example", dom(FieldFrom, "alice@shop.example")},
		{"to address stays exact", "to:alice@shop.example", dom(FieldTo, "alice@shop.example")},
		{"no dot, no @ is as before", "from:alice", dom(FieldFrom, "alice")},
		{"negation", "-from:shop.example", Not{Node: dom(FieldFromDomain, "shop.example")}},
		{"empty value is as before", "from:", dom(FieldFrom, "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := ParseChecked(tc.in, ParseOptions{})
			if err != nil {
				t.Fatalf("ParseChecked(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(q.Root, tc.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tc.in, q.Root, tc.want)
			}
		})
	}
}

func TestParseAddressSyntaxErrors(t *testing.T) {
	for _, in := range []string{
		"from:*foo", "from:foo*", "from:*", "from:*.", "from:@", "from:.",
		"from:alice@", "from:*.com", "from:.com", "from:a..b.example",
		"from:shop.example/x", "to:*foo", "cc:@", "recipient:*.*.example",
		"from:*.shop.*",
		// a bare TLD behind the @ marker, for every operator (#728)
		"from:@com", "to:@com", "cc:@org", "recipient:@com", "alias:@com", "deliveredto:@net",
	} {
		q, err := ParseChecked(in, ParseOptions{})
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("ParseChecked(%q) error = %v, want a *SyntaxError", in, err)
			continue
		}
		if se.Reason == "" || se.Error() == "" {
			t.Errorf("ParseChecked(%q): the error has no reason", in)
		}
		// Parse itself never fails: the clause matches nothing.
		if _, ok := q.Root.(None); !ok {
			t.Errorf("Parse(%q).Root = %#v, want None", in, q.Root)
		}
		if _, ok := Parse(in, ParseOptions{}).Root.(None); !ok {
			t.Errorf("Parse(%q) should degrade to None", in)
		}
	}
	// The valid clauses next to a bad one still parse; the error is reported.
	q, err := ParseChecked("invoice from:*x", ParseOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if and, ok := q.Root.(And); !ok || len(and.Nodes) != 2 {
		t.Fatalf("root = %#v, want an And of both clauses", q.Root)
	}
}

func TestParseBareDomainIsAPhrase(t *testing.T) {
	phrase := func(v string) Node { return TermNode(FieldAny, v, MatchPhrase) }
	addrOr := func(v string) Node {
		return Or{Nodes: []Node{ExactTerm(FieldFrom, v), ExactTerm(FieldTo, v), ExactTerm(FieldDeliveredTo, v)}}
	}
	tests := []struct {
		in   string
		want Node
	}{
		{"shop.example", phrase("shop.example")},
		{"mail.shop.example", phrase("mail.shop.example")},
		{"my-shop.example.com", phrase("my-shop.example.com")},
		{"shop.example*", TermNode(FieldAny, "shop.example", MatchPrefix)},
		{"invoice", TermNode(FieldAny, "invoice", MatchToken)},
		// A complete bare address is exact on the address fields (#728).
		{"alice@shop.example", addrOr("alice@shop.example")},
		{"first.last+tag@shop.example", addrOr("first.last+tag@shop.example")},
		// Not hostnames or addresses: other punctuation keeps the old treatment.
		{"alice@", TermNode(FieldAny, "alice@", MatchToken)},
		{"@shop.example", TermNode(FieldAny, "@shop.example", MatchToken)},
		{"alice@shop", TermNode(FieldAny, "alice@shop", MatchToken)},
		{"a@b@shop.example", TermNode(FieldAny, "a@b@shop.example", MatchToken)},
		{"https://shop.example/x", TermNode(FieldAny, "https://shop.example/x", MatchToken)},
		{"shop..example", TermNode(FieldAny, "shop..example", MatchToken)},
		{"3.14!", TermNode(FieldAny, "3.14!", MatchToken)},
	}
	for _, tc := range tests {
		if got := Parse(tc.in, ParseOptions{}).Root; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Parse(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	// A phrase is never expanded with the typo-tolerance n-gram clause: that
	// clause was what made `shop.example` match most of a mailbox.
	exp := Expand(Parse("shop.example", ParseOptions{}))
	if UsesField(exp, FieldFuzzy) {
		t.Errorf("a bare domain must not reach the fuzzy field: %#v", exp)
	}
	if UsesField(Expand(Parse("alice@shop.example", ParseOptions{})), FieldFuzzy) {
		t.Error("a bare address must not reach the fuzzy field")
	}
}

// domainCorpus builds one message per sender shape that matters, in the
// in-memory backend. The names are keys of the returned map.
func domainCorpus(t *testing.T) (*MemoryBackend, map[string]Document) {
	t.Helper()
	at := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	mk := func(seed int64, from string, to []string, subject, body string) Document {
		d := Document{ID: mkID(t, seed), From: Address{Address: from}, Subject: subject, Body: body, ReceivedAt: at}
		for _, a := range to {
			d.To = append(d.To, Address{Address: a})
		}
		return d
	}
	docs := map[string]Document{
		"apex":      mk(1, "orders@shop.example", []string{"you@example.net"}, "Your order", "Thanks."),
		"sub":       mk(2, "news@mail.shop.example", []string{"you@example.net"}, "Spring sale", "Savings."),
		"deep":      mk(3, "x@a.b.shop.example", nil, "Deep", "Deep."),
		"lookalike": mk(4, "orders@notshop.example", nil, "Not them", "Lookalike."),
		"evil":      mk(5, "orders@shop.example.evil", nil, "Not them either", "Suffix lookalike."),
		"other":     mk(6, "orders@elsewhere.example", []string{"billing@shop.example"}, "Receipt", "See shop.example for details."),
		"mention":   mk(7, "friend@example.net", []string{"you@example.net"}, "Fwd: a deal", "Check out shop.example today, and shop and example separately."),
		"scattered": mk(8, "friend@example.net", nil, "Words", "the example of a shop that has no domain"),
	}
	b := NewMemoryBackend()
	var list []Document
	for _, d := range docs {
		list = append(list, d)
	}
	if err := b.Upsert(context.Background(), list); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return b, docs
}

func TestFromDomainMatchesDomainAndSubdomainsOnly(t *testing.T) {
	b, docs := domainCorpus(t)
	want := map[string]bool{docs["apex"].ID.String(): true, docs["sub"].ID.String(): true, docs["deep"].ID.String(): true}
	for _, in := range []string{"from:shop.example", "from:*.shop.example", "from:@shop.example", "from:SHOP.example"} {
		got := runIDs(t, b, in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q matched %v, want apex+sub+deep only (lookalikes excluded)", in, got)
		}
	}
	if got := runIDs(t, b, "from:mail.shop.example"); !reflect.DeepEqual(got, map[string]bool{docs["sub"].ID.String(): true}) {
		t.Errorf("from:mail.shop.example matched %v, want only the subdomain sender", got)
	}
	// Exact address behaviour is unchanged.
	if got := runIDs(t, b, "from:orders@shop.example"); !reflect.DeepEqual(got, map[string]bool{docs["apex"].ID.String(): true}) {
		t.Errorf("from:orders@shop.example matched %v, want exactly the one address", got)
	}
	if got := runIDs(t, b, "from:news@shop.example"); len(got) != 0 {
		t.Errorf("an exact address must not match its subdomain sibling: %v", got)
	}
	// from:alice (no @, no dot) still means "that exact address", i.e. nothing.
	if got := runIDs(t, b, "from:orders"); len(got) != 0 {
		t.Errorf("from:orders matched %v; it must keep matching only an exact address", got)
	}
	// Negation composes.
	if got := runIDs(t, b, "-from:shop.example from:orders@notshop.example"); !reflect.DeepEqual(got, map[string]bool{docs["lookalike"].ID.String(): true}) {
		t.Errorf("negated domain matched %v", got)
	}
}

func TestToDomainMatchesRecipientDomain(t *testing.T) {
	b, docs := domainCorpus(t)
	want := map[string]bool{docs["other"].ID.String(): true}
	for _, in := range []string{"to:shop.example", "cc:*.shop.example", "to:@shop.example"} {
		if got := runIDs(t, b, in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q matched %v, want the one message addressed to the domain", in, got)
		}
	}
	if got := runIDs(t, b, "to:example.net"); len(got) != 3 {
		t.Errorf("to:example.net matched %d, want 3", len(got))
	}
}

func TestBareDomainSearchesAsAPhrase(t *testing.T) {
	b, docs := domainCorpus(t)
	got := runIDs(t, b, "shop.example")
	// Every message that carries the adjacent words "shop example": the three
	// senders at the domain, the recipient at it, the body mention — and, by the
	// nature of a phrase over analysed text, the lookalike that shares the words.
	for _, k := range []string{"apex", "sub", "deep", "other", "mention"} {
		if !got[docs[k].ID.String()] {
			t.Errorf("bare shop.example missed %s", k)
		}
	}
	// "notshop" is a different token; "the example of a shop" is the words out of
	// order and apart — exactly what the old token query matched.
	for _, k := range []string{"lookalike", "scattered"} {
		if got[docs[k].ID.String()] {
			t.Errorf("bare shop.example wrongly matched %s", k)
		}
	}
	// And the old behaviour is gone: the parts are no longer matched separately.
	old := runIDs(t, b, "shop example")
	if !old[docs["scattered"].ID.String()] {
		t.Fatal("control: the two-word query should still match the scattered words")
	}
}

// runIDs runs a query and returns hit ids as strings.
func runIDs(t *testing.T, b *MemoryBackend, q string) map[string]bool {
	t.Helper()
	res, err := b.Search(context.Background(), Parse(q, ParseOptions{}))
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	got := map[string]bool{}
	for _, h := range res.Hits {
		got[h.ID.String()] = true
	}
	return got
}
