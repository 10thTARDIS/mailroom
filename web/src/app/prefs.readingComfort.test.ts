// SPDX-License-Identifier: AGPL-3.0-only

// THE FIVE TEXT SIZES AND THE TWO ROW LAYOUTS (#514), as pure functions.
//
// Three things are worth a test here and none of them is "does the setter set
// an attribute":
//
//   1. THE STEP ↔ WORD MAPPING, because it is announced. The slider says
//      `aria-valuenow=4` and `aria-valuetext="Large"`, and those two have to be
//      the same fact — an off-by-one is inaudible to everyone except the person
//      relying on the number, which is the worst possible distribution for a
//      defect.
//   2. THE VOCABULARY GUARDS, because the value arrives from a blind cast.
//      core/sync.ts casts `res.list` straight to `ClientPreferences[]`, so a
//      server that predates migration 00052 sends no `textSize` at all; without
//      the guard `<html data-text-size="undefined">` matches no rule in
//      tokens.css and the app silently stays at whatever size it was.
//   3. THAT prefs.ts AND tokens.css AGREE ABOUT THE FIVE PIXEL SIZES. They are
//      the same five numbers written in two languages, which is exactly the
//      shape that drifts, so the test READS THE STYLESHEET rather than trusting
//      the comment that says they match.

import { describe, expect, it } from "vitest";

// The real stylesheet, as text — see web/src/css.d.ts for why `?raw` rather
// than `node:fs` (this package has no @types/node, and going through Vite's
// own transform is what the app does anyway).
import tokensCss from "./tokens.css?raw";

import {
  DEFAULT_ROW_LAYOUT,
  DEFAULT_TEXT_SIZE,
  TEXT_SIZES,
  TEXT_SIZE_PX,
  rowLayoutFromServer,
  stepFromTextSize,
  textSizeFromServer,
  textSizeFromStep,
} from "./prefs";
import type { TextSize } from "./prefs";

describe("the five steps", () => {
  it("is smallest first, with the default in the middle", () => {
    // The ORDER is part of the vocabulary: the server advertises it
    // (`textSizes`), the slider walks it, and `stepFromTextSize` indexes it.
    expect(TEXT_SIZES).toEqual(["xs", "s", "m", "l", "xl"]);
    expect(DEFAULT_TEXT_SIZE).toBe("m");
    expect(stepFromTextSize(DEFAULT_TEXT_SIZE)).toBe(3);
  });

  it("numbers the steps from 1, because that is what aria-valuemin says", () => {
    expect(TEXT_SIZES.map(stepFromTextSize)).toEqual([1, 2, 3, 4, 5]);
  });

  it("round-trips every step through both directions", () => {
    for (const size of TEXT_SIZES) {
      expect(textSizeFromStep(stepFromTextSize(size))).toBe(size);
    }
  });

  it("clamps at both ends rather than wrapping", () => {
    // A wrap would take the largest text to the smallest on one arrow press —
    // the one keystroke that must not be surprising on a control someone is
    // using BECAUSE they cannot read the screen comfortably.
    expect(textSizeFromStep(0)).toBe("xs");
    expect(textSizeFromStep(-4)).toBe("xs");
    expect(textSizeFromStep(6)).toBe("xl");
    expect(textSizeFromStep(99)).toBe("xl");
  });

  it("falls back rather than throwing on a number that is not one", () => {
    // NaN and the infinities are NOT clamped to an end, they fall back to the
    // default: an infinity is not "very large", it is a value that is not a
    // position on this control at all, and there is only one safe answer to
    // that. The clamp above is for real positions that are off the end.
    expect(textSizeFromStep(Number.NaN)).toBe(DEFAULT_TEXT_SIZE);
    expect(textSizeFromStep(Number.POSITIVE_INFINITY)).toBe(DEFAULT_TEXT_SIZE);
    expect(textSizeFromStep(Number.NEGATIVE_INFINITY)).toBe(DEFAULT_TEXT_SIZE);
    // Fractions come from nowhere legitimate, but rounding is the honest answer
    // for a control whose whole range is five stops.
    expect(textSizeFromStep(3.4)).toBe("m");
    expect(textSizeFromStep(3.6)).toBe("l");
  });
});

describe("the vocabulary guards", () => {
  it("passes every word the server can actually store", () => {
    for (const size of TEXT_SIZES) expect(textSizeFromServer(size)).toBe(size);
    expect(rowLayoutFromServer("oneLine")).toBe("oneLine");
    expect(rowLayoutFromServer("multiLine")).toBe("multiLine");
  });

  it("falls back to the default for anything else", () => {
    // "huge" is the value the server's own integration test refuses; the other
    // three are the shapes a real client actually meets — a case difference, an
    // empty string from localStorage, and the missing field an older server
    // sends (which arrives here as `undefined` through a blind cast).
    for (const bad of ["huge", "M", "", "medium"]) {
      expect(textSizeFromServer(bad)).toBe(DEFAULT_TEXT_SIZE);
    }
    expect(textSizeFromServer(undefined as unknown as string)).toBe(DEFAULT_TEXT_SIZE);
    for (const bad of ["twoLine", "oneline", ""]) {
      expect(rowLayoutFromServer(bad)).toBe(DEFAULT_ROW_LAYOUT);
    }
    expect(rowLayoutFromServer(undefined as unknown as string)).toBe(DEFAULT_ROW_LAYOUT);
    expect(DEFAULT_ROW_LAYOUT).toBe("oneLine");
  });
});

describe("prefs.ts and tokens.css agree about the five sizes", () => {
  it("declares the same pixel size for every step, and 14px for the default", () => {
    // ⚠ THE STYLESHEET IS THE ONE THAT RUNS. TEXT_SIZE_PX exists so this test
    // and a reader have the numbers in TypeScript; if the two ever disagree,
    // the app is the CSS and the constant is a lie. So this parses the real
    // file rather than asserting the constant against itself.
    const declared = new Map<string, number>();
    for (const m of tokensCss.matchAll(
      /html\[data-text-size="(xs|s|m|l|xl)"\]\s*\{\s*font-size:\s*(\d+)px/g,
    )) {
      declared.set(m[1] as string, Number(m[2]));
    }
    expect([...declared.keys()].sort()).toEqual([...TEXT_SIZES].sort());
    for (const size of TEXT_SIZES) {
      expect(declared.get(size)).toBe(TEXT_SIZE_PX[size as TextSize]);
    }
    // 14px is load-bearing twice over: it is the size this app has always drawn
    // at (so the default changes nothing for anybody), and it is the base
    // web/scripts/px-to-rem.mjs divided all 164 converted lengths by (so the
    // whole sweep is an identity at the default step).
    expect(TEXT_SIZE_PX[DEFAULT_TEXT_SIZE]).toBe(14);
  });

  it("rises monotonically, so a step is never a step backwards", () => {
    const px = TEXT_SIZES.map((s) => TEXT_SIZE_PX[s as TextSize]);
    for (let i = 1; i < px.length; i++) expect(px[i]!).toBeGreaterThan(px[i - 1]!);
  });
});
