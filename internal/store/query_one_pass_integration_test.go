// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Email/query computes its page and its total in ONE statement (#665): the
// total rides on every page row as count(*) OVER (), and only an EMPTY page
// (no matches, a position at or past the end, limit 0) falls back to the
// separate count statement.
//
// This is the agreement test: for a matrix of queries, ids and total must equal
// what the old TWO-statement method returned. The reference (referenceTwoPass)
// lives in this file so production carries no test switch. It runs the SQL that
// QueryEmails actually sent, captured by a pgx tracer, with the window column
// removed — so it is the old page statement, byte for byte, including its
// LIMIT/OFFSET — and takes the old total from `SELECT count(*)` over the same
// statement without LIMIT/OFFSET (the old countSQL counted exactly those rows).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

const windowTotalCol = ", count(*) OVER () AS total"

func TestQueryEmailsOnePassAgreesWithTwoPass(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := seedCollapseFixture(t, h)
	userID := ulid.MustParse(f.u)

	// Two mailboxes in one account, a trashed message in thread A (the newest,
	// so a collapse that forgot the trash predicate would pick it).
	acct := NewULID().String()
	mustExec(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'A','mail.example.com','smtp.example.com',$3,$4)`,
		acct, f.u, []byte{0x01}, NewULID().String())
	mbX, mbY := NewULID(), NewULID()
	for i, mb := range []ulid.ULID{mbX, mbY} {
		name := []string{"INBOX", "Other"}[i]
		mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
			VALUES ($1,$2,$3,$4,$4)`, mb.String(), f.u, acct, name)
	}
	file := func(mb ulid.ULID, ids ...string) {
		for _, id := range ids {
			mustExec(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
				VALUES ($1,$2,$3,$4,$5,$6,1)`, NewULID().String(), f.u, id, acct, mb.String(), testUID())
		}
	}
	file(mbX, f.aOld, f.aNew, f.s1, f.n1)
	file(mbY, f.aMid1, f.s2, f.s3)
	var trashedID string
	if err := h.super.QueryRow(ctx, `SELECT thread_id FROM app.messages WHERE id = $1`, f.aNew).Scan(&trashedID); err != nil {
		t.Fatal(err)
	}
	trashed := NewULID().String()
	hash := make([]byte, 32)
	copy(hash, trashed)
	mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,10,20,$3)`,
		f.u, hash, NewULID().String())
	mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes, trashed_at, purge_after)
		VALUES ($1,$2,$3,$4,'m', now() + interval '1 minute', 10, now(), now() + interval '30 days')`,
		trashed, f.u, hash, trashedID)
	file(mbX, trashed)

	// A traced pool, as in the desk-scale test, so the reference can replay the
	// exact statement QueryEmails sent.
	cfg, err := pgxpool.ParseConfig(os.Getenv("MAILROOM_TEST_APP_URL"))
	if err != nil {
		t.Fatalf("parse app url: %v", err)
	}
	rec := &sqlRecorder{}
	cfg.ConnConfig.Tracer = rec
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	st := NewFromPool(pool)

	// referenceTwoPass replays the page statement without its window column and
	// counts the same statement without LIMIT/OFFSET, via the old shape.
	referenceTwoPass := func(tx *UserTx, q EmailQuery) ([]string, int) {
		t.Helper()
		var page, full recordedSQL
		rec.stmts = nil
		if _, _, err := tx.QueryEmails(ctx, q); err != nil {
			t.Fatalf("QueryEmails %+v: %v", q, err)
		}
		for _, s := range rec.stmts {
			if strings.Contains(s.sql, windowTotalCol) {
				page = s
			}
		}
		if page.sql == "" {
			t.Fatalf("no one-pass statement recorded for %+v", q)
		}
		// The unwindowed statement: the same query with no limit/offset.
		unq := q
		unq.Limit, unq.LimitSet, unq.Position = 0, false, 0
		rec.stmts = nil
		if _, _, err := tx.QueryEmails(ctx, unq); err != nil {
			t.Fatalf("QueryEmails %+v: %v", unq, err)
		}
		for _, s := range rec.stmts {
			if strings.Contains(s.sql, windowTotalCol) {
				full = s
			}
		}
		oldPage := strings.Replace(page.sql, windowTotalCol, "", 1)
		rows, err := tx.tx.Query(ctx, oldPage, page.args...)
		if err != nil {
			t.Fatalf("reference page: %v", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("reference page rows: %v", err)
		}
		var total int
		oldFull := strings.Replace(full.sql, windowTotalCol, "", 1)
		if err := tx.tx.QueryRow(ctx, "SELECT count(*) FROM ("+oldFull+") old", full.args...).Scan(&total); err != nil {
			t.Fatalf("reference count: %v", err)
		}
		return ids, total
	}

	// The expected collapsed/uncollapsed totals are pinned so the reference
	// cannot agree with the new code by both being empty: 9 live messages
	// (the trashed one excluded), 6 threads.
	type tc struct {
		name string
		q    EmailQuery
	}
	var cases []tc
	for _, collapse := range []bool{false, true} {
		for _, asc := range []bool{false, true} {
			for _, mb := range []*ulid.ULID{nil, &mbX} {
				for _, lim := range []struct {
					name  string
					limit int
					set   bool
				}{{"nolimit", 0, false}, {"limit1", 1, true}, {"limit3", 3, true}, {"limit0", 0, true}} {
					for _, pos := range []int{0, 2, -1, -2, 99} { // -1: exactly at the end, -2: past it by one
						name := []string{"uncollapsed", "collapsed"}[b2i(collapse)] + "/" + lim.name
						cases = append(cases, tc{name, EmailQuery{
							CollapseThreads: collapse, SortAscending: asc, InMailbox: mb,
							Limit: lim.limit, LimitSet: lim.set, Position: pos,
						}})
					}
				}
			}
		}
	}
	// A filter that matches nothing.
	missing := NewULID()
	for _, collapse := range []bool{false, true} {
		cases = append(cases, tc{"nomatch", EmailQuery{CollapseThreads: collapse, InMailbox: &missing}},
			tc{"nomatch/limit3/pos2", EmailQuery{CollapseThreads: collapse, InMailbox: &missing, Limit: 3, LimitSet: true, Position: 2}})
	}

	sawPastEnd, sawEmptyNonzero := 0, 0
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		for _, c := range cases {
			q := c.q
			if q.Position < 0 { // resolve "at the end" / "past the end" against the real total
				unq := q
				unq.Position, unq.Limit, unq.LimitSet = 0, 0, false
				_, n, err := tx.QueryEmails(ctx, unq)
				if err != nil {
					t.Fatal(err)
				}
				if c.q.Position == -1 {
					q.Position = n
				} else {
					q.Position = n + 1
				}
			}
			gotIDs, gotTotal, err := tx.QueryEmails(ctx, q)
			if err != nil {
				t.Errorf("%s %+v: %v", c.name, q, err)
				continue
			}
			wantIDs, wantTotal := referenceTwoPass(tx, q)
			got := make([]string, len(gotIDs))
			for i, id := range gotIDs {
				got[i] = id.String()
			}
			if gotTotal != wantTotal || strings.Join(got, ",") != strings.Join(wantIDs, ",") {
				t.Errorf("%s %+v: one-pass = %d ids / total %d, two-pass = %d ids / total %d", c.name, q, len(got), gotTotal, len(wantIDs), wantTotal)
			}
			if len(got) == 0 && wantTotal > 0 {
				sawEmptyNonzero++ // an empty page over a non-empty result: the fallback's case
			}
			if q.Position > 0 && len(got) == 0 {
				sawPastEnd++
			}
		}
		// Pin absolute values so agreement cannot be two empties.
		for _, p := range []struct {
			collapse bool
			want     int
		}{{false, 9}, {true, 6}} {
			_, n, err := tx.QueryEmails(ctx, EmailQuery{CollapseThreads: p.collapse})
			if err != nil || n != p.want {
				t.Errorf("collapse=%v: total %d (err %v), want %d", p.collapse, n, err, p.want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The matrix must have exercised the fallback on a non-empty result (limit 0
	// and past-the-end), or the empty-page branch was never tested.
	if sawEmptyNonzero == 0 || sawPastEnd == 0 {
		t.Errorf("matrix never hit the empty-page fallback over a non-empty result (empty=%d pastEnd=%d)", sawEmptyNonzero, sawPastEnd)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
