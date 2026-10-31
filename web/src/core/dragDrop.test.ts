// SPDX-License-Identifier: AGPL-3.0-only

// Tests for drag-and-drop rules (#62).
//
// Two of these matter more than the rest:
//
//   * `dragSet` — dragging a row that is part of a selection must carry the WHOLE
//     selection. Acting on one row while five are highlighted is how a drag loses mail.
//   * `decodeDrag` — the payload arrives through the platform's clipboard machinery, so it
//     can be absent, truncated, or written by another application. It must never throw and
//     never guess.

import { describe, expect, it } from "vitest";

import { canDrop, decodeDrag, dragSet, encodeDrag, inboxDropPlan } from "./dragDrop";
import type { DropContext } from "./dragDrop";

describe("dragSet", () => {
  it("carries the whole selection when the dragged row is in it", () => {
    expect(dragSet("b", ["a", "b", "c"]).sort()).toEqual(["a", "b", "c"]);
  });

  it("carries only the dragged row when it is NOT selected", () => {
    // The selection is deliberately left alone: dragging an unselected row is a
    // statement about that row, not about what was highlighted.
    expect(dragSet("z", ["a", "b"])).toEqual(["z"]);
  });

  it("carries the row when nothing is selected", () => {
    expect(dragSet("a", [])).toEqual(["a"]);
  });
});

describe("encodeDrag / decodeDrag", () => {
  it("round-trips", () => {
    expect(decodeDrag(encodeDrag(["a", "b"]))).toEqual(["a", "b"]);
  });

  it("returns [] for anything it does not recognise, and never throws", () => {
    for (const raw of [
      null,
      undefined,
      "",
      "not json",
      "[]",
      "null",
      "42",
      '"a string"',
      "{}",
      '{"ids":"a"}',
      '{"ids":null}',
      '{"nope":["a"]}',
      '{"ids":{"0":"a"}}',
      "{truncated",
    ]) {
      expect(() => decodeDrag(raw)).not.toThrow();
      expect(decodeDrag(raw)).toEqual([]);
    }
  });

  it("drops non-string and empty entries rather than passing them on as ids", () => {
    // One non-string here would otherwise reach an Email/set as an id.
    expect(decodeDrag('{"ids":["a",1,null,"",{"x":1},"b"]}')).toEqual(["a", "b"]);
  });
});

describe("canDrop", () => {
  const ctx = (over: Partial<DropContext> = {}): DropContext => ({
    ids: ["m1"],
    currentRole: "inbox",
    labelsOn: { m1: ["work"] },
    ...over,
  });

  it("refuses an empty drag", () => {
    expect(canDrop({ kind: "role", role: "archive" }, ctx({ ids: [] }))).toBe(false);
  });

  // THE RULE WITH TEETH: a drop that would change nothing is refused, so the cursor says
  // so before the user commits. A drop that lands and does nothing reads as a bug.
  it("refuses the folder the mail is already in", () => {
    expect(canDrop({ kind: "role", role: "inbox" }, ctx({ currentRole: "inbox" }))).toBe(false);
  });

  it("allows a different folder", () => {
    expect(canDrop({ kind: "role", role: "archive" }, ctx({ currentRole: "inbox" }))).toBe(true);
  });

  it("allows a folder move when the current view is not a folder at all", () => {
    expect(canDrop({ kind: "role", role: "archive" }, ctx({ currentRole: null }))).toBe(true);
  });

  it("refuses a label every dragged message already has", () => {
    const target = { kind: "label" as const, labelId: "work", keyword: "$work" };
    expect(canDrop(target, ctx({ ids: ["m1", "m2"], labelsOn: { m1: ["work"], m2: ["work"] } }))).toBe(
      false,
    );
  });

  it("allows a label when even one dragged message lacks it", () => {
    // Partial progress is still progress, and it matches what the label menu does.
    const target = { kind: "label" as const, labelId: "work", keyword: "$work" };
    expect(canDrop(target, ctx({ ids: ["m1", "m2"], labelsOn: { m1: ["work"], m2: [] } }))).toBe(true);
  });

  it("allows a label nothing has, including for a message with no labels recorded", () => {
    const target = { kind: "label" as const, labelId: "urgent", keyword: "$urgent" };
    expect(canDrop(target, ctx())).toBe(true);
    expect(canDrop(target, ctx({ labelsOn: {} }))).toBe(true);
  });
});

// A drop on the Inbox used to be `untrash` for EVERY message, and for anything not in
// Trash the server reads that as a Restore with no destination — which lands in Archive.
// Dragging a false positive out of Junk onto the Inbox therefore archived it (#528).
describe("inboxDropPlan", () => {
  const never = (): boolean => false;

  it("from JUNK is 'Not junk', never untrash — untrash would file the message into Archive", () => {
    expect(inboxDropPlan(["a", "b"], "junk", never)).toEqual({
      untrash: [],
      notJunk: ["a", "b"],
      restoreToInbox: [],
    });
  });

  it("from Trash stays the existing restore", () => {
    expect(inboxDropPlan(["a"], "trash", () => true)).toEqual({
      untrash: ["a"],
      notJunk: [],
      restoreToInbox: [],
    });
  });

  it("from a folder, a label or Archive is a plain move back to the Inbox", () => {
    for (const role of ["archive", null]) {
      expect(inboxDropPlan(["a"], role, never)).toEqual({
        untrash: [],
        notJunk: [],
        restoreToInbox: ["a"],
      });
    }
  });

  it("decides PER MESSAGE outside a role view, so junk dragged out of a search is rescued", () => {
    const junk = new Set(["j"]);
    expect(inboxDropPlan(["a", "j", "b"], null, (id) => junk.has(id))).toEqual({
      untrash: [],
      notJunk: ["j"],
      restoreToInbox: ["a", "b"],
    });
  });

  it("puts every id in exactly one bucket", () => {
    const plan = inboxDropPlan(["a", "b", "c"], null, (id) => id === "b");
    const all = [...plan.untrash, ...plan.notJunk, ...plan.restoreToInbox].sort();
    expect(all).toEqual(["a", "b", "c"]);
  });
});
