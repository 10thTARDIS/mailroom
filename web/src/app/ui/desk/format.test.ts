// SPDX-License-Identifier: AGPL-3.0-only

// The desk's chrome sentences (#375 increment 3).
//
// Each of these is a CLAIM ABOUT THE USER'S MAIL, not decoration: "synced 40 s
// ago" says what you are looking at is current, "oldest waiting 9 d" is a nag,
// and the time on a row is what tells today's mail from March's at a glance.
// So they are tested at a FIXED `now`, which is the only way any of them can be
// tested at all.
//
// Dates are built with the local-time constructor and handed over as ISO
// strings, so "today"/"yesterday" mean the same thing to the test as they do to
// the code whatever timezone the runner is in.

import { describe, expect, it } from "vitest";

import {
  aliasOf,
  asOfText,
  bytesText,
  clockText,
  durationText,
  headerStateParts,
  moreText,
  overWeekText,
  rowTime,
  syncedAgoText,
  drawerButtonLabel,
  lineValueText,
  thresholdText,
  waitText,
} from "./format";
import type { HeaderState } from "../../../core/headerState";

const HOUR = 60 * 60 * 1000;
const DAY = 24 * HOUR;

/** 2026-08-12 15:30 local. */
const NOW = new Date(2026, 7, 12, 15, 30).getTime();

function at(y: number, m: number, d: number, h = 9, min = 14): string {
  return new Date(y, m, d, h, min).toISOString();
}

describe("rowTime", () => {
  it("shows the clock for a message that arrived today", () => {
    const out = rowTime(at(2026, 7, 12), NOW);
    expect(out).toMatch(/^\d{1,2}:\d{2}$/);
  });

  it("says Yest. for yesterday rather than a time", () => {
    const out = rowTime(at(2026, 7, 11), NOW);
    // The whole point of the bucket: a bare `09:14` on yesterday's mail reads
    // as "this morning". The word is the answer; the clock time is noise.
    expect(out).toBe("Yest.");
    expect(out).not.toMatch(/\d{1,2}:\d{2}/);
  });

  it("shows day and month, and no year, inside this year", () => {
    const out = rowTime(at(2026, 2, 3), NOW);
    expect(out).toMatch(/^3 \S+$/);
    expect(out).not.toContain("2026");
    expect(out).not.toContain(":");
  });

  it("carries the year once the message is older than this one", () => {
    const out = rowTime(at(2025, 7, 12), NOW);
    expect(out).toMatch(/^12 \S+ 2025$/);
  });

  it("renders nothing for a date it cannot parse", () => {
    // A row with an unreadable date shows no time rather than "Invalid Date".
    expect(rowTime("not a date", NOW)).toBe("");
  });
});

describe("syncedAgoText", () => {
  it("distinguishes never-synced from just-synced", () => {
    // 0 would claim a round trip that never happened.
    expect(syncedAgoText(null)).toBe("not synced yet");
    expect(syncedAgoText(0)).toBe("synced just now");
  });

  it("counts seconds, then minutes, then hours, then days — never a clock", () => {
    expect(syncedAgoText(9)).toBe("synced just now");
    expect(syncedAgoText(40)).toBe("synced 40 s ago");
    expect(syncedAgoText(59)).toBe("synced 59 s ago");
    expect(syncedAgoText(180)).toBe("synced 3 min ago");
    expect(syncedAgoText(2 * 60 * 60)).toBe("synced 2 h ago");
    expect(syncedAgoText(50 * 60 * 60)).toBe("synced 2 d ago");
  });
});

describe("waitText", () => {
  it("rounds down, so the nag never overstates itself", () => {
    expect(waitText(9 * DAY + 23 * HOUR)).toBe("9 d");
    expect(waitText(2 * DAY)).toBe("2 d");
  });

  it("uses hours below a day and says nothing numeric below an hour", () => {
    expect(waitText(4 * HOUR)).toBe("4 h");
    // Twenty minutes old is not "waiting", it is in progress.
    expect(waitText(20 * 60 * 1000)).toBe("under an hour");
  });
});

describe("overWeekText", () => {
  it("has a singular form, because English does", () => {
    expect(overWeekText(1)).toBe("1 over a week");
    expect(overWeekText(3)).toBe("3 over a week");
  });
});

describe("moreText", () => {
  it("names the page it would fetch and the total it is a bound on", () => {
    expect(moreText(168, 50, 218)).toBe("Show 50 more of 218");
  });

  it("never offers more than is left", () => {
    expect(moreText(7, 50, 57)).toBe("Show 7 more of 57");
  });
});

describe("aliasOf", () => {
  it("keeps the local part and the @, and drops the domain", () => {
    expect(aliasOf("invoices@example.com")).toBe("invoices@");
    expect(aliasOf("a.b+tag@mail.example.com")).toBe("a.b+tag@");
  });

  it("renders nothing rather than a stray glyph for what it cannot read", () => {
    expect(aliasOf(null)).toBe("");
    expect(aliasOf(undefined)).toBe("");
    expect(aliasOf("")).toBe("");
    expect(aliasOf("not-an-address")).toBe("");
    expect(aliasOf("@example.com")).toBe("");
  });
});

describe("thresholdText", () => {
  it("always shows two decimals, so 0.6 and 0.60 are not two answers", () => {
    expect(thresholdText(0.6)).toBe("0.60");
    expect(thresholdText(0.65)).toBe("0.65");
    expect(thresholdText(1)).toBe("1.00");
  });
});

// ---------------------------------------------------------------------------
// The offline / stale sentence (#375 increment 6)
// ---------------------------------------------------------------------------
//
// The rule these tests exist to hold is the one in core/headerState.ts and
// CLAUDE.md: PRINT ONLY THE CLAUSES THE HEALTH DATA CARRIES. A sentence that
// filled its gaps with plausible numbers would be the most convincing wrong
// thing on the screen — it is what somebody reads to decide whether their mail
// is arriving.

describe("durationText", () => {
  it("uses seconds under a minute", () => {
    expect(durationText(0)).toBe("0 s");
    expect(durationText(59)).toBe("59 s");
  });

  it("uses whole minutes under an hour", () => {
    expect(durationText(60)).toBe("1 min");
    expect(durationText(14 * 60 + 59)).toBe("14 min");
  });

  it("carries the minutes alongside the hours, which is the whole point", () => {
    // "3 h" for something 3 h 50 min behind understates the gap by nearly an
    // hour, in the sentence someone is reading to decide whether to go and look
    // at their provider.
    expect(durationText(3 * 3600 + 12 * 60)).toBe("3 h 12 min");
    expect(durationText(3 * 3600 + 50 * 60)).toBe("3 h 50 min");
  });

  it("drops a zero smaller part rather than printing it", () => {
    expect(durationText(2 * 3600)).toBe("2 h");
    expect(durationText(2 * 86400)).toBe("2 d");
  });

  it("uses days and hours past a day", () => {
    expect(durationText(86400 + 5 * 3600)).toBe("1 d 5 h");
  });

  it("rounds DOWN, so the number never overstates the gap", () => {
    expect(durationText(119)).toBe("1 min");
    expect(durationText(7199)).toBe("1 h 59 min");
  });

  it("never counts below zero", () => {
    expect(durationText(-5)).toBe("0 s");
  });
});

describe("clockText", () => {
  it("gives a 24-hour clock for an instant it can read", () => {
    expect(clockText(new Date(2026, 7, 12, 9, 2).toISOString())).toMatch(/^\d{2}:\d{2}$/);
  });

  it("gives nothing at all for an instant it cannot, so the clause is dropped", () => {
    // Rather than "since Invalid Date", which is a sentence about the client's
    // bug wearing the clothes of a fact about the user's mail.
    expect(clockText("not a date")).toBe("");
    expect(clockText("")).toBe("");
  });
});

describe("headerStateParts", () => {
  it("adds nothing when nothing is wrong", () => {
    expect(headerStateParts({ kind: "ok" }, NOW)).toEqual([]);
  });

  it("says what is on screen, how old it is, and how much is unsent", () => {
    const parts = headerStateParts(
      { kind: "offline", lastSyncAt: NOW - 14 * 60 * 1000, queued: 3 },
      NOW,
    );
    expect(parts).toEqual([
      "Offline. Showing what was loaded",
      "last synced 14 min ago",
      "3 changes waiting to send",
    ]);
  });

  it("uses the singular for one queued change", () => {
    const parts = headerStateParts({ kind: "offline", lastSyncAt: NOW, queued: 1 }, NOW);
    expect(parts).toContain("1 change waiting to send");
  });

  it("omits the queue clause when nothing is waiting", () => {
    const parts = headerStateParts({ kind: "offline", lastSyncAt: NOW, queued: 0 }, NOW);
    expect(parts.join(" ")).not.toContain("waiting to send");
  });

  it("does not claim a sync that never happened", () => {
    const parts = headerStateParts({ kind: "offline", lastSyncAt: null, queued: 0 }, NOW);
    expect(parts).toContain("never synced on this device");
    expect(parts.join(" ")).not.toContain("last synced");
  });

  it("names the account and how far behind it is", () => {
    const state: HeaderState = {
      kind: "stale",
      account: "Work",
      lagSeconds: 3 * 3600 + 12 * 60,
    };
    expect(headerStateParts(state, NOW)).toEqual([
      "Sync is behind.",
      "Work last synced 3 h 12 min ago",
    ]);
  });

  it("adds the outage clause only when there is a time to date it from", () => {
    const withSince: HeaderState = {
      kind: "stale",
      account: "Work",
      lagSeconds: 60,
      since: new Date(2026, 7, 12, 9, 2).toISOString(),
    };
    expect(headerStateParts(withSince, NOW).join(" · ")).toMatch(
      /provider not reachable since \d{2}:\d{2}/,
    );
    const without: HeaderState = { kind: "stale", account: "Work", lagSeconds: 60 };
    expect(headerStateParts(without, NOW).join(" ")).not.toContain("not reachable");
  });

  it("drops the outage clause for a timestamp it cannot read", () => {
    const state: HeaderState = {
      kind: "stale",
      account: "Work",
      lagSeconds: 60,
      since: "whenever",
    };
    expect(headerStateParts(state, NOW).join(" ")).not.toContain("not reachable");
  });

  it("adds the retry clause only when a cadence was supplied", () => {
    const withRetry: HeaderState = {
      kind: "stale",
      account: "Work",
      lagSeconds: 60,
      retryEverySeconds: 240,
    };
    expect(headerStateParts(withRetry, NOW)).toContain("retrying every 4 min");
    // Which is what the app passes today — Account/getHealth reports no
    // cadence — so the sentence simply stops one clause earlier.
    const without: HeaderState = { kind: "stale", account: "Work", lagSeconds: 60 };
    expect(headerStateParts(without, NOW).join(" ")).not.toContain("retrying");
  });

  it("says an account has never synced rather than putting a zero on it", () => {
    const state: HeaderState = { kind: "stale", account: "Work", lagSeconds: null };
    expect(headerStateParts(state, NOW)).toContain("Work has never synced");
    expect(headerStateParts(state, NOW).join(" ")).not.toContain("0 s");
  });

  it("keeps the wording the header has always used for a server that said no", () => {
    expect(headerStateParts({ kind: "error", message: "unknownMethod" }, NOW)).toEqual([
      "cannot reach the server",
    ]);
  });
});

describe("asOfText", () => {
  it("is the strip's short form of the same caveat", () => {
    expect(asOfText(14 * 60)).toBe("as of 14 min ago");
  });

  it("does not date counts from a sync that never happened", () => {
    expect(asOfText(null)).toBe("never synced on this device");
  });
});

describe("bytesText", () => {
  it("uses the unit that carries the size", () => {
    expect(bytesText(0)).toBe("0 B");
    expect(bytesText(812)).toBe("812 B");
    expect(bytesText(1024)).toBe("1 KB");
    expect(bytesText(40 * 1024)).toBe("40 KB");
    expect(bytesText(2.4 * 1024 * 1024)).toBe("2.4 MB");
  });

  // The boundary either side of a megabyte, because that is where the shape of the
  // answer changes and where an off-by-one reads as a wrong number rather than as a
  // wrong unit.
  it("changes unit at the boundary, not near it", () => {
    expect(bytesText(1024 * 1024 - 1)).toBe("1024 KB");
    expect(bytesText(1024 * 1024)).toBe("1.0 MB");
  });
});

describe("lineValueText — what the line's slider says instead of a number", () => {
  it("is the split, in words, when nothing is pending", () => {
    expect(lineValueText(6, 37, null)).toBe("6 to review, 37 in drawers");
  });

  it("adds what the pending line would do, and which way it would do it", () => {
    expect(
      lineValueText(6, 37, { threshold: "0.65", moves: 2, direction: "stricter" }),
    ).toBe("6 to review, 37 in drawers; the line at 0.65 would move 2 to the drawers");
    // The other direction is not the same sentence with a different number: mail
    // moves the OTHER WAY, and a slider that said "to the drawers" for a looser
    // line would describe the opposite of what Apply is about to do.
    expect(lineValueText(6, 37, { threshold: "0.45", moves: 9, direction: "looser" })).toBe(
      "6 to review, 37 in drawers; the line at 0.45 would move 9 to the surface",
    );
  });

  it("says a step that changes nothing changes nothing", () => {
    // "would move 0" reads as a failed measurement; this is a real answer — the
    // step is too small to move anything on this desk.
    expect(lineValueText(6, 37, { threshold: "0.60", moves: 0, direction: "stricter" })).toBe(
      "6 to review, 37 in drawers; the line at 0.60 would move nothing",
    );
  });

  it("drops the third clause entirely when there is no preview to base it on", () => {
    // The absence is the case worth pinning: while the preview is in flight there
    // is no number, and inventing one would be the slider claiming a consequence
    // the server has not answered for.
    expect(lineValueText(0, 0, null)).toBe("0 to review, 0 in drawers");
    expect(lineValueText(0, 0, null)).not.toContain("would move");
  });
});

describe("drawerButtonLabel — which drawer, and what happens to it", () => {
  it("names the area, the count and the sweep", () => {
    expect(drawerButtonLabel("Promotions", 24, 30, true)).toBe(
      "Promotions drawer, 24 messages, auto-archives after 30 days",
    );
  });

  it("says the sweep is off, without hiding the period it would use", () => {
    // The period is still the area's setting and still what WOULD happen if the
    // login-level switch were on (#443, which defaults to off), so it stays — the
    // same choice the visible line makes with "· sweep off".
    expect(drawerButtonLabel("Promotions", 24, 30, false)).toBe(
      "Promotions drawer, 24 messages, auto-archives after 30 days, sweep off",
    );
  });

  it("says nothing about a period there is none of", () => {
    // The case that catches a name built from the wrong field: no period must not
    // become "after null days", and it must not silently reuse the sweep sentence.
    expect(drawerButtonLabel("Receipts", 3, null, true)).toBe(
      "Receipts drawer, 3 messages, never auto-archives",
    );
    expect(drawerButtonLabel("Receipts", 3, null, false)).toBe(
      "Receipts drawer, 3 messages, never auto-archives",
    );
    expect(drawerButtonLabel("Receipts", 3, null, true)).not.toContain("sweep off");
  });

  it("counts one message as one, and one day as one", () => {
    expect(drawerButtonLabel("Bills", 1, 1, true)).toBe(
      "Bills drawer, 1 message, auto-archives after 1 day",
    );
    expect(drawerButtonLabel("Bills", 0, 30, true)).toBe(
      "Bills drawer, 0 messages, auto-archives after 30 days",
    );
  });
});
