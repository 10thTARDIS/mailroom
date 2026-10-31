# Mailroom — Self-Hosted Intelligent Email Client
### Technical Specification (v0.4 — design-review fixes incorporated)

> Consolidated authoritative spec. Supersedes v0.3. Companion documents: **mailroom-feature-matrix.md** (v0.4) and **mailroom-design-review.md** (the pre-implementation review that produced these changes). Sections changed in this revision are marked **[v0.4]**; earlier markers ([RESOLVED]/[NEW]) are retained where still accurate.
>
> **v0.4 changelog:** content-hash message identity (replaces Message-ID dedup); JMAP conformance resolved to true RFC 8620/8621 core semantics; honest crypto threat model + key hierarchy + escrow; isolation design made concrete; outbound ledger + DSN pipeline; label↔keyword mapping rules; index-as-cache doctrine + index encryption requirement; durable job model unifying Undo Send / send-later; migration + ADR + restore-drill policy; 0.5 "daily-driver" milestone; decision log appendix.

> **⚠ Open-source portability (add this to `claude.md` at project start):** This project may be open-sourced. All docs, examples, config samples, and code must stay free of anything identifying the operator or their self-hosted infrastructure — no personal domains, internal hostnames, VPS/homelab details, private repo/auth/service URLs, or personal email addresses. Use neutral placeholders (`example.com`, `mail.example.com`, `git.example.com`, `${OIDC_ISSUER}`, `you@example.com`). Naming external/third-party products generically (Gmail, Migadu, Ollama, Authentik, Caddy, etc.) is fine; naming anything the operator personally hosts is not. The project is private until at least a stable v1; the license decision is deliberately deferred until pre-publication (see §18).

---

## 0. Why we're building this (landscape check, confirmed)

Re-verified against the 2026 market: every capable AI-triage email product (alfred_, Superhuman, Shortwave, SaneBox, Spark, Fyxer) is **cloud SaaS, Gmail/Outlook-tied, and content-egressing**. SaneBox alone is provider-agnostic but sorts by sender behavior only (not content) and is cloud. Self-hosted webmail (Roundcube, SnappyMail, Cypht, Nextcloud Mail) gives a client but no learned triage. Modern JMAP webmail (bulwarkmail, root.cloud) is nice but hard-tied to the Stalwart mail *server*. Nothing combines: self-hosted *client* over multiple IMAP accounts/aliases + local no-GPU ML triage + optional self-hosted LLM + fast full-text search + PGP/S-MIME + multi-user + web/mobile. **Confirmed gap → build it.**

---

## 1. Goals & non-goals

### Primary goals
1. Unified **client** over multiple IMAP/SMTP accounts (Migadu reference; provider-agnostic). Never a mail server.
2. Gmail-parity UX: labels, priority inbox, threading, archive, snooze, fast search, keyboard-driven.
3. Priority inbox + auto-triage via **local, no-GPU** machine intelligence; no required external service.
4. Fast, typo-tolerant full-text search over all mail.
5. **Alias-aware identity**: auto-reply from the alias/account a message was addressed to.
6. Optional pluggable advanced AI (self-hosted LLM first; external opt-in). Never required.
7. **Multi-user from day one** with strict per-user isolation.
8. Encrypted mail + signing (PGP + S/MIME), plus a **key-authority / public-key-server** capability. **[v0.4]** Honest framing: Mailroom is **provider-blind with strong at-rest encryption**, not end-to-end encrypted — see §9.
9. **Integrated contacts** (internal in v1; CardDAV sync later).
10. **Documented API + MCP server** for AI-agent access with scoped permissions.
11. Clean, responsive clients: web (Gmail-like) + PWA now; native iOS/Android and a desktop thick client later.
12. Security-first; fast on minimal resources.

### Non-goals (v1)
- Not a mail server (no MX, no internet spam scanning — Migadu/rspamd handles that; we consume Junk verdicts).
- Full groupware is out; calendar is a v3 hook, contacts are in because triage/identity/crypto need them.
- **[v0.4]** Not end-to-end encryption. Server-side keys are a deliberate trade for server-side search/ML over encrypted mail (§9).

---

## 2. Architecture

```
   Migadu + any IMAP/SMTP ◄──── Sync Engine (per acct, IDLE/QRESYNC, writeback)
                                        │
                                        ▼
                            Canonical Store
                            Postgres (RLS) + encrypted blob store
                            + per-user/per-type change log (drives sync, /changes, index)
                                        │
        ┌───────────────┬──────────────┼───────────────┬────────────────┐
        ▼               ▼              ▼               ▼                ▼
  Search (Tantivy,  Triage/ML     Identity/Send   Crypto process    Contacts
  swappable)        T0 rules      (SMTP send-as,  (PGP/S-MIME keys, (internal v1,
                    T1 Go ML      outbound ledger, sign/decrypt API, CardDAV v2)
                    T2 LLM (opt)  DSN pipeline)    WKD/Autocrypt)
        └───────────────┴──────────────┬───────────────┴────────────────┘
                                        ▼
                    API layer: JMAP core (RFC 8620/8621 subset)
                    + Mailroom capability extensions
                    REST/WS + Web Push (VAPID) + MCP server (scoped keys)
                                        │
        ┌───────────────┬──────────────┼───────────────┬────────────────┐
        ▼               ▼              ▼               ▼                ▼
     Web SPA          PWA        Native iOS/       Desktop         AI agents
   (Gmail-like)                  Android (v2)   (Tauri, v3)      (via MCP)
        └──── all consume the shared TS client-core + documented API ────┘

   Fronted by Caddy; auth via Authentik (OIDC). Compose profiles: minimal / enhanced(+LLM).
```

---

## 3. [RESOLVED, v0.4] Stack

**Core: Go.** Chosen over Rust primarily for the mail ecosystem — emersion's `go-imap`, `go-message`, `go-smtp`, `go-sasl`, `go-msgauth` (DKIM), and `go-webdav` (CardDAV/CalDAV) give us nearly the whole stack from one maintainer. Low RAM, great concurrency for many IDLE connections.

**[v0.4] Deployment claim corrected:** Mailroom ships as a **single deployable container**, not a pure-static Go binary — the search engine is Tantivy (Rust) via the `anyproto/tantivy-go` CGO bindings (production-proven in Anytype across platforms, MIT). Accepted costs: a second toolchain in CI, CGO cross-compilation care, and dependence on the binding's exposed API surface. **Precondition before search design is final: a week-one spike** proving tantivy-go supports per-user index directories, fuzzy + prefix + phrase + field queries, highlighting, incremental delete/update, and p95 < 100 ms over a ~25 GB synthetic corpus (see §7 and decision log D6). Fallback if the spike fails: Bleve (pure Go, slower) behind the same interface.

**[v0.4] go-imap v2 is pre-stable** (upstream still marks it in development). Policy: pin a commit and vendor; wrap it behind our own `SyncSession` interface (anti-corruption layer) so library churn and protocol quirks don't ripple through the codebase; contribute fixes upstream where sensible. Verify QRESYNC support in v2 at spike time (CONDSTORE is confirmed exposed).

**[v0.4] Crypto libraries:** `golang.org/x/crypto/openpgp` is deprecated/frozen — OpenPGP uses **ProtonMail's go-crypto fork + gopenpgp**. S/MIME support in Go is thin (the commonly-referenced pkcs7 package is archived); the S/MIME library choice is deferred to the crypto release and is priced as meaningfully more work than PGP (see feature matrix §8).

**ML: Go-only to start (no Python required).** Tier-1 uses Naive Bayes + online logistic regression — both naturally incremental, explainable, CPU-trivial, and easy to implement directly in Go. This keeps Mailroom one language / zero external ML deps, matching the "no required dependencies" goal. The model layer sits behind an interface; **if** we later hit a quality ceiling we add a Python `mailroom-trainer` sidecar (training only — inference stays in Go via ONNX). Static embeddings (model2vec/fastText) can run via ONNX-in-Go when we want semantic features.

**Frontend:** React + TanStack Query (consistent with HomeBooks), packaged so the web app, PWA, and a Tauri desktop shell reuse one codebase. A shared **TypeScript client-core** library holds JMAP client logic, offline cache, sync/action-queue, and local search — reused by every client.

---

## 4. [v0.4] Sync engine (IMAP ingest + writeback)

- **Account** = one IMAP/SMTP login. **Identity/alias** = a send-as address that delivers into a mailbox.
- **[v0.4] Canonical message identity: content hash.** A **Message** is identified per user by `SHA-256(raw RFC 5322 bytes)` (`(user_id, content_hash)` unique). `Message-ID` is **never identity** — it is sender-controlled, optional, reusable, and spoofable; it serves only as a *grouping hint* for threading and for collapsing Gmail label-copies. Identical bytes appearing in multiple folders/accounts collapse to one canonical message with a **provenance list** (account/mailbox/UID each) so it displays once but writes back everywhere it lives. Dedup is **strictly per-user** — never across users. Worked examples the implementation must satisfy: (a) Gmail "All Mail" + label folders deliver identical bytes → one message, N provenances; (b) a mailing-list copy with a rewritten footer + the direct copy share a Message-ID but differ in bytes → **two** messages, threaded together; (c) an attacker reusing someone else's Message-ID can never merge into or overwrite an existing message.
- Per folder track `UIDVALIDITY` / last `UID` / `HIGHESTMODSEQ`; use **CONDSTORE/QRESYNC** for cheap incremental sync; `IDLE` per account for push; periodic reconcile as backstop.
- **Two ingest modes, one canonical store:**
  - **Live IMAP sync** — ongoing accounts you keep. Rate-limited + connection-pooled to respect provider limits (Migadu concurrent-IMAP cap still ⚠ to verify — stay conservative: one long-lived IDLE + a small worker pool, exponential backoff).
  - **Bulk archive import** — one-time ingestion of large historical archives from **mbox / maildir / Google Takeout**. Runs locally at full speed (no provider throttling), background, **resumable**, with progress. This is the recommended path for large archives (e.g. a multi-GB Gmail export) instead of dragging them over IMAP.
- **Gmail-specific handling:** content-hash identity collapses label-copies automatically; `X-GM-MSGID` is kept as provenance metadata. Live Gmail IMAP is bandwidth-throttled by Google (daily quotas); when kept as a live account, throttle hard and prefer Takeout for the back-catalog.
- **[v0.4] Forwarded-mail caveat:** a Gmail forward to another mailbox rewrites the envelope recipient, so **alias derivation for forwarded mail must fall back to header analysis** (`X-Forwarded-To`, original `To`/`Cc`, `Delivered-To` chain) rather than the envelope.
- **Backfill is low-priority and throttled** so bulk import never thrashes CPU/disk or starves live sync; attachment-text extraction (the expensive part) is async and rate-limited.
- Handle UIDVALIDITY resets without dup notifications. Threading via JWZ (References/Message-ID) unified across accounts, **[v0.4] with restricted subject-fallback**: subject-only merging is allowed only within the same account, same correspondent set, and a bounded time window — cross-account subject-fallback is off (false-merge protection). Thread membership is stored but **recomputable**, and merges are logged for diagnosability.
- **Writeback** (Migadu stays source of truth): `\Seen`, `\Flagged`, archive-move, Trash, Sent `APPEND`, and labels (§6). Actions apply locally instantly, reconcile to IMAP asynchronously, idempotent, offline-tolerant, retrying. Conflict semantics: see §4a.
- **[v0.4] Sent-mail dedup:** most providers do **not** save a copy on SMTP submission, so Mailroom APPENDs to Sent per account — but the appender first checks (by content hash) whether the provider self-appended (Gmail does), so relayed sends never appear twice.
- **[v0.4] Deletion semantics (D9):** user "delete" moves the **canonical message** to Trash, propagated to every provenance (per-provenance IMAP moves). Trash auto-purges after a default 30-day retention (configurable per user). Hard delete propagates to: IMAP (EXPUNGE where permitted), blobs, index, ML feature store, and leaves only an audit tombstone. Agent-initiated deletes are **always soft** with a recovery window (§13a).
- **[v0.4] Bounce/DSN pipeline (new, v1.0):** delivery status notifications (RFC 3464) and bounce messages are parsed, associated to the originating message via `References`/original Message-ID/envelope metadata, and surfaced on the thread ("delivery to X failed") plus in a per-account delivery-health view. Sends to derived catchall aliases will bounce more often than average; silent bounce loss would destroy trust in send.
- **[v0.4] Sync test harness is a 0.x deliverable, not a nicety:** Dovecot as reference server in CI; a scripted second client mutating mailboxes concurrently; property-style tests for UIDVALIDITY reset, EXPUNGE racing FETCH, flag changes racing writeback, keyword rejection, IDLE drop/reconnect mid-transaction.

### 4a. [v0.4] Writeback conflict model (intents, not states)
- Every user action is stored as an **intent delta** with an idempotency key: `(+\Seen, msg X)`, `(-label:foo, msg X)`, `(move:Archive, msg X)` — never "set flags to {...}". Deltas commute with concurrent remote changes and replay safely after reconnect.
- On reconcile (QRESYNC/CONDSTORE diff), remote changes are ingested as their own deltas. Genuine conflicts (both sides toggled the same flag opposite ways within one reconcile window) resolve **remote-wins for IMAP-representable state** (the other client's user saw the message — believe them) and **local-wins for Mailroom-only concepts** (local labels, snooze, pin, triage). Every conflict resolution is written to the audit log.
- **Disappearing-UID rule:** actions target `(mailbox, uidvalidity, uid)`. If UIDVALIDITY changed or the UID is gone (e.g. the phone client moved the message), re-resolve via the canonical message's other provenances or by content-hash lookup in the new mailbox generation; if unresolvable, the action goes to a **dead-letter queue and is surfaced to the user** — a user's "archive" is never silently dropped.

---

## 5. [v0.4] Compose, send & identity (the alias story)

- **Reply/reply-all:** default From = the exact address the message was delivered to (`Delivered-To`/`X-Original-To`/envelope → matched `To`/`Cc`; header-analysis fallback for forwarded mail, §4). One-keystroke override.
- **Catchall + wildcard-send model (Migadu, confirmed):** the primary domain is a catchall — arbitrary per-site addresses all land in one mailbox, and there is **no finite alias list to enumerate**. So identity is **derived from the received envelope**, not from configured aliases: whatever address a site used *is* the reply-from address. Wildcard send-as is configured at the provider (any address at the domain can send), so Mailroom can send as any derived address without per-alias setup. Specific real mailboxes (a handful, across a few domains) are still first-class accounts.
- **New compose:** user picks identity; remembered per-recipient and per-domain. **Contacts store the address each correspondent reached you at**, so e.g. a company that mails `store@` defaults your reply to `store@`.
- **Send:** authenticate to the account's SMTP and send as the chosen/derived address (wildcard send-as confirmed). Correct `Message-ID`/`In-Reply-To`/`References`/`Date`; **[v0.4] generate `Message-ID` under the sending identity's domain** (mismatched Message-ID domains cost spam-filter points); keep DKIM alignment (provider signs the domain).
- **[v0.4] Server-side From validation:** the server refuses any From address not matching a verified identity/domain for the authenticated user — even though the provider would also reject it. Defense in depth for multi-user: one user's misconfiguration must never burn another domain's reputation from a shared instance.
- **[v0.4] Outbound ledger (new, v1.0):** Migadu (and most providers) enforce **daily send caps counted per account, not per mailbox**. Every send — interactive, Undo-Send-queued, send-later, vacation responder, MCP agent — draws from a per-account budget tracked in an `outbound_ledger` (quota, spent, resets-at) with backpressure and a **user-visible quota meter**. Approaching-limit and at-limit states are explicit; sends queue rather than silently failing late.
- **[v0.4] Alias-privacy compose hint:** replying from a derived per-site alias to a *human* who has never seen that address leaks the per-site alias scheme. Compose shows a gentle hint when the derived From ≠ any address this recipient has previously received from you.
- **[v0.4] One delayed-submission engine:** Undo Send (hold window) and Send Later are the **same mechanism** — a durable submission job with a fire-at time (§16 job model); "undo" cancels the job inside the window. Built once in v1.0; the send-later *UI* ships in v1.x.
- Rich + plaintext compose, quoting, attachments, inline images, drafts (local + optional IMAP), per-identity signatures + default keys, templates/snippets, and a **vacation responder / server-side filters written via ManageSieve** (confirmed available on Migadu).
- **[v0.4] Draft conflict rule:** drafts are the one object edited concurrently (Mailroom + phone client via IMAP Drafts). Rule: last-writer-wins by IMAP `INTERNALDATE`/modseq, and the losing version is preserved as a second draft rather than destroyed — duplication over data loss.

---

## 6. [RESOLVED, v0.4] Labels, folders, priority inbox

- **Label model: hybrid, IMAP-sync by default.** Canonical labels live in Postgres; **by default each label mirrors to IMAP keywords** so other clients and backups see them. Labels that can't be represented (or that the user marks local-only) stay local. Default = sync; local-only is the exception, not a per-label chore. (Areas' own labels are the exception: local-only by decision, ADR-031.)
- **[v0.4] Keyword mapping rules:** per mailbox, probe `PERMANENTFLAGS` — if `\*` is absent, that mailbox rejects user keywords and is **local-labels-only** (recorded, surfaced in account health). Mapping is deterministic and **persisted in a `label_keyword_map` table** (never re-derived from the label name — renames must not orphan keywords): normalize to the IMAP atom charset as `MailroomLabel-<slug>`, collision-suffixed. Known non-roundtrip caveats, documented in-product: nesting and colors don't mirror (leaf slug only); other clients see raw keyword atoms.
- **Views:** Unified Inbox (+per-account), **Priority Inbox** (learned score → buckets, adjustable threshold), Starred, Snoozed, Sent, Drafts, Archive, Trash, Junk (mirror of provider verdict), label views.
- **[v0.4] Junk is read-only with one exception:** "Not junk" must write back (move to INBOX) so users can rescue false positives; where the provider offers a ham-feedback mechanism we use it, otherwise the move itself is the signal.
- Snooze / mute-thread / pin / "archive = done", implemented locally with writeback where representable. (Mute is the exception: Mailroom-only by decision, ADR-029.) **[v0.4]** Snooze wake-ups ride the durable job model (§16) — timers survive restarts, and missed timers are caught up on boot.

---

## 7. [RESOLVED, v0.4] Search

- **Engine: embedded Tantivy** via `anyproto/tantivy-go` (CGO; see §3 spike precondition) — in-process, low RAM, no extra service — behind a **`SearchBackend` interface** so we can swap to **Meilisearch/Typesense** later with no client changes if we outgrow it. **[v0.4] Mailroom owns its query AST**: Tantivy query syntax never leaks into the API or clients — this is what keeps the backend genuinely swappable.
- **[v0.4] Doctrine: the index is a disposable cache of the canonical store.** It is never backed up, never trusted, always rebuildable (`reindex-user` is a first-class operation). Index updates consume the same **per-user per-type change log** that feeds JMAP `/changes` (§13) — one ordered stream, one cursor per consumer. A lagging cursor is surfaced as "search may be N minutes stale." Crash between DB commit and index write heals by cursor replay (at-least-once + idempotent upsert by message id).
- **[v0.4] Versioned index + rebuild-and-swap:** the index schema carries a version; on bump, build v(n+1) per user in the background into a new directory, swap atomically, delete old. During rebuild the old index keeps serving (stale-but-working beats degraded).
- Indexes headers, addresses, subject, body (text + HTML-stripped), and **attachment text** (opt-in, size-capped).
- Typo tolerance, prefix/as-you-type, phrase, field filters (`from:`/`to:`/`label:`/`has:`/`is:`/`account:`/date), ranked relevance, highlighting.
- **Per-user index** (hard multi-tenant isolation — a security control, not just organization). **[v0.4]** Honesty note: the index is the **weakest isolation layer** (per-user directories + filesystem permissions within one process); the compensating controls are the crypto backstop for blobs and the deployment requirement below.
- **[v0.4] Encrypted-mail consequence:** since keys are server-side (§9), encrypted bodies are decrypted server-side and indexed normally — therefore **the index contains the plaintext of encrypted mail**. Tantivy has no native encryption, so **index directories (and ML model files) require at-rest encryption at the volume/filesystem level (LUKS/dm-crypt or equivalent) as a stated deployment requirement**, listed in §11's inventory.
- Optional CPU semantic search via static embeddings.
- **Scale note:** archives are large (tens of GB per user is realistic). Tantivy is a **disk-backed, memory-mapped** index, so RAM stays modest even as the corpus grows — a key reason for the embedded choice. The extracted-text index is a fraction of raw size; content-hash dedup (esp. Gmail label-copies) keeps it lean. Bulk import builds the index incrementally in the background so search is usable before the full archive finishes.

---

## 8. [v0.4] Intelligence — three tiers (local-first, GPU-free)

Fully functional on **T0 + T1** alone; **T2 is enhancement only**.

- **T0 — Rules:** Sieve-like conditions (sender/domain/`List-Id`/`List-Unsubscribe`/headers/alias/regex) → actions. User-authored + system-suggested. Instant, transparent, great cold-start.
- **T1 — Classical ML (Go, per-user, incremental):**
  - Features: sender/domain, list/bulk headers, auth results (SPF/DKIM/DMARC), **which alias it was addressed to**, your history with sender/thread (replied? how fast? open vs archive-without-read), direct-vs-CC-vs-list, length, time-of-day, hashed n-grams, optional static embeddings.
  - Models: Naive Bayes + online logistic regression → priority score (0–1) and multi-label suggestions. Trains/predicts in sub-ms, CPU-only.
  - **Online learning** from every action (open, reply, reply-speed, archive-without-read, move-to-label, star, snooze, delete, mark-not-important). Per-user models, isolated.
  - **Explainability built in:** every verdict can show top features; one-click correct → retrain.
  - Priority threshold learned per user, adjustable.
  - **[v0.4] Model lifecycle:** models are **snapshotted on a schedule and on version bumps; keep N versions; one-click rollback; user-facing "reset my model."** Online learning drifts and can be poisoned by a bad week (or by adversarial mail content in features) — snapshots make that recoverable.
  - **[v0.4] Eval harness before tuning:** replay held-out user actions against candidate models and report ranking metrics. Built *before* any accuracy work; also serves as regression protection across model-code changes.
  - **[v0.4] Cold start, concretely:** week one runs on T0 rules + explicit heuristics (direct-to-you beats list mail; replied-to senders beat strangers; auth-failing bulk sinks) so the priority inbox is *useful before it is learned* — first impressions decide whether users trust triage at all.
  - **[v0.4] Isolation rule:** feature extraction must never read cross-user state. Global priors are permitted **only** if trained on public/synthetic data. ("Global sender reputation" is a tempting future feature; it is explicitly out unless redesigned to preserve isolation.)
- **T2 — LLM (optional, pluggable, self-hosted first):** any OpenAI-compatible endpoint; default local Ollama/vLLM/llama.cpp; external providers opt-in per user with data-egress warning. Unlocks summaries, smart-reply drafts, NL search, cold-start labeling, task/date extraction. All degrade gracefully to T0/T1 if absent. No GPU ever required for a complete product.

---

## 9. [RESOLVED, v0.4] Encryption, signing & key authority

**Honest framing (replaces v0.3's implicit claim):** Mailroom is **provider-blind with strong at-rest encryption — it is not end-to-end encryption.** Private keys are server-side (deliberate: it is the only design compatible with server-side search/ML over encrypted mail, which is the product's point). The rejected alternative, recorded for posterity: passphrase-wrapped user keys with decrypt-on-login sessions would break background sync/ML for offline users and kill the triage product. Consequence: the key store is the crown jewels, and the guarantees below are stated exactly.

### What a full server compromise yields (threat-model table — keep current)

| Attacker obtains | Consequence |
|---|---|
| All mail plaintext | Store *and* search index contain readable content, incl. decrypted PGP/S-MIME mail |
| All account credentials / OAuth tokens | They must be usable for sync, so they are recoverable by the server — attacker can act against providers until rotated. Passwords and OAuth refresh tokens are sealed under the master key; OAuth access tokens are held only in memory (ADR-046). A Gmail grant is full mailbox control (read, send, permanent delete) until the user revokes it or the token dies; a Microsoft grant is limited to IMAP and SMTP |
| All private keys | Attacker can decrypt previously captured ciphertext to the users' addresses and can sign/decrypt **as every user** until keys are rotated, revoked, and republished |

Mitigations that still matter: the crypto-process boundary (below) raises the bar from "SQL injection" to "process compromise"; per-user DEKs contain partial compromises; audit + anomaly detection shorten dwell time. **Admin blindness (§10) is software policy + audit, not cryptography** — whoever holds the master key can technically read everything; the docs must never imply otherwise.

### Capabilities
- **OpenPGP + S/MIME**: sign outbound, verify inbound, encrypt/decrypt, clear signature/encryption status. Per-identity key binding. OpenPGP via ProtonMail go-crypto/gopenpgp (§3); S/MIME library TBD at crypto-release time and priced as more work than PGP.

### [v0.4] Key hierarchy, crypto process & lifecycle
- **Hierarchy:** master key → per-user KEK → per-purpose DEKs (mail blobs, credentials, private keys). Master key is file-based, delivered via Docker/loaded secret at boot, behind a **`KeyProvider` interface** so KMS/HSM can slot in later. Unseal is automatic at boot — accepted consequence: whoever owns the running box owns the keys; manual Vault-style unseal is theater for a remotely-rebooted deployment and is skipped.
- **Crypto process:** private keys are handled only inside a **separate, minimal crypto process** whose entire API is `sign(user, key_id, digest)`, `decrypt(user, key_id, blob)`, `generate`, `import`, `export-wrapped`. **No API returns raw private key material.** It is the sole holder of unwrapped user KEKs; the main app never sees them. Calls carry an authenticated user context, so a confused-deputy request for another user's key fails closed.
- **Rotation:** master-key rotation = re-wrap KEKs (cheap; do annually or on suspicion). DEK rotation = new DEK for new writes + lazy background re-encrypt. User PGP key rotation = normal lifecycle + WKD republish; old keys kept forever as `decrypt_only`.
- **Loss/recovery & escrow:** an **offline escrow copy of the master key** (age-encrypted to an offline recovery key, stored physically, off-site) is mandatory — without it, backups of envelope-encrypted data are unrecoverable (§11). Because KEKs are master-wrapped, the server can always recover a user's data; that is the honest flip side of policy-level admin blindness.
- Private keys never logged; never leave the crypto boundary; backups of key material separately encrypted; strict access + audit.

### [NEW] Mailroom as key authority / public-key server
Because you control your domain(s) and web server, Mailroom publishes and proves your identity with minimal recipient effort:
- **WKD (Web Key Directory):** auto-publish public keys at the standard `.well-known` path → any WKD-aware client finds your key from just your address, zero effort for them. *(Highest-leverage piece.)* **[v0.4] Standards note:** WKD is an **Internet-Draft** (`draft-koch-openpgp-webkey-service`, currently revision 21), not an RFC. Stable in practice across the GnuPG ecosystem, but **pin the draft revision implemented** and record it. **Catchall corollary:** WKD publishes keys per hashed local-part, so the infinite derived aliases are unpublishable in bulk — keys are published **only for real identities** the user designates. And the `openpgpkey` subdomain must be **absent or fully correct** — clients MUST try the advanced method first, so a half-configured subdomain breaks all lookups.
- **Autocrypt headers** outbound → Autocrypt-aware clients store your key and can encrypt back transparently.
- **Shareable key pages:** per-identity human page with fingerprint, QR, download, cross-check links.
- **Identity proof:** sign everything (PGP/S-MIME + DKIM alignment); optional S/MIME certs render as a verified badge in mainstream clients; optional DNS `OPENPGPKEY` records as a second discovery/cross-check channel.
- **Multi-user bonus:** users' public keys auto-discoverable to each other → internal encrypted mail "just works"; external contacts' keys stored in Contacts (§10a).

### S/MIME certificates (documented path)
S/MIME's trust model differs from PGP: a Certificate Authority vouches for the name↔key binding, and mainstream clients (Outlook, Apple Mail) then show a **verified badge automatically** — the most broadly-legible "prove I'm me" for non-technical recipients. Documented options: obtain a personal S/MIME cert from a CA that issues them (availability shifts; docs list current issuers rather than hard-coding one), install the cert + chain server-side under the same KMS-wrapped key protection as PGP, bind it per identity, and use it to sign (and, where the recipient has a cert, encrypt). PGP-first; S/MIME rides the same signing/verification plumbing and lands in the v1.x crypto release **only if** its true cost (thin Go ecosystem, §3) is accepted at that time — else it moves to v2.

---

## 10. [v0.4] Multi-user, auth & isolation (day one)

- **AuthN:** OIDC via **Authentik** (2FA handled there). Short-lived access tokens + rotating refresh; secure `SameSite` cookies; CSRF protection.
- Each user configures their own accounts/credentials; provider-agnostic; up to ~24 users on modest hardware.
- **[v0.4] Isolation is a layered design, not a slogan:**
  1. **Postgres:** `FORCE ROW LEVEL SECURITY` on every user table; policies deny-by-default; the app connects as a **non-owner role without `BYPASSRLS`**; the *only* way code obtains a DB handle is a **request-scoped transaction wrapper that requires a user identity to construct** (sets the RLS context per transaction). Every hot index leads with `user_id` (RLS appends that predicate to every plan).
  2. **Blobs:** per-user prefix derived from `HMAC(server_key, user_id)` (non-enumerable paths) + **per-user DEKs — the real backstop**: a path-construction bug leaks ciphertext under the wrong key, converting whole bug classes into non-events.
  3. **Search index:** per-user directory, `0700`, same process — honestly the weakest layer (§7); compensated by the volume-encryption deployment requirement and crypto backstop elsewhere.
  4. **ML:** per-user model files under blob discipline; no cross-user features (§8).
  5. **Keys:** per-user KEK inside the crypto process; authenticated-context API fails closed (§9).
  6. **API:** user id comes from the token, never a request parameter; object ids are ULIDs (non-enumerable).
- **[v0.4] Isolation is tested, not assumed:** CI includes **negative tests** attempting cross-user reads through every API endpoint and directly at the SQL layer; a missing RLS context or policy is a red build.
- **Roles [v0.4, reworded honestly]:** `admin` manages users/quotas/health and **cannot read any user's mail through any Mailroom surface — enforced by software role separation, schema policy, and audit, *not* by cryptography** (see §9: the master-key holder can technically decrypt everything). `user` is the norm.
- Per-user quotas/limits (storage, index, sync connections, LLM budget) + rate limits — **[v0.4] enforced defaults in v1.0; admin UI for them may lag** (two-way door). Per-user data export + hard delete. **[v0.4] Export format defined now, shipped v1.x:** mbox or maildir + JSON sidecars for labels/scores/contacts — also the escape hatch if a migration goes wrong.
- **[v0.4] Quota exhaustion behavior:** user storage quota hit mid-sync → **pause sync + alert; never drop mail silently.** Provider-side mailbox-full on writeback → surfaced in account health + dead-letter, same rule.
- Deferred to v3: shared mailboxes / delegation.

### 10a. [NEW] Contacts
Integrated because it's the connective tissue for identity + crypto + triage:
- **v1 (internal, auto-derived):** names, every address seen, **the alias each person reached you at** (→ reply-from default), discovered public keys (WKD/Autocrypt), autocomplete, VIP/block.
- **v2 (CardDAV sync):** bidirectional with Nextcloud + Apple/iCloud over CardDAV; Google via CardDAV or People API. `go-webdav` gives us the CardDAV client, so this is less work than it'd otherwise be.
- Contacts kept **inside Mailroom** (a module, not a satellite project) due to tight coupling.

---

## 11. [v0.4] Security (first-class)

**Threat model:** server/DB compromise (see §9 impact table); network interception; hostile email (HTML XSS, tracking pixels, phishing, malware attachments); credential/key theft; cross-user leakage; SSRF; supply chain; runaway API/MCP agents (§13a).

**Controls:**
- **[v0.4] At-rest encryption inventory (complete):** raw MIME, attachments, credentials, private keys (envelope encryption, per-user data keys, `KeyProvider`-wrapped master) — **plus the search index directories and ML model files via volume/filesystem-level encryption (LUKS/dm-crypt or equivalent), a stated deployment requirement** since they contain the same plaintext (incl. decrypted PGP mail).
- TLS everywhere; IMAPS/SMTPS to providers; optional internal mTLS.
- Hostile-HTML: server+client DOMPurify, **remote content blocked by default** (opt-in image proxy that strips tracking params), strict CSP, no inline scripts, HTML in sandboxed iframe.
- SSRF guard on image proxy / WKD / outbound fetches (allowlist, no internal ranges, DNS-rebind protection).
- Attachments never executed; safe `Content-Disposition`; dangerous types
  **neutralised and never rendered** (the server chooses the `Content-Type` from
  a small allowlist, so `text/html` and `image/svg+xml` are served as
  `application/octet-stream` — the file is still handed over on request, because
  refusing a user their own mail is not a control); optional ClamAV, whose
  `flagged` and `unknown` verdicts ARE withheld at the download route until the
  user releases them (ADR-027 §8); sandboxed viewer.
- Secrets via Docker secrets / age / Vault-style; nothing plaintext in images or commits.
- Containers non-root, read-only FS, dropped caps, seccomp, distroless, pinned deps, SBOM, CI vuln scanning.
- Central AuthZ + RLS backstop; per-user-visible audit log; all admin + agent-key actions logged.
- **[v0.4] Backup/restore is a designed unit, not a checkbox.** The **restore unit** = Postgres + blob store + wrapped key store + **independent master-key escrow** (§9). The search index is explicitly *not* backed up (rebuildable cache, §7). Backups record the schema version (§16). A **scripted restore drill** runs on a schedule: restore into a scratch container, decrypt one message per user, rebuild one user's index, run one search — a backup that hasn't restored is a hypothesis. Encrypted, off-box (e.g. restic) + DR runbook.
- Rate limiting on API/login/send/sync.

---

## 12. [NEW/EXPANDED] Clients & project structure

### Design system (applies to every client)
- **Gmail-like** familiarity; **light + dark modes** (system-follow + manual).
- **Easy multi-select + bulk actions** (checkbox/shift-range/select-all-matching-search → archive/label/read/delete in one move).
- **Responsive to any screen size**; clean layout with **proper margins, no overlapping visuals, good flow, easy navigation**.
- **No transparencies** anywhere (solid, opaque surfaces — readability first).
- Not full Material Design; borrow selectively (elevation cues, motion restraint) without the full language.
- Keyboard-driven power use (you read/type fast): shortcuts + command palette. Accessibility (screen-reader, keyboard nav, high contrast). Density setting. i18n-ready.
- **[v0.4] Account health panel:** users can see per-account sync state ("your Gmail sync is 3h behind"), writeback dead-letters, outbound quota, and delivery failures without reading server metrics.
- **[v0.4] PWA push expectation-setting:** Web Push on iOS works only for installed-to-home-screen PWAs (iOS 16.4+), with OS-level throttling — documented up front so it isn't the first issue anyone files.

### Client topology (enabled by the one documented API)
- **`mailroom` monorepo:** Go core/server **+** web app **+** shared **TS client-core** (JMAP client, offline cache, sync/action-queue, local search). Atomic protocol+web changes.
- **PWA** = the web app (v1): installable, offline cache of recent/starred/label subsets, action queue, Web Push (VAPID), triage-aware notifications (only *important* mail pushes by default).
- **Native iOS/Android** (v2): the real investment, in their **own repo(s)**, consuming the published client-core + documented API. Full offline mail/search/label-sync. Choose later: cross-platform (React Native/Capacitor reusing client-core — one codebase, real offline) vs. fully-native Swift+Kotlin (best feel, most work). Leaning cross-platform unless best-in-class native polish becomes the goal.
- **Desktop thick client** (v3, cheap): **Tauri** shell wrapping the web app + client-core → offline + consistency for little extra; lighter than Electron.

---

## 13. [RESOLVED, v0.4] API & protocol

**Decision (was "JMAP-subset + extensions", now precise):** Mailroom implements **true RFC 8620 core semantics** plus an **RFC 8621 subset**, with Mailroom features as **proper capability extensions**. Rationale: (a) no mature Go JMAP *server* library exists (foxcpp/go-jmap is WIP against pre-RFC draft-17; rockorager/go-jmap is client-only), so the server is bespoke either way — conforming to the RFC costs little more than inventing our own semantics and prevents drift; (b) the RFC's state/changes machinery **is** our client sync protocol — we need it anyway for offline clients, so we build it once; (c) it keeps the matrix promise that third-party JMAP clients can connect, and gives us free conformance testing (aerc via rockorager/go-jmap as an external CI client).

- **Upstream stays IMAP; JMAP is the client-facing protocol.**
- **v1.0 method set:** `Email/get|query|queryChanges|changes|set`, `Mailbox/get|changes|set`, `Thread/get`, `EmailSubmission/get|set` (send; Undo-Send = submission with future `sendAt`, cancelable in-window), `Identity/get` (+ extension property for derived-alias semantics), blob upload/download endpoints, push via **EventSource** first (WebSocket later). Labels are modeled as **JMAP keywords** (folders remain Mailboxes, per RFC 8621), with a Mailroom extension carrying color/nesting metadata only.
- **Hard invariants we commit to (the parts clients depend on):** the session object; opaque per-type **state strings with exactly-correct `/changes` deltas** (backed by the per-user per-type change log, §14); `cannotCalculateChanges` fallback semantics; result references. The change log is the single ordered stream consumed by `/changes`, the search indexer (§7), and client sync cursors.
- **Extensions as capability URNs** — `urn:mailroom:triage` (scores, feedback, explanations), `urn:mailroom:labels` (color/nesting metadata), `urn:mailroom:contacts`, `urn:mailroom:keys`, `urn:mailroom:rules`. Discipline: extensions add methods and properties; they **never change core method semantics** — that's what keeps third-party clients working.
- **Explicitly out for v1.0:** full RFC 8620 (WebSocket push, quotas extension, etc.) — correct core semantics for the subset above, not maximal surface.
- **Fully documented, versioned, machine-readable** — a formal protocol spec + OpenAPI/JSON-Schema (method definitions + examples for every call), so any client / the MCP server / an agent builds against it without guessing. First-class deliverable, not an afterthought.

### 13a. [NEW, v0.4-hardened] MCP server + agent access
- **Scoped API keys / PATs** with fine-grained scopes: `mail:read`, `mail:send`, `mail:draft`, `mail:move` (organize), `mail:delete`, `labels:read`, `labels:write`, `search:read`, `contacts:read`, `contacts:write` — restrictable per account and (optionally) per label.
- **MCP server** exposes tools mapped to those scopes (`search_mail`, `read_message`, `apply_label`, `archive`, `draft_reply`, `send`, …). The key's scopes are enforced at the tool boundary — a read-only key literally cannot send or delete (tool absent / 403).
- **[v0.4] Hardened defaults (a runaway agent is the most realistic total-mailbox-loss scenario):**
  - No key is granted `mail:send` **and** `mail:delete` together without explicit, deliberate intent at creation time.
  - Agent `mail:delete` is **always soft-delete with a recovery window** — no agent path to immediate hard deletion, ever.
  - **Per-key rate limits and daily budgets are mandatory**, not optional; agent sends draw from the outbound ledger (§5).
  - Anomaly hooks (mass-delete, mass-send, off-hours spikes) feed the audit log **with alerting**.
- Destructive scopes (`send`, `delete`) support an optional **human-in-the-loop confirm** mode per key. Every agent action is logged + attributed; keys are revocable.
- MCP is a thin, scoped facade over the same documented JMAP API — no separate surface to maintain.

---

## 14. [v0.4] Data model (sketch — Postgres, user-scoped via forced RLS; ids are ULIDs)

`users` · `accounts` (encrypted creds, folder mapping, sync cursors) · `identities` (alias, signature, default key, allowed-sender) · `mailboxes` (UIDVALIDITY/MODSEQ/last-UID, permanentflags-probe result) · `messages` (**dedup key = `(user_id, content_hash)`**, envelope, addressed-identity, flags) · `message_provenance` (message→account/mailbox/uid, incl. `X-GM-MSGID`) · `message_blobs` (content-addressed) · `attachments` (extracted-text ref, scan status) · `threads` (recomputable membership) · `labels` (name, color, parent, `mirror_to_imap` default true) + `message_labels` + **`label_keyword_map`** (persisted label↔IMAP-keyword mapping) · **`change_log`** (per-user, per-type monotonic sequences — drives JMAP `/changes`, index updates, client sync) · **`jobs`** (durable timers: snooze, delayed submission, purge; fire-at, idempotency key, attempts, dead-letter) · **`outbound_ledger`** (per-account quota/spent/resets-at) · **`dsn_events`** (parsed bounces, linked to originating message) · `triage_scores` (score, model_version, top_features) · `feedback_events` · `ml_models` (per-user, versioned, snapshot lineage) · `rules` · **`contacts`** (addresses, reached-me-at-alias, keys, VIP/block) · **`keys`** (PGP/S-MIME; private keys wrapped, server-side) · **`api_keys`** (scopes, per-account/label restrictions, confirm-mode, rate/budget) · `push_subscriptions` · `audit_log` (incl. agent-key actions, conflict resolutions) · `ai_providers` (per-user LLM config + budgets) · `sync_jobs`.

Design constraints carried from §10: every hot index leads with `user_id`; `messages`/`message_blobs`/`message_labels` are designed for 10⁶–10⁷ rows per user; blob layout is content-addressed under HMAC'd user prefixes so schema migrations never touch blob bytes.

---

## 15. Performance targets (minimal resources, no GPU)

- **Single user:** comfortable on 2–4 vCPU / 4–8 GB RAM (Go + embedded Tantivy keep RAM low). Search p95 < 100 ms; triage inference sub-ms; new mail visible within seconds.
- **~24 users:** ~8 vCPU / 16–32 GB RAM, no GPU. LLM (if enabled) is a separate optional box/profile, never gating core function.
- Shared IDLE + connection pooling, incremental sync, per-user index sharding, in-memory model artifacts, throttled backfill.
- **Storage sizing:** plan blob storage to raw-archive size + encryption/index overhead (a single ~25 GB archive → ~30 GB+; ~24 users with large archives → hundreds of GB). CPU/RAM stay modest; **disk is the resource that scales with mail volume**, so size the volume (and backups) accordingly.

---

## 16. [v0.4] Delivery, versioning & ops

### Versioning & process
- **Semantic versioning** (MAJOR.MINOR.PATCH). **0.x** = pre-release builds toward MVP — **including the 0.5 daily-driver milestone (§17)**; **1.0.0 = MVP + core intelligence** ("v1"); **1.x** = additive features in the v1 line; **2.x**, **3.x** = later majors.
- **Milestones + issues** on the project tracker track everything; each roadmap item becomes an issue tied to a milestone.
- **[v0.4] ADR practice:** `docs/adr/` records every architectural decision (seeded from Appendix A). Cheap now; it's how the open-source version explains itself later.
- **CI/CD from the first commit** (any CI): build, test, lint, security scan, migration checks — plus the sync harness (§4), JMAP conformance client (§13), RLS negative tests (§10), and the restore drill (§11). Pairs well with an AI PR-review step if one is run.

### [v0.4] Schema migrations (policy)
- Tool: **goose / golang-migrate / Atlas — pick one at repo creation** (any is fine; the policy is what matters).
- **Forward-only; expand-and-contract** for anything touching large tables: add new column/table → dual-write → **online background backfill job** (never a blocking `UPDATE` in a migration) → cut reads over → drop old in a later release.
- Backups record the schema version; restore drills exercise migrating a restored older snapshot forward.
- Policy lives as a one-page `docs/migrations.md`.

### [v0.4] Durable job model
One `jobs` table serves every timer in the product — snooze wake-ups, delayed submission (Undo Send / send-later), Trash purges, DEK lazy re-encryption, model snapshots. Single dispatcher; **catch-up on boot for missed timers**; idempotency keys; bounded retries into a dead-letter state that is surfaced, never swallowed.

### [v0.4] Observability (spec'd, not vibes)
Per-account: sync lag seconds, IDLE connection state, action-queue depth, writeback failure rate. Global: index cursor lag, job-queue depth/dead-letters, model versions + feedback counts, outbound ledger levels, attachment-scanner signature version + age (ADR-027 §5c). Prometheus metrics + structured logs; the user-facing subset surfaces in the account health panel (§12).

### Ops
- Docker Compose profiles `minimal` / `enhanced(+Ollama)`; **Caddy** proxy; **OIDC provider** (e.g. Authentik); Postgres; embedded search; filesystem blobs behind a `BlobStore` interface (MinIO slots in when >1 disk); IaC via **Ansible** or similar; health probes; encrypted backups (e.g. restic) per the §11 restore unit.
- A **Migadu provider profile** encoding folder names and send-as rules (send-as + ManageSieve **confirmed available**; concurrent-IMAP cap ⚠ pending a support answer, §18) for one-block onboarding. The provider layer is generic so any IMAP/SMTP host slots in.
- **[v0.4] Onboarding flow (per user):** add-account wizard → credential validation → **SPECIAL-USE (RFC 6154) folder detection with manual mapping fallback** for nonstandard folder names → initial-sync progress with usable-before-complete search (§7) → multi-hour backfill runs visibly in the background.

---

## 17. [v0.4] Roadmap (semver milestones — aligns with feature matrix)

**0.x (toward MVP):** repo + CI/CD scaffolding (incl. sync harness + RLS negative tests + MIME golden corpus); tantivy-go spike; Go core skeleton; single-account IMAP sync + writeback; canonical store + change log; auth via Authentik; minimal web shell.

**[v0.4] 0.5 — "daily-driver" milestone (new, deliberate):** single user, real accounts, read/search/label/reply/archive — **no ML, no crypto, no multi-user UI** — but the **multi-user data model, isolation, content-hash identity, and change log from day one** (those are the one-way doors; the admin conveniences are not). Purpose: a month of dogfooding real mail reorders the rest of the roadmap better than any review. Nothing ships to other users at 0.5.

**1.0.0 — MVP + intelligence ("v1"):** multi-account sync + unified inbox + threading; **mbox/maildir/Takeout bulk archive import** (rate-limited, resumable); hybrid labels (IMAP-sync default, keyword mapping per §6); embedded full-text search; compose/reply with correct (envelope-derived) identity; delayed-submission engine (Undo Send UI); **outbound ledger + DSN pipeline**; OIDC multi-user + full isolation (with negative tests); T0 rules + **T1 ML priority inbox + label suggestions + online learning + explainability + snapshots/eval harness**; internal contacts (derived); security baseline incl. restore drill; Gmail-like web app + PWA + triage-aware Web Push; **RFC 8620/8621-conformant API subset, fully documented**.

**1.x — v1-line features:** PGP sign/verify/encrypt (server-side keys) + **WKD key authority** + Autocrypt + key pages (+ S/MIME if its cost is accepted, §9); **ManageSieve filter + vacation-responder editing**; **archive cleanup assistant** (triage-clustered bulk actions to tame large imports); **live Gmail IMAP via OAuth** (operator-owned OAuth client — see §18); snooze/mute/pin polish; attachment-text search; MCP server + scoped API keys (hardened defaults, §13a); per-identity signatures/templates; send-later UI; saved searches/smart folders; per-user data export (format already defined, §10).

**2.x:** optional LLM (Ollama) features (summaries, smart reply, NL search, cold-start labeling); **native iOS/Android apps** (cross-platform, offline); **CardDAV contacts sync** (Nextcloud/Apple/Google); one-click + bulk unsubscribe; semantic search; S/MIME if deferred from 1.x.

**3.x:** **desktop thick client (Tauri)**; shared mailboxes/delegation; CalDAV/calendar hooks (iTIP accept/decline); optional Python `mailroom-trainer` if a model-quality ceiling is hit; DNS `OPENPGPKEY` publishing.

---

## 18. [v0.4] Still open

1. **Provider support questions (Migadu, for the reference profile; ask now):** (a) concurrent-IMAP connection cap per mailbox/account — pool stays conservative (one IDLE + small worker pool) until answered in writing; (b) **SMTPUTF8/EAI support** — if absent, compose must produce a clear error for internationalized addresses.
2. **tantivy-go spike** (§3/§7): pass/fail against the stated criteria; Bleve is the fallback. Gates final search design, not the data model.
3. **License:** **RESOLVED — AGPL-3.0-only** (ADR-011). Decided pre-publication, as required: relicensing after outside contributions requires CLAs or unanimous consent. The deciding argument was reversibility — as sole copyright holder, AGPL → permissive stays available later, whereas permissive → AGPL does not — reinforced by the fact that AGPL matches §0's anti-SaaS, anti-egress thesis, and that the adoption case for permissive is weak for a deployed application nobody links against. Accepted cost: some organizations ban AGPL outright. Applied as `LICENSE` plus one-line `SPDX-License-Identifier` headers.
4. **Gmail ongoing-delivery path:** **RESOLVED for v1.96 (ADR-046, ADR-047; live sync is #3)** — live Gmail and Outlook/Microsoft 365 accounts connect over OAuth through **operator-owned OAuth clients**, one per instance, as this item always intended. The Google project is published "In production" and left unverified under the personal-use exemption (fewer than 100 users, who click through Google's "unverified app" warning); "Testing" mode is not usable because its grants expire after 7 days. Microsoft uses one app registration for personal and organizational accounts (`/common`). Live Gmail syncs All Mail once and maps Gmail labels (ADR-047, which deviates from §4's label-folder example for live accounts only), with a daily download budget and newest-first history. Takeout remains the recommended path for archives over about 10 GB, and a Gmail forward to another mailbox (with the §4 header-analysis fallback) is unaffected for anyone who keeps it. The operator guide documents the client setup, or Gmail-live becomes a permanent support generator.
5. **Multi-user storage sizing:** large archives make disk the scaling resource — confirm the volume/backup budget for ~24 users before that phase.
6. **S/MIME library + true cost** — decided at crypto-release time (§9).

---

## Appendix A. [v0.4] Decision log (seeds `docs/adr/`)

| ADR | Decision | Status |
|---|---|---|
| D1 | Message identity = per-user SHA-256 content hash; Message-ID is a hint only | **Resolved** (§4) |
| D2 | True RFC 8620/8621 core semantics for the API subset; extensions as capability URNs | **Resolved** (§13) |
| D3 | Per-user, per-type monotonic change log drives `/changes`, index, client sync | **Resolved** (§13/§14) |
| D4 | Content-addressed blobs; per-user DEKs; versioned envelope header; chunked AEAD for large attachments | **Resolved** (§9/§10) |
| D5 | File-based master key behind `KeyProvider`; boot unseal; re-wrap-only rotation; offline escrow | **Resolved** (§9) |
| D6 | tantivy-go pending spike (pass/fail criteria in §3); Bleve fallback; Mailroom owns the query AST | **Resolved-pending-spike** (§7) |
| D7 | ULIDs for API object ids | **Resolved** (§14) |
| D8 | Forward-only migrations, expand/contract, online backfills; tool picked at repo creation | **Resolved** (§16) |
| D9 | Deletion = per-canonical with per-provenance writeback; Trash retention 30d default; agent deletes always soft | **Resolved** (§4/§13a) |
| D10 | Isolation design: forced RLS + identity-scoped tx wrapper + negative tests + per-user DEK backstop | **Resolved** (§10) |
| D11 | License = AGPL-3.0-only (SPDX identifiers, not header banners) | **Resolved** (§18.3; ADR-011) |

---
*v0.4 — design-review fixes incorporated. Private until stable through v1. Still a document to argue with.*
