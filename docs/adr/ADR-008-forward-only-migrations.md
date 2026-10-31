# ADR-008: Forward-only migrations, expand-and-contract, online backfills

**Status:** Accepted (spec v0.4 §16; design review D8)

## Context

Users will hold 10⁶–10⁷ message rows and tens of GB of blobs. A blocking
`UPDATE` in a migration is an outage; down-migrations against real mail data
are a fiction.

## Decision

Migrations are forward-only. Anything touching large tables uses
expand-and-contract: add new column/table → dual-write → online background
backfill job (never a blocking UPDATE in a migration) → cut reads over →
drop old in a later release. Backups record the schema version; restore
drills exercise migrating a restored older snapshot forward. The concrete
tool (goose / golang-migrate / Atlas — any is fine; the policy is what
matters) is chosen when the first real migration is needed; `db/schema.sql`
remains the reference DDL that CI applies to a clean database.

## Consequences

- Policy lives as `docs/migrations.md` (to be written with the first
  migration).
- Blob layout is migration-immune by construction (ADR-004).

## References

spec §16 · design review D8, deep-dive F
