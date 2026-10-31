# ADR-019: A subaddress is authorized by its base, syntactically — never by a header

**Status:** Accepted (extends spec §5's send-as rule; raised by
dogfooding)

## Context

Spec §5 states the send-as rule without qualification:

> the server refuses any From address not matching a verified identity/domain for
> the authenticated user — even though the provider would also reject it. Defense
> in depth for multi-user: one user's misconfiguration must never burn another
> domain's reputation from a shared instance.

`store.ResolveSendIdentity` is that check. It matched two ways: an exact `fixed`
address, or a `domain_wildcard` covering the domain.

Subaddressing (RFC 5233 — `you+shopping@example.com` delivered to `you@example.com`)
falls in the gap. A user on a **catchall** is unaffected: their wildcard matches the
whole domain, so every subaddress is already sendable, and the local part is never
inspected. A user whose identities are all `fixed` — an ordinary Gmail or Migadu
mailbox — cannot send as their own subaddress at all, and mail delivered to one
resolves to no identity, so a reply does not default to the address the sender
actually used.

That second case is the whole point of subaddressing: per-site addresses you can
filter on and reply from.

## The design that was rejected, and why it matters

The attractive option was **evidence-based**: authorize `you+shop@d` once a message
has *arrived* addressed to it. It needs no per-provider configuration and cannot be
wrong about whether a given provider implements the convention, because the provider
demonstrated it by delivering.

**The evidence is forgeable.** Mailroom is an IMAP client, not the MDA: it never
sees the real `RCPT TO`, only the headers in the bytes it fetched. `internal/alias`
is built entirely around that fact and says so:

> Every candidate is a CANDIDATE ONLY. The caller (`store.MatchVerifiedAddress`)
> takes the first one that resolves to a verified identity of THIS user; an address
> we cannot send as is never chosen, no matter how high it ranks. That keeps the
> derivation (a heuristic over attacker-supplied headers) strictly separated from
> the authorization (a database fact) — a hostile sender can put anything in
> `Delivered-To` and still only ever move us between addresses we were already
> allowed to send as.

`Identity/set` holds the same line from the other side: it refuses to create an
identity for any address `ResolveSendIdentity` does not already cover, and only the
operator can provision a wildcard. Nothing in the codebase lets message content
widen a send right.

Wiring arrival to authorization would have been the first thing that did. A sender
could put `Delivered-To: you+security-alert@yourdomain` in a message and mint an
entry in the user's own From picker. The blast radius is bounded — always the user's
own domain, never someone else's — but the invariant is not the kind worth trading
for a convenience, and once crossed it is hard to uncross.

The general shape of the mistake is worth naming: **"the provider proved it" is only
as good as our ability to observe the proof.** We could not.

## Decision

Authorize `me+tag@d` **iff a verified, enabled `fixed` identity exists for `me@d`**.

- **Syntactic, not evidential.** The rule is a database fact (the base identity)
  plus RFC 5233's delimiter. It reads no message data, so no header can influence
  it.
- **Last, after the existing matches.** The fallback runs only when the main query
  found nothing, so an exact `fixed` identity and a `domain_wildcard` both still
  win. A catchall deployment resolves bit-for-bit as it did before — including
  *which* identity, and so which display name and signature, a subaddressed send
  uses.
- **`FromAddress` is the subaddress, not the base.** It is what goes in the From
  line, and what ingest records as the message's `addressed_address` — so
  `deliveredTo` and the `alias` rule field keep naming the exact address a sender
  used. Returning the base would silently collapse every subaddress into one.
- **`SendIdentity.ViaSubaddress`** distinguishes this from an exact match, because
  `Kind` describes the *base* identity and says nothing about `FromAddress`.
  `Identity/set` consults it so naming a subaddress is a create rather than a
  spurious "already exists" — and that create still grants nothing new, which is the
  standard that check exists to hold.
- **Normalization is unchanged.** `NormalizeAddress` deliberately keeps `+tag`,
  because under a catchall those are meaningful distinct addresses. Stripping
  belongs only to the authorization fallback, where the base is a fact being looked
  up rather than an identity being compared.

The inbound half needed no work: `addressed_address` already records the exact
delivered address and is already exposed as the JMAP `Email.deliveredTo` property
and matchable by the `alias` rule field. Authorizing the subaddress is what causes
`addressed_identity_id` to be set, which is what makes a reply default to it.

## Consequences

- A fixed-identity user can send as, and reply from, their own subaddresses, and
  rules can file by the address a sender used.
- **The one failure mode is a provider that does not implement subaddressing.**
  Sending from `me+tag@d` there produces a bounce. That is visible, recoverable, and
  not a security boundary — which is the trade this ADR accepts in exchange for
  consulting no untrusted input. A per-provider-profile gate remains available later
  if bounces are ever observed in practice; it would narrow this rule, not widen it.
- Owning `me@d` still implies nothing about `boss@d`, and a *disabled* identity
  authorizes nothing, including its subaddresses — revoking an address revokes
  everything it covered.
- The ordering constraint is load-bearing and easy to undo by accident. Folding the
  fallback into the main query, or running it first, changes which identity a
  catchall user sends every subaddressed message from. That is pinned by
  `TestSubaddressDoesNotChangeCatchallResolution`, which was verified by probe to be
  the *only* test that fails when the fallback is moved ahead of the wildcard.

## Verification

`internal/store/subaddress_test.go` (pure, no database) covers the parse, including
the forms that must not produce a lookup: `+tag@d`, `me+@d`, a `+` in the domain.
`internal/store/subaddress_integration_test.go` covers authorization against the real
RLS-enforcing database — the positive, the unverified base, the disabled base, the
cross-user case, the catchall non-regression, and a `MatchVerifiedAddress` fed forged
`Delivered-To` candidates, with a control proving that test is not vacuous.
