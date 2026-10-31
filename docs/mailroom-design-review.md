# Mailroom — Pre-Implementation Design Review
### Principal-engineer / security-architect review of spec v0.3 + feature matrix

Reviewed: `mailroom-spec.md` (v0.3), `mailroom-feature-matrix.md`. Library/protocol claims were verified against current sources (July 2026) where flagged; residual uncertainty is called out explicitly.

**Verdict up front:** the architecture is coherent and the resolved decisions are mostly defensible. But the spec has four genuine one-way doors that are *not yet specified precisely enough to build against* — message identity, the JMAP state model, the crypto/key hierarchy and its honest threat model, and the isolation mechanism. Two "resolved" claims are internally contradictory as written (static binary vs. embedded Tantivy; "admin cannot read mail" vs. server-side master key). And the v1.0 scope is the single largest project risk. Details below.

---

## 1. Risk register (ranked)

**Legend:** 🚪 = one-way door (expensive/impossible to change after real user data exists). 🔁 = two-way door.

### P0-1 🚪 Message identity & dedup key is underspecified and currently wrong
- **Problem (spec §4, §14):** dedup by `Message-ID` (+ `X-GM-MSGID` for Gmail). `Message-ID` is sender-controlled, optional, sometimes absent, sometimes reused, and trivially spoofable. Two *different* messages carrying the same Message-ID (malicious, or a mailing-list copy with rewritten body/footers vs. the direct copy) will silently merge into one canonical message with two provenances — you'll show one body and write flags back to both. There is also no statement that dedup is scoped per-user (in multi-user it must never cross users).
- **Impact:** the canonical `messages` row is the FK hub of the entire schema (labels, triage scores, threads, provenance, blobs). Changing message identity after tens of GB of user data means rewriting every table and every client cache. This is the most expensive thing to get wrong.
- **Recommendation:** canonical dedup key = **SHA-256 of the raw RFC 5322 bytes** (or of a canonicalized form if you want to tolerate transport-mangled trailing CRLFs), scoped `(user_id, content_hash)`. `Message-ID` becomes a *grouping hint* for threading and Gmail label-copy collapse, never the identity. Where Gmail delivers the same bytes under multiple labels, content hash collapses them for free. Bonus: content-addressed blobs give you attachment/message dedup on a large import.
- **Decide before any code.**

### P0-2 🚪 Server-side keys: the threat model must be stated honestly, and two spec claims contradict it
- **Problem (spec §9, §10, §7):** three statements can't all be true as written: (1) private keys are server-side, unwrapped by a master key the server can use at boot; (2) "admin cannot read any user's mail (enforced in schema + policy)"; (3) encrypted mail is decrypted server-side and indexed. Whoever controls the box and the master key can read everything, including PGP mail — admin blindness is a *software policy*, not a cryptographic property. The spec's security section implies more than the design delivers.
- **Impact:** if this ships ambiguous and is open-sourced, users will assume E2E-adjacent properties they don't have; that's a reputational and ethical failure mode, not just technical.
- **Recommendation:** keep the decision (it's the only way to get server-side search/ML over encrypted mail, which is the product's point), but (a) add an explicit "What an attacker gets" table to §11 (full server compromise = all mail plaintext-equivalent + ability to decrypt captured ciphertext + ability to sign as every user — see deep-dive A); (b) reword the admin claim to "role separation enforced in software and audited; not cryptographically enforced"; (c) note that the **search index contains the plaintext of encrypted mail** and is currently *missing from the at-rest-encryption list* — Tantivy has no native encryption, so index directories need volume/filesystem-level encryption (LUKS/dm-crypt or per-user encrypted mounts) and this must be stated as a deployment requirement.
- 🚪 because migrating later to client-held keys would invalidate the search/ML architecture entirely. Lock the honest version now.

### P0-3 🚪 JMAP-subset: no mature Go server library exists, and the current plan will quietly break the "third-party JMAP clients can connect" promise
- **Verified:** there is no production-grade Go JMAP *server* library. `foxcpp/go-jmap` is a WIP client+server lib targeting **draft-ietf-jmap-core-17** (pre-RFC), last meaningfully updated ~2023. `rockorager/go-jmap` is a maintained fork but **client-only** (it's what aerc uses). Stalwart's server is Rust. So your server side is hand-rolled no matter what — the question is *what you conform to*.
- **Problem:** "subset + extensions" without a conformance target drifts into JMAP-flavored REST. Real JMAP clients depend on hard invariants: the session object, opaque per-type **state strings with exactly-correct `/changes` deltas**, blob upload/download endpoints, result references, and `cannotCalculateChanges` fallback semantics. Retrofitting correct `/changes` onto a schema that never tracked per-type change sequences is a schema migration, not a patch. 🚪
- **Recommendation:** implement genuine RFC 8620 core semantics + RFC 8621 `Email`/`Mailbox`/`Thread`/`EmailSubmission` for whatever subset of methods you ship, with Mailroom extensions as proper capability URNs (`urn:mailroom:triage` etc.). You need the state/changes machinery *anyway* for your own offline clients — it IS your sync protocol; don't build two. Use aerc (via rockorager/go-jmap) as a free external conformance client in CI. If you decide this is too much, then explicitly delete the "third-party JMAP clients can connect" line from the matrix (§15) — either is fine; ambiguity is not.

### P0-4 🚪 "Embedded Tantivy" contradicts "static Go binary," and the binding's capability surface is unverified
- **Verified:** the viable binding is `anyproto/tantivy-go` — CGO with prebuilt Rust static libs (musl on Linux), production-proven in Anytype across platforms. But it is CGO: cross-compilation pain, a second toolchain in CI, and you get only the API surface anyproto chose to expose (their published API shows edge-ngram/ngram/jieba analyzers, batch add/delete, fast fields — it is *not* obvious that fuzzy/typo-tolerant queries, phrase queries, and highlighting are all exposed the way you need).
- **Impact:** typo tolerance is a headline v1.0 feature. If the binding can't express it, your options are ngram approximation (index bloat), forking the binding, or Bleve (pure Go, but meaningfully slower — a real concern at 24 users × tens of GB).
- **Recommendation:** week-one **spike with pass/fail criteria** before the search design is considered resolved: per-user index directories; fuzzy + prefix + phrase + field queries; highlighting; incremental delete/update; index build over a ~25 GB synthetic corpus with p95 < 100 ms; measure index size. Keep the `SearchBackend` interface, and own your **query AST** in Mailroom (never let Tantivy query syntax leak into the API — that's what makes this door two-way). Update §3 to say "single binary + one native library" or accept Bleve; don't claim both.

### P0-5 🚪 Multi-tenant isolation is asserted ("RLS"), not designed
- **Problem (spec §10, §14):** RLS is only as good as the session discipline around it. With a pooled connection, one missed `SET LOCAL app.user_id` = full-table exposure; an app DB role with `BYPASSRLS` or table ownership quietly disables policies.
- **Recommendation (the design, not just the slogan):** (a) app connects as a non-owner role, policies deny-by-default, `FORCE ROW LEVEL SECURITY` on every user table; (b) a single request-scoped transaction wrapper is the *only* way code gets a DB handle, and it requires a user identity to construct; (c) CI includes **negative tests** that attempt cross-user reads through every API endpoint and directly at the SQL layer; (d) blob paths derived from `HMAC(server_key, user_id)/...` so path construction bugs don't enumerate; (e) — the real backstop — **per-user DEKs mean a leaked blob or index-adjacent file is ciphertext under the wrong key**, converting whole classes of path bugs into non-events; (f) be honest that the **search index is the weakest isolation layer** (per-user directories + FS perms only, same process) and document it in §11. Crypto-level isolation is the control that survives bugs; RLS is defense-in-depth, not the headline.

### P0-6 🔁→🚪 `go-imap` v2 is still pre-stable; the sync engine needs an anti-corruption layer and a real test harness
- **Verified:** the go-imap README still marks v2 as "in development"; issues (client bugs, extension gaps) remain open into 2026; there is no stable v2.0.0 promise. v2 does expose CONDSTORE (`SelectOptions.CondStore`); confirm QRESYNC support at spike time rather than assuming.
- **Impact:** API churn mid-build, and — worse — subtle protocol bugs in the wild (the issue tracker literally has a "non-standard: interoperability with software violating the specs" label). IMAP sync correctness is where mail clients die.
- **Recommendation:** pin a commit and vendor; wrap the library behind your own `SyncSession` interface so churn doesn't ripple; and treat the **sync test harness as a v0.x deliverable**: Dovecot in CI as the reference server, plus property-style tests for UIDVALIDITY reset, EXPUNGE racing a FETCH, flag changes racing writeback, IDLE drop/reconnect, and a mailbox mutated mid-sync by a second client. Becomes a one-way door only if you let go-imap types leak through your codebase — don't.

### P0-7 Outbound is the scarce resource: Migadu daily send caps, bounces, and send-as validation are unhandled
- **Verified:** Migadu's plans carry hard **daily send/receive caps counted per account, not per mailbox** (historically ~100/day outgoing on the low tier, ~500/day Standard, ~2000/day Maxi; incoming similarly capped). The concurrent-IMAP cap remains publicly undocumented — ask the provider's support for a number in writing; until then a "one IDLE + small pool" plan is right.
- **Problem:** nothing in either doc handles (a) an **outbound budget** — Undo-Send queues, send-later, vacation autoresponders, and MCP agents all draw from a pool that can be exhausted by lunch, producing silent late failures; (b) **bounce/DSN handling** — sends to derived catchall aliases will bounce more than normal mail; DSNs must be parsed, threaded to the original message, and surfaced ("delivery failed"), or users simply won't trust send; (c) **send-as validation in multi-user** — `identities.allowed-sender` exists in §14 but the enforcement rule is unstated: the server must refuse any From not matching a verified identity/domain for that user, even though the provider would also reject it — defense against one user's config error burning another domain's reputation on a shared instance.
- **Recommendation:** add an outbound ledger per account (quota, spent, resets-at) with backpressure + user-visible meter; a DSN/NDR parser (RFC 3464) with thread association in v1.0 (it's not in either doc at all); explicit From-validation rule. Deliverability itself is fine — see deep-dive G.

### P0-8 🚪 Backup/restore is listed as a control but not designed; with envelope encryption, an untested restore is a data-loss machine
- **Problem (spec §11, §16):** "encrypted, off-box, restore-tested backups" — but the design creates a circular dependency the doc doesn't acknowledge: blobs and keys are useless without the KMS/master key, and the master key lives on the infrastructure being backed up. If the box dies and the master key wasn't escrowed *independently*, every backup is confetti.
- **Recommendation:** specify the **restore unit** = Postgres + blob store + wrapped key store + master-key escrow (offline copy — e.g., age-encrypted to an offline recovery key stored physically); the index is explicitly *not* backed up (it's a rebuildable cache — see deep-dive C, and that stance must be written down). Add a scripted, scheduled **restore drill** (restore into a scratch container, decrypt one message per user, run one search after reindex) as a CI/ops job. Migration tooling (see decisions register) is part of this: backups must record schema version.

### P1-9 Timers (snooze, send-later, undo-send) need a durable job model
In-memory timers lose mail actions on restart. One durable `jobs` table (fire-at, idempotency key, attempts, dead-letter), a single dispatcher with catch-up-on-boot for missed timers, and "Undo Send" implemented as *delayed submission* (same machinery as send-later — the matrix currently splits them across v1.0/v1.x; build the mechanism once in v1.0 and gate the UI).

### P1-10 The action queue's conflict/idempotency contract is unwritten
Every writeback action needs an idempotency key and a defined answer to "the world changed under me" (UID vanished because the phone client moved the message; flags changed both sides). The correct model: **store intents/deltas, not target states** — "+\Seen on message X" is idempotent and merges naturally with concurrent changes; "set flags to {...}" is a lost-update generator. Full matrix in deep-dive B.

### P1-11 Online-learning ML needs versioning, rollback, and an eval harness before it needs accuracy
Per-user online models will drift and can be poisoned by a bad week of behavior (or by mail content in adversarial features). Snapshot models on a schedule, keep N versions, expose "reset my model," and build the eval harness (replay held-out user actions, report ranking metrics) *before* tuning anything. Also decide the cold-start story concretely: T0 rules + sensible heuristics for week one, or the priority inbox will make a bad first impression on every new user.

### P1-12 MCP/agent keys: defaults decide the blast radius
The scope design (§13a) is good. Harden the defaults: no key gets `send`+`delete` together without explicit intent; `mail:delete` via agent = **soft-delete with a recovery window**, always; per-key rate and daily budgets mandatory (not optional); anomaly hooks (mass-delete, mass-send, off-hours spikes) feeding the audit log with alerting. A runaway agent with a broad key is currently your most realistic total-mailbox-loss scenario.

### P1-13 Crypto library reality-check (Go)
`golang.org/x/crypto/openpgp` is deprecated/frozen; the maintained path is **ProtonMail's go-crypto fork + gopenpgp**. S/MIME in Go is much thinner (the commonly-used pkcs7 package is archived; you'll be maintaining or forking something). The matrix prices PGP and S/MIME as the same v1.x line item; S/MIME is realistically 2–3× the work. Keep it in v1.x only if you accept that, else move S/MIME to v2. (Uncertain: current best S/MIME package in 2026 — verify at crypto-design time.)

### P2-14 Licensing
Emersion's stack is MIT; Tantivy MIT; tantivy-go MIT; go-crypto is BSD-style — no copyleft conflicts. The open decision is **Mailroom's own license** (AGPL-3.0 protects a self-hosted server product from silent SaaS forks; MIT/Apache maximizes adoption). Decide before the repo is public; relicensing after outside contributions requires CLAs or unanimous consent.

### P2-15 Gmail OAuth (v1.x) has a self-hosted-specific wrinkle
Gmail restricted scopes require Google app verification for published apps; the standard self-hosted answer is **each operator creates their own OAuth client** in their own Google Cloud project ("internal"/test mode). Document that explicitly in the provider profile docs or v1.x Gmail-live will generate endless support issues post-open-sourcing.

---

## 2. Gap analysis — what the docs don't say

Ordered roughly by how much pain the omission causes.

1. **Deletion semantics (user-facing, and interacts with dedup).** What does "delete" mean for a message with provenance in two accounts? Trash per-provenance or per-canonical-message? Auto-purge retention for Trash? Hard delete propagation to IMAP + blobs + index + ML features + audit? This touches the data model → decide pre-code.
2. **Bounce/DSN/send-failure pipeline** (P0-7) — absent from both docs entirely.
3. **Sent-mail handling:** most IMAP providers do *not* save on SMTP submission (Gmail does). You plan Sent `APPEND` — add dedup logic for providers that also self-append, or every Gmail-relayed send shows twice.
4. **Onboarding flow per user:** add-account wizard, credential validation, SPECIAL-USE folder detection (RFC 6154) with manual mapping fallback for nonstandard folder names, initial-sync progress UX, and what the user sees during a multi-hour backfill.
5. **Quota exhaustion behavior:** per-user storage quota hit mid-sync — pause sync + alert (never drop mail silently); provider mailbox full on writeback — surfaced how?
6. **Charset/MIME/EAI policy:** adopt a **golden corpus** of hostile MIME (the classic "MIME torture" sets + a large real-world import is a fantastic corpus) as a v0.x test fixture. Explicit policy needed for: malformed RFC 2047 headers, unknown charsets (replacement, never crash), unencoded 8-bit, nested `message/rfc822`, and SMTPUTF8/EAI addresses (verify Migadu's SMTPUTF8 support; if absent, define the compose-time error).
7. **Threading false merges:** JWZ subject-fallback across *accounts* plus content-hash dedup can weld unrelated conversations. Restrict subject-fallback (same account + same correspondents + time window), store thread membership as recomputable, and log merges so they're diagnosable.
8. **Draft conflict story:** draft edited in Mailroom and in the phone's client (via IMAP Drafts) — define winner or duplication rule now, since drafts are the one object users edit concurrently.
9. **Observability spec:** per-account metrics (sync lag seconds, IDLE connection state, action-queue depth, writeback failure rate), index lag, job-queue health, model version/feedback counts — plus a **user-visible "account health" panel** (users must be able to see "the Gmail sync is 3h behind" without reading Prometheus).
10. **Testing strategy** as a deliverable: Dovecot-in-CI sync harness (P0-6), JMAP conformance via aerc/rockorager client, RLS negative tests (P0-5), restore drill (P0-8), MIME corpus, and a load test replaying a large archive import on CI-sized hardware.
11. **Schema migration policy** once users hold tens of GB: name the tool (goose / golang-migrate / Atlas — any is fine), forward-only, **expand-and-contract pattern** with online backfill jobs for large-table changes (never a blocking `UPDATE` in a migration), migration checks already promised in CI (§16) — define what they check.
12. **ADRs:** add `docs/adr/` and record every decision in the register below as ADR-001…N. Cheap, and it's how the open-source version explains itself.
13. **Per-user data export** (v1.x in matrix): specify format now (mbox or maildir + JSON sidecars for labels/scores) since it constrains nothing but reassures everyone; export is also your escape hatch if a migration goes wrong.
14. **PWA push on iOS:** Web Push works only for installed-to-home-screen PWAs on iOS (16.4+), with OS-level throttling. Fine — but set expectations in docs so "notifications don't work on my iPhone" isn't issue #1.

---

## 3. Open-decisions register

### MUST decide before v1.0 code

| # | Decision | Options | Recommended default | Reversal cost |
|---|---|---|---|---|
| D1 | Canonical message identity | Message-ID · content-hash · composite | **SHA-256 of raw MIME, scoped per user**; Message-ID as hint (P0-1) | Extreme — FK hub |
| D2 | JMAP conformance level | true RFC 8620/8621 core subset · bespoke "JMAP-ish" | **True 8620 semantics** for Email/Mailbox/Thread/EmailSubmission; extensions as capabilities (P0-3) | High — state/changes retrofit = schema change |
| D3 | Per-type change tracking | global modseq · per-type sequence table · logical decoding | **Per-user, per-type monotonic change log table** (drives `/changes`, client sync, and index updates from one mechanism) | High |
| D4 | Blob format & layout | ad-hoc AEAD · age-style envelope · Tink | **Content-addressed blobs; per-user DEK; versioned envelope header (key-id + alg); chunked AEAD for large attachments** | High — re-encrypt everything |
| D5 | Key hierarchy & unseal | KMS · HSM · file key + boot secret | **File-based master key delivered via Docker/loaded secret at boot, behind a `KeyProvider` interface** (KMS later); rotation = re-wrap DEKs only (never mass re-encrypt); offline escrow copy (P0-8) | High |
| D6 | Search engine commitment | tantivy-go (CGO) · Bleve (pure Go) | **Spike tantivy-go with pass/fail criteria (P0-4)**; Bleve is the fallback; Mailroom owns the query AST either way | Medium *if* interface discipline holds |
| D7 | API object IDs | serial · UUIDv4 · ULID | **ULIDs** (JMAP wants opaque + stable; ULIDs sort by time, index-friendly) | Medium-high |
| D8 | Migration tooling & policy | goose · golang-migrate · Atlas | Any; **forward-only + expand/contract + online backfill jobs** is the policy that matters | Medium |
| D9 | Deletion semantics | per-provenance · per-canonical | **Per-canonical with per-provenance writeback**; Trash retention default 30d; agent deletes always soft | Medium-high |
| D10 | Isolation enforcement design | "RLS" | The full P0-5 design: forced RLS + mandatory identity-scoped tx wrapper + negative tests + per-user DEK backstop | High |
| D11 | License | AGPL-3.0 · Apache-2.0/MIT | Project owner's call; **AGPL-3.0** fits the self-hosted ethos; decide pre-publication | High post-contributions |

### Safe to defer (with the trigger that forces the decision)

- Image-proxy details (defer to v1.x build; SSRF design already sketched).
- S/MIME issuer list and library choice (defer to crypto release; see P1-13).
- Native-app framework (defer to v2; client-core API is the real dependency).
- Semantic embeddings model choice (defer; feature-gated).
- LLM provider plumbing (defer; interface only).
- Sieve editor UX (defer; ManageSieve confirmed available).
- FS vs MinIO blobs (ship FS behind a `BlobStore` interface; MinIO when >1 disk).
- Meilisearch/Typesense swap (only if the spike or scale forces it).

---

## 4. Challenges to decisions already made

1. **v1.0 scope is the biggest risk in either document.** The v1.0 rollup is multi-account sync + writeback + bulk import + hybrid labels + full-text search + full alias identity + Undo Send + T0+T1 ML with explainability and online learning + contacts + multi-user isolation + security baseline + web+PWA+push + a documented protocol. That is a *year-plus* of disciplined solo work even heavily AI-assisted, and none of it is validated by real use until all of it exists. **Counter-proposal:** define an explicit 0.5 "daily-driver" milestone — single user, real provider accounts, read/search/label/reply/archive, no ML, no crypto, no multi-user *UI* (but the multi-user *data model and isolation* from day one, since that's the one-way door). Dogfooding real mail for a month will reorder the rest of the roadmap better than any review can. The matrix targets can stay; add the intermediate milestone.
2. **"Admin can't read mail" (spec §10)** — challenge sustained per P0-2: reword to what the design delivers (software-enforced role separation + audit), or accept the (large) cost of user-passphrase-wrapped keys and lose offline server-side ML/search. The choice is already made; make the doc say so.
3. **"Static binary" (spec §3)** — pick "single deployable container" as the claim; CGO+Tantivy makes "static Go binary" false in the pure-Go sense (P0-4).
4. **Undo Send (v1.0) vs Send-later (v1.x)** — same mechanism (delayed submission job). Build once in v1.0; the matrix split implies two implementations.
5. **Multi-user *conveniences* in v1.0** — isolation model, OIDC, roles: yes, day one (one-way door). But per-user quotas, LLM budgets, admin dashboards are two-way doors — consider trimming those to "enforced defaults, no UI" for v1.0.
6. **"Third-party JMAP clients can connect — falls out of the API" (matrix §15)** — it does not fall out; it falls out only of real 8620 conformance (P0-3). Keep the line only if you take that path.
7. **Junk as read-only mirror** — sound, but add one exception: "Not junk" must writeback (move to INBOX + optionally feed rspamd via Migadu's mechanisms if any), or users can't rescue false positives. Half-read-only, in other words.

---

## 5. Deep dives

### A. Server-side key management + threat model
- **Hierarchy:** master key (file, delivered via secret store at boot; `KeyProvider` interface so KMS/HSM can slot in) → per-user KEK → per-purpose DEKs (mail blobs, credentials, private keys). Private keys additionally live only inside a **separate crypto process** with a minimal API: `sign(user, key_id, digest)`, `decrypt(user, key_id, blob)`, `generate`, `import`, `export-wrapped`. No API returns raw private key material. This process is the only holder of unwrapped user KEKs; the main app never sees them.
- **Unseal at boot:** automatic (secret injected) — accept that this means *anyone who owns the box owns the keys while it runs*. Manual-unseal (Vault-style) is theater for a self-hosted deployment that is rebooted remotely; skip it.
- **Rotation:** master-key rotation = re-wrap KEKs (cheap, do it annually/on suspicion); DEK rotation = new DEK for new writes, lazy re-encrypt old blobs (background job); user PGP key rotation = normal key lifecycle + WKD republish + keep old keys for decryption forever (mark `decrypt_only`).
- **Loss/recovery:** master-key escrow offline (P0-8). Per-user: because KEKs are master-wrapped, the *server* can always recover a user's data — that is the honest flip side of "admin blindness is policy."
- **What a full compromise yields (write this table into §11):** all mail plaintext (store + index), all credentials (IMAP/SMTP/OAuth tokens — they must be usable, so they're recoverable), all private keys → attacker can decrypt any previously captured ciphertext to the instance's addresses and can sign/decrypt as every user until keys are rotated and revoked. Mitigations that still matter: the crypto-process boundary raises the bar from "SQL injection" to "process compromise"; audit + anomaly detection shortens dwell time; per-user DEKs contain *partial* compromises.
- **Does this undermine E2E enough to reconsider?** No — because the product goal (server-side triage/search over all mail, including encrypted) is *incompatible* with client-held keys. The alternative worth one paragraph in the doc and then rejection: passphrase-wrapped user keys with decrypt-on-login sessions — breaks background sync/ML for offline users, which kills the triage product. Decision stands; framing changes.

### B. IMAP sync correctness, writeback, and multi-client conflict
- **Source-of-truth split (write this into §4):** IMAP owns message existence, flags, and folder placement. Mailroom owns labels-marked-local, triage data, snooze/pin/mute, contacts, and everything IMAP can't represent. Mirrored labels are *dual-homed*: canonical in Postgres, best-effort reflected as keywords.
- **Conflict model:** store **intent deltas, not target states**. Every user action becomes `(op, object, delta, idempotency_key)`: `+\Seen`, `-label:foo`, `move:Archive`. Deltas commute with concurrent remote changes; replay after reconnect is idempotent. On reconcile (QRESYNC/CONDSTORE diff), remote changes merge as their own deltas. Genuine conflicts (both sides toggled the same flag opposite ways within one reconcile window) resolve **remote-wins for flags** (the phone user saw the message; believe them), **local-wins for Mailroom-only concepts**. Log every conflict resolution to the audit trail.
- **The disappearing-UID case:** action targets `(mailbox, uidvalidity, uid)`; if UIDVALIDITY changed or UID is gone, re-resolve via the canonical message's other provenance or by content-hash search in the new mailbox generation; if unresolvable, dead-letter the action and surface it (never silently drop a user's "archive").
- **Label↔keyword mapping:** probe `PERMANENTFLAGS` per mailbox — if `\*` absent, the server rejects user keywords and that mailbox is local-labels-only. Deterministic mapping: normalize to atom charset (`MailroomLabel-<slug>`, collision-suffixed), persist the mapping table (never re-derive from name — renames must not orphan keywords). Nested labels and colors don't roundtrip: mirror leaf slug only, document it.
- **Test harness:** Dovecot in CI + scripted second client mutating concurrently; property tests for UIDVALIDITY reset, expunge-during-fetch, keyword rejection, IDLE drop mid-transaction. This harness is a 0.x milestone, not a nicety.

### C. Search index ↔ source-of-truth consistency
- **Doctrine (one sentence for §7):** the index is a **disposable cache of the canonical store**; it is never backed up, never trusted, always rebuildable.
- **Mechanics:** index updates consume the same per-user change log that feeds JMAP `/changes` (D3) — one ordered stream, one cursor per consumer; the index cursor lagging = "search results may be N minutes stale" surfaced in UI. Crash between DB commit and index write is healed by cursor replay (at-least-once + idempotent upsert by message id).
- **Reindex strategy:** versioned index schema; on version bump, build v(n+1) per user in the background into a new directory, swap atomically, delete old. During rebuild: serve the old index (stale-but-working) rather than degrading. Per-user rebuild also serves "my search seems broken" support cases and the initial bulk import (index incrementally as blobs land, exactly as §7 already says).

### D. Multi-tenant isolation (beyond "we use RLS")
Layered, per P0-5: (1) Postgres — forced RLS, non-owner app role, deny-by-default, identity-mandatory tx wrapper, CI negative tests; (2) blobs — per-user prefix from HMAC + per-user DEK (crypto backstop); (3) index — per-user directory, 0700, and honest documentation that this layer is process-trust; (4) ML — per-user model files under the same blob discipline; feature extraction must never read cross-user aggregate state (global priors are fine if trained on public/synthetic data only — decide and write it down, because "global sender reputation" is a tempting future feature that breaks isolation); (5) keys — per-user KEK inside the crypto process; the process API takes an authenticated user context, so a confused-deputy call for another user's key fails closed; (6) API — user id comes from the token, never from a request parameter, and object ids are ULIDs (non-enumerable).

### E. JMAP-subset scope (concrete)
- **Methods for v1.0:** `Email/get|query|queryChanges|changes|set`, `Mailbox/get|changes|set` (labels modeled as JMAP Mailboxes with a `role`-like extension property, or a separate `Label` type under `urn:mailroom:labels` — pick one; lean: **labels as JMAP keywords + Mailboxes for folders**, which is what 8621 actually models, with a Mailroom extension only for color/nesting metadata), `Thread/get`, `EmailSubmission/get|set` (send, undo-send window = `EmailSubmission` with future `sendAt`), `Identity/get` (+ extension for derived-alias semantics), blob up/download, push via `EventSource` first (WebSocket later).
- **Extensions as capabilities:** `urn:mailroom:triage` (scores, feedback, explanations), `urn:mailroom:contacts` (until JSContact someday), `urn:mailroom:keys`, `urn:mailroom:rules`. Extensions add *methods and properties*; they must not change core method semantics — that's the discipline that keeps third-party clients working.
- **Verified library reality:** build server bespoke (nothing to adopt); use `rockorager/go-jmap` types where convenient and aerc as the external conformance client. Full standard JMAP (all of 8620 incl. WebSocket push, quotas extension, etc.) is *not* worth it for v1.0; correct core semantics for a subset is.

### F. Data model + migration after real data exists
Covered by D1/D3/D8/D9 plus: put `schema_version` in backups; treat `messages`/`message_blobs`/`message_labels` as the tables that will hold 10⁶–10⁷ rows per user and design their indexes with the change-log and RLS predicates in mind from day one (RLS adds `user_id = ...` to every plan — every hot index leads with `user_id`); blob store layout is content-addressed under user prefix (D4) so schema migrations never touch blob bytes; the expand/contract policy plus online backfill jobs is the whole answer to "tens of GB later" — write it as a one-page `docs/migrations.md` now.

### G. Deliverability for the catchall/wildcard model
- The core is fine, given a provider that signs DKIM for the domain and allows wildcard send-as (Migadu does when configured): **From = anything@your-domain via the provider's submission keeps SPF and DKIM aligned** for DMARC. Arbitrary derived addresses do not look like spoofing *for your own domains*.
- Real constraints: (a) daily outbound caps (P0-7) are the binding limit, not reputation; (b) enforce From ∈ verified identities/domains server-side; (c) **first-send-from-a-new-alias UX** — replying from `randomsite@` to a *human* (not the site) leaks the per-site alias scheme; add a gentle compose-time hint when the derived From ≠ any address the recipient has ever seen from you; (d) DSN parsing so bounces to weird aliases are visible; (e) keep `Message-ID` domain consistent (generate under the sending identity's domain — some spam filters score mismatched Message-ID domains).
- Gmail-forward tail (spec §18.1): the right approach — Takeout for the archive, keep the forward short-term, live OAuth only if the tail matters. Note the forward's known cost (SPF breaks on forwarded mail; DKIM usually survives; envelope-to is rewritten so **alias derivation for forwarded Gmail mail must fall back to header analysis** — worth one line in §5).

---

## 6. Concrete edits to the documents

**Spec:**
- §3 — replace "static binary" with "single deployable container; core is Go with one native library (Tantivy via CGO)"; add the tantivy-go spike as a stated precondition. Add: OpenPGP via ProtonMail go-crypto/gopenpgp (x/crypto/openpgp is deprecated); S/MIME library TBD at crypto-release time.
- §4 — replace Message-ID dedup with content-hash identity (D1); add DSN/bounce pipeline; add Sent-append dedup rule; add forwarded-mail alias-derivation fallback; add the sync test harness as a 0.x deliverable.
- §5 — add outbound budget/ledger + quota meter; add server-side From validation rule; add new-alias-to-human compose hint.
- §6 — add PERMANENTFLAGS probe + persisted label↔keyword mapping table + non-roundtrip caveats; add "Not junk" writeback exception.
- §7 — add "index is a disposable cache" doctrine, change-log-driven updates, versioned index + background rebuild-and-swap, and **index directories require at-rest encryption at the volume level** (they contain decrypted PGP mail).
- §8 — add model snapshots/rollback/reset, eval harness, cold-start behavior, and the "no cross-user features" isolation rule.
- §9 — retitle the promise honestly ("provider-blind + at-rest encryption; not E2E"); add the compromise-impact table; specify the key hierarchy + crypto-process API + escrow + rotation from deep-dive A; note WKD is an Internet-Draft (draft-koch-openpgp-webkey-service, currently -21), not an RFC — stable in practice (GnuPG ecosystem), but pin the draft revision you implement; note the catchall corollary: **WKD publishes keys per hashed local-part, so derived aliases are unpublishable in bulk — publish keys only for real identities**, and the `openpgpkey` subdomain must either be absent or fully correct (clients MUST try the advanced method first).
- §10 — reword the admin claim (P0-2); expand isolation to the full P0-5/deep-dive-D design.
- §11 — add search index + ML model files to the at-rest encryption inventory; add backup **restore-unit definition + escrow + scheduled restore drill**.
- §14 — add: `change_log` (per-user per-type sequences), `jobs` (durable timers), `outbound_ledger`, `label_keyword_map`, `dsn_events`; change `messages` dedup key to content hash; note ULID ids.
- §16 — name the migration tool + expand/contract policy; add ADR practice; add restore drill to ops.
- §17 — insert a 0.5 "daily-driver" milestone (single-user read/search/reply on real mail) before 1.0.
- §18 — add: ask the provider (Migadu, for the reference profile) for the concurrent-IMAP cap and SMTPUTF8 support in writing; license choice.

**Feature matrix:**
- §1 — add row: "Bounce/DSN handling — v1.0" (it's currently nowhere).
- §3 — merge Undo Send and Send later mechanically (one delayed-submission engine, v1.0; send-later UI can stay v1.x).
- §8 — add rows: "Search-index at-rest encryption — v1.0 (deployment requirement)"; "Restore drill — v1.0 ops".
- §10 — add row: "Per-user export format defined — v1.0 (spec), implemented v1.x".
- §15 — either keep "third-party JMAP clients — v1.x" *and* adopt true 8620 core semantics, or delete the row.
- Rollup — reflect the 0.5 milestone.

---

## 7. "Design done" checklist (conditions before v1.0 code)

1. ADRs written and numbered for D1–D11 (all "MUST decide" rows resolved).
2. Message identity = content hash, per-user, with the Gmail label-copy and mailing-list cases written out as examples.
3. JMAP conformance decision made; the state/changes mechanism (per-user per-type change log) specified with its exact delta semantics.
4. Crypto design doc: key hierarchy diagram, crypto-process API, unseal, rotation, escrow, and the compromise-impact table.
5. Isolation design: RLS policy templates + tx-wrapper contract + the CI negative-test list.
6. tantivy-go spike executed against pass/fail criteria; SearchBackend interface + Mailroom query AST sketched.
7. Sync conflict matrix (deep-dive B) written, including the label↔keyword mapping and disappearing-UID rules.
8. Deletion semantics, Sent-dedup, and DSN pipeline specified.
9. Backup restore-unit + escrow procedure written; restore drill scripted (even if manual at first).
10. Migration tool chosen; `docs/migrations.md` policy written.
11. Migadu answers in hand (concurrent IMAP cap; SMTPUTF8) or conservative defaults documented.
12. License chosen.
13. The 0.5 daily-driver milestone defined in Forgejo with its issue list.

---

## 8. Highest-leverage next artifact

**The canonical data model + protocol-state design**: Postgres DDL for the full §14 schema (with the change-log, jobs, ledger, and mapping tables added), the RLS policies and tx-wrapper contract, the message-identity/dedup specification with worked examples, and the JMAP state/changes semantics that the change log drives. It is the artifact that pins all four one-way doors at once (identity, wire protocol, isolation, migration), and everything else — crypto doc included — hangs FKs off it.

I can produce it next: full DDL + RLS policies + the change-log/`/changes` semantics spec + the dedup examples, as a reviewable `mailroom-data-model.md` + `schema.sql` pair.
