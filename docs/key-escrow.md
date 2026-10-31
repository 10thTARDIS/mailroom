# Root-secret escrow procedure

**Status:** mandatory operational control (spec v0.4 §9/§11, ADR-005, ADR-013,
ADR-024). 0.5/1.0 derives keys from the master (ADR-013); the wrapped-KEK
hierarchy is 1.x.

## ⚠ There are TWO root secrets, and their losses are not the same loss

This document used to describe one secret. Since [ADR-024] introduced the crypto
process, there are two, held by two different containers, and **you must escrow
both**:

| secret | held by | derives | what its loss costs |
|---|---|---|---|
| **master key** | the `mailroom` server | mail-blob DEKs, credential keys, model keys, blob **paths** | **everything.** The blob store is not merely unreadable, it is **unaddressable** — the files cannot even be located. |
| **PGP root** | the `crypto` service | per-user PGP key-wrapping keys | **every message ever encrypted to your PGP keys, permanently.** |

They are not two copies of one thing, and the second one is not a lesser version
of the first. **They fail differently, and that is the part worth remembering:**

- Losing the master key is *loud*. Nothing works. You will know within seconds,
  and the restore drill fails at step 1.
- Losing the PGP root is **silent**. The backup restores, Postgres comes up, the
  blobs decrypt, search rebuilds, the mailbox opens and reads normally — and the
  mail that arrived encrypted is gone, with nothing anywhere reporting a
  problem. You would find out the first time someone asked you to read an old
  encrypted message.

That asymmetry is why the drill grew a step for it ([the restore drill], step 5) rather than
leaving it to be noticed.

**Do not point both secrets at the same file.** It appears to work — the stack
boots, serves, and drills clean — and it silently deletes the entire reason the
crypto process exists, because an attacker holding the master key can then
derive every PGP private key. The drill checks for exactly this and fails.

## Why this is mandatory, not optional

Mailroom encrypts everything at rest, and in the 0.5/1.0 line every per-user key
is **derived from the master key** by HKDF (`internal/crypto`; ADR-013):

```
master key ──HKDF(salt=userID, info=purpose)──▶ per-user key ──seals──▶ data
```

(Spec §9's *wrapped* `master → KEK → DEK` hierarchy is 1.x hardening — see
ADR-013 for why the code derives rather than wraps, and what that changes.)

The PGP root works the same way, from an independent starting point:

```
pgp root ──HKDF(salt=userID, info="mailroom-pgp-v1")──▶ per-user wrapping key
                                                          └── seals stored PGP private keys
```

Because in both cases the keys are derived rather than stored, **each root is the
sole root of what it covers**. The master key is additionally the more
load-bearing of the two: it also seeds each user's blob **paths** (an HMAC), so
without it the blob store cannot even be addressed.

Both are delivered to their container as file-based secrets
(`/run/secrets/mailroom_master_key`, `/run/secrets/mailroom_pgp_root_key`) and
read automatically at boot. Both copies live on the same box as everything else.
**If you lose that box and you have not escrowed these elsewhere, your backups
are permanently unrecoverable** — the Postgres dump and the blob store restore
fine and decrypt (and address) to nothing.

The restore unit (spec §11) is therefore: **Postgres + the blob store + an
independent master-key escrow + an independent PGP-root escrow**. There is no
separate "wrapped-key store" in 0.5/1.0 — the only stored key material is the
sealed credential columns inside Postgres (which the dump carries) and, from
increment 3 of [ADR-024], the sealed PGP private keys alongside them. This
document is the escrow leg for both roots.

[ADR-024]: adr/ADR-024-pgp-keys-behind-a-crypto-process-with-its-own-root.md
[the restore drill]: backup-restore-runbook.md

## Threat model for the escrow copy

- It must survive total loss of the production host (fire, theft, disk death,
  provider account loss).
- It must **not** be casually readable if the escrow medium is found: it is the
  key to everything. So it is encrypted to an offline recovery key that never
  touches the production host.
- Honest framing (spec §9): whoever holds the master key can decrypt all user
  data. Admin blindness is software policy + audit, **not** cryptography. Treat
  both the live secret and the escrow copy with that gravity.

## What you escrow

**Two** 32-byte secrets, each in the encoding `crypto.ReadKeyFile` accepts:
either 32 raw bytes, or 64 hex characters (see
`internal/crypto/keyprovider.go`). The examples below use the hex form because
it survives copy/paste and printing.

|  | file on the host | reached by |
|---|---|---|
| master key | `deploy/secrets/mailroom_master_key.txt` | `MAILROOM_MASTER_KEY_FILE` |
| PGP root | `deploy/secrets/mailroom_pgp_root_key.txt` | `MAILROOM_PGP_ROOT_KEY_FILE` |

⚠ **And a third secret that is not a Mailroom key at all: the age identity.**
This document once said "two" and escrowed exactly two, which left
the one that opens the other two wherever `age-keygen` happened to write it.

| | file | what it is |
|---|---|---|
| age identity | `recovery-key.txt` (see the setup section below) | the private half of the offline recovery key |

Losing it is **strictly worse than losing the master key**: without the master
key you still hold ciphertext and a decision to make; without the identity you
cannot open the backup artifact at all, so the other two are unreachable no
matter how carefully they were stored.

It is deliberately generated **off** the mail server and never copied onto it —
the host holds only the public recipient (`scripts/backup.sh` seals to it and
cannot open what it wrote). Escrow it the way you escrow a password manager's
recovery code: offline, physically, and not beside the artifacts it opens.

Label each escrow artifact with **which** root it holds. An unlabelled envelope
in a safe five years from now is a coin flip, and restoring the wrong one into
the wrong variable produces a stack that starts and then quietly cannot read
anything.

## One-time setup: create the offline recovery key

We use [`age`](https://github.com/FiloSottile/age) as the escrow-encryption
tool: small, auditable, no key server, one static binary.

1. On a **trusted, offline** machine (not the production host — ideally an
   airgapped laptop or a live USB session), generate an age identity:

   ```sh
   age-keygen -o recovery-key.txt
   # Public recipient: age1qz...   (printed to stderr; also stored in the file)
   ```

2. `recovery-key.txt` contains the **private** recovery key
   (`AGE-SECRET-KEY-1...`). This is the only thing that can decrypt the escrow.
   - Never store it on the production host or in the repo/backups.
   - Print it to paper and/or write it to a dedicated USB key.
   - Store it **physically, off-site** (bank box, home safe, a trusted second
     location). Two copies in two locations beats one lost copy.

3. Record the **public recipient** string (`age1...`) somewhere convenient — it
   is not secret. You will use it to re-encrypt the master key whenever it
   rotates. Keep it in your ops notes / password manager.

   If you did not record it, you can re-derive it from the identity file — the
   public half is computed from the private one, so this needs the identity but
   nothing else:

   ```sh
   age-keygen -y recovery-key.txt
   # age1qz...
   ```

   Note the direction: recipient **from** identity, never the reverse. That
   asymmetry is why a lost identity is unrecoverable (see Rotation), and it is
   also why the host holding only the recipient is safe.

## Escrow both roots (run after generation and after every rotation)

On the machine that holds the current secrets (or immediately when you generate
new ones), with the recovery **public** recipient `age1qz...`:

```sh
# The two 64-hex-char roots, escrowed to the SAME offline recovery key but as
# TWO artifacts. Two files, because they are two secrets and they will be
# retrieved at different times for different reasons.
printf '%s' "$MASTER_KEY_HEX" \
  | age --recipient age1qz... --armor \
  > mailroom-master-key.age

printf '%s' "$PGP_ROOT_HEX" \
  | age --recipient age1qz... --armor \
  > mailroom-pgp-root-key.age
```

- `--armor` yields ASCII output that is safe to print or paste.
- The outputs are the escrow artifacts. Because they are encrypted to the
  offline recovery key, they may be stored less defensively than the raw keys —
  but still treat them as sensitive.
- **Check they differ** before you file them away — `cmp` the two plaintexts.
  Two identical roots is the one failure this whole arrangement cannot survive
  and nothing at runtime will report.

Store both artifacts:

- One copy **off-site and offline** (printed page in a safe, USB in a bank box).
- Optionally a second copy in your encrypted, off-box backup set (restic, etc.)
  — encrypted-to-recovery-key means the backup provider still cannot read it.

Label each with the date, the key version/scheme, and — above all — **which root
it is**, so a future restore knows what it is holding.

⚠ **This procedure escrows the two roots. It does NOT escrow the age identity**,
and that is the third thing a restore needs — see "What you escrow" above. The
two artifacts you just created are sealed TO that identity, so escrowing them
without it produces two envelopes nobody can open. Do it now, separately, and
not in the same place.

## Rotation (ADR-013)

**Master-key rotation is NOT cheap in the 0.5/1.0 line, and is not routine.**
Because every at-rest key is *derived* from the master (not wrapped by it),
changing the master changes every derived key — so rotation means re-encrypting
the affected data (re-sealing credentials, re-writing blobs under their new
paths and keys), an online migration rather than an O(users) KEK re-wrap. Cheap
rotation is one of the things the 1.x wrapped-KEK hierarchy will buy (ADR-013);
until then, treat the master key as **long-lived and escrowed** rather than
rotated on a schedule.

If you must rotate it (e.g. on a suspected compromise), it is a planned
data-migration operation, not a config change — and after it, re-run the escrow
step with the new master key and keep the previous escrow artifact until the
migration is complete and a restore drill against the new key has passed.

**Rotating the PGP root is worse, not better.** It is not a re-key of the same
data: a PGP private key is an identity, and you cannot rotate away a signature a
correspondent has already trusted, or make mail already encrypted to the old key
unreadable to whoever has it. Rotating the root re-seals the *storage* of those
keys; it does not undo an exposure. Treat the PGP root as long-lived and
escrowed, and if it is ever exposed, plan on **publishing new keys and revoking
the old ones** rather than expecting a rotation to clean up after it.

The offline **recovery** key (the age identity) does not depend on the master
key. There are **two** reasons it might need replacing, they are not the same
event, and this section once described only the first.

### The identity may be COMPROMISED

Someone else may have a copy. The artifacts are still readable by you, so
nothing is lost — but everything sealed to that recipient must be treated as
readable by them too. Re-encrypt the current master key and PGP root to a fresh
recovery recipient, replace all escrow copies, update the host's
`MAILROOM_BACKUP_RECIPIENTS`, and destroy the old artifacts rather than leaving
them to be opened later.

### The identity is simply LOST

The USB stick is destroyed, the safe is emptied, the envelope is misplaced — and
there is no reason to think anyone else has it. This is the branch that was
missing, and it is **not** a lesser version of compromise. It is worse, and the
correct actions are almost the opposite.

⚠ **Every artifact already sealed to that identity is permanently unreadable.
There is no recovery step, and no amount of care with the other two secrets
helps.** An age identity cannot be re-derived from its public recipient — the
recipient is all the host ever had, by design (see "Threat model" above), so
there is nothing on the server to reconstruct it from. The escrowed master key
and PGP root are inside those artifacts, so they go with them.

What to do, in this order:

1. **Generate a fresh identity** exactly as in the setup section, on an offline
   machine. Do not wait for the next rotation window; every backup taken before
   you do is sealed to a key nobody holds.
2. **Replace `MAILROOM_BACKUP_RECIPIENTS` on the host** so the *next* backup is
   sealed to the new recipient.
3. **Re-escrow both roots to the new identity** — the master key and the PGP
   root still exist on the running instance, so this is recoverable *provided
   the instance is still alive*. This is the step with a deadline: if the host
   is lost before you do it, the roots are gone as well, and this stops being a
   backup problem and becomes a total loss.
4. **Run a restore drill against a NEW artifact** before trusting any of it
   (see "Verify it works before you need it"). The old drill results attest to
   an identity you no longer have.
5. **Accept that historical artifacts are gone**, and delete them. Keeping
   unopenable files is how a shelf of them comes to look like retention.

⚠ **Nothing will alert you to this.** `scripts/backup-freshness.sh` checks that
a recent, well-formed, age-encrypted artifact exists — and after losing the
identity, one does, every night. The nightly backup keeps succeeding and the
signal stays green, because *sealed to a key you hold* is not a property
anything on the host can check: the host cannot open its own backups, which is
the entire design. Freshness is not recoverability. Only the drill, run with the
identity in hand, distinguishes them.

This is also why **`MAILROOM_BACKUP_KEEP` does not help here.** Retention keeps
dated copies sealed to the same recipient, so losing the identity makes the
whole history unreadable at once rather than one night of it. Retention protects
against a bad backup; it does nothing against a lost key.

## Recovery (disaster restore)

⚠ **Install BOTH roots. This section once covered only the master key, and
the omission is unrecoverable** — see the asymmetry at the top of this file.

⚠ **Do not run `deploy/README.md` §2's generation commands on a restore.** They
mint new roots, which is correct for a first install and fatal here. A generated
PGP root is indistinguishable from the right one until somebody signs or
decrypts.

When rebuilding from backups onto a fresh host:

1. Restore Postgres + the blob store as usual (there is no separate wrapped-key
   store to restore — see ADR-013).
2. Retrieve **both** escrow artifacts and, on a trusted machine, decrypt each
   with the offline recovery private key:

   ```sh
   age --decrypt --identity recovery-key.txt mailroom-master-key.age
   # prints the 64-hex-char master key
   age --decrypt --identity recovery-key.txt mailroom-pgp-root-key.age
   # prints the 64-hex-char PGP root — a DIFFERENT secret, not a copy
   ```

   ⚠ The filename is `mailroom-pgp-root-**key**.age`, matching what the escrow
   section above writes. This line said `mailroom-pgp-root.age` until a
   rehearsal ran the procedure verbatim and hit
   `failed to open input file: no such file or directory` — **on the second
   root, during a disaster restore, on the exact secret this document opens by
   warning is the unrecoverable one to omit.** The master key decrypted fine one
   line earlier, so the failure arrives after the procedure has already appeared
   to work.

3. Install each on the new host, at the path its own variable reads, matching the
   encoding rules in `internal/crypto/keyprovider.go`:

   | | path read by | default |
   |---|---|---|
   | master key | `MAILROOM_MASTER_KEY_FILE` | `/run/secrets/mailroom_master_key` |
   | PGP root | `MAILROOM_PGP_ROOT_KEY_FILE` | `/run/secrets/mailroom_pgp_root_key` |

   Putting the same value in both is its own disaster — the drill's `same-root`
   injection exists for it.

4. Boot Mailroom. Unseal is automatic. The master key is exercised immediately
   (mail becomes readable, or you get `blob: not found`). Stored PGP keys are
   still unsealed **lazily, on first use** — but since migration 00055
   the `crypto` service compares its root with a check value the database
   recorded at its first boot, and **refuses to start on a mismatch**: the
   container restarts, never turns healthy, and logs `PGP ROOT MISMATCH`. That
   means the envelope is wrong; go back to step 2.

   ⚠ A backup from before the crypto process first ran against 00055 has no
   recorded value, and its first boot adopts whatever root it is given. A
   healthy crypto service is evidence only when the database had a record to
   compare against — its log says which: `"outcome":"verified"` or
   `"outcome":"recorded"`.
5. **Prove the PGP root before you trust it.** Run the drill against the artifact
   you just restored (see below) — it unseals every stored PGP key and
   fingerprint-verifies them, and `--inject wrong-pgp-root` exists to prove that
   check can actually fail. The boot check proves the root is the one the
   database recorded; this is the only step that proves it **opens the stored
   keys**.

## Verify it works before you need it (restore drill)

A backup that has never been restored is a hypothesis (spec §11). On a
schedule, in a scratch container:

- Restore the backup set, install the escrowed master key via the recovery
  path above, decrypt **one message per user**, rebuild one user's search
  index, and run one search.
- If any step fails, the escrow or the backup is broken — fix it now, not
  during a real outage.

## Do / don't

- **Do** keep the recovery private key and the production host in different
  failure domains (different physical locations, different accounts).
- **Do** re-escrow immediately on every master rotation and re-run the drill.
- **Don't** commit any real key material (master key, recovery key, or escrow
  artifact) to the repo, container images, or unencrypted backups.
- **Don't** log key material anywhere. Ever.

## References

- ADR-013 — 0.5/1.0 keys are HKDF-derived from the master key; the wrapped-KEK
  hierarchy (and cheap re-wrap rotation) is deferred to 1.x. **This is why the
  above describes derivation, not wrapping.**
- ADR-005 — File-based master key behind `KeyProvider`; boot unseal; offline
  escrow.
- ADR-004 — Content-addressed blobs, per-user DEKs, versioned envelope.
- Spec v0.4 §9 (key hierarchy — the wrapped design, now the 1.x target) and §11
  (restore unit, drill).
- `internal/crypto`, `internal/onboarding/credentials.go`,
  `internal/syncrunner/resolver.go` — the actual HKDF derivation.
