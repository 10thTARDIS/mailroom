// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// The v1.88.0 operators (#722, #723): OR and brackets, relative dates, list:,
// has:unsubscribe, filename:, larger:/smaller:, alias:/deliveredto:, the
// attachment-type families, and the store-side is:/area:/in: values — parsed,
// and (for everything the index answers) run against the in-memory backend.

func word(v string) Node { return TermNode(FieldAny, v, MatchToken) }

func TestParseOrAndBrackets(t *testing.T) {
	a, b, c, d := word("a"), word("b"), word("c"), word("d")
	tests := []struct {
		in   string
		want Node
	}{
		{"a OR b", Or{Nodes: []Node{a, b}}},
		// OR binds looser than the implicit AND.
		{"a b OR c", Or{Nodes: []Node{And{Nodes: []Node{a, b}}, c}}},
		{"a OR b c", Or{Nodes: []Node{a, And{Nodes: []Node{b, c}}}}},
		{"a OR b OR c", Or{Nodes: []Node{a, b, c}}},
		{"(a OR b) c", And{Nodes: []Node{Or{Nodes: []Node{a, b}}, c}}},
		{"(a OR b) -c", And{Nodes: []Node{Or{Nodes: []Node{a, b}}, Not{Node: c}}}},
		{"-(a OR b)", Not{Node: Or{Nodes: []Node{a, b}}}},
		{"-(a b) c", And{Nodes: []Node{Not{Node: And{Nodes: []Node{a, b}}}, c}}},
		{"(a (b OR c)) OR d", Or{Nodes: []Node{And{Nodes: []Node{a, Or{Nodes: []Node{b, c}}}}, d}}},
		{"((a))", a},
		// Only an upper-case, bare OR is the operator.
		{"a or b", And{Nodes: []Node{a, word("or"), b}}},
		{`a "OR" b`, And{Nodes: []Node{a, TermNode(FieldAny, "OR", MatchPhrase), b}}},
		{"a -OR b", And{Nodes: []Node{a, Not{Node: word("OR")}, b}}},
		// Operators inside groups.
		{"(from:a@x.example OR label:Work)", Or{Nodes: []Node{
			ExactTerm(FieldFrom, "a@x.example"),
			Term{Field: FieldLabel, Value: "Work", Match: MatchExact, Boost: 1},
		}}},
	}
	for _, tc := range tests {
		if got := Parse(tc.in, ParseOptions{}).Root; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Parse(%q):\n got  %#v\n want %#v", tc.in, got, tc.want)
		}
	}
}

// A pasted query must never become an empty result or an error because of a
// bracket or a dangling OR (#722).
func TestParseUnbalancedDegradesGracefully(t *testing.T) {
	a, b := word("a"), word("b")
	tests := []struct {
		in   string
		want Node
	}{
		{"(a OR b", Or{Nodes: []Node{a, b}}},          // never closed: runs to the end
		{"a OR b)", Or{Nodes: []Node{a, word("b)")}}}, // closes nothing: kept in the word, analysed away
		{"a )", a},                             // a lone stray bracket is dropped
		{")", All{}},                           // nothing left: matches everything, not nothing
		{"()", All{}},                          // an empty group says nothing
		{"-()", All{}},                         // nor does its negation
		{"a ()", a},                            //
		{"a OR", a},                            // dangling OR while typing
		{"OR a", a},                            //
		{"a OR OR b", Or{Nodes: []Node{a, b}}}, //
		{"((a OR b", Or{Nodes: []Node{a, b}}},  //
		{"a) (b", And{Nodes: []Node{word("a)"), b}}}, // the stray ')' stays in its word
	}
	for _, tc := range tests {
		q, err := ParseChecked(tc.in, ParseOptions{})
		if err != nil {
			t.Errorf("ParseChecked(%q) = %v; brackets must never be a syntax error", tc.in, err)
		}
		if !reflect.DeepEqual(q.Root, tc.want) {
			t.Errorf("Parse(%q):\n got  %#v\n want %#v", tc.in, q.Root, tc.want)
		}
	}
	// A bracket INSIDE a word is part of the word: a pasted URL stays one term.
	for _, in := range []string{"https://example.com/wiki/Foo_(bar)", "f(x)", "label:(x)"} {
		got := Parse(in, ParseOptions{}).Root
		if term, ok := got.(Term); !ok || term.Value != in && term.Value != "(x)" {
			t.Errorf("Parse(%q) = %#v, want one term carrying the brackets", in, got)
		}
	}
}

// The "a OR b)" case keeps the ')' in the word; prove the engine still finds b
// (the analyser drops the punctuation), so the paste is not an empty result.
func TestStrayBracketStillSearches(t *testing.T) {
	b, docs := corpus(t)
	got := run(t, b, "lunch)")
	if !got[docs["lunch"].ID] {
		t.Fatalf(`"lunch)" found %v, want the lunch message`, got)
	}
	got = run(t, b, "(invoice OR lunch")
	if !got[docs["lunch"].ID] || !got[docs["invoice"].ID] {
		t.Fatalf(`"(invoice OR lunch" found %v, want both`, got)
	}
}

func TestOrAgainstTheBackend(t *testing.T) {
	b, docs := corpus(t)
	cases := []struct {
		q    string
		want []string
	}{
		{"lunch OR revenue", []string{"lunch", "invoice"}},
		{"(lunch OR revenue) -is:starred", []string{"invoice"}},
		{"-(lunch OR revenue)", []string{"trashed"}},
		{"invoice OR", []string{"invoice", "trashed"}},
	}
	for _, tc := range cases {
		got := runExact(t, b, tc.q)
		if len(got) != len(tc.want) {
			t.Errorf("%q matched %d, want %v", tc.q, len(got), tc.want)
		}
		for _, k := range tc.want {
			if !got[docs[k].ID] {
				t.Errorf("%q missed %s", tc.q, k)
			}
		}
	}
}

// runExact is run without typo tolerance, so a count asserts on the operators
// rather than on which n-grams happen to overlap.
func runExact(t *testing.T, b *MemoryBackend, q string) map[ulid.ULID]bool {
	t.Helper()
	res, err := b.Search(context.Background(), Parse(q, ParseOptions{NoFuzzy: true}))
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	got := map[ulid.ULID]bool{}
	for _, h := range res.Hits {
		got[h.ID] = true
	}
	return got
}

func TestParsePrefixModeWithOr(t *testing.T) {
	got := Parse("lunch OR inv", ParseOptions{Prefix: true}).Root
	want := Or{Nodes: []Node{word("lunch"), TermNode(FieldAny, "inv", MatchPrefix)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseRelativeDates(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	now := time.Date(2026, 3, 31, 15, 30, 0, 0, time.UTC)
	tests := []struct {
		in   string
		now  time.Time
		want Node
	}{
		{"newer_than:1d", now, DateRange{From: day(2026, 3, 30)}},
		{"newer_than:0d", now, DateRange{From: day(2026, 3, 31)}},
		{"older_than:2w", now, DateRange{To: day(2026, 3, 17)}},
		{"older_than:10D", now, DateRange{To: day(2026, 3, 21)}},
		// Calendar months: 31 March minus one month is the last day of February.
		{"newer_than:1m", now, DateRange{From: day(2026, 2, 28)}},
		{"newer_than:13m", now, DateRange{From: day(2025, 2, 28)}},
		{"older_than:3m", now, DateRange{To: day(2025, 12, 31)}},
		// And years: a leap day minus a year is 28 February.
		{"older_than:1y", time.Date(2028, 2, 29, 1, 0, 0, 0, time.UTC), DateRange{To: day(2027, 2, 28)}},
		// The cutoff is the UTC day, whatever zone "now" is in.
		{"newer_than:1d", time.Date(2026, 4, 1, 1, 0, 0, 0, time.FixedZone("x", 3*3600)), DateRange{From: day(2026, 3, 30)}},
	}
	for _, tc := range tests {
		q, err := ParseChecked(tc.in, ParseOptions{Now: tc.now})
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(q.Root, tc.want) {
			t.Errorf("%q at %s: got %#v, want %#v", tc.in, tc.now, q.Root, tc.want)
		}
	}
}

// Invalid relative dates are refused with a reason (#712's way), and an empty
// value mid-keystroke matches nothing quietly.
func TestParseRelativeDateErrors(t *testing.T) {
	for _, in := range []string{
		"older_than:3x", "older_than:d", "newer_than:-1d", "newer_than:+1d",
		"older_than:12345d", "newer_than:1.5d", "older_than:1 d", "newer_than:week",
	} {
		_, err := ParseChecked(in, ParseOptions{})
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("ParseChecked(%q) = %v, want a *SyntaxError", in, err)
		}
	}
	q, err := ParseChecked("newer_than:", ParseOptions{})
	if err != nil {
		t.Errorf("an empty value is mid-keystroke, not an error: %v", err)
	}
	if _, ok := q.Root.(None); !ok {
		t.Errorf("newer_than: with no value = %#v, want None", q.Root)
	}
}

// newer_than:N and older_than:N partition the mailbox: every message is in
// exactly one of them, for every N and unit.
func TestRelativeDatesPartitionTheMailbox(t *testing.T) {
	now := time.Date(2026, 3, 31, 15, 30, 0, 0, time.UTC)
	b := NewMemoryBackend()
	var docs []Document
	for i := 0; i < 400; i++ {
		d := Document{ID: mkID(t, int64(1000+i)), Subject: "x",
			ReceivedAt: now.Add(-time.Duration(i) * 23 * time.Hour)}
		docs = append(docs, d)
	}
	if err := b.Upsert(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	count := func(q string) map[string]bool {
		res, err := b.Search(context.Background(), withLimit(Parse(q, ParseOptions{Now: now}), MaxLimit))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, h := range res.Hits {
			out[h.ID.String()] = true
		}
		return out
	}
	for _, v := range []string{"0d", "1d", "3d", "2w", "1m", "6m", "1y"} {
		newer, older := count("newer_than:"+v), count("older_than:"+v)
		if len(newer)+len(older) != len(docs) {
			t.Errorf("%s: newer %d + older %d != %d", v, len(newer), len(older), len(docs))
		}
		for id := range newer {
			if older[id] {
				t.Errorf("%s: a message is both newer and older", v)
				break
			}
		}
		if len(newer) == 0 || len(older) == 0 {
			t.Errorf("%s: a side is empty (newer %d, older %d) — the fixture does not exercise the cutoff", v, len(newer), len(older))
		}
	}
}

func withLimit(q Query, n int) Query { q.Limit = n; return q }

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1500", 1500, true},
		{"10K", 10 << 10, true},
		{"10k", 10 << 10, true},
		{"10KB", 10 << 10, true},
		{"2.5M", 5 << 19, true},
		{"1G", 1 << 30, true},
		{"0", 0, true},
		{"", 0, false},
		{"M", 0, false},
		{"big", 0, false},
		{"-5", 0, false},
		{"1e9", 0, false},
		{"5X", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseSize(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseSize(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	for _, in := range []string{"larger:big", "smaller:5X", "larger:-1"} {
		var se *SyntaxError
		if _, err := ParseChecked(in, ParseOptions{}); !errors.As(err, &se) {
			t.Errorf("ParseChecked(%q) = %v, want a *SyntaxError", in, err)
		}
	}
	if q := Parse("smaller:0", ParseOptions{}); !reflect.DeepEqual(q.Root, Node(None{})) {
		t.Errorf("smaller:0 = %#v, want None (nothing is smaller than nothing)", q.Root)
	}
}

// The size cover is exact to the byte: a document matches the cover of [lo, hi)
// if and only if lo <= size < hi. Checked against brute force over sizes and
// ranges at every magnitude, including the block boundaries.
func TestSizeCoverIsExact(t *testing.T) {
	r := rand.New(rand.NewSource(722))
	var sizes []int64
	for _, base := range []int64{0, 1, 15, 16, 17, 255, 256, 4095, 4096, 1 << 20, 10 << 20, 1<<36 - 1, 1 << 36, sizeLimit - 1} {
		sizes = append(sizes, base)
		if base > 0 {
			sizes = append(sizes, base-1, base+1)
		}
	}
	for i := 0; i < 200; i++ {
		sizes = append(sizes, r.Int63n(int64(1)<<uint(r.Intn(sizeBits))+1))
	}
	bounds := append([]int64{}, sizes...)
	for _, lo := range bounds[:60] {
		for _, hi := range bounds[:60] {
			cover := map[string]bool{}
			for _, tok := range sizeCover(lo, hi) {
				cover[tok] = true
			}
			if len(cover) > 2*15*(sizeBits/sizeStep) {
				t.Fatalf("[%d,%d): %d tokens, more than the bound", lo, hi, len(cover))
			}
			for _, s := range sizes {
				in := false
				for _, tok := range sizeTokens(s) {
					if cover[tok] {
						in = true
						break
					}
				}
				want := lo <= s && s < hi
				if s >= sizeLimit {
					want = lo <= sizeLimit-1 && sizeLimit-1 < hi
				}
				if in != want {
					t.Fatalf("size %d in [%d,%d): cover says %v, want %v", s, lo, hi, in, want)
				}
			}
		}
	}
}

func TestLargerSmallerAgainstTheBackend(t *testing.T) {
	sizes := map[string]int64{"tiny": 100, "k": 1024, "k1": 1025, "five": 5 << 20, "huge": 30 << 20}
	b := NewMemoryBackend()
	docs := map[string]Document{}
	i := int64(0)
	for k, s := range sizes {
		i++
		docs[k] = Document{ID: mkID(t, 50+i), Subject: k, Size: s}
	}
	var list []Document
	for _, d := range docs {
		list = append(list, d)
	}
	if err := b.Upsert(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"larger:1K":                {"k1", "five", "huge"},
		"smaller:1K":               {"tiny"},
		"larger:1024":              {"k1", "five", "huge"},
		"smaller:1025":             {"tiny", "k"},
		"larger:5M":                {"huge"},
		"larger:1K smaller:6M":     {"k1", "five"},
		"larger:10M OR smaller:1K": {"tiny", "huge"},
		"-larger:1K":               {"tiny", "k"},
	}
	for q, want := range cases {
		got := runExact(t, b, q)
		if len(got) != len(want) {
			t.Errorf("%q matched %d, want %v", q, len(got), want)
		}
		for _, k := range want {
			if !got[docs[k].ID] {
				t.Errorf("%q missed %s", q, k)
			}
		}
	}
}

func TestListIDValues(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{`"Announcements" <announce.lists.example.org>`, []string{"announce.lists.example.org", "lists.example.org", "example.org"}},
		{"<Dev.Example.COM>", []string{"dev.example.com", "example.com"}},
		{"announce.lists.example.org", []string{"announce.lists.example.org", "lists.example.org", "example.org"}},
		{"<mylist>", []string{"mylist"}},
		{"", nil},
		{"   ", nil},
	}
	for _, tc := range tests {
		if got := listIDValues(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("listIDValues(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// The query side normalizes the same way, and accepts the posting address.
	for in, want := range map[string]string{
		"list:announce.lists.example.org":   "announce.lists.example.org",
		"list:<Announce.Lists.Example.org>": "announce.lists.example.org",
		"list:announce@lists.example.org":   "announce.lists.example.org",
		"list:*.example.org":                "example.org",
	} {
		got := Parse(in, ParseOptions{}).Root
		if !reflect.DeepEqual(got, Node(ExactTerm(FieldList, want))) {
			t.Errorf("Parse(%q) = %#v, want list term %q", in, got, want)
		}
	}
}

// v3Corpus is one message per new index-side property.
func v3Corpus(t *testing.T) (*MemoryBackend, map[string]Document) {
	t.Helper()
	docs := map[string]Document{
		"list":       {ID: mkID(t, 201), Subject: "Weekly", ListID: `"Announce" <announce.lists.example.org>`, Unsubscribe: true},
		"other-list": {ID: mkID(t, 202), Subject: "Daily", ListID: "<dev.other.example>"},
		"unsub-only": {ID: mkID(t, 203), Subject: "Promo", Unsubscribe: true},
		"pdf": {ID: mkID(t, 204), Subject: "Invoice", Attachments: []Attachment{
			{Filename: "Invoice 2026-03.pdf", ContentType: "application/pdf"}}},
		"octet-pdf": {ID: mkID(t, 205), Subject: "Scan", Attachments: []Attachment{
			{Filename: "scan.PDF", ContentType: "application/octet-stream"}}},
		"two-files": {ID: mkID(t, 206), Subject: "Two", Attachments: []Attachment{
			{Filename: "report.docx", ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
			{Filename: "photo.pdf", ContentType: "image/jpeg"}}},
		"sheet": {ID: mkID(t, 207), Subject: "Numbers", Attachments: []Attachment{
			{Filename: "q3.csv", ContentType: "text/csv"}}},
		"report-pdf": {ID: mkID(t, 208), Subject: "Report", Attachments: []Attachment{
			{Filename: "report.pdf", ContentType: "application/pdf"}}},
		"alias":       {ID: mkID(t, 209), Subject: "Patreon", DeliveredTo: "sub@me.example.net"},
		"alias-sub":   {ID: mkID(t, 210), Subject: "Shop", DeliveredTo: "shop@mail.me.example.net"},
		"alias-other": {ID: mkID(t, 211), Subject: "Else", DeliveredTo: "x@notme.example.net"},
	}
	b := NewMemoryBackend()
	var list []Document
	for _, d := range docs {
		list = append(list, d)
	}
	if err := b.Upsert(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	return b, docs
}

func TestV3IndexOperatorsAgainstTheBackend(t *testing.T) {
	b, docs := v3Corpus(t)
	cases := map[string][]string{
		"list:announce.lists.example.org":   {"list"},
		"list:announce@lists.example.org":   {"list"},
		"list:example.org":                  {"list"},
		"list:other.example":                {"other-list"},
		"list:lists.example":                {},
		"has:unsubscribe":                   {"list", "unsub-only"},
		"-has:unsubscribe list:example.org": {},
		"has:pdf":                           {"pdf", "octet-pdf", "report-pdf"},
		"has:image":                         {"two-files"},
		"has:document":                      {"two-files"},
		"has:spreadsheet":                   {"sheet"},
		"has:presentation":                  {},
		"filename:pdf":                      {"pdf", "octet-pdf", "report-pdf", "two-files"},
		"filename:.pdf":                     {"pdf", "octet-pdf", "report-pdf", "two-files"},
		"filename:invoice":                  {"pdf"},
		"filename:report.pdf":               {"report-pdf"}, // NOT two-files (report.docx + photo.pdf)
		"filename:report":                   {"two-files", "report-pdf"},
		"filename:rep*":                     {"two-files", "report-pdf"},
		`filename:"invoice 2026"`:           {"pdf"},
		"alias:sub@me.example.net":          {"alias"},
		"deliveredto:me.example.net":        {"alias", "alias-sub"},
		"alias:*.me.example.net":            {"alias", "alias-sub"},
		"alias:mail.me.example.net":         {"alias-sub"},
		"has:pdf OR has:image":              {"pdf", "octet-pdf", "report-pdf", "two-files"},
	}
	for q, want := range cases {
		got := runExact(t, b, q)
		if len(got) != len(want) {
			t.Errorf("%q matched %d, want %v", q, len(got), want)
		}
		for _, k := range want {
			if !got[docs[k].ID] {
				t.Errorf("%q missed %s", q, k)
			}
		}
	}
	// alias: refuses what from: refuses.
	var se *SyntaxError
	if _, err := ParseChecked("alias:*foo", ParseOptions{}); !errors.As(err, &se) {
		t.Errorf("alias:*foo = %v, want a *SyntaxError", err)
	}
}

func TestParseStoreSideOperators(t *testing.T) {
	tests := []struct {
		in   string
		want Node
	}{
		{"area:Work", StoreTerm{Kind: StoreArea, Value: "Work"}},
		{`area:"Side Projects"`, StoreTerm{Kind: StoreArea, Value: "Side Projects"}},
		{"-area:Work", Not{Node: StoreTerm{Kind: StoreArea, Value: "Work"}}},
		{"area:", None{}},
		{"is:snoozed", StoreTerm{Kind: StoreSnoozed}},
		{"is:muted", StoreTerm{Kind: StoreMuted}},
		{"is:pinned", StoreTerm{Kind: StorePinned}},
		{"is:important", StoreTerm{Kind: StoreAboveLine}},
		{"is:above-line", StoreTerm{Kind: StoreAboveLine}},
		{"has:unsubscribe", ExactTerm(FieldFlag, FlagUnsubscribe)},
		{"has:pdf", ExactTerm(FieldFlag, FamilyPDF)},
		{"has:images", ExactTerm(FieldFlag, FamilyImage)},
		{"in:trash", ExactTerm(FieldFlag, FlagTrashed)},
		{"in:junk", ExactTerm(FieldFlag, FlagJunk)},
		{"in:somewhere", None{}},
	}
	for _, tc := range tests {
		if got := Parse(tc.in, ParseOptions{}).Root; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Parse(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	// in:anywhere is a scope, not a clause.
	q := Parse("invoice in:anywhere", ParseOptions{})
	if !q.Anywhere || !reflect.DeepEqual(q.Root, word("invoice")) {
		t.Errorf("invoice in:anywhere = %#v (anywhere %v), want the word alone with Anywhere set", q.Root, q.Anywhere)
	}
	if q := Parse("-in:anywhere invoice", ParseOptions{}); q.Anywhere {
		t.Error("-in:anywhere must not widen the scope")
	}
}

// RequiredSchemaVersion is what lets an old index refuse a query it would
// answer wrongly (ErrIndexUpgrading) and keep answering the rest.
func TestRequiredSchemaVersion(t *testing.T) {
	tests := map[string]int{
		"invoice":                  1,
		"from:a@b.example":         1,
		"is:unread has:attachment": 1,
		"from:shop.example":        2,
		"list:example.org":         3,
		"has:unsubscribe":          3,
		"has:pdf":                  3,
		"-has:image":               3,
		"filename:x":               3,
		"larger:5M":                3,
		"alias:me@x.example":       3,
		"alias:x.example":          3,
		"lunch OR has:pdf":         3,
		"newer_than:3d":            1,
	}
	for q, want := range tests {
		if got := RequiredSchemaVersion(Expand(Parse(q, ParseOptions{}))); got != want {
			t.Errorf("RequiredSchemaVersion(%q) = %d, want %d", q, got, want)
		}
	}
	if schemaVersion != 3 {
		t.Errorf("schemaVersion = %d; the v3 fields need the bump to 3 or no index is ever rebuilt with them", schemaVersion)
	}
}

func TestSplitStore(t *testing.T) {
	area := StoreTerm{Kind: StoreArea, Value: "Work"}
	pinned := StoreTerm{Kind: StorePinned}
	inv, lunch := word("invoice"), word("lunch")
	tests := []struct {
		in        string
		wantIndex Node
		wantStore Node
	}{
		{"invoice lunch", And{Nodes: []Node{inv, lunch}}, nil},
		{"area:Work", All{}, area},
		{"invoice area:Work", inv, area},
		{"invoice (lunch area:Work)", And{Nodes: []Node{inv, lunch}}, area}, // nested Ands flatten
		{"invoice -area:Work", inv, Not{Node: area}},
		{"area:Work OR invoice", All{}, Or{Nodes: []Node{area, IndexSet{Node: inv}}}},
		{"(area:Work OR invoice OR lunch) is:pinned", All{},
			And{Nodes: []Node{Or{Nodes: []Node{area, IndexSet{Node: Or{Nodes: []Node{inv, lunch}}}}}, pinned}}},
		{"-(area:Work invoice)", All{}, Not{Node: And{Nodes: []Node{area, IndexSet{Node: inv}}}}},
	}
	for _, tc := range tests {
		got := SplitStore(Parse(tc.in, ParseOptions{}).Root)
		if !reflect.DeepEqual(got.Index, tc.wantIndex) || !reflect.DeepEqual(got.Store, tc.wantStore) {
			t.Errorf("SplitStore(%q):\n got  index %#v\n      store %#v\n want index %#v\n      store %#v",
				tc.in, got.Index, got.Store, tc.wantIndex, tc.wantStore)
		}
	}
	// WithoutStore keeps only what the index can highlight.
	for in, want := range map[string]Node{
		"invoice area:Work":     inv,
		"area:Work OR invoice":  inv,
		"-area:Work":            All{},
		"-(area:Work) lunch":    lunch,
		"(area:Work is:pinned)": All{},
	} {
		if got := WithoutStore(Parse(in, ParseOptions{}).Root); !reflect.DeepEqual(got, want) {
			t.Errorf("WithoutStore(%q) = %#v, want %#v", in, got, want)
		}
	}
}

// A backend must never be handed a store-side predicate: it would compile it as
// nothing and widen the query. Service.Run refuses instead.
func TestServiceRefusesStorePredicates(t *testing.T) {
	ix, err := NewIndexer(t.TempDir(), MemoryFactory, newFakeStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	svc := NewService(ix, nil)
	user := mkID(t, 9)
	for _, q := range []string{"area:Work", "invoice is:snoozed", "invoice OR is:pinned", "-is:important"} {
		if _, err := svc.Search(context.Background(), user, q, SearchOptions{}); !errors.Is(err, ErrStorePredicate) {
			t.Errorf("Search(%q) = %v, want ErrStorePredicate", q, err)
		}
	}
	// Snippets drop them instead: there is nothing in them to highlight.
	if _, err := svc.Snippets(context.Background(), user, "invoice area:Work", []ulid.ULID{mkID(t, 1)}, SearchOptions{}); err != nil {
		t.Errorf("Snippets with a store predicate: %v", err)
	}
}

// A date bound far outside the years mail is dated in is clamped, so the cover
// stays small: `after:0001-01-01` and `newer_than:9999y` would otherwise emit a
// token per year back to year one.
func TestDateCoverClampsFarBounds(t *testing.T) {
	far := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	if n := len(dateCover(far, time.Time{})); n > maxDateYear-minDateYear {
		t.Errorf("after:0001-01-01 covers with %d tokens, want at most %d", n, maxDateYear-minDateYear)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if or, ok := Expand(Parse("newer_than:9999y", ParseOptions{Now: now})).(Or); !ok || len(or.Nodes) > maxDateYear-minDateYear {
		t.Errorf("newer_than:9999y expands with too many tokens (%v)", ok)
	}
	// older_than past the floor matches nothing — nothing is that old.
	if got := Expand(Parse("older_than:9999y", ParseOptions{Now: now})); !reflect.DeepEqual(got, Node(None{})) {
		t.Errorf("older_than:9999y = %#v, want None", got)
	}
}

// treeDepth is the nesting depth of a tree (a leaf is 1).
func treeDepth(n Node) int {
	var kids []Node
	switch v := n.(type) {
	case And:
		kids = v.Nodes
	case Or:
		kids = v.Nodes
	case Not:
		kids = []Node{v.Node}
	}
	d := 0
	for _, k := range kids {
		if kd := treeDepth(k); kd > d {
			d = kd
		}
	}
	return d + 1
}

// Ten thousand nested brackets must not drive the parser's recursion (a stack
// overflow kills the whole server): past MaxGroupDepth a '(' is just a
// character, so the tree stays shallow and the words still search.
func TestDeepBracketsAreBounded(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("(", 10000) + "invoice" + strings.Repeat(")", 10000),
		strings.Repeat("(a OR ", 10000) + "b",
		strings.Repeat("-(", 10000) + "x",
		strings.Repeat("( ", 10000) + "invoice",
	} {
		q, err := ParseChecked(in, ParseOptions{})
		if err != nil {
			t.Errorf("deep brackets: %v", err)
		}
		// Every group level can add at most an Or, an And and a Not.
		if d := treeDepth(q.Root); d > 3*MaxGroupDepth+3 {
			t.Errorf("%.20q…: tree depth %d, want at most %d", in, d, 3*MaxGroupDepth+3)
		}
		if _, ok := q.Root.(None); ok {
			t.Errorf("%.20q…: deep brackets matched nothing", in)
		}
	}
	// A query nested within the limit keeps its structure.
	in := strings.Repeat("(", MaxGroupDepth) + "a OR b" + strings.Repeat(")", MaxGroupDepth)
	if got := Parse(in, ParseOptions{}).Root; !reflect.DeepEqual(got, Node(Or{Nodes: []Node{word("a"), word("b")}})) {
		t.Errorf("%d nested groups = %#v, want a OR b", MaxGroupDepth, got)
	}
}

func TestServiceRefusesOverlongQueries(t *testing.T) {
	ix, err := NewIndexer(t.TempDir(), MemoryFactory, newFakeStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	svc := NewService(ix, nil)
	long := strings.Repeat("a ", MaxQueryBytes/2+1)
	if _, err := svc.Search(context.Background(), mkID(t, 9), long, SearchOptions{}); !errors.Is(err, ErrQueryTooLong) {
		t.Errorf("Search of %d bytes = %v, want ErrQueryTooLong", len(long), err)
	}
	if _, err := svc.Search(context.Background(), mkID(t, 9), long[:MaxQueryBytes], SearchOptions{}); err != nil {
		t.Errorf("a query of exactly the limit was refused: %v", err)
	}
}
