# ADR-053: One Activity view, read from the records that already exist

**Status:** Proposed (v1.102.0 beta 5; #45). No operator decision is needed:
the view adds no sign-in log, shows no message subject, and the one migration
is two additive indexes.

## Context

Mailroom already records what happens to a user's mail and access, in six
places, each with its own list or none at all. A user who asks "what did my AI
tool do, who read my encrypted mail, what was deleted for good, which devices
signed in" has to look at four pages and a database. #45 asks for one merged,
filterable list, per user, read from those records with no second write path,
and an operator-side equivalent through the admin CLI.

## Source inventory

Every source is user-scoped under `FORCE ROW LEVEL SECURITY` with a
`user_id = app.current_user_id()` policy for `mailroom_app`, so a `UserTx` read
sees only the caller's rows.

| Source (table) | Time | Actor | Object | Retention today |
|---|---|---|---|---|
| AI-tool calls (`api_key_events`) | `at` | the key (`key_id`, name from `api_keys`) | the key; `pending_id` | none: kept until the key or user is deleted |
| AI-key lifecycle (`audit_log`, `object_type = 'ApiKey'`, action `api_key.*`) | `created_at` | `user` or `system` | the key id | none (append-only) |
| Decryptions (`pgp_decrypt_audit`, ADR-037) | `decrypted_at` | the user's own session (or a key, through the same path) | `message_id` (no foreign key) | none |
| Purges (`audit_log`, action `message.purge`, ADR-009 tombstone) | `created_at` | `system` (the purge job) | `object_id` = the purged message id; only a hash prefix is kept | none |
| Sign-ins (`sessions.created_at`) | `created_at` | the user | the session | a session is deleted 7 days after it ends, or after 45 days unseen (ADR-048 s7) |
| Sign-outs and ended sessions (`sessions.revoked_at`, `revoked_reason`) | `revoked_at` | `signed_out` / `signed_out_elsewhere` (the user), `refused` / `expired` (the provider or idleness) | the session | as above |
| Learned-placement bulk moves (`audit_log`, `object_type = 'PlacementBackfill'`, `placement.backfill.move/undo`) | `created_at` | the operator-run backfill | the batch id | none |
| Rule bulk apply (**no record today**) | n/a | n/a | n/a | see Gaps |

Considered and left out: `feedback_events` (a learning signal, not an action),
`writeback_intents` (an internal queue), `learned_placements` (one row per
message, the state of a decision, not a record of one), `oauth.*` audit rows
for mail-account connections (not in #45's list; one more branch if wanted).

### Gaps and how each is filled

1. **Rule bulk apply writes nothing durable.** `Rule/apply` and its undo change
   labels (visible through `change_log` as Email updates, which carry no actor)
   and leave no record that a bulk action happened. Filled by appending one
   `audit_log` row (`rule.apply` or `rule.apply_undo`, `object_type = 'Rule'`,
   `object_id` = the rule, `detail` = counts only) in the same transaction as
   the apply, from the existing `audit_log` writer. Same table, same policy:
   not a second write path.
2. **No index serves a per-user, time-ordered read** of `audit_log` (it has
   only its identity key) or `api_key_events` (`(user_id, key_id, at)` is keyed
   by key first). One migration adds both, expand-only (ADR-008):
   `audit_log (user_id, object_type, created_at DESC, id DESC)` and a partial
   `api_key_events (user_id, at DESC, id DESC)` over exactly the rows the view
   shows (see Privacy). `pgp_decrypt_audit` and `sessions` already lead with
   `user_id, time`.
3. **Sign-in history is short.** `sessions` is not a sign-in log: an ended
   session is swept after 7 days. The view reports what the table holds and
   says so (see Bounds). A permanent sign-in log would be a new write path and
   a retention decision; it is not added here and stays an open option.
4. **Actor for a decryption** is not stored; the entry says "decrypted", not
   by whom. The decrypt path is session-only for the web and key-reachable only
   where ADR-037 allows, and the per-key list already shows a key's call.

## Decision

### The merge: one SQL `UNION ALL`, keyset on (time, kind, id)

One statement, one transaction, under `UserTx`. Each branch selects
`(at, kind, id, ...)` for one source, restricted by `user_id`, the filters and
the cursor, ordered `at DESC, id DESC` and `LIMIT n+1`; the outer query orders
the union by `at DESC, kind DESC, id DESC` and takes `n+1`. Every branch is
bounded by `n+1` before the union, so the statement never reads more than
`branches x (n+1)` rows, however large a source is.

- **`kind`** is the fine branch name (`ai_tool`, `ai_key`, `decryption`,
  `purge`, `sign_in`, `sign_out`, `placement`, `rule_apply`); it is the
  tie-break between sources that share a timestamp. `source` is the coarser
  filter group (`ai_tool` = both AI kinds, `decryption`, `purge`, `session` =
  sign_in and sign_out, `bulk` = placement and rule_apply).
- **One entry per backfill batch.** A placement backfill writes one audit row per
  message moved; that branch groups them by (batch, direction) and reports the
  count, timed at the batch's last move. It reads that user's backfill rows
  (operator-run, few) rather than stopping at `LIMIT n+1` before grouping; its
  output is still bounded by the limit like every other branch.
- **Ended sessions.** A sign-out entry covers `signed_out`, `signed_out_elsewhere`,
  `refused` and `expired`; `replaced` (a new sign-in taking a session's place) is
  left out as noise.
- **The id** is text for ULID sources; `audit_log`'s bigint id is zero-padded
  to 20 digits so text order equals numeric order, and the branch orders by the
  bigint itself.
- **Cursor** is `(at, kind, id)`, opaque to clients (base64 of
  `unix-micro|kind|id`), validated server-side. Per branch the predicate is
  built from the branch's own constant `kind`, so it stays an index
  condition: `at < c` when the branch's kind sorts after the cursor's,
  `at <= c` when before, `(at, id) < (c, i)` when equal. Entries sharing a
  timestamp are neither skipped nor repeated.
- Rejected: per-source queries merged in Go. It repeats the cursor logic five
  times in a second language, and a truncated source could drop an entry that
  belongs on the page. The SQL form is one statement a test can probe.

### Filters, page size, and the bound

- `sources`: any of `ai_tool`, `decryption`, `purge`, `session`, `bulk`
  (omitted = all; an unknown value is `invalidArguments`; a source not selected
  has its branch left out of the statement). `since` / `until` are UTC dates
  (RFC 3339), inclusive-exclusive on the entry time, `since` < `until`.
- `limit` 1 to 100, default 50. Above 100 is `invalidArguments`, not clamped.
- **No total.** A count over five tables is the expensive part and would
  imply a retention window the records do not share. The response carries
  `next` (null at the end), `limit`, and the oldest time actually covered by
  each short-retention source, so the page can say "showing entries from N
  onward" instead of implying a total. The web page says "Showing the most
  recent entries" and offers "Load more" while `next` is set.

### API

A dedicated read method, like `ApiKey/getEvents` and `SignedInDevice/get`, not
a `/query` + `/get` pair: an entry has no stable object identity (it is a row
of a log, merged from several), so there is nothing to `/changes` and no state
string. `Activity/list` under a new capability `urn:mailroom:activity`.

- Arguments: `{accountId, sources?, since?, until?, before?, limit?}`.
- Result: `{accountId, list: [entry], next, limit, covers}`.
- An entry: `{id, kind, source, at, outcome?, keyName?, method?, count?,
  reason?, object: {type, id, exists} | null}`. Text is built by the client
  from `kind` and these fields (strings stay in the web i18n catalogue), not
  composed by the server.
- **Not a JMAP-synced object.** It appends no `change_log` row (it writes
  nothing) and has no `/changes`, the same choice as `SignedInDevice`.
  A client re-reads when the page opens and on "Load more".
- **Links** are returned as `object.exists`, computed in the same statement
  (`EXISTS` under RLS) for messages and keys. A purged message, a deleted key
  and a sign-in have `exists: false` or no object; the entry stays, its link
  goes.

### API keys cannot read it

`Activity/list` is session-only (`sessionOnly(...)` in the scope table),
default-deny for keys, and OAuth grants are API keys (ADR-050), so they are
refused too. Reason: the list records what keys and other sessions did,
including refusals and anomalies. A compromised or misbehaving tool must not
be able to read the record of its own detection, learn which of its calls
were flagged, or see when the person last signed in and from where. A dedicated
read scope can be added later; nothing needs it now. A refusal is audited like
any other (`session_only`).

### The operator CLI

`mailroom-admin activity [--since 24h|7d|YYYY-MM-DD] [--source ...] [--user ID
[--limit N]] [--cap N]`. Report-only; writes nothing.

- **Default (instance-wide):** counts per source and kind over the window,
  per-kind and never one total, with the oldest entry present per source, so
  "none" is distinguishable from "the source keeps nothing that old". Built by
  running the same store read per active user (`ListActiveUserIDs`, the
  fan-out the background loops already use, then one `UserTx` each) and summing:
  no new `SECURITY DEFINER` function, no cross-user policy, no new grant. A
  per-user `--cap` (default 5,000 entries) bounds the work, and a user who hits
  it is reported, so a lower bound never reads as a total. The `mailroom_admin` role is deliberately not used.
- **Per-user detail only on request:** `--user ID` prints that user's merged
  entries (the same read and shape as the web), because an operator already
  acts per user through `UserTx` in every other admin command. It prints
  keys' names and message ids but, as in the web, no message content.
- Instance events that carry no user (`user.hard_delete` tombstones, user
  id NULL) are not readable by the application role and are out of scope here;
  the existing delete-user report covers them.

### Privacy

- No decrypted content ever appears (ADR-037): a decryption entry is "a
  message was decrypted (body | attachment) with key `<fingerprint tail>`" or
  "decryption failed", with the message id for the link and nothing else.
- **No message subject, sender or snippet is shown or stored for this view:**
  none of the sources stores one (the purge tombstone deliberately keeps only a
  hash prefix). A link to a message that still exists opens it, where its
  subject is shown by the viewer under the viewer's own rules.
- AI-tool entries show the key's name, the method and the outcome. The client
  IP and request summary in `api_key_events` stay on the per-key page.
- **AI-tool entries are actions and refusals, not reads.** A tool reading mail
  all day writes thousands of `ok` events that would bury a purge. The view
  shows a key event when `outcome <> 'ok'` (refused, rate-limited, pending,
  approved, anomaly, ...) or the method is a `/set` or it deleted or sent
  something; every call stays on the per-key page. The same predicate is the
  partial index's, so it is cheap.
- A decryption by an AI tool (where ADR-037 allows it) is an ordinary
  `pgp_decrypt_audit` row and appears under Decryptions.

### Retention

Follows each source as it is today; the view neither extends nor shortens
any. `api_key_events`, `audit_log` and `pgp_decrypt_audit` are kept for the
life of the account; `sessions` for 7 days after a session ends (live ones
for as long as they are used), which is why `covers` reports the oldest
time present per source. Account deletion cascades as it does now.

## Consequences

- One migration (two indexes) and one new `audit_log` writer for rule apply;
  no new table, no new role, no `SECURITY DEFINER`.
- The definer count test does not move.
- Rule applies done before this version are not in the view (they left no
  record); the empty state and the per-source note say so.
- Sign-in coverage is as long as `sessions` lives. If a longer sign-in
  history is wanted it is a separate decision with its own retention.

## Verification

- RLS isolation for every branch, through the store and the API, in the
  isolation suite (data-model s5).
- Keyset: rows sharing a timestamp across pages with no duplicate or gap.
- Probes, each red then reverted: drop one branch's user predicate; break the
  tie-break; let a key through.
- A deleted object keeps its entry and loses its link.
- The axe surface for Settings > Activity in light and dark.

## References

ADR-008, ADR-009, ADR-037, ADR-042, ADR-048, ADR-050; data-model s5.
