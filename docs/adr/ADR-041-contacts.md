# ADR-041: Saved contacts, the user's own name and notes, merge by tombstone, and a report-first date backfill

**Status:** Accepted (Forgejo #816, v1.94 server increment; revisits ADR-014's
"if a future feature promotes seen_count/last_seen_at" clause; builds on ADR-040
for keys). Amended for #834 (§4: separating gives back what the merge borrowed,
and the block goes back with the address that had it; migration 00073).

## Context

Contacts are auto-derived from mail: one contact per address, created at ingest
(`store/contacts.go`). A long-used instance showed that nearly every contact was auto-derived: very few
had ever been written to, none had a second address, and the stored
`first_seen_at`/`last_seen_at` were ingest times (every contact read as "seen in
the last 30 days" after a bulk import). Many had no
mail found from or to them at all: co-recipients on other people's mail.

Decisions on #816:

- Only **saved** contacts appear on the Contacts page.
- A contact is saved automatically when the user **sends** to it.
- The user can save or unsave by hand. **Unsaving deletes nothing**: keys,
  history, VIP and block are kept.
- **VIP saves**; blocking does not.
- Contacts can be **merged** by hand and **separated** again.

## Decision

### 1. Schema (migration 00072, expand-only)

- `contacts.saved_at timestamptz NULL`. Non-NULL means saved, and records when.
  A timestamp rather than a boolean costs nothing and answers "since when".
- `contacts.user_display_name text NULL`: the user's own name for the contact.
  Ingest never writes it. The displayed name (`Contact.name`) is
  `COALESCE(user_display_name, display_name)`; the learned `display_name` keeps
  doing what it did (filled once, from mail, when empty).
- `contacts.notes` (already present) is capped at 4,000 characters, and
  `user_display_name` at 200, by the API and by CHECK constraints.
- `contacts.merged_into`, `contact_addresses.merged_from_contact`,
  `contact_keys.merged_from_contact`: the merge record (§4).
- No backfill in the migration. A test pins that (no DML; no DEFAULT on the new
  columns).

### 2. Saved, by whom, and the user's text

- **Ingest** sets `saved_at` when the message is one the user sent (the rule
  that bumps `outbound_count`: From resolves to a verified identity) and the
  contact is not yet saved. A new contact created that way is inserted saved,
  so its `created` row covers it. An existing one gets one Contact `updated`
  row, folded with any other synced change of the same message (name fill,
  reached-me-at). **These are Contact rows, not the Email `created` row**: the
  established ingest convention for contacts (since #37) is a Contact row of its
  own, because Contact is a separate JMAP type with its own state string. A
  re-send to a saved contact changes nothing and writes nothing. A contact the
  user un-saved is saved again by their next message to it: sending is the
  definition of "someone I correspond with".
- **Contact/set** `isSaved` saves (keeping an existing `saved_at`) or un-saves.
  `isVip: true` also saves; `isBlocked` does not. One patch that both marks VIP
  and un-saves is refused (`invalidProperties`) rather than resolved silently.
- **userName** is trimmed; control characters and the directional formatting
  characters (embeddings, overrides, isolates, LRM/RLM/ALM) are removed, because
  in a name shown next to an address they can reorder what the reader sees.
  Empty after cleaning, or null, clears it. Over 200 characters after cleaning
  is refused, never cut.
- **notes** keep newlines and tabs; CRLF becomes LF; other controls and the
  directional characters are removed; surrounding space trimmed; over 4,000
  characters is refused.
- Every edit is one savepoint with exactly one Contact `updated` row when
  anything visible changed and none for a no-op (the comparison is done on the
  cleaned values, so `"  Ex "` after `"Ex"` is a no-op).

### 3. The new properties, and which of them are synced

`isSaved`, `savedAt`, `name`, `userName`, `notes` are synced state: every change
writes a Contact row.

`inboundCount`, `outboundCount`, `firstMailAt`, `lastMailAt` are
**statistics**, and ADR-014 still holds for them: ingest re-sightings move
`seen_count`, `outbound_count` and `last_seen_at` without a change row. ADR-014
asked to be revisited "if a future feature promotes seen_count/last_seen_at to
client-visible state (e.g. a 'last contacted' column the user sorts on)". This
is that feature, and the decision is to keep the rule: the Contacts page sorts
by `lastMailAt` **server-side** (`Contact/query`), which ADR-014 already allows
("clients that want a fresh ordering re-query"), and the contact page re-reads
the contact when opened. Streaming a change row per received message would be
the firehose ADR-014 exists to prevent.

- `outboundCount` is the sum of the addresses' `outbound_count`.
- `inboundCount` is the sum of `seen_count − outbound_count` per address:
  sightings other than the user's own sent mail. **A deviation from the brief's
  "sum of seen_count"**: `seen_count` also counts every message the user sent to
  the address, so the plain sum would double-count the user's own mail as the
  correspondent's. It still counts mail the correspondent was only a co-recipient
  of; the precise "mail from them" count needs the messages table, and the
  contact page's recent-mail list (`Email/query`) is where that lives.
- `firstMailAt`/`lastMailAt` are the earliest `first_seen_at` and latest
  `last_seen_at` over the addresses: message dates once the backfill (§5) has
  run, and the arrival time of new mail afterwards.

The backfill's bulk correction does write change rows (§5): it is a one-off,
it changes values a client may have cached during this very rollout, and it
touches at most a few thousand contacts once.

### 4. Merge and separate: a tombstone plus a per-row home

`Contact/merge {into, from}` moves every address and key of `from` into `into`
and **keeps `from` as a tombstone** (`merged_into = into`). Each moved address
and key records its **home** (`merged_from_contact = from`, set only when NULL,
so a merge of a merge keeps each row's original home). `Contact/separate {id,
addressIds}` moves the named addresses into a **new** contact made from their
home tombstone's own state.

Why a tombstone rather than only a per-address `merged_from_contact` and a
deleted `from`: separating should restore the other person — their notes, the
user's name for them, their VIP and saved state — and that state lives on the
contacts row. Deleting it on merge would make separate unable to restore
anything but addresses. The tombstone holds no addresses and no keys, so every
reader that joins through addresses (autocomplete, `is:vip`, the ingest block
lookup, out-of-office, sender stats, key resolution) never sees it; the readers
that start from `app.contacts` (`Contact/get`, `Contact/query`, `Contact/set`,
merge, separate) exclude `merged_into IS NOT NULL`. Older tombstones that
pointed at `from` are repointed at `into`.

Why a NEW contact on separate rather than reviving `from`'s id: `from` was
reported `destroyed` to every client; RFC 8620 ids are not reused, and a
`destroyed` then `created` of one id is a client-cache hazard for no benefit.

**Combine rules** (merge):

| | result |
|---|---|
| saved | either; the earlier `saved_at` |
| VIP | either |
| blocked | **either** (a block wins); the **earlier** `blocked_at`; the **stricter** destination among the blocked sides (trash > junk). Neither blocked: `into`'s destination preference |
| notes | `into`'s, then a `---` separator, then `from`'s; refused (`notesTooLong`) if the result would pass 4,000 characters, rather than cutting the user's text |
| userName | `into`'s unless it has none |
| learned name | `into`'s unless it is empty |

**Keys follow their contact. Pins stay per address** (`contact_key_pins` is
keyed by address, so nothing moves). Key events hang off the key row and follow
it. A key both contacts hold (same fingerprint: the same person's two addresses
carrying one key is the ordinary case) cannot be two rows (UNIQUE (user,
contact, fingerprint)), so `from`'s copy is merged into `into`'s with
`pgpkeys.Merge` (a renewal or revocation only `from`'s copy carried is kept),
`refused` and `authenticated` are OR-ed (refused is the stricter; an
authenticated sighting for either address is an authenticated sighting for the
person the user says this is), `removed` only if both were, `from`'s history is
moved onto `into`'s row, and `from`'s row is deleted. **That combined key stays
with `into` on a later separate**: the record of which contact it came from is
gone with the row. Merging means the user asserts one person: a key learned
from one address becomes a candidate for encrypting to the other. The per-address
pin is the safety net — a send whose chosen key differs from that address's pin
still stops for ContactKey/decide (ADR-039).

**Separate**: one new contact per home among the named addresses. Addresses
whose home is a tombstone get that tombstone's own state; addresses whose home
is this contact get a copy of its learned name, saved, VIP and block (not its
notes or the user's name, which describe the merged person). A home's keys go
with the last of its addresses to leave. A contact keeps at least one address.

**Separating gives back what the merge borrowed** (amended for #834). As
first shipped, the contact left behind kept its combined state: a learned name
it had borrowed because its own mail carried none, the joined notes (so the
other person's notes ended up on both contacts), VIP and saved OR-ed in, and
the block. Nothing recorded its pre-merge state, so nothing could be undone.
Now each merge writes one `app.contact_merges` row (migration 00073): `into`,
the `from` tombstone, the address rows the merge moved, `into`'s own state
before the merge and the combined state it wrote (two JSONB snapshots, read
back whole, never queried by field; RLS forced, deny by default, `user_id`-led
index, grants like `app.contacts`; no change row of its own).

- A separate that takes back **every** address a merge moved (that is still on
  the contact) **undoes** that merge: each of `into`'s fields (learned name,
  user's name, notes, VIP, saved, block) goes back to its pre-merge value **if
  it still equals what the merge wrote**. A field changed since (the user
  edited the notes, or mail filled the learned name) is kept, and the response
  names it (`keptFields`). The record is deleted.
- A **partial** separate (only some of those addresses) restores nothing on
  `into`, because which person a field came from is then ambiguous, and says so
  (`restored: false`). The record stays; the separate that takes the rest
  completes the undo.
- **Merges of merges** undo from each merge's own record, newest first. Stacked
  on one contact (A then B into C), undoing the older merge while the newer
  stands rebuilds each field from C's pre-merge state combined again with B by
  the same rules, so B's VIP or notes are not lost, and rebases the newer
  record. Nested (A into B, then B into C), the first record's target is the
  tombstone B: when A's addresses all leave, B's tombstone state is restored
  (no change row: it was reported destroyed), so the contact made from B is B
  as it was before A; when B's addresses have already left, the record followed
  B to its new contact, which is restored with one Contact `updated` row.
- A merge made before migration 00073 has no record: its separate behaves as
  first shipped (`restored: false`).

**The block goes back with the address that had it.** This replaces "separating
never unblocks anyone", which left a contact blocked for something only its
merged-in address had done. When a merge is undone with its block unchanged
since, `into` returns to its own pre-merge block state and the separated
contact takes its own block from its tombstone. No address that was blocked
becomes unblocked by this: every address whose own contact was blocked before
the merge is still blocked afterwards, because the block travels with it (if
`into` was the blocked one, its addresses stay on `into` and so does the
block); the only addresses that leave a block are ones that were blocked solely
by being merged with a blocked contact, and they return to the state they had
before the merge. Where the block on `into` is **not** the merge's own product
(the user blocked or changed it after the merge), or the separate is partial,
or the merge has no record, the block rule still runs against the contact the
addresses leave: the separated contact takes that block too, and `into` keeps
it. So a block the user set is never dropped by a separate.

**Change rows**: merge writes Contact `updated` (into) and `destroyed` (from);
separate writes `created` per new contact, `updated` for the one left (its
addresses changed, and its restored fields with them), and `updated` for
another live contact a nested undo restored, only when it changed. One outer
savepoint each, so a caller recovering from an error cannot commit half.

**Isolation**: both contacts are locked under the caller's RLS scope with an
explicit `user_id` predicate; another user's id is `notFound`, as is a
tombstone. Probed in the store suite and the API cross-user suite, each with a
positive control. `app.contact_merges` is in the RLS negative suite (its
fixture seeds one record and its tombstone per user) and has its own
behavioural isolation test.

### 5. `contact-dates-backfill`: report first, `--apply` opt-in

The verb reuses `contact-dates-report`'s measurement exactly (one shared
`trueDates`: `messages.received_at`, From side and sent side), and:

- sets `first_seen_at`/`last_seen_at` to the true dates where the stored one is
  a day or more away (the report's "within 1 d" bucket is left alone, and so are
  addresses with no mail found);
- sets `saved_at` on live contacts with an address whose `outbound_count > 0`
  that are not saved.

Without `--apply` it writes nothing and prints the report, then the plan in the
report's own buckets plus "would save N contacts" (`--list` prints every
change). With `--apply` it prints the same counts as done. Writes go in batches
of whole contacts (default 200 per transaction), each a savepoint with one
Contact `updated` row per contact it changed (a dates-only change counts:
`firstMailAt`/`lastMailAt` are properties). A row whose stored dates moved
between the read and the write (an ingest in between) is skipped and counted
as stale, never forced. A rerun plans 0.

## Consequences

- The Contacts page can list the few saved contacts rather than
  every address seen, sort them honestly by last mail once the backfill has run,
  and keep VIP and block where the user set them.
- Autocomplete is unchanged: it still covers every contact, saved or not, in the
  same order (the text filter now also matches the user's own name).
- `lastMailAt` can drift again only if a large archive is imported later: ingest
  stamps a re-sighting with the arrival time. The backfill is idempotent and can
  be rerun; making ingest stamp the message date instead is a follow-up, not
  done here.
- Tombstones accumulate, one per merge; they are tiny, invisible, and deleted
  with the user. Merge records (#834) accumulate likewise until their merge is
  undone, and go with either contact.
- A merge made on v1.94.0-rc.1 (before migration 00073) has no record, so
  separating it restores nothing on the contact it was merged into; what it
  borrowed has to be edited by hand.

## Alternatives considered

- **A `saved` boolean.** Equivalent for the page; loses "since when" for nothing.
- **Delete `from` on merge, keep only `merged_from_contact`.** Separate could
  then restore addresses but not the person's notes, names or markers.
- **Make the statistics synced** (a change row per sighting). Rejected for the
  ADR-014 reason; the server-side sort makes it unnecessary.
- **Truncate combined notes.** Rejected: silently cutting the user's text is a
  worse failure than asking them to shorten it.
- **Separate into the original id.** Rejected: `from` was reported destroyed.
