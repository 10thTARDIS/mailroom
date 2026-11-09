# ADR-054: Key rotation: the old key certifies the new one, and a certification by the pinned key makes a rotation vouched-for, never automatic

**Status:** Accepted (GitHub #31; builds on ADR-038, ADR-039 and ADR-040)

## Context

ADR-040 §14 left one line open: "the old key certifying the new one is out of
scope (#31)". On the receiving side, ADR-039 makes every changed correspondent
key the user's decision, with one prompt whatever the circumstances: an honest
rotation and an impersonation produce the same `keyChanged` display
(`internal/api/pgp_signature.go:194-198`) and the same encrypt refusal (ADR-038
§5). OpenPGP's tool for this is old: the previous key signs the new key's user
ID. This ADR decides both halves together, because the outgoing half is worth
nothing unless a receiving side reads it, and Mailroom is the one receiving
side this project controls.

Three facts about the code shape the design:

- **The crypto process holds both keys at replacement.** `Generate`
  (`internal/pgpkeyd/service.go:167`) builds the new entity in memory before
  sealing it, and `lifecycleTarget` + `unseal` (`internal/pgpkeyd/lifecycle.go:43`,
  `service.go:693`) already resolve, ownership-check and open an existing key
  for `Extend` and `Revoke`.
- **The merge drops every third-party certification.** `readKeyBlock` keeps a
  user-ID signature only if `bySelf` holds (`internal/pgpkeys/lifecycle.go:533-535`),
  for the reasons ADR-040 §1 gives. A certification arriving on a
  correspondent's new key is stored raw by the INSERT path of
  `UpsertContactKeyDetail` and then **silently removed on the first
  re-sighting**, when the stored copy is normalised. No test asserts the drop.
- **The pin is the only record of "the key I had"** (`app.contact_key_pins`,
  ADR-038 §5, ADR-039 §3). Everything "vouched-for" is measured against it.

## Decision

### 1. Outgoing: the old key certifies the new key's user ID at replacement

**When.** Only through `UserKey/replace`, which is already the *supersede*
path: it refuses a revoked, retired or unbound key before anything is generated
(`replaceRefusal`, `internal/api/user_key_replace.go:176`). Compromise stays
`revoke --reason compromised` then `generate` (ADR-040 §3), and nothing on that
path certifies. The certifier's **own packets** must also allow it at the crypto
process's clock: `Validity.CanSign(now)` (`internal/pgpkeys/lifecycle.go:121`,
not revoked, primary not expired) and go-crypto's `CertificationKey(now)`. An
old key that has already expired cannot make a certification any reader
accepts, so the replacement **proceeds uncertified** and says so (`certified:
false, certifyReason: "oldKeyExpired"`); it is never refused, because the user
needs a new key most when the old one has lapsed.

**Where.** `GenerateRequest` gains an optional `certifier_key_id`. `Generate`
resolves it as `Extend` does (found, owned by the same `user_id`, else
`denied`), unseals it, and calls `newEntity.SignIdentity(uid, oldEntity, cfg)`
on the new key's one user ID **before** `SerializePublic` and `vault.Insert`.
The stored `public_key` carries the certification from its first byte: no
window with an uncertified key, no second write, no new wire method (the
leak-probe registry is untouched). It is a `SigTypeGenericCert` (0x10) by the
old primary, SHA-256, with the issuer-fingerprint subpacket, no expiry, dated
at generation. The artefact is checked, not assumed: the public bytes are
re-read and the certification verified under the old key's **public** half
(`VerifyUserIdSignature`); a mismatch refuses the generate as `internal`, as
`checkGenerated` does for expiry.

`UserKey/replace` passes the old key's id by default and takes `certify: false`
to opt out: on by default because the two keys already share a user ID in
public; optional because the link is permanent.

**Publication needs no new code.** WKD serves `user_keys.public_key` verbatim
(`internal/keypub/keypub.go:198`), the outgoing Autocrypt header carries the
same bytes (`internal/compose/compose.go:873`), and the key page's download and
`UserKey/export` read the same column. The header grows by about 120 bytes.

**Honest limits.** Autocrypt and WKD-only clients ignore third-party
certifications. GnuPG imports and shows them, so only a correspondent who
verified the old fingerprint out of band and reads certifications gains
anything; Mailroom's own receiving side gains §3. The certification is worse
than worthless if the old key is compromised: it hands the thief's key whatever
trust the old one had, which is why compromise never reaches this path and why
§4 refuses automatic acceptance. And it links the two fingerprints publicly and
permanently, including inside a key that is later revoked.

### 2. The merge keeps exactly one kind of third-party certification

`pgpkeys.Merge(stored, incoming, certifiers)` and `readKeyBlock` take the
public keys of the **key(s) currently pinned for the contact's addresses**
(`app.contact_key_pins`), held, not removed and not refused, other than the key
being merged. **Holding is free**: any spoofed Autocrypt header makes an
attacker's key a held key for a contact, so a held key, and a refused one, is
never a certifier. **Only the pin can certify**, which is also all `CheckVouch`
reads. A user-ID signature that is not a self-signature is kept
**if and only if**:

1. `sig.CheckKeyIdOrFingerprint(certifier.primary)` matches one certifier: the
   signature names that key as issuer (full fingerprint subpacket when present,
   else the 64-bit key id);
2. `certifier.primary.VerifyUserIdSignature(uid, thisPrimary, sig)` succeeds
   under that certifier's public key; a claimed issuer alone is dropped;
3. it is a certification type (0x10–0x13) over a user ID of this key, or a
   certification revocation (0x30) by the same certifier, which voids a vouch
   (§3 rule 2);
4. its hash is not MD5, SHA-1 or RIPEMD-160 (`pgpkeys.WeakHash`, one definition
   shared with the crypto process): a chosen-prefix collision can transplant a
   certification made with one;
5. it is the **newest** of its kind for that (certifier, user ID): at most one
   certification and one revocation are kept per pair, whatever arrives.

Everything else that is not a self-signature is dropped as today: a certifier
that is not the pin, one held for a **different** contact, anything over a
user attribute or at the key or subkey level. The function stays pure and
public-key-only (ADR-024). Certifiers are read once inside the savepoint that
already locks the held row (`UpsertContactKeyDetail`, `ImportContactKey`,
`ContactKey/refresh`), and the INSERT path normalises a new key through the
same `readKeyBlock` with the same certifiers, so what is stored on day one is
what survives the first merge. The size bound: a stranger
**can** make a key held for the contact (holding is free), so the bound does not
rest on holding. It rests on the pin: only a signature that verifies under a
pinned key survives, and then at most two (one certification, one revocation)
per pinned key and user ID. (An earlier draft said a stranger cannot attach a
signature that verifies under a held key; that was false, and with every held key
as a certifier ~1,500 certifications by an attacker's held key filled the honest
key's stored copy to the size limit and blocked its renewals.)

The review question for this exception: can anyone who can send mail add a
signature that survives? Only by controlling a key the user has pinned, and then
the kept bytes are bounded.

### 3. Incoming: a vouched-for rotation, and every refusal

When a correspondent's new key N is judged for address A (`judgeSignature`, and
`RecipientKey/lookup` for compose), N is **vouched for** by the pinned key P
when every rule holds. Each is one test; the first failure ends the check.

1. **P is pinned for A** (`contact_key_pins.fingerprint == P`) and held, not
   removed. A key merely held, or verified once without becoming the pin,
   cannot vouch: only the pin is the user's record of "the key I had".
2. **N carries a certification by P over a user ID that names A**
   (`parseUserID` gives A, case-insensitive) that verifies under P's public key
   now, re-run at judgement rather than trusted from storage, with a strong hash.
   A certification revocation (0x30) by P over the same user ID, dated at or after
   the certification, voids it (`certificationRevoked`); a later re-certification
   stands. A certification
   over a different address, or over a user ID with no parseable address, is
   not a vouch for A; a different display name with the same address is.
3. **P is not revoked, at all**: no key revocation, no revocation of the
   certifying user ID (`Assess(P).Revoked` false). The issue's two cases both
   fall here. Revoked *before* the certification time: a revoked key signed it.
   Revoked *after*: the holder found a reason to kill the key once the
   certification existed, and a thief who made it is exactly who that reason
   may be. Reason `compromised` (`packet.KeyCompromised` from the revocation's
   `RevocationReason`) additionally **hardens** the ordinary prompt (below).
4. **Made while P was valid and N existed**: `sig.CreationTime <
   Validity(P).PrimaryExpires` when P expires; `sig.CreationTime >= N.Created`;
   the certification itself not expired (`SigExpired(now)` false).
5. **N was seen in time.** N's `first_seen` event (ADR-040 §11) is no later
   than `Validity(P).PrimaryExpires + ExpiryWarningWindow` (30 days, the
   existing constant). This is the one defence against **backdating** that does
   not rest on the signer's clock. A creation time is the signer's own claim
   (ADR-040 §9 already refuses to upgrade a verdict on it); a thief holding an
   expired P dates the certification wherever rule 4 wants. What the thief
   cannot choose is when this server first saw N. The cost: an honest
   correspondent who rotates at expiry and does not write for a month gets the
   ordinary prompt, which is the normal path, not a failure.
6. **Clock skew.** A certification dated more than 24 hours in the **future**
   (past now) is refused (whose clock is that?). A date after N's `first_seen` but
   already past is an honest late certification (a GnuPG user who certifies later)
   and vouches; rule 5, not this rule, is the defence against backdating.
7. **N passes every existing check for A unaided**: it names A
   (`NamesAddress`), is readable with no private material, is not expired,
   revoked or user-`refused`, and the message signature verifies against it. A
   vouch softens the prompt; it never stands in for a check N would fail. An
   unauthenticated Autocrypt source for N is **not** a bar: the certification,
   not the transport, vouches.
8. **Nothing is stored as a fact.** The vouch is recomputed from the held copies
   on every judgement, so a revocation of P that arrives later withdraws the
   softer prompt by itself. When the user then trusts N, the `pinned` event's
   source records the voucher (`"vouched:" + P`), so a later revocation of P
   raises one notice (built; see the amendments): "You trusted this key because
   P's holder's previous key vouched for it. That previous key has since been
   revoked (as compromised). Check this key with them another way."

**What the user sees.**

| case | wire | wording |
|---|---|---|
| all rules hold | `keyChanged`, `vouchedBy: P` | "Signed with a new key for {address}. Their previous key, the one you had, vouches for it. Trust the new key, or keep the old one." Trust is the default button; the compare disclosure is unchanged. |
| any rule fails | `keyChanged` | today's prompt, unchanged |
| P revoked as compromised | `keyChanged`, `previousKeyCompromised: true` | today's prompt plus: "The key you had for {address} was reported stolen or compromised, so a new key from them cannot be checked against it. Confirm the fingerprint another way." |

Compose is the same: `cannotEncrypt` still refuses until the user decides
(ADR-038 §5), carrying `vouchedBy` so the dialogue uses the softer sentence.
`ContactKey/decide` is unchanged. **Nothing in this ADR moves a pin without the
user acting.**

### 4. The operator decision: no automatic acceptance

**The question.** When the key a contact used to have vouches for their new
one, should Mailroom switch to the new key without asking?

- **A. Never.** Always ask, but more gently: the prompt says the old key
  vouches, and trusting is one click.
- **B. Automatic while the old key is still valid and unrevoked**, with a
  notice afterwards and an undo that restores the old pin and refuses the new
  key.
- **C. A per-user setting**, default off, choosing A or B.

**Decision: A (operator, 2026-11-09: "always ask, gently").** A vouched-for rotation never moves a pin without the user; the softer prompt has trust as the default button.

**Recommendation: A.** The threat the pin exists for is a stolen private key
the owner has not yet revoked. Its holder can already sign as the contact and
read mail encrypted to them; the pin never claimed otherwise. What the pin does
is bound the damage in **time**: when the owner revokes, or the key expires,
the thief's access ends. Option B removes the bound. The thief generates a key,
certifies it with the stolen one, sends one message, and every Mailroom user who
had the contact pinned now encrypts to the thief, with no prompt. The owner's
revocation arrives later and changes nothing, because the pin is now the thief's
unrevoked key, and the owner's own honest rotation then reads as `keyChanged`
against it. Auto-accept turns a theft that revocation would have ended into a
permanent, silent man-in-the-middle; the "notice afterwards" is a line in a list
read too late, and the undo needs the user to notice, which the prompt made
certain and the notice made optional. B's condition, "while the old key is
still valid", names exactly the window in which a stolen key is most
dangerous. C hands a judgement to someone with no information to make it. The
cost of A is one click per honest rotation per correspondent, now with the one
sentence that makes it easy. The design above has no code path that moves a
pin without a decision, so adopting B later is a new ADR, not a flag.

### 5. Tests the implementation must have

Each §3 rule is one refusal test **with a positive control** (the same fixture
with the rule satisfied is vouched), so a judge that refuses everything cannot
pass: P never pinned (held and verified once); certification over another
address; same address, other display name (vouched); user ID with no address;
one flipped byte; P revoked `superseded` before the certification, and after
it; P revoked `compromised` (asserts `previousKeyCompromised`); dated after P's
expiry; dated before N's creation; a SHA-1 certification; a certification
revoked by P after it (and re-certified after the revocation: vouched); **backdated**: P expired a year ago,
certification dated the day before, N first seen today (refused by rule 5), and
N first seen 20 days after expiry (vouched); dated two days in the future
(refused), nine days after `first_seen` but in the past (vouched), and one hour
after `first_seen` (vouched); N user-refused; N not
naming A; N expired. Each runs through `classifySignature` for both ADR-039
shapes and once through `RecipientKey/lookup`.

Merge: a certification by a key the user does not hold is dropped; by a key
held for a **different** contact, dropped; by a same-contact key but failing to
verify, dropped; one that verifies survives both a merge and the INSERT
normalisation, under `MaxKeyBytes`. Then the probe CLAUDE.md requires: remove
the `VerifyUserIdSignature` call and confirm the "fails to verify" test goes
red; remove the issuer match and confirm "different contact" goes red.

Outgoing: `Generate` with `certifier_key_id` stores a key whose user ID carries
a certification that verifies under the old public key; another user's
certifier is `denied` (with the positive control); a revoked, retired or
expired certifier yields `certified: false` and an uncertified key, never a
failed replacement; the leak probe still covers every method. Publish paths:
the WKD handler's body, the decoded outgoing Autocrypt header and the key page
download are the stored bytes and carry the certification; fed to the §2 merge
with the old key as certifier it survives, and without any certifier it is
dropped. Those two together are the end-to-end proof that a Mailroom-to-Mailroom
rotation is vouched.

## Amendments made on implementation

- **The INSERT path stores the sighted bytes as they arrive** (it never
  normalised them; only a merge does). So the exception is applied where it has
  an effect: the certifiers are read for every merge (`UpsertContactKeyDetail`,
  `ImportContactKey` and the revive path, which covers `ContactKey/refresh`),
  and a key stored with its certification survives its first re-sighting
  unchanged. A certification that no held key issued is still stored raw until a
  merge rewrites the key; that is today's behaviour and is unchanged.
- **A held key with no `first_seen` event** (stored before migration 00071) has
  no first-sighting time, so a certification for it is refused and the ordinary
  prompt shows (rules 5 and 6 cannot be evaluated; they fail toward asking).
- **Replace answers** `certified` and `certifyReason` (`oldKeyExpired`,
  `oldKeyRevoked`, `oldKeyRetired`, `oldKeyUnusable`, or null). `Generate`
  answers the same pair; the leak-probe request for `generate` now names a
  certifier so the unsealing path is inside the leak assertion.
- **`cannotEncrypt`** carries `keyChanges`: per recipient, `vouchedBy` and
  `previousKeyCompromised`, omitted when absent.
- **Rule 8 is built** (after the security review). When the current pin's latest
  `pinned` event is `vouched:<P>` and P is still held and revoked, the signature
  classification (`pgpSignature`) and `RecipientKey/lookup` carry
  `pinVouchedByRevoked: true`, plus `pinVouchedByCompromised: true` when the
  reason is compromise. The reader's signature line and the compose encrypt
  switch show the sentence in rule 8. It is a notice only: the state stays
  `verified`, encryption is not refused, and nothing changes the pin. A pin the
  user decided without a vouch shows nothing.
- **Review fixes to §2.** The certifier set is the pinned key(s) only, not every
  held key; at most the newest certification (and newest revocation) is kept per
  certifier and user ID; weak-hash certifications are refused in the merge and in
  `CheckVouch`; a 0x30 by the pinned certifier is kept in the merge and voids the
  vouch; rule 6 refuses only a future date.

## Alternatives considered

- **A separate `certify` verb after `generate`.** Needs `KeyUpdater` to
  rewrite the new key, leaves a window with an uncertified key, adds a method to
  the leak-probe registry, and buys nothing the request field does not.
- **Certify at `pgp-revoke --reason superseded`.** At revocation the old key is
  being killed and the new key may not exist; `replace` is the one moment both
  are live and the intent is known.
- **Every key held for the contact may certify.** Rejected after review: holding
  is free, so a stranger's held key could fill another key's stored copy.
- **Keep all third-party certifications, bounded by count.** Nothing reads
  them, and a stranger can fill the budget so the honest one is the one that
  does not fit.
- **Accept a certification by any key ever pinned for the address.** No pin
  history exists, and "the key before the key I had" is a weaker claim than the
  issue asks for. Revisit if `contact_key_events` grows one.
- **Auto-accept only on first prompt for a contact, while P is valid.** Option
  B with a smaller window, and the window is the dangerous one.
- **Report a vouched key as `verified`.** ADR-039 §2 defines `verified` as a
  key the user may rely on, and the user has not yet said so.

## Consequences

- An honest Mailroom-to-Mailroom rotation produces a prompt that explains itself
  and takes one click. Every other client sees nothing new.
- The merge gains one input and one exception, both pure, both probed. The
  three key-storing routes read the contact's other keys inside a savepoint
  they already hold: one more indexed read per sighting.
- `pgpSignature`, `RecipientKey/lookup` and the `cannotEncrypt` description
  gain `vouchedBy` and `previousKeyCompromised`, optional and omitted today, so
  older clients are unaffected.
- The old fingerprint lives in the new key's packets for good. `certify: false`
  gives today's behaviour.
- A certification by a stolen, unrevoked key is a vouch until the revocation
  reaches Mailroom; rule 3 then withdraws it on the next read and rule 8 names
  it if the user already trusted. That residual is bounded by the prompt, which
  is why §4 chose A; under B it is unbounded.
- Backdating is undetectable from the signature; rule 5 bounds it by what this
  server observed, at the cost of the ordinary prompt when a new key arrives more
  than 30 days after the old one expired.
- `contact_key_events.source` gains a `vouched:` form; `kind` stays `pinned`.
  **Amended on implementation:** `source` is NOT unconstrained (migration 00071
  has a CHECK on it), so one small loosening migration (00096) admits exactly
  `vouched:` plus an uppercase hex fingerprint (40 or 64 characters), the form
  the code writes. No row changes.

## References

- GitHub #31; ADR-024, ADR-038 §4–§5, ADR-039 §2–§4, ADR-040 §1, §3, §9, §11,
  §14.
- RFC 9580 §5.2.1 (certification types 0x10–0x13), §5.2.3.11 (creation time),
  §5.2.3.23 (reason for revocation), §5.2.3.35 (issuer fingerprint).
- Code: `internal/pgpkeys/lifecycle.go` (`Merge`, `readKeyBlock`, `Validity`),
  `internal/pgpkeyd/service.go` (`Generate`), `internal/pgpkeyd/lifecycle.go`
  (`lifecycleTarget`, `unseal`), `internal/api/user_key_replace.go`,
  `internal/store/user_key_replace.go`, `internal/api/pgp_signature.go`
  (`classifySignature`, `judgeSignature`), `internal/api/contact_key_decide.go`,
  `internal/store/contact_keys.go` (pins, `UpsertContactKeyDetail`),
  `internal/store/contact_key_history.go`, `internal/keypub/keypub.go`,
  `internal/compose/compose.go` (the Autocrypt header).
