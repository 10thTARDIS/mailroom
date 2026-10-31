# ADR-025: PostgreSQL 18 is the supported major; 16 remains the compatibility floor

**Status:** Accepted (deviates from the spec's and `CLAUDE.md`'s
"PostgreSQL 16", which this ADR supersedes for the *running* version — see §4 for
why the DDL's stated floor deliberately does **not** move)

## Context

The spec and `CLAUDE.md` name **PostgreSQL 16**, and `db/schema.sql` is described
as "the reference DDL that CI applies to a clean PostgreSQL 16". Renovate has been
proposing `postgres:16 → 18` since 2026-08-17.

That PR was **closed rather than merged**, and the reason matters more than the
version number:

> It changed `deploy/compose.yml` — the file the running instance uses — while the
> instance's `pgdata` volume was initialised by PostgreSQL 16. Major versions have
> incompatible on-disk formats, so `postgres:18` refuses to start on a PG16 data
> directory (`FATAL: database files are incompatible with server`).
>
> ⚠ **Every automated check would have passed anyway.** Every database job in CI
> builds a *throwaway, empty* cluster and applies `db/schema.sql` to it, where 18
> is perfectly happy. The failure would have surfaced only at the next
> `docker compose up -d` — a documented step of the release process — landing on
> whoever cut the next release rather than on whoever merged the bump.

So a version bump that is one line in a diff is, in this repository, a data
migration. That is the decision this ADR records.

## Why 18, and why now — stated honestly

**There is no forced deadline.** PostgreSQL 16 receives security fixes until
**November 2028**. Nothing is failing, and no feature is blocked. Anyone reading
this looking for a crisis will not find one.

The actual argument is about **when this costs least**:

- The database is **small**. Message bytes live in the blob store, not in
  PostgreSQL, so the database is metadata and a dump/restore takes seconds.
- There are **two users**, and the project is pre-publication.
- Every later moment is worse. The same upgrade against a multi-gigabyte database
  with real users is a maintenance window, a rehearsal, and an announcement. Today
  it is a few minutes on a test instance nobody else depends on.

⚠ **What is deliberately NOT claimed here.** PostgreSQL 18 ships asynchronous I/O
and B-tree skip scan, and skip scan is *plausibly* interesting for this schema
because every hot index leads with `user_id` (`CLAUDE.md`) and `user_id` has
exactly two distinct values on a personal instance — the low-cardinality leading
column skip scan exists for. **That has not been measured, and it is not a reason
this ADR rests on.** Recording it as a hypothesis worth testing later is honest;
citing it as a benefit would be this project's own recurring failure — a number
nobody audited, used to justify a decision.

The reason is currency at the cheapest possible moment, not performance.

## Decision

1. **The supported and shipped major is PostgreSQL 18.** Every *pin* moves in one
   change — `deploy/compose.yml`, `docker-compose.yml`, the CI workflow's throwaway
   containers, and the `MAILROOM_PG_IMAGE` / `PG_IMAGE` defaults in every script
   under `scripts/`. A partial bump is the failure mode to avoid: it means CI
   proves a different database than the one running.

2. **`db/schema.sql`'s stated floor stays "PostgreSQL 16+".** It is a *minimum*,
   not a pin, and it is still true — the DDL was applied cleanly to both 16 and 18
   as part of this work. Raising a floor nothing requires would be a claim without
   a cause.

3. **Comments in `db/migrations/*.sql` that describe behaviour measured on
   PostgreSQL 16 are left exactly as they are.** Migrations are forward-only and
   immutable once applied (ADR-008); their comments are a record of what was
   observed when they were written. Rewriting `00035`'s note about how "PostgreSQL
   16 gives it membership WITH ADMIN OPTION" to say 18 would falsify that record to
   make a search-and-replace look tidy.

4. **The upgrade mechanism is dump/restore, not `pg_upgrade`.** At the database sizes
   involved the restore takes seconds; `pg_upgrade` would need both majors' binaries in one image
   and buys nothing at this size. Dump/restore is also the path `scripts/backup.sh`
   and `scripts/restore-drill.sh` already exercise, so the upgrade rides on
   machinery that is already tested rather than on a one-off.

## Evidence this rests on

Measured before the decision, not after — all against **PostgreSQL 18.6**:

| check | result |
|---|---|
| `db/schema.sql` applied to a clean 18 | clean |
| all 37 migrations via goose, **plus the drift check against `schema.sql`** | pass |
| full integration suite (`scripts/integration-test.sh`) | pass |
| RLS isolation subtests, **observed running rather than assumed** | 1/2/3/5 pass; 4 skips for its documented pre-existing reason |
| **real PG16 dump → 18 restore**, from a running instance | `pg_restore` exit 0, no output |
| row/permission counts across the major | messages, blobs, users, labels, change_log, goose version, **policy count, FORCE-RLS table count** — identical |
| **RLS still enforces on the restored copy** | 0 rows with no identity; **all rows with it**; all rows as superuser |

The last row is the one that makes the rest meaningful. Restoring *data* across a
major version proves nothing about whether the *security posture* came with it, and
"0 rows visible" would read identically if the table were empty or the role could
not connect. The positive control rules both out.

The `migrate-check` result also settles a question this change raised: 00035 and
00037 encode role and ACL behaviour measured on PostgreSQL 16. Both applied
cleanly on 18 and the schema drift-check passed, so those behaviours did not
change underneath us.

## Consequences

- **The instance upgrade is a real operation with a real ordering**, and it is not
  reversible by editing a file: dump, stop, remove the `pgdata` volume, start 18,
  recreate the roles the dump does not carry, restore, then let the idempotent
  bootstrap one-shots and goose reconcile. ⚠ `pg_dump` does **not** carry roles —
  the restore drill learned this the hard way when `mailroom_wkd` arrived in 00035,
  and the same four roles must exist before any restore.
- **`pg_dump` must run as a role with `BYPASSRLS`.** Every app table is
  `FORCE ROW LEVEL SECURITY`, so an owner-role dump fails loudly rather than
  silently emitting nothing (`scripts/backup.sh` documents this, measured).
- **A dump taken before the upgrade is the only rollback.** Reverting the pin does
  not undo an upgraded data directory.
- **`scripts/restore-drill.sh` could not test this.** It used one image variable for
  both the source and destination clusters, so pointing it at 18 tested 18→18 — not
  the cross-version restore an upgrade actually performs. It now takes a source and
  a destination image separately, so the scenario this ADR rests on is a thing the
  drill can exercise instead of a thing done once by hand.
- **PostgreSQL 16 is no longer tested.** The floor in `db/schema.sql` says the DDL
  needs 16+, and that remains true, but CI proves 18 from here. A deployment on 16
  is unsupported rather than forbidden.
