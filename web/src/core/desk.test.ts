// SPDX-License-Identifier: AGPL-3.0-only

// The desk's arithmetic (#375).
//
// What is worth testing here is not that addition works. It is that each number
// the desk states out loud is the number it claims to be:
//
//   * a section's FILTER must match the predicate Area/getSummary counts over,
//     or the list and the count printed beside it disagree — and the user has no
//     way to tell which one is lying;
//   * the header sentence must not count the same message twice, which is the
//     one mistake that is invisible until someone adds the columns up by hand;
//   * a hidden area must keep its sections, because the alternative is mail that
//     silently does not appear anywhere;
//   * an empty tray and a tray whose oldest item just arrived are different
//     states, and 0 cannot mean both.

import { describe, expect, it } from "vitest";

import {
  areaCounts,
  deskSections,
  drawerPeriodDays,
  groupByWait,
  headerCounts,
  pinnedWindow,
  railCounts,
  sectionFilter,
  sectionKey,
  sectionKeyPrefix,
  syncedAgoSeconds,
  trayStats,
  virtualWindow,
  waitBucket,
} from "./desk";
import type { Area, AreaSummaryRow, DeskPreferences, Email } from "./jmap/types";

function area(id: string, position: number, over: Partial<Area> = {}): Area {
  return {
    id,
    name: id,
    position,
    labelId: `L${id}`,
    tabs: [],
    labelIds: [],
    autoArchive: true,
    autoArchiveDays: null,
    hiddenFromRail: false,
    ...over,
  };
}

function row(areaId: string | null, over: Partial<AreaSummaryRow> = {}): AreaSummaryRow {
  return { areaId, surface: 0, drawer: 0, unscored: 0, archivedByDrawer: 0, ...over };
}

function prefs(over: Partial<DeskPreferences> = {}): DeskPreferences {
  return {
    id: "U1",
    lineThreshold: 0.5,
    drawerDefaultDays: 30,
    trayNagDays: 7,
    // An established desk: migration 00050 backfilled every login that already
    // had preferences, so null here would be a login that has never seen the desk.
    setupCompletedAt: "2026-01-01T00:00:00Z",
    drawerSweepEnabled: true,
    ...over,
  };
}

const DAY = 24 * 60 * 60 * 1000;

describe("section keys", () => {
  it("is stable for the same section and different per scope", () => {
    expect(sectionKey("surface", "A1", null)).toBe(sectionKey("surface", "A1", null));
    expect(sectionKey("surface", "A1", "ACC1")).not.toBe(sectionKey("surface", "A1", "ACC2"));
    // The unified desk is its own scope, not "no scope": mail from three
    // accounts is a different list from mail from one of them.
    expect(sectionKey("surface", "A1", null)).not.toBe(sectionKey("surface", "A1", "ACC1"));
  });

  it("distinguishes the kinds and Needs sorting", () => {
    expect(sectionKey("surface", "A1", null)).not.toBe(sectionKey("drawer", "A1", null));
    expect(sectionKey("surface", null, null)).not.toBe(sectionKey("surface", "A1", null));
    expect(sectionKey("tray", null, null)).not.toBe(sectionKey("surface", null, null));
  });

  it("prefixes every key of one scope, and no key of another", () => {
    const mine = sectionKeyPrefix("ACC1");
    expect(sectionKey("drawer", "A9", "ACC1").startsWith(mine)).toBe(true);
    expect(sectionKey("tray", null, "ACC1").startsWith(mine)).toBe(true);
    expect(sectionKey("drawer", "A9", "ACC2").startsWith(mine)).toBe(false);
    expect(sectionKey("drawer", "A9", null).startsWith(mine)).toBe(false);
  });
});

describe("which sections a tab has", () => {
  it("orders Review first by position, ties by id, Needs sorting LAST", () => {
    const areas = [area("B", 2), area("Z", 1), area("A", 1), area("C", 0)];
    expect(deskSections("review", areas)).toEqual([
      { kind: "surface", areaId: "C" },
      { kind: "surface", areaId: "A" },
      { kind: "surface", areaId: "Z" },
      { kind: "surface", areaId: "B" },
      { kind: "surface", areaId: null },
    ]);
  });

  it("gives Everything else one drawer per area and NO Needs-sorting drawer", () => {
    const areas = [area("B", 2), area("A", 1)];
    expect(deskSections("rest", areas)).toEqual([
      { kind: "drawer", areaId: "A" },
      { kind: "drawer", areaId: "B" },
    ]);
    expect(deskSections("rest", areas).some((s) => s.areaId === null)).toBe(false);
  });

  it("keeps the sections of an area hidden from the rail", () => {
    // hiddenFromRail hides the NAVIGATION entry. Dropping the section would
    // make every message in that area disappear from the desk entirely.
    const areas = [area("A", 0, { hiddenFromRail: true })];
    expect(deskSections("review", areas)).toEqual([
      { kind: "surface", areaId: "A" },
      { kind: "surface", areaId: null },
    ]);
    expect(deskSections("rest", areas)).toEqual([{ kind: "drawer", areaId: "A" }]);
  });

  it("still offers Needs sorting when there are no areas at all", () => {
    expect(deskSections("review", [])).toEqual([{ kind: "surface", areaId: null }]);
    expect(deskSections("rest", [])).toEqual([]);
  });
});

describe("what each section asks the server for", () => {
  it("an area's surface is on the desk, above the line, not snoozed", () => {
    expect(sectionFilter({ kind: "surface", areaId: "A1" }, null)).toEqual({
      inArea: "A1",
      onDesk: true,
      onSurface: true,
      isSnoozed: false,
    });
  });

  it("an area's drawer is the same query BELOW the line", () => {
    expect(sectionFilter({ kind: "drawer", areaId: "A1" }, null)).toEqual({
      inArea: "A1",
      onDesk: true,
      onSurface: false,
      isSnoozed: false,
    });
  });

  it("Needs sorting takes NO side of the line", () => {
    // All of it is on the surface whatever its score: nothing claims it, so no
    // drawer will ever sweep it. An onSurface here would strand the low-scoring
    // half in a list the desk does not draw.
    const f = sectionFilter({ kind: "surface", areaId: null }, null);
    expect(f).toEqual({ unsorted: true, onDesk: true, isSnoozed: false });
    expect("onSurface" in f).toBe(false);
  });

  it("the tray is the flag and nothing else", () => {
    expect(sectionFilter({ kind: "tray", areaId: null }, null)).toEqual({
      hasKeyword: "$flagged",
    });
  });

  it("adds inAccount — and only that — when the desk is scoped", () => {
    expect(sectionFilter({ kind: "surface", areaId: "A1" }, "ACC1")).toEqual({
      inArea: "A1",
      onDesk: true,
      onSurface: true,
      isSnoozed: false,
      inAccount: "ACC1",
    });
    expect(sectionFilter({ kind: "drawer", areaId: "A1" }, "ACC1")).toEqual({
      inArea: "A1",
      onDesk: true,
      onSurface: false,
      isSnoozed: false,
      inAccount: "ACC1",
    });
    expect(sectionFilter({ kind: "surface", areaId: null }, "ACC1")).toEqual({
      unsorted: true,
      onDesk: true,
      isSnoozed: false,
      inAccount: "ACC1",
    });
    expect(sectionFilter({ kind: "tray", areaId: null }, "ACC1")).toEqual({
      hasKeyword: "$flagged",
      inAccount: "ACC1",
    });
  });

  it("never asks for an area AND unsorted in the same query", () => {
    // The server refuses the combination; more to the point they are opposites.
    for (const f of [
      sectionFilter({ kind: "surface", areaId: "A1" }, null),
      sectionFilter({ kind: "surface", areaId: null }, null),
      sectionFilter({ kind: "drawer", areaId: "A1" }, null),
    ]) {
      expect(f.inArea !== undefined && f.unsorted !== undefined).toBe(false);
    }
  });
});

describe("the header sentence's counts", () => {
  it("counts each area's surface and unscored as review, its drawer as drawers", () => {
    const rows = [
      row("A", { surface: 3, drawer: 10, unscored: 2 }),
      row("B", { surface: 1, drawer: 7, unscored: 0 }),
    ];
    expect(headerCounts(rows)).toEqual({ review: 6, drawers: 17 });
  });

  it("puts ALL of the Needs-sorting row in review and none of it in drawers", () => {
    // Its below-the-line mail is shown on the surface too, so counting it in
    // `drawers` as well would claim the desk holds more mail than it does.
    const rows = [
      row("A", { surface: 2, drawer: 5, unscored: 1 }),
      row(null, { surface: 4, drawer: 6, unscored: 3 }),
    ];
    expect(headerCounts(rows)).toEqual({ review: 3 + 13, drawers: 5 });
  });

  it("is zero for an empty desk", () => {
    expect(headerCounts([])).toEqual({ review: 0, drawers: 0 });
  });
});

describe("one area's counts", () => {
  const rows = [
    row("A", { surface: 2, drawer: 5, unscored: 1 }),
    row(null, { surface: 4, drawer: 6, unscored: 3 }),
  ];

  it("splits an area at the line", () => {
    expect(areaCounts(rows, "A")).toEqual({ review: 3, drawer: 5 });
  });

  it("gives Needs sorting everything to review and no drawer", () => {
    expect(areaCounts(rows, null)).toEqual({ review: 13, drawer: 0 });
  });

  it("answers zero for an area the summary has no row for", () => {
    expect(areaCounts(rows, "GONE")).toEqual({ review: 0, drawer: 0 });
  });
});

describe("the tray strip's own numbers", () => {
  const now = Date.parse("2026-09-08T12:00:00Z");
  function flagged(flaggedAt: string | null, receivedAt: string): Pick<Email, "flaggedAt" | "receivedAt"> {
    return { flaggedAt, receivedAt };
  }

  it("ages a row by when it was FLAGGED, not when it arrived", () => {
    const s = trayStats(
      [flagged("2026-09-08T11:00:00Z", "2026-06-01T00:00:00Z")],
      now,
    );
    expect(s.count).toBe(1);
    expect(s.oldestWaitMs).toBe(60 * 60 * 1000);
    expect(s.overWeek).toBe(0);
  });

  it("falls back to receivedAt when the server sent no flaggedAt", () => {
    const s = trayStats([flagged(null, "2026-09-01T12:00:00Z")], now);
    expect(s.oldestWaitMs).toBe(7 * DAY);
    expect(s.overWeek).toBe(1);
  });

  it("reports null — not zero — for an empty tray", () => {
    // "Nothing is waiting" and "something arrived this instant" are different
    // states, and 0 reads as the second.
    expect(trayStats([], now)).toEqual({ count: 0, oldestWaitMs: null, overWeek: 0 });
  });

  it("takes the OLDEST wait and counts everything a week or older", () => {
    const s = trayStats(
      [
        flagged("2026-09-08T11:00:00Z", "2026-09-08T11:00:00Z"),
        flagged("2026-08-01T12:00:00Z", "2026-08-01T12:00:00Z"), // 38 days
        flagged("2026-09-01T12:00:00Z", "2026-09-01T12:00:00Z"), // 7 days
      ],
      now,
    );
    expect(s.count).toBe(3);
    expect(s.oldestWaitMs).toBe(38 * DAY);
    // Two: the week-old one AND the month-old one. Mail does not stop being
    // overdue by getting older.
    expect(s.overWeek).toBe(2);
  });

  it("treats an unparseable timestamp as no wait rather than NaN", () => {
    const s = trayStats([flagged(null, "not a date")], now);
    expect(s.oldestWaitMs).toBe(0);
    expect(Number.isNaN(s.oldestWaitMs)).toBe(false);
  });
});

describe("how long a tray row has waited", () => {
  it("bounds each bucket at its edge", () => {
    expect(waitBucket(0)).toBe("today");
    expect(waitBucket(DAY - 1)).toBe("today");
    expect(waitBucket(DAY)).toBe("thisWeek");
    expect(waitBucket(7 * DAY - 1)).toBe("thisWeek");
    expect(waitBucket(7 * DAY)).toBe("overWeek");
    expect(waitBucket(30 * DAY - 1)).toBe("overWeek");
    expect(waitBucket(30 * DAY)).toBe("overMonth");
    expect(waitBucket(365 * DAY)).toBe("overMonth");
  });

  it("reads a clock skewed into the future as today, not as overdue", () => {
    expect(waitBucket(-5 * DAY)).toBe("today");
  });
});

describe("a drawer's sweep period", () => {
  it("is null when the area does not auto-archive", () => {
    expect(drawerPeriodDays(area("A", 0, { autoArchive: false, autoArchiveDays: 14 }), prefs())).toBeNull();
  });

  it("is the area's own period when it has one", () => {
    expect(drawerPeriodDays(area("A", 0, { autoArchiveDays: 14 }), prefs())).toBe(14);
  });

  it("falls back to the login default", () => {
    expect(drawerPeriodDays(area("A", 0, { autoArchiveDays: null }), prefs({ drawerDefaultDays: 45 }))).toBe(45);
  });

  it("is null when the preferences are not loaded and the area names no period", () => {
    // Unknown, not 30. A guessed default would print a promise about mail from
    // a number this client invented.
    expect(drawerPeriodDays(area("A", 0, { autoArchiveDays: null }), null)).toBeNull();
    // ...but an explicit period is still known without them.
    expect(drawerPeriodDays(area("A", 0, { autoArchiveDays: 9 }), null)).toBe(9);
  });
});

describe("how long ago we last reached the server", () => {
  it("is null before the first round trip", () => {
    expect(syncedAgoSeconds(null, 1_000_000)).toBeNull();
  });

  it("is whole seconds, floored", () => {
    expect(syncedAgoSeconds(1_000_000, 1_040_600)).toBe(40);
  });

  it("clamps a backwards clock to just now", () => {
    expect(syncedAgoSeconds(1_000_000, 900_000)).toBe(0);
  });
});

describe("the virtual window", () => {
  it("is empty for an empty list, and for rows that have not been measured", () => {
    expect(virtualWindow(0, 800, 40, 0)).toEqual({ start: 0, end: 0, offsetTop: 0, totalHeight: 0 });
    expect(virtualWindow(0, 800, 0, 100)).toEqual({ start: 0, end: 0, offsetTop: 0, totalHeight: 0 });
    expect(virtualWindow(0, 800, -40, 100)).toEqual({ start: 0, end: 0, offsetTop: 0, totalHeight: 0 });
  });

  it("starts at the top of the first page, with no negative start", () => {
    const w = virtualWindow(0, 400, 40, 100);
    expect(w.start).toBe(0);
    expect(w.offsetTop).toBe(0);
    // 10 rows fit, one more is partly visible, plus the overscan below.
    expect(w.end).toBe(17);
    expect(w.totalHeight).toBe(4000);
  });

  it("treats rubber-band overscroll as the top", () => {
    expect(virtualWindow(-500, 400, 40, 100)).toEqual(virtualWindow(0, 400, 40, 100));
  });

  it("overscans both edges in the middle of a list", () => {
    const w = virtualWindow(2000, 400, 40, 100); // first visible row is 50
    expect(w.start).toBe(44);
    expect(w.end).toBe(67);
    expect(w.offsetTop).toBe(44 * 40);
    expect(w.totalHeight).toBe(4000);
  });

  it("honours a custom overscan", () => {
    const w = virtualWindow(2000, 400, 40, 100, 0);
    expect(w.start).toBe(50);
    expect(w.end).toBe(61);
  });

  it("clamps to the end of the list rather than past it", () => {
    const w = virtualWindow(3800, 400, 40, 100);
    expect(w.start).toBe(89);
    expect(w.end).toBe(100);
    expect(w.offsetTop).toBe(89 * 40);
  });

  it("answers an empty window for a scroll position past the end", () => {
    // A list that shrank under an open scroll position. start never exceeds
    // count, so slice(start, end) is empty rather than reversed.
    const w = virtualWindow(100_000, 400, 40, 10);
    expect(w.start).toBe(10);
    expect(w.end).toBe(10);
    expect(w.end).toBeGreaterThanOrEqual(w.start);
    expect(w.totalHeight).toBe(400);
    expect(w.offsetTop).toBe(400);
  });

  it("keeps offsetTop and totalHeight exact, so the scrollbar does not drift", () => {
    for (const scrollTop of [0, 37, 500, 1234, 3999]) {
      const w = virtualWindow(scrollTop, 300, 37, 250);
      expect(w.totalHeight).toBe(250 * 37);
      expect(w.offsetTop).toBe(w.start * 37);
      expect(w.end).toBeLessThanOrEqual(250);
      expect(w.start).toBeGreaterThanOrEqual(0);
    }
  });
});

describe("the pinned window — the row aria-activedescendant names", () => {
  // The window at the top of a 100-row list: rows 0..16 (see above).
  const top = virtualWindow(0, 400, 40, 100);
  // ...and one 50 rows down: 44..66.
  const middle = virtualWindow(2000, 400, 40, 100);

  it("pins nothing when the window already contains the cursor", () => {
    expect(pinnedWindow(top, 3, 100, 40).pinned).toBeNull();
    expect(pinnedWindow(middle, 44, 100, 40).pinned).toBeNull();
    // The last index INSIDE the window is end - 1; `end` itself is not.
    expect(pinnedWindow(middle, 66, 100, 40).pinned).toBeNull();
  });

  it("pins the cursor when the list has been scrolled away from it", () => {
    // Scrolled down with the cursor left at the top: without this the row that
    // `aria-activedescendant` names is not in the document at all.
    expect(pinnedWindow(middle, 2, 100, 40).pinned).toBe(2);
    // ...and the other direction: the cursor is past the bottom of the window.
    expect(pinnedWindow(top, 90, 100, 40).pinned).toBe(90);
    // The first index OUTSIDE the window is `end`.
    expect(pinnedWindow(middle, 67, 100, 40).pinned).toBe(67);
  });

  it("pins even when the window itself is empty, which is when it matters most", () => {
    // A list that shrank under an open scroll position: `virtualWindow` answers
    // start === end === count, so NO rows are rendered — and the cursor row is
    // exactly the one that still has to be in the document.
    const past = virtualWindow(100_000, 400, 40, 100);
    expect(past.start).toBe(past.end);
    expect(pinnedWindow(past, 3, 100, 40).pinned).toBe(3);
  });

  it("passes the window through untouched", () => {
    const p = pinnedWindow(middle, 2, 100, 40);
    expect(p.start).toBe(middle.start);
    expect(p.end).toBe(middle.end);
    expect(p.offsetTop).toBe(middle.offsetTop);
    expect(p.totalHeight).toBe(middle.totalHeight);
  });

  it("pins nothing for no cursor, an index off the end, or an unmeasured list", () => {
    // -1 is "the cursor is not on this tab" (ui/desk/flatten.ts rowIndexOf).
    expect(pinnedWindow(middle, -1, 100, 40).pinned).toBeNull();
    expect(pinnedWindow(middle, 100, 100, 40).pinned).toBeNull();
    expect(pinnedWindow(middle, 500, 100, 40).pinned).toBeNull();
    // Nothing measured: the caller is laying out in ordinary flow, where an
    // absolutely-positioned extra row would be drawn on top of the list. This is
    // the one case that must NOT pin, so it is asserted from the row height rather
    // than from the empty window it happens to produce.
    expect(pinnedWindow(virtualWindow(0, 800, 0, 100), 40, 100, 0).pinned).toBeNull();
    expect(pinnedWindow(virtualWindow(0, 800, 40, 0), 0, 0, 40).pinned).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The tray page groups (#375 increment 4)
// ---------------------------------------------------------------------------

const NOW = Date.parse("2026-09-08T12:00:00Z");
const DAY_MS = 24 * 60 * 60 * 1000;

/** A tray row that was flagged `ms` ago. `receivedAt` is deliberately ANCIENT
 * on every one of them: the wait must be measured from when the user put the
 * message in the tray, so a fixture whose two timestamps agreed could not tell
 * a correct implementation from one reading the wrong field. */
function waited(ms: number): { flaggedAt: string; receivedAt: string } {
  return {
    flaggedAt: new Date(NOW - ms).toISOString(),
    receivedAt: "2020-01-01T00:00:00Z",
  };
}

describe("groupByWait — the shape of what you have been ignoring", () => {
  it("puts each row in the group its wait falls in", () => {
    const rows = [
      waited(2 * 60 * 60 * 1000), // this morning
      waited(3 * DAY_MS),
      waited(10 * DAY_MS),
      waited(60 * DAY_MS),
    ];
    expect(groupByWait(rows, NOW).map((g) => g.bucket)).toEqual([
      "today",
      "thisWeek",
      "overWeek",
      "overMonth",
    ]);
    for (const group of groupByWait(rows, NOW)) expect(group.emails).toHaveLength(1);
  });

  it("draws the boundaries at a day, a week and a month, exclusive below", () => {
    // The off-by-one-unit cases, stated: a wait of exactly a week is NOT still
    // "this week", and a wait one millisecond short of it is.
    const at = (ms: number): string => (groupByWait([waited(ms)], NOW)[0] as { bucket: string }).bucket;
    expect(at(DAY_MS - 1)).toBe("today");
    expect(at(DAY_MS)).toBe("thisWeek");
    expect(at(7 * DAY_MS - 1)).toBe("thisWeek");
    expect(at(7 * DAY_MS)).toBe("overWeek");
    expect(at(10 * DAY_MS)).toBe("overWeek");
    expect(at(30 * DAY_MS - 1)).toBe("overWeek");
    expect(at(30 * DAY_MS)).toBe("overMonth");
  });

  it("omits empty groups rather than heading a list that does not exist", () => {
    const groups = groupByWait([waited(10 * DAY_MS), waited(11 * DAY_MS)], NOW);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.bucket).toBe("overWeek");
    expect(groups[0]?.emails).toHaveLength(2);
    expect(groupByWait([], NOW)).toEqual([]);
  });

  it("measures the wait from flaggedAt, falling back to receivedAt", () => {
    // The fallback is a DIFFERENT number, not the same one: a message flagged
    // out of a month-old thread has waited a day, and ageing it by arrival
    // would nag about a decision made this morning.
    const fromFlag = groupByWait([{ flaggedAt: null, receivedAt: new Date(NOW - 10 * DAY_MS).toISOString() }], NOW);
    expect(fromFlag[0]?.bucket).toBe("overWeek");
    const flaggedToday = groupByWait(
      [{ flaggedAt: new Date(NOW - 60 * 1000).toISOString(), receivedAt: "2020-01-01T00:00:00Z" }],
      NOW,
    );
    expect(flaggedToday[0]?.bucket).toBe("today");
  });

  it("reads an unparseable timestamp as no wait rather than as overdue", () => {
    // Same rule trayStats keeps: an unreadable date is not evidence that
    // something has been waiting.
    const groups = groupByWait([{ flaggedAt: "not a date", receivedAt: "also not" }], NOW);
    expect(groups[0]?.bucket).toBe("today");
  });

  it("agrees with trayStats about which rows are over a week", () => {
    // The two run side by side on the same screen — the header sentence says
    // "3 over a week" and the headings below split the same rows. A definition
    // that drifted between them would be a page arguing with itself.
    const rows = [waited(1 * DAY_MS), waited(8 * DAY_MS), waited(40 * DAY_MS)];
    const overWeekRows = groupByWait(rows, NOW)
      .filter((g) => g.bucket === "overWeek" || g.bucket === "overMonth")
      .reduce((n, g) => n + g.emails.length, 0);
    expect(overWeekRows).toBe(trayStats(rows, NOW).overWeek);
  });

  it("keeps the order it was given inside a group", () => {
    const a = waited(10 * DAY_MS);
    const b = waited(20 * DAY_MS);
    const groups = groupByWait([a, b], NOW);
    // The caller has already sorted (pinned first, then arrival); re-sorting by
    // wait here would override that with a second opinion.
    expect(groups[0]?.emails).toEqual([a, b]);
  });
});

// ---------------------------------------------------------------------------
// The rail's numbers (#375 increment 4)
// ---------------------------------------------------------------------------

describe("railCounts — what the rail prints beside its entries", () => {
  const rows = [
    row("A1", { surface: 2, drawer: 9, unscored: 1 }),
    row("A2", { surface: 0, drawer: 3, unscored: 0 }),
    row(null, { surface: 1, drawer: 2, unscored: 3 }),
  ];
  const summary = { rows, allMail: 3480 };

  it("counts the INBOX as everything unactioned — the tray included", () => {
    // The rail entry means "still waiting on you", and a flagged message is the
    // most waiting-on-you thing there is. Leaving the tray out would also make
    // the Inbox badge smaller than the sum of the two entries printed directly
    // under it, which is arithmetic a user checks once and then stops trusting.
    const { review, drawers } = headerCounts(rows);
    expect(review).toBe(9); // 2+1 + 0+0 + 1+2+3
    expect(drawers).toBe(12); // 9 + 3
    expect(railCounts(summary, 12).inbox).toBe(12 + 9 + 12);
    expect(railCounts(summary, 0).inbox).toBe(9 + 12);
  });

  it("takes the tray count from the tray section, not from the summary", () => {
    // They measure different sets: Area/getSummary counts what is ON THE DESK,
    // and the tray deliberately is not restricted to that — a message you
    // flagged and then filed is still in the tray (sectionFilter).
    expect(railCounts(summary, 12).tray).toBe(12);
    expect(railCounts({ rows: [], allMail: 0 }, 7).tray).toBe(7);
  });

  it("reports Needs sorting as all three of its buckets", () => {
    // Nothing claims that mail, so no drawer will ever sweep it and all of it
    // is on the surface whatever its score.
    expect(railCounts(summary, 0).needsSorting).toBe(6);
  });

  it("passes All mail through, including a real zero", () => {
    expect(railCounts(summary, 0).allMail).toBe(3480);
    expect(railCounts({ rows, allMail: 0 }, 0).allMail).toBe(0);
    // Null is "the count did not come back", which is not zero.
    expect(railCounts({ rows, allMail: null }, 0).allMail).toBeNull();
  });

  it("says NOTHING before the first summary rather than saying zero", () => {
    // A `0` that means "not loaded" is the most reassuring possible wrong
    // answer on the Inbox entry (CLAUDE.md).
    expect(railCounts(null, 12)).toEqual({
      inbox: null,
      tray: null,
      allMail: null,
      needsSorting: null,
    });
  });

  it("has no Inbox number while the tray is unknown — a sum with a missing term", () => {
    const counts = railCounts(summary, null);
    expect(counts.inbox).toBeNull();
    expect(counts.tray).toBeNull();
    // ...but the counts that do not depend on the tray are still stated.
    expect(counts.needsSorting).toBe(6);
    expect(counts.allMail).toBe(3480);
  });
});
