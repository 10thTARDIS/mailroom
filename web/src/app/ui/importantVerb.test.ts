// SPDX-License-Identifier: AGPL-3.0-only

// The direction a bulk importance verb takes (#921). Written around the bug
// that motivated it: the MIXED selection is the case — it is the one the old
// first-row toggle got wrong — and the all-important selection is the only
// one that may answer "not important".

import { describe, expect, it } from "vitest";

import { importantChange, importantTarget } from "./importantVerb";

describe("importantTarget (#921)", () => {
  it("answers mark-important for an empty selection", () => {
    expect(importantTarget([])).toBe(true);
  });

  it("answers mark-important when none of the selection is important", () => {
    expect(importantTarget([false, false])).toBe(true);
  });

  // THE BUG #921 FIXED: the first-row toggle read the first flag, found it
  // already important, and un-marked the rest of the selection. A mixed
  // selection must always be marked important.
  it("answers mark-important for a mixed selection", () => {
    expect(importantTarget([true, false])).toBe(true);
  });

  it("answers not-important only when every message already is", () => {
    expect(importantTarget([true, true])).toBe(false);
  });
});

describe("importantChange (#921)", () => {
  it("sends only the rows that change, and so Undo flips only those", () => {
    const r = importantChange(["a", "b", "c"], [true, false, false]);
    expect(r.important).toBe(true);
    expect(r.changed).toEqual(["b", "c"]);
  });
  it("un-marks every row when all are important", () => {
    const r = importantChange(["a", "b"], [true, true]);
    expect(r.important).toBe(false);
    expect(r.changed).toEqual(["a", "b"]);
  });
  it("a single row toggles, as the pane's menu always did", () => {
    expect(importantChange(["a"], [false])).toEqual({ important: true, changed: ["a"] });
    expect(importantChange(["a"], [true])).toEqual({ important: false, changed: ["a"] });
  });
});
