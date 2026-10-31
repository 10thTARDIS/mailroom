// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for RFC 8621 §4.4.4 collapseThreads on Email/query
// (mailroom#210, server half only): "Emails in the same Thread as a previous
// Email in the list (given the filter and sort order) will be removed from
// the list" — for the returned page AND for `total`.
//
// The fixture below deliberately interleaves one 4-message thread with
// several singleton threads and two THREAD-LESS messages (thread_id IS NULL),
// at times chosen so that "one row per thread" alone would not catch a window
// whose ORDER BY had drifted from the outer query's — only checking WHICH row
// survives, in BOTH sort directions, catches that (test 3 below).

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"
)

// collapseFixture seeds one user with:
//   - a 4-message thread ("thread A"), received oldest→newest as aOld, aMid1,
//     aMid2, aNew (offsets -50, -40, -20, -5 minutes from now)
//   - three singleton threads (one message each): s1, s2, s3
//   - two THREAD-LESS messages (thread_id IS NULL): n1, n2
//
// 9 messages total, 6 distinct threads (a NULL thread_id message is its own
// "thread" for collapsing purposes — see QueryEmails).
type collapseFixture struct {
	u                        string
	aOld, aMid1, aMid2, aNew string
	s1, s2, s3               string
	n1, n2                   string
}

func seedCollapseFixture(t *testing.T, h *harness) collapseFixture {
	t.Helper()

	uid := NewULID()
	f := collapseFixture{u: uid.String()}
	mustExec(t, h, `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1,$2,$3)`,
		f.u, "sub-"+f.u, f.u+"@example.com")

	newThread := func() string {
		id := NewULID().String()
		mustExec(t, h, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1,$2,'t')`,
			id, f.u)
		return id
	}

	// msg inserts one canonical message (its own blob) at offsetMinutes from
	// now, in threadID (empty string means thread_id stays NULL), and returns
	// its id. Distinct blobs per message, tagged by a fresh ULID rather than a
	// counter, so this helper needs no external state.
	msg := func(threadID string, offsetMinutes int) string {
		t.Helper()
		tag := NewULID().String()
		hash := sha256.Sum256([]byte(tag))
		mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, f.u, hash[:], NewULID().String())
		id := NewULID().String()
		var tid any
		if threadID != "" {
			tid = threadID
		}
		mustExec(t, h, `INSERT INTO app.messages
			        (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
			      VALUES ($1,$2,$3,$4,'m', now() + make_interval(mins => $5), 10)`,
			id, f.u, hash[:], tid, offsetMinutes)
		return id
	}

	threadA := newThread()
	f.aOld = msg(threadA, -50)
	f.aMid1 = msg(threadA, -40)
	f.aMid2 = msg(threadA, -20)
	f.aNew = msg(threadA, -5)

	f.s1 = msg(newThread(), -45)
	f.s2 = msg(newThread(), -30)
	f.s3 = msg(newThread(), -10)

	f.n1 = msg("", -25)
	f.n2 = msg("", -15)

	return f
}

// runCollapseQuery runs q against the fixture's user and returns the ordered
// ids (as strings, preserving order — several assertions below depend on it)
// plus the reported total.
func runCollapseQuery(t *testing.T, h *harness, uid string, q EmailQuery) ([]string, int) {
	t.Helper()
	ctx := context.Background()
	st := h.appStore()
	userID := ulid.MustParse(uid)

	var (
		ids   []ulid.ULID
		total int
	)
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		var err error
		ids, total, err = tx.QueryEmails(ctx, q)
		return err
	}); err != nil {
		t.Fatalf("QueryEmails: %v", err)
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out, total
}

func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// countIn returns how many of `want` appear in `got`.
func countIn(got []string, want ...string) int {
	set := idSet(got)
	n := 0
	for _, w := range want {
		if set[w] {
			n++
		}
	}
	return n
}

// Test 1: collapsing happens, WITH the required control. Thread A's four
// messages must collapse to exactly one row when collapseThreads:true — and,
// as the control, the identical query with collapseThreads:false must return
// all four. Without the control this test would also pass against a query
// that (by some unrelated bug) returned nothing at all for thread A.
func TestQueryEmailsCollapseThreadsCollapses(t *testing.T) {
	h := newHarness(t)
	f := seedCollapseFixture(t, h)
	threadAIDs := []string{f.aOld, f.aMid1, f.aMid2, f.aNew}

	uncollapsed, _ := runCollapseQuery(t, h, f.u, EmailQuery{})
	if n := countIn(uncollapsed, threadAIDs...); n != 4 {
		t.Fatalf("control (collapseThreads:false): thread A contributed %d of 9 rows, want 4 (%v)",
			n, uncollapsed)
	}

	collapsed, _ := runCollapseQuery(t, h, f.u, EmailQuery{CollapseThreads: true})
	if n := countIn(collapsed, threadAIDs...); n != 1 {
		t.Fatalf("collapseThreads:true: thread A contributed %d rows, want exactly 1 (%v)",
			n, collapsed)
	}
}

// Test 2: `total` reports the COLLAPSED count, not the raw row count. 9
// messages across 6 threads (thread A's 4, three singletons, two thread-less
// messages each counted as their own "thread" — see test 4).
func TestQueryEmailsCollapseThreadsTotal(t *testing.T) {
	h := newHarness(t)
	f := seedCollapseFixture(t, h)

	uncollapsedIDs, uncollapsedTotal := runCollapseQuery(t, h, f.u, EmailQuery{})
	if len(uncollapsedIDs) != 9 || uncollapsedTotal != 9 {
		t.Fatalf("collapseThreads:false: got %d ids / total %d, want 9/9 (%v)",
			len(uncollapsedIDs), uncollapsedTotal, uncollapsedIDs)
	}

	collapsedIDs, collapsedTotal := runCollapseQuery(t, h, f.u, EmailQuery{CollapseThreads: true})
	if len(collapsedIDs) != 6 {
		t.Fatalf("collapseThreads:true: got %d ids, want 6 (%v)", len(collapsedIDs), collapsedIDs)
	}
	if collapsedTotal != 6 {
		t.Fatalf("collapseThreads:true: total = %d, want 6 (the COLLAPSED count, not the raw row count 9)",
			collapsedTotal)
	}
}

// Test 3: the kept representative is the FIRST Email in the query's own sort
// order (RFC 8621 §4.4.4) — descending keeps the NEWEST, ascending keeps the
// OLDEST. This is the test that catches a window whose ORDER BY drifted from
// the outer query's: a test that only counts "one row per thread" cannot
// distinguish the correct representative from any other member of the
// thread, so it would still pass against that bug.
func TestQueryEmailsCollapseThreadsPicksFirstInSortOrder(t *testing.T) {
	h := newHarness(t)
	f := seedCollapseFixture(t, h)

	desc, _ := runCollapseQuery(t, h, f.u, EmailQuery{CollapseThreads: true})
	set := idSet(desc)
	switch {
	case !set[f.aNew]:
		t.Errorf("descending (default): thread A's representative is missing the NEWEST message %s; got %v", f.aNew, desc)
	case set[f.aOld] || set[f.aMid1] || set[f.aMid2]:
		t.Errorf("descending (default): thread A kept an OLDER message instead of the newest; got %v", desc)
	}

	asc, _ := runCollapseQuery(t, h, f.u, EmailQuery{CollapseThreads: true, SortAscending: true})
	set = idSet(asc)
	switch {
	case !set[f.aOld]:
		t.Errorf("ascending: thread A's representative is missing the OLDEST message %s; got %v", f.aOld, asc)
	case set[f.aMid1] || set[f.aMid2] || set[f.aNew]:
		t.Errorf("ascending: thread A kept a NEWER message instead of the oldest; got %v", asc)
	}
}

// Test 4: thread_id IS NULL messages must NOT collapse into each other.
// PARTITION BY groups every NULL together by default, which would fold n1
// and n2 into a single row; QueryEmails partitions on
// COALESCE(thread_id, own id) specifically to prevent that.
func TestQueryEmailsCollapseThreadsNullThreadNotCollapsed(t *testing.T) {
	h := newHarness(t)
	f := seedCollapseFixture(t, h)

	collapsed, _ := runCollapseQuery(t, h, f.u, EmailQuery{CollapseThreads: true})
	if n := countIn(collapsed, f.n1, f.n2); n != 2 {
		t.Fatalf("collapseThreads:true: thread-less messages n1/n2 contributed %d rows, want 2 (both must survive — a NULL thread_id is not a shared thread); got %v",
			n, collapsed)
	}
}

// Test 5: paging still works over a collapsed list — position/limit must
// slice the SAME collapsed, ordered result a full unwindowed query returns.
func TestQueryEmailsCollapseThreadsPaging(t *testing.T) {
	h := newHarness(t)
	f := seedCollapseFixture(t, h)

	full, total := runCollapseQuery(t, h, f.u, EmailQuery{CollapseThreads: true})
	if len(full) != 6 || total != 6 {
		t.Fatalf("setup: full collapsed query = %d ids / total %d, want 6/6 (%v)", len(full), total, full)
	}

	const pageSize = 2
	var paged []string
	for pos := 0; pos < len(full); pos += pageSize {
		page, pageTotal := runCollapseQuery(t, h, f.u, EmailQuery{
			CollapseThreads: true, Position: pos, Limit: pageSize,
		})
		if pageTotal != 6 {
			t.Fatalf("page at position %d: total = %d, want 6 (total must not depend on the window)", pos, pageTotal)
		}
		paged = append(paged, page...)
	}
	if len(paged) != len(full) {
		t.Fatalf("paged through %d ids, want %d (%v)", len(paged), len(full), paged)
	}
	for i := range full {
		if paged[i] != full[i] {
			t.Fatalf("page mismatch at index %d: got %s, want %s (paging must reproduce the unwindowed collapsed order exactly)",
				i, paged[i], full[i])
		}
	}
}
