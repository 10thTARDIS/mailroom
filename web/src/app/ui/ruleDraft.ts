// SPDX-License-Identifier: AGPL-3.0-only

// The rule a message suggests (#155), moved out of ListRoute (#375) so both
// list surfaces can hand the same starting point to the rules editor — the
// reading pane's "create a rule from this message" is the same button on the
// desk as it is on the list, and two copies of this function would be two
// different rules from the same message.

import type { EffectiveEmail } from "../../core/store";
import { suggestionsFor } from "../../core/ruleFromMessage";
import type { RuleDraft } from "./RuleEditor";

/**
 * The rule a message suggests, ready for confirmation (#155).
 *
 * It takes the FIRST suggestion — the most precise one available: the mailing
 * list if there is one, else the exact sender. Broader starting points (the
 * whole domain, a subject match) are offered inside the editor, because they are
 * choices a person should make while looking at the blast radius, not defaults
 * chosen for them.
 *
 * The action is deliberately left empty rather than guessed. "Apply which
 * label?" is the one question this flow genuinely cannot answer from the
 * message, and pre-filling a wrong label is worse than an obvious blank.
 *
 * WHICH IS ALSO WHY THE NAME IS NOT FINAL HERE (#292). The suggestion's
 * defaultName is the sender and nothing else — `Venmo` — and rule names are
 * unique per user, so at 85 rules that collided with the rule the operator
 * already had for that sender almost every time they worked the queue. The name
 * a rule ends up with needs the label, and the label is the one thing this
 * function has just refused to guess. So the draft carries the sender half as
 * `nameBase`, flags the name as still ours to choose, and the editor finishes it
 * (`Venmo → Promotions`) the moment the user answers the label question.
 */
export function draftRuleFrom(email: EffectiveEmail): RuleDraft {
  const [best] = suggestionsFor(email);
  const base = best?.defaultName ?? email.subject ?? "";
  return {
    name: base,
    nameBase: base,
    nameAuto: true,
    priority: 100,
    conditions: best ? { all: [best.condition] } : { all: [] },
    actions: [{ type: "addLabel", label: "" }],
    enabled: true,
  };
}
