// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import { canDisable, labelsPerTab, moveTab, stripOrder } from "./categoryTabs";
import type { CategoryTab, CategoryTabState, Id } from "./jmap/types";

// Arranging the strip (#228).
//
// The assertions are deliberately about the ORDER THE USER SEES AFTERWARDS
// rather than about the numbers in the patch. A patch that renumbers correctly
// and a patch that swaps two values look equally plausible in isolation; only
// re-sorting the result tells them apart, and they differ exactly where it
// matters (see the tied-positions case).

function tab(id: string, name: CategoryTab, position: number, enabled = true): CategoryTabState {
  return { id, tab: name, enabled, position };
}

/** The strip as the user would see it after the server applied `patch`. */
function afterPatch(
  tabs: Record<string, CategoryTabState>,
  patch: Record<Id, { position: number }>,
): CategoryTab[] {
  const next: Record<string, CategoryTabState> = {};
  for (const [id, t] of Object.entries(tabs)) {
    const p = patch[id];
    next[id] = p ? { ...t, position: p.position } : t;
  }
  return stripOrder(next).map((t) => t.tab);
}

function byId(list: CategoryTabState[]): Record<string, CategoryTabState> {
  const out: Record<string, CategoryTabState> = {};
  for (const t of list) out[t.id] = t;
  return out;
}

// Gmail's default arrangement, which is what the server materialises.
const seeded = byId([
  tab("t-pri", "primary", 0),
  tab("t-soc", "social", 1),
  tab("t-pro", "promotions", 2),
  tab("t-upd", "updates", 3),
  tab("t-for", "forums", 4),
]);

describe("stripOrder", () => {
  it("orders by position, then by name", () => {
    expect(stripOrder(seeded).map((t) => t.tab)).toEqual([
      "primary",
      "social",
      "promotions",
      "updates",
      "forums",
    ]);
  });

  it("breaks a tie by name, matching the server's ORDER BY position, tab", () => {
    const tied = byId([tab("t-upd", "updates", 1), tab("t-for", "forums", 1)]);
    expect(stripOrder(tied).map((t) => t.tab)).toEqual(["forums", "updates"]);
  });
});

describe("moveTab", () => {
  it("moves one slot earlier", () => {
    const patch = moveTab(seeded, "t-pro", -1);
    expect(afterPatch(seeded, patch)).toEqual([
      "primary",
      "promotions",
      "social",
      "updates",
      "forums",
    ]);
  });

  it("moves one slot later", () => {
    const patch = moveTab(seeded, "t-soc", 1);
    expect(afterPatch(seeded, patch)).toEqual([
      "primary",
      "promotions",
      "social",
      "updates",
      "forums",
    ]);
  });

  it("patches only the rows that actually moved", () => {
    // Two adjacent rows swap places; the three either side of them keep the
    // positions they already have, so the request says so.
    expect(Object.keys(moveTab(seeded, "t-pro", -1)).sort()).toEqual(["t-pro", "t-soc"]);
  });

  it("returns nothing at either end, rather than a request that changes nothing", () => {
    expect(moveTab(seeded, "t-pri", -1)).toEqual({});
    expect(moveTab(seeded, "t-for", 1)).toEqual({});
  });

  it("returns nothing for an id it does not hold", () => {
    expect(moveTab(seeded, "t-nope", 1)).toEqual({});
  });

  // THE CASE THAT DECIDES THE IMPLEMENTATION. With every position equal, display
  // order is the name tie-break — so swapping two position VALUES swaps 1 for 1
  // and the strip does not move. Renumbering from the order on screen does.
  it("still moves a tab when every position is the same", () => {
    const tied = byId([
      tab("t-pri", "primary", 0),
      tab("t-soc", "social", 0),
      tab("t-pro", "promotions", 0),
      tab("t-upd", "updates", 0),
      tab("t-for", "forums", 0),
    ]);
    // Name order: forums, primary, promotions, social, updates.
    expect(stripOrder(tied).map((t) => t.tab)).toEqual([
      "forums",
      "primary",
      "promotions",
      "social",
      "updates",
    ]);
    const patch = moveTab(tied, "t-pro", -1);
    expect(afterPatch(tied, patch)).toEqual([
      "forums",
      "promotions",
      "primary",
      "social",
      "updates",
    ]);
  });

  it("moves a disabled tab, so its slot is already right when it is switched on", () => {
    const withOff = byId([
      tab("t-pri", "primary", 0),
      tab("t-soc", "social", 1),
      tab("t-for", "forums", 2, false),
    ]);
    expect(afterPatch(withOff, moveTab(withOff, "t-for", -1))).toEqual([
      "primary",
      "forums",
      "social",
    ]);
  });
});

describe("canDisable", () => {
  it("refuses Primary and allows the rest", () => {
    // Not a preference: app.category_tabs CHECKs it, because mail from a
    // switched-off tab folds INTO Primary.
    expect(canDisable("primary")).toBe(false);
    for (const t of ["social", "promotions", "updates", "forums"] as const) {
      expect(canDisable(t)).toBe(true);
    }
  });
});

describe("labelsPerTab", () => {
  it("counts mapped labels and ignores unmapped ones", () => {
    expect(
      labelsPerTab([
        { tab: "promotions" },
        { tab: "promotions" },
        { tab: "updates" },
        { tab: null },
      ]),
    ).toEqual({ promotions: 2, updates: 1 });
  });

  it("omits a tab nothing feeds rather than reporting it as zero", () => {
    // The caller renders "no labels feed this" from the absence; a 0 would read
    // as a measured emptiness of the TAB, which this cannot know — the
    // classifier still puts unlabelled mail there.
    expect(labelsPerTab([{ tab: "social" }])["forums"]).toBeUndefined();
  });
});
