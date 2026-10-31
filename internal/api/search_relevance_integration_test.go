// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The relevance sort end to end through Email/query (#724): the index ranks,
// Postgres pages. What has to hold is that best-match order is the order the
// rows come back in, that position/limit windows that one sequence, that
// collapseThreads keeps each thread's BEST match, and that a query with
// nothing for the index to rank falls back to the date order instead of
// failing or ordering arbitrarily.

import (
	"context"
	"crypto/sha256"
	"slices"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func TestSearchRelevanceSortAndPaging(t *testing.T) {
	x := newOpsFixture(t)
	ctx := context.Background()

	// Memory-backend scores for "zebra" (search.TextNode: subject 3, people 2,
	// plus the same fuzzy bonus for every exact hit):
	//   best  — subject AND sender name        → 5, oldest but one
	//   mid   — subject only, 2 days old       → 3
	//   mid2  — subject only, 4 days old       → 3 (ties mid; older, so after it)
	//   low   — sender name only, the newest   → 2
	// Newest-first is low, mid, best, mid2; best match is best, mid, mid2, low.
	seed := func(name, subject, fromName, age string, sameThread bool) {
		t.Helper()
		id := store.NewULID()
		hash := sha256.Sum256([]byte("rel-" + id.String()))
		mustExecAPI(t, x.h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,100,132,$3)`, x.a.UserID.String(), hash[:], store.NewULID().String())
		var thread any
		if sameThread {
			thread = x.a.ThreadID.String()
		}
		mustExecAPI(t, x.h, `INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
			from_name, from_address, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,$5,'',$6,'someone@example.com', now() - $7::interval, 100)`,
			id.String(), x.a.UserID.String(), hash[:], thread, subject, fromName, age)
		x.ids[name], x.name[id] = id, name
	}
	seed("best", "zebra report", "Zebra Co", "3 days", true)
	seed("mid", "zebra notes", "Friend", "2 days", false)
	seed("mid2", "zebra minutes", "Friend", "4 days", true)
	seed("low", "hello there", "Zebra Bot", "1 day", false)
	if _, err := x.ix.Reindex(ctx, x.a.UserID); err != nil {
		t.Fatal(err)
	}

	names := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, s := range ids {
			u, err := ulid.Parse(s)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, x.name[u])
		}
		return out
	}
	relevance := []map[string]any{{"property": "relevance"}}
	check := func(label string, res opsResult, total int, want ...string) {
		t.Helper()
		if got := names(res.IDs); !slices.Equal(got, want) {
			t.Errorf("%s: order %v, want %v", label, got, want)
		}
		if res.Total != total {
			t.Errorf("%s: total %d, want %d", label, res.Total, total)
		}
	}

	// Control: the default order is newest first, and it differs from best
	// match — otherwise every assertion below could pass by accident.
	check("newest first", x.query(t, x.a.UserID, "zebra", nil), 4, "low", "mid", "best", "mid2")

	check("best match", x.query(t, x.a.UserID, "zebra", map[string]any{"sort": relevance}),
		4, "best", "mid", "mid2", "low")

	// Paging: each window is a slice of the same sequence, and the total is
	// the whole match count, not the page.
	var paged []string
	for pos := 0; pos < 4; pos += 2 {
		res := x.query(t, x.a.UserID, "zebra", map[string]any{"sort": relevance, "position": pos, "limit": 2})
		if res.Total != 4 {
			t.Errorf("page at %d: total %d, want 4", pos, res.Total)
		}
		paged = append(paged, names(res.IDs)...)
	}
	if want := []string{"best", "mid", "mid2", "low"}; !slices.Equal(paged, want) {
		t.Errorf("pages joined = %v, want %v", paged, want)
	}

	// isAscending flips it, as it flips every other sort.
	check("worst match first",
		x.query(t, x.a.UserID, "zebra", map[string]any{"sort": []map[string]any{{"property": "relevance", "isAscending": true}}}),
		4, "low", "mid2", "mid", "best")

	// collapseThreads keeps each thread's best match: best and mid2 share a
	// thread, and best is the one that represents it.
	check("collapsed", x.query(t, x.a.UserID, "zebra", map[string]any{"sort": relevance, "collapseThreads": true}),
		3, "best", "mid", "low")

	// Nothing for the index to rank (a store-only query): the date order, not
	// an error and not an arbitrary order.
	byDate := x.query(t, x.a.UserID, "area:Work", nil)
	byRank := x.query(t, x.a.UserID, "area:Work", map[string]any{"sort": relevance})
	if !slices.Equal(byDate.IDs, byRank.IDs) || len(byDate.IDs) == 0 {
		t.Errorf("area:Work: relevance %v, want the date order %v", names(byRank.IDs), names(byDate.IDs))
	}
}
