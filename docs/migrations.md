# Schema migrations

One page, per spec §16 and ADR-008. If this doc and the spec disagree, the
spec wins — flag the conflict rather than resolving it silently.

## Tool: goose

We use [`goose`](https://github.com/pressly/goose) (`github.com/pressly/goose/v3`,
already a module dependency). Any of goose / golang-migrate / Atlas would
satisfy spec §16 — the policy below is what actually matters, not the tool.
goose was picked because it's a plain-SQL, single-binary tool with no code
generation or extra DSL: migrations are files under `db/migrations/`, which
keeps them reviewable as ordinary SQL diffs and keeps `db/schema.sql` (the
reference DDL) and the migration history in the same language.

## Two things that must always agree

- **`db/schema.sql`** — the reference DDL. CI applies it to a clean
  PostgreSQL 18 (`scripts/schema-apply.sh`) and the RLS negative-test suite
  runs against a database built from it. It is what a new deployment or a
  restore drill compares against, and what a human reads to understand "the
  schema" without replaying history.
- **`db/migrations/*.sql`** — the forward-only, ordered delta history that
  goose actually applies to real databases.

**Rule: every schema change is both a new migration file AND a matching edit
to `db/schema.sql`, in the same commit.** They describe the same database by
construction, not by convention — `scripts/migrate-check.sh` proves this by
building one database from migrations and one from `schema.sql` and diffing
`pg_dump --schema-only` output. A nonzero diff fails CI.

## Baseline

`db/migrations/00001_baseline.sql` is the starting point: its `-- +goose Up`
reproduces `db/schema.sql` as of the first migration (there is no production
database yet, so there is nothing to migrate *from* — the baseline exists
purely to give goose a starting version and to prove the tooling works).
Its `-- +goose Down` drops `schema app cascade` — full teardown, useful for
`goose reset` in dev/CI throwaway databases. Down migrations are not expected
to run against a real database with user data (see "forward-only" below);
the baseline's down is the one exception, since there's nothing to lose yet.

### Transactions and goose annotations (why the baseline looks the way it does)

`db/schema.sql` wraps itself in an explicit `BEGIN`/`COMMIT` because it's
meant to be run directly via `psql -f`. goose *also* wraps each migration
file in its own transaction by default, and a nested `BEGIN` inside an
already-open transaction is invalid. So the baseline migration **drops the
outer `BEGIN`/`COMMIT`** and relies entirely on goose's default per-file
transaction. This is safe here because every statement in the schema is
ordinary transactional DDL in Postgres (no `CREATE INDEX CONCURRENTLY`, no
`ALTER TYPE ... ADD VALUE` outside a transaction) — so there was no need for
the `-- +goose NO TRANSACTION` directive.

goose splits a migration file into individual statements on top-level `;`.
The schema has three blocks whose bodies contain internal `;` inside
`$$...$$` dollar-quoting — the role-bootstrap `DO $$ ... END $$;` block and
the two `CREATE FUNCTION ... AS $$ ... $$;` bodies (`app.current_user_id`,
`app.dequeue_due_jobs`). Each of those is wrapped in
`-- +goose StatementBegin` / `-- +goose StatementEnd` so goose treats the
whole block as one statement instead of splitting on the semicolons inside
it. **Any future migration that adds a function body, trigger body, or `DO`
block must do the same**, or goose will mis-split it and the migration will
fail (or silently do the wrong thing).

## Forward-only; expand-and-contract for large tables

Migrations are forward-only (ADR-008). Down-migrations against real mail
data are a fiction we don't pretend to support — `messages`,
`message_provenance`, and the blob tables reach 10⁶–10⁷ rows per user, and
reversing a schema change against that volume safely is not realistic.

For anything touching a large table, use expand-and-contract, never a
blocking `UPDATE` inside a migration:

1. **Expand** — migration adds the new column/table (nullable or with a
   cheap default; no rewrite of existing rows).
2. **Dual-write** — application code starts writing both old and new shape.
3. **Backfill** — an **online background job** (via the `app.jobs` table,
   spec §16 "Durable job model") walks existing rows in batches and
   populates the new shape. Never a single blocking `UPDATE` in a migration
   — that's a full-table lock / outage at this row count.
4. **Cut over** — application code switches reads to the new shape once the
   backfill job reports complete.
5. **Contract** — a later migration/release drops the old column/table,
   once nothing reads it anymore.

Each of those steps that touches DDL is its own migration. A single
"add + backfill + drop" migration defeats the purpose.

**Widening a CHECK on a table that grows.** `ADD CONSTRAINT … CHECK` validates
every existing row under an `ACCESS EXCLUSIVE` lock. Migration 00075 widened
the refusal-reason CHECKs on `app.api_key_events` and
`app.api_key_suppressed_refusals` that way because the tables were new and
small. Once `api_key_events` has real volume (it gains a row per agent call,
and retention is still a follow-up, ADR-042), a widening there should be
`ADD CONSTRAINT … NOT VALID` followed by `VALIDATE CONSTRAINT` in a separate
statement, which takes only a `SHARE UPDATE EXCLUSIVE` lock while it scans.
Once validated it dumps as an ordinary constraint, so db/schema.sql can keep
it inline and the drift check still agrees.

## Backfilling a FORCE-RLS table from a migration

Every user table carries `ENABLE` + `FORCE ROW LEVEL SECURITY` (CLAUDE.md
"Database access"), and migrations are applied by `mailroom_owner` — a role
with `rolbypassrls = false`, matching production, holding no policy of its
own. Under FORCE, the table owner is subject to RLS the same as any other
role without BYPASSRLS: with no policy that applies to mailroom_owner, an
ordinary `UPDATE`/`INSERT`/`DELETE` against that table from inside a
migration sees, and touches, zero rows.

Nothing about that failure is loud. The statement is valid SQL and runs
without error — an `UPDATE` matching zero rows is not a Postgres error, so
goose prints `OK` and moves on. `scripts/migrate-check.sh` cannot catch it
either: it applies migrations to a freshly created, empty database, where
every backfill's true row count is zero anyway.

This actually shipped. Migration `00050`'s

    UPDATE app.desk_preferences SET setup_completed_at = updated_at;

and `00047`'s

    UPDATE app.messages SET flagged_at = received_at WHERE is_flagged AND flagged_at IS NULL;

both matched zero rows on a deployed instance. `00050`'s backfill existed
specifically so an established desk would never read `setup_completed_at`
as NULL — the exact state the day-one wizard treats as "never set up" — and
it silently left every existing login in that state. `00047` had no
candidate rows on the instance at the time, so the identical defect was
present, just invisible there.

### The pattern

Wrap the DML in a transient RLS toggle, inside the same transaction as the
rest of the migration (goose wraps each file in one transaction by default —
don't opt a backfill migration out of that):

```sql
ALTER TABLE app.X NO FORCE ROW LEVEL SECURITY;
UPDATE app.X SET ... WHERE ...;
ALTER TABLE app.X FORCE ROW LEVEL SECURITY;
```

Two shapes that look plausible and are not the fix:

- **A policy granted to mailroom_owner.** That would be a permanent
  widening of the schema to cover a one-off write, and every future
  migration author would have to remember it exists and reason about
  whether it applies to them too.
- **`SET row_security = off`.** This only suppresses enforcement for a role
  that would otherwise have a matching policy evaluated. For a role with no
  BYPASSRLS attribute and no policy that would apply — mailroom_owner's
  exact situation — Postgres raises an error instead of the silent
  zero-row match above.

See `db/migrations/00051_backfills_under_force_rls.sql` for the corrective
migration that re-applies both backfills this way, idempotently (each is
guarded by the same `IS NULL` condition the original migration already had,
so a row touched between the original deploy and this fix keeps its own
value rather than being overwritten).

### The guard

`TestMigrationDMLRespectsForceRLS`
(`internal/store/backfill_force_rls_guard_test.go`) is a static, no-database
unit test that parses every `db/migrations/*.sql` file as text and fails —
naming the file, the statement, and the fix — if a new
`UPDATE`/`INSERT`/`DELETE` against a FORCE-RLS table is not wrapped this way
in the same file. It runs in the ordinary `go test ./...` lane, so it
catches this class of defect at compile-test speed rather than waiting for a
deploy to notice a wizard nobody expected.

**The one-line rule:** a DML statement in a migration on a user table that
is not wrapped in `NO FORCE`/`FORCE` around it has done nothing on every
real database, and `migrate-check` cannot tell you.

## Backups and restore drills

Backups record the schema version (the goose version currently applied) they
were taken at. A restore drill (spec §11) is not just "restore the bytes" —
it exercises **migrating a restored older snapshot forward**: restore the
snapshot into a scratch database, run `goose up` against it, and confirm it
lands on the current schema and passes the same checks as a fresh database.
This is the way we find out a migration silently assumed a newer starting
state than some real backup has.

## Creating a migration

```sh
# from the repo root, inside whatever environment has the goose CLI
# (or: go run github.com/pressly/goose/v3/cmd/goose create ...)
goose -dir db/migrations create <short_description> sql
```

This produces `db/migrations/NNNNN_short_description.sql` with empty
`-- +goose Up` / `-- +goose Down` sections. Naming: lowercase, `snake_case`,
short and descriptive (`add_thread_snippet_column`, not `fix_stuff`). Numbers
are goose's sequential prefix — don't hand-edit them or reorder existing
files.

Checklist for every new migration:

- [ ] `-- +goose Up` does the forward change; if it touches a large table,
      it's expand-only (see above) — the backfill is a job, not this file.
- [ ] `-- +goose Down` is provided for completeness/local dev, but is not
      relied on in production.
- [ ] Any function body / trigger body / `DO` block is wrapped in
      `-- +goose StatementBegin` / `-- +goose StatementEnd`.
- [ ] `db/schema.sql` is updated to match in the same commit.
- [ ] `sh scripts/migrate-check.sh` passes locally (applies all migrations
      *and* drift-checks against `db/schema.sql`).

## Running migrations

Applying migrations to a real database is done with the goose CLI (or
`go run github.com/pressly/goose/v3/cmd/goose`) pointed at
`db/migrations/`, e.g.:

```sh
goose -dir db/migrations postgres "$DATABASE_URL" up
```

Migrations are applied by `mailroom_owner` (or an equivalent owning
superuser in CI/dev) — never by `mailroom_app`, which is deliberately not
the schema owner (spec §10, RLS design).

## CI

CI should run `sh scripts/migrate-check.sh` as its own job (mirroring the
existing `schema-apply` and `isolation-tests` jobs in
`.github/workflows/ci.yml`): it applies `db/migrations/*.sql` to a clean
PostgreSQL 18 via goose and asserts success, then drift-checks that result
against a database built straight from `db/schema.sql`. A failure here means
the migration history and the reference schema have diverged — merge should
be blocked, the same way a red RLS isolation test blocks merge.

## ⚠ `ADD COLUMN` of a DOMAIN type rewrites the whole table

Nobody would guess this from the DDL, and it is the one that will hurt.

```sql
ALTER TABLE app.messages ADD COLUMN x bytea;       -- catalog-only, ~2 ms
ALTER TABLE app.messages ADD COLUMN x app.sha256;  -- FULL TABLE REWRITE
```

PostgreSQL can add a plain nullable column by writing one catalog row. It
**cannot** do that when the column's type is a DOMAIN carrying a `CHECK`
constraint — `app.sha256` and `app.ulid` both do — because it must verify the
constraint against every existing row. That is a full rewrite under an
`ACCESS EXCLUSIVE` lock, and this codebase sets no `lock_timeout`.

Measured, not inferred:

| what | time |
|---|---|
| plain `bytea`, 1M rows | 2 ms |
| `app.sha256`, 1M rows | 689 ms |
| `app.sha256`, 300k rows | 2.0 s |

Migration `00044` did exactly this to `app.attachments` and it was fine — that
table is small, and `mailroom-migrate` is a one-shot the app waits on, so the
lock is held while nothing else is running.

⚠ **The one to avoid is `app.messages`**, the largest table by orders of
magnitude. A domain-typed column there would take an `ACCESS EXCLUSIVE` lock for
the duration of a full rewrite, with no timeout, on a mailbox that may hold
hundreds of thousands of rows.

**If you need the domain's guarantee on a big table**, do it expand-and-contract
(ADR-008): add the column as the plain base type, backfill online, then add the
`CHECK` as `NOT VALID` and `VALIDATE CONSTRAINT` in a separate statement — which
takes only a `SHARE UPDATE EXCLUSIVE` lock and does not block reads or writes.

Migration `00045` is the counter-example worth copying: two plain
(`timestamptz`, `integer`) columns on `app.attachments`, applied in milliseconds on
a live instance.
