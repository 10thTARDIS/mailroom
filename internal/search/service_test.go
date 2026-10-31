// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// fakeResolver stands in for the database lookups label:/account: need.
type fakeResolver struct {
	labels   map[string]ulid.ULID
	accounts map[string]ulid.ULID
}

func (r *fakeResolver) LabelIDByName(_ context.Context, _ ulid.ULID, name string) (ulid.ULID, bool, error) {
	id, ok := r.labels[strings.ToLower(name)]
	return id, ok, nil
}

func (r *fakeResolver) AccountIDByName(_ context.Context, _ ulid.ULID, name string) (ulid.ULID, bool, error) {
	id, ok := r.accounts[strings.ToLower(name)]
	return id, ok, nil
}

// serviceFixture wires an Indexer over a fake store and a fake resolver, with
// three messages: one labelled + in an account, one plain, one in the trash.
func serviceFixture(t *testing.T) (*Service, ulid.ULID, map[string]ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	user := testUser(t)

	labelWork := ulid.MustNew(10, strings.NewReader("LABELWORK0123456789abcdef"))
	acctPersonal := ulid.MustNew(11, strings.NewReader("ACCTPERSONAL0123456789abc"))

	fs := newFakeStore()

	work := msg(t, 1, "Invoice 42", "quarterly revenue report")
	work.LabelIDs = []ulid.ULID{labelWork}
	work.AccountIDs = []ulid.ULID{acctPersonal}
	fs.put(user, work, "created")

	plain := msg(t, 2, "Invoice 43", "another invoice")
	fs.put(user, plain, "created")

	trashed := msg(t, 3, "Invoice 44", "deleted invoice")
	trashed.Trashed = true
	trashed.ReceivedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs.put(user, trashed, "created")

	ix, _ := newTestIndexer(t, fs, nil)
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}

	svc := NewService(ix, &fakeResolver{
		labels:   map[string]ulid.ULID{"work": labelWork},
		accounts: map[string]ulid.ULID{"personal": acctPersonal},
	})
	return svc, user, map[string]ulid.ULID{
		"work": work.ID, "plain": plain.ID, "trashed": trashed.ID,
	}
}

func hitSet(res Result) map[ulid.ULID]bool {
	out := map[ulid.ULID]bool{}
	for _, h := range res.Hits {
		out[h.ID] = true
	}
	return out
}

// Trashed mail is excluded by default: a search box that keeps surfacing what the
// user just deleted is a broken search box. Asking for it explicitly still works.
func TestServiceExcludesTrashByDefault(t *testing.T) {
	ctx := context.Background()
	svc, user, ids := serviceFixture(t)

	res, err := svc.Search(ctx, user, "invoice", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := hitSet(res)
	if got[ids["trashed"]] {
		t.Error("trashed mail must not appear in a default search")
	}
	if !got[ids["work"]] || !got[ids["plain"]] {
		t.Errorf("live mail is missing: %d hits", len(res.Hits))
	}

	// Explicitly asking for trash returns it.
	res, err = svc.Search(ctx, user, "invoice is:trashed", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitSet(res); !got[ids["trashed"]] || len(got) != 1 {
		t.Errorf("is:trashed should return exactly the trashed message, got %d hits", len(got))
	}

	// So does the explicit option.
	res, err = svc.Search(ctx, user, "invoice", SearchOptions{IncludeTrashed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 3 {
		t.Errorf("IncludeTrashed should return all three, got %d", len(res.Hits))
	}
}

// RequiresFlag is what Email/query reads to decide a search asked for trashed
// mail (#623). It must say yes only when NOTHING untrashed can match, because
// the caller then restricts the store query to trashed rows.
func TestRequiresFlag(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"is:trashed", true},
		{"is:trash", true},
		{"is:deleted", true},
		{"invoice is:trashed", true},
		{"from:alice@example.com is:trashed invoice", true},
		{"-is:trashed", false},
		{"invoice -is:trashed", false},
		{"invoice", false},
		{"is:junk", false},
		{"trashed", false}, // free text, not the operator
		{"", false},
	} {
		if got := RequiresFlag(Parse(tc.query, ParseOptions{}).Root, FlagTrashed); got != tc.want {
			t.Errorf("RequiresFlag(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
	// A mention under an Or does not REQUIRE the flag: the other branch can
	// match untrashed mail. The parser never builds this, but the JMAP layer
	// builds its own trees, so the rule has to hold for any tree.
	either := Or{Nodes: []Node{ExactTerm(FieldFlag, FlagTrashed), TermNode(FieldAny, "invoice", MatchToken)}}
	if RequiresFlag(either, FlagTrashed) {
		t.Error("RequiresFlag(is:trashed OR invoice) = true, want false")
	}
	if !RequiresFlag(And{Nodes: []Node{either, ExactTerm(FieldFlag, FlagTrashed)}}, FlagTrashed) {
		t.Error("RequiresFlag((is:trashed OR invoice) AND is:trashed) = false, want true")
	}
}

// WithoutRequiredFlag removes exactly what RequiresFlag counts, so the index is
// never asked to filter on a flag its caller answers itself — and nothing else
// is lost on the way.
func TestWithoutRequiredFlag(t *testing.T) {
	trashed := ExactTerm(FieldFlag, FlagTrashed)
	invoice := TermNode(FieldAny, "invoice", MatchToken)
	unread := ExactTerm(FieldFlag, FlagUnread)

	if _, all := WithoutRequiredFlag(trashed, FlagTrashed).(All); !all {
		t.Error("is:trashed alone should leave All")
	}
	for _, tc := range []struct {
		name     string
		in, want Node
	}{
		{"invoice is:trashed", And{Nodes: []Node{invoice, trashed}}, invoice},
		{"invoice is:trashed is:unread", And{Nodes: []Node{invoice, trashed, unread}}, And{Nodes: []Node{invoice, unread}}},
		// Negated and OR'd mentions are not required, so they stay for the index.
		{"invoice -is:trashed", And{Nodes: []Node{invoice, Not{Node: trashed}}}, And{Nodes: []Node{invoice, Not{Node: trashed}}}},
		{"is:trashed OR invoice", Or{Nodes: []Node{trashed, invoice}}, Or{Nodes: []Node{trashed, invoice}}},
	} {
		if got := WithoutRequiredFlag(tc.in, FlagTrashed); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s -> %#v, want %#v", tc.name, got, tc.want)
		}
	}
	// Every query RequiresFlag says yes to must come out with no required flag.
	for _, q := range []string{"is:trashed", "is:deleted invoice", "from:a@example.com is:trash is:unread"} {
		root := Parse(q, ParseOptions{}).Root
		if RequiresFlag(WithoutRequiredFlag(root, FlagTrashed), FlagTrashed) {
			t.Errorf("%q still requires the flag after removal", q)
		}
	}
}

// label: and account: are typed as NAMES and resolved to the ids the index stores.
func TestServiceResolvesNames(t *testing.T) {
	ctx := context.Background()
	svc, user, ids := serviceFixture(t)

	for _, q := range []string{"label:Work", "label:work", "account:personal"} {
		res, err := svc.Search(ctx, user, q, SearchOptions{})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		got := hitSet(res)
		if len(got) != 1 || !got[ids["work"]] {
			t.Errorf("%q: got %d hits, want exactly the labelled message", q, len(got))
		}
	}
}

// THE dangerous case: an unresolvable name must match NOTHING. Dropping the
// clause instead would quietly widen `label:Typo` into "the entire mailbox".
func TestServiceUnknownNameMatchesNothing(t *testing.T) {
	ctx := context.Background()
	svc, user, _ := serviceFixture(t)

	for _, q := range []string{"label:DoesNotExist", "account:nope", "invoice label:Ghost"} {
		res, err := svc.Search(ctx, user, q, SearchOptions{})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(res.Hits) != 0 {
			t.Errorf("%q: an unknown name must match nothing, got %d hits", q, len(res.Hits))
		}
	}
}

// A negated unknown name is the mirror case: NOT(nothing) is everything, so the
// query must not collapse to empty.
func TestServiceNegatedUnknownNameMatchesEverything(t *testing.T) {
	ctx := context.Background()
	svc, user, _ := serviceFixture(t)

	res, err := svc.Search(ctx, user, "invoice -label:DoesNotExist", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 { // both live messages; trash is excluded by default
		t.Fatalf("got %d hits, want 2", len(res.Hits))
	}
}

func TestServiceSnippets(t *testing.T) {
	ctx := context.Background()
	svc, user, ids := serviceFixture(t)

	snips, err := svc.Snippets(ctx, user, "quarterly", []ulid.ULID{ids["work"], ids["plain"]}, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snips[ids["work"]]) == 0 {
		t.Fatal("no snippet for the matching message")
	}
	if len(snips[ids["plain"]]) != 0 {
		t.Error("a non-matching message must have no snippet")
	}
	var marked string
	for _, s := range snips[ids["work"]] {
		marked += s.Mark()
	}
	if !strings.Contains(marked, "<mark>quarterly</mark>") {
		t.Errorf("snippet not marked: %q", marked)
	}
}

// Reindex is reachable from the service — it is a first-class operation (spec §7).
func TestServiceReindex(t *testing.T) {
	ctx := context.Background()
	svc, user, _ := serviceFixture(t)

	prog, err := svc.Reindex(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if !prog.Rebuilt || prog.Indexed != 3 {
		t.Fatalf("got %+v, want a rebuild of 3 documents", prog)
	}
	st, err := svc.Stats(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if st.Documents != 3 || st.Lag != 0 {
		t.Fatalf("after reindex: %+v", st)
	}
}

// Fail closed: no identity, no search.
func TestServiceRejectsZeroUser(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := serviceFixture(t)
	if _, err := svc.Search(ctx, ulid.ULID{}, "invoice", SearchOptions{}); err == nil {
		t.Fatal("a zero user id must be rejected")
	}
}
