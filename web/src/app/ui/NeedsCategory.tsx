// SPDX-License-Identifier: AGPL-3.0-only

// The needs-a-category surface's own pieces (#176, epic #156): the batch-triage
// group header, the per-row suggestion strip, and the empty state.
//
// This surface exists because SUPERVISION IS THE BOTTLENECK. The classifier, the
// rule inducer and the deterministic kind layer are all downstream of how much
// mail the user has actually filed by hand, and none of them is measurable at 43
// examples. Everything here is therefore designed around one number: how many
// labelled messages does one decision produce?
//
// A per-message queue answers "one". An alias-grouped queue answers "as many as
// arrived at that address" — and it does it while asking an EASIER question,
// because "where does mail to bank@ go?" is answerable from the address
// alone, whereas "what is this message?" needs the message read first.
//
// Nothing here applies anything without being asked. The group proposes, the
// user answers, and the rule that follows is shown with its blast radius against
// real mail before it is accepted.

import type { JSX } from "react";

import type { QueueGroup } from "../../core/needsCategory";
import type { EffectiveEmail } from "../../core/store";
import type { Id, Label, PriorityReason, SuggestedLabel } from "../../core/jmap/types";
import { t } from "../i18n";

/** A confidence as a percentage. Whole numbers: the model's calibration does not
 * support a decimal point, and showing one would claim a precision it has not
 * earned. */
export function confidencePercent(confidence: number): number {
  return Math.round(confidence * 100);
}

/**
 * What to show in a suggestion's confidence slot, split by producer (#242).
 *
 * `confidencePercent` above is only honest for `source: "model"` — a T1
 * probability. A rule-sourced suggestion carries an EVIDENCE RANK in the same
 * `confidence` field (see SuggestedLabel's doc comment in jmap/types.ts): a
 * count of how many of the user's own filings back the candidate, with no
 * calibration behind it at all. Rendering THAT as "67%" would tell the user the
 * server measured a probability it never computed — exactly the claim
 * `confidencePercent`'s own doc comment says a decimal point would make, one
 * level further up.
 *
 * So the two sources render from different data entirely. `percentText` is
 * non-null only for a model suggestion, and `evidenceText` only for a rule
 * one — never both, and a caller that shows `percentText ?? evidenceText`
 * therefore never has to branch on `source` itself.
 *
 * The evidence text is not recomputed here. The rule inducer already wrote it
 * as the LAST entry of this suggestion's own `reasons` — a sentence like
 * "backed by 4 messages you filed under this label" — and reasons is the one
 * place that number's meaning is actually spelled out for a person; a client
 * deriving its own phrase from the evidence-rank integer alone would have to
 * either invent the same wording (a second copy to disagree with the first)
 * or show a bare count with no unit. The `?? t("queue.confidence.ruleFallback")`
 * fallback below exists only for the malformed case the server's own contract
 * already concedes can happen (reasons null or empty) — it is not the normal
 * path for a rule suggestion, which always carries its evidence phrase.
 */
export function suggestionConfidenceDisplay(s: SuggestedLabel): {
  percentText: string | null;
  evidenceText: string | null;
} {
  if (s.source === "rule") {
    const reasons = s.reasons ?? [];
    const evidence = reasons.length > 0 ? reasons[reasons.length - 1]?.label : undefined;
    return { percentText: null, evidenceText: evidence ?? t("queue.confidence.ruleFallback") };
  }
  return { percentText: t("queue.confidence", { percent: confidencePercent(s.confidence) }), evidenceText: null };
}

/** What a batch label DID, once the server has answered — the payoff moment of
 * this surface, and the reason the group header does not simply disappear. */
export interface QueueGroupOutcome {
  /** The QueueGroup.key this outcome belongs to — its React identity, and what
   * keeps a second answer to the same group from stacking up a second card. */
  key: string;
  /** How many messages the batch labelled. */
  count: number;
  labelName: string;
  /**
   * What came back, kept as five distinguishable states rather than one
   * "rule or no rule" boolean.
   *
   * They call for five different sentences, and collapsing them is how the
   * client came to answer "these have too little in common" to a server that had
   * actually said "you have not given me enough examples yet" — a confident
   * wrong diagnosis in a feature whose entire premise is not doing that.
   */
  verdict: "pending" | "rule" | "tooFewExamples" | "noRule" | "unsettled" | "unavailable";
  /** The rule the labelling implies (Rule/induce), when there is one.
   * `complete` is whether it was measured over the whole mailbox or only the
   * window the server scanned — without it, "matches nothing else you have" is
   * an absolute claim made from a bounded count. */
  rule: { covers: number; collateral: number; complete: boolean } | null;
  /** How many more examples the label needs before a rule can be proposed.
   * Meaningful only for `tooFewExamples`. */
  needed?: number;
  /** Runs the induced rule into the editor, where it can be inspected and saved.
   * Absent when there is no rule to review. */
  onReview?: () => void;
  /** Takes the whole batch back (#182): strips the label from exactly the
   * messages this card filed and returns them to the queue. Absent once the
   * undo has run, or while it is running — an undo offered twice would ask the
   * server to remove a label that is already gone. */
  onUndo?: () => void;
  /** True once this batch has been taken back. The card STAYS, saying so: it
   * has to stop claiming the messages were filed, and a card that vanished
   * would leave the user unsure whether the undo worked. */
  undone?: boolean;
}

/**
 * The batch-triage header: "24 messages arrived at bank@ — where do these
 * go?", and one gesture that answers for all of them.
 *
 * The QUESTION IS THE HEADING, deliberately. A count with a "label all" button
 * is a bulk-select affordance, which people use on a queue they already
 * understand; a question is an invitation to answer one, which is what a
 * first-run backlog needs. The count is the second thing said, not the first.
 */
export function QueueGroupHeader({
  group,
  complete,
  onLabel,
  onDismiss,
}: {
  group: QueueGroup;
  /** Whether the list holds the WHOLE queue. When it does not, every count on
   * this header is a floor rather than a total — the group may well continue
   * past the window — and it says so instead of stating a windowed number as a
   * fact about the mailbox. */
  complete: boolean;
  // The GROUP, not its ids: the answer has to find its way back to the header
  // that asked, and by the time the server replies the group may no longer be
  // on screen at all.
  onLabel: (group: QueueGroup) => void;
  onDismiss: (group: QueueGroup) => void;
}): JSX.Element {
  return (
    <li className="queue-group" role="presentation">
      <p className="queue-group-question">
        {group.kind === "alias"
          ? t(complete ? "queue.group.askAlias" : "queue.group.askAliasPartial", {
              count: group.ids.length,
              alias: group.display,
            })
          : t(complete ? "queue.group.askDomain" : "queue.group.askDomainPartial", {
              count: group.ids.length,
              domain: group.display,
            })}
      </p>
      {/* WHETHER THIS IS ONE SERVICE OR A CROWD — the homogeneity signal #175
          identified as what separates a good binding from a bad one.
          Shown, never used to hide the group: the person decides.

          THREE, not two (#186). Two is the ordinary shape of a service address —
          the company's own domain plus its bulk subdomain — and typically every
          service alias on a real mailbox has exactly
          that. Remarking on it would fire the caveat on the normal case and
          teach the reader to ignore it. The genuinely mixed case, a forwarded-in
          address, no longer reaches here at all: it dissolves into its senders
          before grouping (groupQueue). */}
      {group.senderCount > 2 ? (
        <p className="queue-group-mixed">
          {t("queue.group.senders", { senders: group.senderCount })}
        </p>
      ) : null}
      <span className="queue-group-actions">
        <button type="button" className="button" onClick={() => onLabel(group)}>
          {t("queue.group.label", { count: group.ids.length })}
        </button>
        <button type="button" className="button button-quiet" onClick={() => onDismiss(group)}>
          {t("queue.group.dismiss")}
        </button>
      </span>
    </li>
  );
}

/**
 * What the answered batches DID — the payoff, and the whole reason this surface
 * is worth using rather than labelling twenty-four messages by hand.
 *
 * ABOVE the list, not in the group's old position, because by the time there is
 * an outcome the group is GONE: answering clears every message's open questions,
 * every row leaves the queue, and a header anchored to the first of them would
 * have vanished with it — taking the blast radius, the one number this epic
 * promises to show before anything automatic starts, with it.
 *
 * It persists until the view changes. A toast would not: the user is mid-triage
 * and looking at the next group, and "a rule for these would also label 265
 * other messages" is not a thing to glance at for four seconds.
 */
export function QueueOutcomes({
  outcomes,
}: {
  outcomes: QueueGroupOutcome[];
}): JSX.Element | null {
  if (outcomes.length === 0) return null;
  return (
    <div className="queue-outcomes">
      {outcomes.map((o) => (
        <div className="queue-group queue-group-done" key={o.key}>
          <p className="queue-group-question">
            {o.undone
              ? t("queue.group.undone", { count: o.count, label: o.labelName })
              : t("queue.group.labelled", { count: o.count, label: o.labelName })}
          </p>
          {/* Once taken back, the card says ONLY that. The rule proposal was
              about a labelling that no longer exists, and offering to review it
              would be offering to automate the mistake. */}
          {o.undone ? null : o.verdict === "pending" ? (
            <p className="hint">{t("queue.group.checkingRule")}</p>
          ) : o.verdict === "rule" && o.rule ? (
            <p className="queue-group-rule">
              {/* COLLATERAL IS A BLAST RADIUS, NOT AN ERROR COUNT: it is mail the
                  rule would NEWLY label, and since the user has hand-labelled
                  only a fraction of their mailbox, unlabelled is not a negative
                  example. It is shown to be looked at, never scored.
                  `complete` decides whether it can be stated as a total at all —
                  the server measures over a window and says so, and repeating a
                  windowed count as an absolute is how a bounded number becomes a
                  claim nobody made. */}
              {o.rule.collateral > 0
                ? t(o.rule.complete ? "queue.group.ruleEffect" : "queue.group.ruleEffectPartial", {
                    covers: o.rule.covers,
                    collateral: o.rule.collateral,
                  })
                : t(
                    o.rule.complete
                      ? "queue.group.ruleEffectNone"
                      : "queue.group.ruleEffectNonePartial",
                    { covers: o.rule.covers },
                  )}{" "}
              {o.onReview ? (
                <button type="button" className="link-button" onClick={o.onReview}>
                  {t("queue.group.reviewRule")}
                </button>
              ) : null}
            </p>
          ) : o.verdict === "tooFewExamples" ? (
            // What the server ACTUALLY said. Not "too little in common" — the
            // label simply has not been used enough yet, which is a different
            // fact and one with an obvious next step.
            <p className="hint">{t("queue.group.needMore", { needed: Math.max(o.needed ?? 1, 1) })}</p>
          ) : o.verdict === "unsettled" ? (
            <p className="hint">{t("queue.group.notSaved")}</p>
          ) : o.verdict === "unavailable" ? (
            <p className="hint">{t("queue.group.ruleUnavailable")}</p>
          ) : (
            <p className="hint">{t("queue.group.noRule")}</p>
          )}
          {/* UNDO, on the card that did it (#182). A batch answer clears the
              selection and empties the group, so at the moment the user realises
              they picked the wrong label they have neither the messages nor the
              view — this card is the only thing left that still knows which
              messages and which label. */}
          {o.onUndo && !o.undone ? (
            <span className="queue-group-actions">
              <button type="button" className="button button-quiet" onClick={o.onUndo}>
                {t("action.undo")}
              </button>
            </span>
          ) : null}
        </div>
      ))}
    </div>
  );
}

/**
 * What the system thinks about ONE message, on the row: the labels it
 * considered, how sure it was, and why.
 *
 * The reasons are shown INLINE rather than behind a disclosure, unlike the
 * priority why-panel. The priority panel explains a ranking the user can read
 * past; this explains a question they are being asked to answer, and an answer
 * given without the reasoning in view is the "accept or ignore" dynamic that
 * makes automation untrustworthy. Two reasons, because that is what fits without
 * turning a queue into an essay.
 *
 * `reasons === null` is NOT rendered as "no reasons": the server distinguishes
 * "the model left no record of why" from "the model looked and had nothing to
 * say", and flattening them would put a claim in the server's mouth.
 */
export function SuggestionStrip({
  email,
  labels,
}: {
  email: EffectiveEmail;
  labels: Record<Id, Label>;
}): JSX.Element | null {
  if (email.suggestions.length === 0) return null;
  const [leading] = email.suggestions;
  const reasons: PriorityReason[] = leading?.reasons ?? [];
  return (
    <span className="suggestion-strip">
      {email.suggestions.map((s) => {
        const label = labels[s.labelId];
        // Percentage for a model suggestion, evidence phrase for a rule one —
        // NEVER a percentage for the latter (#242, load-bearing: a rule's
        // confidence is an evidence rank, not a calibrated probability). See
        // suggestionConfidenceDisplay's doc comment for why.
        const { percentText, evidenceText } = suggestionConfidenceDisplay(s);
        return (
          <span className="tag tag-suggested" key={s.labelId}>
            {/* A label the user has since deleted still has an open question
                about it. Naming the id would be noise, so it says what it can. */}
            {label?.name ?? t("queue.unknownLabel")}
            <span className="suggestion-confidence">{percentText ?? evidenceText}</span>
          </span>
        );
      })}
      {reasons.length > 0 ? (
        <span className="suggestion-reasons">
          {reasons
            .slice(0, 2)
            .map((r) => r.label)
            .join(" · ")}
        </span>
      ) : null}
    </span>
  );
}

/**
 * The empty state — a SUCCESS, not a void.
 *
 * "Nothing here" is what every other empty list says, and it is the wrong
 * sentence for this one. An empty inbox means you have read your mail; an empty
 * queue of questions means the system is not stuck on anything and is not
 * waiting on you. Saying so is the difference between a surface people revisit
 * and one they conclude is broken.
 */
export function QueueEmptyState(): JSX.Element {
  return (
    <div className="queue-empty" role="status">
      <p className="queue-empty-headline">{t("queue.empty.headline")}</p>
      <p className="hint">{t("queue.empty.body")}</p>
    </div>
  );
}
