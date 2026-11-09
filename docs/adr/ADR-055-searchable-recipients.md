# ADR-055: Searchable recipients and List-Id, stored on the message row

**Status:** Accepted (v1.102.0 beta 6; operator decision Q76, 2026-11-09).
Closes the JMAP gaps left open by #110 (recipient filters), #111 (the `header`
filter) and #113 (the `to` sort).

## Context

Email/query can filter and sort on the sender and the subject because ingest
copies them onto the message row (`from_name`, `from_address`, `subject`).
Recipients and the List-Id header exist only inside the encrypted message file,
so today:

- `to` and `cc` fall back to the tokenising search engine, which is not the
  RFC 8621 substring match and is not delta-stable;
- `bcc` is refused;
- the `to` sort is refused with `invalidArguments`;
- `header` is refused with `unsupportedFilter`.

The operator decided to store recipients and List-Id in searchable form, and
explicitly NOT to build a general header index.

## Decision

### What is stored

Five nullable columns on `app.messages`, all written together:

| Column | Content |
|---|---|
| `rcpt_to`, `rcpt_cc`, `rcpt_bcc` | normalised text: one line per display name and one line per address, lowercased, lines joined by `\n` |
| `rcpt_to_sort` | the first To recipient's display name, else its address (case kept; empty when there is no To) |
| `list_id_key` | the id inside the angle brackets of `List-Id` (the whole trimmed value when it has none), lowercased; empty when absent |

**Text, not arrays.** The filter is a case-insensitive substring match on names
and addresses, as `from` is. `text[]` cannot do a substring match without
unnesting every row; one `text` value per field matches with a single `ILIKE`,
and a trigram index can serve it later without changing the column. Names and
addresses sit on separate lines (as `from_name` and `from_address` are separate
columns) so a match never spans a name and an address. The query text has its
whitespace runs, newlines included, folded to one space, so a query cannot
match across two lines.

**Bounded.** At most 200 recipients per field and 128 characters per display
name are kept; a To list of 5,000 addresses is stored as its first 200. A
substring that is only in the dropped tail does not match. The backfill report
counts the truncated messages, so the bound is visible rather than silent.

**NULL means not filled yet; empty means filled, nothing there.** The five
columns are set together, so `rcpt_to IS NULL` is the whole backfill candidate
set and a filled row is never visited twice. A NULL column matches no
recipient filter and sorts as empty.

**The sort key keeps its case.** The `to` sort lowercases at query time exactly
as the `from` sort does (`lower(...) COLLATE "C"`), because the `i;octet`
collation needs the unfolded value. The stored value is therefore the
unlowered key, not the lowered one.

**Bcc exists only on mail the user sent or drafted.** Compose stores the
message with its Bcc header and removes it only from the bytes handed to SMTP
(`compose.SendPayload`). Received mail never carries a Bcc header, so
`rcpt_bcc` is empty for it. A `bcc` filter therefore finds the user's own sent
mail and drafts, and nothing else.

### The privacy trade-off

These values now sit OUTSIDE the encrypted message file, as the sender and the
subject already do. A database backup, or anyone with read access to the
database, can see who each message was sent to, copied to and blind-copied
(and which list it came from) without the user's key. This was the operator's
informed choice. Row-level security still keeps one user's values from another
user's session, and nothing here is written to logs, the change log or the
search index.

### Indexes

No new index ships with the columns. A substring `ILIKE '%x%'` cannot use a
btree, every query already carries `user_id = ...` (RLS), and the existing
`user_id`-led index (a bitmap scan on `messages_user_msgid`) bounds the read to
one user's rows. pg_trgm is not installed by the test database and was not
needed:

Measured with 30,000 synthetic messages for each of two users (60,002 rows; one
to three recipients per message, a Cc on 30 percent, a List-Id on 20 percent;
every message in one live mailbox), through `QueryEmails` as the application
role with `LIMIT 50` and the total counted, median of 7 runs on a shared
Postgres 18 container (so noisy: the same query varied by a factor of two
between runs):

| Query (30,000 messages of one user) | Median |
|---|---|
| default order (baseline) | 40 ms |
| `from` substring (existing, baseline) | 74 to 244 ms |
| `to` substring, 1,534 matches | 83 to 98 ms |
| `to` substring, no match (worst case, whole user scanned) | 156 ms |
| `cc` substring | 103 to 160 ms |
| `bcc` substring | 72 to 81 ms |
| List-Id present (5,893 matches) | 27 to 31 ms |
| List-Id value | 47 to 58 ms |
| `NOT` of a `to` substring | 110 to 114 ms |
| sort by `from` (existing, baseline) | 65 to 108 ms |
| sort by `to`, either collation | 50 to 70 ms |

`EXPLAIN (ANALYZE)` of the statement shows one bitmap scan on the user's rows
with the `ILIKE` as a filter (30,001 rows removed, 1,594 buffers), and for the
sort a sequential scan of the user's rows plus a top-N heapsort, executing in
45 to 130 ms. The recipient filters therefore cost the same order as the
`from` filter and sort that already ship: linear in one user's mail, not in
the page size or in other users' mail. A trigram index would be added in a later
migration only if a larger measurement justified it, and would lead with
`user_id`.

### The backfill

`mailroom-admin backfill-recipients [--apply] [--user <id>] [--batch N]`.

- **Report first.** The default is read-only: it reads and parses messages and
  reports what `--apply` would write, and writes nothing. `--apply` is opt-in.
- **Per-signal counts, never one total:** messages scanned; would fill or filled;
  with a To, with a Cc, with a Bcc, with a List-Id (so "nothing to do" can be
  told from "the thing measured is always empty"); skipped, by reason (already
  filled; no stored bytes); truncated (a field past the bound); unreadable
  (bytes missing, failed integrity, or undecryptable). An unreadable message is
  counted and left NULL; it is never fatal and never half-written.
- **Batched, online, resumable, idempotent.** It walks one user at a time in
  ULID order in bounded batches, one short transaction each, reading each
  message through the decrypted, integrity-verified raw read. The candidate
  predicate is `rcpt_to IS NULL`; a write is guarded by the same predicate, so
  a second `--apply`, or two overlapping runs, change nothing the first did not.
  Interrupting it loses at most one batch.
- **Change log.** Filling these columns changes no JMAP-visible Email property
  (`to`, `cc`, `bcc` and `header:List-Id` are read from the message file as
  before), so the backfill appends no `change_log` row. A client's
  `Email/queryChanges` state taken before the backfill will not report old
  messages that start matching a new filter; clients re-run the query.

### JMAP

- `to`, `cc` and `bcc` become case-insensitive substring matches on the stored
  names and addresses, compiled by the same `standardConds` as `from`, so they
  work at the top level and inside AND, OR and NOT. They leave the search
  engine's text half entirely (as `from` and `subject` did in beta 2).
- `sort` accepts `to` (both collations, either direction), with the id and
  the date key breaking ties like the other sorts. The session's sort-option
  list gains `to`.
- `header` is supported ONLY for List-Id: `["List-Id"]` matches mail that has
  the header, `["List-Id", "text"]` also requires the id to contain the text
  (case-insensitive). The header name is case-insensitive. Any other header
  name stays `unsupportedFilter`; a `header` array that is not one or two
  strings is `invalidArguments`. The match is on the list id, not on a list's
  descriptive phrase, which is not stored.
- **Delta-stable.** The columns are written at ingest (and by the one-off
  backfill) and never by a later change to the message, so `to`, `cc`, `bcc`
  and `header` join `deltaStableConditions` and the `to` sort is a stable sort.
- **API-key scoping is unchanged.** The top-level account restriction is
  applied by `emailQueryWhere` outside any filter tree, so no recipient
  condition, however it is nested under OR or NOT, can widen a key's view.

### Every insert path

There is one `INSERT INTO app.messages` (`InsertMessage`). Its two callers are
ingest, which also serves import and the archive paths, and
`CreateLocalMessage`, used for drafts and sent copies. Both derive the columns
from the message bytes with one function (`internal/rcptproj`), which the
backfill uses too, so the three cannot disagree.

## Consequences

- Mail stored before the upgrade has NULL columns and does not match the new
  recipient or List-Id filters (and sorts as empty on `to`) until the operator
  runs the backfill. The release notes say so.
- Database growth is the recipient text of each message, bounded as above.
- The tracked JMAP gaps for `header` and the `to` sort leave the third-party
  client job's allowlist.

## References

#110, #111, #113; ADR-052 (header properties on Email/get); ADR-042 (API-key
scoping); RFC 8621 sections 4.4.1 and 4.4.2.
