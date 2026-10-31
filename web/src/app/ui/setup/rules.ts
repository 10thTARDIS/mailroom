// SPDX-License-Identifier: AGPL-3.0-only

// STEP 3'S ARITHMETIC AND ITS WORDS (#446): whether a proposed rule is good
// enough to accept in bulk, how a proposal is described, and what makes two
// acceptances of the same proposal one rule instead of two.
//
// All pure, all tested, and none of it in the component — because each of the
// three is a place where being casually wrong writes a rule that then files
// somebody's mail forever.

import type { InducedRule, RuleCondition, RuleField, RuleOp } from "../../../core/jmap/types";
import { conditionKey } from "../../../core/ruleIdentity";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";
import { countText } from "./sentences";

/** What "Accept all with a preview above 90%" means, as a number. */
export const ACCEPT_ALL_THRESHOLD = 0.9;

/** The two numbers a proposal's quality is judged on: how much of the folder's
 * mail the rule would have placed, out of how much was measured. `covers` is on
 * the candidate and `examples` is on the RESULT that carried it, which is why
 * this is a pair rather than a candidate. */
export interface Coverage {
  covers: number;
  examples: number;
}

/**
 * Whether a proposal clears the bar — `covers / examples >= threshold`.
 *
 * ⚠ FALSE WITH NO DENOMINATOR, and that is the whole reason this is a function
 * rather than an inline division. `examples === 0` makes the ratio NaN, every
 * comparison against it false, and `covers / 0` infinite — so a folder the server
 * found nothing to measure in would either be silently skipped or bulk-accepted,
 * depending on which way the expression happened to be written. An unmeasured
 * ratio is not a high one (#176): "Accept all above 90%" must never accept a rule
 * whose 90% nobody computed.
 */
export function acceptable(c: Coverage, threshold: number): boolean {
  if (c.examples <= 0) return false;
  return c.covers / c.examples >= threshold;
}

/**
 * THE IDENTITY OF AN ACCEPTED PROPOSAL: its conditions plus the label it would
 * apply.
 *
 * Two acceptances of the same proposal must be one rule, and "the same proposal"
 * cannot be answered by object identity — step 3 re-asks `Rule/induce` on every
 * visit, so coming Back to it and pressing Accept again arrives with a DIFFERENT
 * object that means the same thing. Nor by array index: the candidate order is
 * "biggest evidence first" and moves as the mailbox does.
 *
 * ⚠ THE CANONICALISATION IS `core/ruleIdentity.conditionKey`, NOT ONE OF THIS
 * FILE'S OWN. That function already answers "do these two trees match the same
 * mail", it mirrors the Go inducer's own `conditionKey`, and it knows the one
 * thing a fresh implementation would get wrong: THE INDUCER EMITS A BARE LEAF for
 * a one-conjunct hypothesis while the editor always wraps (`all([X])`), and those
 * are one rule. A second canonicaliser here would key them differently — and
 * #446's proposals are exactly the shape that comes back unwrapped, so the bug
 * would land on the most common candidate there is.
 */
export function candidateKey(conditions: RuleCondition, labelName: string): string {
  return `${labelName.trim().toLowerCase()} ${conditionKey(conditions)}`;
}

/** Whether this proposal has already been accepted in this pass of the wizard. */
export function isAccepted(
  accepted: readonly string[],
  conditions: RuleCondition,
  labelName: string,
): boolean {
  return accepted.includes(candidateKey(conditions, labelName));
}

/**
 * A condition tree in words, ONE PHRASE PER CONJUNCT.
 *
 * ⚠ THIS IS THE CLIENT TWIN OF `rules.DescribeCondition`, AND IT IS A TWIN
 * RATHER THAN A REUSE BECAUSE THERE WAS NOTHING TO REUSE. The Go function
 * (internal/rules/suggest.go) renders English prose server-side for stored
 * suggestion reasons; the rules EDITOR renders a condition as editable selects
 * (ui/RuleEditor.tsx ConditionNode), not as a sentence. So the client had no
 * describer at all, and step 3 needs one: a row that showed a proposal's
 * conditions as three dropdowns would be asking the user to review a form rather
 * than to read a claim.
 *
 * It is built out of the SAME message keys the editor's own selects are labelled
 * from (`rules.field.*`, `rules.op.*`), which is what keeps the two surfaces
 * calling a condition the same thing — and what keeps this i18n-clean, where
 * porting the Go function's hard-coded English would not have been.
 *
 * ONE STRING PER CONJUNCT, like the Go original: an AND of three tests is three
 * reasons, and a reader checks each against the folder in front of them. An `any`
 * group collapses to ONE phrase ("x or y"), because a disjunction is one reason;
 * a nested `all` inside an `any` is parenthesised so "(a and b) or c" cannot be
 * misread. A node that is neither a group nor a leaf yields NO phrase — such a
 * node matches nothing, so no proposal can have come from one.
 */
export function describeConditions(c: RuleCondition): string[] {
  if (c.all && c.all.length > 0) return c.all.flatMap(describeConditions);
  if (c.any && c.any.length > 0) {
    const parts: string[] = [];
    for (const sub of c.any) {
      const phrases = describeConditions(sub);
      if (phrases.length === 0) continue;
      parts.push(
        phrases.length === 1
          ? (phrases[0] as string)
          : `(${phrases.join(` ${t("setup.rules.and")} `)})`,
      );
    }
    if (parts.length === 0) return [];
    return [parts.join(` ${t("setup.rules.or")} `)];
  }
  const leaf = describeLeaf(c);
  return leaf === null ? [] : [leaf];
}

function describeLeaf(c: RuleCondition): string | null {
  const field = c.field;
  if (!field) return null;
  const subject = fieldLabel(field, c.header);
  // `exists` takes no value, and a value printed beside it would be one the
  // engine does not read.
  if (c.op === "exists") return t("setup.rules.has", { field: subject });
  return t("setup.rules.leaf", {
    field: subject,
    op: opLabel(c.op ?? "contains"),
    value: c.value ?? "",
  });
}

/** The editor's own word for a field. A `header` condition names the header
 * itself, because "Header… is x" says nothing a reader can check. */
function fieldLabel(field: RuleField, header: string | undefined): string {
  if (field === "header" && header) return header;
  return t(`rules.field.${field}` as MessageKey);
}

function opLabel(op: RuleOp): string {
  return t(`rules.op.${op}` as MessageKey);
}

/**
 * The numbers under a proposal: what it would have done to the mail that is
 * already there.
 *
 * `covers` out of `examples` is the whole claim, and the SHORTFALL is stated
 * separately ("10 left for sorting") rather than left for the reader to subtract
 * — that shortfall is what the folder's remaining mail will need sorting by hand,
 * which is the fact that decides whether one rule is enough.
 */
export function coverageText(result: { examples: number }, candidate: InducedRule): string {
  return t("setup.rules.placed", {
    covers: countText(candidate.covers),
    examples: countText(result.examples),
  });
}
