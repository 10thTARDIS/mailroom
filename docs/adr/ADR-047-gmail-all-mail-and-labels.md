# ADR-047: Gmail live sync is All Mail plus X-GM-LABELS

**Status:** Accepted (#3, v1.96.0; this model, the
pinned go-imap fork, and full-history newest-first import were chosen).
**Deviates from spec §4's worked example (a)** for live Gmail OAuth accounts
only. Implemented in increments 5a (read path, ingest-only) and 5b
(writeback); nothing here is built by increment 2.

## Context

Spec §4 (and ADR-001) give "Gmail All Mail + label folders deliver identical
bytes → one message, N provenances" as a worked example of content-hash
identity. That example is how the Takeout importer already treats Gmail, and
it stays true for imports. For a **live** Gmail account it is the wrong sync
model:

- **Bandwidth is multiplied.** The engine downloads a whole message per folder
  before the hash can deduplicate it (`internal/mailsync/engine.go`). A message
  in Inbox with two labels is downloaded four times, against Gmail's IMAP
  download limit of 2,500 MB/day (verified for Workspace; the consumer figure is
  UNVERIFIED and assumed no higher). Exceeding it suspends IMAP for the account
  **everywhere**, the user's phone included.
- **Labels become folders.** The user's Gmail labels would appear as Mailroom
  mailboxes, and Mailroom labels written back as keywords are invisible in
  Gmail's own apps.
- **Writeback fights Gmail.** Removing a label means `\Deleted` + EXPUNGE in
  the label folder, whose effect depends on the user's "auto-expunge / when a
  message is deleted" IMAP settings; delete-to-Trash per provenance makes every
  other copy vanish and land in the dead-letter queue as noise.
- **Observation cost scales with the label count**: one SEARCH per mailbox per
  pass.

Gmail offers a better primitive. X-GM-EXT-1 gives every message a 64-bit
`X-GM-MSGID`, a thread id `X-GM-THRID`, and its labels as `X-GM-LABELS`
(readable by FETCH, writable by `STORE ±X-GM-LABELS`, searchable). SPECIAL-USE
marks `\All` (All Mail), `\Junk`, `\Trash`, `\Sent`, `\Drafts`, `\Flagged`
(Starred) and `\Important`.

**The blocker:** go-imap v2.0.0-beta.8, which Mailroom pins, cannot parse
X-GM-* FETCH items — its FETCH parser rejects any attribute it does not know
("unsupported msg-att name", `imapclient/fetch.go:805`) and `FetchOptions` is a
closed struct. Without a change to the library the read path cannot even read a
response that contains them.

## Decision

### 1. What is synced

- **Bodies come from `\All`, `\Junk` and `\Trash` only.** These three sets are
  disjoint, so each message is downloaded exactly once.
- Label folders, `[Gmail]/Important` and `[Gmail]/Starred` get
  `sync_scope = 'off'`: listed for role mapping, never fetched.
- The `\Sent` and `\Drafts` mailbox rows stay, because compose resolves them by
  role; their membership comes from the `\Sent` and `\Drafts` labels. The
  existing Message-ID probe already skips the Sent APPEND when Gmail filed its
  own copy.
- **All Mail hidden from IMAP** (no `\All` listed) is refused with an
  actionable error ("turn on 'Show in IMAP' for All Mail"), never worked around.

### 2. Labels map to Mailroom's model

- Reuse the Takeout importer's `classifyGmail` priority logic
  (`internal/importer/gmail.go`) on X-GM-LABELS. Folder: `\Inbox` → the INBOX
  row; no folder-like label → All Mail (the Archive role); plus Sent, Drafts,
  Spam and Trash. `\Starred` → `\Flagged`.
- **User labels become Mailroom labels** (`app.labels`). Labels are user-wide,
  so a same-named label from another account is the same label; writeback goes
  only to the account where the message lives.
- `\Important` and Gmail's categories are not imported: Mailroom has its own
  priority and tabs.
- `X-GM-MSGID` is stored in `message_provenance.gm_msgid` (the column exists)
  and `X-GM-THRID` is a threading hint (ADR-030's hint matching).

#### 2a. Attribution and change semantics (decided in increment 5a)

- **A Gmail user label is the user's own filing: `applied_by = 'user'`.** The
  person put it there — by hand in Gmail's apps, or through a Gmail filter
  they wrote. It is the attribution the Takeout importer already gives the
  same labels, and it makes years of the person's own sorting evidence for
  what learns from filings (learned placement, the label model, rule
  induction). It is not the training-procedure fixed point that keeps `rule`/`desk`/
  `learned` out of the evidence: nothing Mailroom decided is fed back to
  itself. Gmail's own automatic signals are never imported, so they cannot
  masquerade as filings: `\Important`, the category tabs and every other
  system label are dropped, and `\Starred` arrives as the `\Flagged` flag.
  The alternative — a distinct `provider` attribution that learners ignore —
  was rejected because it would throw away exactly the history
  that should be learned from (learned placement is "sorting learned from their
  moves"). The information is still recoverable: `message_provenance.gm_labels`
  records what Gmail reported for each copy, so weighing Gmail filings
  differently later is a join, not an unrecoverable mix.
- **Gmail's CHANGES are applied, not Gmail's state.** Each copy remembers the
  label set Gmail last reported (`gm_labels`); a reading is diffed against it
  and only the difference is applied. A label added in Mailroom, or a move
  made in Mailroom, is not undone by the next sweep of an unchanged Gmail. A
  label Gmail removed is removed whatever its `applied_by`, without recording
  a rule or learned-placement correction (it is a sync observation, not a
  gesture in Mailroom). No writeback intent is ever enqueued from a reading.
- **The folder is a canonical placement.** `\Inbox`/`\Sent`/`\Draft` set
  `app.messages.moved_mailbox_id` to the account's INBOX/Sent/Drafts row
  (archived mail keeps none and is in All Mail by provenance), on the first
  reading and whenever the label set's folder changes. Two guards: a message
  whose bytes also live in ANOTHER of the user's accounts is not placed (a
  placement overrides every location for membership), and a copy found in
  Gmail's Spam or Trash clears a placement this path made, so the message
  shows where it is. A blocked sender's arrival already moved by ingest keeps
  that move.
- **Known gap, accepted for 5a:** ingest's inbox-arrival automations (mute
  diversion, learned placement at ingest) key on the provenance mailbox being
  the INBOX. A Gmail message's provenance is All Mail, so those do not fire
  for Gmail mail yet; the Inbox placement happens in the same transaction,
  after them.

### 3. Writeback uses labels, and never EXPUNGEs outside Trash and Spam

New free-text `writeback_intents.op` values, so no migration:

- archive = `STORE -X-GM-LABELS (\Inbox)` (no MOVE, so the provenance stays
  put); move to Inbox = `+X-GM-LABELS (\Inbox)`;
- label add/remove = `±X-GM-LABELS ("name")`, for labels with
  `mirror_to_imap` (area labels stay local, ADR-031);
- `\Seen` / `\Flagged` = STORE on the All Mail UID;
- delete = MOVE to `[Gmail]/Trash`, so Gmail's 30-day purge lines up with
  ADR-009; Spam / Not spam = MOVE to or from `[Gmail]/Spam`;
- hard delete = EXPUNGE in Trash only.

No EXPUNGE anywhere else, so the model does not depend on the user's Gmail
IMAP settings. Re-resolution after a MOVE uses `UID SEARCH X-GM-MSGID`, which
is exact, instead of the Message-ID probe. Until 5b ships, Gmail accounts are
forced ingest-only (ADR-028).

#### 3a. As built in increment 5b

*(`internal/writeback/gmail.go`; the go-imap patch is patch 3 in
`third_party/go-imap/MAILROOM-PATCHES.md`.)*

- **No new op strings after all.** The intents Email/set already enqueues are
  translated at APPLY time, in a Gmail mode the runner switches on from the
  account row (`auth_kind = 'oauth2'`, `provider_profile = 'gmail'`). One
  place decides what a Gmail write is, and no enqueue path has to know which
  kind of account it is writing for. A label's `±kw` intent now carries the
  label's NAME beside its keyword atom (`{"keyword","label"}`), so a label
  renamed or deleted before the drain still writes what the user did; every
  other account ignores the name.
- **The translation** (copy in All Mail / copy in Trash or Spam): flags →
  `STORE ±FLAGS` (`\Flagged` is Gmail's star) / same; a label →
  `±X-GM-LABELS ("name")` / same; to INBOX → `+X-GM-LABELS (\Inbox)` /
  `MOVE → INBOX`; to All Mail (archive) → `-X-GM-LABELS (\Inbox)` /
  `MOVE → INBOX` then `-X-GM-LABELS (\Inbox)`; to Trash or Spam → `MOVE`; to a user-label view `L` →
  `+X-GM-LABELS (L) -X-GM-LABELS (\Inbox)` (Gmail's own "Move to") /
  `MOVE → L`; to Sent, Drafts, Starred, Important or `[Gmail]` → refused,
  dead-lettered with a reason. A plain keyword (`$Junk`, a client's token)
  stays a keyword.
- **Hard delete never EXPUNGEs outside Trash and Spam.** A permanent delete
  of a copy in Trash or Spam is expunged there; one of a copy anywhere else
  is refused (only draft retirement moves to Trash first, below). `\Deleted` is
  refused as a flag op (Gmail's default auto-expunge makes it destructive),
  and a server without MOVE is refused rather than letting the library fall
  back to COPY + `\Deleted` + EXPUNGE.
- **Draft retirement looks in the Drafts view, never All Mail.** A Gmail
  draft's provenance is All Mail, so the store addresses its header expunge
  there by Message-ID, and the message Gmail filed when the draft was SENT
  carries the same Message-ID in All Mail. On Gmail the draft is searched for
  in `[Gmail]/Drafts` only, identified by X-GM-MSGID, moved to Trash and
  expunged there. The All Mail and Drafts intents for one draft are the same
  message; the second of a pair in one pass is applied as done.
- **The copy is found by X-GM-MSGID before the first write in a pass**
  (`UID SEARCH X-GM-MSGID` in its mailbox), so a message that left All Mail
  and came back at a new UID is written where it is now (`Result.Reresolved`
  counts those). Not found is a retry until the next sync records where it
  went — never a write to the stored UID.
- **A label write is confirmed by reading it back.** The STORE is not silent:
  Gmail's echo (or, with none, a FETCH) must show every added label and none
  of the removed ones, or the intent is retried and then dead-lettered.
  Whether STORE creates a missing label is still UNVERIFIED, so both worlds
  work: an add that fails or does not stick CREATEs its user labels and is
  retried once (`Result.LabelsCreated`). The fake Gmail exercises "STORE
  creates", "STORE answers NO" and "STORE answers OK and ignores it".
- **Echo suppression.** A confirmed label write moves the copy's `gm_labels`
  baseline by exactly the confirmed delta (`RecordGmailLabelDelta`), never to
  the echoed set: a label added in Gmail's own app meanwhile is in the echo
  too and must still be imported. Without it, the next reading would report
  Mailroom's own write as a Gmail change, and with a newer Mailroom change
  still queued it would re-apply the stale state over it. A move into a
  label view records only the `\Inbox` removal, so the next reading gives
  the message the matching Mailroom label, as every other message carrying
  that Gmail label has.
- **All Mail is a Gmail OAuth account's Archive role** (`MailboxForRole`), so
  archiving in Mailroom places the message there and the move's writeback is
  the `\Inbox` removal. The M3 rule treats that placement as the one Gmail's
  Archive folder implies, so a later "back to the inbox" made in Gmail still
  applies. A generic account's `\All` is not used (it is a virtual folder on
  some servers).
- **Area labels stay local** (ADR-031): they have no keyword mapping, so no
  intent exists to write. Renaming or deleting a Mailroom label is not
  written back to Gmail yet (a Gmail label rename is a mailbox RENAME, and a
  deleted label's messages keep it in Gmail).
- **The switch is a sibling of the sync switch, and stays FALSE.**
  `oauthmail.GmailWritebackReady`, mirrored by `store.GmailWritebackReady`
  (a test checks they agree), holds every Gmail OAuth account ingest-only:
  intents are born `suppressed`, Account/set refuses `writebackEnabled`, and
  the runner refuses to dial even for a row that is somehow pending.
  Separate from `GmailSyncReady` so the operator can lift sync first and run
  an account ingest-only (ADR-028's safe mode) while the read path's open
  points are settled, then lift writeback after the live test of the points
  below. Tests run the Gmail path with the hold injected off
  (`writeback.WithGmailWritebackHold`).
- **Hardened after the mail-safety review:**
  - **Draft retirement only destroys an unsent draft.** The Drafts-view
    match must have a label reading that carries `\Draft` and not `\Sent`,
    or the intent is dead-lettered.
  - **Every MOVE out of Trash or Spam is checked.** The message must be
    found by X-GM-MSGID exactly once in the destination, or the intent is
    dead-lettered loudly: a MOVE is a COPY and an EXPUNGE, and a COPY Gmail
    treated as a no-op would otherwise lose the message silently. A copy
    with no X-GM-MSGID is not moved out at all. **Restore to the archive is
    never a MOVE into All Mail**: it is the standard untrash (MOVE to INBOX,
    checked), then `-X-GM-LABELS (\Inbox)` on the All Mail copy.
  - **A message-addressed permanent delete of a copy outside Trash and Spam
    is refused.** Move-then-expunge exists only for draft retirement.
  - **A move into a label view keeps `\Inbox` until the label is
    confirmed.** The remove is not sent unless the add's echo, or a FETCH,
    shows every added label.
  - **User labels named like Gmail system folders are refused** (Inbox,
    Trash, Spam, Sent, Sent Mail, Starred, Important, Drafts, All Mail,
    Chats, Snoozed, Scheduled, and anything under `[Gmail]/` or
    `[Google Mail]/`, in any case): Gmail may read them as the folder.
  - **Labels compare case-insensitively**, as Gmail's names do, and the
    baseline records Gmail's echoed spelling.
  - **An intent's idempotency key names its kind** (`flag:`, `kw:`,
    `label:`), so a raw keyword equal to a label's atom in one update
    cannot swallow the label's intent.
  - **Ambiguous roles are refused:** an account listing two mailboxes for
    one role gets nothing written, and the Trash used for a permanent
    delete must be unambiguously Trash.
  - **UIDPLUS is required** in Gmail mode, so an EXPUNGE can never fall
    back to a plain one.
  - **Purge never promotes an expunge for a held Gmail account**
    (`ExpungeableAccountIDs` applies the hold).
- **Do not replay the hold-period ledger's flag-class rows on flip.** The
  intents born `suppressed` while `GmailWritebackReady` was false stay in
  the ledger when the switch is flipped; nothing promotes them
  automatically (the EnqueueIntent upgrade arm needs a NEW request for the
  same key). They must not be bulk-applied with `writeback-ledger --apply`
  either: a read, a star or a label set weeks earlier would overwrite what
  the person has done in Gmail since. Only new changes made after the flip
  are written back.
- **UNVERIFIED for the live test:** STORE creating a missing label (and what
  CREATE answers for an existing one); `MOVE` from Trash or Spam into INBOX
  and into a label view (each is checked afterwards by X-GM-MSGID); that Gmail
  echoes X-GM-LABELS for a non-silent STORE (a FETCH covers it if not);
  `$Junk`/`$NotJunk` keywords being accepted; label names Gmail normalises
  (a confirmed write would then fail its read-back and dead-letter, visibly);
  and that X-GM-MSGID is the same in All Mail and Trash.

### 4. Change detection

CONDSTORE MODSEQ on All Mail **if** a label change bumps it (UNVERIFIED;
probed with a real account before relying on it). Otherwise a periodic label
sweep, `UID FETCH 1:* (X-GM-LABELS)`, at roughly 50 bytes per message (about
5 MB per 100k messages).

### 5. The go-imap fork

- **A small fork of go-imap with a `replace` directive**, adding X-GM-MSGID,
  X-GM-THRID and X-GM-LABELS. This is spec §3's own policy for the pre-stable
  client library: "pin a commit … contribute fixes upstream where sensible".
  The patch is offered upstream; the fork is dropped when upstream has it.
- **Vendored in-repo, not hosted (decided in 5a).** The module is copied into
  `third_party/go-imap` (v2.0.0-beta.8, its MIT `LICENSE` kept) and go.mod
  says `replace github.com/emersion/go-imap/v2 => ./third_party/go-imap`. A
  hosted fork would be a second repository the build depends on, at a URL
  that must stay reachable and must not identify the operator (the
  portability rule); in-repo, the patch is reviewed in the same pull request
  as the code that needs it and the build needs nothing new. The patch is
  documented in `third_party/go-imap/MAILROOM-PATCHES.md`, every patched spot
  is marked, and the Dockerfile and the restore drill's relevance filter know
  the directory. 5a patches FETCH only; STORE and SEARCH come with writeback
  (5b).
- Mailroom-side the extension stays behind `SyncSession` as an optional
  capability (`Supports(CapabilityGmailExt)`); no go-imap type escapes the seam.
- **Fallback:** if the fork proved unworkable, model (a) is the fallback, with
  its costs accepted explicitly: labels as folders and multiplied bandwidth.

### 6. Pacing and the initial import (both providers)

*(As built for Gmail in 5a — `internal/mailsync/gmail.go`; Microsoft follows
in increment 6.)*

- **The budget** is per Mailroom account over a ROLLING 24 HOURS,
  `MAILROOM_GMAIL_DAILY_DOWNLOAD_MB` (default 2000; there is no "off"),
  metered in `app.account_sync_status.download_hours` (25 one-hour buckets,
  all summed: a byte stays counted until at least 24 hours after the end of
  its hour, so the window is conservative by up to one hour and never short —
  with 24 buckets, a full budget at 00:01 and another at 24:00 would have put
  twice the budget inside one true 24 hours)
  and persisted after every FETCH command, so a restart cannot reset it.
  - **Rolling, because Gmail's window is UNVERIFIED** (calendar day — in which
    time zone — or rolling 24 hours). A calendar-day meter would allow twice
    the budget across its midnight (2,000 MB at 23:00 and 2,000 MB more at
    00:00); a rolling one is safe under either reading. Bytes are charged at
    the time they are downloaded, not at the pass's start.
  - **Per Mailroom account, not per Google identity.** The same Google account
    connected to two Mailroom accounts (two users, say) has two meters, and
    Gmail would see the sum. Not guarded in 5a.
  - It counts message-body bytes (new mail, history and gap refetches) **on
    receipt, before ingest**, so a body whose ingest then fails is still
    charged; and when a FETCH command fails part-way, the RFC822.SIZE already
    known for every requested body the callback never saw is charged too,
    because closing the command drains them off the wire.
  - The label sweep's metadata (~100 bytes a message) is not metered and is
    what the 500 MB margin to Gmail's 2,500 MB is for.
  - A per-account override was not built: it would be a JMAP-visible Account
    property with its own change rows, and nothing yet needs it.
- **The pauses are asked about before any download.** A pass first asks
  ingest whether it would refuse every message (storage quota full, scanner
  not answering PING); if so the pass stops before fetching a body. The
  generic engine only learns that from the first failed ingest, after a
  chunk has been downloaded and drained.
- **Sized before fetched.** Each chunk's RFC822.SIZE is read first (no
  bodies) and the chunk is taken only as far as the budget allows, so a day
  never overshoots; the one exception is a single message larger than the
  whole allowance on a day nothing else was downloaded, taken alone rather
  than blocking the mailbox forever.
- **History may spend 90% of the budget**; the rest is kept for new mail and
  gap refetches, which are taken first on every pass.
- **Newest first, by chunk.** A mailbox's first sync sets its cursor to
  UIDNEXT−1 and its floor (`app.mailboxes.backfill_floor_uid`) to UIDNEXT.
  History is walked downward from the floor in chunks; within a chunk the
  server answers in its own order. The floor is persisted per chunk.
- **The gap backfill is bounded by the floor**: a gap is a UID at or
  above it. Below it is history not yet imported. Gap refetches are sized
  before they are fetched and taken only as far as the budget allows; a
  mailbox with a gap left over stays unverified and unreaped, as any
  incomplete view.
- **A Gmail mailbox whose UIDVALIDITY changes is not re-fetched** (that
  would be the whole account at once, outside the budget): nothing is
  retired or reaped and the mailbox reports an error every pass until an
  operator decides.
- **Progress** is computed in `Account/getHealth` (`gmailImport`: messages on
  the server and imported, bytes imported, today's meter, the budget, and an
  ESTIMATED remaining size and ETA once there is an average to estimate
  from). Computed, so no change rows.
- **Stop = the account's existing sync switch** (`Account/set
  syncEnabled:false`; `mailroom-admin disable-account`). The floor and the
  cursors are durable, so switching it back on resumes exactly where it
  stopped. A separate "pause history, keep new mail" control was not built;
  it would be a new Account property for a need nobody has yet.
- **Gmail accounts hold no IDLE connection**: the watcher would SELECT INBOX,
  a label folder. They poll on the gmail profile's 5-minute floor.
- **Label views are marked `sync_scope = 'off'`** (except INBOX, Sent and
  Drafts, which hold placements), so clients render them as the frozen,
  never-fetched folders they are. They still appear in the mailbox list;
  hiding them is a client decision for later.
- **Migration 00079's index is a plain `CREATE INDEX`**, not CONCURRENTLY,
  inside goose's per-file transaction like every migration here (00060
  precedent, on a larger table). On existing rows it indexes nothing.
- **Label changes**: the CONDSTORE pass asks for X-GM-LABELS too, and the
  label sweep (`MAILROOM_GMAIL_LABEL_SWEEP_INTERVAL`, default 6h, 0 disables)
  re-reads FLAGS and X-GM-LABELS for every imported UID, no bodies, recording
  its progress per chunk (`label_sweep_next_uid`) so an interrupted sweep
  resumes instead of starting over. Keep the sweep on until the live test
  confirms MODSEQ covers label changes.
- **A missing X-GM-LABELS is NO reading, never "no labels".** go-imap gives
  a message nil labels, without an error, when the response was truncated,
  malformed, or split across two FETCH responses (the error, if any, comes
  only at the command's close, after earlier messages committed). Read as an
  empty set it would strip every label and placement, and the next sweep
  would re-add them as fresh `user` filings. So nil is carried as nil end to
  end: the copy is ingested or refreshed without a label reading, and
  `ApplyGmailObservation` refuses a nil set.
- **Label names are sanitised** (invalid UTF-8 replaced; NUL and C0/C1
  controls removed): modified UTF-7 can encode a NUL, which PostgreSQL text
  rejects. Labels are applied under a savepoint and a failure is logged, so a
  label that cannot be stored never blocks its message. Names are not
  Unicode-normalised: Mailroom keeps the exact name Gmail sent, which is what
  5b must write back.
- **A Gmail placement dies with the Gmail copy.** When the reaper drops an
  account's last live copy of a message (a sent or discarded draft, a
  delete-forever, a Trash purge), a placement in that Gmail account's INBOX,
  Sent or Drafts is cleared and the message is judged like any other
  (vanished, or a member elsewhere). Without this, every such message stayed
  in Drafts or INBOX forever: the reaper skips placed messages, and
  `RetireCanonicalPlacements` needs provenance in the label view, which
  never exists.
- **Gmail moves only what Gmail placed.** A folder change from Gmail
  overwrites the placement only when it is empty or is the one the previous
  Gmail folder implied; a move the user made in Mailroom stays.
- **The census** is `mailroom-admin gmail-census` — a CLI, not a JMAP method:
  it is the operator's pre-flight before switching a person's mailbox on, it
  runs for minutes on a large mailbox, and it must run while the sync hold
  stands. It EXAMINEs only All Mail, Spam and Trash, fetches only
  RFC822.SIZE, INTERNALDATE, X-GM-LABELS and (for the dedup preview)
  ENVELOPE, and reports per signal, stating its bound when it samples.

- **A per-account daily byte budget** in the provider profile — Gmail default
  2,000 MB/day, below the 2,500 MB limit, operator-overridable — counted from
  the `BodyBytesFetched` the engine already measures.
- **Full history, newest first.** The first sync starts the cursor near
  UIDNEXT, so new mail flows within minutes; history backfills downward in
  budgeted chunks below an explicit per-mailbox backfill floor (without the
  floor, the gap backfill would treat all of history as gaps and fetch it
  unpaced).
- Progress is messages done out of the server's count, **with an ETA and a
  Stop control**, on the existing sync-jobs surface. Takeout stays the
  recommended path for archives over about 10 GB.
- The connection pool stays small: Gmail allows 15 simultaneous clients per
  account.
- **Report first:** before the first real import, a per-account dry-run
  census (messages, bytes, labels, and the would-be folder and label
  assignment counts per signal).

### 7. Until 5a: connected, but held with sync off

From increment 3 (the connect flow) until this model ships, a connected Gmail
OAuth account is held with sync OFF (ADR-046 §10, "The Gmail sync hold"):
the generic engine would download every label folder with no budget, the
very pattern §6 exists to prevent. `Account/getHealth` reports
`syncHold.code = "gmail_sync_pending"`, and switching sync on is refused.
**5a built the model, the budget and the census, and deliberately left
`oauthmail.GmailSyncReady = false`**: the UNVERIFIED points below are settled
by a live test, and writeback is 5b. The model is exercised in
tests with the hold injected off (`syncrunner.WithSyncHold`); behind every
door, the sync resolver itself refuses a held account
(`syncrunner.ErrSyncHeld`). Flipping the constant is still the one line that
lifts the hold. Gmail writeback has its own hold
(`oauthmail.GmailWritebackReady`, mirrored by `store.GmailWritebackReady`):
while it is false every intent for a Gmail OAuth account is born `suppressed`
whatever its `writeback_enabled` column says, and switching writeback on is
refused. 5b built the writeback (§3a) and left that switch false too.

## Consequences

- One download per message, labels that round-trip with Gmail's own apps, and
  writeback that cannot be changed in meaning by a user setting.
- Mailroom carries a fork of its IMAP library until upstream accepts the
  patch, and must re-apply it on every go-imap bump.
- ADR-001 example (a) still describes imports and still holds for any Gmail
  account connected with an app password through the generic path; this ADR
  governs only Gmail OAuth accounts with the `gmail` profile.
- The UNVERIFIED points — MODSEQ on label changes, STORE creating a label, the
  consumer bandwidth figure, labels with "Show in IMAP" off still appearing in
  X-GM-LABELS, Gmail's post-auth CONDSTORE, the exact wire form of system
  labels (atom vs quoted; both are parsed) — are settled with a real account
  before the hold is lifted. 5a's tests run against a scripted fake Gmail
  built from Google's documentation, not against Gmail.

## References

- #3.
- ADR-001 (content-hash identity), ADR-009 (deletion), ADR-028 (ingest-only),
  ADR-030 (threading hints), ADR-031 (area labels local), ADR-046 (OAuth).
- Spec §3 (pinned client library), §4 (Gmail handling), §18.4.
