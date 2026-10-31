# ADR-038: Outgoing mail is signed then encrypted to recipients whose keys we trust, and to the sender

**Status:** Accepted (v1.92 item 3; builds on ADR-024 and
ADR-037; the key-changed confirmation is `ContactKey/decide`, ADR-039)

## Context

ADR-037 made encrypted mail readable. This is the other direction: a compose
"Encrypt" switch. The requirement was the simplest implementation that relies
on existing packages, so ProtonMail go-crypto does all OpenPGP work and Mailroom
writes glue: which keys, what to refuse, RFC 3156 framing.

The design question that is not glue is **whose key to encrypt to**, because a
wrong answer is silent and unrecallable: the user's mail is readable by whoever
owns the key. The first version of key resolution picked the newest row in
`app.contact_keys`. A security review showed why that is not acceptable: an
Autocrypt key arrives in a header on received mail, and the From of that mail can
be forged, so a stranger could send one message "From: alice" carrying their own
key and the user's next encrypted mail to Alice would be readable by the
stranger. The rules below close that.

## Decision

### 1. The `encrypt` crypto-process verb (no private key used)

`encrypt` takes the user id, the sender's key id, the recipients (address plus
PUBLIC key, binary or armored) and one MIME entity, and returns an armored
OpenPGP message and the fingerprints used.

- It adds the **sender's own public key** as a recipient (looked up by key id,
  ownership-checked exactly like `sign`), so the Sent copy stays readable through
  decrypt-on-open.
- It requires every recipient key to have a valid, unexpired, unrevoked
  encryption subkey (`Entity.EncryptionKey` at the daemon's clock). Otherwise it
  refuses with `no_recipient_key` whose message names the **addresses**, all of
  them at once, never key material. A key carrying private material is refused.
- SEIPD with an MDC only (AEAD config left nil, so SEIPD v1); there is no code
  path that writes a Symmetrically Encrypted Data packet, and the verb reads its
  own output back and refuses unless it is N session-key packets and one
  integrity-protected data packet.
- Bounded: entity 24 MiB (below the 32 MiB decrypt cap, so the sender can always
  open what they sent), 100 recipients, 256 KiB per key, its own request and
  response limits; it shares the decrypt concurrency slots.
- It is in the method registry with a **succeeding** leak probe; its tests assert
  that no private key, no session key and no plaintext appears in any response or
  log (the session key is recovered in the test by unwrapping the session-key
  packet with the recipient's key, with a control proving the search can see it).

**Deviation from the plan, and why:** the verb is `encrypt`, not `sign-encrypt`.
Signing is the existing `sign` verb: the application wraps the message in an RFC
3156 `multipart/signed` first (`pgpmime.Sign`), then `pgpmime.Encrypt` hands that
signed entity to `encrypt`. This is RFC 3156 §6.1 sign-then-encrypt, the form
ADR-037's decrypt-on-open already verifies with `pgpverify`, and it needs no
second signing path or MIME code inside the crypto process. `encrypt` uses no
private key at all, so it cannot lose one; it lives in the crypto process so all
OpenPGP operations stay in one small, bounded implementation, not for secrecy.
It is not audited or rate-limited like `decrypt` (it returns no plaintext and
opens nothing).

### 2. Where it happens: Submit, rewriting the stored message

At the point signing already happens (`prepareOutbound`), after signing, the
stored message is rewritten to the encrypted one before the submission row
exists. The Sent copy appended to IMAP, the blob, the content hash and the bytes
the recipients receive are one object. Drafts are not encrypted. The plaintext
`snippet` column is blanked with the rewrite (an inbound encrypted message has no
readable snippet either); the search indexer re-reads the new bytes from the
Email `updated` change row. A prepared-but-unencrypted message (signed earlier,
un-sent, re-sent with Encrypt) is encrypted without being signed twice; an
already-encrypted one is left alone. **No downgrade:** any failure fails the
submit, the draft's bytes are untouched.

### 3. Refusals, before anything is written

`PlanSubmit` (SELECT-only) refuses with SetError `cannotEncrypt`:

- **Bcc.** A Bcc header, or any envelope recipient the To/Cc headers do not name.
  One ciphertext carries one session-key packet per recipient naming each
  recipient's key id, so Bcc'd people's key ids would be readable by everyone.
  Per-Bcc ciphertexts are not built in this version.
- a recipient without an eligible key (all named), or whose key **changed** (§5);
- a sending identity with no bound key (encryption signs and always includes the
  sender), or a server with no crypto process.

`RecipientKey/lookup` (urn:mailroom:pgp) tells a client beforehand: per address
`usable`, `fingerprint`, `source`, `wkd`, `keyChanged`, `pinnedFingerprint`;
never key bytes. With `wkd` true (default) an address with no eligible stored key
gets a bounded WKD lookup (10 s per fetch, 15 s total, 10 per call, 4 at a time,
through the SSRF-guarded egress client); a key found is stored (source `wkd`) so
that Submit, which never touches the network, uses the same key.

### 4. Which key an address gets (the trust rules)

A stored key is **eligible** only if all hold at send time:

1. **Source and authentication.** `manual` (the user imported it), `wkd` (served
   by the recipient's own domain over HTTPS), or `autocrypt` **and
   `contact_keys.authenticated`**. The column is set at ingest when the message
   that carried the header was authenticated for its From domain: a DMARC pass
   bound to that domain under the topmost trusted Authentication-Results
   (`ml.TopmostTrustedAuthResults(...).DMARCPassFor`, judged at the provider's
   received time with `ml.TrustedAt`), or a local DKIM pass that is aligned and
   displayable (ADR-036, live messages only). Existing rows are false: nobody
   checked. `keyserver` and unknown sources are not eligible.
2. **Usable.** Readable, no private material, unexpired and unrevoked primary
   key, user id and encryption subkey.
3. **Names the address.** A valid, non-revoked user ID whose email equals the
   recipient address (case-insensitive). (`Key.CoversAddress` stays advisory for
   *storing* a key; for *encrypting* it is a gate.)

Among eligible keys, **source rank decides, then recency**: imported > WKD >
authenticated Autocrypt. A higher-trust key is never displaced by a lower one,
whatever the dates.

### 5. Trust on first use: the pin

The fingerprint actually encrypted to is recorded per (user, address) in
`app.contact_key_pins` (migration 00068; RLS forced, in the isolation inventory)
in the same transaction as the send. If the key resolved on a later send differs
from the pin, the send is **refused** (`cannotEncrypt`, both fingerprints in the
description; `RecipientKey/lookup` reports `keyChanged: true`) until the user
confirms. A key the user **imported** may replace the pin, because importing is
an explicit act. Confirming is `ContactKey/decide` (ADR-039): "trust" imports the
held key as `manual` and re-pins; "keep" marks it refused.

### 5a. Sources: the import (amendment)

Until this amendment nothing let the user *add* a correspondent's key: it arrived
by WKD or authenticated Autocrypt, and `manual` was reachable only by trusting a
key already held. `ContactKey/import` (`{accountId, address, publicKey}`) is the
deliberate route: an ASCII-armored (or base64 binary) public key becomes an
eligible `manual` key under the contact that owns the address, and replaces the
pin. It is refused, storing nothing, when it carries secret-key material
(`secretKeyRefused`), has more than one primary key or is unreadable
(`invalidKey`), is over 256 KiB (`tooLarge`), is expired or revoked
(`keyExpiredOrRevoked`), has no encryption subkey (`noEncryptionSubkey`), or has
no valid user ID naming the address (`keyDoesNotNameAddress`, the same gate as
rule 3 above: an imported key becomes the pin, so it must name what it is the
pin for). An address the user sends as is refused too (`ownAddress`): it is
never a correspondent, so there is no contact to keep the key under.
`authenticated` has no meaning for a `manual` key and is stored false.
`dryRun: true` runs every rule and returns the fingerprint and user ID while
storing nothing, which is how the web client shows both before the user
confirms (the browser has no OpenPGP parser).

### 6. Out of scope

Protected headers (RFC 9788): the Subject and recipients travel in the clear and
the UI says so. Autocrypt `prefer-encrypt=mutual` stays off. S/MIME (2.x).
Per-Bcc ciphertexts. Encrypting drafts. Searching encrypted mail (ADR-037).

## Consequences

- An encrypted Sent copy has a blank snippet and is not searchable by body.
- A user whose correspondents only ever send unauthenticated Autocrypt keys
  (no trusted Authentication-Results configured and no aligned DKIM) cannot
  encrypt to them until they import the key or the correspondent publishes WKD.
  That is the intended cost: it is exactly the case where the key cannot be
  attributed to the address.
- Pins accumulate one row per address encrypted to; they are not exported (the
  next encrypted send rebuilds them).
- Residual: a key a correspondent legitimately rotates is refused until imported
  (see §5); and WKD trusts the recipient's domain's web server over HTTPS, which
  is as strong as that domain's TLS and control.

## References

- ADR-024, ADR-036, ADR-037; RFC 3156 §4, §6.1;
  Autocrypt Level 1 (header must match From; here, additionally, authenticated)
