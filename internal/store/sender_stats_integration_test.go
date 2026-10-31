// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE CLEAN-UP RANKING (#53): exact answers on a hand-built mailbox, the RLS
// negative, and the measured cost on a seeded 25k-message mailbox.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

type statMsg struct {
	from, name string
	daysOld    int
	seen       bool
	listID     string
	unsub      bool
	trashed    bool
	draft      bool
	junk       bool
}

// seedStatMsgs bulk-inserts messages (and their blobs) for user through COPY,
// returning their ids in order.
func seedStatMsgs(t *testing.T, h *harness, user ulid.ULID, msgs []statMsg) []ulid.ULID {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ids := make([]ulid.ULID, len(msgs))
	blobs := make([][]any, len(msgs))
	rows := make([][]any, len(msgs))
	for i, m := range msgs {
		id := NewULID()
		ids[i] = id
		sum := sha256.Sum256([]byte(id.String()))
		blobs[i] = []any{user.String(), sum[:], int64(10), int64(20), NewULID().String()}
		var list, unsub, trashed, purge any
		if m.listID != "" {
			list = m.listID
		}
		if m.unsub {
			unsub = "<https://unsubscribe.example/x>"
		}
		if m.trashed {
			trashed, purge = now, now.Add(30*24*time.Hour)
		}
		rows[i] = []any{id.String(), user.String(), sum[:], "s", m.name, m.from,
			now.Add(-time.Duration(m.daysOld)*24*time.Hour - time.Duration(i)*time.Second), int64(10),
			m.seen, m.draft, m.junk, list, unsub, trashed, purge}
	}
	if _, err := h.super.CopyFrom(ctx, pgx.Identifier{"app", "blobs"},
		[]string{"user_id", "content_hash", "size_plain", "size_stored", "dek_id"}, pgx.CopyFromRows(blobs)); err != nil {
		t.Fatalf("copy blobs: %v", err)
	}
	if _, err := h.super.CopyFrom(ctx, pgx.Identifier{"app", "messages"},
		[]string{"id", "user_id", "content_hash", "subject", "from_name", "from_address", "received_at", "size_bytes",
			"is_seen", "is_draft", "is_junk", "list_id", "list_unsubscribe", "trashed_at", "purge_after"},
		pgx.CopyFromRows(rows)); err != nil {
		t.Fatalf("copy messages: %v", err)
	}
	return ids
}

func seedStatUser(t *testing.T, h *harness) ulid.ULID {
	t.Helper()
	u := NewULID()
	if _, err := h.super.Exec(context.Background(), `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		u.String(), "sub-"+u.String(), u.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	return u
}

func senderStats(t *testing.T, st *Store, user ulid.ULID, q SenderStatsQuery) ([]SenderStat, int) {
	t.Helper()
	var (
		out   []SenderStat
		total int
	)
	if err := st.UserTx(context.Background(), user, func(tx *UserTx) error {
		var e error
		out, total, e = tx.SenderStats(context.Background(), q)
		return e
	}); err != nil {
		t.Fatalf("SenderStats(%+v): %v", q, err)
	}
	return out, total
}

func byKey(stats []SenderStat) map[string]SenderStat {
	m := map[string]SenderStat{}
	for _, s := range stats {
		m[s.Key] = s
	}
	return m
}

func TestSenderStatsExactAnswers(t *testing.T) {
	h := newHarness(t)
	st := h.appStore()
	ctx := context.Background()
	user := seedStatUser(t, h)
	other := seedStatUser(t, h)
	acct := NewULID()
	if _, err := h.super.Exec(ctx, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1, $2, 'p', 'imap.example.com', 'smtp.example.com', '\x00', $3)`, acct.String(), user.String(), NewULID().String()); err != nil {
		t.Fatal(err)
	}
	// "Me": a fixed identity and a wildcard domain. Neither is a sender.
	for _, q := range []string{
		`INSERT INTO app.identities (id, user_id, account_id, kind, address) VALUES ('` + NewULID().String() + `', $1, $2, 'fixed', 'me@mine.example')`,
		`INSERT INTO app.identities (id, user_id, account_id, kind, domain) VALUES ('` + NewULID().String() + `', $1, $2, 'domain_wildcard', 'wild.example')`,
	} {
		if _, err := h.super.Exec(ctx, q, user.String(), acct.String()); err != nil {
			t.Fatal(err)
		}
	}
	const list = `"News" <news.list.example>`
	ids := seedStatMsgs(t, h, user, []statMsg{
		// news@: 5 in the window, 4 unread, 1 read (archived unread from Mailroom, below)
		{from: "news@list.example", name: "News", daysOld: 1, listID: list, unsub: true},
		{from: "news@list.example", name: "News", daysOld: 2, listID: list, unsub: true},
		{from: "news@list.example", name: "News", daysOld: 3, listID: list},
		{from: "news@list.example", name: "News", daysOld: 40, listID: list},
		{from: "news@list.example", name: "News", daysOld: 41, listID: list, seen: true},
		// a rotated address on the same list
		{from: "news2@list.example", name: "News", daysOld: 5, listID: `<NEWS@list.example>`},
		// boss@: 3 read in the window, 1 read 400 days ago, plus excluded mail
		{from: "boss@work.example", name: "Boss", daysOld: 1, seen: true},
		{from: "boss@work.example", name: "Boss", daysOld: 2, seen: true},
		{from: "boss@work.example", name: "Boss", daysOld: 3, seen: true},
		{from: "boss@work.example", name: "Boss", daysOld: 400, seen: true},
		{from: "boss@work.example", daysOld: 1, trashed: true},
		{from: "boss@work.example", daysOld: 1, draft: true},
		{from: "boss@work.example", daysOld: 1, junk: true},
		// one domain, two addresses
		{from: "promo1@shop.example", daysOld: 10},
		{from: "promo2@shop.example", daysOld: 11, seen: true},
		// me, never a sender
		{from: "me@mine.example", daysOld: 1},
		{from: "anything@wild.example", daysOld: 1},
	})
	seedStatMsgs(t, h, other, []statMsg{
		{from: "news@list.example", daysOld: 1}, {from: "news@list.example", daysOld: 1},
		{from: "secret@other.example", daysOld: 1},
	})
	if _, err := h.super.Exec(ctx, `INSERT INTO app.feedback_events (id, user_id, message_id, action) VALUES ($1, $2, $3, 'archive_unread')`,
		NewULID().String(), user.String(), ids[4].String()); err != nil {
		t.Fatal(err)
	}
	// boss is a blocked contact; promo1 has a contact too.
	bossContact := seedContact(t, h, user, "boss@work.example", false, true)

	all, total := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Sort: SenderSortVolume})
	if total != 5 {
		t.Fatalf("address groups = %d, want 5 (news, news2, boss, promo1, promo2): %+v", total, all)
	}
	got := byKey(all)
	if _, leak := got["secret@other.example"]; leak {
		t.Fatal("another user's sender appeared in the ranking")
	}
	for _, me := range []string{"me@mine.example", "anything@wild.example"} {
		if _, ok := got[me]; ok {
			t.Errorf("%s is the user, not a sender", me)
		}
	}
	news := got["news@list.example"]
	if news.Total != 5 || news.Unread != 4 || news.Neglected != 5 || news.DisplayName != "News" {
		t.Errorf("news@ = %+v, want total 5, unread 4, neglected 5 (4 unread + 1 archived unread)", news)
	}
	// The newest INGESTED message carrying the header (max ULID): ids[1] was
	// created after ids[0] by the seed, although ids[0] has the later date.
	if news.UnsubscribeMessageID == nil || *news.UnsubscribeMessageID != ids[1] {
		t.Errorf("news@ unsubscribe message = %v, want the newest-ingested with the header %s", news.UnsubscribeMessageID, ids[1])
	}
	boss := got["boss@work.example"]
	if boss.Total != 4 || boss.Neglected != 0 || boss.UnsubscribeMessageID != nil {
		t.Errorf("boss@ = %+v, want total 4 (trashed, draft and junk not counted), neglected 0", boss)
	}
	if len(boss.Addresses) != 1 || boss.Addresses[0].ContactID == nil || *boss.Addresses[0].ContactID != bossContact || !boss.Addresses[0].Blocked {
		t.Errorf("boss@ addresses = %+v, want the blocked contact %s", boss.Addresses, bossContact)
	}
	if all[0].Key != "news@list.example" || all[1].Key != "boss@work.example" {
		t.Errorf("volume order = %s, %s, want news then boss", all[0].Key, all[1].Key)
	}

	// The window: 365 days leaves boss's old one out.
	since := time.Now().Add(-365 * 24 * time.Hour)
	year, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Since: &since})
	if b := byKey(year)["boss@work.example"]; b.Total != 3 {
		t.Errorf("boss@ in 365 days = %d, want 3", b.Total)
	}
	month := time.Now().Add(-30 * 24 * time.Hour)
	m30, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Since: &month})
	if n := byKey(m30)["news@list.example"]; n.Total != 3 || n.Neglected != 3 {
		t.Errorf("news@ in 30 days = %+v, want 3/3", n)
	}

	// Neglect ranking: the neglected count first.
	neg, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Sort: SenderSortNeglect})
	// news (5 neglected), then the two with 1 each (share 1.0, one message;
	// key breaks the tie), then boss (4 messages, none neglected) — where a
	// volume order would put boss second.
	wantNeg := []string{"news@list.example", "news2@list.example", "promo1@shop.example", "boss@work.example", "promo2@shop.example"}
	for i, k := range wantNeg {
		if i >= len(neg) || neg[i].Key != k {
			t.Errorf("neglect order[%d] = %v, want %s (full: %+v)", i, func() string {
				if i < len(neg) {
					return neg[i].Key
				}
				return "<none>"
			}(), k, neg)
		}
	}

	// Domain: both shop.example addresses in one row, listed.
	dom, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByDomain})
	shop := byKey(dom)["shop.example"]
	if shop.Total != 2 || shop.AddressCount != 2 || len(shop.Addresses) != 2 {
		t.Errorf("shop.example = %+v, want 2 messages from 2 addresses", shop)
	}

	// List: the rotated address joins the list's row, and the key is what
	// list: searches for (search.normalizeListID): "news.list.example".
	lists, ltotal := senderStats(t, st, user, SenderStatsQuery{Group: SenderByList})
	if ltotal != 1 || lists[0].Key != "news.list.example" || lists[0].Total != 6 || lists[0].AddressCount != 2 {
		t.Errorf("lists = %+v (total %d), want one row news.list.example with 6 messages from 2 addresses", lists, ltotal)
	}

	// Paging carries the full total; past the end still reports it.
	page, ptotal := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Limit: 2, Position: 2})
	if len(page) != 2 || ptotal != 5 || page[0].Key != all[2].Key {
		t.Errorf("page 2 = %+v total %d", page, ptotal)
	}
	past, pastTotal := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Position: 50})
	if len(past) != 0 || pastTotal != 5 {
		t.Errorf("past the end: %d rows, total %d, want 0 and 5", len(past), pastTotal)
	}

	// RLS: the other user sees only their own two senders.
	theirs, theirTotal := senderStats(t, st, other, SenderStatsQuery{Group: SenderByAddress})
	if theirTotal != 2 || byKey(theirs)["news@list.example"].Total != 2 {
		t.Errorf("other user's ranking = %+v (total %d), want their own 2 senders only", theirs, theirTotal)
	}

	if _, _, err := (func() ([]SenderStat, int, error) {
		var e error
		_ = st.UserTx(ctx, user, func(tx *UserTx) error {
			_, _, e = tx.SenderStats(ctx, SenderStatsQuery{Group: "nonsense"})
			return nil
		})
		return nil, 0, e
	})(); err == nil {
		t.Error("an unknown grouping was accepted")
	}
}

// THE MEASUREMENT (#53): a seeded 25,000-message mailbox — 400 senders over
// 120 domains and 60 lists, skewed so a few senders dominate, two years of
// mail, 2,000 archive_unread events — plus 5,000 messages of another user in
// the same tables. Each ranking is timed (best of 3, after a warm-up) and its
// plan logged; the bound asserted is generous on purpose (a CI runner is not a
// benchmark machine): it exists to catch a pathological plan, an order of
// magnitude off, not a few milliseconds.
// The ranking can hold keys a malformed From produced, and a group's
// addresses carry the correspondence signal (#53 security review): rows whose
// key would search for something else are marked non-actionable, and an
// address the user has written to is flagged so a group block leaves it out
// by default.
func TestSenderStatsMalformedKeysAndCorrespondents(t *testing.T) {
	h := newHarness(t)
	st := h.appStore()
	ctx := context.Background()
	user := seedStatUser(t, h)
	seedStatMsgs(t, h, user, []statMsg{
		{from: "example.com", daysOld: 1},
		{from: "@example.com", daysOld: 1},
		{from: "*.example.com", daysOld: 1},
		{from: ".example.com", daysOld: 1},
		{from: "alice@shop.example", daysOld: 1},
		{from: "friend@shop.example", daysOld: 1},
	})
	seedContact(t, h, user, "alice@shop.example", false, false)
	friend := seedContact(t, h, user, "friend@shop.example", false, false)
	if _, err := h.super.Exec(ctx, `UPDATE app.contact_addresses SET outbound_count = 3 WHERE contact_id = $1`, friend.String()); err != nil {
		t.Fatal(err)
	}

	addrs, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress})
	got := byKey(addrs)
	for _, bad := range []string{"example.com", "@example.com", "*.example.com", ".example.com"} {
		s, ok := got[bad]
		if !ok {
			t.Fatalf("the malformed sender %q is not in the ranking at all (it should be counted, and shown as non-actionable): %+v", bad, addrs)
		}
		if s.Actionable {
			t.Errorf("address row %q is actionable — from:%s would search a whole domain", bad, bad)
		}
	}
	if !got["alice@shop.example"].Actionable {
		t.Error("a well-formed address row is not actionable")
	}

	doms, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByDomain})
	shop := byKey(doms)["shop.example"]
	if !shop.Actionable {
		t.Error("the shop.example domain row is not actionable")
	}
	corr := map[string]bool{}
	for _, a := range shop.Addresses {
		corr[a.Address] = a.Correspondent
	}
	if !corr["friend@shop.example"] || corr["alice@shop.example"] {
		t.Errorf("correspondent flags = %v, want friend@ true (written to) and alice@ false", corr)
	}
}

// COUNT = IDS (#53 security review): every clean-up action moves the ids
// SenderMessageIDs enumerates, so for every row, in every grouping, in one
// window, the enumeration must be exactly the row — never a sibling sub-list
// (`list:` would take x.news.example.org with news.example.org) or a
// subdomain the row did not count.
func TestSenderStatsCountEqualsIDs(t *testing.T) {
	h := newHarness(t)
	st := h.appStore()
	ctx := context.Background()
	user := seedStatUser(t, h)
	other := seedStatUser(t, h)
	seedStatMsgs(t, h, user, []statMsg{
		{from: "a@news.example.org", daysOld: 1, listID: "<news.example.org>"},
		{from: "a@news.example.org", daysOld: 2, listID: "<news.example.org>"},
		{from: "a@news.example.org", daysOld: 200, listID: "<news.example.org>"},
		{from: "b@x.news.example.org", daysOld: 1, listID: "<x.news.example.org>"},
		{from: "b@x.news.example.org", daysOld: 3, listID: "<x.news.example.org>"},
		{from: "c@example.org", daysOld: 1},
		{from: "d@sub.example.org", daysOld: 1},
		{from: "d@sub.example.org", daysOld: 2},
		{from: "c@example.org", daysOld: 1, trashed: true},
	})
	// The same keys at another user: never in this user's ids.
	seedStatMsgs(t, h, other, []statMsg{
		{from: "a@news.example.org", daysOld: 1, listID: "<news.example.org>"},
		{from: "c@example.org", daysOld: 1},
	})

	ids := func(u ulid.ULID, g SenderGroup, key string, since *time.Time, pos, limit int) ([]ulid.ULID, int) {
		var (
			out   []ulid.ULID
			total int
		)
		if err := st.UserTx(ctx, u, func(tx *UserTx) error {
			var e error
			out, total, e = tx.SenderMessageIDs(ctx, SenderIDsQuery{Group: g, Key: key, Since: since, Position: pos, Limit: limit})
			return e
		}); err != nil {
			t.Fatalf("SenderMessageIDs(%s, %s): %v", g, key, err)
		}
		return out, total
	}
	month := time.Now().Add(-30 * 24 * time.Hour)
	checked := 0
	for _, since := range []*time.Time{nil, &month} {
		for _, g := range []SenderGroup{SenderByAddress, SenderByDomain, SenderByList} {
			rows, _ := senderStats(t, st, user, SenderStatsQuery{Group: g, Since: since, Limit: MaxSenderStatsPage})
			if len(rows) == 0 {
				t.Fatalf("%s: no rows", g)
			}
			for _, r := range rows {
				got, total := ids(user, g, r.Key, since, 0, 0)
				if total != r.Total || len(got) != r.Total {
					t.Errorf("%s %q (window %v): the row counts %d, the enumeration holds %d (total %d)",
						g, r.Key, since != nil, r.Total, len(got), total)
				}
				checked++
			}
		}
	}
	// The two siblings, named: the parent list is 3 messages all time, 2 in
	// the month; the sub-list is its own row of 2.
	if _, n := ids(user, SenderByList, "news.example.org", nil, 0, 0); n != 3 {
		t.Errorf("news.example.org enumerates %d, want 3 (not the sub-list's 2 as well)", n)
	}
	if _, n := ids(user, SenderByDomain, "example.org", nil, 0, 0); n != 1 {
		t.Errorf("example.org enumerates %d, want 1 (not sub.example.org's 2, not the trashed one)", n)
	}
	// Paging: 1,000-cap semantics on a small set — two pages, no overlap.
	p1, n := ids(user, SenderByList, "news.example.org", nil, 0, 2)
	p2, _ := ids(user, SenderByList, "news.example.org", nil, 2, 2)
	if n != 3 || len(p1) != 2 || len(p2) != 1 || p1[0] == p2[0] || p1[1] == p2[0] {
		t.Errorf("paging: %v then %v (total %d)", p1, p2, n)
	}
	if _, n := ids(user, SenderByList, "news.example.org", nil, 50, 2); n != 3 {
		t.Errorf("past the end: total %d, want 3", n)
	}
	// RLS: the other user's enumeration of the same keys holds only their own.
	if got, n := ids(other, SenderByList, "news.example.org", nil, 0, 0); n != 1 || len(got) != 1 {
		t.Errorf("other user's news.example.org = %v (%d), want their 1", got, n)
	}
	// A key that is not actionable is refused, not enumerated.
	if err := st.UserTx(ctx, user, func(tx *UserTx) error {
		_, _, e := tx.SenderMessageIDs(ctx, SenderIDsQuery{Group: SenderByAddress, Key: "example.org"})
		return e
	}); !errors.Is(err, ErrSenderStatsInvalid) {
		t.Errorf("a domain-shaped address key: err=%v, want ErrSenderStatsInvalid", err)
	}
	t.Logf("count = ids held for %d rows across 3 groupings and 2 windows", checked)
}

func TestSenderStatsAt25k(t *testing.T) {
	h := newHarness(t)
	st := h.appStore()
	ctx := context.Background()
	user := seedStatUser(t, h)
	other := seedStatUser(t, h)
	r := rand.New(rand.NewSource(53))
	gen := func(n int) []statMsg {
		out := make([]statMsg, n)
		for i := range out {
			// Zipf-ish: sender index skewed toward low numbers.
			s := int(float64(400) * r.Float64() * r.Float64() * r.Float64())
			d := s % 120
			m := statMsg{
				from:    fmt.Sprintf("sender%d@domain%d.example", s, d),
				name:    fmt.Sprintf("Sender %d", s),
				daysOld: r.Intn(730),
				seen:    r.Float64() < 0.6,
			}
			if s%7 == 0 {
				m.listID = fmt.Sprintf("<list%d.lists.example>", s%60)
				m.unsub = true
			}
			out[i] = m
		}
		return out
	}
	start := time.Now()
	ids := seedStatMsgs(t, h, user, gen(25000))
	seedStatMsgs(t, h, other, gen(5000))
	// Folders, so the trash and junk exclusions (the search's own, which
	// read provenance) are measured as they run on a real mailbox: one
	// account with INBOX, Trash and Junk, 24,000 messages live in INBOX and
	// 500 each in the other two.
	acct, inbox, trashBox, junkBox := NewULID(), NewULID(), NewULID(), NewULID()
	if _, err := h.super.Exec(ctx, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1, $2, 'p', 'imap.example.com', 'smtp.example.com', '\x00', $3)`, acct.String(), user.String(), NewULID().String()); err != nil {
		t.Fatal(err)
	}
	for id, m := range map[ulid.ULID][2]string{inbox: {"INBOX", ""}, trashBox: {"Trash", `\Trash`}, junkBox: {"Junk", `\Junk`}} {
		if _, err := h.super.Exec(ctx, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
			VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''))`, id.String(), user.String(), acct.String(), m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	prov := make([][]any, len(ids))
	for i, id := range ids {
		box := inbox
		switch {
		case i < 500:
			box = trashBox
		case i < 1000:
			box = junkBox
		}
		prov[i] = []any{NewULID().String(), user.String(), id.String(), acct.String(), box.String(), int64(i + 1), int64(1), "live"}
	}
	if _, err := h.super.CopyFrom(ctx, pgx.Identifier{"app", "message_provenance"},
		[]string{"id", "user_id", "message_id", "account_id", "mailbox_id", "uid", "uidvalidity", "state"}, pgx.CopyFromRows(prov)); err != nil {
		t.Fatalf("copy provenance: %v", err)
	}
	// Feedback in realistic proportion: archive_unread is a minority of what
	// the table holds (every mark-read, flag, reply and trash is a row too),
	// which is the case the partial index exists for.
	fb := make([][]any, 0, 42000)
	other4 := []string{"mark_read_no_open", "flag", "reply", "trash"}
	for i := 0; i < 2000; i++ {
		fb = append(fb, []any{NewULID().String(), user.String(), ids[r.Intn(len(ids))].String(), "archive_unread"})
	}
	for i := 0; i < 40000; i++ {
		fb = append(fb, []any{NewULID().String(), user.String(), ids[r.Intn(len(ids))].String(), other4[i%4]})
	}
	if _, err := h.super.CopyFrom(ctx, pgx.Identifier{"app", "feedback_events"},
		[]string{"id", "user_id", "message_id", "action"}, pgx.CopyFromRows(fb)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `ANALYZE app.messages; ANALYZE app.feedback_events; ANALYZE app.message_provenance`); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded 25,000 + 5,000 messages and 42,000 feedback events (2,000 archive_unread) in %s", time.Since(start).Round(time.Millisecond))

	month := time.Now().Add(-30 * 24 * time.Hour)
	cases := []struct {
		name string
		q    SenderStatsQuery
	}{
		{"address/volume/all-time", SenderStatsQuery{Group: SenderByAddress, Sort: SenderSortVolume}},
		{"address/neglect/all-time", SenderStatsQuery{Group: SenderByAddress, Sort: SenderSortNeglect}},
		{"domain/volume/all-time", SenderStatsQuery{Group: SenderByDomain}},
		{"list/neglect/all-time", SenderStatsQuery{Group: SenderByList, Sort: SenderSortNeglect}},
		{"address/volume/30d", SenderStatsQuery{Group: SenderByAddress, Since: &month}},
	}
	// THE FLOOR, on the same machine and data: the plainest possible GROUP BY
	// over the same rows. The ranking's own numbers are read against it — a
	// slow runner makes both slow; a bad plan makes only the ranking slow.
	floor := time.Duration(1 << 62)
	for i := 0; i < 4; i++ {
		t0 := time.Now()
		if err := st.UserTx(ctx, user, func(tx *UserTx) error {
			rows, err := tx.tx.Query(ctx, `SELECT lower(from_address::text), count(*) FROM app.messages
				WHERE user_id = $1 GROUP BY 1`, user.String())
			if err != nil {
				return err
			}
			for rows.Next() {
			}
			rows.Close()
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(t0); i > 0 && d < floor {
			floor = d
		}
	}
	t.Logf("MEASURED %-26s %4s         best of 3: %s", "floor: bare GROUP BY", "", floor.Round(100*time.Microsecond))
	const bound = 2 * time.Second
	for _, c := range cases {
		senderStats(t, st, user, c.q) // warm-up
		best := time.Duration(1 << 62)
		var total int
		for i := 0; i < 3; i++ {
			t0 := time.Now()
			_, total = senderStats(t, st, user, c.q)
			if d := time.Since(t0); d < best {
				best = d
			}
		}
		t.Logf("MEASURED %-26s %4d groups  best of 3: %s", c.name, total, best.Round(100*time.Microsecond))
		if c.name == "address/volume/all-time" {
			// The 1,000 in Trash and Junk folders are not counted: the
			// exclusions ran, not just their cost.
			all, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Limit: MaxSenderStatsPage})
			rest, _ := senderStats(t, st, user, SenderStatsQuery{Group: SenderByAddress, Limit: MaxSenderStatsPage, Position: MaxSenderStatsPage})
			counted := 0
			for _, s := range append(all, rest...) {
				counted += s.Total
			}
			if counted != 24000 {
				t.Errorf("counted %d messages across all senders, want 24,000 (25,000 minus 500 in Trash and 500 in Junk)", counted)
			}
		}
		if best > bound {
			t.Errorf("%s took %s on 25k messages, bound %s — the plan is pathological", c.name, best, bound)
		}
	}

	// The plans, logged: the real statement (all-time, by address, by volume)
	// and the neglect set on its own, which must use the partial index once
	// archive_unread is a minority of the table.
	key, _ := senderKeySQL(SenderByAddress)
	explain := func(tx *UserTx, sql string, args ...any) string {
		rows, err := tx.tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		return plan.String()
	}
	if err := st.UserTx(ctx, user, func(tx *UserTx) error {
		excl, err := tx.senderStatsExclusions(ctx)
		if err != nil {
			return err
		}
		args := append([]any{user.String(), nil, 50, 0, []string{}, []string{}}, excl.args...)
		full := explain(tx, senderStatsSQL(key, `g.total DESC, g.neglected DESC, g.last DESC, g.k`, excl.sql), args...)
		t.Logf("ranking plan (address, volume, all time):\n%s", full)
		set := explain(tx, `SELECT DISTINCT f.message_id FROM app.feedback_events f
			 WHERE f.user_id = $1 AND f.action = 'archive_unread' AND f.message_id IS NOT NULL`, user.String())
		t.Logf("neglect-set plan:\n%s", set)
		if !strings.Contains(set, "feedback_archive_unread") {
			t.Errorf("the archive_unread set does not use feedback_archive_unread:\n%s", set)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
