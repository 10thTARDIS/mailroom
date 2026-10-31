# ADR-021: A message's category tab is derived, and the label→tab mapping is data

**Status:** Accepted (reverses the "explicitly not Gmail's tabs"
position taken earlier in the classification epic, within spec §8 rather than against it)

> **⚠ Amended by [ADR-022](ADR-022-tab-query-stores-the-classifier-verdict-only.md).**
> "A message's tab is DERIVED, never stored" is narrowed to **the mapping half is
> never stored**. Filtering `Email/query` by tab needs a SQL-expressible predicate,
> and re-writing the Go classifier in SQL would be two implementations of one
> decision list — so the classifier's *verdict* is stored while the label→tab
> mapping stays a live join. This decision's actual guarantee is untouched:
> re-pointing a label still moves its mail on the next read, with no backfill.

## Context

Gmail-style category tabs — Primary, Social, Promotions, Updates, Forums — were
rejected in the classification epic as *"a content classifier bootstrapped from a corpus we do not
have, learning nothing from the individual user."* That objection is correct
about Gmail's implementation and does not apply to this one, by the epic's own
reasoning: it also says the global tier *"needs **public knowledge**, which
spec §8 permits"*, and spec §8's isolation rule says global priors are permitted
when trained on public data.

The distinction that unblocked it, measured twice from opposite directions,
is **function versus topic**:

- **Function** is what a message *does* — marketing, receipt, notification, list
  traffic. It is legible from message *form*: `List-Id` (RFC 2919),
  `List-Unsubscribe` (RFC 2369), one-click (RFC 8058), `Auto-Submitted`
  (RFC 3834), `Precedence`, `Feedback-ID`, DKIM `d=`, `Return-Path`. Because the
  RFCs define what those headers mean, they mean the same thing for a sender
  never seen before — function is **sender-independent by construction**.
- **Topic** is what a message is *about* — theatre, gaming, a particular creator.
  Open-ended vocabulary that cannot be enumerated in a way surviving an unseen
  sender. That is what needs a semantic representation, i.e. embeddings, i.e. v2.

**All five of Gmail's tabs are function categories.** So tabs and labels are two
orthogonal axes, each with its own mechanism, and neither blocks the other.

This ADR does not re-argue that. It records the two *structural*
decisions that argument leaves open, because both are the kind a later reader
would otherwise reverse by accident.

## Decision

### 1. A message's tab is DERIVED at read time. It is never stored.

There is no `tab` column on `app.messages` and there must not be one. Resolution
is:

1. the tabs the message's labels map to (a join through `app.message_labels` →
   `app.labels.tab`), tie-broken by a stated precedence order when they disagree;
2. failing that, a classifier over the function signals migration 00027 persists;
3. finally folded through the user's enabled set — mail in a switched-off tab
   appears in Primary, which is what unchecking a category does in Gmail.

The alternative is a stored verdict, and the cost is not storage but
**maintenance of a lie**: the stated requirement is *"I can rearrange
label mapping later if I need to."* Under a stored tab, re-pointing one label
means re-deriving every message that carries it — a backfill, a job, a window
during which the mailbox disagrees with itself, and a bug class where the
backfill silently misses rows. Under a join, the same edit moves that mail on the
next read and there is nothing to leave stale.

The accepted cost: reading a tab is a join rather than a column read, and
filtering a query by tab will be a join too. The set is small on both sides (five
tabs, single-digit labels) and every index leads with `user_id` already.

### 2. The mapping and the enabled set are per-user DATA, never a Go map.

- `app.labels.tab` — which tab a label's mail feeds; NULL means unmapped, which
  falls through to the classifier exactly as an unlabelled message does. A column
  on the label, because a label's tab is a property of that label like its
  colour: a join table would need its own cascade handling and could outlive the
  label it maps.
- `app.category_tabs` — which tabs this user has switched on, and their order.

Both are JMAP-visible, so every mutation appends a `change_log` row in the same
transaction (the load-bearing invariant). `app.labels.tab` gets that for free —
`UpdateLabel` already appends the `Label` row for any field change — and
`CategoryTab` is a new object type with its own `/get`, `/changes` and `/set`.

A compiled mapping would need a release to change, which defeats the requirement
outright, and would be wrong for the second user the moment there is one. The
operator's current mapping is kept outside this repository and applied as **data**, never
seeded by a migration: it hard-codes one person's label names, which the
portability rule forbids in this repo and which would be meaningless to anyone
else.

### 3. Three smaller choices that follow, and would otherwise look arbitrary

- **The tab vocabulary is a closed enum** (`app.category_tab`), not free text. A
  tab is only useful if something can decide it, and the decider is code — so a
  sixth tab is a release either way, and at that point `ALTER TYPE … ADD VALUE`
  is the cheaper half of the change.
- **Primary cannot be disabled** (`CHECK (tab <> 'primary' OR enabled)`). It is
  the residue *and* the destination of the disabled-tab fold, so a disabled
  Primary would be a tab that is switched off and still holding mail. Made
  unrepresentable rather than handled.
- **Tab rows are materialised lazily on first read**, with a `created` change_log
  row for each row actually inserted. There is no Go-side user-provisioning step
  to hang a seed off — login runs through the `app.upsert_login` SECURITY
  DEFINER — and seeding inside the migration would create JMAP-visible rows with
  no change_log row. Assigning persistent state on first read has precedent here:
  `KeywordAtomForLabel` does it for label keyword atoms.

## Consequences

- Rearranging the mapping is an edit, not a deploy, and takes effect immediately
  with no backfill.
- Deleting a label deletes its mapping with it; there is no orphan state to
  reconcile.
- A tab disagreeing with the user's mapping is **data, not failure**. `List-Id`
  is the strongest function signal there is, and a typical mapping routes all
  list traffic to Updates and Promotions — so the classifier will contradict the
  mapping on roughly that population. Neither side is wrong: the mapping says
  where the user wants this mail *today*, the classifier says what the mail
  *is* per RFC 2919. **A Forums recall of 0 against today's mapping is expected**,
  and the acceptance test must report per-tab precision and recall separately
  rather than an aggregate Forums can drag down.
- The precedence order for labels spanning two tabs is *stated*, not derived:
  narrowest function first — Forums, Social, Promotions, Updates, Primary. One
  pair of it, **Promotions before Updates, is arbitrary** and is flagged as such
  in the code. It currently fires on zero messages (every labelled message carries
  exactly one label), so `mailroom-admin tabs` counts how often it
  actually arbitrates rather than leaving the question to argument.
- Nothing here decides an unlabelled message yet. The classifier over the function
  signals is separate work, and it must be built on the **facts**, not on
  `kind.Classify`'s aggregate verdict — measured on a real corpus, that
  aggregate returned `personal` essentially never (a Primary tab built on it would ship *empty*)
  and `unknown` on a large share of plainly functional mail.

## Verification

`internal/tabs/tabs_test.go` pins the precedence order pair by pair **in both
input orders** — a rule that kept whichever label the join returned first would
pass half the table and fail the other half — plus the disabled-tab fold, and that
"no mapped label" is undecided rather than Primary. Probed: replacing the
precedence rule with first-wins, making `Fold` a no-op, returning Primary for the
undecided case, and dropping a tab from the ranking each turn a distinct test red.

`internal/store/category_tabs_integration_test.go` pins that materialisation is
exactly-once (four rows and four change_log rows against a fixture that already
holds one, then a repeat read that moves nothing), that the Go vocabulary and the
`app.category_tab` enum are read out of the live database and compared rather than
restated, and that re-pointing a label moves its mail with no write to any message.
The Primary constraint is proved at both layers, with a **negative control**: the
test attempts the disabled-Primary write as the superuser, so a missing `CHECK`
fails the test rather than passing silently behind the Go guard. Probed by removing
the constraint from `db/schema.sql`, by making the materialising insert
`DO UPDATE`, and by adding a sixth value to the SQL enum alone.

`internal/api/category_tab_integration_test.go` pins the wire behaviour, including
a `/set` batch containing a refused create, a refused destroy, a refused immutable
patch **and a valid update**, asserting on the valid one — the savepoint trap this
project has shipped once, where a failed statement aborts the transaction and the
per-object error handling above it becomes decoration. Probing it by deleting the
Go-side Primary guard turns the per-object refusal into a whole-method
`serverFail`, which is what proves the Go check, not the savepoint, is what makes
the refusal per-object.
