// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import type { QuietHours } from "../../../core/jmap/types";
import {
  initialQuietHours,
  minutesToTime,
  quietHoursFact,
  quietHoursProblem,
  timeToMinutes,
  timeZoneChoices,
} from "./notifyPrefs";

const base: QuietHours = { enabled: false, start: 1320, end: 420, days: [0, 1, 2, 3, 4, 5, 6], timeZone: "UTC", allowVip: true };

describe("quiet-hours arithmetic (#59)", () => {
  it("round-trips minutes and HH:MM at the edges", () => {
    for (const m of [0, 59, 60, 420, 1320, 1439]) expect(timeToMinutes(minutesToTime(m))).toBe(m);
    expect(minutesToTime(7)).toBe("00:07");
    expect(timeToMinutes("")).toBeNull();
    expect(timeToMinutes("24:00")).toBeNull();
    expect(timeToMinutes("12:60")).toBeNull();
  });

  it("a never-enabled setting starts from the browser's zone; a saved one keeps its own", () => {
    expect(initialQuietHours(base, "Europe/London").timeZone).toBe("Europe/London");
    const saved = { ...base, enabled: true, timeZone: "UTC" };
    expect(initialQuietHours(saved, "Europe/London").timeZone).toBe("UTC");
    const other = { ...base, timeZone: "Asia/Tokyo" };
    expect(initialQuietHours(other, "Europe/London").timeZone).toBe("Asia/Tokyo");
  });

  it("refuses a period with no length, and an enabled period with no day", () => {
    expect(quietHoursProblem({ ...base, start: 600, end: 600 })).not.toBeNull();
    expect(quietHoursProblem({ ...base, enabled: true, days: [] })).not.toBeNull();
    expect(quietHoursProblem({ ...base, enabled: true })).toBeNull();
  });

  it("the state line: nothing before the server says, off, or the period", () => {
    expect(quietHoursFact(undefined)).toBeNull();
    expect(quietHoursFact(base)).toBe("quiet hours off");
    expect(quietHoursFact({ ...base, enabled: true })).toBe("quiet 22:00–07:00");
  });

  it("always offers the stored and the browser's zone", () => {
    const zones = timeZoneChoices("Mars/Olympus", "Europe/London");
    expect(zones).toContain("Mars/Olympus");
    expect(zones).toContain("Europe/London");
    expect(zones).toContain("UTC");
  });
});
