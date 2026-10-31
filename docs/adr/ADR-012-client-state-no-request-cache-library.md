# ADR-012: Client state lives in the TS client-core; no request-cache library (TanStack Query)

**Status:** Accepted (deviates from spec §3's client-library sketch; raised while building §12's web UI)

## Context

Spec §3 lists TanStack Query among the client libraries. Building the daily-driver
web UI made that a concrete choice, and it turned out to conflict with two
harder constraints that the spec states elsewhere.

**JMAP is already a cache-coherence protocol.** Spec §13 commits us to "opaque
per-type **state strings with exactly-correct `/changes` deltas**", backed by the
per-user per-type change log (§14). That is not a transport detail we wrap — it
*is* the cache invalidation algorithm, and it is authoritative in a way no
client-side heuristic can be. A request cache keyed on query arguments, with its
own staleness/refetch policy, would sit on top of it and duplicate it: two
invalidation systems, one of which is guessing.

**The offline/optimistic layer cannot live in the React app.** Spec §12 says the
`mailroom` monorepo ships a "shared **TS client-core**" that is framework-agnostic
and DOM-free, and that the v2 native apps consume it *unmodified*. Optimistic
apply, the durable action queue, and the offline cache are exactly the parts a
native shell most needs to reuse. Putting them in a React-bound request cache in
`src/app` would strand them: the native client would have to reimplement the
hardest, most correctness-sensitive code in the product.

There is also a specific correctness trap that a generic request cache gets wrong
by default. An `Email` object is a *projection of the requested properties* — the
cheap list/poll `Email/get` deliberately does not fetch bodies. A
last-write-wins object cache would let a cheap poll blank the body of the message
currently open. And until canonical placement and expunge reconciliation were fixed, a naive overwrite-on-refetch made
archived mail visibly bounce back into the inbox.

## Decision

**No request-cache library.** Client state is one normalized store in
`web/src/core` (DOM-free, framework-agnostic), bound to React via
`useSyncExternalStore`. The core owns:

- **Cache coherence** driven by JMAP state strings + `Foo/changes` (and the
  EventSource push stream), not by a time/staleness heuristic.
- **Optimistic apply** as an *overlay* on server data — never an overwrite — plus
  a durable action queue that reconciles or rolls back per the server's own typed
  error.
- **Projection-aware merges**: a partial `Email` (list properties only) merges
  into the cached object rather than replacing it.

React stays a rendering layer. Spec §3's mention of TanStack Query is superseded
for this project.

## Consequences

- We own our cache. There is no library to lean on for retry/backoff/dedup
  semantics, and the store is code we must test — that cost is accepted, because
  the alternative is a second, weaker invalidation system layered over the
  authoritative one.
- The v2 native clients (spec §12) can import `core/` and inherit offline,
  optimistic apply, and sync for free. That is the main payoff.
- The optimistic overlay is currently doing double duty: part of it exists to hide
  server bugs (canonical placement, expunge reconciliation) rather than to
  express genuine offline intent. **Once both fixes have landed and settled,
  the overlay's "stickiness" should be revisited and simplified** — otherwise a
  future reader will mistake a workaround for architecture.
- If a future client needs plain request caching for a non-JMAP surface (an admin
  API, say), this ADR does not forbid it; it governs the mail data path.

## References

- Spec §3 (libraries), §12 (clients & design system, shared TS client-core), §13
  (API: state strings + exact `/changes`), §14 (change log).
