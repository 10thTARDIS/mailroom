// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dumpBlockers logs who is waiting on whom in the test database (#855). The
// database-level lock_timeout that scripts/integration-test.sh sets turns a
// silent hang into a failed statement, but the error only says THAT a lock wait
// timed out; this says which session held the lock and what it was doing. It
// runs from a failed test's cleanup, as the superuser (pg_stat_activity hides
// other roles' queries from a non-superuser), and is best-effort: a diagnostic
// that errors just says so, it never changes the verdict. A holder that already
// finished leaves nothing to list, which is itself a finding (the stall was a
// timeout, not a deadlock).
func dumpBlockers(t *testing.T, super *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := super.Query(ctx, `
		SELECT a.pid, a.usename::text, a.state, coalesce(a.wait_event_type, ''), coalesce(a.wait_event, ''),
		       pg_blocking_pids(a.pid)::text,
		       coalesce(l.relation::regclass::text, ''),
		       coalesce(l.mode, ''),
		       coalesce((now() - a.xact_start)::text, ''),
		       left(a.query, 200)
		  FROM pg_stat_activity a
		  LEFT JOIN pg_locks l ON l.pid = a.pid AND NOT l.granted
		 WHERE a.datname = current_database() AND a.pid <> pg_backend_pid()
		   AND (a.state <> 'idle' OR cardinality(pg_blocking_pids(a.pid)) > 0)
		 ORDER BY a.xact_start`)
	if err != nil {
		t.Logf("lock diagnostics unavailable: %v", err)
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var (
			pid                                                      int32
			user, state, wtype, wevent, blockedBy, rel, mode, age, q string
		)
		if err := rows.Scan(&pid, &user, &state, &wtype, &wevent, &blockedBy, &rel, &mode, &age, &q); err != nil {
			t.Logf("lock diagnostics unreadable: %v", err)
			return
		}
		n++
		t.Logf("pg session pid=%d user=%s state=%s wait=%s/%s blocked_by=%s waiting_on=%s(%s) xact_age=%s query=%q",
			pid, user, state, wtype, wevent, blockedBy, rel, mode, age, q)
	}
	if n == 0 {
		t.Logf("lock diagnostics: no active or blocked sessions remain (the holder had already finished)")
	}
}
