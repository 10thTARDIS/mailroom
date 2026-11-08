# ADR-052: Mailbox state advances when counts change, and Email/get answers header properties

**Status:** Proposed (v1.102.0 beta 3; #114, #115). Both came from the
third-party JMAP client check (`internal/api/jmapclient_integration_test.go`).
No operator decision is needed: neither part changes what is stored about a
message, and the one migration is additive.

## Context

- **#115.** `Mailbox/get` reports `totalEmails` and `unreadEmails`, but moving or
  reading a message does not move the Mailbox state and `Mailbox/changes` lists
  nothing. A client that syncs by state shows stale counts for ever. The counts
  are computed on read (`GetMailboxes`, over `effectiveMembershipSQL`), never
  stored, so no `change_log` row says they moved.
- **#114.** `Email/get` silently drops `header:*` and `headers` properties
  (anything it does not know is dropped). RFC 8621 §4.1.2 requires them, and
  RFC 8620 §5.1 requires `invalidArguments` for a property the server does not
  have.

## Decision: Mailbox state (#115)

1. **Counts stay computed on read.** Storing them would add a second copy that
   every membership write must keep exact. The change log only has to say *that*
   they may have moved.
2. **A count-affecting mutation notes the mailboxes it touches; the store
   appends one `Mailbox` `updated` row per distinct mailbox when the
   transaction ends.** `UserTx` carries a per-transaction set of mailbox ids.
   `NoteMailboxCounts(messageIDs)` resolves the effective mailboxes of those
   messages with one statement and adds them; `Store.UserTx` flushes the set
   through the existing `AppendChanges` (one sequence-range statement plus one
   insert), just before commit. The set is the dedupe: moving 5,000 messages
   across three mailboxes appends three rows, however many statements or
   calls ran. A rolled-back transaction appends nothing. There is no second
   write path; the flush is `AppendChanges`.
3. **Which mutations note.** Anything that changes membership or the seen/trash
   state of a message: Email/set (move, seen and unseen, trash, destroy),
   rules that move, ingest (new message), sync (flags, reaper vanish and
   restore, provenance moves), Gmail label moves and writeback relocation,
   purge and collapse deletes, draft finalisation, account removal. A move
   notes the source BEFORE the write and the destination AFTER it (the
   effective membership is derived, so a note taken at one point sees only one
   side). Flag-only changes that cannot move a count (flagged, answered,
   keywords) note nothing: otherwise every flag toggle would wake every client.
4. **`Mailbox/changes` lists the mailboxes under `updated`.** `updatedProperties`
   (RFC 8621 §2.2) is `["totalEmails","unreadEmails","totalThreads","unreadThreads"]`
   when every row for that mailbox in the window is a count row, and `null`
   otherwise (a rename, a role or scope change) - null is the RFC's own answer
   when the server cannot say.
5. **One migration, additive:** `app.change_log.counts_only boolean NOT NULL
   DEFAULT false` (PostgreSQL 18 stores a constant default in the catalogue, so
   no table rewrite). Existing rows read as "not counts-only", which yields
   `updatedProperties: null`, the safe answer. Forward-only, expand-only
   (ADR-008); compaction is unaffected (it deletes a seq prefix). `db/schema.sql`
   gets the column last.
6. **Cost.** O(mailboxes touched) rows per transaction, never O(messages).
   Each note is one indexed membership query for the ids it is given. The
   per-`(user, Mailbox)` sequence row lock is taken once at flush, so a bulk
   move holds it for the commit only.
7. **Consumers.** `Mailbox/get ids:null` is cached under the `(Email, Mailbox)`
   sequence pair; a Mailbox row only invalidates it sooner, which is correct. The
   search indexer reads `Email` rows only. EventSource polls each subscribed
   type's sequence, so a Mailbox row produces a `Mailbox` state event, which is
   the point. Client cursors for other types are untouched. Tests cover each.

## Decision: header properties (#114)

1. **Forms.** `header:Name[:asForm][:all]` and `headers`. `Name` is matched
   case-insensitively and echoed exactly as asked. Forms and the headers they
   accept (RFC 8621 §4.1.2.1-7): Raw (any; the default), Text (any), Addresses
   and GroupedAddresses (From, Sender, Reply-To, To, Cc, Bcc and the `Resent-`
   forms), MessageIds (Message-ID, In-Reply-To, References, Resent-Message-ID),
   Date (Date, Resent-Date), URLs (the `List-*` set). A form on a header it is
   not defined for is `invalidArguments`, not null. Without `:all` the value is
   the LAST such field in the message; with it, every field top to bottom. A
   missing header is `null` (`[]` with `:all`); an unparseable one is `null`.
2. **Source and trust.** The raw header block comes from the stored message
   through the same `RawSource.ReadRaw` the body properties use: decrypted
   through the existing blob path, whole-blob integrity verified before any
   byte is used. A prefix read would skip that verification, so the blob is read
   in full; `loadBodies` already does this once per message for every body
   property, and header properties share that read. A header-only request skips
   the MIME body walk. The headers of an RFC 3156 encrypted message are the
   outer ones.
3. **Bounds.** Per message: the header block is cut at the first blank line and
   at `maxHeaderBlockBytes` (128 KiB; real headers are a few KiB); a longer
   block is parsed up to the last whole field before the cap and the object
   carries `headersTruncated: true` (a Mailroom extension, present whenever a
   header property was asked), so a bound is reported, never silent. Per
   request: header properties on more than `maxHeaderPropertyIDs` (200) ids are
   `requestTooLarge`, and more than 64 header properties are `invalidArguments`.
   A message whose blob cannot be read comes back with `bodyUnavailable: true`
   and no header values, as for body properties.
4. **Unknown properties are refused.** Any property name that is neither in the
   known set nor a well-formed header property is `invalidArguments` naming the
   property; it is never dropped. The known set is derived from the object the
   server renders, plus the extension properties `emailGet` adds, and a test
   pins that every rendered key is in it.
5. **Scope.** Email/get is already one scope, `mail:read`, for bodies, addresses
   and every extension property. Header values are the same bytes, so they need
   exactly `mail:read` and an account-restricted key still sees only its
   accounts' messages (the `notFound` path runs before the blob read). No new
   scope.
6. **Index and logs.** Header values are computed per request and returned; they
   are not stored, not passed to the search indexer, and not logged. Errors log
   the message id and cause only, as `loadBodies` does today.

## Consequences

- A client can sync mailbox counts by state, and read any header.
- Email/get with header properties costs one blob read per message (the same as
  a body property). The 200-id bound is what keeps one call from becoming a
  hang; a client with more ids pages.
- `headersTruncated` and `updatedProperties` are new in responses; neither
  changes an existing property.
- A count-affecting write path added later must call `NoteMailboxCounts`. A
  test walks the existing paths so a missed one is a red test, not a stale
  badge.
