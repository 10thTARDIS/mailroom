# ADR-014: Contact re-sightings bump internal counters without a change_log row

**Status:** Accepted (deliberate narrowing of the load-bearing change_log
invariant for one field group; raised by internal-contacts work)

## Context

The load-bearing invariant (CLAUDE.md, spec §13/§14, data-model §3) is:

> Every mutation of a JMAP-visible object appends a `change_log` row in the same
> transaction as the mutation.

That single ordered stream drives JMAP `/changes`, the search indexer, and
client sync cursors.

Contacts are **auto-derived from mail flow**: every ingested message runs
`UpsertContactsFromMessage`, which for each correspondent either creates a
contact or re-sights an existing one. A re-sighting updates two bookkeeping
columns on `app.contact_addresses` — `seen_count` (incremented) and
`last_seen_at` (advanced). A busy correspondent is re-sighted on *every* message
they send you.

A strict reading of the invariant says each of those `seen_count` bumps is a
mutation of a JMAP-visible object and must append a `Contact` change row. For a
heavy mailbox that means a `Contact` change row per inbound message — a flood
that would inflate every client's `/changes` delta and the indexer's work for
data that no client renders in real time.

## Decision

**The synced state of a Contact is: its existence, its display name, its set of
addresses, and each address's `reached_me_at` alias.** A `Contact` change_log
row is appended (in the ingest transaction) only when one of those changes:

- contact **created**,
- an address **first added** to a contact,
- **display_name** filled or changed,
- **reached_me_at** changed.

`seen_count` and `last_seen_at` are **internal bookkeeping**, not part of the
synced Contact state. A pure re-sighting updates them and appends **no**
change_log row. At most one `Contact` row is emitted per affected contact per
message even when several of its addresses are touched.

`seen_count` still drives autocomplete ordering (most-corresponded-with first);
it is simply computed server-side and read on demand via `Contact/query`, never
streamed. Clients that want a fresh ordering re-query; they do not learn of it
through `/changes`.

## Consequences

- `/changes` and the indexer stay proportional to *meaningful* contact change,
  not to mail volume. This is the whole point.
- A client's cached `seen_count`/`last_seen_at` can be stale between explicit
  `Contact/query` calls. Acceptable: these are soft ranking signals, not facts a
  user acts on, and autocomplete always re-queries.
- The invariant remains intact for everything it protects — no *visible* Contact
  state changes without a change row, so no client can miss a new contact, a new
  address, a renamed contact, or a changed reply-from alias.
- If a future feature promotes `seen_count`/`last_seen_at` to client-visible,
  synced state (e.g. a "last contacted" column the user sorts on and expects to
  refresh live), this decision must be revisited — at that point those fields
  would need to emit change rows, or move to a separate low-frequency rollup.

## Alternatives considered

- **Emit a change row on every re-sighting (strict invariant).** Rejected:
  couples `/changes` and indexer load to raw mail volume for data no client
  renders live; the tail (a mailing list that mails you hourly) is pathological.
- **Coalesce re-sighting rows (one per contact per sync window).** More
  machinery (a debounce/rollup) than the value of a live `seen_count` justifies;
  can be added later under the "future feature" clause above if needed.

## Amendment (2026-10-17): four more contact columns, all synced

v1.89.0 added four columns to the contact (`internal/store/contact_flags.go`):

- `vip` (boolean): the sender is a VIP. Drives `is:vip`, the VIP push modes and
  the quiet-hours exception.
- `blocked` (boolean): new mail from the sender's addresses is placed in Junk
  or Trash on arrival.
- `block_destination` (`junk` or `trash`): where blocked mail goes.
- `blocked_at` (timestamptz, nullable): when the block was set; null when not
  blocked.

Unlike `seen_count` and `last_seen_at`, these are **user-set and client-visible**
(served as `isVip`, `isBlocked`, `blockDestination`, `blockedAt`). They are
therefore *inside* the change_log invariant, not an exception to it: a change
to any of them appends exactly one `Contact` change row in the same
transaction, and a no-op appends none. The narrowing this ADR records still
covers only `seen_count` and `last_seen_at`.
