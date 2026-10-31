# ADR-018: A second hash catches re-delivered mail; identity stays the content hash

**Status:** Accepted (amends ADR-001's "no canonicalization in v1"; raised by
Forgejo #206, dogfooding)

## Context

ADR-001 identifies a message per user by `SHA-256(raw RFC 5322 bytes)` and closes
with an explicit consequence:

> Raw bytes are hashed exactly as received — no canonicalization in v1 (a
> transport that mutates bytes creates a second message, **which is correct**).

That reasoning is sound for *identity*: `content_hash` is the blob address, the FK
hub of the schema, and ADR-001 rightly calls changing it the most expensive thing
to get wrong.

But it has a consequence nobody had met in practice until it was run against a
real mailbox: **the case dedup most obviously exists for is the one it cannot
see.** When a mail path delivers the same message more than once, each delivery is
stamped with its own trace metadata, so the bytes differ, so it becomes another
message. The inbox showed some mail three and four times.

### What was measured

Guessing the cause would have cost a release, because the fix depended entirely on
*where* the copies differ. `mailroom-admin diff-duplicates` (report-only; no
`-apply`, no write path) decrypted every duplicate group in a real mailbox and
diffed it:

```
=== N duplicate Message-ID groups, M copies ===
  identical once trace headers are stripped : most
  headers differ beyond the trace set       : a few
  bodies genuinely differ                   : 0
  unreadable                                : 0
```

**The bodies were byte-identical in every group.** `Received:` differed in nearly all.
The rest also differed in `X-Gm-Message-State`, `X-Google-Dkim-Signature`,
`X-Forwarded-Encrypted`, `X-Migadu-Queue-Id`, `X-Migadu-Spam-Score`,
`X-Migadu-Flow`, `X-Migadu-Reason`, `X-Report-Abuse` and a re-signed
`Dkim-Signature` — per-delivery stamping from one forwarder and one provider.

Scale: a few percent of messages were redundant copies, spread
across several different aliases (so not an artefact of one forwarded address).

### The options

1. **Change `content_hash` to a canonical form.** Rejected: it is the blob address,
   so the stored bytes would no longer hash to their own key, and it is the FK hub
   ADR-001 warns about.
2. **Dedup on `Message-ID`.** Rejected for exactly ADR-001's reason — the header is
   sender-controlled, optional and reusable, and a mailing-list copy with a
   rewritten footer shares it with the direct copy.
3. **A second hash over the bytes MINUS a denylist of trace headers.** Would have
   worked on the measured data. Rejected because two providers had already
   contributed nine fields between them *from a single mailbox*: a denylist works
   today and rots, silently, on the next provider.
4. **A second hash over an ALLOWLIST of identity-bearing headers plus the body.**

## Decision

**Option 4.** `app.messages.dedup_hash` (migration 00023), computed by
`ingest.DedupHash`:

```
dedup_hash = SHA-256( Message-ID, Date, From, To, Cc, Subject ‖ body )
```

**`content_hash` is unchanged and remains identity.** `dedup_hash` is a second,
additive column with a different job, and it must never be used as identity: two
rows *can* legitimately share it — that is the condition it exists to find —
whereas two rows sharing `content_hash` are the same bytes.

Ingest asks two questions, in order of certainty (`ingest.findExisting`):

1. identical **bytes** (`content_hash`) — certain, and the check that always existed;
2. identical **message, re-delivered** (`dedup_hash`) — a strong inference.

A hit on either returns the existing message and the delivery is recorded as
another **provenance row**. Nothing is discarded: the second delivery is still
known to have happened, at its own `(mailbox, uidvalidity, uid)`, so writeback
still targets every coordinate.

Three implementation details are load-bearing rather than incidental:

- **Field names are hashed alongside values**, so a value cannot migrate between
  adjacent empty fields (`Subject: X` with no `Cc` must not collide with `Cc: X`
  with no `Subject`).
- **The field order is fixed.** Iterating a map would make the hash depend on Go's
  map iteration order — the worst possible failure mode for a dedup key, because it
  would work intermittently.
- **Header folding and CRLF/LF are normalised**, because refolding and line-ending
  rewrites are precisely the "transport mutates bytes" behaviour at issue.

## Consequences

- **ADR-001's stated hazard does not return**, and that is why the body is in the
  hash. A mailing-list copy with a rewritten footer has a different body ⇒ a
  different `dedup_hash` ⇒ it stays a separate message. Same for malicious
  Message-ID reuse: to collide, an attacker must reproduce the body, `Date`,
  `From`, `To`, `Cc` *and* `Subject`, at which point it **is** the same message and
  treating it as one is correct rather than dangerous. Asserted in
  `internal/ingest/dedup_test.go` and again end-to-end in
  `redelivery_integration_test.go`.
- **`To`/`Cc` are in the allowlist deliberately**, as the conservative choice: one
  message genuinely sent to two of a catchall's aliases stays two rows. Where the
  two error directions conflict, this picks separate — **under-merging is
  recoverable by a person; over-merging silently loses mail.**
- **The column is nullable and NOT unique.** Nullable per ADR-008's
  expand-and-contract: existing rows are backfilled online by `mailroom-admin
  backfill-dedup-hash`, which must decrypt each blob and so cannot be a SQL
  `UPDATE`. NULL means "does not participate", i.e. the pre-00023 behaviour. Not
  unique because rows sharing a hash can already exist in a real database *right
  now*, so the constraint could not be created; once those are collapsed (#206
  part 3, deliberately separate because it moves provenance and deletes message
  rows) a follow-up migration can add it and make the guarantee structural.
- **Until the backfill runs, the fix only protects mail that arrives after it.**
  This is stated in the command's own output rather than left to be discovered.
- **Setting the hash appends no `change_log` row.** `dedup_hash` is on no JMAP
  object, in no `properties` set, and readable by no method, so there is nothing
  for a client to resync and a change row would make every client refetch a message
  byte-for-byte identical to what it holds. Same reasoning as ADR-014's contact
  re-sighting counters.
- **If `DedupHash` ever changes, every stored hash must be recomputed** or old and
  new rows stop matching each other. `backfill-dedup-hash -recompute` exists for
  that, and it is the reason the flag exists at all rather than being added later
  under pressure.
- The fix was **validated against real bytes, not only unit tests**:
  `diff-duplicates` reports whether `DedupHash` agrees across each group's actual
  blobs, and on a real mailbox it collapsed **every group, zero disagreements**.
  Tests written to agree with a design prove nothing about the data it will meet.

## References

- ADR-001 (message identity), ADR-008 (forward-only migrations), ADR-014 (a
  mutation with no change row)
- spec §4, §14 · `docs/mailroom-data-model.md` §2 (its four worked dedup examples)
- `db/migrations/00023_dedup_hash.sql`, `internal/ingest/dedup.go`,
  `cmd/mailroom-admin/diffduplicates.go`, `cmd/mailroom-admin/backfilldedup.go`
- Forgejo #206
