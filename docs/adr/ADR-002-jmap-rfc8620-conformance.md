# ADR-002: True RFC 8620/8621 core semantics for the API subset

**Status:** Accepted (spec v0.4 §13; design review P0-3/D2)

## Context

No production-grade Go JMAP *server* library exists (foxcpp/go-jmap targets a
pre-RFC draft; rockorager/go-jmap is client-only), so the server is bespoke
either way. "Subset + extensions" without a conformance target drifts into
JMAP-flavored REST, and real JMAP clients depend on hard invariants (session
object, opaque per-type state strings with exactly-correct `/changes` deltas,
`cannotCalculateChanges` fallback, result references). Retrofitting correct
`/changes` is a schema migration, not a patch.

## Decision

Implement genuine RFC 8620 core semantics plus an RFC 8621 subset
(`Email/get|query|queryChanges|changes|set`, `Mailbox/get|changes|set`,
`Thread/get`, `EmailSubmission/get|set`, `Identity/get`, blob endpoints,
EventSource push). Mailroom features ship as capability-URN extensions
(`urn:mailroom:triage`, `urn:mailroom:labels`, …) that add methods and
properties but never change core method semantics. Labels are JMAP keywords;
folders are Mailboxes.

## Consequences

- The RFC state/changes machinery *is* the client sync protocol — built once.
- Third-party JMAP clients can genuinely connect; aerc (via rockorager
  go-jmap) serves as a free external conformance client — no CI workflow runs
  it today, this is a manual/local check.
- Full RFC 8620 (WebSocket push, quotas extension) is explicitly out for v1.0.

## References

spec §13 · mailroom-data-model.md §3 · design review P0-3, deep-dive E
