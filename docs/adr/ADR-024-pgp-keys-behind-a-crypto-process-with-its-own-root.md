# ADR-024: PGP private keys live behind a crypto process with its OWN root secret; the at-rest hierarchy is untouched

**Status:** Accepted (Forgejo #47; scopes spec §9's crypto process to PGP only,
and leaves [ADR-013](ADR-013-key-derivation-hkdf-not-wrapped-kek.md)'s deferral
of the wrapped-KEK hierarchy standing)

> **⚠ §4 amended 2026-08-10 (Forgejo #275): its original wording described a
> control the process does not have.** It claimed the caller's identity was
> *authenticated* and that the identity was "the caller's, not the request's".
> Neither is true — `user_id` is a field on the wire request, asserted by the
> application. The decision itself is unchanged and still Accepted; §4 now states
> the control that is actually in force, and the threat model it does and does
> not cover. The correction is recorded rather than made silently because a
> document asserting a control it does not have is the exact failure this project
> paid for three times over in #48 → #257 → #266.
>
> **Amended 2026-09-11 (Forgejo #525):** the process now verifies its root at boot
> against a check value the database recorded, and refuses to listen on a
> mismatch. See Consequences.

## Context

Spec §9 requires a *"separate, minimal crypto process whose entire API is
`sign` / `decrypt` / `generate` / `import` / `export-wrapped`. No API returns raw
private key material. It is the sole holder of unwrapped user KEKs; the main app
never sees them."*

**There are no KEKs.** `crypto.UnwrapKEK` has no caller outside its own package.
Every per-user key in the 0.5/1.0 line is *derived*, not wrapped:

```
MasterKey() ──HKDF(salt=userID, info=purpose)──▶ per-purpose DEK ──seals──▶ data
```

That is not drift — ADR-013 records it as an accepted deviation and defers the
wrapped hierarchy to "1.x hardening", which is the milestone #47 sits in. So
#47 arrived carrying two coupled pieces: OpenPGP itself, and a key-hierarchy
refactor the spec's own wording presumes.

They are separable, and the reason to separate them is **blast radius**.

- A **PGP private key is an identity**. Its exposure is permanent and
  unrecoverable: you cannot rotate away a signature a correspondent already
  trusted, and mail encrypted to that key stays readable by whoever took it,
  forever.
- An **at-rest DEK protects data** whose compromise is bounded, and is already
  mitigated by RLS, envelope encryption, and the offline master-key escrow.

Different consequences deserve different urgency.

## Decision

### 1. The crypto process holds PGP private keys ONLY

`internal/crypto`'s at-rest paths (blob DEKs, credential keys, model keys, blob
path HMACs) are **unchanged** and stay in the main app. ADR-013's deferral of the
wrapped `master → KEK → DEK` hierarchy remains open on its own terms, as its own
hardening item, and is **not** a prerequisite for PGP.

This narrows spec §9's stated scope. It is recorded here rather than silently
implemented, because a later reader comparing the code to §9 would otherwise
find the crypto process holding less than the spec says and reasonably conclude
it was unfinished.

### 2. It has its OWN root secret, not the app's master key

This is the decision the whole ADR exists for.

If PGP keys were sealed under `HKDF(master, userID, "pgp-v1")`, the main app —
which already holds the master key — could re-derive them at will, and the
"boundary" would be process isolation alone. An attacker with app RCE would
still walk away with the identity keys.

So the crypto process is given an **independent root secret**, delivered as its
own file-based secret and never readable by the main app:

```
pgp root ──HKDF(salt=userID, info="mailroom-pgp-v1")──▶ per-user PGP key-wrapping key
                                                          └── seals stored private keys
```

Spec §9 states both a strong custody claim (*"the main app never sees them"*) and
a weaker threat framing (*"raises the bar from SQL injection to process
compromise"*). This satisfies the strong one, which is the only one worth having
for material that cannot be un-leaked.

### 3. The API is PGP-shaped, because S/MIME is deferred

Step 0 measured a real mailbox: **zero S/MIME messages, zero S/MIME
correspondents** (#47). Spec §18 item 6 makes the S/MIME
go/no-go a crypto-release-time decision; it is hereby **deferred to 2.x**
(Forgejo #69).

Consequently the process API names OpenPGP concepts directly rather than carrying
an algorithm-neutral abstraction that has exactly one implementation. Reversing
this later means an interface change — a known, stated cost rather than a
surprise.

### 4. No API returns private key material, and a key belonging to another user fails closed

The API surface is `generate`, `import`, `export-public`, `sign`, `decrypt`.
There is no `export-private`, and `export-wrapped` from spec §9's list is **not**
implemented until something needs it — an export path that exists "for
completeness" is an exfiltration route that nothing tests.

⚠ **Correction, 2026-09-18: `decrypt` is not yet in the served method registry.**
`internal/pgpwire/wire.go`'s `Methods` lists `generate`, `export-public`,
`import`, `bind-identity`, `sign`, `export-wrapped` — no `decrypt`. It is
planned (milestone "v1.71.0 — PGP encrypt/decrypt") but not built; the line
above describes the intended surface, not the current one.

> **AMENDED 2026-10-20 (#47 increment 6b): `decrypt` is now served.** It is in
> `pgpwire.Methods` with a succeeding leak probe. Its bounds, opaque error, SED
> refusal, rate limit, crypto-process-written audit trail and the rule that
> retired keys still decrypt are recorded in
> [ADR-037](ADR-037-decrypt-on-open-plaintext-never-persisted.md).

#### ⚠ AMENDED 2026-08-28: that condition has now been met, and the gate is `--apply` (#54)

The clause above is a deferral with a trigger, not a refusal, and **the trigger
has arrived**. Spec §9 lists `export-wrapped` in the process API; this ADR
declined to *build* it early. #54 (per-user export and hard delete) is the
"something" it was waiting for.

The reasoning, and why it binds on delete rather than on export:

- **Export without key material is a partial service.** The user keeps every
  byte; they merely cannot read the PGP-encrypted subset. They can export again
  later. Recoverable.
- **Hard delete without key material is destruction.** For a key this process
  *generated*, the private half exists nowhere else. Deleting it is irreversible
  and **retroactive**: mail the user exported months earlier becomes permanently
  unreadable, because an export carries ciphertext and not the key.

So `export-wrapped` is a **prerequisite for `--apply`**, not for export.

⚠ **"Wrapped" must mean wrapped under a passphrase the USER chooses.** A private
key wrapped under this process's own root satisfies the letter of spec §9 and
delivers zero portability — an exported file nothing outside this instance can
open. That would be the worst outcome available: the exfiltration surface built,
and the promise still unmet.

**Migration 00039 is the first half, and it landed first because it only works
forward.** `app.user_keys.origin` records `generated` / `imported`, because the
two cases have opposite consequences and were previously indistinguishable —
nothing in the schema, and nothing in `app.audit_log` (empty on a small instance), could tell them apart after the fact. Rows predating 00039 are NULL,
meaning NOT RECORDED, and **NULL must never be read as `imported`**: unknown is
the cautious case, not the safe one. `--apply` must refuse an unrecorded key
rather than assume a delete is harmless.

Measured when this was decided (`pgp-report`): the instance held **one PGP key** and
**zero PGP-encrypted messages** (a few were *signed*,
which is unaffected — signed mail is plaintext). So the population harmed today
is empty, and the deferral remains sound. What is *not* deferrable is recording
provenance, since every key created without it is permanently unclassifiable.

A request for a key belonging to a different user **fails closed**: the service
compares the stored record's owner against the requesting `user_id` and refuses.
This is spec §9's confused-deputy requirement, and it is the property most likely
to be quietly broken by a later refactor, so it is a test
(`internal/pgpkeyd/boundary_test.go`) rather than a convention.

#### ⚠ The caller's identity is ASSERTED, not authenticated — and the socket is the boundary

`user_id` is a plain field on the wire request. The crypto process performs **no
authentication of the caller**: there is no shared token and no `SO_PEERCRED`
peer check. So the fail-closed guarantee above holds *with respect to the
identity the caller claims* — a caller that can reach the socket can name any
user.

What actually constrains who may call is the socket itself, and
`internal/pgpkeyd/server.go` says so directly: it is created `0600` and owned by
the crypto process's uid, and `deploy/compose.yml` runs both containers as the
same uid, "which is what makes the 0600 socket reachable at all — the access
control is the socket."

**This is accepted, and it is what the boundary was for.** The threat this
decision addresses is stated in the Context above: a compromise that yields the
master key, or SQL injection, must not yield PGP private keys. It does not, and
cannot, defend against a fully compromised application process — that process is
the only thing on the socket, and it is also the only thing that knows which user
a request is for, because it is where the OIDC session is resolved. Requiring the
crypto process to independently authenticate a user would mean giving it a second
copy of the session layer, which is a larger attack surface than the one it
removes.

⚠ Two things follow, and they are the reason this is spelled out rather than left
implicit:

- **Do not add an API here on the assumption that the caller cannot lie about
  identity.** It can. Any new call must be safe under "the caller names the user".
- **Do not loosen the socket's mode or its uid arrangement** believing the user
  context is the real boundary and the file mode is belt-and-braces. It is the
  other way round.

`SO_PEERCRED` would let the process check the peer's uid itself rather than
relying on the mode alone. That is defence in depth and is not planned here; it
would not change anything above, because the application would still be the party
naming the user.

> **Re-evaluated 2026-10-20 at decrypt (#47 checklist item 8), and declined
> again.** A plaintext-returning verb was the moment to reconsider. Both
> containers still run the same image as the same uid, and only the application
> and the crypto process mount the socket volume. Separate uids plus a peer-uid
> check would authenticate the application *process*, which the 6b threat (a
> compromised application) passes by construction; it would defend only against
> a third process on the socket volume, of which there is none. The controls
> that bound a decrypt oracle are the rate limit, the append-only audit trail
> the crypto process writes itself, the opaque error and the caps
> ([ADR-037](ADR-037-decrypt-on-open-plaintext-never-persisted.md) §6). The
> model above stays true as written: the socket is the boundary, and the caller
> names the user.

## Consequences

### ⚠ The restore unit gains a second escrowed secret, and losing it is unrecoverable

`docs/key-escrow.md` currently states the restore unit as **Postgres + the blob
store + an independent master-key escrow**, and explains that because keys are
derived rather than stored, the master key is *"the single root of everything"*.

After this decision there are **two** roots:

| secret | what its loss costs |
|---|---|
| master key | the entire mailbox — blobs become unreadable *and unaddressable* (it seeds blob path HMACs) |
| **PGP root** | **every message ever encrypted to those keys, permanently** |

These consequences are not symmetric and must not be documented as one bullet.
Losing the master key loses everything; losing the PGP root loses a smaller set,
**irreversibly and silently** — the mailbox restores, opens, and works, and only
the encrypted mail is gone.

**`docs/key-escrow.md`, the backup documentation, and the restore drill (#40)
change in the same increment as the process itself**, not afterwards. A restore
drill that green-lights a backup which cannot decrypt encrypted mail is a drill
that certifies a hypothesis — precisely what #40 exists to prevent.

### ⚠ AMENDED 2026-09-11: the process verifies its root at boot, against a value the database recorded (#525)

The section above says a lost PGP root fails **silently**. Until #525 the same
was true of a *wrong* one at runtime: stored keys are unsealed lazily, so a
process started with the wrong root booted, reported healthy, and served until
the first signature or decryption, possibly months later.

**Decision.** At its first boot against a database the crypto process records a
check value, `HKDF(pgp root, info="mailroom-pgp-root-check-v1")`, in
`app.pgp_root_check` (migration 00055). At every later boot it re-derives the
value and, on a mismatch, **exits before it listens**. So the healthcheck, which
dials the socket, fails, and no request can reach a process holding the wrong
root. In particular `generate` cannot seal a new key under it, which would put
two roots into one database and make the eventual fix lose data.

**Why not "unseal one stored key", which #525 asked for.** At boot there is no
caller, and `app.user_keys` is readable only under its owner's identity — the
vault's fail-closed design, not a gap in it. Reading "some key" would take a new
cross-user `SECURITY DEFINER` returning *sealed key material*, and it still could
not see a wrong root on a database with no keys yet, which is the likeliest shape
of a bad restore. The check value needs neither.

What it does and does not claim:

- **It proves "this is the root this database was built with", not "this root
  opens the stored keys".** For a restore the two coincide. The restore drill's
  step 5, which unseals every stored key with the escrowed root, is still what
  proves the second.
- **A database that predates 00055 has no record**, so its first boot records
  whatever root it is given — trust on first use. The process cannot check that
  root against keys already stored, because RLS hides them from it. An upgrade
  that is *also* a restore with a different root, in the same step, is not
  caught; any other ordering is.
- **The value is not a secret.** It is a one-way function of a 256-bit random
  root under a label no other derivation uses, and it travels in the backup —
  which is the point: a restored database paired with a different root is exactly
  the combination that mismatches.
- **The application shares `mailroom_app` with this process**, so the grant is
  the control: `SELECT` and `INSERT` only, admitted only in an identity-less
  transaction. The application can neither overwrite the value (to wedge the
  crypto process) nor delete it (so the next boot adopts any root). **Resetting
  it is a superuser act**; `docs/backup-restore-runbook.md` step 5 says when that
  is right and what it costs.

This leaves §4's threat model where it was: it adds no API and no cross-user
surface, and the socket is still the boundary.

### Other consequences

- **A second long-running process** in the deployment: a compose service, its own
  secret mount, its own healthcheck, and a socket shared with the app. The
  operator's stack gains a service; `deploy/compose.yml` and its README change.
- **The main app cannot fall back.** If the crypto process is down, signing and
  decryption fail — they do not degrade to doing it in-process, because a
  fallback that reads the key in the app is the boundary deleting itself under
  load. Mail delivery and reading are unaffected; only crypto operations fail.
- **Nothing is encrypted at rest twice.** Stored PGP private keys are sealed
  under the PGP root; the blobs holding *messages* remain sealed under the master
  key as today. The two hierarchies do not interact.
- **Spec §9 line 148 still applies and is not softened by any of this.** Because
  keys are server-side, decrypted bodies are indexed normally — the search index
  contains the plaintext of encrypted mail. Tantivy has no native encryption, so
  index and model directories require volume-level encryption (LUKS/dm-crypt) as
  a **stated deployment requirement**. That lands with the decryption increment.

  > **SUPERSEDED for decryption by ADR-037.** Decrypted bodies are
  > NOT indexed: they are decrypted on open, on the request path, and never
  > persisted anywhere, so the index never holds the plaintext of encrypted mail
  > and this LUKS-on-index requirement does not land with decryption. The cost
  > is that encrypted mail is not searchable. Volume encryption of the index and
  > model directories remains good practice for the ordinary plaintext mail they
  > hold; it is no longer a gate on this feature.
- **The honest framing does not change**: Mailroom is provider-blind with strong
  at-rest encryption, **not end-to-end encrypted** (spec §9). Server-side keys are
  a deliberate trade for server-side search and ML. No user-facing string this
  arc adds may imply otherwise.

### Sequencing this decision does not settle

Step 0's zero moved key **publication** (#48 — WKD, Autocrypt, key pages) ahead
of signing, and decryption last with an **entry condition** rather than a queue
position: it is built when `mailroom-admin pgp-report` shows there is inbound
mail to apply it to. Building decryption for a mailbox with nothing to decrypt
would ship a mechanism whose only evidence of working is that it does not crash.

## Verification

The two properties worth testing are both **negative**, and both are the kind
that pass vacuously if written carelessly:

- **No API path returns private key material.** Tested by calling every method
  and asserting on what comes back — not by reading the code, and not by
  asserting that a method named `export-private` is absent.
- **A confused-deputy request fails closed.** User A's authenticated context
  asking for user B's key must be refused. The test needs a **positive control**
  alongside it — the same call with A's own key must succeed — or a process that
  refused everything would pass.

Per this project's standing rule, both get probed: delete the identity check and
confirm the confused-deputy test goes red; return a key from an export path and
confirm the first test goes red.

## References

- **#47** — the arc, its increments, and step 0's measurement
- **#48** — key publication, which step 0 moved ahead of signing
- **ADR-013** — why there are no KEKs, and the deferral this ADR leaves standing
- **ADR-005 / docs/key-escrow.md** — the existing single-root escrow this amends
- **#40** — the restore drill that must learn about the second root
- **#69** — S/MIME's deferred home
