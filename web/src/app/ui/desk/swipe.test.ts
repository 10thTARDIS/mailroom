// SPDX-License-Identifier: AGPL-3.0-only

// The swipe decision (#375 increment 7).
//
// This is the one gesture in the app that can destroy something without a
// click, so the tests below are written around the ways it could be WRONG
// rather than around the way it is right:
//
//   * a scroll must never archive — the vertical veto, at the boundary;
//   * a short drag must never archive — both thresholds, at the boundary;
//   * a row nobody could measure must not make every tap a threshold pass;
//   * left and right must not be interchangeable.

import { describe, expect, it } from "vitest";

import {
  SWIPE_FRACTION,
  SWIPE_MAX_DRIFT_PX,
  SWIPE_MIN_PX,
  swipeDecision,
} from "./swipe";

/** A row narrow enough that 35% of it is the binding threshold (0.35 × 200 =
 * 70px, below the 96px floor) — which is what makes the proportional half of
 * the rule observable at all. */
const NARROW = 200;
/** ...and one wide enough that the absolute floor binds instead (0.35 × 900 =
 * 315px, well above 96px). */
const WIDE = 900;

describe("swipeDecision: direction", () => {
  it("reads a right-hand swipe as archive and a left-hand one as flag", () => {
    expect(swipeDecision(120, 0, WIDE)).toBe("archive");
    expect(swipeDecision(-120, 0, WIDE)).toBe("flag");
  });

  it("answers null for a tap", () => {
    expect(swipeDecision(0, 0, WIDE)).toBeNull();
  });
});

describe("swipeDecision: the vertical veto", () => {
  it("refuses a long horizontal drag that also drifted 24px vertically", () => {
    // The whole point: this is a SCROLL that wandered sideways. 400px of
    // horizontal travel is not enough to make it a swipe.
    expect(swipeDecision(400, SWIPE_MAX_DRIFT_PX, WIDE)).toBeNull();
    expect(swipeDecision(400, -SWIPE_MAX_DRIFT_PX, WIDE)).toBeNull();
  });

  it("allows the same drag one pixel inside the drift limit", () => {
    expect(swipeDecision(400, SWIPE_MAX_DRIFT_PX - 1, WIDE)).toBe("archive");
    expect(swipeDecision(-400, -(SWIPE_MAX_DRIFT_PX - 1), WIDE)).toBe("flag");
  });

  it("vetoes even a gesture that is otherwise perfect, at any width", () => {
    expect(swipeDecision(SWIPE_MIN_PX, 40, WIDE)).toBeNull();
    expect(swipeDecision(NARROW, 40, NARROW)).toBeNull();
  });
});

describe("swipeDecision: the travel thresholds", () => {
  it("commits at 35% of a narrow row, below the absolute floor", () => {
    const at = NARROW * SWIPE_FRACTION; // 70px — under the 96px floor
    expect(at).toBeLessThan(SWIPE_MIN_PX);
    expect(swipeDecision(at, 0, NARROW)).toBe("archive");
    expect(swipeDecision(at - 1, 0, NARROW)).toBeNull();
  });

  it("commits at the absolute floor on a wide row, well below 35% of it", () => {
    const proportional = WIDE * SWIPE_FRACTION; // 315px
    expect(SWIPE_MIN_PX).toBeLessThan(proportional);
    expect(swipeDecision(SWIPE_MIN_PX, 0, WIDE)).toBe("archive");
    expect(swipeDecision(SWIPE_MIN_PX - 1, 0, WIDE)).toBeNull();
  });

  it("refuses a nudge that is neither proportional nor long", () => {
    expect(swipeDecision(30, 0, NARROW)).toBeNull();
    expect(swipeDecision(-30, 0, WIDE)).toBeNull();
  });
});

describe("swipeDecision: measurements it cannot trust", () => {
  it("falls back to the absolute floor for a row of unknown width", () => {
    // A rowWidth of 0 would make the proportional threshold 0, and then every
    // tap is a swipe past it. The floor is what stops that.
    expect(swipeDecision(4, 0, 0)).toBeNull();
    expect(swipeDecision(SWIPE_MIN_PX, 0, 0)).toBe("archive");
    expect(swipeDecision(4, 0, -50)).toBeNull();
    expect(swipeDecision(4, 0, Number.NaN)).toBeNull();
  });

  it("answers null rather than guessing for a drag with no numbers", () => {
    expect(swipeDecision(Number.NaN, 0, WIDE)).toBeNull();
    expect(swipeDecision(200, Number.NaN, WIDE)).toBeNull();
  });
});
