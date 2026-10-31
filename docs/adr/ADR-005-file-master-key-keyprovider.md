# ADR-005: File-based master key behind `KeyProvider`; boot unseal; re-wrap rotation; offline escrow

**Status:** Accepted (spec v0.4 §9; design review D5)

## Context

The key hierarchy (master key → per-user KEK → per-purpose DEKs) needs a root.
KMS/HSM is overkill for the target deployment; manual Vault-style unseal is
theater for a remotely-rebooted self-hosted box.

## Decision

Master key is file-based, delivered via Docker/loaded secret at boot, behind
a `KeyProvider` interface so KMS/HSM can slot in later. Unseal is automatic
at boot — accepted consequence: whoever owns the running box owns the keys.
Master-key rotation = re-wrap KEKs only (cheap; annually or on suspicion).
An offline escrow copy of the master key (age-encrypted to an offline
recovery key, stored physically, off-site) is **mandatory** — without it,
backups of envelope-encrypted data are unrecoverable.

Private keys are handled only inside a separate minimal crypto process
(`sign`/`decrypt`/`generate`/`import`/`export-wrapped`; no API returns raw
private material; authenticated user context fails closed).

## Consequences

- Honest framing everywhere: provider-blind with strong at-rest encryption,
  **not** end-to-end encryption; admin blindness is software policy + audit,
  not cryptography (spec §9 threat-model table).
- The restore unit (spec §11) = Postgres + blobs + wrapped keys + independent
  master-key escrow; a scheduled restore drill proves it.

## References

spec §9, §11 · design review P0-2, P0-8, deep-dive A · D5
