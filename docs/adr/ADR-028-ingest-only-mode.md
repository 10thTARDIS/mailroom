# ADR-028: Ingest-only mode is a per-account writeback switch, and it declares which local state is authoritative

**Status:** Accepted (Forgejo #521, milestone v1.70.0 "Ready for a real inbox";
does **not** supersede or amend ADR-020)

## Context

The cutover plan for a primary mailbox is to run Mailroom against
it and *observe* — sync, search, classification, threading, the desk — while the
ability to write to that mailbox is **structurally impossible**, not merely
unused. "We won't click anything" is not a control.

No such mode exists. The only account-level switch is `sync_enabled`
(`internal/store/accounts.go, ListSyncableAccounts`), which stops ingest too, and
`mailboxes.sync_scope` is a *per-mailbox, pull-side* control that the writeback
path never reads. Verified rather than assumed: `sync_scope` does not appear
anywhere in `internal/writeback/` nor in `internal/store/writeback.go`, and its
one operational read is `internal/mailsync/engine.go:395`.

### Two facts that the design is built on, both measured

**1. There is exactly one production path that mutates a remote mailbox.**

```
internal/writeback/engine.go → internal/sync/writer.go → internal/sync/session.go
```

Every other occurrence of `StoreFlags`/`Copy`/`Move`/`Expunge`/`Append` in the
tree is a test simulating a third-party client. `SyncSession` additionally has
no `Create`/`Delete`/`Rename`/`Subscribe` at all, so no code path can create,
delete or rename a remote folder. `internal/compose` does not import
`internal/sync`: the draft mirror (`compose.go`) and the Sent copy
(`send.go`) both *enqueue* an APPEND intent rather than dialling.

The sole production construction of an `IMAPWriter` is
`internal/writeback/runner.go`, inside `DrainAccount` — which already returns
early, **before** `r.dial`, when nothing is pending. That is the chokepoint.

⚠ **No longer the only path (#647).** `mailroom-admin consolidate-folders` moves
whole folders and DELETEs folders on the provider through `sync.FolderAdmin`, a
separate interface obtained only from `sync.DialFolderAdmin`; `SyncSession` is
unchanged and still has no `Create`/`Delete`/`Rename`. The command is
operator-run and report-first, it does not pass through writeback, and so this
chokepoint does not stop it. It honours this ADR's switches explicitly instead:
on an account with `writeback_enabled = false`, or with
`MAILROOM_WRITEBACK_DISABLED` set, `--apply` refuses unless
`--despite-ingest-only` is given.

**2. Suppressing writeback does not give "local wins". It gives "local wins
until the server next speaks about that message."**

This was the assumption that had to be measured, because two quite different
designs follow from the two answers. The split is not add-versus-remove; it is
**which table the mutation landed in**:

| Local state | Where it lives | What ingest does | Outcome |
|---|---|---|---|
| `\Seen`, `\Flagged`, `\Answered`, `\Draft`, `$junk`, raw keywords | `app.messages` flag columns + `keywords` | Recomputes from live provenance and **overwrites** (`internal/store/ingest.go:348`) | **Server wins** |
| Labels | `app.message_labels` | Never reads or writes it | **Local wins, permanently** |
| Move / filing | `messages.moved_mailbox_id` | Ingest never writes it, and it *overrides* provenance for JMAP membership (`jmap_email.go:1557`). One reaper path does clear it — see below | **Local wins, permanently** |
| Trash | `trashed_at` / `purge_after` | Never reads or writes it | **Local wins, permanently** |

⚠ **"Never reads or writes it" was too strong for the filing row**, and the
correction matters because the whole contract rests on this table.
`RetireCanonicalPlacements` (`internal/store/reaper.go:619`) *does* write
`moved_mailbox_id`, setting it to NULL — but only for a message whose
`local_mailbox_id` already equals it and whose provenance agrees, i.e. once the
override has become redundant. So the **outcome** column is right (local filing
is never overridden by the server) while the wording was not, and the difference
would matter to anyone reasoning about this table rather than reading it for
reassurance.

`mergeFlags` (`internal/ingest/ingest.go:1281`) folds the flag sets of every
**live provenance** row; the local `app.messages` row is not an input. The
`IS DISTINCT FROM` on the update (`internal/store/ingest.go:348`) is a
change *detector*, not a conflict guard — local-says-seen versus
server-says-unseen *are* distinct, so it writes unseen.

There is **no** "local pending change, don't clobber" concept anywhere.
Confirmed by grep, not assumed: neither `internal/ingest` nor
`internal/mailsync` reads `app.writeback_intents`, and the only protection a
local change has today is *temporal* — writeback runs before the sync pass in
the same per-account sequence (`internal/writeback/runner.go`, the Runner doc comment). Remove the
writeback leg and nothing is left.

Crucially the overwrite is **latent, not scheduled**. Under CONDSTORE the flag
pass re-fetches only messages whose **server-side** MODSEQ advanced
(`internal/mailsync/engine.go:528-533`). So a shadow user's `\Seen` survives
until some *other* actor — their phone, webmail, the provider stamping a flag —
touches that message. On a real mailbox read from several clients that is a
routine event. A UIDVALIDITY roll re-fetches `1:*` and reverts every message's
flags in one pass.

## Decision

### 1. The switch is a new per-account boolean, not a third `sync_scope` value

`app.accounts.writeback_enabled boolean NOT NULL DEFAULT true`, mirroring the
`sync_enabled` column beside it and reusing its whole groove: the `Account`
struct, the metadata-only `Account/set` patch class (`internal/api/account.go, the patch classifier`),
and the settings row.

A third `sync_scope` value is rejected for a reason stronger than "it deviates
from ADR-020": **a `move` has two mailboxes.** A per-mailbox writeback switch
has no defined answer when a message moves from a shadowed folder into a live
one. `sync_scope` is also the *pull* direction and writeback is the *push*
direction; they are orthogonal axes, and conflating them would recreate exactly
the defect ADR-020 closed — an enum value whose name does not describe its
behaviour. **ADR-020 is untouched by this ADR.**

Two booleans on the account give a small, honest matrix, and "ingest-only" is a
named point in it rather than a third thing:

| `sync_enabled` | `writeback_enabled` | meaning |
|---|---|---|
| true | true | normal |
| **true** | **false** | **ingest-only (shadow)** |
| false | true | detached; queued intents drain on re-enable |
| false | false | fully detached |

A **two-value enum is a boolean with extra steps**, and ADR-020's own lesson is
that an advertised value nobody implemented is worse than no value at all. If a
third writeback mode is ever wanted, it arrives with its own ADR.

### 2. An instance-level flag ANDs in, and the API cannot override it

`MAILROOM_WRITEBACK_DISABLED` (the `MAILROOM_SEARCH_DISABLED` shape) disables
writeback for every account on the instance. While it is set, `Account/set`
**refuses** to set `writebackEnabled` true, with `invalidProperties` naming the
property, rather than silently accepting a value that will not take effect.

Two switches need justifying. The per-account column is the operating control
and the useful granularity — one account shadowed while another syncs fully.
The instance flag is the *safety catch for the cutover window*: it lives in the
deployment's compose file and cannot be
removed by a misclick in the settings UI. Given that the thing being guarded is
the user's real mail, a control that survives the UI is worth one extra
concept.

⚠ **The instance flag is only honoured by a process that loads the same
environment.** It is not a property of the database; it is installed on the
`Store` at boot, so a binary that opens the store without installing it does
not honour it. `mailroom` and `mailroom-admin` install it, and those are the
two that matter: the rule is **every binary that can reach `EnqueueIntent`, the
writeback drain, purge or send.** `mailroom-import`, `mailroom-drill`,
`mailroom-export` and `mailroom-crypto` open a store and do not install it, and
are safe only because none of them can reach those paths — import labels
provenance-less messages only, the drill runs on its own synthetic account, and
ingest-time rule arms write directly rather than enqueueing. That is a property
worth re-checking whenever one of them grows a feature.

A CLI that skipped it *and* could enqueue would write `pending` intents on an
account the server considers shadowed, to fire at cutover. That was a real
defect in this change's first implementation, found in review.

⚠ It is honoured only if the invocation carries the same environment. Running
`mailroom-admin` in a one-off container with just a database URL silently opts
out of the instance flag, so admin commands against a shadowed instance must be
run with the deployment's env file.

**Therefore the real account's own column MUST be `false`.** The instance
flag is the second lock, not the first. `writeback_enabled` is settable at
create time from the declared-accounts file, from `add-account --ingest-only`,
and through `Account/set`, so there is no window in which the account exists
un-shadowed.

### 3. The real control is that the writer is never constructed

Enforcement is layered, and this ADR names which layer is the guarantee rather
than listing three and implying they are equal.

- **Structural — THE control.** `Runner.DrainAccount` returns at the top, before
  `r.dial`, for a shadowed account. No IMAP connection is opened and no
  `imapsync.NewWriter` is constructed, and `DrainAccount` is the sole production
  construction site.

  ⚠ **Stated precisely, because the loose version is false.** `SyncSession`
  itself declares `StoreFlags`/`Copy`/`Move`/`Expunge`/`Append`
  (`internal/sync/session.go`), and `Writer` is a thin adapter over it — so
  every sync pass, IDLE watcher and onboarding validation is already holding a
  write-*capable* object. There is no type-level guarantee here. What is true,
  and what was verified by grep rather than assumed, is that **no non-test call
  site of those five methods exists outside `internal/sync/writer.go`**, and the
  only production caller of `writer.go` is the writeback engine. Splitting
  `SyncSession` into read and write interfaces would turn this into a real
  compile-time property; until then the guarantee is "one call path, guarded at
  its head", which is weaker and should not be advertised as more.
- **Ledger — enqueue.** `EnqueueIntent` writes `state='suppressed'` (§4) instead
  of `'pending'`. This is bookkeeping and honesty, not safety.
- **Backstop — the writer itself.** If anything ever does obtain a writer for a
  shadowed account, it is a `refusingWriter` whose every method returns an error
  and logs at WARN. Reaching it is a bug by construction: it means a layer above
  has a hole, so it must be loud rather than quiet.

Note a trap: because suppressed intents are not `pending`, `PendingIntentCount`
returns 0 and `DrainAccount`'s *existing* early return would already skip the
dial. That is incidental, not the control — a later change to that count would
silently remove the guarantee. The explicit guard is the named control and is
tested as such.

### 4. Accumulated intents become a ledger; lifting the mode does not replay it

`app.intent_state` gains a fifth value, `suppressed`, which is **terminal and
never claimed**. Switching the mode on transitions that account's outstanding
`pending` intents to `suppressed` in the same transaction as the flag change.

#521 proposed *dropping* accumulated intents as "the honest default". This ADR
takes the other path, because dropping is the option that reproduces #520's
disease by design: at lift, local state and the server disagree permanently,
with nothing recording it.

Instead, **the suppressed queue is the shadow run's most valuable artifact** — a
countable, per-op-class ledger of every change Mailroom wanted to make to real
mail. That is precisely the read-only report CLAUDE.md's measurement discipline
asks for, and throwing it away throws away the evidence the cutover decision
should rest on.

**Lifting the mode does not replay it.** Replay is a separate, explicit
`mailroom-admin` command that prints counts per op class and requires `--apply`,
so a fortnight of queued moves can never fire as a side effect of flipping a
flag. Report first, `--apply` opt-in.

⚠ **"Nothing leaves `suppressed` without `--apply`" is too strong, and this ADR
claimed it for one review round.** Two enqueue paths use a *deterministic*
idempotency key and are re-driven by a timer rather than by a user: the phase-1
purge EXPUNGE (`purge-expunge:<message>:<account>`) and a superseded draft's
APPEND / header-EXPUNGE. Once writeback is live, such a re-enqueue promotes its
own suppressed row back to `pending` via `EnqueueIntent`'s
`w.state = 'suppressed' AND EXCLUDED.state = 'pending'` arm — no `--apply`, and
the ledger count silently shrinks as it happens. **Measured**, not deduced: a
sweep on a lifted account moves a suppressed purge EXPUNGE to `pending` and
reports `expungeEnqueued = 1`.

For purge this is the *safest* available behaviour rather than a hole, and it is
why the code is left as it is: `ExpungeableAccountIDs` re-decides from where the
copy physically is **at that moment** (§7), so a promotion can only happen when
destroying the copy is genuinely correct. Leaving the row dormant and re-deciding
it physically is better than either replaying it blind or dropping it. So the
accurate statement is: **the ledger is never replayed wholesale by lifting the
mode; individual rows whose enqueue is timer-driven and deterministically keyed
can be re-armed once writeback is live — purge ones only after the physical
predicate passes again.**

**A row can also reach `pending` on an account that is *already* shadowed, and
the drain heals that every pass.** Suppression at switch-on is one transaction,
and two routes get around it. The first was measured with a `pg_stat_activity`
witness: `EnqueueIntent` decides pending-vs-suppressed from a subselect on
`app.accounts` at its own statement snapshot, and only *then* takes `KEY SHARE`
on the account row via its FK — which conflicts with `UpdateAccount`'s
`FOR UPDATE`, so the INSERT blocks mid-statement, is invisible to
`SuppressAccountIntents` in that same transaction (0 rows), and lands `pending`
after the account is already shadowed. It then applied on the first drain after
the mode was lifted, with no `--apply`, invisible to the ledger report. The
second is duller: booting with `MAILROOM_WRITEBACK_DISABLED` set while rows are
already `pending` calls `Account/set` never, so nothing suppresses them.

`Runner.DrainAccount` therefore suppresses any rows still `pending` when it
finds the account shadowed, before returning. It already holds the effective flag
and the count, so this adds no second predicate that could disagree with the
first, and it happens strictly before `r.dial` — a store write, never an IMAP
capability. **It is not a claim that no `pending` row ever exists on a shadowed
account**: between the race and the next pass the row sits there, so the residue
is "the mode was switched on and off again inside one sync interval". Closing
that at source needs a locking read in `EnqueueIntent`'s subselect, which is
tracked rather than done here.

A hazard that exists **today, unguarded, and is NOT fixed here**:
`sync_enabled=false` stops the pass but not the enqueue, so intents accumulate
`pending` and re-enabling the account fires the whole set in one drain. That is
#521's "mass mutation nobody reviewed", already shipped.

⚠ **The drain-side self-heal above does not reach it, and that is worth stating
because the shapes look identical.** The self-heal runs inside `DrainAccount`,
and `DrainAccount` is only ever called for accounts the sync runner lists —
`ListSyncableAccounts` selects `sync_enabled = true` (verified, not assumed). A
`sync_enabled=false` account therefore has no pass at all, so nothing observes
its growing `pending` pile and nothing moves it into a ledger. Ingest-only mode
and a paused account accumulate the same rows and resolve them completely
differently, by design — but only one of the two has a guard.

An earlier draft of this ADR claimed "the same rule applies to it". It does not
— `Account/set` suppresses only on `writebackEnabled: false`. Extending
suppression to `sync_enabled` is a behaviour change to a different feature, and
arguably the wrong one (pausing sync for a week and resuming it probably
*should* apply what you did meanwhile). It is left alone deliberately and
tracked separately; the sentence is corrected rather than the code, because a
document that claims a guard exists is worse than one that admits the gap.

*Migration note:* `ALTER TYPE ... ADD VALUE IF NOT EXISTS` runs safely inside
goose's per-migration transaction on PostgreSQL 12+ **as long as the new value
is not used in the same transaction** — the pattern and its caveat are already
documented in `db/migrations/00009_account_object_type.sql:22`. The
`pending → suppressed` transition is runtime code, not the migration, so the
constraint is satisfied. Every existing query over `state` must be audited for
the new value: `CountOutstandingIntents` (`state IN ('pending','inflight')`) and
`PendingIntentCount` (`state='pending'`) both correctly exclude it, and
`EnqueueIntent`'s `Revive` path must not silently resurrect a suppressed row.

### 5. The mode declares that flags are the server's and filing is yours

Given fact 2, no uniform policy is right for every mutation class, and pretending
otherwise would manufacture exactly the silent divergence this milestone exists
to remove. The mode therefore ships with a **stated contract**, which is what
the code already does rather than a fight against it:

> In ingest-only mode, **flags mirror the server**: marking a message read or
> starred in Mailroom is provisional and will be replaced by the server's value
> the next time the server speaks about that message. **Labels, filing and trash
> are yours** and stay local; the server never learns about them.

Mutations are **accepted, not refused**. Refusing flag writes would mean a mail
client that cannot mark a message read, which destroys the reading experience
the shadow run exists to evaluate. Accepting them costs only that read/unread
re-syncs from the server — the *safe* direction of divergence, and arguably the
correct semantics for an observer.

The part that matters for the milestone survives intact: **labels are durable**,
so the classification-correction loop — the single thing a 37k-message real
corpus is most worth having — works during the shadow run.

### 6. Submission is disabled separately and explicitly

Suppressing writeback stops the Sent copy being *filed* (it is an APPEND intent)
while leaving the *send itself* working, because SMTP is a different egress
behind a different seam (`provider.Sender`, `internal/compose/send.go`). That
combination is worse than either half: a not-yet-trusted instance emits real mail
to third parties and does not even record it in Sent.

So a shadowed account **cannot submit**, gated twice: `EmailSubmission/set`
refuses with `forbidden` *before* `Submit` does anything durable (it signs and
rewrites the message's stored bytes for a PGP-keyed user, so refusing after it
would leave a rewritten message and a cancelled submission behind), and
`SendWorker.Send` refuses at the `provider.Sender` seam. The second is not
duplication: the API cannot speak for a submission created *before* the mode was
switched on and only now being dispatched, and the seam is the one place bytes
reach SMTP.

Drafts still work locally — a draft that never reaches IMAP is a new object, not
a conflicting edit.

⚠ **What this mode does NOT bound: network activity that is neither a mailbox
mutation nor outbound mail.** Image-proxy fetches (triggered when the user opens
a message), WKD/HTTP key lookups and web push are untouched by the switch, and
they will happen during a shadow run. Nothing here is wrong — none of them
mutates the mailbox or emits mail, which is what the mode promises — but
"structurally incapable of touching third parties" is a stronger claim than this
design makes, and nobody should read §3 as making it. If a future deployment
needs egress silence as well as mailbox silence, that is a different control and
belongs in its own decision.

Two limits, stated rather than discovered. **The client does not disable compose**:
the user can write a message and will be refused on send, with the banner as the
only forewarning. That is tolerable because a shadow run is not for sending, and
is tracked as a follow-up rather than claimed here. And a submission **scheduled
while live but dispatched while shadowed** is failed permanently, not held — it
is not sent after the mode is lifted.

**Permanently, but not silently, and this half was only true from v1.73.0 on.**
That failure is recorded with a cause written for a person rather than for the
schema — *"this account is in ingest-only mode: Mailroom is observing the
mailbox and will not send from it"* — the reserved daily quota is given back,
and the draft itself is untouched, so re-sending after the mode is lifted is one
deliberate action rather than a recovery. Until #589 the row was recorded and
shown to nobody: the message sat in Drafts, which reads as *"I never sent it"*
rather than *"it was refused"*, and the last thing the interface had said about
it was that it was on its way. A failed submission now appears in the Outbox
carrying that sentence. Nothing re-queues it, and nothing should — a send the
user scheduled days earlier, dispatched into a mode that exists precisely
because the instance is not trusted yet, is not a send anyone should make on
their behalf.

### 7. Consequences for purge, which #521 did not anticipate

Purge is two-phase: phase 1 enqueues an EXPUNGE for a due message that still has
live provenance (`internal/purge/purge.go, enqueueExpunge`), and phase 2 hard-deletes only
once that provenance is gone — `HardDeleteMessage` **fails closed** while any
live copy survives (`internal/store/purge.go, HardDeleteMessage`).

In shadow mode phase 1 can never land, so phase 2 is refused forever and the
worker re-enqueues the expunge (`Revive: true`) on every sweep. Left alone, a
shadowed account accumulates permanently-stuck soft-deleted messages and a queue
that never drains.

**Decision: phase 1 may EXPUNGE a copy only when that copy is physically in the
account's Trash.** `ExpungeableAccountIDs` returns the accounts where writeback
is effectively on *and every live copy the account holds is in that account's
`\Trash`*; the worker enqueues for exactly those, and defers the deadline when
there are none.

⚠ **This is the third answer to one question, and the first two were both
wrong. The pattern is more instructive than either bug.** Phase 1 asks "may I
destroy this copy?", and rounds one and two answered it with *bookkeeping
proxies*:

1. **A deadline** — defer `purge_after` while shadowed. Missed every message
   trashed *during* the run, because the defer is only reachable for rows
   `DueForPurge` already returned, and that query selects `purge_after <= now`.
2. **A ledger state** — no suppressed `move` intent for the account. Missed the
   multi-account case, because the check lived in the worker while the enqueue
   resolved its own account set in the store with a *different* predicate. It
   also could not distinguish the latest trash move from any historical one,
   treated a dead-lettered move as permission to destroy, and self-cleared at
   `pending` rather than `applied`.

Each was one layer over the same mistake: **two places computing one safety
decision from state that merely stands in for the fact.** The fact that
licenses an EXPUNGE is not in the intent queue at all — it is where the copy
physically is. If live provenance says Trash, expunging empties Trash. If it
says INBOX, expunging destroys inbox mail, and that is equally wrong whether
the move is suppressed, dead-lettered, still queued, or was never made.

The predicate is therefore computed **once**, in the store, and
`EnqueuePurgeExpungeIntents` now takes the account set rather than resolving
its own — the two can no longer disagree.

Three consequences worth stating:

- It **fixes a defect that predates this mode**: a trash move that dead-letters
  used to leave the message due, and phase 1 would then expunge it from
  wherever it still sat.
- `bool_and`, not `bool_or`: re-delivered mail can have several live copies in
  one account (#206) and the engine applies an intent to every one of them, so
  a single copy still in INBOX disqualifies the account.
- An account with no `\Trash` mailbox can never satisfy this. That is correct
  rather than a gap — its trash MOVE already dead-letters for want of a
  destination — and it is no longer silent: it reports as `unlanded` (below),
  which is exactly what it is, a trash move that never reached the server.
- The comparison must be `bool_and(COALESCE(m.special_use, '') = '\Trash')`, not
  `bool_and(m.special_use = '\Trash')`. `special_use` is nullable and an
  ordinary folder carries NULL, so the bare comparison yields NULL for that copy
  and `bool_and` *skips* it like every SQL aggregate — measured on PostgreSQL 18,
  `bool_and` over `{true, NULL}` is `true`. An account with one copy in Trash and
  one in INBOX passed on the strength of the Trash copy alone, and the engine
  destroyed both. The rule above was stated correctly and the SQL did not
  implement it for one commit.

**When no account qualifies, the reason is recorded, and there are three of
them.** A single counter asserted a cause it could not support: during the
shadow run the mode's own count is expected to be large, so a stalled trash move
folded into it would read as the mode working correctly. So
`ExpungeableAccountIDs` returns *why* it found nothing, from the same rows that
make the decision (never a second query, which is how earlier attempts here went
wrong), and the worker tallies each separately:

| cause | meaning | expected? |
|---|---|---|
| `shadowed` | no account holding a live copy has writeback on | yes, in bulk, during a shadow run |
| `unlanded` | a writeback-**enabled** account holds a live copy outside its Trash | **no** — a trash move has stalled; also logged per message at Warn |
| `vanished` | no live copy anywhere; a race against the due scan | rarely; informational |

The deferral is kept, renamed `DeferPurgeDeadline`, as the batching measure it
actually is: `DueForPurge` is `ORDER BY purge_after LIMIT n`, so a backlog of
un-expungeable messages would otherwise fill every batch and starve a live
account belonging to the same user. It is **not** the safety property.

### 7a. Lifting the mode: the supported procedure

The mode is safe to turn on unilaterally. Turning it **off** has an order, and
the ADR states it because the machinery assumes it:

1. Lift the mode (`writebackEnabled: true`, and/or remove the instance flag).
   The ledger is not replayed, and nothing is destroyed: a message is not a purge
   target while its copy is still sitting outside the server's Trash, no matter
   how long ago it was trashed locally or what state its intent is in. What does
   *not* hold is the stronger "nothing at all leaves `suppressed`" — see §4's
   note on timer-driven, deterministically-keyed rows.
2. Run `mailroom-admin writeback-ledger` — the read-only report, per op class.
   It is read-only and checks the mode only on `--apply`, so it can and should
   also be run **before** step 1, to decide with the numbers in hand.
3. Decide, and if replaying, run it with `--apply`. Moves land, so trashed
   messages actually reach the server's Trash.
4. Only then does retention run out and phase-1 purge expunge them, from Trash.

⚠ **Step 3 is not optional in the way it looks.** Skipping it does not mean
"the deletions are abandoned and the mail is expunged from wherever it sits" —
an earlier draft of this ADR said exactly that and it was both wrong and
internally inconsistent (it abandoned the MOVEs while executing the trash
decisions against INBOX). What actually happens is that those messages are
**never expunged**: their copies are not in the server's Trash, so phase 1
never takes them, and they stay soft-deleted locally and present on the server.

That is the safe failure, but it is a standing state rather than a resolution.
An operator who decides not to replay should expect the local Trash to hold
messages that will not empty.

⚠ **Restoring and re-deleting them does not clear it either**, which an earlier
draft of this section claimed. A re-trash enqueues a *new* move
(`ApplyEmailUpdate` mints a fresh action id into every idempotency key, so it
never collides with the shadow-era row), and that new move does land — but only
once it drains, and only then does the copy reach Trash and become purgeable.
So the honest remedy is the same either way: **let the moves reach the server**,
whether by replaying the ledger or by re-issuing them live. Until a copy is
physically in Trash, nothing will destroy it, which is the property this design
is buying.

⚠ **Flag-class rows are stale by construction and should not be replayed
uncritically.** §5's whole contract is that flags mirror the server during the
run, so a week-1 `+flag \Seen` in the ledger may describe a message the phone
marked unread in week 5 — and the local row will have followed the server, so
replaying re-imposes a six-week-old opinion on both.

⚠ **"MOVEs are fine; the engine resolves current provenance" was the wrong
reassurance**, and it was in this section for two drafts. Resolving current
provenance is what makes a replayed MOVE *apply cleanly*; it is not what makes it
*right*. Replay is a **delta** replay, not a state reconcile: a week-1 `move` is
applied to wherever the copy is now, so filing the user did from their phone
during the shadow weeks is silently reversed, and a message they pulled back out
of Trash is re-trashed. The engine treats only missing or stale provenance as a
conflict — a copy that simply moved is not one. Moves are the op class that
decides *where mail lives*, so they deserve more scrutiny than flags, not less.
The report should flag intents whose provenance changed after the intent's
`created_at` (#554 item 5).

Per-op selection on `--apply` is deferred (#554); until it exists, read the
per-op report and decide deliberately rather than replaying the ledger whole.
There is also no `--discard`, so "decide not to replay" is a standing state
rather than a terminal one (#554 item 4).

**Expect noise between steps 1 and 3.** Every shadow-trashed message that comes
due in that window reports `unlanded` and logs the per-message Warn whose text
says it is not supposed to happen — which is true in normal operation and
expected here, because the moves have not been replayed yet. It stops once
step 3 lands them.

### 8. `sync_scope='off'` is a read control, not a write control

#521 asked this to be settled here. It is now answered from the code rather than
inferred: **yes, an `off` mailbox is a fully live writeback target.**
`liveAccountIDs` (`jmap_email.go:1712`) has no join to `app.mailboxes`, and
`MessageProvenanceTargetsForAccount` (`store/writeback.go, MessageProvenanceTargetsForAccount`) joins it only to
read `imap_name`. No test covers it — the six test files mentioning `sync_scope`
are all about the read side.

**Decision: this is intended, and is documented rather than changed.** `off`
means "stop reading this folder" (a read-cost control, `db/migrations/00026`),
not "stop writing to it". The missing test is added so the contract is pinned
rather than merely true.

⚠ An earlier draft justified this by saying that gating writeback on
`sync_scope` would break the reaper's conservative case for a message moved into
an unsynced folder. **That justification is wrong** and is corrected here: the
reaper comment it cited already tolerates a message "whose writeback move has
not landed", and a dead-lettered move leaves the placement override in exactly
the state a suppressed one would. The real reason is narrower and sufficient —
`sync_scope` is a per-mailbox *read-cost* control, writeback is a per-account
*trust* decision, and overloading the first to mean the second is the category
error ADR-020 was written about.

The oddity is recorded honestly: Mailroom will mutate a folder it never observes
again, so that mutation's effect is unverifiable and the local row's flags will
never be corrected. That is acceptable for a read-cost control and is now stated.

## Consequences

- An account can be attached, fully ingested, indexed and browsed with the
  writeback path inert by construction rather than by convention.
- The shadow run produces a reviewable ledger of everything Mailroom would have
  done, which is the evidence the go-live decision should rest on.
- Two switches exist (per-account and per-instance) and can disagree; the API
  refusal in §2 is what stops that being confusing.
- `app.intent_state` grows a value, so every query over `state` is now a place
  the new value could be mishandled. Audited in §4; pinned by tests.
- Read/unread and stars are explicitly *not* durable during a shadow run. This
  is stated to the user rather than discovered by them.
- A shadowed account cannot send mail at all. This is a real capability loss for
  the duration and is the point.

## Verification

Per #521, an integration test that turns the mode on and performs **every
mutation class** — flag, keyword, label add/remove, move, trash, draft save,
draft destroy — then asserts:

- zero IMAP mutations reached the fake server, **and**
- zero intents are left in any state that would drain later.

Strengthened beyond the issue's wording: the fake dial **fails the test if it is
called at all**, which proves no connection was opened rather than merely that
no write was issued.

**Negative control, required in both halves:** with the mode off, the same test
must see every one of those writes reach the fake server. A test that cannot
fail is documentation (#62), and three assertions about a sandbox once passed
against a page that could not have loaded at all (#131).

### The probe, and what it caught

Per CLAUDE.md, the suite passed on first run, so each mechanism was deleted in
turn to confirm a test goes red — and the failing LINE was read, not just the
word FAIL.

| mechanism deleted | result |
|---|---|
| `DrainAccount`'s `if !writeback` guard | `TestIngestOnlyNeverDials` red **at the exploder-dial assertion**; `TestWritebackEnabledDoesDial` still green |
| the `CASE … ELSE 'suppressed'` in `EnqueueIntent` | `TestIngestOnlySuppressesEveryMutationClass` red on state, drainable count and ledger; the writeback-enabled control still green |
| the suppressed→pending `ON CONFLICT` arm | `TestRepeatingAMutationAfterLiftUpgradesTheLedgerRow` red at the swallowed-change assertion; both siblings still green |

**The first probe failed, and that is why this section exists.** On the initial
implementation, deleting the guard left `TestIngestOnlyNeverDials` *passing*.
The transaction above it returned early on `!writeback`, leaving `pending` at
its zero value, so the unrelated `pending == 0` return was enforcing the mode —
the named control was dead code and the guarantee rested on an accident of
variable initialisation. The count is now always taken, so the guard is the
only thing that can stop the drain. Had the suite simply been run and believed,
this would have shipped as a mode enforced by coincidence.

⚠ **This section overstated the purge probe for three review rounds, and the
correction belongs here rather than quietly in a test file.** It claimed that
removing `DeferPurgeDeadline` made `TestLiftingTheModeDoesNotImmediatelyExpunge`
report `expungeEnqueued = 1` "for a message whose only live copy is in INBOX".
That test's fixture puts the copy in **Trash** (`purgeHarness.seedProvenance`),
and the test's own comment now says so, including that an earlier version of its
failure message "claimed INBOX and was describing a scenario its own fixture did
not model". Under the physical predicate (§7) an EXPUNGE for a copy that really
is in Trash is a *correct* EXPUNGE, so what that test actually pins is the
batching deferral — which §7 itself says is **not** the safety property. A
Verification section that overstates what its probe proved is the precise
failure CLAUDE.md's measurement rules exist to prevent, and it survived here
longer than any of the defects it was describing.

**What is genuinely pinned, and by what.** The INBOX case — the destruction
this ADR's §7 exists to prevent — is
`TestTrashedDuringTheRunIsNotExpungedAfterTheLift`, which seeds the INBOX copy
explicitly. The mixed case, one copy in Trash and one outside in the same
account, is `TestACopyOutsideTrashInTheSameAccountBlocksTheExpunge`, with
`TestEveryCopyInTrashStillExpunges` as the control that a fix keying on copy
*count* rather than copy *location* would break. The multi-account case is
`TestMultiAccountUnlandedMoveIsNotExpunged`, asserting per-account on the intent
rows because `ExpungeEnqueued` is a per-message count and structurally cannot
see a wrong-account intent. The
writeback-enabled control stayed green throughout, so the fix is not "purge
stopped working".

Two claims are deliberately NOT made. The probe shows the guard is load-bearing
by failing at the dial; it does not thereby show `RefusingWriter` refuses every
write once reached. That half is `refusing_writer_test.go` plus the
compile-time `var _ IMAPWriter`, and the code comment says so rather than
implying the probe covered it.

Additional coverage this ADR obliges:

- The `suppressed` state is never claimed — including by the 15-minute stranded-
  `inflight` reclaim arm added for #520.
- Lifting the mode does not **replay** the ledger; only the explicit `--apply`
  does. (Not "drains nothing" — see §4 on deterministically-keyed rows.)
- A trashed message in a shadowed account does not accumulate expunge intents
  and is not hard-deleted (§7).
- A message with one live copy in Trash and one outside it, in the **same**
  account, is not expunged — with the every-copy-in-Trash case as the control
  (§7).
- A row left `pending` on an already-shadowed account is moved into the ledger by
  the next drain, and a `pending` row on a **live** account is not (§4).
- `sync_scope='off'` as a writeback target, the test #521 found missing (§8).
- `Account/set` refuses `writebackEnabled: true` while the instance flag is set.
- **The `provider.Sender` seam itself refuses** (§6) — not just
  `EmailSubmission/set`. This one was omitted from this list for three review
  rounds while §6 called it the structural gate, and `NewSendWorker` appeared in
  no test file at all, so the gate the ADR leaned on hardest was the one with no
  test that could fail. Both halves of the switch are now covered, with a
  positive control that asserts the sender *is* called when writeback is on.

## The provider does advertise CONDSTORE — measured, not assumed

`condStore` is a runtime fact (`internal/mailsync/engine.go:377`), so §5's
contract was written against an assumption until it was checked. It has been.

`SelectData.HighestModSeq` is **0 when CONDSTORE was not requested or not
supported** (`internal/sync/types.go:155`), and the engine persists it to
`app.mailboxes.highest_modseq` unconditionally (`engine.go:565`). So a non-zero
stored cursor is conclusive positive evidence; zero would have been ambiguous
between "unsupported", "not requested" and "never synced".

Queried on a test instance running the **same provider** as a real mailbox would, every
mailbox reported a non-zero `highest_modseq` — including an empty Trash, which shows the value is reported
rather than derived from traffic. The flag pass's guard
(`condStore && baseModSeq != 0 && status.HighestModSeq > baseModSeq && lastSeen > 0`,
`engine.go:528`) is therefore live on every mailbox that has any mail.

**Consequence: §5's contract is accurate, not pessimistic.** Flags *will* be
overwritten from the server whenever another client touches a message, so the
mode must state that rather than hope otherwise.

*Limit of the inference:* this is measured on one test account. Another
account on the same provider platform is a different account, and CONDSTORE is a
server capability rather than a per-account setting, so it carries — but it is
one account's evidence, not two.

## References

- Forgejo #521 (this issue), #519 (production readiness), milestone v1.70.0
- #520 — the stranded-`inflight` lease, whose silent-divergence framing §4 follows
- #296 — every copy, not one; the OR-merge that makes flags server-authoritative
- ADR-020 — `sync_scope` is two values (**not** amended by this ADR)
- ADR-009 — deletion semantics, the two-phase purge §7 depends on
- spec §4/§4a (writeback and reconciliation), §12 (account health)
