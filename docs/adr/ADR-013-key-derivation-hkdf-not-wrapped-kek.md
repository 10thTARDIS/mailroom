# ADR-013: 0.5/1.0 keys are HKDF-derived from the master key; the wrapped-KEK hierarchy is deferred

**Status:** Accepted (deviation from spec §9 / ADR-005's key hierarchy; raised by the backup/restore work)

## Context

Spec §9 and ADR-005 describe a **wrapped** key hierarchy:

```
master key ──wraps──▶ per-user KEK ──wraps──▶ per-purpose DEK ──seals──▶ data
```

with the per-user KEKs stored (wrapped) in the database and carried in backups,
and master-key rotation implemented as a cheap **re-wrap of the KEKs only**.
`docs/key-escrow.md` was written to that design.

**The code does not do this.** In the 0.5/1.0 line every per-user key is
**derived**, not wrapped, straight from the master key by HKDF:

- credentials key = `HKDF(master, salt=userID, info="mailroom-credentials-v1")`
  (`internal/onboarding/credentials.go`),
- mail-blob DEK = `HKDF(master, salt=userID, info="mailroom-mail-blob-v1")`
  (`internal/syncrunner/resolver.go`),
- blob **paths** = HMAC of a key derived from the master
  (`blobPathInfo`, `cmd/mailroom`).

There is no per-user KEK, no wrapped-key store, and no `crypto.RewrapKEK`. The
code comments already call the wrapped random-KEK/DEK hierarchy "1.0 hardening."

## Decision

For 0.5 and 1.0, **keys are HKDF-derived from the master key**, and this ADR is
the authoritative record of that deviation. The wrapped `master → KEK → DEK`
hierarchy of spec §9 is **deferred to 1.x hardening**; it is a defense-in-depth
and key-rotation improvement, not a correctness requirement for the MVP.

`docs/key-escrow.md` is corrected to describe what the code actually does.

## Consequences

- **The restore unit is Postgres + the blob store + an offline master-key
  escrow — there is no separate "wrapped-key store."** The only stored key
  material is the *sealed credential columns* in Postgres (a `pg_dump` carries
  them); the mail-blob and credentials keys are re-derived from the master at
  boot and are never stored. The restore drill is built to this reality.
- **The master key is more load-bearing than the wrapped design implies.**
  Because it also seeds the per-user blob **paths** (HMAC), losing it makes the
  blob store not merely unreadable but **unaddressable** — the files cannot even
  be located. Escrow is correspondingly non-negotiable.
- **Master-key rotation is NOT a cheap re-wrap.** Every at-rest key is a pure
  function of the master, so rotating the master changes every derived key.
  Rotation therefore requires re-encrypting the affected data (re-sealing
  credentials, re-writing blobs under new paths/keys) — an online migration, not
  an O(number-of-users) re-wrap. In practice 0.5/1.0 treats the master key as
  long-lived and escrowed rather than routinely rotated; a cheap-rotation story
  is one of the things the 1.x wrapped hierarchy buys.
- The escrow *procedure* is unchanged and correct: age-encrypt the master key to
  an offline recovery key, store it off-site, decrypt on restore.

## When 1.x implements the wrapped hierarchy

Introduce per-user KEKs (random, wrapped by the master) and rekey the derived
DEKs under them, so master rotation becomes a KEK re-wrap and per-user
compromise is contained. That work must: add the wrapped-key store to the backup
restore unit (the restore drill's manifest already carries a `wrapped_key_store.location`
field so this becomes a deliberate change, not a silent omission), provide the
real `RewrapKEK`, and supersede this ADR.

## References

- Spec §9 (key hierarchy, honest framing), §11 (restore unit).
- ADR-004 (content-addressed blobs, per-user DEKs, versioned envelope), ADR-005
  (file-based master key, boot unseal, offline escrow).
- `internal/crypto`, `internal/onboarding/credentials.go`,
  `internal/syncrunner/resolver.go`, `cmd/mailroom` (`blobPathInfo`).
