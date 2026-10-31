# ADR-037: PGP/MIME mail is decrypted on open, in the crypto process, and its plaintext is never persisted

**Status:** Accepted (Forgejo #47 increment 6b; deviates from spec §7 line 148
and §9's threat table; amends ADR-024 §4 and the LUKS consequence in its
Consequences; resolves ADR-027 §6's deferral for mail that can be opened)

## Context

Spec §7 (line 148) and §9 assume that because PGP keys are server-side,
encrypted bodies are decrypted server-side **and indexed normally**, so the
search index holds the plaintext of encrypted mail, and the index directory
needs volume-level encryption (LUKS/dm-crypt) as a deployment requirement.
ADR-024 repeated that and made the LUKS gate land "with the decryption
increment". The 2026-08-21 checklist on #47 (item 7) made it blocking on 6b's
PR, and it was still red on the test host when this was written: no
dm-crypt anywhere.

The requirement was the simplest implementation that relies on existing
packages (the v1.92 plan on #47). The simplest design that is also safe is one
where plaintext never reaches anything that persists. Then the index never
holds it, and the LUKS-on-index gate is not needed for this feature.

The threat model changes shape at 6b: `user_id` on the crypto socket is
asserted by the application, not authenticated (ADR-024 §4 as amended), so an
application compromise turns a `decrypt` method into an oracle for every
user's encrypted history. That is a strict widening, and it is why everything
below is bounded, opaque, rate-limited and audited.

## Decision

### 1. ProtonMail go-crypto does all OpenPGP work; Mailroom writes glue

The crypto process (`internal/pgpkeyd`) gains one verb, `decrypt`. It parses
nothing itself: `openpgp.ReadMessage` unwraps the session key, decrypts and
checks integrity. Mailroom chooses which keys to offer, pins the library's
options, caps the output, and decides what to tell the caller. RFC 3156
framing is split by `internal/pgpmime` (`SplitEncrypted`, `Rewrap`).

### 2. The `decrypt` verb, and the 2026-08-21 checklist

Input: user id (asserted), message id (asserted, for the audit row), purpose
(`body` or `attachment`), and the ciphertext. Output: the decrypted MIME entity
or one opaque error.

1. **One opaque error.** Every cryptographic failure (no key opens it, wrong
   key, MDC/AEAD failure, malformed packets, SED, plaintext over the cap, a
   user with no key) returns `decrypt_failed` with the fixed message "the
   message could not be decrypted". go-crypto's error text is never relayed,
   not even to the log: the log records a reason category in our own words.
   `TestDecryptFailuresAreIndistinguishable` asserts the full error string is
   identical across eleven failure shapes. Timing is not equalised; that is a
   residual (see Consequences).
2. **SED refused outright.** `InsecureAllowUnauthenticatedMessages: false` is
   set explicitly (it is also go-crypto v1.5.2's default), so a Symmetrically
   Encrypted Data packet (tag 9, no MDC, EFAIL's precondition) is refused.
   SEIPD with a verified MDC and AEAD packets are accepted. `CacheSessionKey`,
   `InsecureAllowDecryptionWithSigningKeys` and strict packet-sequence checking
   are pinned too. `TestDecryptRefusesSED` builds a genuine SED message, proves
   it opens with the insecure flag, then asserts the daemon refuses it.
3. **Bounds.** `MaxDecryptRequestBytes` (64 MiB of JSON) is its own constant.
   The decompressed plaintext is capped at `MaxDecryptedBytes` (32 MiB) twice:
   go-crypto's per-layer `MaxDecompressedMessageSize` (plus a framing slack),
   and a hard `io.LimitReader` on the output that refuses rather than
   truncates. Integrity is checked only at EOF, so a read stopped at the cap
   never returns. Compression nesting is bounded by go-crypto's 32-layer packet
   reader; each layer is size-limited. Decrypts in flight are bounded to 2 per
   process, because one call holds roughly five times the plaintext at peak.
   The crypto container's default `mem_limit` rises from 128m to 384m.
   (32 MiB, not internal/mime's 64 MiB, for that memory reason; a larger
   encrypted message shows as "could not be decrypted".)
4. **Rate limit and a persistent audit trail.** A per-user token bucket in the
   crypto process (burst 120, 1 per second), checked before any key is
   touched; a refusal has its own code (`rate_limited`) because it is not
   cryptographic. Every call that reaches the keys writes one row to
   `app.pgp_decrypt_audit` (see §3) **before** anything is returned; if the row
   cannot be written, no plaintext leaves.
5. **Retired keys still decrypt.** Retirement semantics invert compared with
   Sign: `decrypt_only` and revoked keys are offered to the library like any
   other. `TestRevokedKeyStillDecrypts` encrypts to a key, revokes it with a
   real OpenPGP revocation signature, marks it `decrypt_only` and `revoked_at`,
   proves Sign now refuses it, then decrypts the old message with it.
6. **Leak probe.** `MethodDecrypt` is in the registry with a succeeding probe in
   `TestProbeTableCoversEveryMethod` / `TestNoAPIPathReturnsPrivateKeyMaterial`.
   `TestDecryptLeaksNeitherSessionKeyNorPrivateKey` builds a message whose
   session key the test knows and asserts neither it nor any private-key window
   appears in responses or the daemon's log, with a control proving the search
   can see a session key.
7. **Plaintext is never persisted.** See §4.
8. **`SO_PEERCRED` re-evaluated and declined.** See §6.

### 3. The audit row is written by the crypto process, not the application

The crypto process already connects to Postgres as `mailroom_app` (StoreVault).
That decides it: an audit row written by the application is one a compromised
application, the exact attacker 6b adds, would not write. Written by the
crypto process in the same request, before plaintext is returned, every
decryption leaves a row. The grant makes it stick: `SELECT, INSERT` only, no
`UPDATE` or `DELETE`, like `app.audit_log` and `app.pgp_root_check`. Both
processes share the role, so a compromised application can add noise but not
erase a decryption. Rows go with their user (the hard-delete cascade).

The row carries no content: message id (no foreign key, so the trail outlives
the message), purpose, outcome (`decrypted`/`failed`), and the decrypting key's
fingerprint. Failures are recorded too: many failures for one user is what an
oracle attempt looks like from here. "Decrypted N messages for user U" is
`store.PGPDecryptSummary`. Rate-limited refusals touch no key and are logged,
not audited.

### 4. Plaintext is never persisted, and that replaces the LUKS-on-index gate

Decryption happens in exactly two places, both on a request path:

- **Email/get**, only when a body property (`bodyStructure`, `textBody`,
  `htmlBody`, `attachments`, `bodyValues`, or `pgpSignature`) is
  requested. A list view never decrypts. Asking only for `encrypted`/`decrypted`
  reads the stored bytes and decrypts nothing.
- **A part download or preview** inside a decrypted message (§5).

The decrypted entity is rendered through the same `buildEmailBody` pipeline as
ordinary mail (sanitised HTML, quarantine-aware `bodyValues`) and dropped. No
column, blob, search document, snippet, ML feature or cache receives it. The
search indexer, the snippet (`preview`), the classifier and rules all run at
ingest on the ciphertext message, which is unchanged. A decrypted part cannot be
attached to a draft (its plaintext would land in a draft blob). Decrypted part
responses carry `Cache-Control: no-store` and no ETag.
`TestDecryptOnOpenWritesNothing` snapshots every `app` table's row count, the
change-log high-water mark and every blob file (size and mtime) around an
Email/get and a part download, and searches every column of every table for a
marker that exists only inside the ciphertext, with a control proving the
search finds it when present.

**Headers inside the ciphertext are not trusted.** Encryption authenticates
nothing: anyone can encrypt to a published key. `pgpmime.Rewrap` copies only
the entity's MIME content fields (Content-Type, Content-Transfer-Encoding,
Content-Disposition, Content-ID, Content-Description, Content-Language).
Identity, trace and authentication headers always come from the outer,
provider-delivered message, so a `From:` or `Authentication-Results:` hidden in
the ciphertext cannot spoof the sender or defeat the one-From rule (#754) and
the local-DKIM guards (ADR-036). **Protected headers** (RFC 9788 "memory hole",
an inner Subject meant to replace the outer one) are out of scope and not
honoured; honouring them is a possible later feature that needs its own
security review.

**The deviation from spec §7**, stated plainly: the spec's index holds the
plaintext of encrypted mail by design; here it never sees it. **The cost:**
encrypted mail is not searchable by content, and its snippet stays whatever
ingest saw (the ciphertext message's placeholder text). **Why:** it removes a
deployment requirement (LUKS on the index) that was blocking and unverified, it
keeps the oracle's output inside one request's memory, and the measured
population is small (the first encrypted test mail was only just being created).
Searching encrypted mail can be revisited with its own ADR, together with the
at-rest requirement it would bring back.

### 5. Attachments inside a decrypted message are scanned on demand

ADR-027 §6 forced every part of an encrypted message to `unknown` at ingest,
because clamd sees ciphertext, and said the scan "moves to wherever plaintext
exists once #47's decryption lands". Plaintext now exists only at request time,
so that is where the scan is:

- Parts of a decrypted message have partIds `e.<path>` and blobIds
  `<messageBlobId>.e.<path>`, so they never collide with the stored ciphertext
  parts or their stored verdicts.
- A download or preview decrypts again, finds the part, and judges it by
  ADR-027's rules unchanged: only attachment leaves are scanned and gated
  (`mime.IsAttachmentLeaf`, as at ingest); a truncated walk forces `unknown`
  (`attachscan.ForcedUnknown`), and so does a part inside an encrypted subtree
  of the decrypted entity, such as a second encryption layer or a forwarded
  encrypted message (`attachscan.EncryptedPaths`, the subtree-scoped rule
  ingest applies since #405); zero bytes is
  `unknown`; no scanner is `unscanned`, which does not block; a scanner error is
  `unknown`, which blocks like `flagged`. A release the user already made for
  the same bytes (keyed on the content digest) is honoured.
- Nothing is cached: not the plaintext, not the verdict. `fetchAllBodyValues`
  withholds the value of attachment-leaf text parts of a decrypted message, so
  the only door to them is the scanned download.
- The ciphertext message's stored parts are recorded exactly as before: #405
  (PR #785) scoped the ingest rule to the encrypted SUBTREE, which stays
  `unknown` at ingest, while plaintext siblings and the parts of a message that
  merely forwards an encrypted one are scanned normally. This ADR builds on top
  of that: the encrypted subtree's real attachments become reachable, scanned,
  through the decrypted tree. Mail that cannot be decrypted is unchanged.

There is no release flow for a decrypted part that scans `flagged`/`unknown`
and has no stored row; that is a follow-up.

### 6. `SO_PEERCRED`: re-evaluated, declined again

ADR-024 §4 declined `SO_PEERCRED` while both processes share a uid; checklist
item 8 asked for that to be reconsidered now that a verb returns plaintext.
Measured: both containers run the same image as the same `mailroom` user, and
the socket volume is mounted into the application and the crypto process only.
Splitting uids and checking the peer's uid would authenticate the **process**,
and the threat 6b adds is a compromised application, which is that process and
would pass the check by construction. It would defend only against a third
process on the socket volume, and there is none. What actually bounds the
oracle is §2's rate limit, the crypto-process-written append-only audit trail,
the opaque error and the caps. ADR-024 §4 is amended to say so.

### 7. Inline PGP and other out-of-scope items

Inline PGP (`-----BEGIN PGP MESSAGE-----` in a text body) is shown as today.
Signatures inside the OpenPGP layer (RFC 3156 §6.2) were not verified in
1.92.0-rc.1/rc.2; since the rc.2 test they are checked in the same `decrypt`
read against candidate public keys the application sends, and reported as a
status only (ADR-039 section 6; the crypto process still holds no correspondent
keys, the audit row is unchanged, and a bad signature still returns the
plaintext). A PGP/MIME `multipart/signed`
inside the decrypted entity (§6.1) is verified with `pgpverify` and exposed as
`pgpSignature` (first shipped as `encryptedSignature` in 1.92.0-rc.1; folded into
the one property for all signed mail by ADR-039). Encrypt, the key-changed UI and Autocrypt
`prefer-encrypt=mutual` are separate work.

## Consequences

- **Encrypted mail is not searchable**, and its snippet is a placeholder. The
  web client says so in the reading pane line ("the decrypted text is not
  stored, so it does not appear in search").
- **Every open costs a decryption** (and one per inline image or attachment
  fetched). The rate limit is sized for that; a heavy thread with many images
  can spend a few dozen calls in a burst.
- **Deployment:** the crypto container's `mem_limit` default is 384m. A release
  ships images only, so an existing deployment must raise
  `MAILROOM_CRYPTO_MEM_LIMIT` (or update its compose file) itself; at 128m a
  large encrypted message can OOM the crypto process. The figure is an
  estimate, not a measurement on real encrypted mail.
- **Residuals:** response timing is not equalised across failure kinds; a
  restart refills every rate-limit bucket; the audit trail records what the
  application asserted (message id, purpose), not what it proves.
- **The honest framing is unchanged** (ADR-024): Mailroom is not end-to-end
  encrypted. The server decrypts to show the message, and no user-facing
  string implies otherwise; a test checks the reading-pane strings for that.

## References

- #47: the 2026-08-21 checklist (carried over from #301) and the v1.92 plan
- ADR-024 (crypto process), ADR-027 §5b, §6, §8 (attachment scanning)
- #405 (blanket quarantine of encrypted mail), #263 / #301 C4 (interop
  circularity, closed for the key shape by the GnuPG decrypt interop test)
