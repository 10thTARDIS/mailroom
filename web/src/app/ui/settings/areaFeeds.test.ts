// SPDX-License-Identifier: AGPL-3.0-only

// TWO FAILURES ARE GUARDED HERE AND BOTH ARE SILENT ONES.
//
//   1. `feedPatch` sending only the tab that changed. `tabs`/`labelIds` are
//      REPLACE-ALL server-side (internal/api/area.go), so that write SUCCEEDS and
//      unsubscribes the area from every other feed it had. Nothing on screen says
//      so until the next reload.
//   2. `labelsInNoArea` forgetting that an area's OWN label is claimed. It does
//      not appear in `labelIds`, so the naive filter reports every area's own
//      label as homeless — and then offers to assign Receipts to Receipts, which
//      the server refuses.
//
// So the assertions are about the WHOLE arrays and about the own label, not about
// the one value that changed.

import { describe, expect, it } from "vitest";

import { feedPatch, labelsInNoArea, movedByOne, rulesForArea } from "./areaFeeds";
import type { Area, CategoryTab, Label, Rule } from "../../../core/jmap/types";

const L_OWN = "01JBQZ3K7WX2N4Y6P8R0S1T2L0";
const L_A = "01JBQZ3K7WX2N4Y6P8R0S1T2LA";
const L_B = "01JBQZ3K7WX2N4Y6P8R0S1T2LB";
const L_C = "01JBQZ3K7WX2N4Y6P8R0S1T2LC";

/** An Area in the shape the server serves, with only the fields under test
 * varied — every other field carries the value a real record has, so a fixture
 * cannot pass by omitting the property being exercised. */
function area(over: Partial<Area> = {}): Area {
  return {
    id: "01JBQZ3K7WX2N4Y6P8R0S1T2A0",
    name: "Work",
    position: 0,
    labelId: L_OWN,
    tabs: [],
    labelIds: [],
    autoArchive: true,
    autoArchiveDays: null,
    hiddenFromRail: false,
    ...over,
  };
}

function label(id: string, name: string, over: Partial<Label> = {}): Label {
  return {
    id,
    name,
    color: null,
    parentId: null,
    mirrorToImap: false,
    keyword: null,
    tab: null,
    ...over,
  };
}

describe("feedPatch — the arrays are always complete", () => {
  it("returns EVERY tab, not only the one toggled on", () => {
    const a = area({ tabs: ["promotions", "updates"] });
    expect(feedPatch(a, { kind: "toggleTab", tab: "forums" })).toEqual({
      tabs: ["promotions", "updates", "forums"],
      labelIds: [],
    });
  });

  it("returns EVERY remaining tab when one is toggled off", () => {
    const a = area({ tabs: ["promotions", "updates", "forums"] });
    expect(feedPatch(a, { kind: "toggleTab", tab: "updates" })).toEqual({
      tabs: ["promotions", "forums"],
      labelIds: [],
    });
  });

  it("sends the LABELS too when only a tab changed", () => {
    // The write that loses an area's label feeds. `labelIds` is replace-all when
    // present and the patch below carries both, so a tab toggle cannot wipe them.
    const a = area({ tabs: ["social"], labelIds: [L_A, L_B] });
    expect(feedPatch(a, { kind: "toggleTab", tab: "social" })).toEqual({
      tabs: [],
      labelIds: [L_A, L_B],
    });
  });

  it("sends the TABS too when only a label changed", () => {
    const a = area({ tabs: ["social", "updates"], labelIds: [L_A] });
    expect(feedPatch(a, { kind: "addLabel", labelId: L_B })).toEqual({
      tabs: ["social", "updates"],
      labelIds: [L_A, L_B],
    });
  });

  it("removes a label and keeps the rest in order", () => {
    const a = area({ tabs: ["social"], labelIds: [L_A, L_B, L_C] });
    expect(feedPatch(a, { kind: "removeLabel", labelId: L_B })).toEqual({
      tabs: ["social"],
      labelIds: [L_A, L_C],
    });
  });

  it("never adds the area's OWN label as a feed", () => {
    // internal/api/area.go refuses it — "the area's own label is already its
    // feed" — and the refusal is per object, so it would lose the tab edit beside
    // it as well.
    const a = area({ tabs: ["social"], labelIds: [L_A] });
    expect(feedPatch(a, { kind: "addLabel", labelId: L_OWN })).toEqual({
      tabs: ["social"],
      labelIds: [L_A],
    });
  });

  it("strips the own label if a server ever sent it inside labelIds", () => {
    const a = area({ labelIds: [L_OWN, L_A] });
    expect(feedPatch(a, { kind: "addLabel", labelId: L_B }).labelIds).toEqual([L_A, L_B]);
  });

  it("does not add a label twice", () => {
    const a = area({ labelIds: [L_A] });
    expect(feedPatch(a, { kind: "addLabel", labelId: L_A }).labelIds).toEqual([L_A]);
  });

  it("does not mutate the area it was given", () => {
    // The area is a store object shared with the rail and the desk. A patch that
    // mutated it would move those surfaces before the server had agreed.
    const a = area({ tabs: ["social"], labelIds: [L_A] });
    feedPatch(a, { kind: "toggleTab", tab: "forums" });
    feedPatch(a, { kind: "addLabel", labelId: L_B });
    expect(a.tabs).toEqual(["social"]);
    expect(a.labelIds).toEqual([L_A]);
  });

  it("round-trips a toggle: on then off is where it started", () => {
    const a = area({ tabs: ["promotions"], labelIds: [L_A] });
    const once = feedPatch(a, { kind: "toggleTab", tab: "forums" });
    const twice = feedPatch({ ...a, ...once }, { kind: "toggleTab", tab: "forums" });
    expect(twice).toEqual({ tabs: ["promotions"], labelIds: [L_A] });
  });

  it("carries every one of the five tabs when they are all on", () => {
    const all: CategoryTab[] = ["primary", "social", "promotions", "updates", "forums"];
    const a = area({ tabs: all });
    expect(feedPatch(a, { kind: "addLabel", labelId: L_A }).tabs).toEqual(all);
  });
});

describe("labelsInNoArea", () => {
  it("does NOT report an area's own label as homeless", () => {
    // The own label is the most firmly attached label there is; it simply is not
    // in `labelIds`. Reporting it would offer to assign an area to itself.
    const labels = [label(L_OWN, "Work"), label(L_A, "Receipts")];
    const got = labelsInNoArea(labels, [area({ labelId: L_OWN })]);
    expect(got.map((l) => l.id)).toEqual([L_A]);
  });

  it("does not report a label another area feeds from", () => {
    const labels = [label(L_A, "Receipts"), label(L_B, "Travel")];
    const got = labelsInNoArea(labels, [area({ labelIds: [L_A] })]);
    expect(got.map((l) => l.id)).toEqual([L_B]);
  });

  it("counts claims across every area, not only the first", () => {
    const other = area({ id: "01JBQZ3K7WX2N4Y6P8R0S1T2A1", labelId: L_B, labelIds: [L_C] });
    const labels = [label(L_OWN, "Work"), label(L_A, "A"), label(L_B, "B"), label(L_C, "C")];
    const got = labelsInNoArea(labels, [area({ labelId: L_OWN }), other]);
    expect(got.map((l) => l.id)).toEqual([L_A]);
  });

  it("orders by name so the list does not reshuffle between renders", () => {
    const labels = [label(L_C, "Zebra"), label(L_A, "Apple"), label(L_B, "Mango")];
    expect(labelsInNoArea(labels, [area()]).map((l) => l.name)).toEqual([
      "Apple",
      "Mango",
      "Zebra",
    ]);
  });

  it("returns an empty list when every label is claimed", () => {
    // A measured empty: there ARE areas and there ARE labels, and none is
    // homeless. Distinct from the caller's "no areas loaded" case, which must not
    // reach this function at all.
    const labels = [label(L_OWN, "Work"), label(L_A, "Receipts")];
    expect(labelsInNoArea(labels, [area({ labelId: L_OWN, labelIds: [L_A] })])).toEqual([]);
  });
});

describe("movedByOne — what the arrows produce", () => {
  it("swaps with the neighbour above", () => {
    expect(movedByOne(["a", "b", "c"], 1, -1)).toEqual(["b", "a", "c"]);
  });

  it("swaps with the neighbour below", () => {
    expect(movedByOne(["a", "b", "c"], 1, 1)).toEqual(["a", "c", "b"]);
  });

  it("leaves the list alone at either end", () => {
    // Not a clamp: a clamp would turn a mis-click at the end into a write that
    // renumbers every area for nothing.
    expect(movedByOne(["a", "b"], 0, -1)).toEqual(["a", "b"]);
    expect(movedByOne(["a", "b"], 1, 1)).toEqual(["a", "b"]);
  });

  it("does not mutate the list it was given", () => {
    const list = ["a", "b", "c"];
    movedByOne(list, 0, 1);
    expect(list).toEqual(["a", "b", "c"]);
  });

  it("is a no-op for an index that is not in the list", () => {
    expect(movedByOne(["a"], 5, 1)).toEqual(["a"]);
    expect(movedByOne([], 0, 1)).toEqual([]);
  });
});

describe("rulesForArea", () => {
  function rule(over: Partial<Rule> & Pick<Rule, "id" | "name" | "actions">): Rule {
    return {
      priority: 100,
      conditions: { all: [] },
      enabled: true,
      systemSuggested: false,
      corrections: 0,
      ...over,
    };
  }

  it("matches an addLabel naming the area's own label by id", () => {
    const r = rule({ id: "r1", name: "Invoices", actions: [{ type: "addLabel", label: L_OWN }] });
    expect(rulesForArea([r], area({ labelId: L_OWN }), undefined)).toEqual([r]);
  });

  it("matches the keyword atom and the name too", () => {
    // RuleAction.label is "a label id, a keyword atom, or a label name" — a rule
    // written by hand names whichever its author had, and matching only the id
    // would report no rules for an area three rules feed.
    const byKeyword = rule({
      id: "r2",
      name: "Atom",
      actions: [{ type: "addLabel", label: "MailroomLabel-work" }],
    });
    const byName = rule({ id: "r3", name: "Name", actions: [{ type: "addLabel", label: "Work" }] });
    const own = label(L_OWN, "Work", { keyword: "MailroomLabel-work" });
    expect(rulesForArea([byKeyword, byName], area({ labelId: L_OWN }), own)).toEqual([
      byKeyword,
      byName,
    ]);
  });

  it("ignores a rule that labels something else, and one with no label at all", () => {
    const other = rule({ id: "r4", name: "Other", actions: [{ type: "addLabel", label: L_A }] });
    const flag = rule({ id: "r5", name: "Flag", actions: [{ type: "flag" }] });
    expect(rulesForArea([other, flag], area({ labelId: L_OWN }), undefined)).toEqual([]);
  });
});
