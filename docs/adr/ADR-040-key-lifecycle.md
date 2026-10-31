# ADR-040: PGP key lifecycle: merge on renewal, two-year expiry, retire vs revoke, WKD serves revocations

**Status:** Accepted (v1.93 increment A; amended at increment B1 and at
increment C1; builds on
ADR-024, ADR-037, ADR-038 and ADR-039)

## Context

Until v1.93 Mailroom handled key expiry only after it had happened. An expired
recipient key was skipped by encrypt resolution (ADR-038), and a signature by an
expired key read `bad`. There were four gaps behind that:

1. **A renewed correspondent key stayed expired forever.** On a fingerprint
   conflict, `store.UpsertContactKey` advanced `fetched_at` and never touched
   `public_key`. A correspondent who extended their key (same fingerprint, new
   self-signature) kept the old, expiring copy in Mailroom whichever route the
   renewal arrived by. `ImportContactKey` had the opposite fault: it replaced the
   stored copy outright, so pasting an old export could strip a revocation.
2. **Mailroom-made keys never expired.** `pgp-generate` set no
   `KeyLifetimeSecs`.
3. **Nothing wrote `decrypt_only` or `revoked_at`.** Every reader of the two
   columns existed (signing refuses them, WKD and Autocrypt skip them, decrypt
   keeps using them), but no code path set either one.
4. **An expired own key was still advertised.** The outgoing Autocrypt header
   carried the bound key whatever its packets said.

## Decision

### 1. A re-sighted key is MERGED, never replaced

`pgpkeys.Merge(stored, incoming)` combines two copies of the same primary key the
way GnuPG's import does. The result is the union of user IDs, subkeys and
signatures, keeping only self-signatures that verify against the primary key.
Expiry and revocation are then whatever the merged packets say; go-crypto
computes that from the newest self-signature on every read.

- A renewal extends usability, because its newer self-signature is the one in
  force.
- A replayed older copy cannot shorten usability: everything it carries is
  already held or is older.
- A revocation survives any later merge. A union never removes a signature.
- A forged self-signature, one that does not verify, is dropped. A copy claiming
  a later expiry without the private key therefore extends nothing.
- Third-party certifications, user attributes and unknown packets are dropped.
  Nothing in Mailroom reads them, and keeping whatever a stranger attaches would
  let one inflate a stored key past `MaxKeyBytes` and block every later renewal.

The function is pure, works on public keys only, and lives in `internal/pgpkeys`
beside `Parse`, in the application rather than the crypto process (ADR-024 keeps
only private-key operations there).

**Every route that stores a contact key goes through it.** Autocrypt ingest, WKD
discovery (`RecipientKey/get` and `mailroom-admin key-discover`) call
`UpsertContactKey`; `ContactKey/import` calls `ImportContactKey`. Both merge into
the held row under `FOR UPDATE`, in one savepoint with the change row. A copy
that cannot be merged (unreadable, a different primary key, a result too large
or unreadable) leaves the stored key exactly as it was. That is the pre-v1.93
behaviour, so ingest is never failed by a bad header.

**Change rows.** A merge that changes the stored bytes writes the Contact
`updated` row, the same row `ImportContactKey` and `TrustContactKey` write for a
key change (Contact.keys is part of the synced Contact). A merge that adds
nothing writes no row and no `public_key` update. `ImportContactKey` writes its
own row in every case, so its merge adds none: one logical change, one row.

**Pins.** Same fingerprint means same pin. Nothing in the merge touches
`app.contact_key_pins`, so a renewal never reads as `keyChanged` (ADR-039).

### 2. New keys expire after two years

Decision: keys should expire.
`pgp-generate` creates keys whose primary key **and encryption subkey** expire
`pgpwire.DefaultKeyLifetimeDays` (730) days after creation. That is one named
constant. go-crypto's `NewEntity` applies the lifetime to the primary key only,
so the subkey binding is re-made with the same lifetime. `--expires-days N` on
the CLI and `expires_days` on the wire override it, with 0 meaning never and
`MaxKeyLifetimeDays` (ten years) as the cap. Existing keys are untouched.

"Usable until" is `pgpkeys.Validity.UsableUntil`: the earlier of the primary
key's expiry and the encryption subkey's. A subkey can expire first, and it is
the subkey that correspondents encrypt to. The report, the WKD rule, the
Autocrypt rule and (in increment B) the warnings all use this one function.
`pgpkeys.ExpiryWarningWindow` (30 days) is the one warning threshold.

### 3. Retire and revoke are different verbs

- **`pgp-retire`** tells Mailroom "I have stopped using this key". It is a store
  write only, with no private key and no crypto process. It sets
  `decrypt_only = true` and clears `wkd_published` in one transaction, with a
  Key `updated` row and Identity rows for whatever an Identity projects that
  moved. The key keeps decrypting forever (ADR-037). If the key is still an
  identity's bound default, the command refuses unless `--unbind` is given; with
  it, the binding is cleared in the same transaction. Retiring tells
  correspondents nothing.
- **`pgp-revoke`** tells the world "nobody should use this key", and it cannot
  be taken back. A crypto-process verb produces a real OpenPGP key revocation
  signature (reason `compromised`, `superseded` or `retired`) and stores the
  revoked public key. It sets `revoked_at` and `decrypt_only`, and the key still
  decrypts. The command names the key twice: `--confirm` must be the last 8 hex
  digits of the fingerprint of the key the daemon resolved, so a wrong key id is
  refused rather than revoked.
- **`pgp-extend --days N`** is a crypto-process verb that re-signs the primary
  key's user-ID self-signatures and every live subkey binding with expiry now+N
  days. Each new signature is strictly later than the one it supersedes, so it
  wins in a correspondent's merged copy. The fingerprint is the same, so no
  correspondent sees "key changed". The re-sealed private half carries the new
  signatures too, so signing keeps working after the old date. The verb works on
  a key that never expired (it then sets an expiry). It refuses N ≤ 0, N above
  the ten-year cap, and a revoked key. The artefact is checked before it is
  stored: same fingerprint, and usable until exactly the requested second.

### 4. WKD keeps serving a revoked key, after the current one

`app.wkd_public_key` used to return only the newest live key, so a revoked key
disappeared from the directory. A correspondent who had cached it then had
nothing to fetch that said otherwise. draft-koch-openpgp-webkey-service allows a
server to return revoked keys in addition to the current one. **Decision:** WKD
serves the newest published key that can still be encrypted to, followed by
every published **revoked** key at the address, as concatenated binary key
blocks. When only revoked keys remain, they are still served, so a client that
refreshes learns of the revocation. The human-facing key page shows nothing in
that case, because it would only be offering a dead key.

- An **expired** key is never served as the live key. Nobody can encrypt to it.
- A **retired** (decrypt-only, unrevoked) key is not served. Retiring means
  "stop advertising", not "tell the world". A key retired first and revoked later
  therefore stays unpublished. Publish it again before revoking if the
  revocation should be served.
- Revoking does not clear `wkd_published`. `pgp-unpublish` withdraws the
  revocation too, if an operator wants that.
- Migration 00070 is `CREATE OR REPLACE` with the same signature. The definer
  keeps its owner, ACL and column grant. It returns at most 8 rows. Expiry is
  decided in Go (`store.selectWKDKeys`), because it lives in the packets, not in
  a column.
- Unpublishing or retiring an old key clears the identity's WKD hash only when
  no other key is still published there. Before this change the hash was always
  cleared, so withdrawing the old key after publishing its replacement also took
  the replacement out of WKD.

### 5. Autocrypt does not advertise a lapsed own key

The outgoing Autocrypt header uses the identity's bound key
(`SigningKeyForIdentity`, which already excludes retired and revoked rows). Its
packets are now read as well. A key that cannot be encrypted to (expired, its
encryption subkey expired, or revoked in its packets) is not put in the header,
and a key whose primary has lapsed is not asked to sign. The send still goes
out. An encrypted send with a lapsed sender key is refused with
`ErrEncryptNoSenderKey`, because the crypto process always encrypts to the
sender too. A key whose packets cannot be read keeps the old treatment, so a
store anomaly is not turned into silently unsigned mail.

### 6. Report first

`mailroom-admin pgp-report` (and `--keys-only`, which skips the message scan)
prints own keys (all, and live) and held contact keys by bucket: expired, <30 d,
<90 d, later, never, revoked, and no-encryption-key. It also shows the
unreadable count and the number examined, so a run that finds nothing can be
told apart from one that holds no keys. This is read-only, and it is how the
30-day threshold gets checked against real data.

## Consequences

- Correspondents' renewals now take effect by every route, and an attacker who
  can send mail (and so put a key in an Autocrypt header) cannot strip a
  revocation or shorten a key by replaying an old copy.
- Keys made from now on stop working after two years unless extended. The
  client-side warnings that make that visible are increment B.
- ~~A JMAP Identity's signing facts (`signingKeyId`, shown as "Will sign") still
  follow the columns and not the packets' expiry.~~ Closed by the amendment
  below (increment B1).
- The WKD response can be several concatenated key blocks. Clients that
  implement the draft handle this. A client that reads only the first block
  still gets the live key, because it is always first.

## Amendment (v1.93 increment B1): the JMAP surface

### 7. `signingKeyId` reads the packets, as a read-time projection

`Identity.signingKeyId` is now nil when the bound key's own packets say it can
no longer sign (primary expired, or revoked in its packets), using
`pgpkeys.OwnKeyFitness`, the function compose already uses to decide whether to
sign. The two answers are one function, so "Will sign" cannot promise a
signature the send path will not make.

The alternative was a separate `signingKeyUsable` property with `signingKeyId`
left on the columns. It was rejected because it keeps the lie in the property
every existing client already reads, and moves the fix into a property no
client reads yet.

**No change_log row, and why that is correct.** Nothing stored changes when a
key's date passes. The projection is a function of (stored rows, now), like
`UserKey.expiresAt` and `expiringSoon`. A change row records a mutation; there
is none to record, and inventing one would need a clock-driven writer that
appends rows nobody caused. The consequence is that a client which caches
Identity objects and syncs only through `Identity/changes` keeps the old
`signingKeyId` until it next fetches the Identity. The web client refetches on
load, and B2's warnings read `UserKey.expired` and `expiresAt` directly, which
are always current. A key that is extended or revoked does write rows (Key, and
Identity where the binding's projection moves), so every state change a person
causes is synced.

### 8. Expiry on the read side

`UserKey/get`: `expiresAt` (`Validity.UsableUntil`, null for never),
`expired`, `usable` (columns live, and `OwnKeyFitness` advertise),
`expiringSoon` (usable and within `pgpkeys.ExpiryWarningWindow`), `canSign`
(columns live, a private half, and `OwnKeyFitness` sign). The one threshold is
`pgpkeys.ExpiryWarningDays` (30), sent to clients as the capability's
`keyExpiryWarningDays`. `ContactKey/get` gives the same facts for held
correspondent keys. `RecipientKey/lookup` gives `expiresAt` and `expiringSoon`
for the chosen key, and `reason: "expired"` with `expiredAt` when the only
eligible keys (ADR-038 rules 1 and 3) have lapsed.

### 9. Signatures by an expired key: `bad`, with a reason

ADR-039 §6 put "an expired or revoked key" in `bad`, and a later change kept that. This
amendment keeps the state and adds a reason. go-crypto returns `ErrKeyExpired`
only after the signature has verified over the content, in both the detached
and the embedded readers (`checkMessageSignatureDetails` runs after
`VerifySignature` succeeds). So an expired key is not altered content. Such a
signature is `bad` with `reason: "keyExpired"`, the expired key's fingerprint,
`signedAt` (the signature's creation time), `keyExpiredAt` and
`validWhenSigned`.

- **It is not upgraded to `verified`, even when it was valid when it signed.**
  The signature's creation time is the signer's claim, and anyone holding a key
  can backdate one. `validWhenSigned` is shown as context ("signed on 3 May with
  a key that expired on 1 June"), never as a verdict.
- **Only an attributable key earns the reason.** The expired key must pass the
  same gate the verified path uses (`heldSigner.attributable`): the user's own
  key naming the From address, or a held contact key that names it (read just
  before the key's expiry) from an eligible, unrefused source. An attacker's own
  throwaway, already-expired key, an unauthenticated Autocrypt key, a refused
  key, or a key that does not name From gets a plain `bad`. "Signed with a key
  that has expired" is a benign story, so only a key we would otherwise have
  believed may tell it.
- A weak hash is refused before the date is examined, so an expired key can
  never turn a SHA-1 signature into a softer reason.
- The crypto process reports the embedded case with new optional wire fields
  (`key_expired`, `expired_signer_fingerprint`, `signed_at`,
  `key_expired_at`). An older application reading a newer crypto process sees
  only `bad`.

### 10. Own-key management methods

`UserKey/generate`, `/import`, `/set`, `/extend`, `/revoke`, `/export` and
`ContactKey/get`, `/set`. Private-key operations go through `KeyCustody`, the
crypto process's verbs as an interface in `internal/api` (the package still
does not import the wire; `cmd/mailroom` adapts `pgpwire.Client`). Bind,
publish and retire are store writes, as in `mailroom-admin`. Rules:

- One kind of change per `UserKey/set` update (with one exception: retire with
  unbind), so each update is one store write with one savepoint and its
  documented rows. A write that changes nothing appends no row:
  `BindKeyToIdentity`, `UnbindUserKey`, `PublishKey` and `UnpublishKey` now
  detect their own no-ops under `FOR UPDATE`.
- Publishing is refused when WKD is not configured, or when the bound address
  is not at `MAILROOM_WKD_DOMAIN`. Either would otherwise commit
  `wkdPublished: true` for a key no correspondent can fetch.
- Unbinding a published key is refused (unpublish first, or retire).
- Export returns the wrapped key inline in the method response, not as a blob.
  The blob store is persistent mail storage. A single-use blob would be a second
  copy of the key on disk with a deletion to get right, and a JMAP response
  already happens exactly once.
- Generate, import and export spend a per-user token (burst 5, then one a
  minute). Revoke is never limited, because a compromised key must always be
  revocable. Extend is cheap and unlimited.
- `ContactKey/set` destroy removes the key and any pin naming it for the
  contact's addresses, with one Contact `updated` row. (Since §11 the removal
  is a soft delete.) There is no
  `ContactKey/changes`: keys are part of the Contact, and Contact rows already
  carry their changes, except that a brand-new key from ingest or discovery
  writes none (unchanged since key publication).

## Amendment (v1.93 final scope, increment C1)

### 11. Key history: nothing is deleted, and a re-sighting revives

Migration 00071 (expand-only) adds `user_keys.retired_at` and `replaced_by`,
`contact_keys.removed_at` and `checked_at`, and `app.contact_key_events`
(user_id-led index, RLS forced, deny by default, in the isolation suite).
`change_log` cannot be the history, because it is compacted.

- **`ContactKey/set` destroy is a soft delete.** It sets `removed_at` and
  removes any pin naming the key, with one `removed` event and one Contact
  row, as before. Every reader that chooses a key excludes removed rows
  through one predicate (`store.notRemoved`): `ContactKeysFor` (encryption in
  `compose.ResolveRecipientKey` and `explainNoKey`, signature attribution in
  `pgp_signature.go`, `ContactKey/decide`, and `key-discover`'s de-duplication),
  `ContactKeysForMany` (Contact.keys and `ContactKey/get`), `AllContactKeys`
  (`ContactKey/get`, `pgp-report`), `ContactKeyByID` (refresh),
  `CountContactKeys`, `ExportContactKeys` (the export carries the keys the
  user holds), and Contact/query `hasKey`. `TrustContactKey` and
  `RefuseContactKey` refuse a removed key. Only the history readers
  (`*IncludingRemoved`, `ContactKeyHistory`) see removed rows.
- **A removed key that is seen again is REVIVED, not added as a new row.**
  The alternative was a new row under a new id. Reviving was chosen because
  the unique key (user, contact, fingerprint) already says it is the same
  key, and a second row would split one key's history in two. The revived row
  takes what a fresh row would have: the sighting's source and authentication,
  `refused` cleared, the incoming copy merged, `removed_at` cleared. It gets a
  `first_seen` event and a Contact `updated` row. The pin is not restored,
  because removal deleted it. The next encrypted send or verified signature
  pins as for a new key. So removing a key does not block it. A user who
  wants to stop trusting a key that keeps arriving uses "keep the old key"
  (refuse), not remove. This is the same behaviour as the hard delete before
  it: the key came back then too, only without its history.
- **Events.** One row per real change, in the savepoint of the change:
  `first_seen` (new or revived), `renewed` (a merge moved the end of
  usability later), `updated` (a merge changed the stored copy without
  extending or revoking it), `revoked_seen`, `pinned` (trust, or the import of
  a held key), `refused`, and `removed`. Each records its source (a key route,
  or `user`) and the key's usable-until after the change. A no-op writes no
  event and no change row. Trust and refuse now detect their own no-ops (until
  now they wrote a row every time). `ContactKey/import` writes one event per
  import, as it writes one change row.
- **`retired_at` is recorded, never invented.** Retire sets it when a live
  key retires, and revoke sets it when a live key is revoked. A key that was
  already retired keeps what it had. There is no backfill, so for keys
  retired before 00071 it is NULL, which means "not recorded". A unit test
  pins that the migration writes no row and gives the columns no default.
- `pinned` events are written for explicit decisions only. Trust on first
  use (compose's first encrypted send, the first verified signature) moves a
  pin without one, as it moves it without a change row.

### 12. Exact expiry

`UserKey/generate` takes `expiresAt` instead of `expiresDays` (at most one of
the two), and `UserKey/extend` takes it instead of `days` (exactly one).

- A **date** `YYYY-MM-DD` means the **end of that day in UTC**, 23:59:59Z.
  The client turns the date the user picked into this form and the server
  never guesses a timezone. A **date-time** (UTCDate, or RFC 3339 with an
  offset) is that instant, in UTC, truncated to the second (OpenPGP's
  resolution).
- It must be **at least one day** (`MinKeyLifetime`, 24 hours) and at most
  `MaxKeyLifetimeDays` (3650) from now. The API checks this for a clean
  `invalidArguments`. The crypto process checks it again as the authority.
  A date-only "tomorrow" always passes, because its end is more than 24 hours
  away.
- The wire carries the absolute instant (`expires_at`). The crypto process
  computes the packet lifetime as instant minus the key's own creation time.
  The self-signature and the subkey binding therefore say exactly that second.
  Generate now checks its artefact: the primary key and the encryption subkey
  must both expire at created + lifetime, or the key is not stored. Extend
  already checked its artefact. An older crypto process reading a newer
  request without `days` refuses it, because `days` must be 1 or more, so the
  mismatch fails closed.
- A key that expires inside the warning window is `expiringSoon` at once.

### 13. User IDs, and the unnamed-address warning

`UserKey/get` and `ContactKey/get` list `userIds: [{name, email, revoked,
primary}]`, with the primary first.

- **Only self-signed user IDs count.** `pgpkeys.UserIDs` reads the key through
  `readKeyBlock`, Merge's parser, which verifies every self-signature against
  the primary key and drops the rest. go-crypto then reads the normalised key
  for revocation and the primary choice. A user ID packet with no signature,
  or with a self-signature that does not verify, is never listed. (go-crypto
  alone would refuse the whole key over the second kind. Reading through the
  verifying parser is what lets the honest user IDs still be listed.)
- **Untrusted text.** Each name and email has control characters and Unicode
  format characters (category Cf: bidi overrides such as U+202E, zero-width
  characters) and U+FFFD removed. It is then trimmed and capped at 256 runes.
  At most 32 user IDs are listed. Clients render them as text.
- `UserKey.boundAddressListed` is `pgpkeys.NamesAddress` against the bound
  fixed address. That is the gate a correspondent's client applies before
  encrypting: a user ID that is not revoked, whose self-signature has not
  expired, with that email. It is null when the key is unbound or bound to a
  wildcard.
- **Binding warns, it does not refuse.** A `UserKey/set` bind to an address the
  key does not name succeeds, and `updated[id]` is `{boundAddressListed:
  false, warning: "addressNotInKey"}`. A key that does name the address gets
  `{boundAddressListed: true}`. Refusing would block the Kleopatra-made keys
  this was found with, which are legitimate and only mislabelled.

### 14. Replace

`UserKey/replace {id, expiresDays | expiresAt}` takes two steps, and the
second is atomic.

1. The crypto process generates the new key, **unbound**, for the old key's
   identity (its display name and address). It writes the Key `created` row.
2. **One savepoint** (`store.ReplaceUserKey`) binds the new key to the
   identity, publishes it when the old key was published and this instance
   serves the address (`MAILROOM_WKD_DOMAIN`), retires the old key, and sets
   `retired_at` and `replaced_by`. If the new key is not published and the old
   one was, the identity's WKD hash is cleared when nothing else is published
   there. Any other identity still defaulting to the old key is unbound. One
   change row is written per object that moved: Key (new), Key (old), and
   Identity.

A failure in step 2 leaves the old key exactly as it was. An injected
failure after the bind, after the retire and after the change rows proves
this, with a caller that swallows the error and commits. The new key then
exists unbound, and the error names it. It refuses a revoked key, a retired
key, a key that is not its address's signing key (bound once, then
superseded), and a wildcard binding. All of these are refused before
anything is generated. It spends the generate rate-limit token. WKD
afterwards serves the new key and not the old one. The old one stays
unserved even if revoked later, because a retired key is not published
(§4). The old key certifying the new one is out of scope (#31).

### 15. Refresh

`ContactKey/refresh {id}` re-fetches a held key over WKD.

- **The discovery path, not a new one.** It uses the KeyFetcher that
  RecipientKey/lookup uses: `internal/wkd` over the egress guard (https only,
  no private or loopback hosts, a key-sized cap, a timeout). It asks at the
  key's user ID addresses that are also its contact's, or else at the
  contact's addresses, at most three. Every key in the answer is read
  (`pgpkeys.SplitKeys`), because WKD may serve the current key followed by
  revoked ones (§4). Copies of the held key go through
  `UpsertContactKeyDetail`, which merges with `pgpkeys.Merge`.
- **Results:** `renewed`, `revoked`, `updated`, `unchanged`, `differentKey`,
  `notFound`, `unreachable`. `revoked` and `updated` are additions to the five
  first proposed, so that a revocation is never reported as a renewal and a
  change is never reported as `unchanged`. `unreachable` covers every case in
  which an address could not be reached. It is never folded into `notFound`
  or `unchanged`.
- **A different key is never a silent replacement.** It is stored (source
  `wkd`, a `first_seen` event and a Contact row). If the address has no pin,
  the held key is pinned first, so the new key surfaces as `keyChanged` and
  waits for ContactKey/decide.
- **Limits.** Per user: 10, then one per 30 s. Per domain across all users,
  which protects the stranger's server: 10, then one per 6 s. Per key: 15
  minutes, using `contact_keys.checked_at`, claimed by one conditional UPDATE
  before the network is touched. A refused call touches nothing. `checked_at`
  is bookkeeping that is not on the wire, so it writes no change row.
- **No background refresh from compose.** It was offered as optional. It is
  not built. `RecipientKey/lookup` runs as the user types, and a refresh from
  there would make a read path mutate state and spend the per-domain budget
  on recipients the user never sends to. The explicit action and compose's
  "expired on ..." line, which can call `ContactKey/refresh`, cover the need.

## References

- RFC 9580 §5.2.3.23 (reason for revocation), §5.2.3.13 (key expiration time).
- draft-koch-openpgp-webkey-service §3.1 (returning revoked keys).
- Code: `internal/pgpkeys/lifecycle.go`, `internal/pgpkeyd/lifecycle.go`,
  `internal/store/user_key_lifecycle.go`, `internal/store/contact_keys.go`,
  `internal/store/wkd.go`, `internal/compose/ownkey.go`,
  `cmd/mailroom-admin/pgplifecycle.go`, `cmd/mailroom-admin/pgpexpiry.go`,
  `db/migrations/00070_wkd_serves_revocations.sql`; for the C1 amendment
  `db/migrations/00071_key_history.sql`, `internal/store/contact_key_history.go`,
  `internal/store/user_key_replace.go`, `internal/pgpkeys/userids.go`,
  `internal/pgpkeys/split.go`, `internal/api/user_key_replace.go`,
  `internal/api/contact_key_refresh.go`.
