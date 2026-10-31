# ADR-032: Learned placement: known senders are filed by the user's own filings, at ingest, behind a correction gate

**Status:** Accepted (increment 1; amends ADR-023; next increments: #28)

## Context

The requirement was for Mailroom "to learn where mail should go, and sort it
itself based on machine learning that is continually trained and
reinforced", without writing rules. The choice was for it to **act immediately**,
with no shadow period. That choice is what makes the safeguards below
mandatory rather than optional.

### Why this does not contradict ADR-023, and what it amends

ADR-023 measured content-based learning with senders held out as groups, so a
model could not succeed by memorising them. Precision at the apply threshold
was 0.20 to 0.60, and the learner was never allowed to act. The same work
found that what separates the classes is **identity and stream** (the sender
address or domain, `List-Id`, the delivery alias). It set that aside because
the goal then was generalising to *unseen* senders, and for a known sender a
rule already does the job.

Two things change that verdict for known senders:

1. **ADR-023's measurements came from a test mailbox with little history**.
   A long-used daily mailbox is a different
   population, and ADR-023's numbers do not describe it.
2. **The goal is different.** Most mail comes from repeat senders, so a
   memory of "this sender's mail goes in this area", learned from the user's
   own moves, is the right model. It is per-sender memorisation, which
   ADR-023 §"The argument that closed it" (3) already identified as the thing
   that works. This ADR lets the memory act directly, instead of requiring the
   user to accept a rule.

ADR-023 still governs everything else. Content generalisation for unseen
senders stays on the 2.x semantic route (#8/#22), and the per-label learner
stays suggest-only behind its promotion gate. `internal/ml/train.go` still
trains label models with `dryRun = true`. Learned placement does not use those
models, and this ADR does not change their status.

### What a long-used mailbox looks like (read-only, counts only)

Measured read-only on a long-used daily mailbox, and reported here as findings rather
than figures:

- Nearly all mail comes from repeat senders; one-off senders are a small share of mail.
- Folder-filing history gave nothing to learn from: mail sat in INBOX and Archive.
- Area labels were overwhelmingly applied by the desk (the classifier); only a small
  fraction were the user's own filings, and every sender with a user filing was
  **consistent** (never filed into two areas).

**Leave-one-out on the user filings.** Each filing's area was predicted from the
*other* filings on the same key, acting only when there were >= 2 others and all
agreed. Keyed on the From address (or domain, or address plus delivery alias) the
predictions were right nearly every time and **never wrong**, abstaining only where
fewer than two other filings existed. Keyed on the delivery alias alone, nearly all
cases abstained as mixed, and `List-Id` almost never applied.

⚠ **The agreement is optimistic.** Most filings came from bulk sessions, where a
sender's batch was filed at once, so the filings agree with each other by
construction. The leave-one-out justifies the key and the threshold. It is not the
safeguard. The live correction rate is.

**Reach.** Applied to existing unfiled mail, the rule would move a modest number of
messages between areas, with some no-ops. Moving them is a backfill, and it is out of
scope here.

## Decision

### 1. Evidence: the user's own filings, and nothing else

The evidence is a row of `app.message_labels` for an area's **own** label with
`applied_by = 'user'`. Re-file, the label menu, the sender card and drag and
drop all write that row. Rows from `desk`, `setup`, `rule`, `ml`, `agent`, or
the new `learned` are never evidence. A model trained on its own output
confirms its own mistakes. Mail in Trash is excluded.

### 2. Key: the From address, with the domain as a narrow fallback

The key is the lowercased From address. The domain is a fallback, and it
applies **only when the address has no evidence at all**. So the domain can
never override an address the user has filed, or break a tie for it. It also
requires that every filing for the domain agrees, and that those filings span
**at least 2 distinct addresses** (`MinDomainAddresses`). A domain whose
filings all come from one address is that address's evidence under another
name. That second condition is stricter than the brief, and it only narrows
the fallback.

**The domain fallback never applies where a domain is not a sender**
(`placement.DomainFallbackBlocked`):

- **Consumer and freemail providers**, from a fixed, explicit list in
  `internal/placement` (Gmail, Outlook/Hotmail/Live/MSN, Yahoo, iCloud, AOL,
  Proton, GMX/web.de, mail.com, Zoho, Yandex, mail.ru, Fastmail, HEY, Tuta,
  QQ/163). Two friends at gmail.com filed into Updates say nothing about a
  third person writing from gmail.com, and the gate would only notice after
  that person's mail had been filed away.
- **The user's own domains**, meaning the domains of their enabled
  identities. This uses the same SQL (`ownDomainsSQL`) the selfSpoof warning
  reads. The user's own domain is often their biggest sender, and
  its mail is split across areas.

Address-level learning on those domains is unchanged. The report counts a
blocked fallback as its own reason.

### 3. Decision: confident, or abstain

A key is confident for area X when it has **>= 2 user filings**
(`MinFilings`), all into X, and **no correction newer than its newest
filing**. Two is the smallest number that is a pattern rather than one act,
and it matches the measurement above. Every other case abstains, with one of
these reasons:

- `thin`: too few filings, or a domain with too few addresses
- `mixed`: filings into more than one area
- `contradicted`: a correction is outstanding

When it abstains, the desk's tab placement stands. The decision is pure, in
`internal/placement`. The ingest path and the report call the same function
on the same evidence query, so the report cannot describe a decision that
ingest would not make (ADR-023 §2).

### 4. Act at ingest

Learned placement runs on the live path only. It runs after rules and before
the desk's own area label (`internal/ingest/ingest.go`). When the key is
confident, the area's own label is added with `applied_by = 'learned'`. It
goes through the desk's write path, `store.insertAreaOwnLabel`, rather than a
second one. Once that label is on, the message resolves by label, so the desk
adds no label of its own. That is how a rule's label already beats the
desk's.

It does nothing in six cases:

- A label already decides the message's area. At ingest that can only be a
  rule's label.
- **The From is ambiguous, or the sender is not authenticated** (see
  section 7b).
- **The sender warnings would flag the message.**
- The tab verdict already puts the message in the learned area. Nothing is
  logged in that case, so the gate is not padded with placements that learning
  could not have got wrong.
- The user has switched it off.
- The area's gate has stopped it.

Everything, including the reads, runs in one savepoint. A failure is logged
and swallowed, as for the neighbouring calls, so learned placement can never
abort an ingest.

**change_log:** the label rides the ingest's Email `created` row, as the
desk's label does. The placement log is not a JMAP object. **No writeback:**
the label is written before the message has a live account, and an area's own
label has no keyword atom (ADR-031).

### 5. Online learning, no training job

The model is the evidence query plus the contradiction lookup, evaluated
fresh at every decision. Every filing changes the next decision, which is the
"continually trained" half of the request. There is no materialised table: the
user's own area filings are a small table slice. The partial index
`message_labels_user_filings (user_id, label_id) WHERE applied_by = 'user'`
reaches them without walking the far more numerous desk rows, and the query narrows
them to one domain before they leave Postgres. A cache would be a second copy
of those rows, which every Re-file and undo would have to keep in step.

### 6. Reinforcement: corrections

When the user takes a `learned` label off a message, by Re-file's swap or by
hand, the placement is recorded as **corrected** (`ApplyEmailUpdate`). This
uses the same `RemovalIsUndo` opt-out a rule's correction has.

After a Re-file, the new user filing is itself evidence for the new area, so
the key is mixed and abstains. The contradiction rule covers the other case:
the label is removed without any re-filing, which leaves the old evidence
consistent. The key stays blocked until the user files on it **strictly
after** the correction. The filing written in the same transaction as the
correction does not count.

**No decay in increment 1.** Evidence is the *current* state of the user's
labels, not a history, so it already changes when the user re-files old mail.
A mixed key comes back once the stragglers are re-filed, and those filings
are newer than the correction. A time decay would also forget agreeing
evidence the user never revisited, which is most of it. Whether that is worth
doing is a measurement for increment 2.

**An undo of a correction still counts as a correction.** The undone label
comes back as a user filing. This errs in the safe direction: it can only stop
learned placement sooner.

### 7. The placement log, `app.learned_placements` (migration 00060)

There is one row per placement: user, message, key (kind and value), area,
`decided_at`, and outcome. The outcome is one of:

- **open**
- **kept**, after **7 days** (`KeptAfter`) without a correction
  (`kept_by = 'age'`), or at once when the user **archives** the message from
  the area it was placed in (`'archive'`)
- **corrected**, with `corrected_at`

Reading a message does not keep it: opening a message to see what it is, and
then re-filing it, is the correction path itself. Seven days covers a reader
who only looks at a quiet area at the weekend, and still resolves outcomes in
the first week. A placement already kept is final, so a later move does not
reopen it. Two CHECK constraints make a half-resolved row unrepresentable.

The table has FORCE ROW LEVEL SECURITY with the deny-by-default policy. Its
indexes lead with `user_id`. It is in the RLS isolation suite. The export
drops it, because it is a measurement of this instance; the labels and the
user filings travel.

### 7b. Authentication: never lend "learned from your moves" to a forgery

The key is the From address, and From is forgeable. A spoofed message
claiming to be a sender the user files into Primary, such as their bank, would
otherwise land there labelled as learned. Four rules stop that.

**One identity, computed once.** `placement.Sender` is built at ingest from
`proj.FromAddress`: the first mailbox of `parsed.Header.From()`, which is the
same value stored as `messages.from_address`. It is checked against the raw
From headers. Its fields are unexported. Every downstream use reads it:

- the placement keys and the domain fallback;
- the domain DMARC must pass for, because `NewSender` itself passes that
  domain to the authentication function, not the caller;
- the sender-warning check;
- the placement log's `key_value`.

The store no longer re-reads `from_address`. So the identity that was
authenticated cannot be a different identity from the one acted on.

**An ambiguous From is never acted on** (reason `ambiguous`), in both
modes. A From is ambiguous when there are:

- two or more From headers;
- two or more mailboxes in the From header;
- a group (`name: list;`);
- or a parsed mailbox that differs from the stored address. This one is
  equal by construction today, and is kept as a tripwire.

Each of these is a way for the receiving MTA's "From domain", which its DMARC
verdict is about, to differ from Mailroom's.

**Authentication** (`placement.SenderAuth.Authenticated()`):

- **Trusted authserv-ids configured:** the message needs a DMARC pass
  **bound to the sender's domain** (`ml.AuthResults.DMARCPassFor`).
  - The pass must come from the **topmost** Authentication-Results header
    whose authserv-id is trusted (`ml.TopmostTrustedAuthResults`). The
    provider prepends its header above anything the sender wrote. If a sender
    forges a header with the provider's id, it sits below, and it is ignored
    even if the provider does not strip it (RFC 8601 §5 asks it to, but that
    cannot be verified here). A provider that split its verdicts across
    several headers would lose the lower ones. That fails closed.
  - The pass clause must carry exactly one `header.from`, equal to the
    sender's domain after case-folding, root-dot removal and IDNA
    normalisation (`ml.NormalizeAuthDomain`). A pass with no `header.from`,
    or with two, does not count. **Amended** (see "Amendment:
    relaxed DMARC alignment" below): it may also be exactly the sender
    domain's organisational domain.
  - Any DMARC `fail` clause vetoes.
  - DMARC rather than the warnings' `AffirmativelyPassed` (DKIM or DMARC),
    because a DKIM pass alone may be an ESP signing as itself.
- **None configured:** nothing can be believed as a pass, so the floor is
  "did not clearly fail" (`auth_fail` false, `ml.AuthResultsFail`), plus the
  ambiguous-From refusal.
  - ⚠ **The remaining weakness:** this floor cannot bind anything to a
    domain. A forged From for a known sender with no Authentication-Results
    at all, or with one the provider did not fail, is placed. The fix is to
    configure `MAILROOM_AUTHSERV_IDS`, which production does.
  - The server logs a startup WARN when it runs without it.
- **Always:** if `senderwarn` would flag the message, it is not placed. It
  uses the same index as the reading pane. Its `AuthPassed` input is
  `AffirmativelyPassed` read from the same topmost trusted header.

**The parser** (`internal/ml/authresinfo.go`) is the one tokenizer every
caller uses: the display property, the sender warnings, the cold-start
feature and learned placement. It works in three steps:

1. Strip RFC 5322 comments, nested ones included, honouring quoted strings.
2. Split the resinfo clauses on `;` outside quotes.
3. Attach each `ptype.property=value` to the clause it was written in.

A `method=result` pair is recognised only as a token of its own whose name has
no `.`. So neither a comment nor another clause's property value can produce a
result.

That closed an older hole in the regex it replaced. The regex read
`dkim=pass` out of `smtp.mailfrom=dkim=pass@attacker.example`, whose local
part the sender controls, inside the provider's own trusted header, and that
suppressed the selfSpoof warning. The authserv-id match is whole-string and
case-insensitive. Only a numeric version may follow the id; any other token
makes the header nobody's.

**Open question (not changed here):** whether the warnings'
`AffirmativelyPassed` should also bind DKIM's `header.d` to the From domain,
and read only the topmost trusted header. A DKIM pass for an unrelated domain
on mail claiming the user's own domain suppresses selfSpoof today. Binding it
would warn on own-domain mail sent through an ESP that signs as itself. That
is a behaviour change, so it is left for its own decision.

A message held back gets the desk's normal placement. Evidence is unaffected:
the user's own filings are their decisions, whatever the mail claimed to be.
The report's backfill simulation cannot read raw headers. It can judge only the
stored `auth_fail` and the warnings that need no verdict, so it says that its
would-move counts are an **upper bound**.

### 8. The safeguard: a correction gate per area

For each area, precision is `kept / (kept + corrected)` over resolved
placements, gated on the **lower bound of the 95% Wilson interval**. It uses
`stats.WilsonLowerBound`, which is moved out of `internal/ml` so the label
promotion gate and this gate are one calculation. An area with
**>= 20 resolved** placements (`GateMinResolved`) and a bound **below 0.80**
(`GateMinLCB`) **stops acting**. This is surfaced in a WARN log line on every
message it holds back, and in the report.

| resolved | corrections tolerated | example |
|---|---|---|
| 20 | 0 | 20/20 → 0.839 acts; 19/20 → 0.764 stops |
| 40 | 3 | 37/40 → 0.801 acts; 36/40 stops |
| large | about 15% | |

The gate is stateless. It is recomputed from the log at every decision, over
placements decided after the area's last **reset**, so it resumes in either
of two ways:

- **On newer data:** placements made before it tripped later resolve as kept
  and lift the bound.
- **By an operator:** `mailroom-admin placement-report --reset-gate AREA_ID`
  writes `app.learned_placement_gate_resets`, and the count starts afresh from
  that moment. No outcome is rewritten.

### 9. The off switch

`desk_preferences.learned_placement_enabled`, default **true**, is exposed as
`DeskPreferences.learnedPlacementEnabled`. The store reads it at ingest, so it
is enforced on the server, not only in the client. The web client shows a
Settings row under Areas whose accessible name is its visible text
(WCAG 2.5.3).

### 10. Visible provenance

`Email.areaProvenance` (opt-in, like `area`) is the `applied_by` of the
resolved area's own label. Until now an area chip could not say who filed a
message: an area's own label is local-only (ADR-031), and `labelProvenance`
is keyed by keyword atom, so every chip read "automatically". The reading
pane and the sender card now say "learned from your moves" for `learned`.

### 11. The report

`mailroom-admin placement-report --user-id` is read-only. It prints:

- confident keys per area;
- placements by outcome;
- precision and Wilson lower bound per area;
- gate status per area;
- key verdicts by reason;
- a **backfill simulation**: counts per (now in → learned) area pair for desk
  mail, plus no-ops, held mail, off-desk mail, and abstentions by reason.

It has no `--apply`; `placement-backfill` is the exact report and its
apply (addendum).

## Out of scope

- **Unseen senders and content understanding.** These stay on ADR-023's 2.x
  route.
- **Folders.** Areas stay local-only labels (ADR-031), and learned placement
  never moves mail on the server.
- **The backfill.** Applying learned placement to existing mail is a later
  step that needs explicit approval. The report's simulation is its report.
  (Built in increment 2a: see the addendum below.)
- **The noise signal.** A few senders that are ignored at least
  half the time can make up a large share of Primary. This is a candidate second
  signal, to be coordinated with the priority model.
- **Decay**, and `List-Id` as a key: very few filed messages carry one.

## Consequences

- New mail from a sender the user has filed twice into one area lands there,
  with no rule written and no prompt.
- One wrong move by the user reverses the memory for that sender, and the
  area's gate turns the whole feature off for that area if corrections pile
  up. The gate's first judgement comes at 20 resolved placements, so an area
  can make up to 19 placements before it is judged at all. The contradiction
  rule is what limits harm per sender in that window.
- Senders without a DMARC policy are never placed once trusted authserv-ids
  are configured: their result is `dmarc=none`, not `pass`. That costs reach,
  and the report shows how much. It is the price of not lending a learned
  label to a forgeable From.
- A freemail provider missing from the fixed list falls back to the correction
  gate. The list is short and explicit by design, not a heuristic.

## Addendum (increment 2a): the backfill of existing mail

**Status:** Accepted (increment 2a). The simulation's per-sender list was
reviewed and applying it approved.

`mailroom-admin placement-backfill --user-id U` applies learned placement to
mail already on the desk. It is report-only by default; `--apply` writes,
`--exclude ADDRESS-or-DOMAIN` (repeatable) skips keys, and `--undo BATCH_ID`
reverses one batch.

### A1. One planner, three callers

`store.PlanPlacementBackfill` is the backfill's report, the plan its
`--apply` performs (the command hands the very plan it printed to
`store.ApplyPlacementBackfill`), and `placement-report`'s simulation. Every
decision is `placement.Decide` over the evidence query ingest reads. The
scanned population is partitioned (held, off the desk, already there, each
abstention reason, moves), and the command refuses to apply a plan whose
partition does not add up.

The backfill is **desk-only**. Mail off the desk (archived, other folders) is
counted and never moved.

### A2. The same safety as arrival, exactly

`placement-report`'s simulation reads no headers, so it is an upper bound.
The backfill reads every would-move message's raw bytes from the blob store
(`ingest.BackfillSenders`, through `blob.BlobStore.Get`) and judges them with
the arrival-time functions, which were factored so both paths call one copy:

- `ingest.LearnedSender` builds the `placement.Sender` (ingest's
  `learnedSender` now calls it). `from` is the stored From address, so
  `NewSender`'s tripwire (a parsed mailbox that differs from the stored one)
  is live here, where it could not fire at ingest.
- The unconfigured floor is `ml.AuthResultsFail` on the headers, as ingest
  computes it, not the stored `auth_fail`, so a value stored by an older
  parser cannot vouch for a message today's parser refuses.
- `store.senderRefusal` (ambiguous From, `Authenticated()`, the sender
  warnings) is shared with `learnedPlacement`, as is `decideLearned`.
- The area's correction gate is asked as at ingest, and a message learned
  placement has placed before is never placed again: if its label is gone,
  the user took it off.

A message whose bytes cannot be read is skipped as `unreadable`. The report
states that its count is exact.

### A3. What a move is

A move is Re-file's swap, from the area-filing plan Rule/apply uses
(`PlanAreaFiling`). The other areas' own labels applied by `desk` or
`setup` come off, and the learned area's own label goes on with
`applied_by = 'learned'`. User, rule, learned and other routing labels are
never touched: such mail is held before the planner decides, and a contested
step is skipped. Each move is one `ApplyEmailUpdate` in its own transaction,
so there is one Email change_log row and no writeback. The apply refuses
outright if an area label ever carried a keyword atom.

At apply time each move re-asks, inside its own transaction, every question
the plan answered: the off switch, the key's decision, the resolved area and
the exact labels to remove. A move whose state changed since the plan is
skipped as `changed`. The switch is checked by the store, not only by the
command.

### A4. The record and the undo, without a migration

Each move writes an `app.learned_placements` row (open), so the user's
corrections of a backfilled message are recorded exactly as for an arrival
one, and the report shows its outcome. It is **not** counted by the gate
(A5). It also writes an `app.audit_log` row:

- `object_type 'PlacementBackfill'`, `object_id` the batch id;
- `action 'placement.backfill.move'`;
- `detail`: the message, the area, the label added, and every label removed
  with its `applied_by` and `created_at`. Ids only: no address, subject or
  content.

That row is both the undo journal and the mark that tells a backfill
placement from an arrival one. `audit_log` already fits: it is user-scoped,
FORCE RLS, append-only by grant, withheld from export, and the backfill is an
administrative action on the user's mail, which is what it records. The
undo reads a batch through the `object_id` column and decodes the detail in
Go. The one place the detail is read in SQL is `backfilledMessagesSQL`, the
single definition of "a backfilled placement". It projects
`detail->>'message'` from the move rows as a set and is never a filter on the
detail's contents. Two alternatives were rejected:

- a column on `learned_placements`, which needs a migration;
- the arrival/backfill split from `decided_at` versus the message's ingest
  time, which is an unenforced invariant.

`--undo BATCH_ID` reverses the batch one message per transaction. It does
four things:

- removes the learned label, with `RemovalIsUndo`, so the undo records no
  correction;
- restores the removed labels with their original attribution and
  timestamp;
- deletes the placement-log row, because an undone placement was never a
  placement for the gate;
- appends a `placement.backfill.undo` row.

A message is **skipped as `refiled`** when any of these holds:

- its learned label is gone;
- another routing label from anyone but the desk or setup is on it;
- a label the undo would restore is already back;
- its placement was corrected.

The user's later decision stands. Running the undo again counts the messages
it has already undone and changes nothing.

### A5. Consequences

- **The correction gate judges ARRIVAL placements only.** Old mail is rarely
  revisited, so most backfilled placements would resolve as kept by age
  without anyone looking. Counting them would lift each area's Wilson bound
  and mute the corrections of new mail, a metric that does not match the
  harm. As an example, 19 kept and 1 corrected arrival placements give a
  bound of 0.764, and the area stops. Adding 30 kept backfills would make
  that 49/50, about 0.89, and the area would act again.

## Amendment: relaxed DMARC alignment for subdomain senders

**Status:** Accepted (decision, 2026-10-23: relax it as
described).

### Finding

A `placement-explain` run showed that most
senders refused as `unauthenticated` wrote from a subdomain, such as
`news.example.com`. Each message carried the provider's trusted header with
`dkim=pass`, `spf=pass` and `dmarc=pass header.from=example.com`. The
provider writes the DMARC policy (organisational) domain into `header.from`.
§7b required `header.from` to equal the From domain, so every subdomain
sender failed closed.

### Decision

A trusted DMARC pass counts for From domain D when, after
`ml.NormalizeAuthDomain`, its single `header.from` H satisfies either of
these (`ml.DMARCHeaderFromMatch`):

1. **exact:** H == D (unchanged); or
2. **org-domain:** all of the following hold:
   - D is a strict subdomain of H on a label boundary;
   - H == `ml.OrgDomain(D)`;
   - H is its own organisational domain, so it is never a public suffix;
   - both names are valid IDNA lookup hostnames (A-labels).

`ml.OrgDomain` is `publicsuffix.EffectiveTLDPlusOne`, covering both the ICANN
and private sections. It is the one Public Suffix List helper, and
`ingest.DKIMAligned` (ADR-036) now reads the list through it too. Any list
error refuses, and so does input that is not already in normal form.

This is narrower than RFC 7489 §3.1 relaxed alignment, which accepts any
H with OrgDomain(H) == OrgDomain(D). The following are all refused:

- a sibling (`shop.example.com` for `news.example.com`);
- an intermediate parent (`news.example.com` for `a.news.example.com`);
- any parent above the organisational domain, including one that is not
  itself a suffix (`kawasaki.jp` above the wildcard suffix `*.kawasaki.jp`);
- the reverse case, a subdomain's pass covering its parent;
- a look-alike (`evilexample.com`);
- a public suffix (`co.uk`, `github.io`, a TLD).

**Unchanged:**

- the trusted authserv-id and its date bound (ADR-035);
- the topmost-trusted rule;
- exactly one `header.from` per pass clause;
- the veto by any `dmarc=fail`;
- the ambiguous-From refusal;
- the sender warnings, including the self-spoof check;
- the correction gate.

### Why this lends no new trust

The ambiguous-From refusal guarantees that the provider evaluated DMARC for
exactly one From domain, and that domain is D. DMARC's own relaxed
alignment, which is the default (`adkim=r`, `aspf=r`), already treats every
name under the organisational domain as one owner. So a pass whose
`header.from` names that organisational domain describes the same
evaluation. A party that controls any subdomain of the organisation could
already pass DMARC for D at the provider, whichever name the provider
writes. What stays refused is a `header.from` naming anything other than D
or exactly D's organisational domain.

**List vintage:** the list is embedded in `golang.org/x/net` and changes
only with a dependency bump. A suffix added upstream but missing here could
make OrgDomain(D) one label too short. Exploiting that would also need the
trusted provider's own DMARC evaluation to have treated the names as one
organisation, which is the same evaluation that produced the pass.

### Every caller, and its mode

`DMARCPassFor` and `DMARCMatchFor` take an explicit `ml.DMARCAlignment`.
Its zero value matches nothing, so no caller changes behaviour by accident.

| Caller | Mode | Why |
|---|---|---|
| `ingest.LearnedSender` (arrival, `placement-backfill` via `BackfillSenders`, `placement-explain` via `ExplainLearnedRaw`) | **Relaxed** | The decision in that amendment. One function serves all three paths, so the backfill and the explanation judge exactly as arrival does. |
| `mailroom-admin placement-explain`, the per-clause `header.from` line (`DMARCHeaderFromMatch`) | **Relaxed** | It restates the decision above, so it prints "exact match", "org-domain match" or "no". |
| `mailroom-admin from-field-report`, the DMARC column | **Relaxed** | Its contract is to measure the verdict placement acts on. |
| `ingest.autocryptAuthenticated` (Autocrypt key eligibility, ADR-038) | **Strict** | Encryption-key trust is a separate decision. Relaxing it was not part of that amendment, so it keeps its behaviour. |
| `ical.Verified` (calendar "Verified") | **Strict** | The badge asserts the exact organiser address to the user. Relaxing it needs its own decision. |

**Not callers:** the sender warnings read `AffirmativelyPassed`. The
authentication notice reads the notice severity over `TrustedAuthResults`.
Neither binds a pass to a domain, so neither changed. Local DKIM alignment
(`DKIMAligned`) keeps ADR-036's own rule, where `d=` may be any non-suffix
parent. Only its suffix test moved into `ml.OrgDomain`.

### Measurement and roll-out

- Before and after deploy, read `mailroom_learned_placement_total{reason}`.
  `unauthenticated` should fall and `placed` rise.
- `placement-backfill` uses `LearnedSender`, so it may now find existing
  subdomain-sender mail to move. Read its report first. `--apply` waits for
  explicit approval.

### Tests

Every test below was negative-controlled: the mechanism was removed, the
test went red, and the mechanism was restored.

- `internal/ml/dmarcalign_test.go` covers:
  - the PSL facts it relies on, pinned;
  - `OrgDomain`;
  - the rule in both modes: exact, org-domain, deeper subdomain,
    `example.co.uk`, the private-section `github.io`, the wildcard suffix,
    public suffixes, sibling, intermediate parent, a parent above the org
    domain, reverse, look-alike, case and trailing dots, IDN in both
    directions, an invalid hostname, and the zero mode;
  - the clause rules in relaxed mode, on the production header shape.
- `TestDMARCPassFor` runs its whole table in both modes.
- The per-caller mode is guarded by these tests:
  - `TestLearnedSenderUsesRelaxedDMARCAlignment`;
  - `TestVerifiedStaysStrictForASubdomainOrganiser`;
  - the Autocrypt integration case 4b;
  - `TestFromReportDMARCColumnIsRelaxedLikePlacement`;
  - `TestPrintPlacementExplainOrgDomainMatch`.
- The real ingest path is covered by
  `TestLearnedPlacementRelaxedDMARCForASubdomainSender`. Each refusal
  asserts the arrival's reason, not only that nothing was placed.
  `TestPlacementExplainReportsAnOrgDomainMatch` covers the explanation.
  - `placementAreaCounts` therefore anti-joins the placement log against
    `backfilledMessagesSQL`: one `DISTINCT` derived table, `LEFT JOIN … IS
    NULL`, set-based.
  - The gate at ingest, the backfill planner's gate check and the report all
    read it.
  - `placement-report` prints backfill outcomes per area as their own
    "(backfill)" row: open, kept, corrected, "not gated".
  - ⚠ **Index:** `app.audit_log` has no index that leads with `user_id`, so
    that derived table scans the audit log under the RLS predicate. The scan
    happens where the gate is evaluated (a confident, authenticated arrival
    that would move) and in the report. If it shows, the index is
    `(user_id, object_type, action)`, which needs a migration. It is not
    added here.
- **A correction of a backfilled message still contradicts its key**
  (`placementCorrections` reads every origin). A key the user corrects stops
  acting for new mail, whatever placed the corrected message.

## References

- **#28**: the next increments of learned placement.
- **ADR-023**: amended here, for placement of known senders.
- **ADR-031**: area labels are local-only, which is why the provenance needed
  its own property.
- Never train on your own output.
- Re-file's swap semantics: A rule's area label wins at ingest.
- `internal/placement`, `internal/store/learned_placement.go`,
  `internal/store/placement_report.go`, `db/migrations/00060_learned_placement.sql`.
- Increment 2a: `internal/store/placement_backfill.go`,
  `internal/ingest/learned_sender.go`, `cmd/mailroom-admin/placementbackfill.go`.
