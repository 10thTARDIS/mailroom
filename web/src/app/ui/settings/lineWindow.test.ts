// SPDX-License-Identifier: AGPL-3.0-only

// The assertion that carries the feature is the SIGN of `stricterDelta`. Both
// directions produce a number, both render as a fluent sentence, and only one of
// them is true — "one step stricter would move 12 to drawers" is exactly as
// readable when the 12 is the mail that would come OUT of the drawers. A test that
// only checked `typeof got === "number"` would pass on the reversed subtraction.

import { describe, expect, it } from "vitest";

import {
  LINE_DEFAULT_THRESHOLD,
  LINE_WINDOW_DAYS,
  sevenDayWindow,
  stricterDelta,
} from "./lineWindow";
import type { AreaSummaryRow } from "../../../core/jmap/types";

function row(over: Partial<AreaSummaryRow> = {}): AreaSummaryRow {
  return {
    areaId: "01JBQZ3K7WX2N4Y6P8R0S1T2A0",
    surface: 0,
    drawer: 0,
    unscored: 0,
    archivedByDrawer: 0,
    ...over,
  };
}

describe("sevenDayWindow", () => {
  it("is seven times twenty-four hours before the instant it was given", () => {
    const now = Date.parse("2026-09-08T12:34:56.000Z");
    expect(sevenDayWindow(now)).toEqual({ after: "2026-09-01T12:34:56Z" });
  });

  it("emits the canonical UTCDate with no fractional seconds", () => {
    // RFC 8620 §1.4's form. The server also parses the millisecond variant, but a
    // canonical value cannot be refused by a stricter parser later.
    const got = sevenDayWindow(Date.parse("2026-01-01T00:00:00.123Z")).after;
    expect(got).toBe("2025-12-25T00:00:00Z");
    expect(got).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
  });

  it("crosses a month and a year boundary by hours, not by calendar", () => {
    // A calendar-day window would change length at midnight and by timezone, and
    // the count under it would move without any mail arriving.
    expect(sevenDayWindow(Date.parse("2026-03-03T23:00:00Z")).after).toBe("2026-02-24T23:00:00Z");
  });

  it("names the window length once, where the sentence can read it", () => {
    expect(LINE_WINDOW_DAYS).toBe(7);
  });

  it("pins the line a fresh login starts on", () => {
    // db/schema.sql: `line_threshold real NOT NULL DEFAULT 0.5`. It is the reset
    // target, and a reset to the wrong number is a reset that moves mail.
    expect(LINE_DEFAULT_THRESHOLD).toBe(0.5);
  });
});

describe("stricterDelta", () => {
  it("is POSITIVE for mail that would leave the surface", () => {
    // The sign. Stricter = higher threshold = less on the surface, so
    // current − preview. Reversed, this reads "would move -8 to drawers", or worse,
    // is rendered as 8 in the wrong direction.
    const current = [row({ surface: 10, drawer: 5 })];
    const preview = [row({ surface: 2, drawer: 13 })];
    expect(stricterDelta(current, preview)).toBe(8);
  });

  it("counts unscored mail as being on the surface, on both sides", () => {
    // Unjudged mail is never hidden in a drawer (types.ts AreaSummaryRow), so it is
    // part of `review` at every threshold and cancels out — a delta that ignored it
    // would be right by accident only when there was none.
    const current = [row({ surface: 10, unscored: 4 })];
    const preview = [row({ surface: 6, drawer: 4, unscored: 4 })];
    expect(stricterDelta(current, preview)).toBe(4);
  });

  it("counts the Needs-sorting row's whole pile on the review side", () => {
    // headerCounts puts all three of its counts in `review` and none in a drawer,
    // so subtracting the DRAWER side instead would disagree with this by whatever
    // unsorted mail crossed the line.
    const current = [row({ surface: 6 }), row({ areaId: null, surface: 3, drawer: 2, unscored: 1 })];
    const preview = [row({ surface: 1, drawer: 5 }), row({ areaId: null, surface: 3, drawer: 2, unscored: 1 })];
    expect(stricterDelta(current, preview)).toBe(5);
  });

  it("is 0 when the step would move nothing — a measured zero", () => {
    const same = [row({ surface: 4, drawer: 1 })];
    expect(stricterDelta(same, [row({ surface: 4, drawer: 1 })])).toBe(0);
  });

  it("is null when either side was never measured", () => {
    // Not 0. "Nothing would move" and "nobody asked" are different states, and only
    // one of them is worth a sentence.
    expect(stricterDelta(null, [row()])).toBeNull();
    expect(stricterDelta([row()], null)).toBeNull();
    expect(stricterDelta(null, null)).toBeNull();
  });

  it("does not mutate the rows it was handed", () => {
    // They are the store's summary rows, shared with the desk.
    const current = [row({ surface: 3 })];
    const preview = [row({ surface: 1, drawer: 2 })];
    stricterDelta(current, preview);
    expect(current).toEqual([row({ surface: 3 })]);
    expect(preview).toEqual([row({ surface: 1, drawer: 2 })]);
  });
});
