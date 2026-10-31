// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { POPOVER_GAP, placePopover } from "./popover";

const VIEW = { width: 1200, height: 800 };
const SIZE = { width: 320, height: 200 };

describe("placePopover", () => {
  it("sits below the anchor, left edges aligned", () => {
    const got = placePopover({ top: 100, left: 400, width: 120, height: 20 }, SIZE, VIEW);
    expect(got).toEqual({ top: 100 + 20 + POPOVER_GAP, left: 400, side: "below" });
  });

  it("flips above when there is no room below and there is room above", () => {
    // The anchor is near the bottom: 700 + 20 + 6 + 200 = 926 > 800 - 6.
    const got = placePopover({ top: 700, left: 100, width: 120, height: 20 }, SIZE, VIEW);
    expect(got.side).toBe("above");
    expect(got.top).toBe(700 - POPOVER_GAP - 200);
  });

  it("stays below when it fits below, even though it would also fit above", () => {
    // NEGATIVE CONTROL for the flip: "below" is the default, not merely the
    // branch that happens to be taken when nothing else fits.
    const got = placePopover({ top: 300, left: 100, width: 120, height: 20 }, SIZE, VIEW);
    expect(got.side).toBe("below");
  });

  it("clamps to the right edge rather than hanging off it", () => {
    const got = placePopover({ top: 100, left: 1150, width: 40, height: 20 }, SIZE, VIEW);
    expect(got.left).toBe(1200 - POPOVER_GAP - 320);
    expect(got.left + SIZE.width).toBeLessThanOrEqual(VIEW.width);
  });

  it("clamps to the left edge for a negative anchor (a scrolled-out name)", () => {
    const got = placePopover({ top: 100, left: -80, width: 40, height: 20 }, SIZE, VIEW);
    expect(got.left).toBe(POPOVER_GAP);
  });

  it("pins to the top when the popover is taller than the window", () => {
    // Neither side fits. Showing the FIRST lines is the useful failure: the
    // name and address are at the top of the content.
    const tall = { width: 320, height: 900 };
    const got = placePopover({ top: 400, left: 100, width: 120, height: 20 }, tall, VIEW);
    expect(got.top).toBe(POPOVER_GAP);
    expect(got.side).toBe("below");
  });

  it("keeps the popover inside a tiny viewport in both axes", () => {
    const got = placePopover(
      { top: 40, left: 40, width: 60, height: 20 },
      { width: 320, height: 200 },
      { width: 300, height: 180 },
    );
    expect(got.left).toBe(POPOVER_GAP);
    expect(got.top).toBe(POPOVER_GAP);
  });

  it("takes a custom gap", () => {
    const got = placePopover({ top: 100, left: 400, width: 120, height: 20 }, SIZE, VIEW, 20);
    expect(got.top).toBe(140);
  });
});
