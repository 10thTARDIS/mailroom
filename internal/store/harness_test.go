// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration-test harness (data-model §5/§8). scripts/integration-test.sh
// provisions a throwaway PostgreSQL 18 with db/schema.sql applied and the
// app/admin roles made connectable, then exports:
//
//   MAILROOM_TEST_SUPER_URL  — superuser; fixture seeding only (bypasses RLS)
//   MAILROOM_TEST_APP_URL    — mailroom_app; what the application uses
//   MAILROOM_TEST_ADMIN_URL  — mailroom_admin; admin-plane grants only
//   MAILROOM_TEST_OWNER_URL  — mailroom_owner; what migrations run as (no
//                              BYPASSRLS, subject to FORCE RLS like any other
//                              non-superuser role — mailroom#505). Not part
//                              of the base skip gate below: only the
//                              backfill-under-FORCE-RLS test needs it, via
//                              ownerPool.
//
// Tests skip when the env is absent so a bare `go test -tags integration`
// without a database does not fail spuriously.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type harness struct {
	super *pgxpool.Pool // superuser: seeding + introspection only
	app   *pgxpool.Pool // mailroom_app: everything the application does
	admin *pgxpool.Pool // mailroom_admin: admin plane
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	adminURL := os.Getenv("MAILROOM_TEST_ADMIN_URL")
	if superURL == "" || appURL == "" || adminURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	open := func(url string) *pgxpool.Pool {
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			t.Fatalf("open pool: %v", err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	h := &harness{super: open(superURL), app: open(appURL), admin: open(adminURL)}
	// Registered AFTER the pools' Close cleanups, so it runs BEFORE them (LIFO)
	// and the pool is still open when a failed test asks who held what (#855).
	t.Cleanup(func() {
		if t.Failed() {
			dumpBlockers(t, h.super)
		}
	})

	// The suite assumes db/schema.sql is applied; fail loudly if not.
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'app'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("schema 'app' not applied (tables=%d err=%v) — run scripts/integration-test.sh", n, err)
	}
	return h
}

// appStore returns a Store backed by the mailroom_app pool — the exact
// production configuration.
func (h *harness) appStore() *Store { return NewFromPool(h.app) }

// ownerPool opens a connection as mailroom_owner — the role migrations
// actually run as (mailroom#505). Separate from newHarness's base skip gate
// because only the backfill-under-FORCE-RLS test needs it; scripts/
// integration-test.sh always sets MAILROOM_TEST_OWNER_URL, but a manual
// `go test -tags integration` run against an older harness invocation should
// still skip cleanly here rather than failing every other test.
func (h *harness) ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("MAILROOM_TEST_OWNER_URL")
	if url == "" {
		t.Skip("MAILROOM_TEST_OWNER_URL not set — run via scripts/integration-test.sh")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("open owner pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
