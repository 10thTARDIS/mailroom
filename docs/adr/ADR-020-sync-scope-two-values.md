# ADR-020: `sync_scope` is two values, not three — `headers` cannot exist under ADR-001

**Status:** Accepted (narrows the baseline schema's advertised enum; raised by
Forgejo #62, selective sync)

## Context

`app.mailboxes.sync_scope` has been in the baseline schema since the beginning,
carrying a comment that advertised three values:

```sql
sync_scope text NOT NULL DEFAULT 'full',   -- 'full' | 'headers' | 'off'
```

Two of the three were real. `internal/mailsync/engine.go` compares against exactly
one value:

```go
if stored.SyncScope == syncScopeOff || !selectable(mb) { … skip … }
```

Everything else falls through to a full sync. There was no `CHECK` constraint, and
until #62 there was no way to write the column at all except direct SQL — so the
only person who could ever have set `'headers'` was an operator reading that
comment and trusting it.

**`'headers'` therefore meant its own opposite.** A folder set to it did not sync
headers only; it synced completely.

## Why it is not being implemented instead

That is the other way to close the gap, and it is much larger than an enum value.

Message identity is `SHA-256(raw RFC 5322 bytes)` (**ADR-001**). `content_hash` is
the blob address and the FK hub of the schema: `app.messages` is keyed by it,
`message_provenance` references it, dedup and re-delivery detection are defined in
terms of it (**ADR-018**), and the blob store is addressed by it.

**A headers-only message has no raw bytes.** It cannot be an `app.messages` row.
Implementing `'headers'` means inventing a parallel "stub" concept and answering,
at minimum:

- what identifies a stub, given there is no full byte string to hash;
- what happens when the stub is later fetched in full — is it a new message, or an
  in-place promotion, and what does the change log say;
- what every consumer of `content_hash` does when it is absent (search indexing,
  the dedup hash, provenance, thread membership, blob resolution);
- whether a stub is JMAP-visible at all, and if so what `Email/get` returns for
  `bodyValues`, `size` and `blobId`;
- how the reaper reasons about a mailbox holding a mix of both.

That is an architecture change with its own ADR, not a setting. It also has a much
weaker motivation than it first appears: the point of selective sync in #62 is *do
not touch this folder*, and `'off'` delivers that completely. `'headers'` is an
optimisation for a folder you *do* want to see — a different feature, wanting a
different design (probably lazy body fetch on open, which needs no new identity).

## Decision

The set is closed to **`'full'`** and **`'off'`**, enforced in three places so it
cannot drift apart again:

1. **The database** — `mailboxes_sync_scope_known CHECK (sync_scope IN ('full','off'))`
   (migration 00026). A value the engine does not implement must not be storable.
2. **The store** — `SyncScopeFull` / `SyncScopeOff` constants and
   `ErrUnknownSyncScope`, checked before any write is attempted.
3. **The API** — `Mailbox/set` refuses anything else with `invalidProperties`
   naming `syncScope`, rather than accepting a value that would behave as its
   opposite.

Migration 00026 rewrites any surviving non-conforming value to `'full'`. That is
**behaviour-preserving**, which is the only reason it is safe to do without asking:
such a row is being fully synced right now, and `'full'` is the value that says so.

## Consequences

- The advertised setting and the implemented behaviour are the same thing again.
- `'off'` is the whole of selective sync, and it is complete: the mailbox is skipped
  before `SELECT`, so it is never polled, never gap-backfilled, and — the property
  the feature rests on — **never a reap candidate**. Its mail is frozen, not
  deleted. "We are not looking" cannot become "the mailbox is empty".
- If header-only sync is ever wanted, it arrives deliberately, with an ADR that
  answers the identity questions above. Re-adding the enum value alone would
  recreate exactly the defect this ADR closes.
- The `Down` migration drops the constraint but does not restore `'headers'` to any
  row: there is nothing to restore to, because no code ever produced that state.

## Verification

`internal/mailsync/syncscope_test.go` pins the behaviour of `'off'` — including,
separately from "not polled", that it is **not reaped** and not gap-backfilled, and
that turning it back on catches up on new mail, on expunges, and across a
UIDVALIDITY roll that happened while nobody was looking. A probe confirmed the
reap-exclusion assertion is load-bearing: simulating the plausible refactor (skip
the poll, add the mailbox to the reap targets anyway) makes that file report
`ProvenanceReaped: 2, MessagesVanished: 1` on a folder the user had turned off.

`internal/api/syncscope_integration_test.go` pins the refusals, including every
malformed `syncScope` value and a patch that smuggles a rename alongside a legal
scope change.
