// SPDX-License-Identifier: AGPL-3.0-only

// What makes two rules THE SAME rule, and what to call a new one (#292).
//
// The bug this file exists for: "create a rule from this message" named the new
// rule after the SENDER ALONE. `nameFor()` in ruleFromMessage.ts returns a
// display name, a domain, a List-Id or a subject and nothing else — so a rule
// built from a Venmo message defaults to `Venmo`, and the server enforces
// `UNIQUE (user_id, name)` (db/schema.sql, app.rules). The second rule anyone
// writes for a sender they already have a rule for therefore fails with "a rule
// with that name already exists". At 85 rules, working the needs-a-category
// queue — which is precisely the activity that keeps putting you in front of
// senders you already have rules for — that is not an edge case, it is the
// common case.
//
// It is also the ONLY name generator in the tree that collides, and for one
// reason: every other one includes the label. cmd/mailroom-admin/accept.go's
// `ruleName` produces `Cybersecurity: fromDomain equals "…" AND …`, and says
// why in as many words — "a label with three rules cannot have them all called
// 'Shop (suggested)', because the name is UNIQUE per user and the second
// create would simply fail". The client's omitted the label and duly failed.
//
// Three different things go wrong at once there, and they want three separate
// answers, in this order of trust:
//
//   1. THE SERVER IS THE ARBITER. `UNIQUE (user_id, name)` is the only thing
//      that actually knows whether a name is free at the moment of the write.
//      So a collision is answered where the write happens — bump a suffix and
//      retry (see `nameTakenBy` / `bumpRuleName`, used by actions.createRule).
//      EVERYTHING ELSE IN THIS FILE IS ADVISORY and can be raced.
//   2. A BETTER DEFAULT NAME makes the collision rare instead of constant
//      (`autoRuleName`): label AND condition key, like every other generator.
//   3. A DUPLICATE CONDITION is the real complaint hiding behind most of these
//      collisions — the user is about to write a second rule for a sender they
//      already handle — and deserves to be answered as itself rather than as a
//      naming inconvenience (`findDuplicateRule`).
//
// DOM-free and framework-agnostic (spec §12), like everything in core/.

import type { Rule, RuleAction, RuleCondition, SetError } from "./jmap/types";
import { trimRuleNamePart } from "./ruleFromMessage";

// ---------------------------------------------------------------------------
// 1. The server's answer: name collisions, detected structurally
// ---------------------------------------------------------------------------

/**
 * How many times `Rule/set` is worth attempting under a bumped name before the
 * failure is simply reported.
 *
 * Bounded, not a loop. Three attempts covers "you already have this name" and
 * "you already have this name and its (2)"; past that, something other than an
 * ordinary collision is going on and grinding through `(4)`, `(5)`, `(6)` would
 * turn one bad write into a burst of them.
 */
export const MAX_NAME_ATTEMPTS = 3;

/**
 * The longest rule name we will construct.
 *
 * NOT a server limit — `app.rules.name` is `text` and `rules.Validate` checks
 * the condition tree and the action list and nothing about the name, so a name
 * of any length would be accepted. It is the convention set by
 * cmd/mailroom-admin/accept.go's `ruleName`, which truncates at 120, and a rule
 * list is unreadable long before that anyway. Matching it means the two
 * generators produce names of the same shape.
 */
export const MAX_RULE_NAME = 120;

/**
 * Whether this SetError is the server saying the NAME IS TAKEN — as opposed to
 * any of the other things it says about a rule.
 *
 * MATCHED ON THE STRUCTURED FIELDS, NEVER ON THE DESCRIPTION. `ruleSetError`
 * (internal/api/rule.go) answers a name collision with
 *
 *     {type: "invalidProperties", properties: ["name"],
 *      description: "a rule with that name already exists"}
 *
 * and that description is prose written for a human. Keying a retry off it
 * would make an English sentence load-bearing: the day someone improves the
 * wording, or a locale reaches it, the retry silently stops firing and #292 is
 * back with no test going red. `type` and `properties` are the wire contract.
 *
 * But the structured fields ALONE are ambiguous, and pretending otherwise would
 * be its own bug: `createRule` in the same file answers an EMPTY name with
 * `{type: "invalidProperties", properties: ["name"], description: "rule name is
 * required"}` — structurally identical. Retrying that would bump "" to " (2)",
 * which the server would happily accept, and the user would get a rule called
 * "(2)". So the disambiguator is something only the caller knows: THE NAME WE
 * SENT. If it was non-empty, "name is required" cannot be the answer we got,
 * and the only remaining reading of properties:["name"] is a collision.
 *
 * @param sentName the name that was actually put on the wire, unmodified.
 */
export function nameTakenBy(err: SetError, sentName: string): boolean {
  if (!sentName.trim()) return false;
  if (err.type !== "invalidProperties") return false;
  const props = err.properties ?? [];
  // Exactly ["name"]. An ErrInvalidRule comes back as ["conditions", "actions"],
  // and a future error naming name ALONGSIDE something else is not a collision
  // either — a collision is a statement about the name and nothing else.
  return props.length === 1 && props[0] === "name";
}

/** Trailing " (2)", " (3)"… as added by bumpRuleName, so bumping is idempotent. */
const BUMP_SUFFIX = /\s\(\d+\)$/;

/**
 * The same name again, disambiguated: `Venmo → Promotions (2)`.
 *
 * Strips an existing bump first, so attempt 3 produces `… (3)` rather than
 * `… (2) (3)` — including when the user's own typed name already ends that way,
 * which is the same shape and means the same thing.
 */
export function bumpRuleName(name: string, attempt: number): string {
  const base = name.replace(BUMP_SUFFIX, "").replace(/\s+$/, "");
  const suffix = ` (${attempt})`;
  if (!base) return suffix.trim();
  const room = MAX_RULE_NAME - suffix.length;
  return (base.length > room ? base.slice(0, room) : base) + suffix;
}

// ---------------------------------------------------------------------------
// 2. The better default name
// ---------------------------------------------------------------------------

/** Cap a finished name. A hard slice, like accept.go's `ruleName`. */
function capRuleName(name: string): string {
  return name.length > MAX_RULE_NAME ? name.slice(0, MAX_RULE_NAME) : name;
}

/**
 * What the rule KEYS ON, in a few legible words: the half of the name that is
 * not the label.
 *
 * `base` — the prefill's own `RuleSuggestion.defaultName` — wins when there is
 * one, and that is a deliberate trade. It is the sender's DISPLAY name ("Venmo")
 * where the condition only has the address ("no-reply@venmo.example"), and the
 * display name is what the user recognises in a rule list. The cost is that it
 * is frozen at prefill: if they then retarget the condition inside the editor,
 * the key describes what they started from rather than what they ended with.
 * That is a cosmetic staleness on a name they can see in the field and edit
 * before saving — where deriving from the condition every time would trade a
 * legible name for a literal one on every rule, including the ones nobody
 * retargets.
 *
 * With no base (the plain "New rule" path) it falls back to the first condition
 * that actually says something, so a rule written from scratch still gets a name
 * rather than being named after its label alone.
 */
export function ruleNameKey(base: string | undefined, conditions: RuleCondition): string {
  const b = (base ?? "").trim();
  if (b) return trimRuleNamePart(b);
  const derived = firstStatedValue(conditions);
  return derived ? trimRuleNamePart(derived) : "";
}

/** The first leaf in the tree that pins something down, depth-first. An `exists`
 * leaf or a blank value is skipped: naming a rule after a test that names no
 * value produces "Mail from" and helps nobody. */
function firstStatedValue(c: RuleCondition): string {
  const kids = (c.all?.length ?? 0) > 0 ? c.all : c.any;
  if (kids && kids.length > 0) {
    for (const kid of kids) {
      const found = firstStatedValue(kid);
      if (found) return found;
    }
    return "";
  }
  const value = (c.value ?? "").trim();
  if (!value) return "";
  const header = (c.header ?? "").trim();
  // A header leaf's value alone ("yes") is meaningless without the header it
  // was read from.
  return header ? `${header}: ${value}` : value;
}

/**
 * The name a rule gets when the user never typed one (#292).
 *
 * BOTH HALVES, because either alone collides. The label alone repeats for every
 * rule feeding that label; the sender alone repeats for every rule about that
 * sender — which is the bug. `Venmo → Promotions` is unique in the way the rule
 * itself is: this mail, filed there.
 *
 * The LABEL IS CHOSEN IN THE EDITOR, not at prefill, so this cannot run in
 * suggestionsFor: at the moment the draft is built there is no label to name.
 * With no label yet it returns the key alone — which is exactly the old
 * `defaultName`, so the prefill still shows something sensible from the first
 * frame and only sharpens when the user answers the question the rule is for.
 *
 * `format` is passed in rather than baked here so the separator stays in the
 * i18n catalog with every other user-visible string (`rules.autoName`); core
 * does not import from app/.
 */
export function autoRuleName(
  parts: { base?: string; label?: string; conditions: RuleCondition },
  format: (key: string, label: string) => string,
): string {
  const key = ruleNameKey(parts.base, parts.conditions);
  const label = (parts.label ?? "").trim();
  if (!label) return key;
  if (!key) return capRuleName(label);
  return capRuleName(format(key, label));
}

/**
 * The name the editor's field should be showing: the generated one while the
 * name is still ours, the user's the instant it is theirs.
 *
 * A FUNCTION, not two lines inside a React effect, because this is the whole of
 * the "never overwrite what the user typed" promise and it deserves to be
 * somewhere it can be tested without a DOM (spec §12). What remains in the
 * component is the one event that flips `nameAuto` off.
 *
 * Note what is NOT here: any comparison between the current name and the last
 * default. That is the tempting implementation of "untouched" and it is subtly
 * wrong — type the default by hand and the field silently reverts to being ours
 * to rewrite, so choosing the name the app suggested becomes the one way to
 * lose it.
 */
export function nameForDraft(
  draft: { name: string; nameAuto?: boolean; nameBase?: string; conditions: RuleCondition },
  label: string,
  format: (key: string, label: string) => string,
): string {
  if (!draft.nameAuto) return draft.name;
  return autoRuleName(
    { ...(draft.nameBase !== undefined ? { base: draft.nameBase } : {}), label, conditions: draft.conditions },
    format,
  );
}

// ---------------------------------------------------------------------------
// 3. The duplicate that the name collision was really about
// ---------------------------------------------------------------------------

/**
 * A condition tree rendered canonically, so two rules that match the same mail
 * compare equal.
 *
 * This MIRRORS `conditionKey` in internal/rules/induce.go — the Go
 * canonicaliser the rule inducer dedups candidates with — because the two are
 * answering the same question about the same objects and disagreeing would be
 * worse than either answer. Same walk, same `all(…)` / `any(…)` framing, same
 * `field|op|value;` leaf, same `strings.ToLower` on the value and nowhere else.
 *
 * TWO DELIBERATE DIVERGENCES, both because the client sees inputs the inducer
 * never does:
 *
 *   - A SINGLE-CHILD GROUP IS UNWRAPPED. This is not a refinement, it is
 *     required for the feature to work at all. The web editor always wraps
 *     (`{all: [leaf]}` — RuleEditor's newLeaf path and App's draftRuleFrom),
 *     while `generalizationLadder` in induce.go emits a BARE LEAF whenever the
 *     hypothesis has one conjunct (`out = append(out, rest[0])`) — so every
 *     accepted one-condition suggestion in the operator's 85 rules is stored
 *     unwrapped. Keying them differently would mean duplicate detection missed
 *     exactly the rules there are most of. `all([X])` and `X` match identical
 *     mail; they are one rule.
 *   - THE HEADER NAME IS PART OF THE LEAF. Go omits it, harmlessly: the inducer
 *     only ever emits from/fromDomain/listId/alias leaves, so it can never meet
 *     one. The editor offers `header` as its escape hatch, and without the name
 *     `X-Spam-Flag equals yes` and `X-Mailer equals yes` would be "the same
 *     rule". For every condition Go can actually produce, this renders the same
 *     bytes Go does.
 *
 * Order is significant and children are NOT sorted, exactly as in Go. Two rules
 * listing the same conjuncts in a different order will not compare equal; that
 * is a missed duplicate (one extra rule) rather than a false one (an offer to
 * edit a rule that is not the one you meant), and this whole surface is
 * advisory — the server's UNIQUE constraint is what actually decides anything.
 */
export function conditionKey(c: RuleCondition): string {
  const out: string[] = [];
  walk(c, out);
  return out.join("");
}

function walk(c: RuleCondition, out: string[]): void {
  const all = c.all ?? [];
  const any = c.any ?? [];
  // `all` first, matching Go's switch: a node carrying both is malformed and
  // this is not the place to decide what it meant.
  const kids = all.length > 0 ? all : any;
  if (kids.length > 0) {
    if (kids.length === 1) {
      walk(kids[0] as RuleCondition, out);
      return;
    }
    out.push(all.length > 0 ? "all(" : "any(");
    for (const kid of kids) {
      walk(kid, out);
      out.push(",");
    }
    out.push(")");
    return;
  }
  const header = (c.header ?? "").trim().toLowerCase();
  out.push(
    `${c.field ?? ""}${header ? `[${header}]` : ""}|${c.op ?? ""}|${(c.value ?? "").toLowerCase()};`,
  );
}

/** An existing rule that files the same mail the draft would. */
export interface DuplicateRule {
  rule: Rule;
  /**
   * Whether it also applies the SAME label. False means "a rule keyed the same
   * way, filing somewhere else" — still a duplicate worth surfacing, but a
   * different sentence. See findDuplicateRule.
   */
  sameLabel: boolean;
}

/** The label an addLabel action targets, or "" — the first one that names
 * anything, matching how RuleForm and the preview pick a rule's target. */
function labelTargetOf(actions: readonly RuleAction[]): string {
  for (const a of actions) {
    if (a.type === "addLabel" && (a.label ?? "").trim()) return (a.label ?? "").trim();
  }
  return "";
}

/**
 * The existing rule this draft would duplicate, if there is one (#292).
 *
 * WHY THIS IS THE FEATURE AND THE NAME COLLISION WAS THE SYMPTOM: at 85 rules,
 * a default name that collides almost always means the sender already has a
 * rule. Answering that with a unique name gets the user a second rule for a
 * sender that was already handled — which is how a rules list becomes something
 * nobody can reason about. So the caller shows this first, with "edit the
 * existing rule" as the PRIMARY action.
 *
 * SAME CONDITION, DIFFERENT LABEL COUNTS AS A DUPLICATE — the crux of the
 * feature, and the argument for it:
 *
 *   - The user's actual question at this moment is "is this sender already
 *     handled?", and "yes, and it files under Receipts" is the answer they
 *     need, not noise to be filtered out. It is also the more surprising of the
 *     two answers, and therefore the one worth interrupting for: a second rule
 *     with the same key and a different label means the same mail now gets both
 *     labels on arrival. #290's label untangle was 43 messages that had
 *     accumulated 2–4 labels each; this is one of the ways that happens.
 *   - The harms are lopsided. Surfacing a rule the user did not want to know
 *     about costs one click on "create another anyway". MISSING one costs a
 *     duplicate rule, double-labelled mail, and — before the retry above — an
 *     opaque "that name is taken" with nothing to do about it.
 *   - But it is NOT the same claim, so the caller must not make the same
 *     sentence out of it: `sameLabel` distinguishes "you already have this
 *     rule" from "you already have a rule keyed this way".
 *
 * Only rules that APPLY A LABEL are compared, mirroring `savedRules` in
 * accept.go. A `stop` or `skipInbox` rule keyed on the same sender does
 * something categorically different, and offering to "edit" it would invite the
 * user to overwrite a routing rule with a labelling one. Note that an
 * UNRESOLVABLE label target still counts: such a rule is inert (RuleEditor's
 * isInert), and "you already have a rule for this sender, but its label was
 * deleted" is a better thing to learn than nothing — fixing it beats adding a
 * second one beside it.
 *
 * The rule being EDITED is skipped: a rule is not a duplicate of itself, and
 * without this every save of an existing rule would accuse it.
 *
 * @param resolveLabel maps an addLabel target (id, keyword atom, or name) to a
 *   label id — the same resolution order the server uses, supplied by the
 *   caller because core does not hold the label map.
 */
export function findDuplicateRule(
  draft: { id?: string; conditions: RuleCondition; actions: readonly RuleAction[] },
  rules: readonly Rule[],
  resolveLabel: (target: string) => string | undefined,
): DuplicateRule | null {
  const key = conditionKey(draft.conditions);
  // A draft with no stated condition matches nothing in particular, and would
  // otherwise "duplicate" every other empty rule. Bail rather than accuse.
  if (!firstStatedValue(draft.conditions)) return null;

  const draftLabel = resolveLabel(labelTargetOf(draft.actions));
  let fallback: DuplicateRule | null = null;

  for (const rule of rules) {
    if (draft.id && rule.id === draft.id) continue;
    const target = labelTargetOf(rule.actions);
    if (!target) continue;
    if (conditionKey(rule.conditions) !== key) continue;
    const sameLabel = draftLabel !== undefined && resolveLabel(target) === draftLabel;
    // The strongest match wins outright: "this is the same rule" is a more
    // useful thing to be told than "something is keyed this way". Otherwise
    // keep the first match in EVALUATION order, which is the one that would
    // actually fire first on the mail in question.
    if (sameLabel) return { rule, sameLabel: true };
    if (!fallback) fallback = { rule, sameLabel: false };
  }
  return fallback;
}
