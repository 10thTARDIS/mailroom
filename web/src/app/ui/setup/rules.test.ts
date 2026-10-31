// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  ACCEPT_ALL_THRESHOLD,
  acceptable,
  candidateKey,
  coverageText,
  describeConditions,
  isAccepted,
} from "./rules";
import type { InducedRule, RuleCondition } from "../../../core/jmap/types";

function candidate(over: Partial<InducedRule> = {}): InducedRule {
  return {
    conditions: { field: "fromDomain", op: "equals", value: "creators.example" },
    matched: 320,
    covers: 320,
    collateral: 0,
    conflicts: 0,
    conflictLabels: [],
    samples: [],
    ...over,
  };
}

describe("acceptable — what `Accept all above 90%` means", () => {
  it("is the ratio of what it would have placed to what was measured", () => {
    expect(acceptable({ covers: 320, examples: 320 }, ACCEPT_ALL_THRESHOLD)).toBe(true);
    expect(acceptable({ covers: 190, examples: 200 }, ACCEPT_ALL_THRESHOLD)).toBe(true);
    expect(acceptable({ covers: 179, examples: 200 }, ACCEPT_ALL_THRESHOLD)).toBe(false);
    // Exactly on the bar is above it: the button says "above 90%" and 0.9 is what
    // the design's number means.
    expect(acceptable({ covers: 180, examples: 200 }, ACCEPT_ALL_THRESHOLD)).toBe(true);
  });

  it("is FALSE with no denominator", () => {
    // NaN comparisons are all false and `covers / 0` is Infinity, so an inline
    // division would either silently skip this folder or bulk-accept it depending
    // on how the expression happened to be written. An unmeasured ratio is not a
    // high one (#176).
    expect(acceptable({ covers: 0, examples: 0 }, ACCEPT_ALL_THRESHOLD)).toBe(false);
    expect(acceptable({ covers: 5, examples: 0 }, ACCEPT_ALL_THRESHOLD)).toBe(false);
    expect(acceptable({ covers: 5, examples: -1 }, ACCEPT_ALL_THRESHOLD)).toBe(false);
  });
});

describe("candidateKey — accepting the same proposal twice is one rule", () => {
  const conditions: RuleCondition = { field: "fromDomain", op: "equals", value: "creators.example" };

  it("is stable across a re-induction that returns an equal tree", () => {
    // Step 3 re-asks Rule/induce on every visit, so coming Back and pressing
    // Accept again arrives with a DIFFERENT object that means the same thing.
    const again: RuleCondition = { value: "creators.example", op: "equals", field: "fromDomain" };
    expect(candidateKey(again, "Creators")).toBe(candidateKey(conditions, "Creators"));
    expect(isAccepted([candidateKey(conditions, "Creators")], again, "Creators")).toBe(true);
  });

  it("separates two proposals that differ only in the label they apply", () => {
    expect(candidateKey(conditions, "Creators")).not.toBe(candidateKey(conditions, "Notices"));
  });

  it("separates a header condition from a leaf that never had one", () => {
    const withHeader: RuleCondition = { field: "header", header: "X-Mailer", op: "exists" };
    const withoutHeader: RuleCondition = { field: "header", op: "exists" };
    expect(candidateKey(withHeader, "L")).not.toBe(candidateKey(withoutHeader, "L"));
  });

  it("treats a bare leaf and a single-child group as ONE rule", () => {
    // The inducer emits a BARE LEAF for a one-conjunct hypothesis while the editor
    // always wraps, and #446's proposals are mostly that shape — so this is the
    // case a second canonicaliser would get wrong, and the reason this keys on
    // core/ruleIdentity's own conditionKey rather than a fresh walk.
    const bare: RuleCondition = { field: "fromDomain", op: "equals", value: "creators.example" };
    expect(candidateKey({ all: [bare] }, "Creators")).toBe(candidateKey(bare, "Creators"));
  });

  it("distinguishes an all-group from an any-group of the same leaves", () => {
    const leaves = [
      { field: "from", op: "contains", value: "a" },
      { field: "subject", op: "contains", value: "b" },
    ] as RuleCondition[];
    expect(candidateKey({ all: leaves }, "L")).not.toBe(candidateKey({ any: leaves }, "L"));
  });
});

describe("describeConditions — the client twin of rules.DescribeCondition", () => {
  it("renders a leaf out of the editor's own words for the field and the operator", () => {
    expect(
      describeConditions({ field: "fromDomain", op: "equals", value: "creators.example" }),
    ).toEqual(["Sender domain is creators.example"]);
  });

  it("renders a presence test without a value", () => {
    // `exists` reads no value, and one printed beside it would be a condition the
    // engine does not test.
    expect(describeConditions({ field: "listUnsubscribe", op: "exists" })).toEqual([
      "has Has unsubscribe link",
    ]);
  });

  it("names the header itself rather than the word `Header…`", () => {
    expect(describeConditions({ field: "header", header: "X-Campaign", op: "exists" })).toEqual([
      "has X-Campaign",
    ]);
  });

  it("gives ONE PHRASE PER CONJUNCT and collapses a disjunction to one", () => {
    expect(
      describeConditions({
        all: [
          { field: "from", op: "contains", value: "alice" },
          { field: "subject", op: "contains", value: "invoice" },
        ],
      }),
    ).toEqual(["Sender contains alice", "Subject contains invoice"]);
    expect(
      describeConditions({
        any: [
          { field: "from", op: "contains", value: "alice" },
          { field: "from", op: "contains", value: "bob" },
        ],
      }),
    ).toEqual(["Sender contains alice or Sender contains bob"]);
  });

  it("parenthesises a nested all inside an any, so the boolean structure survives", () => {
    expect(
      describeConditions({
        any: [
          {
            all: [
              { field: "from", op: "contains", value: "a" },
              { field: "subject", op: "contains", value: "b" },
            ],
          },
          { field: "from", op: "contains", value: "c" },
        ],
      }),
    ).toEqual(["(Sender contains a and Subject contains b) or Sender contains c"]);
  });

  it("yields no phrase for a node that is neither a group nor a leaf", () => {
    // Such a node matches nothing server-side, so no proposal can have come from
    // one — and an empty string joined into an `any` would render as "x or  or y".
    expect(describeConditions({})).toEqual([]);
    expect(
      describeConditions({ any: [{}, { field: "from", op: "contains", value: "a" }] }),
    ).toEqual(["Sender contains a"]);
  });
});

describe("coverageText", () => {
  it("states the claim with its denominator, grouped", () => {
    expect(coverageText({ examples: 2000 }, candidate({ covers: 1980 }))).toBe(
      "would have placed 1,980 / 2,000",
    );
  });
});
