# ADR-007: ULIDs for API object ids

**Status:** Accepted (spec v0.4 §14; design review D7)

## Context

JMAP wants opaque, stable object ids. Serial ids are enumerable (isolation
hazard at the API surface); UUIDv4 fragments hot B-tree indexes.

## Decision

IDs are ULIDs (26-char Crockford base32), generated app-side, stored as the
`app.ulid` text domain. Time-ordered (index-friendly inserts), opaque,
non-enumerable.

## Consequences

- `app.ulid` domain CHECK enforces the alphabet at the database boundary.
- The API never accepts a user id as a request parameter — identity comes
  from the token; ULIDs make object ids non-guessable but authorization
  still never relies on that.
- Exception: `audit_log.id` is a plain bigint identity (append-only, never
  FK-referenced; sequential inserts are the whole workload).

## References

spec §10, §14 · mailroom-data-model.md §1, §7 · design review D7
