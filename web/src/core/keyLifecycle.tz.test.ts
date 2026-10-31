// SPDX-License-Identifier: AGPL-3.0-only

// THE DATE MAPPING ACROSS A TIMEZONE BOUNDARY (#813). Run in a UTC− zone, late
// in the evening, when the local calendar and the UTC one disagree about what
// day it is: the user's "today" is the 21st while UTC is already on the 22nd.
//
// What must hold: the picked date goes on the wire as picked (not shifted by a
// round trip through Date), "in N days" counts the user's own calendar days,
// the date is printed as the day itself, and the first date offered is one the
// server will take (its end, in UTC, must be a day away).
//
// The zone is set before anything reads the clock, and the first test proves
// it took: a run in UTC would pass these for the wrong reason.

// (Through globalThis: the web tsconfig carries no Node types.)
(globalThis as unknown as { process: { env: Record<string, string> } }).process.env.TZ = "America/Los_Angeles";

import { describe, expect, it } from "vitest";

import {
  calendarDateLong,
  calendarDaysFromToday,
  exactDateBounds,
  exactDateProblem,
  localDateString,
  newKeyExpiryArg,
} from "./keyLifecycle";

// 23:30 on 21 Oct in Los Angeles (PDT, UTC−7) is 06:30 on 22 Oct in UTC.
const NOW = new Date("2026-10-22T06:30:00Z");

describe("late in the evening in a UTC− zone", () => {
  it("really is in that zone, and the calendars disagree", () => {
    expect(NOW.getTimezoneOffset()).toBe(420);
    expect(NOW.toISOString().slice(0, 10)).toBe("2026-10-22");
    expect(localDateString(NOW)).toBe("2026-10-21");
  });

  it("sends the picked date verbatim", () => {
    expect(newKeyExpiryArg("date", "2026-11-04", NOW)).toEqual({ expiresAt: "2026-11-04" });
  });

  it("counts the user's own days, and prints the day itself", () => {
    expect(calendarDaysFromToday("2026-11-04", NOW)).toBe(14);
    expect(calendarDaysFromToday("2026-10-22", NOW)).toBe(1);
    expect(calendarDateLong("2026-11-04")).toBe("4 Nov 2026");
  });

  it("does not offer a tomorrow the server would refuse", () => {
    // Local tomorrow (the 22nd) ends at 23:59:59Z on the 22nd, 17.5 hours away.
    expect(exactDateProblem("2026-10-22", NOW)).toBe("tooSoon");
    const { min } = exactDateBounds(NOW);
    expect(min).toBe("2026-10-23");
    expect(exactDateProblem(min, NOW)).toBeNull();
  });
});
