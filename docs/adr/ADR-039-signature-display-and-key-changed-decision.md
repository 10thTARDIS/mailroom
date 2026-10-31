# ADR-039: A PGP signature is shown as one of four honest states, and a changed key is the user's decision

**Status:** Accepted (Forgejo #47 v1.92 item 4, #626 section E; builds on ADR-024,
ADR-037 and ADR-038)

## Context

ADR-037 verified a signature inside a decrypted message and ADR-038 refused to
encrypt to a recipient whose key changed, both leaving the user without a way to
act. Two things were missing: signature results on ordinary signed mail, and the
decision a changed key calls for (the 2026-09-07 ask on #47: "signed with a key
you have not seen from this sender", both fingerprints, trust or keep).

The design question that is not glue: **what may be called "verified".** The
signature verifying against a key we hold proves the content matches that key.
It proves nothing about the sender when the key was obtained from the same
message (an Autocrypt header on forgeable mail), or when the key does not name
the From address. A display that said "verified" in those cases would tell a user
a forgery was fine.

## Decision

### 1. One property, `pgpSignature`, for both shapes

`Email.pgpSignature` (urn:mailroom:pgp) replaces `encryptedSignature` from
ADR-037. A PGP/MIME `multipart/signed` message (verified at Email/get with
`pgpverify`, nothing stored) and the `multipart/signed` inside a decrypted
message answer the same question with the same four states, so a client has one
table, not two. `encryptedSignature` shipped only in `1.92.0-rc.1`, a
test pre-release, and is removed rather than aliased: the web client and server
are one image, and an alias would keep a second shape alive for no reader.

`null` when unsigned. Otherwise `{state, address, fingerprint?,
previousFingerprint?, refused?}`:

| state | meaning |
|---|---|
| `verified` | good signature, by a key held for the From address that names it, that the user may rely on, or by the user's own key |
| `bad` | there is a signature and it does not check out, or cannot be read |
| `unknownKey` | well-formed, not attributable to the sender (no key held; the key does not name the From address; or a held key nobody vouched for with nothing to compare it with) |
| `keyChanged` | good signature by a key different from the pinned one (or, unpinned, the best eligible one) |

### 2. The rules (the same ones that gate encryption)

A good signature is `verified` only if all hold:

1. **It names the address.** The signing key has a valid, non-revoked user ID
   whose email equals the From address (`pgpkeys.NamesAddress`, the rule of
   ADR-038 section 4 rule 3, without needing an encryption subkey). A good
   signature by a key for another address is `unknownKey`, whatever else is true.
2. **It is an eligible key** (`compose.EligibleSource`, the same function the
   encrypt path ranks with): imported, WKD, or Autocrypt from an authenticated
   message; not refused. An unauthenticated Autocrypt key that verifies is
   **never** `verified`: it is `keyChanged` if there is something to change from,
   otherwise `unknownKey`.
3. **It is the pinned key, or one the user imported/trusted** (source `manual`:
   an explicit act, as ADR-038 section 5 already lets it replace a pin). A
   different, non-manual key than the pin is `keyChanged`.

The keys tried are those held for the From address plus the user's own. A
signature by a key held under another contact is `unknownKey` (its issuer, read
from the signature packet, is shown).

### 3. Trust on first verification

"A key that differs from a previously verified key" needs a record of what was
verified. The existing pin is that record, so the first time an eligible key
verifies for an address with no pin, it is pinned (`PinContactKeyIfAbsent`,
insert-or-nothing). Without this, change detection would apply only to
correspondents the user has encrypted to. This is the one write Email/get makes
for this property; it never replaces a pin; it is not a JMAP-visible object (the
pin, like ADR-038's, has no change_log row).

### 4. The decision, `ContactKey/decide`

A narrow method: `{address, fingerprint, decision: "trust" | "keep"}`, for a key
already held under the contact that owns `address`. It cannot add or delete a key.

- **trust**: the key becomes source `manual`, un-refused and most recently
  fetched, so ADR-038 ranks it first; it becomes the pin. Encryption to that
  address then uses it (the `keyChanged` refusal of ADR-038 section 5 clears).
  It requires the key to name the address, else `keyDoesNotNameAddress`:
  trusting a key for an address it does not name would make it the pin for mail
  to that address.
- **keep**: the key is marked refused (`contact_keys.refused`, migration 00069,
  one column, no new table). It is never encrypted to (`compose.sourceRank`
  returns ineligible), the pin stays the old key, and a signature by it keeps
  showing `keyChanged` with `refused: true`. Trust after keep reverses it.

Both write the key row, the pin (trust only) and a **Contact `updated`
change_log row** in one savepoint: `Contact.keys` (which reports `source` and
`refused`) is part of the synced Contact object, so the invariant applies.

### 5. Display

The reading pane's signature line says each state in a sentence (nothing is
colour alone). A changed key shows both fingerprints grouped in fours, one
sentence on what it can mean (the person changed keys, or someone is
impersonating them), the two buttons and a "compare" disclosure. Compose's
encrypt refusal for a changed key shows the same component. The wording says a
verified signature means the content matches a key held for the sender, not who
the sender is.

### 6. Signatures inside the encryption (RFC 3156 section 6.2)

Added after the 1.92.0-rc.2 test (#47, "rc.2 test finding"): Thunderbird, like
GnuPG and most clients, signs and encrypts in ONE OpenPGP message (section 6.2,
"combined": one-pass signature, literal data and signature packets inside the
SEIPD packet), not as a nested `multipart/signed` (section 6.1). Section 1 and
ADR-037 section 7 showed `null` for that form, so real signed mail
had no signature line.

- **The crypto process checks it, in the same read as the decryption**, with
  go-crypto only: `openpgp.ReadMessage` over a key ring that keeps the two jobs
  apart (the user's keys decrypt; the candidate keys verify; neither does the
  other's job). The body is read to EOF through the existing caps and only then
  are `IsSigned`, `SignedBy`, `Signature` and `SignatureError` read: the library
  checks the signature at the end of the literal data, so any earlier answer is
  meaningless. A nil `SignatureError` alone is not a good signature (go-crypto
  leaves it nil when no candidate key matched and nothing was checked).
- **The application chooses the candidates** for the From address (outer
  message, as always) from the same keys section 2 tries for section 6.1: the
  pinned key, eligible keys by rank, the user's own keys that name the address,
  then INELIGIBLE held keys (unauthenticated Autocrypt, refused), so that a
  signature by one of those reads as `keyChanged`/`unknownKey` rather than as "no
  key". At most 16 keys and 1 MiB (`pgpwire.MaxDecryptSigners`,
  `MaxDecryptSignerBytes`); a key carrying private material refuses the request.
  Eligibility is decided here, never in the crypto process.
- **The wire answer is a status, not a story**: `{signed: false}`, or `{signed:
  true, status: verified|bad|noKey, issuer_fingerprint | issuer_key_id,
  signer_fingerprint (verified only)}`. No library text. `bad` covers a candidate
  key whose signature does not verify: altered content, an expired or revoked key
  or signature, a missing signature packet, and a weak hash. A bad signature
  still returns the plaintext (the message did decrypt); a decryption failure is
  still the single opaque error; the audit row is unchanged (decrypting is the
  audited act; verifying persists nothing beyond section 3's pin).
- **Weak hashes are refused on our side.** Measured in go-crypto v1.5.2: the v1
  reader verifies a SHA-1 signature as good; `RejectMessageHashAlgorithms` is only
  consulted by the v2 API. The crypto process therefore reports SHA-1 (and MD5,
  RIPEMD-160) as `bad`, the verify-side twin of the #301 C2 refusal in Sign.
  `pgpverify` (section 6.1) had the same gap and accepted a SHA-1 detached
  signature as verified; it now applies the same list (`pgpverify.WeakHashes`,
  which a test holds equal to the crypto process's), so the two forms agree. MD5
  and RIPEMD-160 packets cannot even be parsed by this library and are skipped,
  so such a message reads as unsigned, never as verified.
- **Same states, same code.** `{status, fingerprint}` is mapped onto the
  `pgpverify.Result` that section 6.1 produces (`verified` → verified with that
  signer, `noKey` → no key with the issuer, `bad` or anything unknown → bad) and
  judged by the same `classifySignature`, so the UID-names-From rule, eligibility,
  the pin, TOFU pinning and `keyChanged` are identical for both forms.
- **Precedence.** When the decrypted entity is itself a `multipart/signed`, that
  inner signature is reported and the embedded one is not: it covers exactly the
  entity that is rendered, content headers included, while an embedded signature
  covers the whole plaintext, of which that signed entity is one part. A
  malformed inner `multipart/signed` is `bad`, not a fall-through to the embedded
  verdict. Otherwise the embedded verdict is reported.

## Consequences

- The decrypt-time and read-time displays cannot disagree: one function
  (`classifySignature`) decides, over facts gathered once.
- A correspondent never written to gets a pin from the first verified signed
  message; a later key from them differs and is flagged. The cost is that an
  eligible key which legitimately rotates is flagged once per address until the
  user trusts it (as for encryption).
- Old mail signed by a retired key shows `keyChanged` against the new pin until
  the user trusts the old key too; the display does not keep a history of keys.
- Signatures inside the OpenPGP layer (RFC 3156 section 6.2) are verified by the
  crypto process against keys the application sends (section 6), which widens
  that process's input by up to 16 public keys per decrypt and its output by a
  status and public identifiers; it still holds no correspondent keys of its own.
- A key cut off by section 6's bound makes its signature `unknownKey`, never
  `verified`; a correspondent with more than 16 keys is not expected.

## References

- #47 (2026-09-07 ask; v1.92 plan item 4; the rc.2 test finding), #626 section E,
  #301 C2; ADR-024, ADR-037, ADR-038; RFC 3156 sections 5, 6.1 and 6.2.
