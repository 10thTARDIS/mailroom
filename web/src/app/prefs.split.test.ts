// SPDX-License-Identifier: AGPL-3.0-only

// The px ↔ percent seam between this device's `--pane-split` and the synced
// `ClientPreferences.paneSplitPct` (#375 increment 5).
//
// It is the arithmetic in the middle of a feature whose failure is silent: a
// wrong conversion does not throw, it just puts the divider somewhere nobody
// asked for on whichever device syncs second.

import { describe, expect, it } from "vitest";

import {
  SPLIT_PCT_BOUNDS,
  clampSplitPct,
  densityFromServer,
  densityToServer,
  pctToPx,
  pxToPct,
  splitBounds,
} from "./prefs";
import type { Density } from "./prefs";

describe("pctToPx / pxToPct", () => {
  it("round-trips a percent that lands inside the px bounds", () => {
    // 40% of 1000px = 400px, which is between vertical's 280 and 720.
    const px = pctToPx(40, 1000, "vertical");
    expect(px).toBe(400);
    expect(pxToPct(px as number, 1000)).toBe(40);
  });

  it("round-trips every percent whose pixels land inside the bounds", () => {
    // 30%..55% of 1200px is 360..660, comfortably between vertical's 280 and
    // 720. Past that the px clamp bites and the round trip is deliberately not
    // identity — the test below asserts exactly that.
    for (const pct of [30, 40, 50, 55]) {
      const px = pctToPx(pct, 1200, "vertical");
      expect(px).not.toBeNull();
      expect(pxToPct(px as number, 1200)).toBe(pct);
    }
  });

  it("clamps to the LAYOUT's px bounds, so a round trip is not identity there", () => {
    // 20% of 1000 is 200px — below vertical's 280px minimum, which exists so a
    // drag cannot collapse the list to a sliver. The clamp is the honest
    // answer, and it is visible in the percent that comes back.
    const { min } = splitBounds("vertical");
    expect(pctToPx(20, 1000, "vertical")).toBe(min);
    expect(pxToPct(min, 1000)).toBe(28);
  });

  it("uses the horizontal bounds for the horizontal layout", () => {
    // 20% of 1000 is 200px, which IS legal horizontally (min 160) and is not
    // vertically (min 280) — the same input, two answers, which is why the
    // layout is a parameter.
    expect(pctToPx(20, 1000, "horizontal")).toBe(200);
    expect(pctToPx(20, 1000, "vertical")).toBe(280);
  });

  it("clamps the PERCENT to what the server will store", () => {
    // The server's CHECK is 20..80 (migration 00046); a device that sent 12
    // would spend a round trip being refused.
    expect(pxToPct(100, 1000)).toBe(SPLIT_PCT_BOUNDS.min);
    expect(pxToPct(950, 1000)).toBe(SPLIT_PCT_BOUNDS.max);
    expect(clampSplitPct(0)).toBe(20);
    expect(clampSplitPct(1000)).toBe(80);
    expect(clampSplitPct(43.4)).toBe(43);
  });

  it("answers null for a container it cannot measure, rather than guessing", () => {
    // Null is not zero and not a default: a percent computed against an
    // unknown container would be written to the server as if someone chose it.
    expect(pctToPx(40, 0, "vertical")).toBeNull();
    expect(pctToPx(40, -1, "vertical")).toBeNull();
    expect(pctToPx(Number.NaN, 1000, "vertical")).toBeNull();
    expect(pxToPct(400, 0)).toBeNull();
    expect(pxToPct(Number.NaN, 1000)).toBeNull();
  });

  it("returns whole pixels — a fractional grid track is not a size", () => {
    expect(pctToPx(37, 1013, "vertical")).toBe(Math.round((1013 * 37) / 100));
    expect(Number.isInteger(pctToPx(37, 1013, "vertical") as number)).toBe(true);
  });
});

describe("density vocabularies", () => {
  it("maps the server's three words onto this app's three", () => {
    expect(densityFromServer("relaxed")).toBe("comfortable");
    expect(densityFromServer("normal")).toBe("cozy");
    expect(densityFromServer("compact")).toBe("compact");
  });

  it("maps back, and round-trips every value", () => {
    for (const d of ["comfortable", "cozy", "compact"] as Density[]) {
      expect(densityFromServer(densityToServer(d))).toBe(d);
    }
    expect(densityToServer("comfortable")).toBe("relaxed");
    expect(densityToServer("cozy")).toBe("normal");
    expect(densityToServer("compact")).toBe("compact");
  });

  it("falls back to the middle step for a word neither side knows", () => {
    // A server that grows a fourth density must not leave this device with no
    // density at all; the middle one is the default `loadDensity` already uses.
    expect(densityFromServer("luxurious")).toBe("cozy");
  });
});
