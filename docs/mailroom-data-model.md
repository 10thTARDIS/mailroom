# Mailroom — Canonical Data Model & Protocol-State Design (v0.1)
### Companion to `schema.sql` · targets spec v0.4 · pins ADRs D1, D3, D7, D9, D10

This document specifies the semantics the DDL cannot express: message identity with worked examples, the change-log mechanism and its exact JMAP `/changes` behavior, the transaction-wrapper contract that makes RLS real, and the deletion lifecycle. `schema.sql` is the reference DDL; both files become the seed of `docs/` at repo creation.

**Status of the DDL:** applied and exercised. `schema-apply` runs `psql -f schema.sql` against a clean PostgreSQL 18 on every push, `migrate-check` proves the migration chain converges to the same schema, and the RLS negative-test suite (§5) is a required check. This line read "not yet applied to a live PostgreSQL 16" until it was corrected — long after all three jobs existed.

⚠ **"PostgreSQL 16+" below is the DDL's compatibility FLOOR, not the version we run.** ADR-025 moved the shipped stack to 18 (v1.40.1); `db/schema.sql` deliberately keeps the floor at 16. Both statements are true and neither is drift.

---

## 1. Conventions

- **PostgreSQL 16+.** Everything lives in schema `app`, owned by `mailroom_owner` (migrations only).
- **IDs are ULIDs** (ADR D7), generated app-side, stored as the `app.ulid` text domain. Time-ordered (index-friendly inserts), opaque, non-enumerable.
- **Timestamps** are `timestamptz`, UTC.
- **JSONB is for payloads, never for queryable state.** `jobs.payload`, `rules.conditions`, `triage_scores.top_features` are opaque to SQL; anything the app filters or joins on is a column.
- **Every hot index leads with `user_id`** — RLS appends that predicate to every plan, so an index that doesn't lead with it is an index the planner can't fully use.

## 2. Message identity (ADR D1) — the worked examples

Canonical identity is `(user_id, content_hash)` where `content_hash = SHA-256(raw RFC 5322 bytes)`. The raw bytes are hashed exactly as received; no canonicalization in v1 (a transport that mutates bytes creates a second message, which is *correct* — the bytes differ). `Message-ID` is stored verbatim as `message_id_hint` and used only for threading and diagnostics.

These four cases are the acceptance tests for the ingest path:

1. **Gmail label copies.** "All Mail" + `Work` + `Receipts` deliver byte-identical copies → one `messages` row, three `message_provenance` rows. Flags merge (§4); an archive writes back to all three locations.
2. **Mailing-list + direct copy.** You're CC'd directly and via a list; the list rewrote the footer and injected `List-Id`. Same `Message-ID`, different bytes → **two** `messages` rows, threaded together via the `message_id_hint`/`references_ids` graph. Both visible (collapsed in the thread UI if desired — a presentation choice, not a data-model one).
3. **Message-ID spoofing.** An attacker sends mail reusing a Message-ID already in your store. Different bytes → new row. It can *thread* near the original (JWZ uses the hint) but can never merge with, overwrite, or write back to the original. Threading adjacency is the accepted residual risk; identity corruption is impossible.
4. **Cross-account duplicate.** The same message arrives in two of your accounts (you were addressed at both). Byte-identical → one canonical message, provenance in both accounts; "archive" archives it in both. Byte-different (different `Received` headers — the common case) → two messages in one thread. Both outcomes are acceptable; the model never guesses.

**Dedup is strictly per-user.** The unique key includes `user_id`; two users receiving identical bytes hold two rows and two blobs (encrypted under different DEKs — cross-user blob dedup is deliberately impossible, which is the isolation property, not a storage bug).

**Blob relationship:** `messages(user_id, content_hash)` has an FK to `blobs(user_id, content_hash)` — a message cannot exist without its raw bytes. Blob bytes live in the BlobStore at a path *derived* from `HMAC(server_key, user_id)/hash` (derivable ⇒ not stored). Attachments extracted for search/preview become their own content-addressed blobs; identical attachments across messages within one user store once.

## 3. The change log (ADR D3) — one stream, three consumers

Every mutation of a JMAP-visible object appends a row to `change_log` **in the same transaction** as the mutation. This is the load-bearing invariant of the whole protocol layer; it is enforced by the store layer (the only code allowed to touch these tables) plus tests, with constraint-trigger hardening available later if discipline slips.

**Sequence allocation:** `UPDATE change_seq SET last_seq = last_seq + 1 WHERE user_id = $1 AND object_type = $2 RETURNING last_seq`. The row lock serializes writers per (user, type) — the known cost is a per-user hot row, which is a non-issue at ≤ 24 users and revisited only if measured.

**Consumers:**
1. **JMAP `/changes`** (below) — the state string *is* the sequence.
2. **The search indexer** — a cursor over `(user, 'Email', seq)`; indexer lag = "search may be stale" in the account-health panel.
3. **Client sync** (TS client-core offline cache) — same cursors as any third-party JMAP client; no privileged side channel, which is what keeps the API honest.

### 3.1 JMAP state strings — exact semantics

- The state string for type T is the **decimal `last_seq`** for (user, T), rendered opaque to clients (e.g. `"s:184223"`). Clients must treat it as opaque per RFC 8620; we can change encoding without breaking anyone who behaved.
- `Foo/get` and `Foo/query` responses report the current state read **inside the same transaction** as the data read — state and data are always mutually consistent.

### 3.2 `Foo/changes` — the algorithm

Given `sinceState = s`:

1. If `s < change_floor(user, T)` → return **`cannotCalculateChanges`**. The client refetches via `/query` + `/get` (RFC 8620's designed fallback).
2. Else scan `change_log` rows `(user, T, seq > s)` in seq order (covered by the PK), **collapse per object_id**, and bucket:
   - first `created` … last op ≠ `destroyed` → **created**
   - first `created` … last op = `destroyed` → **omit entirely** (client never knew it)
   - first op ≠ `created` … last `destroyed` → **destroyed**
   - otherwise → **updated**
3. Return `newState` = the max seq scanned (or current `last_seq` if capped), honoring `maxChanges` by truncating at an object boundary and setting `hasMoreChanges`.

Properties this buys: exactly-once delta semantics per object between any two states a client has seen; no phantom updates; deterministic under concurrent writes (the scan is over committed, sequenced history).

### 3.3 Compaction

A weekly per-user job (`internal/changelog`, job kind `change_log_compact`) deletes `change_log` rows older than the retention window (default **90 days**, `MAILROOM_CHANGE_LOG_RETENTION`, refused below a 7-day floor) *and* advances `change_floor` to the highest deleted seq — **not** +1: a client sitting exactly at the new floor already has every row past it, by construction, because that seq is by definition the last one compaction deleted, so rejecting `since == floor` would refuse an otherwise-satisfiable request for nothing. Any client syncing less often than the window gets `cannotCalculateChanges` once and refetches — correct, bounded, and cheap. Retention is a config knob; the floor mechanism is what makes it safe to turn.

**The strict-prefix rule.** `created_at` is the transaction's *start* time, not guaranteed monotonic with `seq` — two concurrent writers can commit out of `created_at` order relative to when each took its `change_seq` turn, so a row can be older by `seq` but younger by `created_at` (an inversion). Compaction therefore computes one boundary seq `D` per type from `created_at` (the seq just below the oldest row still inside the retention window, or the newest row that exists at all when none is), then deletes every row with `seq <= D` regardless of that individual row's own `created_at` — never a row above `D`. Deleting straight off "`created_at` older than cutoff" could delete a row above the boundary while leaving one below it, a hole in the contiguous history the `seq > since` scan above assumes. Compaction appends no `change_log` row (it mutates no JMAP-visible object — see §3.4) and never touches `change_seq`.

Consumers must read the floor, the current state, and the `change_log` scan in **one SQL statement**: `UserTx` runs at READ COMMITTED, so separate statements get separate snapshots, and a compaction commit landing between a floor-read and a log-scan would raise the floor and delete exactly the rows a stale floor still promised were available — an incomplete delta with no error. `store.UserTx.Changes` (used by both `Foo/changes` and the search indexer) is written this way for exactly that reason.

### 3.4 What is *not* in the change log

Provenance rows, writeback intents, triage scores, and ledger rows mutate without change-log entries **unless** they alter a JMAP-visible property (e.g. `UpsertTriageScore` emits an `Email updated` entry on *any* change to the score, model version or top features — not only a rescore that crosses the priority threshold — because the extension property changed either way). The rule: *if a conforming client could observe the difference through the API, it's a change; otherwise it isn't.*

## 4. Flags, provenance, and the intent queue

- `messages.is_seen/is_flagged/…` are the **canonical merged view** (JMAP keywords). `message_provenance.remote_flags` records what each IMAP location last reported.
- Merge rule on ingest/reconcile: OR across provenances for sticky-positive flags (`\Seen`, `\Flagged`, `\Answered`); a remote *change* (delta vs. that provenance's previous `remote_flags`) is ingested as a delta and applied to canonical state per spec §4a (remote-wins for IMAP-representable state).
- User actions write: canonical state + change_log + one `writeback_intents` row **per account the message currently lives in** (`liveAccountIDs`, a `SELECT DISTINCT account_id` over `message_provenance`), all in one transaction. The writeback worker applies intents per account (ordered per mailbox), marks `applied`, or dead-letters with `dead_reason` after bounded retries — dead letters surface in the account-health panel, never swallowed.
- Idempotency: `idempotency_key = hash(op, object, payload, origin_action_id)`; replays after crashes are UNIQUE-constraint no-ops.

## 5. The RLS contract (ADR D10) — what makes the policies real

**Roles.** `mailroom_owner` owns objects and runs migrations; the app never connects as it (owners bypass RLS unless FORCEd — we FORCE anyway, belt and suspenders). `mailroom_app` is the only role application code uses. `mailroom_admin` has grants on `users` + admin-plane `audit_log` reads and **no grants at all on mail-content tables** — "admin can't read mail" is implemented as absent privilege + RLS + audit, exactly as spec §9/§10 words it.

**The tx wrapper is the only door.** Application code cannot obtain a raw connection. The store layer exposes exactly:

```go
// UserTx: the ONLY way to touch user data.
func (s *Store) UserTx(ctx context.Context, userID ulid.ULID,
                       fn func(tx *UserTx) error) error
// Internally: BEGIN;
//   SET LOCAL mailroom.user_id = '<userID>';   -- transaction-scoped, pool-safe
//   fn(tx); COMMIT/ROLLBACK.

// SystemTx: for the job dispatcher & reconcilers. mailroom.user_id stays
// UNSET (policies see NULL ⇒ user rows invisible); cross-user work happens
// only through SECURITY DEFINER functions with narrow signatures
// (app.dequeue_due_jobs) that expose job rows, never mail content.
func (s *Store) SystemTx(ctx context.Context, fn func(tx *SystemTx) error) error
```

`SET LOCAL` scopes the setting to the transaction, so pooled connections can't leak identity between requests; `app.current_user_id()` returns NULL when unset, and every policy then evaluates to not-visible. **Fail closed: no identity, no rows.**

**CI negative tests (red build on any failure):**
1. For every user-scoped table: `SELECT count(*)` as `mailroom_app` with no identity set → 0 rows.
2. For every user-scoped table: seeded rows for users A and B; with identity A, B's rows are invisible to SELECT and unreachable by UPDATE/DELETE, and INSERTs carrying `user_id = B` are rejected by `WITH CHECK`.
3. As `mailroom_admin`: `SELECT` on `messages`/`blobs`/`user_keys`/`contacts` → permission denied (no grant).
4. For every API endpoint: authenticated as A, request B's object ids → 404/403, verified against the fixture set.
5. `audit_log`: UPDATE and DELETE as any role → permission denied.
6. Instance-level tables — no `user_id`, and the inverse of "no identity, no rows", because their rows exist *only* in the identity-less context — are listed separately (`systemTables`), must carry no user-scope column, and must have row security forced. Each has its own isolation test. The one today is `app.pgp_root_check`: invisible and uninsertable under any user identity, and insert-only for `mailroom_app`, so the application can neither overwrite nor delete the recorded PGP root.

## 6. Deletion lifecycle (ADR D9)

```
 live ──trash──▶ trashed (trashed_at, purge_after = now()+30d)
   ▲                │            │
   └───restore──────┘            ▼ purge job at purge_after (or user hard-delete)
                              hard-deleted
```

- **Trash** sets `trashed_at`/`purge_after`, emits `Email updated`, and enqueues per-provenance moves to each account's Trash mailbox.
- **Restore** clears both, emits `updated`, moves back (to Archive if the origin mailbox is gone).
- **Hard delete (purge job or explicit):** delete provenance (IMAP expunge intents where permitted) → delete `messages` row (cascades: labels, scores, attachments) → delete now-unreferenced blobs → emit `Email destroyed` → write the audit tombstone `{action: message.purge, object_id, content_hash_prefix}`. The tombstone is the only trace that survives.
- **Agent deletes** (`mail:delete` scope) can only reach the *trash* transition — the purge path is unreachable through any agent surface (spec §13a).
- **User account hard-delete** is a system job walking the same machinery user-wide, then removing the `users` row; admin has no direct DELETE grant on `users` precisely so this path is the only one.

## 7. Table-by-table notes (only the non-obvious)

- **`change_seq` / `change_floor`** — separate tables rather than columns on `users` so the hot row lock never contends with profile reads.
- **`messages.content_hash` vs `dedup_hash`** — two hashes, two jobs, and confusing them is the trap. `content_hash` is IDENTITY (ADR-001): the blob address, the FK hub, unique per user, and two rows sharing one are the same bytes. `dedup_hash` (migration 00023, ADR-018) detects a RE-DELIVERY: `SHA-256(Message-ID, Date, From, To, Cc, Subject + body)`, so it survives the trace metadata a mail path stamps on each delivery. Two rows *can* share it — that is the condition it exists to find — so it is **not** unique, and it is nullable because it is backfilled online (the hash covers the body, which lives in an encrypted blob, so it cannot be a SQL `UPDATE`). NULL means "does not participate", the pre-00023 behaviour. Ingest asks for `content_hash` first because identical bytes are *certain* while an equal `dedup_hash` is an inference; a hit on either records the delivery as another provenance row rather than inserting a second message. The allowlist shape was chosen from measurement, not taste: across 18 real duplicate groups the bodies were byte-identical in **all 18** while nine per-delivery header fields varied across two providers, so subtracting a denylist would have rotted on the next provider.
- **`messages.sent_at` vs `received_at`** — `sent_at` is the Date header (absent or lying is fine, it's display-only); `received_at` (earliest INTERNALDATE) is the sort key and is NOT NULL.
- **`message_provenance.state`** — `stale_uidvalidity` is a **transient** state meaning "retired, awaiting reconciliation", not a history graveyard. When a mailbox generation resets, its live rows are retired (so the invisible-but-unique `provenance_live_uq (mailbox, uidvalidity, uid)` can admit the new generation, and so writeback never targets a dead coordinate), the mailbox is re-fetched, and re-resolution matches messages by content hash into the new generation. Once that re-fetch is **proved complete against the server's own live UID set**, the retired rows are **deleted** and the messages they no longer place anywhere are soft-deleted, retractably (`store.ReconcileRetiredGeneration` — see §6). Keeping them instead was the original design and was wrong twice over: nothing reads them (membership, the flag merge and the writeback target query all filter `state = 'live'`), so a message left holding only retired rows fell out of every folder with **no `change_log` row and no `trashed_at`** — it silently ceased to exist; and they grew without bound on a mailbox that rolls repeatedly.
- **`jobs` vs `sync_jobs` vs `writeback_intents`** — three shapes on purpose: point-in-time timers (`jobs`), long-running resumable runs with progress + cursor (`sync_jobs`), and per-provenance IMAP deltas (`writeback_intents`). Collapsing them was considered and rejected: their lifecycles, indexes, and consumers don't overlap.
- **`submissions`** — the JMAP `EmailSubmission` object *and* the delayed-submission engine: Undo Send = `send_at` in the future + a `jobs` row; cancel inside the window flips `state='canceled'` and cancels the job. The From address is validated against `identities` (fixed match, or `domain_wildcard` suffix match) at creation — spec §5's server-side rule lands here. `idempotency_key` (nullable, unique per user via a partial index) is what makes a client's RETRY of a send safe: a repeat returns the existing submission rather than creating a second one, so an offline action queue can replay the request whose response it never received. Null for the sends that do not need it, which is why the index is partial.
- **`outbound_ledger`** — `(account_id, day)` with atomic `INSERT … ON CONFLICT DO UPDATE SET sent_count = sent_count + 1 RETURNING`; the send path checks the returned count against `accounts.outbound_daily_quota` *before* SMTP and queues (never late-fails) at the cap.
- **`audit_log`** — plain `bigint` identity, not ULID: append-only, never referenced by FK, and sequential inserts are the whole workload.
- **`api_keys.token_hash`** — tokens are shown once and stored hashed; scope enforcement happens at the tool/handler boundary, but rate and budget live in columns so they're enforceable and auditable.
- **`sessions`** — one row per signed-in browser (ADR-048): the cookie's secret is stored only as its SHA-256 (`token_hash`, `UNIQUE`), the provider refresh and ID tokens only sealed (key derived per user from the master key, so no per-row DEK). The per-request lookup is `app.session_for_token(hash)`, a `SECURITY DEFINER` function with an owner policy mirroring `api_keys`; it refuses a revoked row and a disabled or deleted user. No `change_log` rows; withheld from user export. Migration 00083 adds `push_subscriptions.session_id` (nullable, `ON DELETE SET NULL`, `(user_id, session_id)` index) so a device sign-out deletes exactly its subscriptions, and `app.sweep_sessions(p_now)`, a `SECURITY DEFINER` daily sweep that returns a count per reason (zeros included) and is backed by owner SELECT and DELETE policies (`sessions_sweep_select`, `sessions_sweep_delete`).
- **`compose_snippets`** — signatures *and* templates in one table, discriminated by `kind`; see ADR-017 for why not a `signature` column on `identities`. Two non-obvious constraints: a template may not carry an `identity_id` (`compose_snippets_template_has_no_identity`), and "at most one default per `(user, kind, identity)`" needs **two** partial unique indexes rather than one `UNIQUE` — PostgreSQL treats `NULL`s as distinct, so a single constraint would admit any number of unbound defaults. The statement that clears the previous default matches the slot with `IS NOT DISTINCT FROM` so the `NULL` identity is handled by the same code path as a bound one.

## 8. Open items this document creates

1. **Apply-and-test:** `psql -f schema.sql` on PG 16 + the §5 negative suite = the first CI job of the repo. (The DDL is written to be re-runnable at the role level; table DDL assumes a clean database — the migration tool owns evolution from the first commit.)
2. **Store-layer skeleton:** `UserTx`/`SystemTx` + the change-log append helpers are the first Go package worth writing — everything else builds on them.
3. **Constraint-trigger hardening** for the "mutation ⇒ change_log row" invariant: deferred, revisit if a bug ever slips past the store-layer tests.
4. **JMAP `/queryChanges`:** v1 answers honestly with `cannotCalculateChanges` for volatile queries (forcing refetch) except for the stable mailbox-listing query; full query-delta support is a later optimization the schema already permits.
