// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mailroom/internal/search"
	"mailroom/internal/search/tantivy"
)

// from:<domain> and the bare-domain phrase (#712), against the REAL engine. The
// in-memory backend answers the same cases in internal/search/domain_test.go;
// this is where the claim "the n-gram clause no longer swallows the mailbox" is
// checked against the engine that has the n-gram clause.

func TestTantivyDomainSearch(t *testing.T) {
	b, err := tantivy.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	at := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	mk := func(seed int64, key, from string, to []string, subject, body string) search.Document {
		d := search.Document{
			ID: mkID(seed, key), From: search.Address{Address: from},
			Subject: subject, Body: body, ReceivedAt: at,
		}
		for _, a := range to {
			d.To = append(d.To, search.Address{Address: a})
		}
		return d
	}
	named := map[string]search.Document{
		"apex":      mk(1, "APX", "orders@shop.example", []string{"you@example.net"}, "Your order", "Thanks."),
		"sub":       mk(2, "SUB", "news@mail.shop.example", []string{"you@example.net"}, "Spring sale", "Savings."),
		"lookalike": mk(3, "LKA", "orders@notshop.example", nil, "Not them", "Lookalike."),
		"evil":      mk(4, "EVL", "orders@shop.example.evil", nil, "Not them either", "Suffix lookalike."),
		"recipient": mk(5, "RCP", "orders@elsewhere.example", []string{"billing@shop.example"}, "Receipt", "Details."),
		"mention":   mk(6, "MEN", "friend@example.net", nil, "A deal", "Check out shop.example today."),
		"scattered": mk(7, "SCT", "friend@example.net", nil, "Words", "the example of a shop that has no domain"),
	}
	var docs []search.Document
	for _, d := range named {
		docs = append(docs, d)
	}
	// Filler mail from unrelated senders, enough that "matches most of the
	// mailbox" is visible: every one shares n-grams with the domain's name.
	for i := 0; i < 60; i++ {
		docs = append(docs, mk(int64(100+i), fmt.Sprintf("F%02d", i),
			fmt.Sprintf("person%d@example.com", i), []string{"you@example.net"},
			fmt.Sprintf("Weekly digest %d", i), "Nothing to see here."))
	}
	if err := b.Upsert(context.Background(), docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	ids := func(keys ...string) map[string]bool {
		out := map[string]bool{}
		for _, k := range keys {
			out[named[k].ID.String()] = true
		}
		return out
	}
	got := func(q string) map[string]bool {
		out := map[string]bool{}
		for id := range hits(t, b, q) {
			out[id.String()] = true
		}
		return out
	}
	same := func(a, b map[string]bool) bool {
		if len(a) != len(b) {
			return false
		}
		for k := range a {
			if !b[k] {
				return false
			}
		}
		return true
	}

	tests := []struct {
		name string
		q    string
		want map[string]bool
	}{
		{"domain and subdomains", "from:shop.example", ids("apex", "sub")},
		{"wildcard form", "from:*.shop.example", ids("apex", "sub")},
		{"at form", "from:@shop.example", ids("apex", "sub")},
		{"case-insensitive", "from:SHOP.Example", ids("apex", "sub")},
		{"a subdomain narrows", "from:mail.shop.example", ids("sub")},
		{"exact address stays exact", "from:orders@shop.example", ids("apex")},
		{"exact address is not its sibling subdomain", "from:news@shop.example", ids()},
		{"no @ and no dot is as before", "from:orders", ids()},
		{"to: matches the recipient domain", "to:shop.example", ids("recipient")},
		{"cc: is the same field", "cc:*.shop.example", ids("recipient")},
		{"to: example.net finds the recipients", "to:example.net", func() map[string]bool {
			m := ids("apex", "sub")
			for _, d := range docs {
				if len(d.To) > 0 && d.To[0].Address == "you@example.net" {
					m[d.ID.String()] = true
				}
			}
			return m
		}()},
		// The bare dotted word is a phrase: the adjacent words in order, in any
		// field. Not "notshop", not the scattered words, and not the fillers
		// the n-gram clause used to drag in.
		{"bare domain is a phrase", "shop.example", ids("apex", "sub", "evil", "recipient", "mention")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if g := got(tc.q); !same(g, tc.want) {
				t.Errorf("%q: got %d hits, want %d", tc.q, len(g), len(tc.want))
				for id := range g {
					if !tc.want[id] {
						t.Errorf("  unexpected hit %s", id)
					}
				}
				for id := range tc.want {
					if !g[id] {
						t.Errorf("  missing hit %s", id)
					}
				}
			}
		})
	}
}

// A bare address typed as free text (#728) is an exact address match, not a fuzzy
// token query: it must not drag in near-miss addresses (a stemmed local part
// included, which a phrase would still have matched).
func TestTantivyBareAddressIsExact(t *testing.T) {
	b, err := tantivy.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	at := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	mk := func(seed int64, key, from, subject, body string) search.Document {
		return search.Document{
			ID: mkID(seed, key), From: search.Address{Address: from},
			Subject: subject, Body: body, ReceivedAt: at,
		}
	}
	named := map[string]search.Document{
		"exact":    mk(1, "EXA", "alice@example.com", "Hello", "Hi."),
		"mention":  mk(2, "MEN", "bob@example.net", "Intro", "Please write to alice@example.com about it."), // body mention: not an address match
		"othertld": mk(3, "OTL", "alice@example.org", "Near miss", "Different TLD."),
		"typo":     mk(4, "TYP", "alices@example.com", "Near miss", "Different local part."),
		"sibling":  mk(5, "SIB", "alice@example.co", "Near miss", "Different domain."),
		"other":    mk(6, "OTH", "carol@example.com", "Near miss", "Same domain."),
	}
	var docs []search.Document
	for _, d := range named {
		docs = append(docs, d)
	}
	for i := 0; i < 30; i++ {
		docs = append(docs, mk(int64(100+i), fmt.Sprintf("F%02d", i),
			fmt.Sprintf("person%d@example.com", i), fmt.Sprintf("Digest %d", i), "Nothing."))
	}
	if err := b.Upsert(context.Background(), docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := map[string]bool{}
	for id := range hits(t, b, "alice@example.com") {
		got[id.String()] = true
	}
	want := map[string]bool{named["exact"].ID.String(): true}
	if len(got) != len(want) {
		t.Errorf("alice@example.com: got %d hits, want %d", len(got), len(want))
	}
	for id := range got {
		if !want[id] {
			t.Errorf("unexpected near-miss hit %s", id)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("missing hit %s", id)
		}
	}
}
