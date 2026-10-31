# ADR-003: Per-user, per-type monotonic change log

**Status:** Accepted (spec v0.4 §13/§14; design review D3)

## Context

Three consumers need an ordered stream of object changes: JMAP `/changes`,
the search indexer, and client sync cursors. Building them on separate
mechanisms guarantees drift; global modseq or logical decoding couple
consumers to storage internals.

## Decision

One `change_log` table with per-user, per-type monotonic sequences
(`change_seq` allocated via row-locked `UPDATE … RETURNING`). **Every
mutation of a JMAP-visible object appends a change_log row in the same
transaction as the mutation** — the load-bearing invariant, enforced by the
store layer plus tests. JMAP state strings are the sequence, rendered opaque.
A `change_floor` per (user, type) makes compaction safe: requests below the
floor get `cannotCalculateChanges` and refetch.

## Consequences

- One ordered stream, one cursor per consumer; indexer lag is measurable and
  surfaced ("search may be N minutes stale").
- Per-(user, type) hot counter row is the known cost — a non-issue at ≤ ~24
  users; revisit only if contention is measured.
- Weekly compaction deletes rows older than the retention window (default
  90 days) and advances the floor.

### Compaction (mailroom#526, implemented)

- The job is `internal/changelog`, job kind `change_log_compact` — a
  recurring per-user durable job, seeded at boot and self-rescheduling,
  modelled on `internal/drawer`/`internal/purge`. It fires weekly and drains
  its backlog in bounded batches (`store.DefaultChangeLogCompactBatch` rows
  per object type per transaction), each batch its own `UserTx`, so a large
  first-run backlog on an old account never sits behind one unbounded
  `DELETE`.
- Retention is `MAILROOM_CHANGE_LOG_RETENTION`, default 90 days
  (`config.DefaultChangeLogRetention`). **Config refuses anything below 7
  days** (`config.MinChangeLogRetention`): a shorter window would force
  every client, not just one genuinely offline a while, through
  `cannotCalculateChanges` on a recurring basis, so a typo is a boot-time
  config error rather than a standing correctness surprise.
- **The strict-prefix rule.** `change_log.created_at` is `DEFAULT now()` —
  the enclosing transaction's START time — which is not guaranteed to be
  monotonic with `seq`: two concurrent writers can commit out of
  `created_at` order relative to when each took its `change_seq` row-lock
  turn, so a row can be older by `seq` but younger by `created_at` (or the
  reverse — an inversion). Compaction therefore never deletes "rows with
  `created_at` older than the cutoff": it picks one boundary seq `D` per
  type from `created_at` (the seq just below the earliest row still inside
  the retention window, or the newest row that exists at all when nothing
  is), and then deletes every row with `seq <= D`, regardless of that
  individual row's own `created_at` — never a row above `D`, even one that
  is individually older than the cutoff. Deleting by `created_at` alone
  could delete a row above the boundary while leaving one below it, a hole
  in the middle of the contiguous history `Foo/changes`'s `seq > since`
  scan assumes.
- **Floor semantics: `floor_seq = D`, the highest DELETED seq — not `D+1`.**
  `Foo/changes` rejects `since < floor` and accepts `since == floor`: a
  client sitting exactly at the boundary already has every row past it, by
  construction, because `D` is by definition the last seq compaction
  deleted. (An earlier draft of this document, and of §3.3 below, said
  "+1"; both are corrected here — the off-by-one would have rejected an
  otherwise-satisfiable request for nothing.)
- **Compaction appends no `change_log` row.** It is not a mutation of any
  JMAP-visible object (no Email, Mailbox, or anything else a client can
  observe through `Foo/get`/`Foo/changes` changes state, gains a property,
  or stops existing) — it removes entries from the log mechanism itself,
  which §3.4 already places outside what the log records about itself. It
  also never touches `change_seq`: JMAP state strings are unaffected.
- `store.UserTx.Changes` — the method both `Foo/changes` and the search
  indexer (`internal/search.StoreSource`) call — reads the floor, the
  current state, and scans `change_log` in **one SQL statement**, not
  three. `UserTx` runs at the default READ COMMITTED
  (`pgx.BeginFunc`, no `TxOptions`), so each *statement* gets its own
  snapshot; a compaction commit landing between a separate floor-read and a
  separate log-scan would raise the floor and delete exactly the rows the
  (now stale) floor read still promised were available, returning an
  INCOMPLETE delta with NO ERROR. A single statement sees one consistent
  snapshot even under READ COMMITTED, which is the only reason splitting it
  back into multiple round trips would be unsafe — do not.
- Admin surface: `mailroom-admin compact-changelog --user-id [--retention]
  [--apply]`, report-first (the default run shows the TRUE eligible total
  per type, uncapped, and writes nothing).

## References

spec §13/§14 · mailroom-data-model.md §3 (exact `/changes` algorithm) ·
design review D3
