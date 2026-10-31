<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Connecting your first real mailbox

Pointing a Mailroom instance at a mailbox that holds real mail: in what order,
with what checkpoints, and how to stop. Its sibling is
[`backup-restore-runbook.md`](backup-restore-runbook.md), which covers getting the
data back; this one covers getting there in the first place. (The file keeps its
original name, `production-cutover-runbook.md`, so existing links still work.)

**The rule this document is written to:** every step either has an observable
check, or says plainly that it has none. A step with neither is a wish.

**What has actually been exercised** is marked ✅ MEASURED. Everything else is
derived from the code and is marked as such. The distinction matters more here
than anywhere else in the project: a runbook that reads as if it had been
rehearsed, when it has not, is worse than one that admits which half is theory.
Where a figure depends on your mailbox, the text says to measure it rather than
quoting a number from someone else's.

---

## 0. Before you start: what this runbook assumes

- **One user, one provider** for the first account. UIDPLUS and SPECIAL-USE are
  assumed to hold on the provider; if they do not, the bare-EXPUNGE fallback and
  manual folder-role mapping come into play.
- **Size the mailbox before you size the host.** Message count and byte total are
  an estimate until the census in step 1.2 replaces them with a measurement.
- A small throwaway instance is the same software against a small mailbox. What
  you measure there does **not** transfer by ratio: the first sync is dominated by
  round trips and body fetches, not by message count alone.

---

## 1. Before the mailbox is attached

### 1.1 A provider-side backup, taken independently of Mailroom

Take it from the provider (or by IMAP-copying the account elsewhere) **before**
Mailroom logs in at all.

> Mailroom's own backup protects Mailroom's copy of the mail. It is not a copy of
> the source, and it is not what you want if the thing that goes wrong is
> Mailroom.

**Check:** the artifact exists off the host and you have opened it (listed its
folders, opened one message). An unverified backup is a hypothesis —
`backup-restore-runbook.md` exists because of that.

### 1.2 Census the real mailbox

The census ships in the Mailroom image as `/mailroom-census`, so it runs anywhere
the image can be pulled — it does **not** need the Mailroom host, only the
account's password. No accounts file exists yet at this point, so pass the
password in a throwaway file readable by the image's non-root user:

```
docker run --rm --entrypoint /mailroom-census \
    -v "$PWD/imap-password:/run/census-password:ro" \
    <image> -host <imap host> -user <address> \
    -password-file /run/census-password
```

⚠ **`--entrypoint` is not optional.** The image's ENTRYPOINT is `/mailroom` and
docker appends arguments, so without it this starts a Mailroom server instead of
the census. Delete the password file afterwards. (On a host that already runs the
stack, `cmd/mailroom-census`'s doc comment gives the compose form that reuses the
stack's own accounts secret.)

Read-only: it opens each mailbox with EXAMINE and never writes. Record per-folder
counts and bytes, and size the host's disk from the byte total plus room for the
search index and the blob store.

**Check:** the report's folder list matches what the provider's own UI shows. A
folder missing here is a folder Mailroom will not sync.

⚠ `-messages` prints one line per message (folder, UID, size). That is what lets
you attribute a later difference between the provider and the database instead of
guessing at it.

### 1.3 Bring the stack up clean on an EMPTY database first

Before any real account exists, on the Mailroom host:

```
docker compose -f deploy/compose.yml up -d
```

The bootstrap chain is `role-bootstrap-owner` → `migrate` → `role-bootstrap-app`
→ `mailroom`, and it has **first-boot-only** failure modes — role creation,
ownership, the migration chain applying from zero — that a long-lived instance
can never exhibit again. Finding them now costs nothing; finding them with real
mail in flight costs a maintenance window.

**Check:** all services healthy, `/healthz` and `/readyz` both `ok`, and
`SELECT max(version_id) FROM goose_db_version` equal to the migration head of the
tag you deployed.

⚠ **Check that the signature updater is actually updating — a fresh
`clamav-db` volume is itself a first-boot-only failure mode.** Before
v1.79.0, freshclam could not initialise on an empty volume, and the container
reported *healthy* anyway, because the image keeps it alive with no updater
running. An instance could scan its whole first sync of real mail against the
signatures baked into the image, weeks old by then, without anyone noticing.
"Healthy" is not the check, and the process list is:

- `docker top <clamav-signatures>` lists a `freshclam` process, and its
  arguments are ours (from v1.79.0 it runs as the image's `clamav` user, with
  no added capabilities);
- within a few minutes, `docker exec <clamd> clamdscan --version` shows a
  `daily` version dated within the last day or two, **not** the date the image
  was built;
- within a minute of boot (v1.79.1 or later),
  `time() - mailroom_scanner_signature_published_timestamp_seconds` on
  `/metrics` is under a day or two. Zero, and therefore a huge age, means clamd
  reported no publication date.

⚠ **Migration 00055 pins the PGP root on first boot** (trust on first use): the
first successful boot records the root it sees and requires it at every boot
afterwards. Make sure the escrowed root is the one mounted, before that first
boot, not after.

### 1.4 `MAILROOM_OPERATOR_SUBJECT` must be the IdP's opaque `sub`

Nothing in the stack can validate this for you, and getting it wrong is the worst
failure in this document because it looks like success:

- the wrong subject is provisioned at boot and **owns the declared accounts**, so
  the instance cheerfully syncs the real mailbox into a user nobody can log in
  as;
- your real login then either **is refused** (when the emails match —
  `app.users.email` is UNIQUE while `upsert_login` resolves only on
  `oidc_subject`), or **creates a second, empty user** (when they differ). Neither is
  silent: the refusal logs `login refused: this email already
  belongs to another user` at ERROR, the split logs `login created a NEW user`
  at WARN, and both name `token_subject` and `operator_subject`.

**How to get the right value:** do one trial login against the identity provider's
app binding and read the `sub` from the `"login provisioned"` log line, or take
it from the IdP's own admin view. Then set it and restart.

**If it is already wrong and mail has synced,** do NOT just correct the variable:
the next boot collides on the email and refuses to start. Follow
`deploy/README.md` §4, *A wrong operator subject*, which moves the user that
owns the mail with `deploy/rebind-operator-subject.sql` first.

**Check:** after setting it, one login lands in a user that can see the declared
accounts in the accounts screen.

### 1.5 The host prerequisites

At-rest volume encryption and TLS termination, each with a way to verify it —
not "it was configured". If your deployment is managed by automation, this is
that automation's half.

### 1.6 The provider must let this mailbox SEND as its aliases

A catchall domain RECEIVES at every address, and that is all a catchall grants.
Sending as a derived, per-correspondent address is a separate permission, and
without it every reply from such an address is refused at the envelope sender:

```
mail from: 550 "5.7.1 sender address rejected: not allowed"
```

Without that permission, sends from a derived alias are refused this way; once the
mailbox is allowed to send as any address on its domain, a send arrives at an
external mailbox with the alias in `From` untouched. ⚠ **The permission is
per MAILBOX, not per domain** — a second mailbox does not inherit the first one's
setting. Grant it at the provider for this mailbox before §3.

**Check:** in §3 step 3, one send from a derived alias to an external address
**arrives** — read it in the recipient's mailbox. A send Mailroom reports as
queued proves nothing about this; the refusal happens at the provider.

---

## 2. The shadow period: ingest-only (ADR-028)

Attach the real account with **writeback disabled** for it:
`accounts.writeback_enabled = false`, or the instance-wide
`MAILROOM_WRITEBACK_DISABLED`.

⚠ The instance-wide switch is on for `1`, `true`, `yes` or `on` and off for `0`,
`false`, `no` or `off` (any case). **Any other value — `enabled`, `y`, a typo —
stops the server at boot** with `MAILROOM_WRITEBACK_DISABLED must be one of …`
(before v1.73.1 it read as OFF, silently, which is the failure this guard
replaced). After setting it, confirm the boot log carries the WARN
`writeback is DISABLED instance-wide` before attaching anything real.

### What "healthy" looks like

Two lines matter, and they are different lines:

```
"msg":"account synced","pass":0,"account_id":"…","name":"…","mailboxes":6,"mailbox_errors":0
"msg":"sync pass complete","pass":0,"accounts":1,"disabled":0,"synced":1,"failed":0
```

- **`"synced":1` on the pass-complete line is the check**, together with
  `mailbox_errors:0` on the per-account line. The absence of a "sync disabled"
  line is not a check: an account that never ran produces no line at all, and a
  pass with nothing due logs `"accounts":0,"synced":0`, which looks calm and
  means nothing happened.
- ⚠ **The instance logs JSON, not text.** A grep written against the e2e
  harness's text format matches nothing and reads as silence.
- **`mailboxes_deferred` is not an error count.** It appears on both lines from
  v1.78.0 and counts mailboxes this pass did not ask the server about,
  because the server refused a `UID SEARCH` for rate limiting. Nothing was
  concluded about them and they are due again next pass; they are deliberately
  *not* in `mailbox_errors`. A non-zero value with a `mailboxes deferred to the
  next pass` WARN beside it means the account is being paced, not failing. If it
  stays non-zero pass after pass on an account with many folders, lower
  `MAILROOM_IMAP_SEARCH_RATE` (default 30/min) — the provider is answering fewer
  searches than that.
- **`mailboxes_observed` is the SEARCH rate** (v1.79.0): folders whose live UID
  set the server answered this pass, one `UID SEARCH` each. Summed over an hour
  it is what the pacing budget is spent on. ⚠ **Do not read it off
  `mailboxes_reaped`**, which counts only folders that went through a reap. An
  observation with nothing to prove is stamped but not reaped, and reading it that
  way undercounts.
  Compare the observed-per-hour rate with the default `MAILROOM_IMAP_SEARCH_RATE`
  (30/min): it should sit well inside it.
- **`mailboxes_unchanged` and `observations_skipped` are work not done, on
  purpose** (both lines). On a server with `LIST-STATUS` the pass's one
  `LIST` also reports every folder's status. `mailboxes_unchanged` counts
  folders not `SELECT`ed because nothing moved since the last poll; they are
  still counted in `mailboxes`, so that figure does not drop.
  `observations_skipped` counts due folders not `SEARCH`ed because nothing moved
  since the last *real* observation. Those are not in `mailboxes_observed`, and
  every folder still gets a real one at least once a day. Both read **0 on the
  first pass after the upgrade** (nothing is recorded yet to compare against)
  and on a server without `LIST-STATUS`; that is the old behaviour, not a fault.
  `duration_ms` (per-account line only) is the engine's wall time for the
  account, the number this is meant to bring down. Measure the effect on
  `duration_ms` and on `mailboxes_observed` per hour on your own mailbox.

### What to expect, so a long wait is not mistaken for a hang

- The **first sync can take hours** on a multi-gigabyte mailbox. It is one account, one connection,
  sequential by design (spec §4: stay conservative with provider connections).
- The **attachment sweep runs afterwards** and takes its own hours. It shares the
  sequential job queue with outbound mail.
- ✅ MEASURED: once caught up, a pass reports
  `messages:0, flag_updates:0, bodies_fetched:0`. That is the steady state, not
  the backfill — do not read the backfill's duration from it.
- On an account with many folders a steady-state pass can take most of the
  polling interval, because every folder is selected each pass and
  SEARCH pacing adds the rest. A pass that outlasts the interval is not a hang;
  the next one simply starts later.

### ⚠ What the shadow period does NOT do

**Anything you do in Mailroom during the shadow period never reaches the
provider — not even after you lift it.** Verified in `internal/store/writeback.go`:
a suppressed intent is **terminal** and is never claimed; it is a ledger entry
recording a change Mailroom *would* have made. Lifting writeback does not drain
the ledger.

The exception is narrow and deliberate: a caller with a **deterministic**
idempotency key (the purge expunge, the drawer sweep's re-enqueue) upgrades its
suppressed row to `pending` when it re-enqueues while writeback is live.
`Email/set` is **not** such a caller — it mints a fresh action id per call — so
user actions are exactly the ones that do not come back.

**Consequence for the plan:** treat the shadow period as "read and judge, do not
file". Triage done there lives only in Mailroom (labels, filing and trash are
local-wins anyway), and the provider's copy will not match. If you want the
provider to match, do the work after the switch.

### Stop conditions

Go back (section 4) if any of these hold after the first full sweep:

- `mailbox_errors` is non-zero on repeated passes (`mailboxes_deferred` is a
  separate field and is *not* a stop condition — see above);
- dead-lettered intents appear (`app.writeback_intents` with `state='dead'`) —
  during the shadow there should be none at all, since nothing is being applied;
- message counts in the database do not converge toward the census figures;
- memory ceilings are being hit (see the per-container limits in `deploy/compose.yml`) rather than
  merely approached.

---

## 3. The switch

Do these **in order**, checking each before the next. Each step widens what
Mailroom may do to the real mailbox.

1. **Writeback on, one account.** Set `writeback_enabled = true` (and clear the
   instance switch). Restart. Confirm the boot log no longer carries the disabled
   WARN.
2. **Prove one benign round trip before anything else.** Flag a single message
   in Mailroom, wait for the writeback pass, and confirm the flag on the provider
   — by census or another client, not by Mailroom's own view.
   ✅ MEASURED (v1.72.0+): an action reaches the provider in about a second,
   because an enqueued intent wakes the drain at once; read it off
   `applied_at − created_at` in `app.writeback_intents`. ⚠ Expect longer when a
   sync pass already holds the account — the drain waits for it rather than
   racing it. Report both numbers, never just the best.
   ⚠ **On a many-folder mailbox, "longer" can mean minutes, and that is not a
   failure.** A pass is serialised with the drain on purpose, so a flag set at a
   random moment lands up to one pass-length later. Measure it on this step and
   note the number for your own instance.
3. **Sending.** Only once reads and writebacks are behaving, and only after
   §1.6. Verify with the round-trip address, and check what the recipient
   actually sees in the `From` header rather than what compose believed it
   selected. Send once from a **derived alias** to an external address and
   confirm it arrives: that is the one send §1.6 exists for.
4. **The destructive background jobs, last.** `MAILROOM_TRASH_RETENTION`
   governs an unattended purge that hard-deletes Trash after 30 days by default
   and expunges the provider's copy. **Choose that number deliberately at
   cutover**; it is the one setting in this document whose default quietly
   destroys mail.

---

## 4. Backing out

### 4.1 Rolling back the application (no migration crossed)

✅ MEASURED end to end (v1.71.0 → v1.70.0 → v1.71.0):

```
cd <mailroom-checkout> && git checkout vX.Y.Z          # the OLD tag
cd deploy && MAILROOM_IMAGE=<image-ref>:vX.Y.Z \
  docker compose -p <project> -f compose.yml up -d
```

- Down-and-up takes seconds with the old image already on the host, and the same
  to roll forward again, with every container healthy afterwards and
  `0 WARN / 0 ERROR` in the logs.
- ⚠ **Pin `MAILROOM_IMAGE`.** A floating tag is whatever was pulled last — after
  a deploy that is the NEW build, so an unpinned rollback silently runs the
  version you were rolling away from.
- ⚠ **A rollback to ≤ v1.70.0 recreates the plaintext-DSN volume**
  (`<project>_runtime-secrets`). Those compose files write the composed DSNs
  there at every start; v1.71.0 does not use it. **Delete it again after rolling
  forward** — it is the thing v1.71.0 removed.
- ⚠ **A job kind the old build does not know** (e.g. `change_log_compact`, added
  in v1.71.0) logs `no handler registered for job kind` and **retries**, by
  design, so it recovers when you roll forward. It dead-letters after 5 attempts,
  so after a long rollback check for `state='dead'` rows and re-enqueue.
- Mail that arrives while you are on the old build is re-ingested by the next
  sync, so a short gap self-heals.

**Check:** `--version` from inside the running container reports the tag you
intended; counts unchanged; `/healthz` and `/readyz` ok.

### 4.2 Rolling back across a migration

The path is the dump/restore in `backup-restore-runbook.md` (ADR-008 —
forward-only migrations mean there is no `goose down` for production): restore
the pre-upgrade dump, **create the four roles before restoring** (`pg_dump` does
not carry them), then start the old tag.

⚠ **Still not rehearsed end to end — say so rather than assume it.** This section
was written at a release that crossed no migration. What IS proven is each half on its own: the restore half by
the restore drill against a real artifact (runbook §5a there), and the old
tag against its own schema is simply what it ran before the upgrade. What is not
is the whole sequence timed as one, under pressure. Walk it on a throwaway copy
before cutover if the cutover itself will cross a migration.

### 4.3 Rolling back the *decision*

Going back to the previous client as the source of truth is not a deploy step,
and it is the case worth thinking about before you need it:

- **Reads:** nothing to undo. Mailroom never deleted anything it read.
- **Writes made after the switch** are already on the provider — moves, flags,
  labels-as-keywords. Going back means living with them, or undoing them in the
  other client.
- **What is NOT reversible:** an expunge. Once the purge (or a Trash expunge)
  removes a message from the provider, no Mailroom state brings it back. The only
  recovery is the provider-side backup from step 1.1.
- **The shadow-period ledger** never reached the provider at all (section 2), so
  there is nothing to undo from it.

---

## 5. After

- **If you used a throwaway instance for rehearsal, decide what it is for**,
  rather than letting it drift: a staging target for the next tag, or retired. If
  it holds a real provider connection, remember that the provider's
  concurrent-connection budget is shared with the main instance.
- **Record the real numbers** that only your own mailbox can produce: per-container
  `memory.peak` after the first full sweep, and the steady-state pass duration.
- **Check the schedule actually runs**: the weekly `change_log_compact` job, the
  drawer sweep, the purge, and the backup schedule. A schedule nobody
  observes is not a schedule.

---

## 6. Consolidating the provider's folder tree (`consolidate-folders`)

A mailbox filed by years of server-side rules can carry many per-sender
folders under INBOX, dated `Archive/<year>/<month>` folders, and more that
are already empty. `mailroom-admin consolidate-folders` flattens that **on the
provider**, over IMAP: every message in the folders matching `--from` moves into
`--into`, then the folders matching `--delete-empty` are deleted once empty.

⚠ **It writes to the real mailbox, and it writes NOTHING to Mailroom's database.**
Mailroom's sync sees the moves the way it sees any other client's: moved mail
arrives as *new* mail in the destination, its bodies are **downloaded again**, and
ingest deduplicates them by content hash onto the messages Mailroom already holds.
Expect the passes after a large run to fetch as many bodies as were moved.

⚠ **It does not go through writeback, so ingest-only mode (section 2) does not
stop it.** On a shadowed account `--apply` refuses unless `--despite-ingest-only`
is also given. That flag is the decision; nothing else asks.

**Before you start:** take a **fresh provider-side backup** (section 1.1). Mailroom's
own backup is of Mailroom's copy, not of the provider's folder tree, and nothing in
Mailroom can undo a DELETE on the provider.

**1. Read the report.** Without `--apply` nothing changes anywhere:

```
mailroom-admin consolidate-folders --user-id <user> --account-id <account> \
    --from 'INBOX/*' --into INBOX \
    --delete-empty 'INBOX/*' --delete-empty 'Trash/*' --delete-empty '<pattern>'
```

Patterns are IMAP LIST patterns: `*` crosses the hierarchy delimiter, `%` does not.
For every matched folder the report prints the server's `STATUS` (MESSAGES,
UIDNEXT), the number of live UIDs Mailroom holds for it (`*` where the two differ,
`-` where Mailroom holds no row), and the plan: `move N`, `nothing to move`,
`delete`, `delete after its move`, or `REFUSE (<reason>)`. Totals are **per
category** — moves (folders and messages), empties, deletions now and after a
move, and refusals by reason — never one number.

It always refuses: INBOX; any folder with a SPECIAL-USE attribute (`\Sent`,
`\Drafts`, `\Trash`, `\Junk`, `\Archive`, `\All`, `\Flagged`) or a Mailroom role; a
source that is, or contains, the destination; a folder whose child folders are not
also being deleted; and every move when the destination does not exist or the
server does not advertise MOVE (it never falls back to COPY + EXPUNGE).

**2. Apply, bounded at first.** Add `--apply` (and `--despite-ingest-only` if the
account is shadowed), with `--max-folders 5` for the first run. Each move is one
`UID MOVE 1:*`, verified by `STATUS` before and after: the source must read 0 and
the destination exactly what it held plus what moved. On any mismatch the run
**stops** and prints the folder, the numbers, and what it had already done.
Deletions run after every move, leaves before parents, each re-reading `STATUS`
immediately before `DELETE` and deleting only at 0.

A destination that receives new mail mid-move (INBOX does) reads *higher* than
expected and stops the run. That is safe: check the folder, then re-run.

**Pause the account's sync for the run** (`mailroom-admin disable-account`, then
`enable-account` afterwards). Not for safety — a pass that reaps an emptied source
before it polls the destination soft-deletes those messages and the next poll
retracts it (`RetractVanished`) — but because that window shows as churn in the UI
and the change log. Only the before/after case is covered by the live test; a
sync running *during* the moves is not.

**Expect about two STATUS commands per folder, unpaced.** Only `UID SEARCH` is known
to be rate-limited at the provider. If STATUS turns out to be limited too,
the run stops at the refused command and a re-run continues; `--max-folders` in
batches of 50 keeps each run short either way.

**3. Re-run until the report says `Nothing to do`.** Every decision is re-read from
the server, so re-running after a stop or a `--max-folders` bound continues where
the provider is now. Then run the dated-archive flatten the same way:

```
mailroom-admin consolidate-folders --user-id <user> --account-id <account> \
    --from 'Archive/*' --into Archive --delete-empty 'Archive/*'
```

**Check:** after the next sync passes, the account's message count in Mailroom is
unchanged and no message was soft-deleted (the sync log's `messages_vanished` stays
0); the deleted folders disappear from Mailroom's folder list. The live test
`TestLiveConsolidationKeepsMailroomsMessagesWhole` (`internal/mailsync`) proves
exactly this against Dovecot and PostgreSQL: 8 messages before and after, 7 bodies
re-fetched and deduplicated, 0 vanished.

⚠ **MEASURED: before v1.80.1 this check FAILED for a
large move.** Sync fetched a folder's new mail as ONE `UID FETCH n:*`. Thousands of
messages moved into INBOX made that a download of over a gigabyte; it broke off
mid-stream, and the provider then refused the command (`NO [UNAVAILABLE]`) on every
pass, while the pass before had already reaped the emptied sources. The moved mail sat
soft-deleted (`messages_vanished` in the thousands, `purge_after` 30 days out) until
INBOX could be read. v1.80.1 fetches in chunks of at most 500 and resumes a partial
folder. **Run a large consolidation only on v1.80.1 or later.**

⚠ **Judge "settled" by `mailbox_errors`, not by `bodies_fetched`.** A pass that FAILS to
fetch also reports `bodies_fetched: 0`, so two quiet-looking passes can be two failed
ones. Wait for passes with `mailbox_errors: 0`, then check `messages_vanished` is back
to 0 in the database (`SELECT count(*) FROM app.messages WHERE vanished_at IS NOT
NULL`), not only in the log.

**Then look at the desk.** Mail moved into INBOX is on the desk, however old it is:
a consolidation of thousands of messages puts thousands on it. Day-one setup's age
window (the Settings "Run setup again" flow, or `mailroom-admin`'s day-one command)
archives everything older than the window **from the desk only** — a Mailroom-local
mark, with a preview first and an undo — so the provider's INBOX keeps the mail
while the desk shows what is recent.
