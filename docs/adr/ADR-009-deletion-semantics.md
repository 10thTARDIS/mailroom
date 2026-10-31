# ADR-009: Deletion = per-canonical with per-provenance writeback; 30-day Trash; agent deletes always soft

**Status:** Accepted (spec v0.4 §4/§13a; design review D9)

## Context

A canonical message can live in several IMAP locations (provenances). "What
does delete mean?" interacts with dedup, writeback, blobs, the index, ML
features, and agent access — it must be one designed lifecycle.

## Decision

User "delete" trashes the **canonical message** (sets `trashed_at` /
`purge_after = now()+30d`, configurable), propagated as per-provenance IMAP
moves to Trash. Restore clears both and moves back (Archive if the origin
mailbox is gone). Hard delete (purge job or explicit) propagates to IMAP
(EXPUNGE where permitted), blobs, index, and ML feature store, emits
`Email destroyed`, and leaves only an audit tombstone
`{action, object_id, content_hash_prefix}`. Agent-initiated deletes
(`mail:delete` scope) can only reach the trash transition — no agent path to
hard deletion, ever. User-account hard delete is a system job walking the
same machinery.

## Consequences

- The purge path is the only way rows leave `messages`; admin has no DELETE
  grant on `users` so account removal flows through it too.
- Dead-lettered provenance moves surface to the user — an "archive" or
  "delete" is never silently dropped.

## References

spec §4, §13a · mailroom-data-model.md §6 · design review gap #1, D9
