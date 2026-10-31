# ADR-001: Message identity = per-user SHA-256 content hash

**Status:** Accepted (spec v0.4 §4; design review P0-1/D1)

## Context

Dedup by `Message-ID` is wrong: the header is sender-controlled, optional,
reusable, and spoofable. Two different messages sharing a Message-ID
(mailing-list copy with rewritten footer vs. direct copy; a malicious reuse)
would silently merge. The canonical `messages` row is the FK hub of the whole
schema — changing identity after real data exists means rewriting every table
and client cache. This is the most expensive thing to get wrong.

## Decision

A Message is identified per user by `SHA-256(raw RFC 5322 bytes)`, unique key
`(user_id, content_hash)`. Raw bytes are hashed exactly as received — no
canonicalization in v1 (a transport that mutates bytes creates a second
message, which is correct). `Message-ID` is stored verbatim as a *grouping
hint* for threading and Gmail label-copy collapse, never identity. Identical
bytes in multiple folders/accounts collapse to one canonical message with a
provenance list. Dedup is strictly per-user — never across users.

## Consequences

- The four worked examples in mailroom-data-model.md §2 (Gmail label copies,
  list + direct copy, Message-ID spoofing, cross-account duplicate) are the
  acceptance tests for the ingest path.
- An attacker reusing a Message-ID can thread near a message but can never
  merge with, overwrite, or write back to it.
- Content-addressed blobs give free attachment/message dedup within a user.

## References

spec §4, §14 · mailroom-data-model.md §2 · design review P0-1
