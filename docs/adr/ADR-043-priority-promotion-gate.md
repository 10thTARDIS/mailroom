# ADR-043: The priority model is promoted only through a gate against the cold start, and learns from honest signals

**Status:** Accepted (Forgejo #842; all three steps approved). Amends spec §8's learning loop (mailroom#33 PR2): promotion is no
longer "the candidate's holdout AUC is at least the incumbent's".

## Context

A report came in that none of a user's mail was marked
important. Measured read-only on a real instance:

- Only a handful of inbound messages in 30 days scored at or above
  `ImportantThreshold` (0.6), all by the cold-start heuristic (`priority:0`).
  Every trained version scored far lower; nothing reached 0.4.
- Feedback was dominated by `mark_read_no_open` and `archive_unread`, with a
  few `flag`, `trash`, `mark_important` and `mark_not_important`. Zero `reply`
  events, although sent messages existed.

The audit of the training and evaluation pipeline (the measurement discipline in CLAUDE.md, earned
in #169 and #220) found these defects. Each is a property of the code, not an
inference from the numbers.

1. **Reading was a negative.** `api/feedback.go` recorded every mark-read as
   `mark_read_no_open` (label 0). The web client marks a message read when it
   opens it, so every message the user read was taught as "did not matter",
   including every message they then replied to. That message appeared in
   training as a negative and a positive at once.
2. **Replies from other clients were invisible.** The only reply seam was
   EmailSubmission/set. A reply written anywhere else and ingested from Sent
   recorded nothing.
3. **No class weighting.** Plain SGD over thousands of negatives and a
   handful of positives. The bias walked to the majority, and every score sank.
4. **One sample per event, not per message.** That is how the contradiction
   in (1) reached the fit.
5. **Leaked features.** Training replays features from today's row:
   `flagged:1` is set on exactly the messages the user flagged (a positive
   label), and `corr:`/`sent:` count the reply that made a message positive.
   Neither was true when the message arrived and was scored.
6. **The promotion rule could not fail on this data.** The rule compared AUC
   on the newest 20% of events: the candidate had to be at least the
   incumbent and at least 0.5. AUC on a holdout without positives is 0.5 by
   convention (`eval.go`). With a handful of positives among thousands of events the newest
   slice usually held none, so both sides scored 0.5, and 0.5 >= 0.5 promoted.
   That is why v1 replaced the cold start, and why each daily version replaced
   the one before. `priority-report` reconstructs each version's training
   split and prints `holdout lacked a class` where this happened.
7. **A time split, not a sender split.** Train and holdout share senders,
   which is how sender memorisation passes for skill (#169, #220).

## Decision

### 1. The gate

A trained priority model is activated only if every check below passes, in
this order (evidence first, then quality). Otherwise the cold start serves.
If a trained model is active, it is **reverted** to the cold start
(`Registry.RevertToColdStart`: deactivated, never deleted). The model in
service came from the same procedure on older evidence, so when the procedure
fails today, it has no better claim than the candidate.

| check | constant | value |
|---|---|---|
| enough positives | `MinPositives` | 100 |
| enough sender domains among positives | `priorityGateFolds × priorityGateSendersPerFold` | 5 × 2 = 10 |
| at least one fold held both classes | | |
| the candidate puts something at or above the line | `PriorityGateThreshold` | 0.6 (= `api.ImportantThreshold`, pinned by a test) |
| recall better than the cold start | `priorityPairedFloor`, z = 1.96 | Wilson lower bound on wins/(wins+losses) > 0.5 |
| precision not much worse | `PriorityPrecisionMargin` | candidate precision ≥ cold-start precision − 0.05 |

**What is measured, and where.** Every labelled inbound message from the last
90 days gets one example. It is positive if any engagement signal touched it
(`open`, `reply`, `forward`, `star`, `flag`, `mark_important`), and negative if
it carries only dismissals (`archive_unread`, `trash`, `delete`, `spam`,
`mark_not_important`). Features are rebuilt **as of arrival**:
`flagged:1` is removed where the user flagged the message, and the
correspondence count is reduced by replies sent after the message arrived.
Sender identity (`from:`, `dom:`, `alias:`) is not a feature (see 3).

The judgement uses 5-fold cross-validation **grouped by sender domain**
(`labelCVFolds`, the label models' fold assignment). Each fold model is fitted
on four folds and scores the fifth. The cold start scores the same examples,
and it fits nothing, so every example is out-of-sample for it. Both sides are
compared at the threshold on the value the `real` column will hold
(`store.StoredScore`).

**Why recall, and why paired.** The harm in #842 is engaged mail not shown as
important. That is recall at the line the user looks at, not a ranking
number. Both models score the same messages. The comparison therefore counts
only the engaged messages they **disagree** on: wins are messages the
candidate catches and the cold start misses, losses the reverse. The
candidate must win a share whose 95% Wilson lower bound exceeds one half
(McNemar's reasoning). Comparing two independent recall intervals would
discard the pairing and need far more evidence for the same conclusion. In
practice the candidate needs at least 4 uncontested wins (4/4 gives a lower
bound of 0.51; 3/3 gives 0.44). The tests pin this boundary.

**Why `MinPositives` = 100.** The 95% Wilson interval on a proportion from n
trials is at most ±1.96·√(0.25/n) wide. At n = 100 that is ±0.098: the first
round number at which recall is known to within ten points anywhere in [0,1].
Below it, "better than the cold start" cannot be told from noise on any
realistic gap. v1 trained on 13 (±0.27).

**Why the precision margin.** It is a policy number. The gate exists for
recall, and a recall gain may cost a little precision. It may not cost a lot:
without the check, "mark everything important" would pass on recall alone.
The 0.05 margin is small next to the precision differences the gate is
expected to see. If a corpus ever makes the exact value decisive, argue it
from that corpus.

**Logged.** Every pass writes one `app.audit_log` row (`object_type`
`ml_model.priority`, action `priority.gate.promote|refuse|revert`) with every
number above, plus the existing slog line. No migration is needed.

### 2. Training

There is one balanced fit (`fitPriority`). The minority class is interleaved
against the majority, as `TrainLabeler` does, for 8 passes. The shuffle is
seeded from the user id, so a pass is reproducible from its evidence. The
data floor (`MinTrainSamples` = 50, both classes) still applies before
anything is fitted.

### 3. Sender identity is not a feature of the trained model

The grouped cross-validation cannot vouch for a weight on `from:` or `dom:`:
a held-out sender's keys are unseen. `alias:` crosses domains and would leak
(see `labeler.go`). So the trained model is fitted and scored with
`labelModelFeatures` (identity stripped). The relationship itself is still
carried by `corr:`/`sent:`, which do generalise.

### 4. Signals

- **`open` (label 1)** comes from the client. Triage/open is called once per
  message, after the message has stayed in the reading pane for
  `OPEN_DWELL_MS` = 4,000 ms with the page visible. A quick flick past a
  message never counts. The server records it once per message.
  **Session-only:** an API key has no reading pane. When #841's scopes land,
  `Triage/open` belongs in `agent_scopes.go`'s notForKeys set. #841 had not
  merged when this landed, so this is the note for that merge.
- **`reply` (label 1)** is derived at ingest from any client, but only from
  mail the user **verifiably sent**. A From header naming the user proves
  nothing: anyone can send "From: <the user>" with In-Reply-To naming their own
  earlier message, which would make the attacker's mail a positive and teach
  the model to rank it important (security review of this change). A message
  counts only if its From is one of the user's identities (`ResolveSendIdentity`)
  **and** it was filed into a Sent-role mailbox by the user's server (a
  provenance row, or arrival into one at ingest), is placed locally in one
  (Mailroom-composed sends, imported Sent folders), or was sent by a Mailroom
  submission. A live inbound copy that passes DMARC for the user's domain is
  deliberately not accepted: the Sent copy carries the same signal, so that
  route would add risk and no evidence. Messages that claim the user's address
  without that evidence are counted in `priority-report` and record nothing.
  For a verified message, the original is the In-Reply-To parent, or else the
  nearest References ancestor, that is stored and is **not the user's own
  mail**. The reverse direction is also covered: an original that arrives
  after the reply is found through its thread. A reply is recorded **once per
  original**, under a transaction-scoped advisory lock (no migration). The
  same send can reach the store twice, as the submitted draft and as a Sent
  copy, and a per-pair key would count it twice. The event is dated at the
  reply. `priority-reply-backfill` applies the same rule to history: report
  first, `--apply` opt-in.
- **`mark_read_no_open` is retired.** It is no longer emitted, and it is no
  longer a label, so historical rows are skipped. The decision does not depend
  on the counts. The server cannot tell an open from a bulk mark-read. The
  token was emitted on every message the user then replied to, so it
  contradicts the positive label by construction. A bulk dismissal of unread
  mail is already `archive_unread`. `priority-report` prints how many
  `mark_read_no_open` messages are engaged positives, so the production run
  shows the size of the contradiction.

### 5. Re-scoring

A revert by itself changes nothing the user sees: stored scores stay where
the collapsed model put them. `priority-rescore --since DAYS` re-scores
messages that already carry a score with the model in service. It is a dry
run by default and reports how many messages change band and how many become
important. `--apply` writes exactly the rows the dry run counted, using the
same SQL comparison, with one Email change row each. A second `--apply`
writes nothing.

`priority-train` runs the daily pass on demand: report only by default, and
`--apply` promotes, keeps the cold start, or reverts. A release can then
apply the gate before it re-scores, instead of re-scoring with a model the
next scheduled pass is about to revert. The release order is:
`priority-report`, `priority-reply-backfill` (then `--apply`),
`priority-train` (then `--apply`), `priority-rescore` (then `--apply`), and
`priority-report` again.

The dry run's comparison (`TriageScoreWouldChange`) casts the new score to
`real`, the column's type, so a score that float32 cannot represent exactly
still compares equal to itself. `RescoreTriage` makes the same comparison
explicit. Its parameter was already inferred as `real` from the `VALUES`
position: a probe that reverted that cast stayed green, so this is not a
fix to the existing write path.

## Consequences

- On production at the time of #842 the gate refuses. Positives are 16
  events plus the unrecorded replies the report counts, so the cold start
  serves and v9 is reverted on the first pass. That is the intended answer.
  The open signal is expected to carry the corpus past `MinPositives`; the
  report shows when.
- The trained model can no longer memorise senders. If a future corpus shows
  that per-sender weights would help, measure it with a design that can
  credit it, then decide.
- `Triage/set reset` still activates a feature-blind baseline. That baseline
  scores 0.5, below the line, so a reset user sees nothing important. The
  next daily pass now reverts it to the cold start. Changing reset itself is
  left as a follow-up.
- A domain-wildcard identity treats every address at that domain as the
  user's own, for replies exactly as for contacts. The Sent-provenance
  requirement bounds the effect to mail actually filed as sent.
- A reply recorded at ingest stays recorded. If the user later moves it out
  of Sent, the reaper drops its Sent provenance, so a later backfill no
  longer finds it. A reply whose only copy was never in Sent and was not sent
  through Mailroom never counts. Both losses are accepted as the price of not
  trusting a From header.

## References

- #842 (this), #841 (API-key scopes: `Triage/open` must be notForKeys), #169
  and #220 (the label gate this mirrors), #155 (the preview must be the
  engine), #222 (reproducible scoring), #299 (re-score change rows).
- `internal/ml/priority_gate.go`, `priority_train.go`, `priority_report.go`,
  `priority_rescore.go`, `train.go`; `internal/store/priority_signals.go`;
  `internal/api/triage_open.go`, `feedback.go`;
  `web/src/app/ui/useOpenDwell.ts`; `cmd/mailroom-admin/priority.go`.
