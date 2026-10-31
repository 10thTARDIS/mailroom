// SPDX-License-Identifier: AGPL-3.0-only

// Tests for rule identity and rule naming (#292).
//
// The bug being fixed is a FALSE NEGATIVE dressed as an error message: the
// client named a rule after the sender alone, the server enforces unique names
// per user, and so "create a rule from this message" failed with "a rule with
// that name already exists" for every sender the operator already had a rule
// for. The three mechanisms below answer three different halves of that, and
// each has a way of failing silently:
//
//   - nameTakenBy can be too EAGER (retrying an error that is not a collision,
//     e.g. bumping an empty name into "(2)" and creating a rule called that),
//     so the negative controls matter more than the positive case.
//   - conditionKey can be too STRICT (two spellings of the same rule keyed
//     differently, so duplicate detection quietly finds nothing, forever — the
//     "ran and did nothing" failure this project keeps meeting).
//   - nameForDraft can be too KEEN (overwriting a name the user typed).
//
// Fixtures use example.com throughout.

import { describe, expect, it } from "vitest";

import {
  MAX_RULE_NAME,
  autoRuleName,
  bumpRuleName,
  conditionKey,
  findDuplicateRule,
  nameForDraft,
  nameTakenBy,
  ruleNameKey,
} from "./ruleIdentity";
import type { Rule, RuleAction, RuleCondition, SetError } from "./jmap/types";

/** The real `rules.autoName` template from the i18n catalog, so these tests
 * assert the shape the user actually sees. Passed in rather than imported
 * because core/ does not depend on app/. */
const format = (key: string, label: string): string => `${key} → ${label}`;

const leaf = (field: string, op: string, value: string): RuleCondition =>
  ({ field, op, value }) as RuleCondition;

function rule(id: string, conditions: RuleCondition, actions: RuleAction[]): Rule {
  return {
    id,
    name: `rule ${id}`,
    priority: 100,
    conditions,
    actions,
    enabled: true,
    systemSuggested: false,
    corrections: 0,
  };
}

const addLabel = (label: string): RuleAction => ({ type: "addLabel", label });

/** Identity resolution: the tests use label ids directly, so a target resolves
 * to itself unless it is blank. */
const resolveSelf = (target: string): string | undefined => target || undefined;

// ---------------------------------------------------------------------------

describe("nameTakenBy — the server's collision, matched structurally", () => {
  it("fires on the SetError internal/api/rule.go actually sends for a taken name", () => {
    const err: SetError = {
      type: "invalidProperties",
      properties: ["name"],
      description: "a rule with that name already exists",
    };
    expect(nameTakenBy(err, "Venmo → Promotions")).toBe(true);
  });

  it("does not depend on the description — the wire contract is type + properties", () => {
    // If this ever regresses to matching English, the description is the thing
    // that will have been keyed off. A collision with no description at all,
    // or with a reworded/localised one, is still a collision.
    expect(nameTakenBy({ type: "invalidProperties", properties: ["name"] }, "Venmo")).toBe(true);
    expect(
      nameTakenBy(
        { type: "invalidProperties", properties: ["name"], description: "ce nom est déjà pris" },
        "Venmo",
      ),
    ).toBe(true);
  });

  // ---- NEGATIVE CONTROLS. Without these the retry could fire on any failure,
  // and "createRule retries" would be indistinguishable from "createRule
  // retries everything three times before reporting it".
  it("does NOT fire on an unrelated invalidProperties error", () => {
    // ErrInvalidRule — a bad regex, an unsupported action — comes back as
    // invalidProperties naming conditions/actions. Bumping the name would
    // resubmit the same broken rule twice more and then report the same error.
    expect(
      nameTakenBy(
        {
          type: "invalidProperties",
          properties: ["conditions", "actions"],
          description: "rules: invalid regex",
        },
        "Venmo",
      ),
    ).toBe(false);
    // Named alongside something else is not a statement about the name alone.
    expect(nameTakenBy({ type: "invalidProperties", properties: ["name", "priority"] }, "V")).toBe(
      false,
    );
    expect(nameTakenBy({ type: "invalidProperties" }, "Venmo")).toBe(false);
  });

  it("does NOT fire on a different error type carrying the same properties", () => {
    expect(nameTakenBy({ type: "forbidden", properties: ["name"] }, "Venmo")).toBe(false);
    expect(nameTakenBy({ type: "invalidPatch", properties: ["name"] }, "Venmo")).toBe(false);
  });

  it("does NOT fire on 'rule name is required', which is structurally identical", () => {
    // createRule answers an empty name with the same type and the same
    // properties (internal/api/rule.go). The only thing that separates them is
    // the name WE sent: a bump here would turn "" into "(2)", the server would
    // accept it, and the user would get a rule called "(2)".
    const err: SetError = {
      type: "invalidProperties",
      properties: ["name"],
      description: "rule name is required",
    };
    expect(nameTakenBy(err, "")).toBe(false);
    expect(nameTakenBy(err, "   ")).toBe(false);
  });
});

describe("bumpRuleName", () => {
  it("appends the attempt number", () => {
    expect(bumpRuleName("Venmo → Promotions", 2)).toBe("Venmo → Promotions (2)");
  });

  it("replaces an existing bump rather than stacking them", () => {
    expect(bumpRuleName("Venmo → Promotions (2)", 3)).toBe("Venmo → Promotions (3)");
  });

  it("stays inside the name budget", () => {
    const long = "x".repeat(200);
    expect(bumpRuleName(long, 2).length).toBe(MAX_RULE_NAME);
    expect(bumpRuleName(long, 2).endsWith(" (2)")).toBe(true);
  });
});

// ---------------------------------------------------------------------------

describe("autoRuleName / ruleNameKey — the label is half the name", () => {
  it("names a rule after BOTH the condition key and the label", () => {
    // The whole fix in one assertion: `Venmo` collided with the rule the user
    // already had for Venmo; `Venmo → Promotions` does not.
    expect(
      autoRuleName(
        { base: "Venmo", label: "Promotions", conditions: { all: [leaf("from", "equals", "no-reply@venmo.example.com")] } },
        format,
      ),
    ).toBe("Venmo → Promotions");
  });

  it("falls back to the prefill's own default while no label is chosen", () => {
    // The draft has to show something from the first frame, before the user has
    // answered the one question this flow cannot answer for them.
    expect(
      autoRuleName({ base: "Venmo", conditions: { all: [leaf("from", "equals", "a@example.com")] } }, format),
    ).toBe("Venmo");
  });

  it("derives a key from the conditions when there is no prefill", () => {
    // The "New rule" path: no suggestion, so the name comes from whatever the
    // first stated condition says.
    expect(
      autoRuleName(
        { label: "Receipts", conditions: { all: [leaf("fromDomain", "equals", "shop.example.com")] } },
        format,
      ),
    ).toBe("shop.example.com → Receipts");
  });

  it("skips conditions that state no value rather than naming a rule after nothing", () => {
    expect(
      autoRuleName(
        {
          label: "Lists",
          conditions: {
            all: [
              { field: "listUnsubscribe", op: "exists" } as RuleCondition,
              leaf("listId", "contains", "announce.example.com"),
            ],
          },
        },
        format,
      ),
    ).toBe("announce.example.com → Lists");
  });

  it("keeps the 48-character trim on the key half, and caps the whole name", () => {
    const subject = "A very long subject line that nobody would ever want to read twice, honestly";
    const key = ruleNameKey(subject, { all: [] });
    expect(key.length).toBe(48);
    expect(key.endsWith("…")).toBe(true);

    const name = autoRuleName({ base: subject, label: "y".repeat(200), conditions: { all: [] } }, format);
    expect(name.length).toBe(MAX_RULE_NAME);
  });
});

describe("nameForDraft — an untouched name is ours, a typed one is not", () => {
  const conditions = { all: [leaf("from", "equals", "no-reply@venmo.example.com")] };

  it("fills an untouched name from the label", () => {
    expect(
      nameForDraft({ name: "Venmo", nameAuto: true, nameBase: "Venmo", conditions }, "Promotions", format),
    ).toBe("Venmo → Promotions");
  });

  it("NEVER overwrites a name the user typed", () => {
    expect(
      nameForDraft({ name: "Venmo money stuff", nameAuto: false, nameBase: "Venmo", conditions }, "Promotions", format),
    ).toBe("Venmo money stuff");
    // Absent flag means the same thing: an existing rule opened for editing has
    // a name its owner chose, and nothing here may touch it.
    expect(nameForDraft({ name: "Payments", nameBase: "Venmo", conditions }, "Promotions", format)).toBe(
      "Payments",
    );
  });

  it("leaves a typed name alone even when it is EXACTLY what we would have generated", () => {
    // The reason "untouched" is a tracked event and not a comparison. Under a
    // comparison this name would look like ours, and picking the name the app
    // suggested would be the one way to lose it: choose a different label
    // afterwards and it silently changes.
    expect(
      nameForDraft(
        { name: "Venmo → Promotions", nameAuto: false, nameBase: "Venmo", conditions },
        "Receipts",
        format,
      ),
    ).toBe("Venmo → Promotions");
  });
});

// ---------------------------------------------------------------------------

describe("conditionKey — two spellings of one rule", () => {
  it("renders a leaf the way internal/rules/induce.go does", () => {
    expect(conditionKey(leaf("from", "equals", "a@example.com"))).toBe("from|equals|a@example.com;");
    expect(conditionKey({ all: [leaf("from", "equals", "a@example.com"), leaf("subject", "contains", "receipt")] })).toBe(
      "all(from|equals|a@example.com;,subject|contains|receipt;,)",
    );
  });

  it("treats values differing only in CASE as the same rule", () => {
    // strings.ToLower on the value, exactly as the Go canonicaliser does — and
    // it matters here because the editor's value is typed by hand while the
    // prefill's comes from a header.
    expect(conditionKey(leaf("from", "equals", "No-Reply@Venmo.Example.com"))).toBe(
      conditionKey(leaf("from", "equals", "no-reply@venmo.example.com")),
    );
  });

  it("unwraps a single-child group — the web editor wraps, the inducer does not", () => {
    // generalizationLadder in induce.go emits a BARE LEAF for a one-conjunct
    // hypothesis, so accepted suggestions are stored unwrapped; the web editor
    // always writes {all: [...]}. If these keyed differently, duplicate
    // detection would silently miss every rule the operator accepted from a
    // suggestion — which is most of them.
    const bare = leaf("fromDomain", "equals", "venmo.example.com");
    expect(conditionKey({ all: [bare] })).toBe(conditionKey(bare));
    expect(conditionKey({ any: [bare] })).toBe(conditionKey(bare));
    expect(conditionKey({ all: [{ all: [bare] }] })).toBe(conditionKey(bare));
  });

  it("separates conditions that differ in field, op, value or header", () => {
    const base = leaf("from", "equals", "a@example.com");
    expect(conditionKey(leaf("to", "equals", "a@example.com"))).not.toBe(conditionKey(base));
    expect(conditionKey(leaf("from", "contains", "a@example.com"))).not.toBe(conditionKey(base));
    expect(conditionKey(leaf("from", "equals", "b@example.com"))).not.toBe(conditionKey(base));
    // The header name is part of the leaf here even though Go omits it: Go's
    // inducer can never emit a header condition, and the editor can. Without
    // this, "X-Spam-Flag is yes" and "X-Mailer is yes" would be one rule.
    expect(conditionKey({ field: "header", header: "X-Spam-Flag", op: "equals", value: "yes" } as RuleCondition)).not.toBe(
      conditionKey({ field: "header", header: "X-Mailer", op: "equals", value: "yes" } as RuleCondition),
    );
  });

  it("separates all() from any() over the same children", () => {
    const kids = [leaf("from", "equals", "a@example.com"), leaf("subject", "contains", "x")];
    expect(conditionKey({ all: kids })).not.toBe(conditionKey({ any: kids }));
  });
});

describe("findDuplicateRule", () => {
  const draftConditions = { all: [leaf("from", "equals", "no-reply@venmo.example.com")] };
  const draft = { conditions: draftConditions, actions: [addLabel("promotions")] };

  it("finds an existing rule with an equivalent condition, unwrapping included", () => {
    const existing = rule("r1", leaf("from", "equals", "No-Reply@Venmo.Example.com"), [addLabel("promotions")]);
    const found = findDuplicateRule(draft, [existing], resolveSelf);
    expect(found?.rule.id).toBe("r1");
    expect(found?.sameLabel).toBe(true);
  });

  it("surfaces a rule keyed the same way with a DIFFERENT label, flagged as such", () => {
    // The deliberate choice (#292): the user's question is "is this sender
    // already handled", and "yes, under Receipts" is the answer they need —
    // creating this rule anyway means the mail arrives with both labels. It is
    // reported as a different claim, not the same one.
    const existing = rule("r2", { all: [leaf("from", "equals", "no-reply@venmo.example.com")] }, [addLabel("receipts")]);
    const found = findDuplicateRule(draft, [existing], resolveSelf);
    expect(found?.rule.id).toBe("r2");
    expect(found?.sameLabel).toBe(false);
  });

  it("prefers the same-label match over a same-key one", () => {
    const other = rule("r2", leaf("from", "equals", "no-reply@venmo.example.com"), [addLabel("receipts")]);
    const same = rule("r3", leaf("from", "equals", "no-reply@venmo.example.com"), [addLabel("promotions")]);
    expect(findDuplicateRule(draft, [other, same], resolveSelf)?.rule.id).toBe("r3");
  });

  // ---- NEGATIVE CONTROLS: what must NOT be called a duplicate.
  it("does not false-positive on a different condition", () => {
    expect(
      findDuplicateRule(draft, [rule("r4", leaf("from", "equals", "billing@other.example.com"), [addLabel("promotions")])], resolveSelf),
    ).toBeNull();
    // Same sender, broader test — a domain rule is not the sender rule.
    expect(
      findDuplicateRule(draft, [rule("r5", leaf("fromDomain", "equals", "venmo.example.com"), [addLabel("promotions")])], resolveSelf),
    ).toBeNull();
    // Same key plus a restriction is a narrower rule, not this one.
    expect(
      findDuplicateRule(
        draft,
        [rule("r6", { all: [leaf("from", "equals", "no-reply@venmo.example.com"), leaf("subject", "contains", "receipt")] }, [addLabel("promotions")])],
        resolveSelf,
      ),
    ).toBeNull();
  });

  it("ignores rules that apply no label", () => {
    // A stop/skipInbox rule keyed on the same sender does something
    // categorically different; offering to "edit" it would invite the user to
    // overwrite a routing rule with a labelling one.
    const routing = rule("r7", leaf("from", "equals", "no-reply@venmo.example.com"), [
      { type: "skipInbox" },
      { type: "stop" },
    ]);
    expect(findDuplicateRule(draft, [routing], resolveSelf)).toBeNull();
  });

  it("never accuses a rule of duplicating itself", () => {
    const self = rule("r8", draftConditions, [addLabel("promotions")]);
    expect(findDuplicateRule({ id: "r8", ...draft }, [self], resolveSelf)).toBeNull();
  });

  it("says nothing about a draft whose conditions state nothing yet", () => {
    // The "New rule" starting point is `from contains ""`, which matches
    // everything and pins nothing. Without this, every blank new rule would be
    // reported as a duplicate of the first empty rule in the list.
    const blank = { conditions: { all: [leaf("from", "contains", "")] }, actions: [addLabel("promotions")] };
    const other = rule("r9", { all: [leaf("from", "contains", "")] }, [addLabel("receipts")]);
    expect(findDuplicateRule(blank, [other], resolveSelf)).toBeNull();
  });
});
