// SPDX-License-Identifier: AGPL-3.0-only

// The mapping is tested in BOTH directions and as a round trip, because the two
// states a reader would collapse are exactly the two that behave differently:
//
//   * `never` vs `default` — a null period has never meant "off" (types.ts), so a
//     mapping that used null for off would switch an area to the login default
//     while the UI said it had stopped sweeping.
//   * `default` vs `days: 30` when the login default IS 30 — identical on screen,
//     different the moment the login default moves.

import { describe, expect, it } from "vitest";

import {
  DRAWER_DEFAULT_DAYS,
  drawerDefaultLabelDays,
  drawerPeriodChoice,
  drawerPeriodPatch,
} from "./drawerPeriod";
import type { DrawerPeriod } from "./drawerPeriod";
import type { Area, DeskPreferences } from "../../../core/jmap/types";

function area(over: Partial<Area> = {}): Area {
  return {
    id: "01JBQZ3K7WX2N4Y6P8R0S1T2A0",
    name: "Work",
    position: 0,
    labelId: "01JBQZ3K7WX2N4Y6P8R0S1T2L0",
    tabs: [],
    labelIds: [],
    autoArchive: true,
    autoArchiveDays: null,
    hiddenFromRail: false,
    ...over,
  };
}

function prefs(over: Partial<DeskPreferences> = {}): DeskPreferences {
  return {
    id: "01JBQZ3K7WX2N4Y6P8R0S1T2U0",
    lineThreshold: 0.5,
    drawerDefaultDays: 30,
    trayNagDays: 7,
    // An established desk: migration 00050 backfilled every login that already
    // had preferences, so null here would be a login that has never seen the desk.
    setupCompletedAt: "2026-01-01T00:00:00Z",
    drawerSweepEnabled: false,
    ...over,
  };
}

/** Applies a patch the way the server does, so the round trip below exercises the
 * mapping rather than a re-statement of it: an ABSENT `autoArchiveDays` leaves the
 * stored value alone. */
function applied(a: Area, patch: ReturnType<typeof drawerPeriodPatch>): Area {
  return {
    ...a,
    autoArchive: patch.autoArchive,
    ...("autoArchiveDays" in patch ? { autoArchiveDays: patch.autoArchiveDays ?? null } : {}),
  };
}

describe("drawerPeriodChoice — what an area is on", () => {
  it("reads autoArchive: false as never, whatever the days say", () => {
    // "Off" is ONLY autoArchive: false. An area switched off while holding a
    // 14-day period is off, not on 14 days.
    expect(drawerPeriodChoice(area({ autoArchive: false, autoArchiveDays: 14 }))).toEqual({
      kind: "never",
    });
    expect(drawerPeriodChoice(area({ autoArchive: false, autoArchiveDays: null }))).toEqual({
      kind: "never",
    });
  });

  it("reads a null period as following the login default", () => {
    expect(drawerPeriodChoice(area({ autoArchive: true, autoArchiveDays: null }))).toEqual({
      kind: "default",
    });
  });

  it("reads an explicit period as pinned, including when it equals the default", () => {
    // The pair a reader would collapse. They differ the moment the login default
    // moves, and only one of them follows it.
    expect(drawerPeriodChoice(area({ autoArchiveDays: 30 }))).toEqual({ kind: "days", days: 30 });
    expect(drawerPeriodChoice(area({ autoArchiveDays: 7 }))).toEqual({ kind: "days", days: 7 });
    expect(drawerPeriodChoice(area({ autoArchiveDays: 21 }))).toEqual({ kind: "days", days: 21 });
  });
});

describe("drawerPeriodPatch — the write for one option", () => {
  it("switches off WITHOUT touching the stored period", () => {
    // Sending autoArchiveDays: null here succeeds and throws away the period the
    // user chose, so re-enabling would land on the login default instead of where
    // they left it.
    const patch = drawerPeriodPatch({ kind: "never" });
    expect(patch).toEqual({ autoArchive: false });
    expect("autoArchiveDays" in patch).toBe(false);
  });

  it("clears the period to follow the login default", () => {
    expect(drawerPeriodPatch({ kind: "default" })).toEqual({
      autoArchive: true,
      autoArchiveDays: null,
    });
  });

  it("pins a period, switching the sweep on as it does", () => {
    // Both fields: choosing a period for an area that was off has to turn it on,
    // or the number would be stored under a switch that is still false.
    expect(drawerPeriodPatch({ kind: "days", days: 90 })).toEqual({
      autoArchive: true,
      autoArchiveDays: 90,
    });
  });
});

describe("the two directions agree", () => {
  const periods: DrawerPeriod[] = [
    { kind: "never" },
    { kind: "default" },
    { kind: "days", days: 7 },
    { kind: "days", days: 14 },
    { kind: "days", days: 30 },
    { kind: "days", days: 90 },
    { kind: "days", days: 21 },
  ];

  it("round-trips every option through a patch and back", () => {
    for (const period of periods) {
      const after = applied(area({ autoArchiveDays: 14 }), drawerPeriodPatch(period));
      expect(drawerPeriodChoice(after)).toEqual(period);
    }
  });

  it("keeps the period across an off/on round trip", () => {
    // The consequence of `never` not sending the field: switching off and back on
    // returns to 14, not to the login default.
    const off = applied(area({ autoArchiveDays: 14 }), drawerPeriodPatch({ kind: "never" }));
    expect(off.autoArchiveDays).toBe(14);
    const on = applied(off, drawerPeriodPatch({ kind: "days", days: 14 }));
    expect(drawerPeriodChoice(on)).toEqual({ kind: "days", days: 14 });
  });
});

describe("the login default", () => {
  it("is the number db/schema.sql ships", () => {
    // Pinned rather than assumed: `drawer_default_days int NOT NULL DEFAULT 30`.
    // This is the reset target, and a reset to the wrong number is a reset that
    // changes the setting.
    expect(DRAWER_DEFAULT_DAYS).toBe(30);
  });

  it("labels the default option from the loaded preferences, and not at all without them", () => {
    expect(drawerDefaultLabelDays(prefs({ drawerDefaultDays: 45 }))).toBe(45);
    // Null, not 30: an unread number is not the default, it is unread.
    expect(drawerDefaultLabelDays(null)).toBeNull();
  });
});
