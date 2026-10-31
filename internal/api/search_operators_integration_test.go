// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The v1.88.0 search operators end to end through Email/query (#722, #723):
// the store-side predicates (area:, is:snoozed / is:muted / is:pinned /
// is:important, and how they compose with index terms under AND, OR and NOT),
// the scope operators (in:trash, in:junk, in:anywhere and the junk default),
// the v3 index fields read from Postgres by the real StoreSource (list:,
// has:unsubscribe, has:pdf, filename:, alias:, larger:), truthful totals and
// paging, the reported bound when the index's hit limit cuts a search short,
// and isolation: another user's areas are invisible.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/search"
	"mailroom/internal/store"
)

type opsFixture struct {
	h    *apiHarness
	a, b apiFixture
	ix   *search.Indexer

	// a's messages, by name; every one says "invoice" in its subject.
	ids  map[string]ulid.ULID
	name map[ulid.ULID]string
	bInX ulid.ULID // b's message in b's own area called "Work"
}

func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	ctx := context.Background()
	h := newAPIHarness(t)
	ix, err := search.NewIndexer(t.TempDir(), search.MemoryFactory, search.NewStoreSource(h.store), nil)
	if err != nil {
		t.Fatalf("search indexer: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	h.api.SetSearch(search.NewService(ix, search.NewStoreResolver(h.store)))

	a, b := seedTwo(t, h.super)
	x := &opsFixture{h: h, a: a, b: b, ix: ix, ids: map[string]ulid.ULID{}, name: map[ulid.ULID]string{}}

	areaLabels := func(f apiFixture, names ...string) map[string]string {
		create := map[string]any{}
		for i, n := range names {
			create[fmt.Sprint(i)] = map[string]any{"name": n}
		}
		res := call(t, h.api.areaSet, f.UserID, map[string]any{"create": create})
		created, _ := res["created"].(map[string]any)
		out := map[string]string{}
		for i, n := range names {
			obj, ok := created[fmt.Sprint(i)].(map[string]any)
			if !ok {
				t.Fatalf("create area %q: %#v", n, res)
			}
			out[n], _ = obj["labelId"].(string)
		}
		return out
	}
	aLabels := areaLabels(a, "Work", "Side Projects")
	bLabels := areaLabels(b, "Work", "Secret")
	junk := seedMailbox(t, h.super, a, "Junk", `\Junk`)

	uid := 1000
	seed := func(f apiFixture, name, subject string, mailbox *ulid.ULID, label string, extra string, args ...any) ulid.ULID {
		t.Helper()
		id := store.NewULID()
		hash := sha256.Sum256([]byte("ops-" + id.String()))
		mustExecAPI(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,100,132,$3)`, f.UserID.String(), hash[:], store.NewULID().String())
		mustExecAPI(t, h, `INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
			from_name, from_address, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,$5,'a preview','Friend','friend@example.com', now(), 100)`,
			id.String(), f.UserID.String(), hash[:], f.ThreadID.String(), subject)
		if mailbox != nil {
			uid++
			mustExecAPI(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
				VALUES ($1,$2,$3,$4,$5,$6,1)`,
				store.NewULID().String(), f.UserID.String(), id.String(), f.AccountID.String(), mailbox.String(), uid)
		}
		if label != "" {
			mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
				f.UserID.String(), id.String(), label)
		}
		if extra != "" {
			mustExecAPI(t, h, `UPDATE app.messages SET `+extra+` WHERE user_id = $1 AND id = $2`,
				append([]any{f.UserID.String(), id.String()}, args...)...)
		}
		if f.UserID == a.UserID {
			x.ids[name], x.name[id] = id, name
		}
		return id
	}
	inbox := a.InboxID
	seed(a, "inX1", "invoice x1", &inbox, aLabels["Work"], "")
	seed(a, "inX2", "invoice x2", &inbox, aLabels["Work"], "")
	seed(a, "inX3", "invoice x3", &inbox, aLabels["Work"], "")
	seed(a, "inY", "invoice y", &inbox, aLabels["Side Projects"], "")
	seed(a, "plain", "invoice plain", &inbox, "", "")
	seed(a, "snoozed", "invoice snoozed", &inbox, "", "snoozed_until = now() + interval '2 days'")
	seed(a, "woke", "invoice woke", &inbox, "", "snoozed_until = now() - interval '1 hour'")
	seed(a, "pinned", "invoice pinned", &inbox, "", "pinned_at = now()")
	seed(a, "above", "invoice above", &inbox, "", "")
	seed(a, "below", "invoice below", &inbox, "", "")
	seed(a, "placed", "invoice placed", &inbox, "", "line_side = 'surface', line_side_at = now()")
	seed(a, "junkFlag", "invoice junk flag", &inbox, "", "is_junk = true")
	seed(a, "junkFolder", "invoice junk folder", &junk, "", "")
	seed(a, "trashed", "invoice trashed", &inbox, "", "trashed_at = now(), purge_after = now() + interval '30 days'")
	seed(a, "list", "invoice list", &inbox, "",
		"list_id = $3, list_unsubscribe = $4, addressed_address = $5, size_bytes = $6",
		`"Billing" <billing.lists.shop.example>`, "<mailto:unsub@shop.example>", "shop@me.example.net", 3<<20)
	seed(a, "pdf", "invoice pdf", &inbox, "", "has_attachments = true")
	mustExecAPI(t, h, `INSERT INTO app.attachments (id, user_id, message_id, part_path, filename, content_type, size_bytes)
		VALUES ($1,$2,$3,'2','Invoice Q3.pdf','application/pdf',1234)`,
		store.NewULID().String(), a.UserID.String(), x.ids["pdf"].String())

	// Muted: a thread of its own, muted.
	muted := store.NewULID()
	mustExecAPI(t, h, `INSERT INTO app.threads (id, user_id, subject, message_count, last_received_at, muted_at)
		VALUES ($1,$2,'muted thread',1, now(), now())`, muted.String(), a.UserID.String())
	seed(a, "muted", "invoice muted", &inbox, "", "thread_id = $3", muted.String())

	// Scores against the default line (0.5): above, below; "placed" is below by
	// score but the user put it on the surface, which outranks the score.
	for name, score := range map[string]float64{"above": 0.9, "below": 0.1, "placed": 0.1} {
		mustExecAPI(t, h, `INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
			VALUES ($1,$2,$3,'test')`, a.UserID.String(), x.ids[name].String(), score)
	}

	binbox := b.InboxID
	x.bInX = seed(b, "", "invoice b work", &binbox, bLabels["Work"], "")
	seed(b, "", "invoice b secret", &binbox, bLabels["Secret"], "")

	for _, u := range []ulid.ULID{a.UserID, b.UserID} {
		if _, err := ix.Reindex(ctx, u); err != nil {
			t.Fatalf("reindex: %v", err)
		}
	}
	return x
}

type opsResult struct {
	IDs          []string `json:"ids"`
	Total        int      `json:"total"`
	SearchCapped int      `json:"searchCapped"`
}

func (x *opsFixture) query(t *testing.T, user ulid.ULID, q string, extra map[string]any) opsResult {
	t.Helper()
	args := map[string]any{"filter": map[string]any{"query": q}}
	for k, v := range extra {
		args[k] = v
	}
	raw, mErr := x.h.api.methods["Email/query"](context.Background(), user, mustJSON(t, args))
	if mErr != nil {
		t.Fatalf("Email/query %q: %+v", q, mErr)
	}
	var out opsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// expect asserts the query returns exactly the named messages of user a, and
// that total agrees with them.
func (x *opsFixture) expect(t *testing.T, q string, want ...string) {
	t.Helper()
	res := x.query(t, x.a.UserID, q, nil)
	got := map[string]bool{}
	for _, id := range res.IDs {
		u, err := ulid.Parse(id)
		if err != nil {
			t.Fatal(err)
		}
		n, ok := x.name[u]
		if !ok {
			n = "fixture:" + id
		}
		got[n] = true
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for w := range wantSet {
		if !got[w] {
			t.Errorf("%q: missing %s (got %v)", q, w, keys(got))
		}
	}
	for g := range got {
		if !wantSet[g] {
			t.Errorf("%q: returned %s, which it must not (want %v)", q, g, want)
		}
	}
	if res.Total != len(want) {
		t.Errorf("%q: total = %d, want %d", q, res.Total, len(want))
	}
	if res.SearchCapped != 0 {
		t.Errorf("%q: searchCapped = %d on a small result", q, res.SearchCapped)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// everyday is every message of a's that a plain "invoice" search should return:
// not trashed, not junk.
var everyday = []string{"inX1", "inX2", "inX3", "inY", "plain", "snoozed", "woke", "pinned",
	"above", "below", "placed", "list", "pdf", "muted"}

func without(all []string, drop ...string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	var out []string
	for _, a := range all {
		if !skip[a] {
			out = append(out, a)
		}
	}
	return out
}

func TestSearchStorePredicates(t *testing.T) {
	x := newOpsFixture(t)

	x.expect(t, "invoice", everyday...)
	x.expect(t, "area:Work", "inX1", "inX2", "inX3")
	x.expect(t, "area:work invoice", "inX1", "inX2", "inX3")
	x.expect(t, `area:"Side Projects"`, "inY")
	x.expect(t, "area:Work OR area:\"Side Projects\"", "inX1", "inX2", "inX3", "inY")
	x.expect(t, "invoice -area:Work", without(everyday, "inX1", "inX2", "inX3")...)
	// An unknown area matches nothing, like an unknown label — never everything.
	x.expect(t, "area:Nowhere")
	x.expect(t, "invoice area:Nowhere")
	x.expect(t, "is:snoozed", "snoozed")
	x.expect(t, "invoice -is:snoozed", without(everyday, "snoozed")...)
	x.expect(t, "is:pinned", "pinned")
	x.expect(t, "is:muted", "muted")
	x.expect(t, "is:important", "above", "placed")
	x.expect(t, "is:above-line", "above", "placed")
	x.expect(t, "invoice -is:important", without(everyday, "above", "placed")...)
	// Mixed with index terms under OR and NOT: the store-free side is answered
	// by the index as an id set.
	x.expect(t, "area:\"Side Projects\" OR subject:pinned", "inY", "pinned")
	x.expect(t, "(is:pinned OR subject:x2) -is:snoozed", "pinned", "inX2")
	x.expect(t, "-(area:Work OR subject:plain) invoice", without(everyday, "inX1", "inX2", "inX3", "plain")...)
	x.expect(t, "(area:Work subject:x2) OR is:muted", "inX2", "muted")
}

// Paging an area: search over the whole of it is an ordinary Email/query, so
// total is the area's size and the window moves through it.
func TestSearchAreaTotalAndPaging(t *testing.T) {
	x := newOpsFixture(t)
	seen := map[string]bool{}
	for pos := 0; pos < 3; pos++ {
		res := x.query(t, x.a.UserID, "area:Work", map[string]any{"position": pos, "limit": 1})
		if res.Total != 3 || len(res.IDs) != 1 {
			t.Fatalf("area:Work position %d: total %d ids %v, want total 3 and one id", pos, res.Total, res.IDs)
		}
		seen[res.IDs[0]] = true
	}
	if len(seen) != 3 {
		t.Errorf("paging area:Work visited %d distinct messages, want 3", len(seen))
	}
}

// Another user's areas are invisible: a's search for b's area name matches
// nothing, and b's "Work" finds only b's own mail.
func TestSearchAreaIsolation(t *testing.T) {
	x := newOpsFixture(t)
	x.expect(t, "area:Secret")
	res := x.query(t, x.b.UserID, "area:Work", nil)
	if len(res.IDs) != 1 || res.IDs[0] != x.bInX.String() {
		t.Errorf("b's area:Work = %v, want only b's own message %s", res.IDs, x.bInX)
	}
	for _, id := range res.IDs {
		if u, _ := ulid.Parse(id); x.name[u] != "" {
			t.Errorf("b's area:Work returned a's message %s", x.name[u])
		}
	}
	// And the store predicate itself fails closed: a's area id under b's
	// identity resolves nothing, even handed over directly.
	var aWork ulid.ULID
	if err := x.h.store.UserTx(context.Background(), x.a.UserID, func(tx *store.UserTx) error {
		var found bool
		var err error
		aWork, found, err = tx.AreaIDByName(context.Background(), "Work")
		if err == nil && !found {
			err = fmt.Errorf("a's Work area not found")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Control first: the very same predicate under a's own identity finds a's
	// three messages, so the zero below is isolation, not a predicate that
	// cannot match anything.
	if err := x.h.store.UserTx(context.Background(), x.a.UserID, func(tx *store.UserTx) error {
		_, total, err := tx.QueryEmails(context.Background(), store.EmailQuery{
			Search: &store.SearchPredicate{Op: store.SearchArea, AreaID: aWork}})
		if err == nil && total != 3 {
			err = fmt.Errorf("CONTROL: a sees %d messages in its own area, want 3", total)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := x.h.store.UserTx(context.Background(), x.b.UserID, func(tx *store.UserTx) error {
		ids, total, err := tx.QueryEmails(context.Background(), store.EmailQuery{
			Search: &store.SearchPredicate{Op: store.SearchArea, AreaID: aWork}})
		if err == nil && (len(ids) != 0 || total != 0) {
			err = fmt.Errorf("b sees %d messages (total %d) in a's area", len(ids), total)
		}
		return err
	}); err != nil {
		t.Error(err)
	}
}

func TestSearchScopeTrashAndJunk(t *testing.T) {
	x := newOpsFixture(t)
	// Junk — the $junk flag AND the Junk folder — is excluded by default.
	x.expect(t, "invoice", everyday...)
	x.expect(t, "in:junk", "junkFlag", "junkFolder")
	x.expect(t, "is:junk invoice", "junkFlag", "junkFolder")
	x.expect(t, "in:trash", "trashed")
	x.expect(t, "invoice in:anywhere", append(append([]string{}, everyday...), "junkFlag", "junkFolder", "trashed")...)
	x.expect(t, "subject:junk")
	x.expect(t, "subject:junk in:anywhere", "junkFlag", "junkFolder")
	// Junk named inside an OR switches the default off, and the index's own
	// junk flag answers it — which is the provider's $junk verdict only: mail
	// another client filed in the Junk folder without it is not in that
	// branch. (A REQUIRED in:junk is answered by Postgres and finds both; see
	// above.) The trashed message stays out: nothing asked for trash.
	x.expect(t, "subject:trashed OR in:junk", "junkFlag")
	// The RFC text filter keeps its plain JMAP meaning: no junk default.
	raw, mErr := x.h.api.methods["Email/query"](context.Background(), x.a.UserID,
		mustJSON(t, map[string]any{"filter": map[string]any{"text": "junk"}}))
	if mErr != nil {
		t.Fatal(mErr)
	}
	if got := idSet(t, raw); !got[x.ids["junkFlag"].String()] || !got[x.ids["junkFolder"].String()] {
		t.Errorf("filter.text must not get the search box's junk default: %v", got)
	}
	// A search inside the Junk view is a search of junk.
	raw, mErr = x.h.api.methods["Email/query"](context.Background(), x.a.UserID,
		mustJSON(t, map[string]any{"filter": map[string]any{"query": "invoice", "inMailbox": x.junkID(t).String()}}))
	if mErr != nil {
		t.Fatal(mErr)
	}
	if got := idSet(t, raw); !got[x.ids["junkFolder"].String()] {
		t.Errorf("a search in the Junk folder lost the junk in it: %v", got)
	}
}

func (x *opsFixture) junkID(t *testing.T) ulid.ULID {
	t.Helper()
	var id string
	if err := x.h.super.QueryRow(context.Background(),
		`SELECT id FROM app.mailboxes WHERE user_id = $1 AND imap_name = 'Junk'`, x.a.UserID.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return ulid.MustParse(id)
}

// The v3 index fields, read from Postgres by the real StoreSource.
func TestSearchV3IndexFieldsFromTheStore(t *testing.T) {
	x := newOpsFixture(t)
	x.expect(t, "list:billing.lists.shop.example", "list")
	x.expect(t, "list:shop.example", "list")
	x.expect(t, "has:unsubscribe", "list")
	x.expect(t, "alias:shop@me.example.net", "list")
	x.expect(t, "deliveredto:me.example.net", "list")
	x.expect(t, "larger:1M", "list")
	x.expect(t, "has:pdf", "pdf")
	x.expect(t, "filename:pdf", "pdf")
	x.expect(t, "filename:q3", "pdf")
	x.expect(t, "has:pdf OR has:unsubscribe", "pdf", "list")
	x.expect(t, "area:Work OR has:pdf", "inX1", "inX2", "inX3", "pdf")
	x.expect(t, "newer_than:1d invoice area:Work", "inX1", "inX2", "inX3")
	x.expect(t, "older_than:1d invoice")
}

// When the index's hit limit cuts a search short, the response says so rather
// than presenting the capped count as the total — and a search the store
// answers alone is never capped.
func TestSearchReportsTheIndexBound(t *testing.T) {
	x := newOpsFixture(t)
	ctx := context.Background()
	n := search.DefaultLimit + 20
	ids := make([]string, n)
	for i := range ids {
		ids[i] = store.NewULID().String()
	}
	mustExecAPI(t, x.h, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		SELECT $1, sha256(convert_to(u.id, 'UTF8')), 100, 132, $3
		  FROM unnest($2::text[]) AS u(id)`, x.a.UserID.String(), ids, store.NewULID().String())
	mustExecAPI(t, x.h, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
		  from_name, from_address, received_at, size_bytes)
		SELECT u.id, $1, sha256(convert_to(u.id, 'UTF8')), $3, 'bulk mail', '',
		  'Bulk', 'bulk@example.com', now(), 100
		  FROM unnest($2::text[]) AS u(id)`, x.a.UserID.String(), ids, x.a.ThreadID.String())
	if _, err := x.ix.Reindex(ctx, x.a.UserID); err != nil {
		t.Fatal(err)
	}
	res := x.query(t, x.a.UserID, "bulk", nil)
	if res.SearchCapped != search.DefaultLimit {
		t.Errorf("a search with %d matches: searchCapped = %d, want %d", n, res.SearchCapped, search.DefaultLimit)
	}
	if res.Total != search.DefaultLimit {
		t.Errorf("control: the capped total is %d, want the cap %d — the bound this test is about", res.Total, search.DefaultLimit)
	}
	// The area alone goes nowhere near the index.
	if res := x.query(t, x.a.UserID, "area:Work", nil); res.SearchCapped != 0 || res.Total != 3 {
		t.Errorf("area:Work: total %d searchCapped %d, want 3 and absent", res.Total, res.SearchCapped)
	}
}

// Email/query's searchLimit (#769) is what lets a bulk action reach every match.
// Without it a text search enumerates only the index's first DefaultLimit hits;
// with it, up to MaxLimit.
func TestSearchLimitArgument(t *testing.T) {
	x := newOpsFixture(t)
	ctx := context.Background()
	const n = 1200
	ids := make([]string, n)
	for i := range ids {
		ids[i] = store.NewULID().String()
	}
	mustExecAPI(t, x.h, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		SELECT $1, sha256(convert_to(u.id, 'UTF8')), 100, 132, $3
		  FROM unnest($2::text[]) AS u(id)`, x.a.UserID.String(), ids, store.NewULID().String())
	mustExecAPI(t, x.h, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
		  from_name, from_address, received_at, size_bytes)
		SELECT u.id, $1, sha256(convert_to(u.id, 'UTF8')), $3, 'bulk mail', '',
		  'Bulk', 'bulk@example.com', now(), 100
		  FROM unnest($2::text[]) AS u(id)`, x.a.UserID.String(), ids, x.a.ThreadID.String())
	// 700 of them sit in the inbox, so a structured filter drops the other 500.
	const inboxed = 700
	pids := make([]string, inboxed)
	for i := range pids {
		pids[i] = store.NewULID().String()
	}
	mustExecAPI(t, x.h, `
		INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		SELECT p.pid, $1, p.mid, $3, $4, 5000 + p.n, 1
		  FROM unnest($2::text[], $5::text[]) WITH ORDINALITY AS p(mid, pid, n)`,
		x.a.UserID.String(), ids[:inboxed], x.a.AccountID.String(), x.a.InboxID.String(), pids)
	if _, err := x.ix.Reindex(ctx, x.a.UserID); err != nil {
		t.Fatal(err)
	}
	q := func(extra map[string]any) (opsResult, *MethodError) {
		args := map[string]any{"filter": map[string]any{"from": "bulk@example.com"}, "calculateTotal": true}
		for k, v := range extra {
			args[k] = v
		}
		raw, mErr := x.h.api.methods["Email/query"](ctx, x.a.UserID, mustJSON(t, args))
		if mErr != nil {
			return opsResult{}, mErr
		}
		var out opsResult
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out, nil
	}

	// No argument: today's behaviour, asserted — capped at the default.
	res, mErr := q(map[string]any{"limit": 5000})
	if mErr != nil {
		t.Fatal(mErr)
	}
	if res.Total != search.DefaultLimit || res.SearchCapped != search.DefaultLimit {
		t.Errorf("no searchLimit: total %d searchCapped %d, want both %d", res.Total, res.SearchCapped, search.DefaultLimit)
	}

	// The maximum reaches all of them, and says nothing is cut off.
	res, mErr = q(map[string]any{"limit": 5000, "searchLimit": search.MaxLimit})
	if mErr != nil {
		t.Fatal(mErr)
	}
	if res.Total != n || len(res.IDs) != n || res.SearchCapped != 0 {
		t.Errorf("searchLimit max: total %d ids %d searchCapped %d, want %d, %d, 0", res.Total, len(res.IDs), res.SearchCapped, n, n)
	}
	seen := map[string]bool{}
	for _, id := range res.IDs {
		seen[id] = true
	}
	if len(seen) != n {
		t.Errorf("searchLimit max: %d distinct ids, want %d", len(seen), n)
	}

	// Paging past the default window: position 600 returns the remaining 600.
	res, mErr = q(map[string]any{"position": 600, "limit": 1000, "searchLimit": search.MaxLimit})
	if mErr != nil {
		t.Fatal(mErr)
	}
	if len(res.IDs) != n-600 || res.Total != n {
		t.Errorf("position 600: %d ids total %d, want %d and %d", len(res.IDs), res.Total, n-600, n)
	}

	// A structured filter combined with it still returns every match it keeps.
	inbox := x.a.InboxID.String()
	res, mErr = q(map[string]any{"limit": 5000, "searchLimit": search.MaxLimit,
		"filter": map[string]any{"from": "bulk@example.com", "inMailbox": inbox}})
	if mErr != nil {
		t.Fatal(mErr)
	}
	inInbox := inboxed
	if res.Total != inInbox || len(res.IDs) != inInbox || res.SearchCapped != 0 {
		t.Errorf("with inMailbox: total %d ids %d searchCapped %d, want %d, %d, 0", res.Total, len(res.IDs), res.SearchCapped, inInbox, inInbox)
	}

	// Clamped: below the default it is raised to it, so asking for less never
	// shrinks the old behaviour; negative is a refusal.
	res, mErr = q(map[string]any{"limit": 5000, "searchLimit": 10})
	if mErr != nil || res.SearchCapped != search.DefaultLimit {
		t.Errorf("searchLimit 10: err %v searchCapped %d, want %d", mErr, res.SearchCapped, search.DefaultLimit)
	}
	res, mErr = q(map[string]any{"limit": 5000, "searchLimit": 1_000_000})
	if mErr != nil || res.Total != n {
		t.Errorf("searchLimit 1e6: err %v total %d, want clamped to the max and %d", mErr, res.Total, n)
	}
	if _, mErr = q(map[string]any{"searchLimit": -1}); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("searchLimit -1: %v, want invalidArguments", mErr)
	}
}
