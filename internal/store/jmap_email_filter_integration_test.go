// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// fxMsg is one fixture message; the reference orderings below are computed from
// these rows in Go, independently of the SQL under test.
var fxUID = func() *atomic.Int64 { v := &atomic.Int64{}; v.Store(1000); return v }()

type fxMsg struct {
	name     string
	id       ulid.ULID
	thread   string // "" = no thread
	acct2    bool
	subject  string
	fromName string
	fromAddr string
	size     int64
	attach   bool
	kw       []string
	age      time.Duration
}

type filterFixture struct {
	h            *harness
	a, b         fixtureUser
	acct1, acct2 ulid.ULID
	mbox1, mbox2 ulid.ULID
	msgs         []*fxMsg
	byName       map[string]*fxMsg
	bMsgs        []ulid.ULID
	seedID       ulid.ULID
}

// newFilterFixture seeds user A with two accounts and a thread that spans both,
// and user B with messages that match the same text. Two messages (t1, t2) tie on
// every sort key AND on received_at, so only the id can order them.
func newFilterFixture(t *testing.T) *filterFixture {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	f := &filterFixture{h: h, a: a, b: b, byName: map[string]*fxMsg{}, seedID: mustULID(t, a.MessageID)}
	f.acct1, f.mbox1 = mustULID(t, a.AccountID), mustULID(t, a.MailboxID)
	f.acct2, f.mbox2 = NewULID(), NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'second', 'mail.example.com', 'smtp.example.com', $3, $4)`,
		f.acct2.String(), a.ID, []byte{1}, NewULID().String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	      VALUES ($1, $2, $3, 'INBOX', 'Inbox')`, f.mbox2.String(), a.ID, f.acct2.String())
	for _, th := range []string{"T", "U", "V"} {
		exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
			threadULID(a.ID, th), a.ID, th)
	}
	day := 24 * time.Hour
	f.msgs = []*fxMsg{
		{name: "x1", thread: "T", subject: "Re: Lunch plans", fromName: "Bob Builder", fromAddr: "bob@example.net", size: 500, age: 1 * day},
		{name: "x2", thread: "T", acct2: true, subject: "Secret", fromAddr: "carol@example.org", size: 900, attach: true, kw: []string{"kwx"}, age: 2 * day},
		{name: "x3", subject: "100%_sure", fromAddr: "alice@example.com", size: 100, age: 3 * day},
		{name: "x4", thread: "U", subject: "Zeta", fromName: "Alice A", fromAddr: "alice@example.com", size: 700, attach: true, kw: []string{"kwx"}, age: 4 * day},
		{name: "y1", thread: "V", subject: "FWD: Tie", fromAddr: "dup@example.com", size: 300, kw: []string{"kwx"}, age: 5 * day},
		{name: "y2", thread: "V", acct2: true, subject: "Tie", fromAddr: "dup@example.com", size: 300, age: 5 * day},
	}
	for _, m := range f.msgs {
		m.id = NewULID()
		f.byName[m.name] = m
		mb, ac := f.mbox1, f.acct1
		if m.acct2 {
			mb, ac = f.mbox2, f.acct2
		}
		f.insert(t, a.ID, m, mb, ac)
	}
	// y1/y2 must tie exactly on received_at.
	exec(`UPDATE app.messages SET received_at = (SELECT received_at FROM app.messages WHERE id = $1) WHERE id = $2`,
		f.byName["y1"].id.String(), f.byName["y2"].id.String())
	// User B holds mail that matches A's text conditions.
	for i, name := range []string{"bx1", "bx2"} {
		m := &fxMsg{name: name, id: NewULID(), thread: "", subject: "Re: Lunch plans", fromName: "Bob Builder",
			fromAddr: "bob@example.net", size: 500 + int64(i), attach: true, kw: []string{"kwx"}, age: day}
		f.insert(t, b.ID, m, mustULID(t, b.MailboxID), mustULID(t, b.AccountID))
		f.bMsgs = append(f.bMsgs, m.id)
	}
	exec(`ANALYZE app.messages`)
	return f
}

func threadULID(user, name string) string {
	h := sha256.Sum256([]byte("fx-thread-" + user + name))
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	out := make([]byte, 26)
	out[0] = '0'
	for i := 1; i < 26; i++ {
		out[i] = alpha[int(h[i])%32]
	}
	return string(out)
}

func (f *filterFixture) insert(t *testing.T, userID string, m *fxMsg, mailbox, account ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	hash := sha256.Sum256([]byte("fx-" + m.id.String()))
	var thread any
	if m.thread != "" && userID == f.a.ID {
		thread = threadULID(userID, m.thread)
	}
	kw := m.kw
	if kw == nil {
		kw = []string{}
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1, $2, 1, 1, $3)`,
			[]any{userID, hash[:], NewULID().String()}},
		{`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, from_name, from_address,
		        received_at, size_bytes, has_attachments, keywords)
		  VALUES ($1, $2, $3, $4, $5, $6, $7, now() - $8::interval, $9, $10, $11)`,
			[]any{m.id.String(), userID, hash[:], thread, m.subject, m.fromName, m.fromAddr,
				m.age.String(), m.size, m.attach, kw}},
		{`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		  VALUES ($1, $2, $3, $4, $5, $6, 1)`,
			[]any{NewULID().String(), userID, m.id.String(), account.String(), mailbox.String(), fxUID.Add(1)}},
	} {
		if _, err := f.h.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed %s: %v", m.name, err)
		}
	}
}

// query runs q as user A and returns ids and total.
func (f *filterFixture) query(t *testing.T, q EmailQuery) ([]ulid.ULID, int) {
	t.Helper()
	var ids []ulid.ULID
	var total int
	if err := f.h.appStore().UserTx(context.Background(), mustULID(t, f.a.ID), func(tx *UserTx) error {
		var err error
		ids, total, err = tx.QueryEmails(context.Background(), q)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ids, total
}

func (f *filterFixture) names(ids []ulid.ULID) []string {
	rev := map[ulid.ULID]string{f.seedID: "seed"}
	for n, m := range f.byName {
		rev[m.id] = n
	}
	var out []string
	for _, id := range ids {
		if n, ok := rev[id]; ok {
			out = append(out, n)
		} else {
			out = append(out, "?"+id.String())
		}
	}
	return out
}

func leaf(q EmailQuery) EmailFilterNode { return EmailFilterNode{Leaf: q} }
func op(o string, c ...EmailFilterNode) EmailFilterNode {
	return EmailFilterNode{Operator: o, Conditions: c}
}

func sortedNames(f *filterFixture, ids []ulid.ULID) string {
	n := f.names(ids)
	sort.Strings(n)
	return strings.Join(n, ",")
}

// TestFilterTreeKeyScopeLeaksNothing is the property that matters most: an API
// key's account restriction sits OUTSIDE the tree, so NOT and OR can never widen
// what the key sees. Each query is asked as the restricted key and compared with
// the same query unrestricted, which proves the test can see a leak (the
// unrestricted answer contains the forbidden mail).
func TestFilterTreeKeyScopeLeaksNothing(t *testing.T) {
	f := newFilterFixture(t)
	restrict := []ulid.ULID{f.acct1}
	inB2 := leaf(EmailQuery{InMailbox: &f.mbox2})
	inB1 := leaf(EmailQuery{InMailbox: &f.mbox1})
	trees := map[string]EmailFilterNode{
		"NOT forbidden mailbox":         op(FilterNOT, inB2),
		"OR forbidden mailbox":          op(FilterOR, inB2, inB1),
		"OR forbidden only":             op(FilterOR, inB2),
		"NOT NOT forbidden":             op(FilterNOT, op(FilterNOT, inB2)),
		"AND OR NOT nested":             op(FilterAND, op(FilterOR, inB2, op(FilterNOT, inB1)), op(FilterNOT, op(FilterAND, inB2, inB2))),
		"NOT keyword in forbidden acct": op(FilterNOT, leaf(EmailQuery{InAccount: &f.acct2})),
		"OR inAccount forbidden":        op(FilterOR, leaf(EmailQuery{InAccount: &f.acct2}), leaf(EmailQuery{HasKeyword: "kwx"})),
		"empty NOT (true)":              op(FilterNOT),
	}
	forbidden := []string{"x2", "y2"}
	anyDiffers := false
	for name, tree := range trees {
		tree := tree
		keyIDs, keyTotal := f.query(t, EmailQuery{RestrictAccounts: restrict, Tree: &tree})
		allIDs, allTotal := f.query(t, EmailQuery{Tree: &tree})
		for _, n := range f.names(keyIDs) {
			if slices.Contains(forbidden, n) || strings.HasPrefix(n, "bx") || strings.HasPrefix(n, "?") {
				t.Errorf("%s: the restricted key was shown %s (ids %v)", name, n, f.names(keyIDs))
			}
		}
		if keyTotal != len(keyIDs) {
			t.Errorf("%s: total %d differs from the ids it counted (%d): the total leaks", name, keyTotal, len(keyIDs))
		}
		for _, id := range allIDs {
			for _, b := range f.bMsgs {
				if id == b {
					t.Errorf("%s: user A's query returned user B's message", name)
				}
			}
		}
		if allTotal != keyTotal {
			anyDiffers = true
		}
	}
	if !anyDiffers {
		t.Fatal("no tree answered differently with and without the restriction: the fixture cannot show a leak")
	}
	// The named cases of the brief, exactly.
	got, _ := f.query(t, EmailQuery{RestrictAccounts: restrict, Tree: ptr(op(FilterNOT, inB2))})
	base, baseTotal := f.query(t, EmailQuery{RestrictAccounts: restrict})
	if sortedNames(f, got) != sortedNames(f, base) {
		t.Errorf("NOT forbidden-mailbox: %s, want everything the key sees: %s", sortedNames(f, got), sortedNames(f, base))
	}
	_, orTotal := f.query(t, EmailQuery{RestrictAccounts: restrict, Tree: ptr(op(FilterOR, inB2, inB1))})
	if orTotal != baseTotal {
		t.Errorf("OR with a forbidden mailbox changed the total: %d vs %d", orTotal, baseTotal)
	}
	_, onlyForbidden := f.query(t, EmailQuery{RestrictAccounts: restrict, Tree: ptr(op(FilterOR, inB2))})
	if onlyForbidden != 0 {
		t.Errorf("OR with only a forbidden mailbox: total %d, want 0", onlyForbidden)
	}
}

func ptr[T any](v T) *T { return &v }

// TestFilterThreadKeywordsAreScopedToWhatTheKeySees: a thread can span accounts,
// and a key must not learn from a result (or its total) that a message in an
// account it cannot see carries a keyword.
func TestFilterThreadKeywordsAreScopedToWhatTheKeySees(t *testing.T) {
	f := newFilterFixture(t)
	restrict := []ulid.ULID{f.acct1}
	// Thread T: x1 (visible, no keyword), x2 (hidden, kwx). Thread V: y1 (visible,
	// kwx), y2 (hidden, no keyword).
	cases := []struct {
		name                string
		q                   EmailQuery
		wantKey, wantAll    string
		wantKeyT, wantAllTo int
	}{
		{"some", EmailQuery{SomeInThreadHaveKeyword: "kwx"}, "x4,y1", "x1,x2,x4,y1,y2", 2, 5},
		{"none", EmailQuery{NoneInThreadHaveKeyword: "kwx"}, "seed,x1,x3", "seed,x3", 3, 2},
		{"all", EmailQuery{AllInThreadHaveKeyword: "kwx"}, "x4,y1", "x4", 2, 1},
		{"NOT some", EmailQuery{Tree: ptr(op(FilterNOT, leaf(EmailQuery{SomeInThreadHaveKeyword: "kwx"})))}, "seed,x1,x3", "seed,x3", 3, 2},
		{"OR some, forbidden mailbox", EmailQuery{Tree: ptr(op(FilterOR, leaf(EmailQuery{SomeInThreadHaveKeyword: "kwx"}),
			leaf(EmailQuery{InMailbox: &f.mbox2})))}, "x4,y1", "x1,x2,x4,y1,y2", 2, 5},
		{"NOT all", EmailQuery{Tree: ptr(op(FilterNOT, leaf(EmailQuery{AllInThreadHaveKeyword: "kwx"})))}, "seed,x1,x3", "seed,x1,x2,x3,y1,y2", 3, 6},
	}
	for _, c := range cases {
		qk := c.q
		qk.RestrictAccounts = restrict
		gotKey, totKey := f.query(t, qk)
		gotAll, totAll := f.query(t, c.q)
		if g := sortedNames(f, gotKey); g != c.wantKey || totKey != c.wantKeyT {
			t.Errorf("%s as a restricted key: %s (total %d), want %s (%d)", c.name, g, totKey, c.wantKey, c.wantKeyT)
		}
		if g := sortedNames(f, gotAll); g != c.wantAll || totAll != c.wantAllTo {
			t.Errorf("%s unrestricted: %s (total %d), want %s (%d)", c.name, g, totAll, c.wantAll, c.wantAllTo)
		}
	}
}

func TestFilterStoredColumnConditions(t *testing.T) {
	f := newFilterFixture(t)
	yes := true
	min700, max500 := int64(700), int64(500)
	cases := []struct {
		name string
		q    EmailQuery
		want string
	}{
		{"from address", EmailQuery{FromContains: "bob@example.net"}, "x1"},
		{"from name, any case", EmailQuery{FromContains: "BOB BUILDER"}, "x1"},
		{"from domain substring", EmailQuery{FromContains: "example.net"}, "x1"},
		{"from matches name and address", EmailQuery{FromContains: "alice"}, "x3,x4"},
		{"from ignores the other headers", EmailQuery{FromContains: "friend@"}, "seed"},
		{"from, nothing", EmailQuery{FromContains: "nobody"}, ""},
		{"subject case-insensitive", EmailQuery{SubjectContains: "LUNCH"}, "x1"},
		{"subject % is literal", EmailQuery{SubjectContains: "%"}, "x3"},
		{"subject _ is literal", EmailQuery{SubjectContains: "%_s"}, "x3"},
		{"subject _ is not a wildcard", EmailQuery{SubjectContains: "0x"}, ""},
		{"hasAttachment", EmailQuery{HasAttachment: &yes}, "x2,x4"},
		{"minSize inclusive", EmailQuery{MinSize: &min700}, "x2,x4"},
		{"maxSize exclusive", EmailQuery{MaxSize: &max500}, "seed,x3,y1,y2"},
		{"size window", EmailQuery{MinSize: ptr(int64(300)), MaxSize: &min700}, "x1,y1,y2"},
	}
	for _, c := range cases {
		ids, total := f.query(t, EmailQuery{FromContains: c.q.FromContains, SubjectContains: c.q.SubjectContains,
			HasAttachment: c.q.HasAttachment, MinSize: c.q.MinSize, MaxSize: c.q.MaxSize})
		got := sortedNames(f, ids)
		want := c.want
		if got != want || total != len(ids) {
			t.Errorf("%s: %q (total %d), want %q", c.name, got, total, want)
		}
	}
	// Inside a tree, and negated: a NULL-safe complement.
	notAlice, _ := f.query(t, EmailQuery{Tree: ptr(op(FilterNOT, leaf(EmailQuery{FromContains: "alice"})))})
	if g := sortedNames(f, notAlice); g != "seed,x1,x2,y1,y2" {
		t.Errorf("NOT from alice: %q", g)
	}
	// And: AND(from alice, NOT hasAttachment) = x3.
	g, _ := f.query(t, EmailQuery{Tree: ptr(op(FilterAND, leaf(EmailQuery{FromContains: "alice"}),
		op(FilterNOT, leaf(EmailQuery{HasAttachment: &yes}))))})
	if sortedNames(f, g) != "x3" {
		t.Errorf("AND/NOT: %q", sortedNames(f, g))
	}
}

func TestFilterTreeBounds(t *testing.T) {
	f := newFilterFixture(t)
	deep := leaf(EmailQuery{})
	for i := 0; i < MaxFilterDepth; i++ { // the leaf then sits at depth MaxFilterDepth+1
		deep = op(FilterAND, deep)
	}
	wide := EmailFilterNode{Operator: FilterOR}
	for i := 0; i < MaxFilterNodes; i++ {
		wide.Conditions = append(wide.Conditions, leaf(EmailQuery{}))
	}
	for name, tree := range map[string]EmailFilterNode{"deep": deep, "wide": wide} {
		tree := tree
		err := f.h.appStore().UserTx(context.Background(), mustULID(t, f.a.ID), func(tx *UserTx) error {
			_, _, err := tx.QueryEmails(context.Background(), EmailQuery{Tree: &tree})
			return err
		})
		if err == nil || !strings.Contains(err.Error(), ErrFilterTooComplex.Error()) {
			t.Errorf("%s tree beyond the bound: err = %v, want ErrFilterTooComplex", name, err)
		}
	}
	ok := leaf(EmailQuery{})
	for i := 0; i < MaxFilterDepth-1; i++ {
		ok = op(FilterAND, ok)
	}
	if _, total := f.query(t, EmailQuery{Tree: &ok}); total == 0 {
		t.Error("a tree exactly at the depth bound must still run")
	}
}

type refKey func(m *fxMsg) string

// refOrder is the expected order computed in Go: by the key (with its own
// direction), then received_at and id in the direction of the default date key.
func (f *filterFixture) refOrder(t *testing.T, key refKey, asc bool, dateAsc bool) []string {
	rows := append([]*fxMsg{}, f.msgs...)
	seed := &fxMsg{name: "seed", id: f.seedID, subject: "seed message", fromAddr: "friend@example.com", size: 100, age: 0}
	rows = append(rows, seed)
	recv := func(m *fxMsg) time.Duration { return m.age }
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		ka, kb := key(a), key(b)
		if ka != kb {
			if asc {
				return ka < kb
			}
			return ka > kb
		}
		// newest first = smaller age first, when the date key is descending
		if recv(a) != recv(b) {
			if dateAsc {
				return recv(a) > recv(b)
			}
			return recv(a) < recv(b)
		}
		if dateAsc {
			return a.id.String() < b.id.String()
		}
		return a.id.String() > b.id.String()
	})
	var out []string
	for _, m := range rows {
		out = append(out, m.name)
	}
	return out
}

func itoa(n int64) string {
	s := ""
	for n > 0 || s == "" {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return strings.Repeat("0", 12-len(s)) + s
}

func baseSubject(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"re:", "fwd:", "fw:"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	return s
}

func fromKey(m *fxMsg) string {
	if m.fromName != "" {
		return strings.ToLower(m.fromName)
	}
	return strings.ToLower(m.fromAddr)
}

func TestSortKeysOrderAndTieBreak(t *testing.T) {
	f := newFilterFixture(t)
	type sc struct {
		name string
		keys []EmailSortKey
		sa   bool // SortAscending: the direction of the date key and the id
		ref  func() []string
	}
	scs := []sc{
		{"size asc", []EmailSortKey{{Kind: SortKeySize, Ascending: true}}, false,
			func() []string { return f.refOrder(t, func(m *fxMsg) string { return itoa(m.size) }, true, false) }},
		{"size desc", []EmailSortKey{{Kind: SortKeySize}}, false,
			func() []string { return f.refOrder(t, func(m *fxMsg) string { return itoa(m.size) }, false, false) }},
		{"size asc, ties ascending", []EmailSortKey{{Kind: SortKeySize, Ascending: true}}, true,
			func() []string { return f.refOrder(t, func(m *fxMsg) string { return itoa(m.size) }, true, true) }},
		{"from asc", []EmailSortKey{{Kind: SortKeyFrom, Ascending: true}}, false,
			func() []string { return f.refOrder(t, fromKey, true, false) }},
		{"from desc", []EmailSortKey{{Kind: SortKeyFrom}}, false,
			func() []string { return f.refOrder(t, fromKey, false, false) }},
		{"subject asc (base subject)", []EmailSortKey{{Kind: SortKeySubject, Ascending: true}}, false,
			func() []string {
				return f.refOrder(t, func(m *fxMsg) string { return baseSubject(m.subject) }, true, false)
			}},
		// false before true ascending (RFC 8621's example: flagged first with
		// isAscending false), so mail WITHOUT the keyword leads here...
		{"hasKeyword asc", []EmailSortKey{{Kind: SortKeyHasKeyword, Keyword: "kwx", Ascending: true}}, false,
			func() []string {
				return f.refOrder(t, func(m *fxMsg) string {
					if slices.Contains(m.kw, "kwx") {
						return "1"
					}
					return "0"
				}, true, false)
			}},
		// ...and mail WITH it leads descending, as in the RFC's example.
		{"hasKeyword desc (flagged first)", []EmailSortKey{{Kind: SortKeyHasKeyword, Keyword: "kwx"}}, false,
			func() []string {
				return f.refOrder(t, func(m *fxMsg) string {
					if slices.Contains(m.kw, "kwx") {
						return "1"
					}
					return "0"
				}, false, false)
			}},
	}
	for _, c := range scs {
		ids, total := f.query(t, EmailQuery{SortKeys: c.keys, SortAscending: c.sa})
		got := f.names(ids)
		want := c.ref()
		// the reference treats y1/y2 as ordered by id only
		if strings.Join(got, ",") != strings.Join(want, ",") || total != len(want) {
			t.Errorf("%s:\n got  %v (total %d)\n want %v", c.name, got, total, want)
		}
		// With collapseThreads: each thread's first message in this order.
		cids, ctotal := f.query(t, EmailQuery{SortKeys: c.keys, SortAscending: c.sa, CollapseThreads: true})
		seen := map[string]bool{}
		var wantC []string
		for _, n := range want {
			th := "msg:" + n
			if m := f.byName[n]; m != nil && m.thread != "" {
				th = m.thread
			}
			if n == "seed" {
				th = "seed-thread"
			}
			if !seen[th] {
				seen[th] = true
				wantC = append(wantC, n)
			}
		}
		if strings.Join(f.names(cids), ",") != strings.Join(wantC, ",") || ctotal != len(wantC) {
			t.Errorf("%s collapsed:\n got  %v (total %d)\n want %v", c.name, f.names(cids), ctotal, wantC)
		}
	}
}

func TestAnchorIndex(t *testing.T) {
	f := newFilterFixture(t)
	ctx := context.Background()
	keys := []EmailSortKey{{Kind: SortKeySize, Ascending: true}}
	q := EmailQuery{SortKeys: keys}
	order, _ := f.query(t, q)
	idx := func(q EmailQuery, id ulid.ULID, restrictTo ...ulid.ULID) (int, bool) {
		var i int
		var ok bool
		if err := f.h.appStore().UserTx(ctx, mustULID(t, f.a.ID), func(tx *UserTx) error {
			var err error
			i, ok, err = tx.EmailAnchorIndex(ctx, q, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return i, ok
	}
	for want, id := range order {
		if got, ok := idx(q, id); !ok || got != want {
			t.Errorf("anchor %d: index %d found %v", want, got, ok)
		}
	}
	if _, ok := idx(q, NewULID()); ok {
		t.Error("an id that is in no message was found")
	}
	if _, ok := idx(q, f.bMsgs[0]); ok {
		t.Error("another user's message was found as an anchor")
	}
	// A key sees only account 1: a message of account 2 is not an anchor, and
	// indexes count only the visible ones.
	qk := EmailQuery{SortKeys: keys, RestrictAccounts: []ulid.ULID{f.acct1}}
	if _, ok := idx(qk, f.byName["x2"].id); ok {
		t.Error("a hidden message was found as an anchor for a restricted key")
	}
	visible, _ := f.query(t, qk)
	for want, id := range visible {
		if got, ok := idx(qk, id); !ok || got != want {
			t.Errorf("restricted anchor %d: %d %v", want, got, ok)
		}
	}
	// Collapsed: a message that is not its thread's first is not in the list.
	qc := EmailQuery{SortKeys: keys, CollapseThreads: true}
	collapsed, _ := f.query(t, qc)
	for want, id := range collapsed {
		if got, ok := idx(qc, id); !ok || got != want {
			t.Errorf("collapsed anchor %d: %d %v", want, got, ok)
		}
	}
	for _, id := range order {
		if !slices.Contains(collapsed, id) {
			if _, ok := idx(qc, id); ok {
				t.Errorf("%s was collapsed away but was found as an anchor", f.names([]ulid.ULID{id})[0])
			}
		}
	}
}
