// SPDX-License-Identifier: AGPL-3.0-only

// The desk's flattened model (#375 increment 3).
//
// What is worth testing here is not that arrays concatenate. It is the handful
// of facts the screen would otherwise get wrong in silence:
//
//   * NEEDS SORTING IS DRAWN, and drawn last. It is the only section whose mail
//     no area claims and no drawer will ever sweep, so a tab that dropped it
//     would hide mail with nowhere else to be shown — the one failure a mail
//     client must never have.
//   * an EMPTY AREA still shows its heading, or the user cannot see that the
//     partition is whole;
//   * the "show more" item appears ONLY when the window is a bound, or the desk
//     offers to fetch mail that is already on screen;
//   * a CLOSED DRAWER contributes exactly one item, which is what lets the tab
//     paint before any drawer's contents exist;
//   * the LINE sits below the sections on one tab and above the drawers on the
//     other, because on one it is a floor and on the other a ceiling;
//   * `j`/`k` walk ROWS, crossing headings, the line and drawer rows — a cursor
//     that can land on a heading is a cursor whose next keystroke does nothing.

import { describe, expect, it } from "vitest";

import { UNSORTED, cursorAfterRemoval, flatten, nextRowId, positionOf, rowIds, rowIndexOf } from "./flatten";
import type { DeskItem, DeskModel, DrawerView, SectionView } from "./flatten";

function section(key: string, areaId: string | null, over: Partial<SectionView> = {}): SectionView {
  return { key, areaId, ids: [], total: 0, count: 0, error: null, ...over };
}

function drawer(areaId: string, over: Partial<DrawerView> = {}): DrawerView {
  return { areaId, key: `d/${areaId}`, open: false, ids: [], total: 0, error: null, ...over };
}

function model(over: Partial<DeskModel> = {}): DeskModel {
  return { tab: "review", sections: [], drawers: [], trayCount: 0, ...over };
}

function kinds(items: DeskItem[]): string[] {
  return items.map((i) => i.kind);
}

describe("flatten: Review first", () => {
  it("draws every section in order, Needs sorting last, and ends with the line", () => {
    const items = flatten(
      model({
        sections: [
          section("s/A", "A", { ids: ["1", "2"], total: 2, count: 2 }),
          section("s/B", "B", { ids: ["3"], total: 1, count: 1 }),
          section("s/none", null, { ids: ["4"], total: 1, count: 1 }),
        ],
      }),
    );
    expect(kinds(items)).toEqual([
      "heading",
      "row",
      "row",
      "heading",
      "row",
      "heading",
      "row",
      "line",
    ]);
    // Needs sorting is the LAST heading, and it is present at all.
    const headings = items.filter((i) => i.kind === "heading");
    expect(headings.map((h) => (h.kind === "heading" ? h.areaId : null))).toEqual(["A", "B", null]);
    expect(rowIds(items)).toEqual(["1", "2", "3", "4"]);
  });

  it("shows an empty area's heading, so the partition is visibly whole", () => {
    const items = flatten(
      model({
        trayCount: 1,
        sections: [
          section("s/A", "A", { ids: [], total: 0, count: 0 }),
          section("s/B", "B", { ids: ["3"], total: 1, count: 1 }),
        ],
      }),
    );
    const headings = items.filter((i) => i.kind === "heading");
    expect(headings).toHaveLength(2);
    expect(headings[0]).toMatchObject({ areaId: "A", count: 0 });
  });

  it("carries the summary count and the section's own total separately", () => {
    // They answer different questions: the heading prints what the AREA holds,
    // while the total is what the section's query said — and "Show N more" is a
    // bound on the second, not the first.
    const [heading] = flatten(
      model({ sections: [section("s/A", "A", { ids: ["1"], total: 218, count: 220 })] }),
    );
    expect(heading).toMatchObject({ kind: "heading", count: 220, total: 218 });
  });

  it("adds a `more` item only when the window is a bound", () => {
    const bounded = flatten(
      model({ sections: [section("s/A", "A", { ids: ["1", "2"], total: 218, count: 218 })] }),
    );
    expect(bounded.filter((i) => i.kind === "more")).toEqual([
      { kind: "more", section: "s/A", remaining: 216 },
    ]);

    const whole = flatten(
      model({ sections: [section("s/A", "A", { ids: ["1", "2"], total: 2, count: 2 })] }),
    );
    expect(whole.some((i) => i.kind === "more")).toBe(false);
  });

  it("puts an errored section's error in place of its rows, and offers no `more`", () => {
    const items = flatten(
      model({
        sections: [section("s/A", "A", { ids: ["1"], total: 9, count: 9, error: "no route to host" })],
      }),
    );
    expect(kinds(items)).toEqual(["heading", "error", "line"]);
    expect(items[1]).toMatchObject({ kind: "error", message: "no route to host" });
  });

  it("collapses to the empty state, keeping the line, when nothing is anywhere", () => {
    const items = flatten(model({ trayCount: 0, sections: [section("s/A", "A"), section("s/none", null)] }));
    expect(kinds(items)).toEqual(["empty", "line"]);
  });

  it("is not empty while the tray holds something", () => {
    const items = flatten(model({ trayCount: 3, sections: [section("s/A", "A")] }));
    expect(kinds(items)).toEqual(["heading", "line"]);
  });

  it("is not empty when a section merely FAILED — unknown is not nothing", () => {
    const items = flatten(model({ sections: [section("s/A", "A", { error: "timeout" })] }));
    // "Nothing to review" over a query that failed is the exact wrong answer:
    // it states a fact about the mailbox that nobody actually established.
    expect(kinds(items)).toEqual(["heading", "error", "line"]);
  });
});

describe("flatten: Everything else", () => {
  it("puts the line above the drawers and gives a closed drawer exactly one item", () => {
    const items = flatten(
      model({
        tab: "rest",
        drawers: [drawer("A", { total: 824 }), drawer("B", { total: 12 })],
      }),
    );
    expect(kinds(items)).toEqual(["line", "drawer", "drawer"]);
  });

  it("draws an open drawer's rows under it, with its own bound", () => {
    const items = flatten(
      model({
        tab: "rest",
        drawers: [
          drawer("A", { open: true, ids: ["1", "2"], total: 824 }),
          drawer("B", { total: 12 }),
        ],
      }),
    );
    expect(kinds(items)).toEqual(["line", "drawer", "row", "row", "more", "drawer"]);
    expect(items[4]).toMatchObject({ kind: "more", remaining: 822 });
  });

  it("shows the empty state when there are no areas at all", () => {
    expect(kinds(flatten(model({ tab: "rest" })))).toEqual(["line", "empty"]);
  });
});

describe("nextRowId", () => {
  const items = flatten(
    model({
      sections: [
        section("s/A", "A", { ids: ["a1", "a2"], total: 2, count: 2 }),
        section("s/B", "B", { ids: ["b1"], total: 1, count: 1 }),
      ],
    }),
  );

  it("crosses a heading rather than landing on one", () => {
    // The cursor must go a2 → b1. Walking the item array itself would stop on
    // B's heading, where every verb the user then presses does nothing.
    expect(nextRowId(items, "a2", 1)).toBe("b1");
    expect(nextRowId(items, "b1", -1)).toBe("a2");
  });

  it("steps within a section", () => {
    expect(nextRowId(items, "a1", 1)).toBe("a2");
    expect(nextRowId(items, "a2", -1)).toBe("a1");
  });

  it("stays put at either end rather than wrapping", () => {
    expect(nextRowId(items, "b1", 1)).toBe("b1");
    expect(nextRowId(items, "a1", -1)).toBe("a1");
  });

  it("starts from the near end when the cursor is not on this tab", () => {
    expect(nextRowId(items, null, 1)).toBe("a1");
    expect(nextRowId(items, null, -1)).toBe("b1");
    expect(nextRowId(items, "gone", 1)).toBe("a1");
  });

  it("walks into an open drawer, across the line and the drawer row", () => {
    const rest = flatten(
      model({
        tab: "rest",
        drawers: [
          drawer("A", { open: true, ids: ["a1", "a2"], total: 2 }),
          drawer("B", { open: true, ids: ["b1"], total: 1 }),
        ],
      }),
    );
    expect(nextRowId(rest, null, 1)).toBe("a1");
    expect(nextRowId(rest, "a2", 1)).toBe("b1");
  });

  it("returns nothing at all when the tab holds no rows", () => {
    expect(nextRowId(flatten(model()), null, 1)).toBeNull();
  });
});

describe("flatten: collapsed sections (#375 increment 7)", () => {
  it("gives a collapsed section its heading and nothing else", () => {
    const items = flatten(
      model({
        sections: [
          section("s/A", "A", { ids: ["a1", "a2"], total: 40, count: 40 }),
          section("s/B", "B", { ids: ["b1"], total: 1, count: 1 }),
        ],
        collapsed: new Set(["A"]),
      }),
    );
    // A's two rows AND its "show more" are gone; B is untouched.
    expect(kinds(items)).toEqual(["heading", "heading", "row", "line"]);
    expect(rowIds(items)).toEqual(["b1"]);
  });

  it("still prints the summary count on the folded heading", () => {
    // The number is the whole reason a fold is safe: "Bills · 40", folded, is
    // not the same screen as an area that has quietly vanished.
    const [heading] = flatten(
      model({ sections: [section("s/A", "A", { ids: ["a1"], total: 40, count: 40 })], collapsed: new Set(["A"]) }),
    );
    expect(heading).toMatchObject({ kind: "heading", areaId: "A", count: 40, collapsed: true });
  });

  it("marks an expanded section's heading as not collapsed", () => {
    const [heading] = flatten(model({ sections: [section("s/A", "A", { ids: ["a1"], total: 1, count: 1 })] }));
    expect(heading).toMatchObject({ kind: "heading", collapsed: false });
  });

  it("collapses Needs sorting, which has no area id to be named by", () => {
    // `areaId: null` is what "no area claims this mail" means, and null is not
    // a set key — hence the sentinel. Getting this wrong would leave the one
    // section that asks for work as the one section that cannot be folded.
    const items = flatten(
      model({
        sections: [
          section("s/A", "A", { ids: ["a1"], total: 1, count: 1 }),
          section("s/none", null, { ids: ["u1", "u2"], total: 2, count: 2 }),
        ],
        collapsed: new Set([UNSORTED]),
      }),
    );
    expect(kinds(items)).toEqual(["heading", "row", "heading", "line"]);
    expect(rowIds(items)).toEqual(["a1"]);
  });

  it("does not fold a section merely because another one is folded", () => {
    const items = flatten(
      model({
        sections: [section("s/A", "A", { ids: ["a1"], total: 1, count: 1 })],
        collapsed: new Set(["B"]),
      }),
    );
    expect(rowIds(items)).toEqual(["a1"]);
  });

  it("keeps the headings when everything is folded, rather than the empty state", () => {
    // "Nothing to review" over sections the USER folded would be the desk
    // reporting a fact about the mailbox that is a fact about a preference.
    const items = flatten(
      model({
        sections: [
          section("s/A", "A", { ids: ["a1"], total: 1, count: 1 }),
          section("s/none", null, { ids: ["u1"], total: 1, count: 1 }),
        ],
        collapsed: new Set(["A", UNSORTED]),
      }),
    );
    expect(kinds(items)).toEqual(["heading", "heading", "line"]);
  });

  it("takes the folded rows out of the cursor's walk", () => {
    // The cursor indexes the ITEM list, so a row that is not emitted must not
    // be reachable — otherwise `j` lands on a message that is not on screen.
    const items = flatten(
      model({
        sections: [
          section("s/A", "A", { ids: ["a1", "a2"], total: 2, count: 2 }),
          section("s/B", "B", { ids: ["b1"], total: 1, count: 1 }),
        ],
        collapsed: new Set(["A"]),
      }),
    );
    expect(nextRowId(items, null, 1)).toBe("b1");
    expect(rowIndexOf(items, "a1")).toBe(-1);
  });
});

describe("positionOf (#375 increment 7)", () => {
  const items = flatten(
    model({
      sections: [
        section("s/A", "A", { ids: ["a1", "a2"], total: 2, count: 2 }),
        section("s/B", "B", { ids: ["b1"], total: 1, count: 1 }),
      ],
    }),
  );

  it("counts from one, because it is a sentence a person reads", () => {
    // "0 of 3" is either an off-by-one or a claim the message is not in the
    // list; neither is what the reader of the FIRST message should be told.
    expect(positionOf(items, "a1")).toEqual({ index: 1, count: 3 });
    expect(positionOf(items, "a2")).toEqual({ index: 2, count: 3 });
    expect(positionOf(items, "b1")).toEqual({ index: 3, count: 3 });
  });

  it("counts ROWS, not items — headings and the line are not messages", () => {
    // The item list here is heading,row,row,heading,row,line = 6 items and 3
    // messages. A denominator of 6 would promise three messages that are not
    // reachable by swiping.
    expect(items).toHaveLength(6);
    expect(positionOf(items, "b1")?.count).toBe(3);
  });

  it("answers null for a message this list does not hold", () => {
    expect(positionOf(items, "gone")).toBeNull();
    expect(positionOf(items, null)).toBeNull();
    expect(positionOf(flatten(model()), "a1")).toBeNull();
  });

  it("counts only the rows a collapsed section left on screen", () => {
    const folded = flatten(
      model({
        sections: [
          section("s/A", "A", { ids: ["a1", "a2"], total: 2, count: 2 }),
          section("s/B", "B", { ids: ["b1"], total: 1, count: 1 }),
        ],
        collapsed: new Set(["A"]),
      }),
    );
    expect(positionOf(folded, "b1")).toEqual({ index: 1, count: 1 });
    expect(positionOf(folded, "a1")).toBeNull();
  });
});

describe("rowIndexOf", () => {
  it("is the ITEM index, which is what the virtual window is measured in", () => {
    const items = flatten(
      model({ sections: [section("s/A", "A", { ids: ["a1", "a2"], total: 2, count: 2 })] }),
    );
    expect(rowIndexOf(items, "a1")).toBe(1); // after the heading
    expect(rowIndexOf(items, "a2")).toBe(2);
    expect(rowIndexOf(items, null)).toBe(-1);
    expect(rowIndexOf(items, "nope")).toBe(-1);
  });
});

// #685: the cursor's row left (archived, trashed, snoozed) — the cursor takes
// the row that took its place, which is where the reading pane's advance goes
// too. Moving it to the TOP was what scrolled the desk twice on every archive.
describe("cursorAfterRemoval", () => {
  const before = ["a", "b", "c", "d", "e"];

  it("leaves a cursor whose row is still there alone", () => {
    expect(cursorAfterRemoval(before, before, "c")).toBe("c");
  });

  it("takes the row after the one that left, not the first row", () => {
    expect(cursorAfterRemoval(before, ["a", "b", "d", "e"], "c")).toBe("d");
  });

  it("skips rows that left with it", () => {
    expect(cursorAfterRemoval(before, ["a", "b", "e"], "c")).toBe("e");
  });

  it("falls back to the nearest row above when nothing below survived", () => {
    expect(cursorAfterRemoval(before, ["a", "b"], "e")).toBe("b");
    expect(cursorAfterRemoval(before, ["a"], "c")).toBe("a");
  });

  it("starts at the top when the cursor was never on this list, or nothing is", () => {
    expect(cursorAfterRemoval(before, ["x", "y"], "zz")).toBe("x");
    expect(cursorAfterRemoval(before, ["x", "y"], null)).toBe("x");
    expect(cursorAfterRemoval(before, [], "c")).toBeNull();
  });

  it("finds a neighbour that a reload has moved", () => {
    // The surviving neighbour is the first FORMER successor still present,
    // wherever the new list put it.
    expect(cursorAfterRemoval(before, ["d", "a", "b"], "c")).toBe("d");
  });
});
