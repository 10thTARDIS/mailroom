// SPDX-License-Identifier: AGPL-3.0-only

// The shared pop-up rule's arithmetic (#880). No DOM here (this repo's web tests
// have no renderer); the real geometry is asserted in a browser by the PHONEMENUS
// step of scripts/a11y-check.cjs.

import { describe, expect, it } from "vitest";

import { computeFit } from "./popoverFit";
import type { Box } from "./popoverFit";

// A 375x667 phone, 8px margin.
const view: Box = { top: 8, bottom: 659, left: 8, right: 367 };
const at = (top: number, left: number, h: number, w: number): Box => ({ top, bottom: top + h, left, right: left + w });

describe("computeFit", () => {
  it("leaves a menu that fits exactly where the stylesheet put it", () => {
    const anchor = at(100, 20, 36, 80);
    const pop = at(140, 20, 200, 220);
    expect(computeFit(pop, anchor, view, true)).toEqual({ flip: false, maxHeight: null, maxWidth: null, dx: 0 });
  });

  it("flips a menu hung below a bottom-docked toolbar (the Re-file menu of #880)", () => {
    const anchor = at(560, 20, 36, 90); // the toolbar's button, near the bottom
    const pop = at(600, 20, 220, 200); // hangs below it: bottom 820, off a 667 screen
    const fit = computeFit(pop, anchor, view, true);
    expect(fit.flip).toBe(true);
    expect(fit.maxHeight).toBeNull(); // 220 fits in the 548px above
  });

  it("caps a menu taller than either side and lets it scroll inside itself", () => {
    const anchor = at(560, 20, 36, 90);
    const pop = at(600, 20, 900, 200); // 30 areas
    const fit = computeFit(pop, anchor, view, true);
    expect(fit.flip).toBe(true);
    expect(fit.maxHeight).toBe(560 - 4 - 8); // the room above, to the margin
  });

  it("stays below when below is the bigger side, capped", () => {
    const anchor = at(20, 20, 36, 90);
    const pop = at(60, 20, 900, 200);
    const fit = computeFit(pop, anchor, view, true);
    expect(fit.flip).toBe(false);
    expect(fit.maxHeight).toBe(659 - (56 + 4));
  });

  it("flips a menu the stylesheet hung above when it runs off the top", () => {
    const anchor = at(60, 20, 36, 90);
    const pop = at(-150, 20, 200, 200); // above, off the top
    const fit = computeFit(pop, anchor, view, false);
    expect(fit.flip).toBe(true);
  });

  it("shifts a menu left when it runs off the right edge, and right when off the left", () => {
    expect(computeFit(at(140, 300, 100, 220), at(100, 300, 36, 60), view, true).dx).toBe(367 - 520);
    expect(computeFit(at(140, -40, 100, 220), at(100, 0, 36, 60), view, true).dx).toBe(48);
  });

  it("narrows a menu wider than the screen and keeps its left edge on screen", () => {
    const fit = computeFit(at(140, 100, 100, 600), at(100, 100, 36, 60), view, true);
    expect(fit.maxWidth).toBe(359);
    expect(fit.dx).toBe(8 - 100);
  });
});
