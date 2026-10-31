// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the needs-a-category grouping (#176).
//
// This module is deliberately pure so it can be checked without a browser, and
// it is where the surface's user-facing NUMBERS come from: how many messages a
// batch will label, and how many distinct senders that batch spans. The second
// is a safety signal — it is what tells the operator that `forward@` is a
// forwarding address rather than a service, and that one label across it would
// be the heterogeneous sweep #175 identified as the difference between a good
// binding and a bad one. A wrong number here is not cosmetic.

import { describe, expect, it } from "vitest";

import { aliasOf, groupQueue, isBatchable, layoutQueue, senderDomainOf } from "./needsCategory";
import type { EffectiveEmail } from "./store";

/** A minimal EffectiveEmail with only the fields grouping reads. */
function msg(
  id: string,
  opts: { deliveredTo?: string | null; from?: string; confidence?: number } = {},
): EffectiveEmail {
  const confidence = opts.confidence;
  return {
    id,
    deliveredTo: opts.deliveredTo ?? null,
    from: opts.from ? [{ email: opts.from }] : null,
    subject: "s",
    suggestions:
      confidence === undefined ? [] : [{ labelId: "L", confidence, reasons: null }],
    topConfidence: confidence ?? null,
  } as unknown as EffectiveEmail;
}

describe("aliasOf", () => {
  it("keeps the local part and drops the domain, which is the same for every group under a catchall", () => {
    expect(aliasOf({ deliveredTo: "bank@example.com" })).toBe("bank@");
  });

  it("is null for an address with no local part, which would group unrelated mail under a bare @", () => {
    expect(aliasOf({ deliveredTo: "@example.com" })).toBeNull();
    expect(aliasOf({ deliveredTo: "" })).toBeNull();
    expect(aliasOf({ deliveredTo: null })).toBeNull();
  });
});

describe("senderDomainOf", () => {
  it("keeps the FULL domain rather than guessing a registrable one", () => {
    // e.shop.example and shop.example may or may not be one service, and without a
    // public-suffix list there is no honest way to decide. Grouping them apart
    // costs the user one extra keystroke; merging them wrongly costs a label.
    expect(senderDomainOf({ from: [{ email: "deals@e.shop.example" }] })).toBe("e.shop.example");
  });

  it("is null when there is no usable From", () => {
    expect(senderDomainOf({ from: null })).toBeNull();
    expect(senderDomainOf({ from: [{ email: "bare-local-part" }] })).toBeNull();
  });
});

describe("groupQueue", () => {
  it("groups by alias, and by sender domain when there is no alias", () => {
    const groups = groupQueue([
      msg("1", { deliveredTo: "bank@example.com", from: "a@bank.example" }),
      msg("2", { deliveredTo: "bank@example.com", from: "b@bank.example" }),
      msg("3", { from: "news@news.example" }),
      msg("4", { from: "other@news.example" }),
    ]);
    const byKey = Object.fromEntries(groups.map((g) => [g.key, g]));
    expect(byKey["alias:bank@"]?.ids).toEqual(["1", "2"]);
    expect(byKey["domain:news.example"]?.ids).toEqual(["3", "4"]);
  });

  it("groups EVERY message it is given, not only the ones with open questions", () => {
    // The regression that would empty the whole surface: on a mailbox with no
    // trained model the entire queue is unlabelled mail with not a suggestion in
    // it, so filtering on `suggestions.length` here renders nothing at all.
    const groups = groupQueue([
      msg("1", { deliveredTo: "parcel@example.com" }),
      msg("2", { deliveredTo: "parcel@example.com" }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.ids).toEqual(["1", "2"]);
  });

  it("orders groups by their most confident member, so the best question stays first", () => {
    const groups = groupQueue([
      msg("1", { deliveredTo: "a@example.com", confidence: 0.3 }),
      msg("2", { deliveredTo: "b@example.com", confidence: 0.7 }),
      msg("3", { deliveredTo: "c@example.com" }),
    ]);
    expect(groups.map((g) => g.key)).toEqual(["alias:b@", "alias:a@", "alias:c@"]);
  });

  it("ranks a group by its BEST member, never its worst", () => {
    const groups = groupQueue([
      msg("1", { deliveredTo: "a@example.com", confidence: 0.2 }),
      msg("2", { deliveredTo: "a@example.com", confidence: 0.9 }),
      msg("3", { deliveredTo: "b@example.com", confidence: 0.5 }),
    ]);
    expect(groups[0]?.key).toBe("alias:a@");
    expect(groups[0]?.topConfidence).toBeCloseTo(0.9);
  });

  it("counts DISTINCT sender domains, which is what says whether a batch is one service or a crowd", () => {
    // The real case this exists for: an address used to forward everything in.
    const groups = groupQueue([
      msg("1", { deliveredTo: "forward@example.com", from: "a@one.com" }),
      msg("2", { deliveredTo: "forward@example.com", from: "b@two.com" }),
      msg("3", { deliveredTo: "forward@example.com", from: "c@two.com" }),
      msg("4", { deliveredTo: "bank@example.com", from: "x@bank.example" }),
      msg("5", { deliveredTo: "bank@example.com", from: "y@bank.example" }),
    ]);
    const byKey = Object.fromEntries(groups.map((g) => [g.key, g]));
    expect(byKey["alias:forward@"]?.senderCount).toBe(2);
    // One service: no "from N different senders" caveat is shown for this one.
    expect(byKey["alias:bank@"]?.senderCount).toBe(1);
  });

  it("keeps a message with neither alias nor sender in its own group rather than a shared bucket", () => {
    // A shared "unknown" bucket would offer one label across genuinely unrelated
    // mail — the one outcome this surface must never produce.
    const groups = groupQueue([msg("1"), msg("2")]);
    expect(groups).toHaveLength(2);
    expect(groups.every((g) => g.ids.length === 1)).toBe(true);
  });
});

describe("a forwarded address is not a service address (#186)", () => {
  /** n messages to one alias, spread across the given sender domains. */
  const toAlias = (alias: string, domains: string[]): EffectiveEmail[] =>
    domains.map((d, i) => msg(`${alias}-${i}`, { deliveredTo: `${alias}@x.com`, from: `s@${d}` }));

  it("keeps an alias whose mail comes from essentially one sender", () => {
    // bank@ on a real mailbox: a few messages, 2 domains, most from one.
    const groups = groupQueue(
      toAlias("bank", ["bank.example", "bank.example", "bank.example", "alerts.bank.example"]),
    );
    expect(groups).toHaveLength(1);
    expect(groups[0]?.kind).toBe("alias");
    expect(groups[0]?.display).toBe("bank@");
    expect(groups[0]?.ids).toHaveLength(4);
  });

  it("dissolves a forwarder into its senders", () => {
    // forward@ on a real mailbox: many messages, dozens of domains, a tiny top share. No
    // sender is anywhere near a majority, so the alias says nothing about the
    // mail — only about the plumbing that delivered it.
    const groups = groupQueue(
      toAlias("forward", [
        "news.example",
        "news.example",
        "club.example",
        "club.example",
        "books.example",
        "tax.finance.example",
      ]),
    );
    expect(groups.every((g) => g.kind === "domain")).toBe(true);
    const byDisplay = Object.fromEntries(groups.map((g) => [g.display, g.ids.length]));
    expect(byDisplay["news.example"]).toBe(2);
    expect(byDisplay["club.example"]).toBe(2);
    expect(byDisplay["books.example"]).toBe(1);
    expect(byDisplay["tax.finance.example"]).toBe(1);
  });

  it("merges dissolved mail with domain groups from elsewhere", () => {
    // The same sender reaching both the forwarded address and the catchall is
    // ONE question, not two — it is the same sender either way.
    const groups = groupQueue([
      ...toAlias("forward", ["news.example", "club.example", "books.example"]),
      msg("direct", { from: "news@news.example" }), // no alias at all
    ]);
    const verge = groups.find((g) => g.display === "news.example");
    expect(verge?.ids).toHaveLength(2);
    expect(verge?.ids).toContain("direct");
  });

  it("treats a lone sender as concentrated, whatever the count", () => {
    // One message at a brand-new alias has nothing to disagree with it yet, and
    // the alias is the better guess.
    const groups = groupQueue(toAlias("newthing", ["newthing.com"]));
    expect(groups[0]?.kind).toBe("alias");
  });

  it("keeps an exact half — a majority test, and half is not a minority", () => {
    // vendor2@ on a real mailbox sits exactly here: 4 messages, 2 domains.
    const groups = groupQueue(toAlias("vendor2", ["vendor.example", "vendor.example", "e.vendor.example", "e.vendor.example"]));
    expect(groups).toHaveLength(1);
    expect(groups[0]?.kind).toBe("alias");
  });

  it("loses no message when an alias dissolves", () => {
    const input = toAlias("forward", ["a.com", "b.com", "c.com", "d.com"]);
    const groups = groupQueue(input);
    const out = groups.flatMap((g) => g.ids).sort();
    expect(out).toEqual(input.map((m) => m.id).sort());
  });
});

describe("layoutQueue (#183)", () => {
  /** Two batchable groups and three one-offs, deliberately interleaved on
   * input — which is how they arrive, since the queue is ordered by confidence
   * and not by whether a message happens to have company. */
  const mixed = () => [
    msg("magazine1", { deliveredTo: "magazine@x.com", from: "a@magazine.example" }),
    msg("appliance", { deliveredTo: "appliance@x.com", from: "n@appliance.example" }),
    msg("magazine2", { deliveredTo: "magazine@x.com", from: "b@magazine.example" }),
    msg("taxes", { deliveredTo: "tax@x.com", from: "k@tax.example" }),
    msg("vendor1", { deliveredTo: "vendor2@x.com", from: "a@vendor.example" }),
    msg("books", { deliveredTo: "books@x.com", from: "tim@books.example" }),
    msg("vendor2", { deliveredTo: "vendor2@x.com", from: "b@vendor.example" }),
  ];

  it("puts every batch above every one-off", () => {
    const { ids } = layoutQueue(mixed());
    expect(ids.slice(0, 4)).toEqual(["magazine1", "magazine2", "vendor1", "vendor2"]);
    expect(ids.slice(4).sort()).toEqual(["appliance", "books", "taxes"]);
  });

  it("keeps each batch's members adjacent", () => {
    const { ids } = layoutQueue(mixed());
    expect(ids.indexOf("magazine2")).toBe(ids.indexOf("magazine1") + 1);
    expect(ids.indexOf("vendor2")).toBe(ids.indexOf("vendor1") + 1);
  });

  it("loses nothing and duplicates nothing", () => {
    // The display order is what the shell navigates, so a dropped id is a
    // message the user can neither see nor reach.
    const input = mixed();
    const { ids } = layoutQueue(input);
    expect([...ids].sort()).toEqual(input.map((m) => m.id).sort());
  });

  it("marks the header, the members and the closing row of each batch", () => {
    const { headerFor, grouped, groupLast } = layoutQueue(mixed());
    expect(headerFor.get("magazine1")?.display).toBe("magazine@");
    expect(headerFor.has("magazine2")).toBe(false); // header goes above the FIRST only
    expect(grouped.has("magazine1")).toBe(true);
    expect(grouped.has("magazine2")).toBe(true);
    expect(grouped.has("taxes")).toBe(false); // a one-off is not in a group
    expect(groupLast.has("magazine2")).toBe(true);
    expect(groupLast.has("magazine1")).toBe(false);
  });

  it("starts the one-off section at the first ungrouped row", () => {
    const { ids, restStartsAt } = layoutQueue(mixed());
    expect(restStartsAt).toBe(ids[4]);
  });

  it("draws no divider when there is nothing above it to divide from", () => {
    // A queue of nothing but one-offs needs no heading announcing that fact.
    const { restStartsAt } = layoutQueue([
      msg("a", { from: "a@one.com" }),
      msg("b", { from: "b@two.com" }),
    ]);
    expect(restStartsAt).toBeNull();
  });

  it("draws no divider when everything is in a batch", () => {
    const { restStartsAt } = layoutQueue([
      msg("a", { deliveredTo: "x@d.com", from: "a@x.com" }),
      msg("b", { deliveredTo: "x@d.com", from: "b@x.com" }),
    ]);
    expect(restStartsAt).toBeNull();
  });

  it("orders batches by their most confident member", () => {
    const { ids } = layoutQueue([
      msg("lo1", { deliveredTo: "lo@d.com", confidence: 0.2 }),
      msg("lo2", { deliveredTo: "lo@d.com", confidence: 0.2 }),
      msg("hi1", { deliveredTo: "hi@d.com", confidence: 0.9 }),
      msg("hi2", { deliveredTo: "hi@d.com", confidence: 0.3 }),
    ]);
    expect(ids).toEqual(["hi1", "hi2", "lo1", "lo2"]);
  });
});

describe("isBatchable", () => {
  it("needs two, because one message shares its alias with itself", () => {
    expect(isBatchable({ ids: ["1"] } as never)).toBe(false);
    expect(isBatchable({ ids: ["1", "2"] } as never)).toBe(true);
  });
});
