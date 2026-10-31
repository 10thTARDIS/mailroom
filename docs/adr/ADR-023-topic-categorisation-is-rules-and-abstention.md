# ADR-023: Topic categorisation is rules plus abstention; the per-label learner is retained, unpromoted, behind a measured gate

**Status:** Accepted (measured on a real corpus and
deployed in v1.30.0/v1.30.1). **Amended by ADR-032** (#28): these
measurements came from a mailbox with little history, not a long-used
daily mailbox; for KNOWN senders, learned placement now files mail from the
user's own filings. Everything here about unseen senders and the per-label
learner stands.

> **This is not a spec deviation.** Spec §17's 1.0.0 line commits to *"T1 ML
> priority inbox + **label suggestions** + online learning + explainability +
> snapshots/eval harness"* — **suggestions, not auto-application**. Automatic
> filing from a learned per-label model was the v1.6.0 epic's ambition, never a
> spec requirement. This ADR records why that ambition is not being pursued, and
> what delivers the spec's commitment instead.

## Context

The v1.6.0 classification epic assumed a learned per-label classifier would carry the
long tail that rules could not: *"label a couple of messages and have Mailroom
start doing it for you."* The rule half worked immediately — 66 induced rules at
zero collateral, nearly all keyed on the delivery alias. The learned half was
measured three times over five weeks and never worked.

**The first two rounds of numbers were artifacts.** An audit found four defects in
the evaluation the whole epic had been judged on, one of which made sender
memorisation a *fixed point of the training procedure* — so the conclusion
"structural features don't generalise" had never actually been tested. Fixing
them changed every number and retired the "150–200 label floor" outright: the
constraint was never volume.

**The third round, on an audited pipeline, is the real verdict.** With identity
features excluded and senders held out as groups, AUC lands at 0.55–0.72 while
**precision at the apply threshold is 0.20–0.60**, and on four labels the model
*confidently rejects* 77–88% of true positives. A respectable ranking number
coexisting with unusable precision is why AUC was the wrong question.

### The argument that closed it, rather than a fourth round of numbers

A tension was left open by the learner work and the epic: the tab probe measured that the
hard residue has **no function-level discriminator** — what separates the classes
is *which stream a known brand used* (`email.venmo.com` marketing vs `venmo.com`
notifications) — while the learner's own reshape says to **exclude identity features
entirely**. A learner denied identity, asked to beat the deterministic layer on
an identity-shaped residual, looked set up to fail, and its failure would then
read as a fourth verdict on "structure doesn't generalise".

Settled by argument:

1. **The objection is a category error.** Identity features cannot help on an
   **unseen** sender at all — the feature value is novel and the model has no
   weight for it. They are informative only on senders in the training set, and
   there an induced rule already does the job visibly, editably, at zero
   collateral. So the exclusion costs nothing on the population that matters,
   and the learner is not handicapped by it. `labelModelFeatures` already said
   this in its own comment.
2. **The two residues are different objects.** The tab probe's is the FUNCTION axis (which
   tab); the learner works the TOPIC axis (which label). Importing one as
   evidence about the other muddles them. Dropping the tab probe as an argument
   here changes nothing, because the learner work holds its own better-aimed measurement.
3. **The honest middle case was already measured and points the wrong way.**
   Sender *morphology* — computed from identity but not of it — does generalise.
   The tab probe measured the generic version: `bulk_stream_subdomain` fires on a larger share of
   Updates than of Promotions, i.e. **anti-correlated**, because
   `s.shop.example`, `mail.vendor.example` and `email.vendor-alt.example` wear the same
   costume while carrying notifications. Only "for THIS brand, this stream means
   marketing" works, and that is per-sender memorisation, i.e. a rule.

## Decision

**Topic categorisation is rules plus abstention. The learned per-label model is
retained, remains suggest-only, and may only ever serve by clearing a gate that
measures the quantity the feature actually risks.**

Four parts:

1. **The promotion gate is the lower end of a 95% Wilson interval on pooled
   out-of-fold precision at the apply threshold**, over a label with at least two
   positive-bearing senders per fold. It replaces `labelMinAUC = 0.65` over a
   single grouped split — the wrong quantity *and* the wrong estimator. Wilson
   rather than the normal interval because the normal one has zero width at 5/5
   and would promote on five suggestions; the bottom of the interval rather than
   the point estimate because 9/10 is precision 0.900 and evidence for very
   little.
2. **The diagnostic and the decision are one calculation.** `train-labels
   --folds N` prints the verdict `--apply` would reach, because it *is* that
   verdict — the `Rule/preview` precedent, where a preview that can disagree
   with the engine is worse than none.
3. **The spec's "label suggestions" commitment is delivered by the rule
   inducer**, not the model. Measured on a real corpus: **most
   unlabelled messages would receive a rule-derived suggestion, none
   ambiguous between two labels**, with the rest reaching nothing because their senders
   have never been filed — which is abstention working, not a gap to tune.
4. **`internal/kind` weights and thresholds stay untouched**, and the AUC floor
   is deleted rather than moved: the learner work's "do not move the floor; it was never the
   binding constraint" stands, and this is not a retuning of it.

### What the gate says on the real corpus

**No evaluable label cleared it.** Some were refused on *quality* (the interval's
bottom too low); the rest on *evidence* (too few
positive-bearing senders against a floor of 10). The checks are ordered
evidence-first because "cannot judge" and "judged and not good enough" are
different answers, and collapsing them is how a label that merely needs more mail
reads as a label the model is bad at.

**One label is the case that justifies the change**: its mean AUC was the
highest in the mailbox and comfortably over the retired 0.65 floor — yet
precision at the threshold that decides what a user sees was poor. The old gate
would have promoted it.

## Consequences

- **The epic's goal is met by a different mechanism than planned**, and that is
  delivery rather than abandonment: rules do topic categorisation at zero
  collateral, and the tail that rules cannot reach needs a semantic
  representation (embeddings, #8/#22, 2.x), not a better optimiser over the
  present feature space.
- **A gate that refuses everything today is still worth having.** What it
  replaces said *yes* for a reason that was noise, and it was reachable by one
  flag (`train-labels --apply`). Nothing promoted before because nobody ran the
  command, not because the bar held.
- **The epic's acceptance test passes for an unseen sender by abstention**, which the
  criterion explicitly permits ("or be surfaced for manual classification"), and
  the gate is what keeps it passing. It is now executable
  (`internal/ml/acceptance_test.go`) rather than prose that drifted out of two
  milestone audits.
- **The function axis answers the harder half**: `tabs.Classify` puts the
  transactional lexicon above the ESP rules precisely so a retailer's receipt and
  its campaign — which leave on the same infrastructure — get different tabs,
  sender-independently. ⚠ It does **not** assign the labels `Shopping` and
  `Promotions`; those are topic, per-user, and no public fact implies them.
- **Naive Bayes, the hard-negative pool, and per-label thresholds from CV are
  closed as tuning** inside a feature space that has been measured on an audited
  harness. Reopening any of them needs a new *signal*, not a new optimiser.
- **The classifier-feedback work is moot**: it was blocked on a promoted model
  that this decision says should not exist, and the tab work already routes corrections
  to the label→tab mapping and `Rule/tighten`.
- **This decision is reversible on evidence.** The gate is the mechanism: give it
  a corpus where a label's interval bottom clears the bar over ten-plus senders
  and it promotes, with no code change and no argument.

## Verification

`internal/ml/promotion_gate_test.go` pins the gate, probed with four mutations
producing four distinct reds: reading the point estimate instead of the interval
(9/10 promotes), removing the sender floor, folding "nothing reached the
threshold" into precision, and swapping Wilson for the normal approximation.

`internal/ml/train_integration_test.go` pins the **wiring** from both directions,
because the unit tests prove the gate decides correctly and nothing about whether
`trainOneLabel` consults it — and a gate that only ever refuses is
indistinguishable from `return false`. Separable-by-subject evidence over 20
senders promotes (60/60, interval bottom 0.940); evidence separable only by
sender is refused with the gate's own reason. Forcing the check off turns the
first red; forcing it never to pass turns the second red.

`internal/ml/acceptance_test.go` states the epic's acceptance test as three cases,
both probed into failure: asserting the *rule* that fired rather than only the
tab (the residue rule also answers Updates, so a tab-only assertion could not
fail), and an adversarial single-sender fixture (with identity restored the
receipt scores 0.881 against a 0.80 threshold; an earlier fixture scored 0.799
and the sweep assertion could not fail).

`cmd/mailroom-admin/suggestreport_test.go` carries a negative control on the
match funnel and pins the collateral tautology that made the report's first real
run vacuous — a filter of `collateral <= 0` forces "reached" to zero on any
corpus, because collateral counts exactly the non-positive messages a candidate
reaches.

## References

- **ADR-021 / ADR-022** — the function-vs-topic split and the tab axis
- **ADR-032** — learned placement for known senders
